package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func handleSessionFlags(opts Options) (bool, error) {
	if opts.Profile == "" {
		opts.Profile = "default"
	}
	if !isValidProfile(opts.Profile) {
		return false, fmt.Errorf("invalid profile %q: use letters, numbers, dash or underscore", opts.Profile)
	}
	if opts.Logout {
		if err := deleteProfileAuthentication(opts.Profile); err != nil {
			return true, err
		}
		fmt.Fprintf(os.Stderr, "deleted web session, OAuth token, and stored login credentials for profile %q; instance configuration was preserved\n", opts.Profile)
		return true, nil
	}
	if opts.SessionStatus {
		session, ok := loadWebSession(opts.Profile)
		if !ok {
			fmt.Fprintf(os.Stderr, "no saved web session for profile %q\n", opts.Profile)
			return true, nil
		}
		fmt.Printf("profile: %s\n", opts.Profile)
		fmt.Printf("instance: %s\n", session.InstanceURL)
		fmt.Printf("auth: %s\n", session.AuthMode)
		if session.Username != "" {
			fmt.Printf("user: %s\n", session.Username)
		}
		fmt.Printf("created: %s\n", session.CreatedAt.Format("2006-01-02 15:04:05 MST"))
		fmt.Printf("cookies: yes\n")
		if session.UserToken != "" {
			fmt.Printf("user token: yes\n")
		} else {
			fmt.Printf("user token: no\n")
		}
		fmt.Printf("path: %s\n", sessionFile(opts.Profile))
		if _, stored, err := loadStoredInstanceCredentials(opts.Profile, session.InstanceURL); err != nil {
			fmt.Printf("stored login: invalid (%v)\n", err)
		} else if stored {
			fmt.Printf("stored login: yes (%s)\n", credentialSecretsFile(opts.Profile))
		} else {
			fmt.Printf("stored login: no\n")
		}
		return true, nil
	}
	return false, nil
}

func deleteProfileAuthentication(profile string) error {
	for _, path := range []string{sessionFile(profile), fileTokenPath(profile), credentialSecretsFile(profile)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete saved authentication %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}
