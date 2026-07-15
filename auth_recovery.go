package main

import (
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

func credentialRecoveryInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(terminalStderrFD())
}
