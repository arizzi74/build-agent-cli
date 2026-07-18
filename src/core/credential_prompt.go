package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

func (c *Client) hasTurnInteractionProvider() bool {
	if c == nil {
		return false
	}
	c.turnFrontendMu.RLock()
	available := c.turnInteractionProvider != nil
	c.turnFrontendMu.RUnlock()
	return available
}

func (c *Client) credentialInputAvailable() bool {
	// Interactive authentication has historically accepted both a terminal and
	// deliberately piped stdin. Noninteractive callers use the separate
	// PrepareConnectionAuthenticationNonInteractive path and never reach these
	// prompts.
	return c != nil
}

func (c *Client) promptCredentialValue(ctx context.Context, kind, prompt string, secret bool) (string, error) {
	if answer, handled, err := c.requestTurnInteraction(ctx, turnInteractionRequest{Kind: kind, Prompt: prompt, Secret: secret}); handled {
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return "", errors.New("credential input cannot be empty")
		}
		return answer, nil
	}
	if secret {
		return promptPassword(prompt + ": ")
	}
	return promptLine(prompt + ": ")
}

func (c *Client) promptServiceNowCredentials(ctx context.Context, suggestedUser, reason string) (string, string, error) {
	if !c.credentialInputAvailable() {
		return "", "", errors.New("ServiceNow reauthentication requires an interactive TUI or the authorized Telegram turn owner")
	}
	suggestedUser = strings.TrimSpace(suggestedUser)
	usernamePrompt := fmt.Sprintf("ServiceNow username for %s (profile %s)", c.cfg.InstanceURL, c.opts.Profile)
	if reason != "" {
		usernamePrompt = reason + ". " + usernamePrompt
	}
	if suggestedUser != "" {
		usernamePrompt += fmt.Sprintf("; current user %s", suggestedUser)
	}
	username, err := c.promptCredentialValue(ctx, "credential_username", usernamePrompt, false)
	if err != nil {
		return "", "", err
	}
	password, err := c.promptCredentialValue(ctx, "credential_password", "ServiceNow password for "+username, true)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return "", "", errors.New("ServiceNow username and password are required")
	}
	return strings.TrimSpace(username), password, nil
}

func (c *Client) confirmCredentialStorage(ctx context.Context, username string) (bool, error) {
	destination := "the profile's private secrets.json file"
	if !c.hasTurnInteractionProvider() {
		destination = credentialSecretsFile(c.opts.Profile)
	}
	prompt := fmt.Sprintf("Store the username and password for %s (%s) in %s? The file is not encrypted and is restricted to the current OS user", c.cfg.InstanceURL, username, destination)
	if answer, handled, err := c.requestTurnInteraction(ctx, turnInteractionRequest{Kind: "approval", Prompt: prompt, Options: []string{"Approve", "Reject"}}); handled {
		if err != nil {
			return false, err
		}
		return parseTurnApprovalAnswer(answer)
	}
	return authConfirm(prompt)
}

func (c *Client) persistCredentialChoice(ctx context.Context, username, password string, removeRejected bool) error {
	store, err := c.confirmCredentialStorage(ctx, username)
	if err != nil {
		return err
	}
	if store {
		if err := saveStoredInstanceCredentials(c.opts.Profile, c.cfg.InstanceURL, username, password); err != nil {
			return err
		}
		c.reportAuthenticationNotice(
			"Stored the ServiceNow login in the profile's private secrets.json file.",
			"saved login credentials: "+credentialSecretsFile(c.opts.Profile), false,
		)
		return nil
	}
	if removeRejected {
		return deleteStoredInstanceCredentials(c.opts.Profile)
	}
	return nil
}

func (c *Client) reportAuthenticationNotice(remote, local string, warning bool) {
	if c != nil && c.hasTurnInteractionProvider() {
		kind := turnPresentationSummary
		if warning {
			kind = turnPresentationRuntimeError
		}
		c.publishTurnPresentation(turnPresentationEvent{Kind: kind, Text: remote})
		return
	}
	fmt.Fprintln(os.Stderr, local)
}
