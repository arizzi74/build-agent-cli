package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const nodeDownloadURL = "https://nodejs.org/en/download"

type executableLookup func(string) (string, error)

type localBuildTools struct {
	NodePath string
	NPMPath  string
}

func discoverLocalBuildTools(lookup executableLookup) localBuildTools {
	if lookup == nil {
		return localBuildTools{}
	}
	var tools localBuildTools
	if path, err := lookup("node"); err == nil {
		tools.NodePath = strings.TrimSpace(path)
	}
	if path, err := lookup("npm"); err == nil {
		tools.NPMPath = strings.TrimSpace(path)
	}
	return tools
}

func currentLocalBuildTools() localBuildTools {
	return discoverLocalBuildTools(exec.LookPath)
}

func (tools localBuildTools) missingNames() []string {
	missing := make([]string, 0, 2)
	if tools.NodePath == "" {
		missing = append(missing, "node")
	}
	if tools.NPMPath == "" {
		missing = append(missing, "npm")
	}
	return missing
}

func (tools localBuildTools) warningMessage(goos string) string {
	missing := tools.missingNames()
	if len(missing) == 0 {
		return ""
	}

	impact := make([]string, 0, 2)
	if tools.NodePath == "" {
		impact = append(impact, "Node.js is required for local application builds.")
	}
	if tools.NPMPath == "" {
		impact = append(impact, "npm is required when bacli must install missing project dependencies; a checkout with complete node_modules may still build when Node.js is available.")
	}

	return fmt.Sprintf(
		"Missing from PATH: %s.\n\n%s\n\n%s\n\nAfter installation, restart VS Code or the terminal that launches bacli, then verify:\n  node --version\n  npm --version\n\nChat, conversations, workspace selection, and /sync remain available.",
		strings.Join(missing, ", "),
		strings.Join(impact, " "),
		localBuildInstallInstructions(goos),
	)
}

func localBuildInstallInstructions(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin":
		return "Install Node.js on macOS:\n  Homebrew: brew install node\n  Or install the current Node.js LTS release from " + nodeDownloadURL
	case "linux":
		return "Install a current Node.js LTS release and npm on Linux:\n  Debian/Ubuntu: sudo apt update && sudo apt install nodejs npm\n  Fedora/RHEL: sudo dnf install nodejs npm\n  If the distribution package is too old for the app SDK, use " + nodeDownloadURL
	case "windows":
		return "Install Node.js LTS on Windows from PowerShell:\n  winget install -e --id OpenJS.NodeJS.LTS\n  Or use the installer from " + nodeDownloadURL
	default:
		return "Install the current Node.js LTS release, including npm, from " + nodeDownloadURL
	}
}

func (tools localBuildTools) requireNode() error {
	if tools.NodePath != "" {
		return nil
	}
	message := "Node.js executable 'node' was not found in PATH; local application builds require Node.js.\n\n" + localBuildInstallInstructions(runtime.GOOS)
	return codedError{Code: "NODE_NOT_FOUND", Message: message}
}

func (tools localBuildTools) requireNPM() error {
	if tools.NPMPath != "" {
		return nil
	}
	message := "npm executable was not found in PATH; bacli cannot install missing local build dependencies.\n\n" + localBuildInstallInstructions(runtime.GOOS)
	return codedError{Code: "NPM_NOT_FOUND", Message: message}
}

func showStartupLocalBuildWarning(status statusBarState) {
	message := currentLocalBuildTools().warningMessage(runtime.GOOS)
	if message == "" {
		return
	}
	const title = "Build environment warning"
	if terminalRecordPersistentWarningTextAndAppend(title, message, status) {
		return
	}
	fmt.Fprintf(os.Stderr, "%s\n%s\n", title, message)
}
