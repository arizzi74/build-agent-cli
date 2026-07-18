package core

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// handleInstanceCommand switches only between already-configured profiles. It
// deliberately never invokes setup or writes profile configuration.
func handleInstanceCommand(ctx context.Context, c *Client, args []string) error {
	if len(args) == 0 || strings.EqualFold(args[0], "select") || strings.EqualFold(args[0], "choose") {
		return c.PromptInstanceSelection(ctx)
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
		slashCommandPrintf("%s %-24s %s  [%s]\n", marker, instance.Name, instance.InstanceURL, configuredInstanceCredentialText(instance))
	}
}

func configuredInstanceCredentialText(instance ProfileInfo) string {
	credentials := make([]string, 0, 3)
	if instance.HasToken {
		credentials = append(credentials, "oauth")
	}
	if instance.HasSession {
		credentials = append(credentials, "web-session")
	}
	if instance.HasLogin {
		credentials = append(credentials, "stored-login")
	}
	if len(credentials) == 0 {
		return "no-credentials"
	}
	return strings.Join(credentials, ",")
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
	choice := ""
	if _, remote := telegramCommandSourceFromContext(ctx); remote {
		options := make([]string, 0, len(instances)+1)
		for _, instance := range instances {
			marker := ""
			if instance.Name == c.opts.Profile {
				marker = " (current)"
			}
			options = append(options, fmt.Sprintf("%s — %s [%s]%s", instance.Name, instance.InstanceURL, configuredInstanceCredentialText(instance), marker))
		}
		options = append(options, "Cancel")
		answer, handled, interactionErr := c.requestTurnInteraction(ctx, turnInteractionRequest{
			Kind: "instance_selection", Prompt: "Choose the ServiceNow instance to use.", Options: options,
		})
		if interactionErr != nil {
			return interactionErr
		}
		if !handled {
			return errors.New("Telegram instance selection is unavailable")
		}
		choice, err = parseInstanceSelectionAnswer(answer, instances)
	} else {
		choice, err = promptInstanceSelection(instances, c.opts.Profile)
	}
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

func parseInstanceSelectionAnswer(answer string, instances []ProfileInfo) (string, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", errors.New("instance selection cannot be empty")
	}
	if strings.EqualFold(answer, "cancel") || strings.EqualFold(answer, "q") {
		return "__cancel__", nil
	}
	if number, err := strconv.Atoi(answer); err == nil {
		if number == len(instances)+1 {
			return "__cancel__", nil
		}
		if number < 1 || number > len(instances) {
			return "", fmt.Errorf("instance selection %d is out of range", number)
		}
		return instances[number-1].Name, nil
	}
	profile, err := resolveInstanceProfile(answer)
	if err != nil {
		return "", fmt.Errorf("choose an instance by number, profile, or URL: %w", err)
	}
	return profile, nil
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

// SetupInstance delegates to the process owner because completing setup
// replaces the active Client just like /instance use.
func (c *Client) SetupInstance(ctx context.Context) error {
	if c.processing {
		return errors.New("cannot set up an instance while a turn is processing")
	}
	if c.instanceSetup == nil {
		return errors.New("instance setup is unavailable in this client runtime")
	}
	return c.instanceSetup(ctx)
}

type newInstanceSetup struct {
	Profile     string
	InstanceURL string
}

// setupNewConfiguredInstance collects only profile identity here. The selected
// runtime/auth mode remains the one bacli was launched with; SwitchInstance
// then performs saved-credential recovery or asks for new credentials through
// the initiating TUI/Telegram front end.
func (c *Client) setupNewConfiguredInstance(ctx context.Context) error {
	setup, err := c.collectNewInstanceSetup(ctx)
	if err != nil {
		return err
	}
	cfg := CLIConfig{InstanceURL: setup.InstanceURL, WSURL: deriveWSURL(setup.InstanceURL)}
	if err := applyConfiguredProjectRoot(&cfg, ""); err != nil {
		return err
	}
	if err := saveProfileConfig(setup.Profile, cfg); err != nil {
		return fmt.Errorf("save new instance profile %q: %w", setup.Profile, err)
	}
	if err := c.SwitchInstance(ctx, setup.Profile); err != nil {
		var cleanupErr error
		if _, statErr := os.Lstat(profileDir(setup.Profile)); statErr == nil {
			cleanupErr = deleteConfiguredInstance(setup.Profile)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			cleanupErr = statErr
		}
		if cleanupErr != nil {
			return fmt.Errorf("set up instance %q: %w (also could not remove incomplete profile: %v)", setup.Profile, err, cleanupErr)
		}
		return fmt.Errorf("set up instance %q: %w; incomplete profile removed", setup.Profile, err)
	}
	slashCommandPrintf("setup complete: %s (profile %s)\n", setup.InstanceURL, setup.Profile)
	return nil
}

func (c *Client) collectNewInstanceSetup(ctx context.Context) (newInstanceSetup, error) {
	instanceURL := ""
	validationNote := ""
	for instanceURL == "" {
		prompt := "ServiceNow instance URL (for example https://dev12345.service-now.com)"
		if validationNote != "" {
			prompt = validationNote + ". " + prompt
		}
		answer, err := c.promptInstanceSetupValue(ctx, "setup_instance_url", prompt, "")
		if err != nil {
			return newInstanceSetup{}, err
		}
		if strings.EqualFold(strings.TrimSpace(answer), "cancel") {
			return newInstanceSetup{}, errors.New("instance setup canceled")
		}
		instanceURL, err = validateSetupInstanceURL(answer)
		if err != nil {
			validationNote = err.Error()
			instanceURL = ""
		}
	}
	existing, err := configuredProfileForInstanceURL(instanceURL)
	if err != nil {
		return newInstanceSetup{}, err
	}
	if existing != "" {
		return newInstanceSetup{}, fmt.Errorf("%s is already configured as profile %q; use /instance use %s", instanceURL, existing, existing)
	}

	defaultProfile := profileNameFromInstanceURL(instanceURL)
	profileNote := ""
	for {
		prompt := fmt.Sprintf("Profile name (reply - to use %s)", defaultProfile)
		if profileNote != "" {
			prompt = profileNote + ". " + prompt
		}
		profile, err := c.promptInstanceSetupValue(ctx, "setup_profile", prompt, defaultProfile)
		if err != nil {
			return newInstanceSetup{}, err
		}
		profile = strings.TrimSpace(profile)
		if strings.EqualFold(profile, "cancel") {
			return newInstanceSetup{}, errors.New("instance setup canceled")
		}
		if !isValidProfile(profile) {
			profileNote = "Profile names may contain only letters, numbers, dash, or underscore"
			continue
		}
		if _, err := os.Lstat(profileDir(profile)); err == nil {
			profileNote = fmt.Sprintf("Profile %q already exists", profile)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return newInstanceSetup{}, fmt.Errorf("inspect profile %q: %w", profile, err)
		}
		return newInstanceSetup{Profile: profile, InstanceURL: instanceURL}, nil
	}
}

func (c *Client) promptInstanceSetupValue(ctx context.Context, kind, prompt, defaultValue string) (string, error) {
	if answer, handled, err := c.requestTurnInteraction(ctx, turnInteractionRequest{Kind: kind, Prompt: prompt}); handled {
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer == "-" && defaultValue != "" {
			return defaultValue, nil
		}
		if answer == "" {
			return "", errors.New("setup input cannot be empty")
		}
		return answer, nil
	}
	answer, err := promptLine(prompt + ": ")
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if (answer == "" || answer == "-") && defaultValue != "" {
		return defaultValue, nil
	}
	return answer, nil
}

func validateSetupInstanceURL(raw string) (string, error) {
	cleaned := cleanInstanceURL(raw)
	parsed, err := url.Parse(cleaned)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return "", errors.New("enter a valid http or https ServiceNow instance URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("enter the instance base URL without a path, query, or fragment")
	}
	return strings.TrimRight(cleaned, "/"), nil
}

func configuredProfileForInstanceURL(instanceURL string) (string, error) {
	instances, err := listProfiles()
	if err != nil {
		return "", err
	}
	for _, instance := range instances {
		if sameInstance(instance.InstanceURL, instanceURL) {
			return instance.Name, nil
		}
	}
	return "", nil
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
