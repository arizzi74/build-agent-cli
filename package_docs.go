package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	packageDocsMaxREADMEBytes = 12 * 1024
	packageDocsMaxExports     = 32
	packageDocsMaxStringBytes = 1024
)

var packageDocsNamePartRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// packageDocsManifest is deliberately narrow: package_docs is an inspection
// action, not a general package.json parser.
type packageDocsManifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	License     string          `json:"license"`
	Homepage    string          `json:"homepage"`
	Repository  json.RawMessage `json:"repository"`
	Keywords    []string        `json:"keywords"`
	Exports     json.RawMessage `json:"exports"`
}

// answerPackageDocs returns local documentation for an already-installed Node
// package. It is intentionally read-only and only considers the canonical
// active checkout for the selected application.
func (c *Client) answerPackageDocs(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, string) {
	if err := ctx.Err(); err != nil {
		return packageDocsError(err, "PACKAGE_DOCS_CANCELLED"), "error"
	}
	name, err := packageDocsName(firstString(payload, "packageName", "package_name", "name"))
	if err != nil {
		return packageDocsError(err, "INVALID_PACKAGE_NAME"), "error"
	}
	projectDir, err := c.resolvePackageDocsProject()
	if err != nil {
		return packageDocsError(err, "LOCAL_PROJECT_NOT_FOUND"), "error"
	}
	packageDir, err := packageDocsInstalledPath(projectDir, name)
	if err != nil {
		code := "PACKAGE_NOT_INSTALLED"
		if errors.Is(err, errUnsafePackageDocsPath) {
			code = "UNSAFE_PACKAGE_PATH"
		}
		return packageDocsError(err, code), "error"
	}
	if err := ctx.Err(); err != nil {
		return packageDocsError(err, "PACKAGE_DOCS_CANCELLED"), "error"
	}

	manifestPath, err := packageDocsChildPath(projectDir, packageDir, "package.json")
	if err != nil {
		return packageDocsError(err, "UNSAFE_PACKAGE_PATH"), "error"
	}
	raw, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return packageDocsError(fmt.Errorf("installed package %q has no package.json", name), "PACKAGE_NOT_INSTALLED"), "error"
	}
	if err != nil {
		return packageDocsError(fmt.Errorf("read package metadata: %w", err), "PACKAGE_DOCS_READ_FAILED"), "error"
	}
	if err := ctx.Err(); err != nil {
		return packageDocsError(err, "PACKAGE_DOCS_CANCELLED"), "error"
	}

	var manifest packageDocsManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return packageDocsError(fmt.Errorf("invalid package.json for %q: %w", name, err), "INVALID_PACKAGE_METADATA"), "error"
	}
	readme, readmeTruncated, readmeFound, err := packageDocsReadREADME(ctx, projectDir, packageDir)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return packageDocsError(err, "PACKAGE_DOCS_CANCELLED"), "error"
		}
		return packageDocsError(err, "PACKAGE_DOCS_READ_FAILED"), "error"
	}

	result := map[string]interface{}{
		"packageName":            name,
		"packagePath":            filepath.ToSlash(filepath.Join("node_modules", filepath.FromSlash(name))),
		"metadata":               packageDocsMetadata(manifest),
		"readme":                 readme,
		"readmeFound":            readmeFound,
		"exports":                packageDocsExports(manifest.Exports),
		"truncated":              readmeTruncated,
		"documentationAvailable": readmeFound,
	}
	if !readmeFound {
		result["message"] = "README not found; returning package metadata and exports."
	}
	return result, "complete"
}

func packageDocsError(err error, code string) map[string]interface{} {
	return map[string]interface{}{"error": err.Error(), "code": code}
}

func packageDocsName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("package_docs requires packageName")
	}
	if strings.ContainsAny(name, `\\/`) && !strings.HasPrefix(name, "@") {
		return "", fmt.Errorf("unsafe package name %q", raw)
	}
	parts := strings.Split(name, "/")
	if strings.HasPrefix(name, "@") {
		if len(parts) != 2 || len(parts[0]) < 2 || !packageDocsNamePartRE.MatchString(parts[0][1:]) || !packageDocsNamePartRE.MatchString(parts[1]) {
			return "", fmt.Errorf("invalid scoped package name %q", raw)
		}
	} else if len(parts) != 1 || !packageDocsNamePartRE.MatchString(name) {
		return "", fmt.Errorf("invalid package name %q", raw)
	}
	return name, nil
}

// resolvePackageDocsProject mirrors the read-only project resolution order but
// never repairs a registry or clears an invalid process-local override.
func (c *Client) resolvePackageDocsProject() (string, error) {
	appID := strings.TrimSpace(c.activeAppSysID())
	if appID == "" {
		return "", errors.New("package_docs requires an application with a local project")
	}
	instanceURL, appID := c.projectRegistryIdentity(appID)
	rootURI := c.activeAppRootURI()
	if override := strings.TrimSpace(c.localProjectOverride); override != "" {
		if _, ok := validManifestedProject(override, instanceURL, appID, rootURI); ok {
			return canonicalProjectPath(override)
		}
	}
	if dir, _, ok := matchingManifestedAncestor(instanceURL, appID, rootURI); ok {
		return canonicalProjectPath(dir)
	}
	registry, err := loadProjectRegistry(c.opts.Profile)
	if err != nil {
		return "", err
	}
	if project, ok := registryProject(registry, instanceURL, appID); ok {
		valid, _ := validRegisteredCheckouts(project, instanceURL, appID, rootURI)
		if len(valid) > 0 {
			chosen := valid[0]
			if primary, found := projectCheckoutByID(project, project.PrimaryCheckoutID); found {
				for _, checkout := range valid {
					if checkout.ID == primary.ID {
						chosen = checkout
						break
					}
				}
			}
			return canonicalProjectPath(chosen.Path)
		}
	}
	candidate, err := c.persistentProjectDirForAppNamed(appID, c.persistentAppDisplayName(appID))
	if err != nil {
		return "", err
	}
	if _, ok := validCanonicalManifestedProject(candidate, instanceURL, appID, rootURI); ok {
		return canonicalProjectPath(candidate)
	}
	return "", os.ErrNotExist
}

var errUnsafePackageDocsPath = errors.New("unsafe package documentation path")

func packageDocsInstalledPath(projectDir, name string) (string, error) {
	root, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve local project: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(root, "node_modules", filepath.FromSlash(name))
	resolved, err := filepath.EvalSymlinks(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("package %q is not installed in the active local project", name)
	}
	if err != nil {
		return "", fmt.Errorf("resolve installed package %q: %w", name, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || !packageDocsWithin(root, resolved) {
		return "", fmt.Errorf("%w for package %q", errUnsafePackageDocsPath, name)
	}
	return filepath.Clean(resolved), nil
}

func packageDocsChildPath(projectDir, packageDir, name string) (string, error) {
	if filepath.Base(name) != name {
		return "", errUnsafePackageDocsPath
	}
	candidate := filepath.Join(packageDir, name)
	resolved, err := filepath.EvalSymlinks(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	}
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || !packageDocsWithin(packageDir, resolved) || !packageDocsWithin(projectDir, resolved) {
		return "", errUnsafePackageDocsPath
	}
	return resolved, nil
}

func packageDocsWithin(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func packageDocsReadREADME(ctx context.Context, projectDir, packageDir string) (string, bool, bool, error) {
	for _, name := range []string{"README.md", "README", "README.txt"} {
		if err := ctx.Err(); err != nil {
			return "", false, false, err
		}
		path, err := packageDocsChildPath(projectDir, packageDir, name)
		if err != nil {
			return "", false, false, err
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, false, fmt.Errorf("read README: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, packageDocsMaxREADMEBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return "", false, false, fmt.Errorf("read README: %w", readErr)
		}
		if closeErr != nil {
			return "", false, false, fmt.Errorf("close README: %w", closeErr)
		}
		truncated := len(data) > packageDocsMaxREADMEBytes
		if truncated {
			data = boundedPackageDocsBytes(data, packageDocsMaxREADMEBytes)
		}
		return string(bytesToValidUTF8(data)), truncated, true, nil
	}
	return "", false, false, nil
}

func packageDocsMetadata(manifest packageDocsManifest) map[string]interface{} {
	keywords := make([]string, 0, len(manifest.Keywords))
	for _, keyword := range manifest.Keywords {
		keyword = boundedPackageDocsString(keyword)
		if keyword != "" {
			keywords = append(keywords, keyword)
		}
	}
	sort.Strings(keywords)
	if len(keywords) > packageDocsMaxExports {
		keywords = keywords[:packageDocsMaxExports]
	}
	return map[string]interface{}{
		"name":        boundedPackageDocsString(manifest.Name),
		"version":     boundedPackageDocsString(manifest.Version),
		"description": boundedPackageDocsString(manifest.Description),
		"license":     boundedPackageDocsString(manifest.License),
		"homepage":    boundedPackageDocsString(manifest.Homepage),
		"repository":  boundedPackageDocsString(packageDocsRepository(manifest.Repository)),
		"keywords":    keywords,
	}
}

func packageDocsRepository(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var object struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.URL
	}
	return ""
}

func packageDocsExports(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}
	}
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return []string{}
	}
	set := map[string]struct{}{}
	packageDocsCollectExports(value, "", set)
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, boundedPackageDocsString(key))
	}
	sort.Strings(out)
	out = uniquePackageDocsStrings(out)
	if len(out) > packageDocsMaxExports {
		out = out[:packageDocsMaxExports]
	}
	return out
}

func uniquePackageDocsStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func packageDocsCollectExports(value interface{}, prefix string, set map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		if prefix == "" {
			set["."] = struct{}{}
		} else {
			set[prefix] = struct{}{}
		}
	case map[string]interface{}:
		hasSubpath := false
		for key, child := range typed {
			if !strings.HasPrefix(key, ".") {
				continue // condition keys such as import/default are not exports.
			}
			hasSubpath = true
			packageDocsCollectExports(child, key, set)
		}
		if !hasSubpath {
			if prefix == "" {
				set["."] = struct{}{}
			} else {
				set[prefix] = struct{}{}
			}
		}
	}
}

func boundedPackageDocsString(value string) string {
	value = string(bytesToValidUTF8([]byte(strings.TrimSpace(value))))
	return string(boundedPackageDocsBytes([]byte(value), packageDocsMaxStringBytes))
}

func boundedPackageDocsBytes(value []byte, limit int) []byte {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.Valid(value) {
		value = value[:len(value)-1]
	}
	return value
}

func bytesToValidUTF8(value []byte) []byte {
	if utf8.Valid(value) {
		return value
	}
	return []byte(strings.ToValidUTF8(string(value), "�"))
}
