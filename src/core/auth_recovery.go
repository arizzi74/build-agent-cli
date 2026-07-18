package core

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	credentialRecoveryReauth = "reauthenticate"
	credentialRecoveryRemove = "remove"
	credentialRecoveryCancel = "cancel"
)

func promptCredentialRecovery(profile, instanceURL, what string, cause error) (string, error) {
	if !credentialRecoveryInteractive() {
		return credentialRecoveryReauth, nil
	}
	if what == "" {
		what = "credentials"
	}
	for {
		fmt.Fprintf(os.Stderr, "%s for %s (%s) failed or expired", what, instanceURL, profile)
		if cause != nil {
			fmt.Fprintf(os.Stderr, ": %v", cause)
		}
		fmt.Fprintln(os.Stderr)
		answer, err := promptLine("Reauthenticate, remove this instance, or cancel? [r/d/c]: ")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "r", "reauth", "reauthenticate", "login":
			return credentialRecoveryReauth, nil
		case "d", "delete", "remove", "rm":
			return credentialRecoveryRemove, nil
		case "c", "cancel", "q", "quit":
			return credentialRecoveryCancel, nil
		}
		fmt.Fprintln(os.Stderr, "please answer r, d, or c")
	}
}

func parseCredentialRecoveryAnswer(answer string) (string, error) {
	answer = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(answer, "/")))
	switch answer {
	case "", "1", "r", "reauth", "reauthenticate", "login":
		return credentialRecoveryReauth, nil
	case "2", "d", "delete", "remove", "remove instance", "rm":
		return credentialRecoveryRemove, nil
	case "3", "c", "cancel", "q", "quit":
		return credentialRecoveryCancel, nil
	default:
		return "", errors.New("reply Reauthenticate, Remove instance, or Cancel")
	}
}

func credentialRecoveryInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(terminalStderrFD())
}
