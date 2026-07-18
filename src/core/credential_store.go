package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	instanceCredentialSchemaVersion = 1
	instanceCredentialMaxBytes      = 16 << 10
)

// storedInstanceCredentials is intentionally separate from session.json.
// session.json contains revocable cookies; secrets.json exists only after the
// user explicitly chooses to retain the original username and password.
type storedInstanceCredentials struct {
	SchemaVersion int       `json:"schemaVersion"`
	InstanceURL   string    `json:"instanceUrl"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	StoredAt      time.Time `json:"storedAt"`
}

func credentialSecretsFile(profile string) string {
	return filepath.Join(profileDir(profile), "secrets.json")
}

func loadStoredInstanceCredentials(profile, instanceURL string) (storedInstanceCredentials, bool, error) {
	if !isValidProfile(profile) {
		return storedInstanceCredentials{}, false, errors.New("invalid credential profile")
	}
	path := credentialSecretsFile(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return storedInstanceCredentials{}, false, errors.New("unsafe credential secrets path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return storedInstanceCredentials{}, false, nil
	}
	if err != nil {
		return storedInstanceCredentials{}, false, privateFileError(path)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return storedInstanceCredentials{}, false, errors.New("credential secrets must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return storedInstanceCredentials{}, false, errors.New("credential secrets permissions must be 0600")
	}
	if info.Size() <= 0 || info.Size() > instanceCredentialMaxBytes {
		return storedInstanceCredentials{}, false, errors.New("credential secrets file has an invalid size")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return storedInstanceCredentials{}, false, privateFileError(path)
	}
	var credentials storedInstanceCredentials
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		return storedInstanceCredentials{}, false, errors.New("credential secrets file is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return storedInstanceCredentials{}, false, errors.New("credential secrets file has trailing data")
	}
	credentials.InstanceURL = cleanInstanceURL(credentials.InstanceURL)
	credentials.Username = strings.TrimSpace(credentials.Username)
	if credentials.SchemaVersion != instanceCredentialSchemaVersion || credentials.InstanceURL == "" || credentials.Username == "" || credentials.Password == "" {
		return storedInstanceCredentials{}, false, errors.New("credential secrets file is incomplete")
	}
	if strings.ContainsAny(credentials.Username+credentials.Password, "\x00\r\n") {
		return storedInstanceCredentials{}, false, errors.New("credential secrets file contains invalid characters")
	}
	if !sameInstance(credentials.InstanceURL, instanceURL) {
		return storedInstanceCredentials{}, false, nil
	}
	return credentials, true, nil
}

func saveStoredInstanceCredentials(profile, instanceURL, username, password string) error {
	if !isValidProfile(profile) {
		return errors.New("invalid credential profile")
	}
	credentials := storedInstanceCredentials{
		SchemaVersion: instanceCredentialSchemaVersion,
		InstanceURL:   cleanInstanceURL(instanceURL),
		Username:      strings.TrimSpace(username),
		Password:      password,
		StoredAt:      time.Now().UTC(),
	}
	if credentials.InstanceURL == "" || credentials.Username == "" || credentials.Password == "" || strings.ContainsAny(credentials.Username+credentials.Password, "\x00\r\n") {
		return errors.New("refusing to store invalid credentials")
	}
	raw, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return privateFileError(credentialSecretsFile(profile))
	}
	return writePrivateFile(credentialSecretsFile(profile), append(raw, '\n'))
}

func deleteStoredInstanceCredentials(profile string) error {
	err := os.Remove(credentialSecretsFile(profile))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return privateFileError(credentialSecretsFile(profile))
}
