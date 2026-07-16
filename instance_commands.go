package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// handleInstanceCommand switches only between already-configured profiles. It
// deliberately never invokes setup or writes profile configuration.
func handleInstanceCommand(ctx context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "select") || strings.EqualFold(args[0], "choose") {
		if _, remote := telegramCommandSourceFromContext(ctx); remote {
			if len(args) == 0 {
				args = []string{"current"}
			} else {
				args = []string{"list"}
			}
		} else {
			return c.PromptInstanceSelection(ctx)
		}
	}
	switch strings.ToLower(args[0]) {
	case "help":
		slashCommandPrintln("usage: /instance [current|list|use <profile-or-instance>]")
		slashCommandPrintln("opens the configured-instance picker when used without arguments")
		return nil
	case "current", "show":
		slashCommandPrintf("instance: %s\n", c.cfg.InstanceURL)
		slashCommandPrintf("profile: %s\n", c.opts.Profile)
		return nil
	case "list", "ls":
		instances, err := listProfiles()
		if err != nil {
			return err
		}
		if len(instances) == 0 {
			slashCommandPrintln("no instances configured")
			return nil
		}
		printConfiguredInstances(instances, c.opts.Profile)
		return nil
	case "use", "switch", "open":
		if len(args) < 2 {
			return errors.New("usage: /instance use <profile-or-instance>")
		}
		return c.SwitchInstance(ctx, strings.Join(args[1:], " "))
	default:
		return fmt.Errorf("unknown /instance command %q", args[0])
	}
}

func printConfiguredInstances(instances []ProfileInfo, currentProfile string) {
	slashCommandPrintln("instances:")
	for _, instance := range instances {
		marker := " "
		if instance.Name == currentProfile {
			marker = "*"
		}
		creds := []string{}
		if instance.HasToken {
			creds = append(creds, "oauth")
		}
		if instance.HasSession {
			creds = append(creds, "web-session")
		}
		credentialText := "no-credentials"
		if len(creds) > 0 {
			credentialText = strings.Join(creds, ",")
		}
		slashCommandPrintf("%s %-24s %s  [%s]\n", marker, instance.Name, instance.InstanceURL, credentialText)
	}
}

func (c *Client) PromptInstanceSelection(ctx context.Context) error {
	if c.processing {
		return errors.New("cannot switch instance while a turn is processing")
	}
	instances, err := listProfiles()
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return errors.New("no configured instances; run bacli --setup first")
	}
	choice, err := promptInstanceSelection(instances, c.opts.Profile)
	if err != nil {
		return err
	}
	choice = strings.TrimSpace(choice)
	if choice == "" || choice == "__cancel__" || strings.EqualFold(choice, "q") || strings.EqualFold(choice, "cancel") {
		slashCommandPrintln("instance unchanged")
		return nil
	}
	return c.SwitchInstance(ctx, choice)
}

// SwitchInstance delegates ownership of the process-level client replacement
// to main. Keeping the old Client immutable is essential: Close permanently
// closes its lifecycle channels and its goroutines must never be retargeted to
// another profile's credentials or workspace state.
func (c *Client) SwitchInstance(ctx context.Context, selector string) error {
	if c.processing {
		return errors.New("cannot switch instance while a turn is processing")
	}
	if c.instanceSwitch == nil {
		return errors.New("instance switching is unavailable in this client runtime")
	}
	return c.instanceSwitch(ctx, selector)
}

// newConfiguredInstanceClient resolves an existing profile and builds a fully
// separate client with only that profile's persisted state. The caller owns
// authentication/connection and decides when to close/replace the old client.
func newConfiguredInstanceClient(current *Client, selector string) (*Client, string, error) {
	if current == nil {
		return nil, "", errors.New("active client is required")
	}
	profile, err := resolveInstanceProfile(selector)
	if err != nil {
		return nil, "", err
	}
	if profile == current.opts.Profile {
		return nil, profile, nil
	}
	candidate, err := newConfiguredInstanceClientForProfile(current, profile)
	if err != nil {
		return nil, "", err
	}
	return candidate, profile, nil
}

func newConfiguredInstanceClientForProfile(current *Client, profile string) (*Client, error) {
	if current == nil {
		return nil, errors.New("active client is required")
	}
	opts := configuredProfileOptions(current.opts, profile)
	cfg, err := resolveConfig(&opts)
	if err != nil {
		return nil, err
	}
	candidate, err := NewClient(cfg, opts)
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

// configuredProfileOptions retains transport and authentication choices while
// removing startup-only selectors. A running /instance command must load the
// chosen profile's saved endpoint and workspace rather than carry an explicit
// startup URL, websocket override, or conversation from the prior profile.
func configuredProfileOptions(current Options, profile string) Options {
	opts := current
	opts.Profile = profile
	opts.ProfileExplicit = true
	opts.InstanceURL = ""
	opts.WSURL = ""
	opts.Conversation = ""
	opts.Setup = false
	opts.InstanceList = false
	opts.InstanceDelete = ""
	opts.ProfileList = false
	opts.ProfileDelete = ""
	opts.Prompts = nil
	return opts
}

type configuredInstancePrepare func(context.Context, *Client) error
type configuredInstanceConnect func(context.Context, *Client) error
type configuredInstanceProfileSave func(string) error

// transitionConfiguredInstance constructs and connects a separate client
// while the old client remains live. It never resets or retargets old: its
// readers and AMB loops can therefore only mutate their original instance.
// Candidate failures are isolated and leave old plus the persisted active
// profile unchanged.
func transitionConfiguredInstance(ctx context.Context, old *Client, selector string, prepare configuredInstancePrepare, connect configuredInstanceConnect, saveActive configuredInstanceProfileSave) (*Client, string, bool, error) {
	if old == nil {
		return nil, "", false, errors.New("active client is required")
	}
	profile, err := resolveInstanceProfile(selector)
	if err != nil {
		return old, "", false, err
	}
	if profile == old.opts.Profile {
		return old, profile, false, nil
	}
	if err := old.saveCurrentState(); err != nil {
		return old, profile, false, fmt.Errorf("save current %s workspace state: %w", old.opts.Profile, err)
	}
	candidate, err := newConfiguredInstanceClientForProfile(old, profile)
	if err != nil {
		return old, profile, false, err
	}
	fail := func(err error) (*Client, string, bool, error) {
		_ = candidate.Close()
		return old, profile, false, err
	}
	if prepare != nil {
		if err := prepare(ctx, candidate); err != nil {
			return fail(fmt.Errorf("prepare %q authentication: %w", profile, err))
		}
	}
	if connect == nil {
		connect = func(ctx context.Context, candidate *Client) error { return candidate.Connect(ctx) }
	}
	if err := connect(ctx, candidate); err != nil {
		return fail(fmt.Errorf("connect %q: %w", profile, err))
	}
	if saveActive == nil {
		saveActive = saveActiveInstanceProfile
	}
	if err := saveActive(profile); err != nil {
		return fail(fmt.Errorf("persist active instance profile: %w", err))
	}
	_ = old.Close()
	return candidate, profile, true, nil
}
