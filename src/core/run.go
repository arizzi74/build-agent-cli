package core

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type Options struct {
	InstanceURL         string
	InstanceList        bool
	InstanceDelete      string
	WSURL               string
	Profile             string
	ProfileExplicit     bool
	ProfileList         bool
	ProfileDelete       string
	ProjectRoot         string
	Setup               bool
	Prompts             []string
	Conversation        string
	Provider            string
	Model               string
	NoOpen              bool
	AutoApprove         bool
	Debug               bool
	DebugFile           string
	Nirvana             bool
	WebGateway          bool
	CodeAssistWS        bool
	AuthMode            string
	BasicUser           string
	Logout              bool
	SessionStatus       bool
	AdvertiseLocalTools bool
	ApplicationIDList   string
	TurnTimeout         time.Duration
	TelegramOnly        bool
	TelegramSetup       bool
	TelegramStatus      bool
	TelegramApprove     string
	Version             bool
}

// Run starts the bacli application and handles its full command-line lifecycle.
func Run() {
	if versionFlagRequested(os.Args[1:]) {
		fmt.Println(cliVersion)
		return
	}
	loadDotEnvFiles()
	if updated, message := checkForSelfUpdate(); updated {
		fmt.Fprintln(os.Stderr, message)
		return
	}
	opts := parseFlags()
	if opts.Version {
		fmt.Println(cliVersion)
		return
	}
	if _, err := openDebugTraceFile(opts.DebugFile); err != nil {
		fatal(fmt.Errorf("could not open debug trace file %q: %w", opts.DebugFile, err))
	}
	defer closeDebugOutputLog()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if handled, err := handleInstanceFlags(opts); handled {
		if err != nil {
			fatal(err)
		}
		return
	}
	if handled, err := handleProfileFlags(opts); handled {
		if err != nil {
			fatal(err)
		}
		return
	}
	if handled, err := handleTelegramOfflineFlags(ctx, opts); handled {
		if err != nil {
			fatal(err)
		}
		return
	}
	if handled, err := handleSessionFlags(opts); handled {
		if err != nil {
			fatal(err)
		}
		return
	}
	// Keep the optional bot token out of OAuth/browser, Node/npm, and other child
	// process environments. The in-memory copy is passed only to the bot client.
	telegramEnvironmentToken := takeTelegramEnvironmentToken()

	if err := selectStartupInstance(&opts); err != nil {
		fatal(err)
	}

	cfg, err := resolveConfig(&opts)
	if err != nil {
		fatal(err)
	}
	if opts.Profile != "" {
		_ = saveActiveInstanceProfile(opts.Profile)
	}

	client, err := NewClient(cfg, opts)
	if err != nil {
		fatal(err)
	}
	if err := client.PrepareConnectionAuthentication(ctx); err != nil {
		fatal(err)
	}

	appScreenEntered := false
	if len(opts.Prompts) == 0 && !opts.TelegramOnly && enterTerminalAppScreen() {
		appScreenEntered = true
		defer func() {
			if appScreenEntered {
				leaveTerminalAppScreen()
			}
		}()
	}

	startupDetails := startupConnectionDetails(opts, client)
	if !appScreenEntered {
		fmt.Fprint(os.Stderr, startupDetails)
	}

	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	stopConnectingStatus := func() {}
	stopConnectingInput := startConnectingInputCapture(cancel)
	if appScreenEntered {
		stopConnectingStatus = startTerminalConnectingStatus(startupDetails)
	}
	if err := client.Connect(connectCtx); err != nil {
		stopConnectingInput()
		stopConnectingStatus()
		cancel()
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			if appScreenEntered {
				leaveTerminalAppScreen()
				appScreenEntered = false
			}
			fmt.Fprintln(os.Stderr, "connection cancelled")
			return
		}
		if appScreenEntered {
			leaveTerminalAppScreen()
			appScreenEntered = false
		}
		fatal(err)
	}
	stopConnectingInput()
	stopConnectingStatus()
	cancel()
	clientRef := newActiveClientRef(client)
	defer func() {
		if active := clientRef.Get(); active != nil {
			_ = active.Close()
		}
	}()

	var switchInstance func(context.Context, string) error
	switchInstance = func(switchCtx context.Context, selector string) error {
		old := clientRef.Get()
		if old == nil {
			return errors.New("active client is unavailable")
		}
		_, remoteSwitch := telegramCommandSourceFromContext(switchCtx)
		wasAppScreen := appScreenEntered
		restoreOldScreen := func() {
			if remoteSwitch || !wasAppScreen {
				return
			}
			if !appScreenEntered && enterTerminalAppScreen() {
				appScreenEntered = true
			}
			status := old.statusBarState()
			old.restoreStartupConversationTranscript(status)
			_ = showTerminalFooterTempMessage(status, startupReadyFooterMessage(), 5*time.Second)
		}
		prepare := func(authCtx context.Context, candidate *Client) error {
			if remoteSwitch {
				return candidate.PrepareConnectionAuthenticationNonInteractive(authCtx)
			}
			if wasAppScreen && appScreenEntered {
				restoreTerminalFooter()
				leaveTerminalAppScreen()
				appScreenEntered = false
			}
			if err := candidate.PrepareConnectionAuthentication(authCtx); err != nil {
				return err
			}
			if wasAppScreen && enterTerminalAppScreen() {
				appScreenEntered = true
			}
			return nil
		}
		connect := func(ctx context.Context, candidate *Client) error {
			connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if remoteSwitch {
				return candidate.Connect(connectCtx)
			}
			stopConnectingStatus := func() {}
			stopConnectingInput := startConnectingInputCapture(cancel)
			if appScreenEntered {
				stopConnectingStatus = startTerminalConnectingStatus(startupConnectionDetails(candidate.opts, candidate))
			}
			err := candidate.Connect(connectCtx)
			stopConnectingInput()
			stopConnectingStatus()
			if err != nil && wasAppScreen && appScreenEntered {
				leaveTerminalAppScreen()
				appScreenEntered = false
			}
			return err
		}

		candidate, profile, switched, err := transitionConfiguredInstance(switchCtx, old, selector, prepare, connect, saveActiveInstanceProfile)
		if err != nil {
			restoreOldScreen()
			return err
		}
		if !switched {
			slashCommandPrintf("instance unchanged: %s (profile %s)\n", old.cfg.InstanceURL, profile)
			return nil
		}

		candidate.instanceSwitch = switchInstance
		clientRef.Set(candidate)
		candidate.drawPersistentStatus()
		if wasAppScreen {
			status := candidate.statusBarState()
			candidate.replaceTerminalConversationTranscript(status)
			_ = showTerminalFooterTempMessage(status, startupReadyFooterMessage(), 5*time.Second)
		}
		slashCommandPrintf("switched instance: %s (profile %s)\n", candidate.cfg.InstanceURL, profile)
		return nil
	}
	clientRef.Get().instanceSwitch = switchInstance

	telegram, telegramErr := maybeStartTelegramService(ctx, opts.Profile, clientRef, opts.TurnTimeout, telegramEnvironmentToken)
	if telegramErr != nil {
		if opts.TelegramOnly {
			fatal(telegramErr)
		}
		if errors.Is(telegramErr, errTelegramChannelInUse) {
			showTelegramChannelInUseWarning(clientRef.Get().statusBarState())
		} else {
			clientRef.Get().printRuntimeError("warning: Telegram channel unavailable: " + telegramErr.Error())
		}
	}
	if telegram != nil {
		defer telegram.Close()
	}
	if opts.TelegramOnly {
		if len(opts.Prompts) > 0 {
			fatal(errors.New("--telegram-only cannot be combined with --prompt"))
		}
		if telegram == nil {
			fatal(errors.New("--telegram-only requires an enabled Telegram policy and a secure token file or BACLI_TELEGRAM_BOT_TOKEN"))
		}
		select {
		case <-ctx.Done():
		case err := <-telegram.Fatal():
			fatal(err)
		}
		return
	}

	if len(opts.Prompts) > 0 {
		for _, prompt := range opts.Prompts {
			if _, err := runPromptForActiveClient(ctx, clientRef, prompt, opts.TurnTimeout); err != nil {
				fatal(err)
			}
		}
		return
	}

	startupClient := clientRef.Get()
	startupStatus := startupClient.statusBarState()
	startupClient.restoreStartupConversationTranscript(startupStatus)
	showStartupLocalBuildWarning(startupStatus)
	defer restoreTerminalFooter()
	if !showTerminalFooterTempMessage(startupStatus, startupReadyFooterMessage(), 5*time.Second) {
		fmt.Fprintln(os.Stderr, "Type a message. Commands: /help, /instance, /conversation, /mcp, /workspace, /app, /exit, /quit")
	}
	for {
		promptClient := clientRef.Get()
		status := promptClient.statusBarState()
		line, err := promptCommandLineForClient("ba> ", &status, promptClient)
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" && clientRef.Get().pendingAttachmentCount() == 0 {
			continue
		}
		switch strings.ToLower(line) {
		case "/exit", "/quit":
			return
		}
		if strings.HasPrefix(line, "/") {
			active, handled, err := handleSlashCommandForActiveClient(ctx, clientRef, line)
			if err != nil {
				message := fmt.Sprintf("error: %v", err)
				if active == nil || !terminalRecordSystemTextAndAppend("Command", message, active.statusBarState()) {
					fmt.Fprintln(os.Stderr, message)
				}
			}
			if handled {
				continue
			}
		}
		bacliActionMu.Lock()
		active := clientRef.Get()
		status = active.statusBarState()
		active.printLiveUserTurn(line)
		terminalRelay := newTelegramTerminalTurnRelay(telegram)
		if terminalRelay != nil {
			terminalRelay.addPrompt(line)
		}
		restoreTurnFrontend := func() {}
		if terminalRelay != nil {
			restoreTurnFrontend = active.installTurnFrontend(terminalRelay.presentation, nil)
		}
		capture := startProcessingInputCapture("ba> ", &status, func() bool {
			if current := clientRef.Get(); current != nil {
				return current.cancelActiveTurn()
			}
			return false
		})
		response, turnErr := runPromptForResponseLocked(ctx, active, line, opts.TurnTimeout)
		capture.Stop()
		restoreTurnFrontend()
		connectionRecovered := false
		if turnErr != nil && turnConnectionNeedsRecovery(turnErr) {
			wasAppScreen := appScreenEntered
			if wasAppScreen {
				restoreTerminalFooter()
				leaveTerminalAppScreen()
				appScreenEntered = false
			}
			recovered, recoveryErr := active.recoverConnectionAfterTurnError(ctx, turnErr)
			if wasAppScreen && enterTerminalAppScreen() {
				appScreenEntered = true
				status = active.statusBarState()
				active.replaceTerminalConversationTranscript(status)
			}
			if recovered {
				connectionRecovered = true
				response = connectionRestoredMessage
				turnErr = nil
			} else if recoveryErr != nil {
				turnErr = fmt.Errorf("%v; connection recovery: %w", turnErr, recoveryErr)
			}
		}
		bacliActionMu.Unlock()
		if terminalRelay != nil {
			if relayErr := terminalRelay.finish(); relayErr != nil && ctx.Err() == nil {
				active.printRuntimeError("warning: Telegram local-turn progress send failed: " + relayErr.Error())
			}
			if turnErr == nil {
				terminalRelay.sendFinal(response)
			}
		}
		err = turnErr
		if connectionRecovered {
			if !terminalRecordSystemTextAndAppend("Connection", connectionRestoredMessage, status) {
				fmt.Fprintln(os.Stderr, connectionRestoredMessage)
			}
		} else if errors.Is(err, context.Canceled) {
			clearTerminalFooterTempMessage()
			if !terminalRecordSystemTextAndAppend("Action", "Cancelled", status) {
				fmt.Fprintln(os.Stderr, "Action cancelled")
			}
		} else if err != nil {
			active.printRuntimeError(fmt.Sprintf("error: %v", err))
		}
	}
}

func startupConnectionDetails(opts Options, client *Client) string {
	var out strings.Builder
	fmt.Fprintf(&out, "profile: %s\n", opts.Profile)
	if opts.Nirvana {
		fmt.Fprintf(&out, "oauth token cache: %s\n", tokenStorageDescription(opts.Profile))
		if client != nil {
			fmt.Fprintf(&out, "model: provider=%s large=%s small=%s\n", client.runtime.Provider, client.runtime.LargeModel, client.runtime.SmallModel)
		}
		out.WriteString("transport: Nirvana websocket; matches the Glider Build Agent web client streaming path\n")
		return out.String()
	}
	if opts.CodeAssistWS {
		out.WriteString("transport: experimental Code Assist websocket\n")
	} else {
		out.WriteString("transport: web Build Agent gateway; MCP tools should match the web UI\n")
	}
	mode, _ := normalizeAuthMode(opts.AuthMode)
	fmt.Fprintf(&out, "auth: %s; pure Go/no browser automation; secrets stay under ~/.ba-cli when persisted\n", mode)
	return out.String()
}

func startupReadyFooterMessage() string {
	return "Connected · type a message · /help /instance /conversation /mcp /workspace /app · /exit /quit"
}

func extractDebugFileArg(args []string) ([]string, string, error) {
	out := make([]string, 0, len(args))
	debugFile := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-debug" || arg == "--debug" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, "", fmt.Errorf("%s requires a filename", arg)
			}
			debugFile = args[i+1]
			i++
			continue
		}
		for _, prefix := range []string{"-debug=", "--debug="} {
			if strings.HasPrefix(arg, prefix) {
				value := strings.TrimSpace(strings.TrimPrefix(arg, prefix))
				if !looksLikeDebugFilename(value) {
					return nil, "", fmt.Errorf("%s requires a filename", strings.TrimSuffix(prefix, "="))
				}
				debugFile = value
				goto nextArg
			}
		}
		out = append(out, arg)
	nextArg:
	}
	return out, debugFile, nil
}

func looksLikeDebugFilename(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "true", "false", "1", "0", "t", "f":
		return false
	default:
		return true
	}
}

func parseFlags() Options {
	var prompts multiFlag
	normalizedArgs, debugFileFromArgs, err := extractDebugFileArg(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	opts := Options{Nirvana: true, DebugFile: debugFileFromArgs}
	flag.StringVar(&opts.InstanceURL, "instance", "", "ServiceNow instance URL, e.g. https://dev12345.service-now.com")
	flag.BoolVar(&opts.InstanceList, "instance-list", false, "list configured ServiceNow instances and exit")
	flag.BoolVar(&opts.InstanceList, "instances", false, "alias for --instance-list")
	flag.StringVar(&opts.InstanceDelete, "instance-delete", "", "delete configured instance by profile, FQDN, or URL and exit")
	flag.StringVar(&opts.WSURL, "ws-url", "", "override Build Agent websocket URL")
	flag.StringVar(&opts.Profile, "profile", "default", "ServiceNow runtime profile; Telegram channel configuration is global")
	flag.BoolVar(&opts.ProfileList, "profile-list", false, "list configured profiles and exit")
	flag.StringVar(&opts.ProfileDelete, "profile-delete", "", "delete a named profile and exit; refuses to delete the active --profile")
	flag.BoolVar(&opts.Setup, "setup", false, "prompt for instance URL and save it to the active profile")
	flag.StringVar(&opts.ProjectRoot, "project-root", "", "canonical project root (absolute path or ~/...); saves to an existing profile, or use with --setup")
	flag.Var(&prompts, "prompt", "message to send after connecting; repeat for multi-turn scripted use")
	flag.StringVar(&opts.Conversation, "conversation", "", "select Build Agent conversation by number, id, id-prefix, 'latest', or 'new' before prompts/REPL")
	flag.StringVar(&opts.Provider, "provider", "", "model provider: openai, bedrock, anthropic, vertex, nowllm; default bedrock")
	flag.StringVar(&opts.Model, "model", "", "large model override, e.g. claude-opus-4-6, gemini_large, gpt_large")
	flag.BoolVar(&opts.NoOpen, "no-open", false, "print OAuth URL but do not try to open a browser")
	flag.BoolVar(&opts.AutoApprove, "auto-approve", false, "auto-approve approval/client prompts with safe defaults")
	flag.StringVar(&opts.DebugFile, "debug-file", debugFileFromArgs, "write terminal output plus redacted debug trace to file and enable debug mode")
	flag.BoolVar(&opts.Nirvana, "nirvana", true, "use the Glider Build Agent Nirvana websocket transport for web UI streaming parity (default)")
	flag.BoolVar(&opts.WebGateway, "web-gateway", false, "use legacy web Build Agent gateway/AMB transport instead of Nirvana")
	flag.BoolVar(&opts.CodeAssistWS, "code-assist-ws", false, "experimental: use /sncapps/code/assist/ba/web-socket instead of REST+AMB web gateway")
	flag.StringVar(&opts.AuthMode, "auth", "form", "ServiceNow auth: form session (default), cookie recovery; basic is --web-gateway-only")
	flag.StringVar(&opts.BasicUser, "user", "", "ServiceNow username for web gateway basic/form auth; password is prompted and never stored")
	flag.BoolVar(&opts.Logout, "logout", false, "delete saved web-session and OAuth credentials for the active profile and exit")
	flag.BoolVar(&opts.SessionStatus, "session-status", false, "show saved web session status for the active profile and exit")
	flag.BoolVar(&opts.Version, "version", false, "print the bacli version and exit")
	flag.BoolVar(&opts.AdvertiseLocalTools, "advertise-local-tools", false, "advertise tools.execute; local tools are mostly not implemented in this first Go version")
	flag.StringVar(&opts.ApplicationIDList, "application-id-list", "", "comma-separated sys_app ids for Build Agent conversation listing; mirrors the web UI application_id_list query")
	flag.BoolVar(&opts.TelegramOnly, "telegram-only", false, "run headless with the private Telegram command channel")
	flag.BoolVar(&opts.TelegramSetup, "telegram-setup", false, "configure the global private Telegram channel with a guided wizard and exit")
	flag.BoolVar(&opts.TelegramStatus, "telegram-status", false, "show global Telegram pairing/configuration status and exit without connecting")
	flag.StringVar(&opts.TelegramApprove, "telegram-approve", "", "approve a one-hour Telegram pairing code offline and exit")
	turnTimeout := flag.Duration("turn-timeout", 60*time.Minute, "timeout per agent turn")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "  -debug filename")
		fmt.Fprintln(flag.CommandLine.Output(), "    \twrite terminal output plus redacted debug trace to filename; debug without filename is invalid")
		flag.PrintDefaults()
	}
	_ = flag.CommandLine.Parse(normalizedArgs)
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "profile" {
			opts.ProfileExplicit = true
		}
	})
	if strings.TrimSpace(opts.DebugFile) != "" {
		opts.Debug = true
	}
	opts.Prompts = prompts
	opts.TurnTimeout = *turnTimeout
	if opts.WebGateway || opts.CodeAssistWS {
		opts.Nirvana = false
	}
	return opts
}

func versionFlagRequested(args []string) bool {
	for _, arg := range args {
		if arg == "-version" || arg == "--version" {
			return true
		}
	}
	return false
}

func runPrompt(ctx context.Context, client *Client, prompt string, timeout time.Duration) error {
	_, err := runPromptForResponse(ctx, client, prompt, timeout)
	return err
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	_ = closeDebugOutputLog()
	os.Exit(1)
}
