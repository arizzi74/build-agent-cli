package main

import (
	"context"
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
	Setup               bool
	Prompts             []string
	Conversation        string
	Provider            string
	Model               string
	NoOpen              bool
	AutoApprove         bool
	Debug               bool
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
}

func main() {
	loadDotEnvFiles()
	opts := parseFlags()
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
	if handled, err := handleSessionFlags(opts); handled {
		if err != nil {
			fatal(err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	appScreenEntered := false
	if len(opts.Prompts) == 0 && enterTerminalAppScreen() {
		appScreenEntered = true
		defer leaveTerminalAppScreen()
	}

	fmt.Fprintf(os.Stderr, "profile: %s\n", opts.Profile)
	if opts.Nirvana {
		fmt.Fprintf(os.Stderr, "oauth token cache: %s\n", tokenStorageDescription(opts.Profile))
		fmt.Fprintf(os.Stderr, "model: provider=%s large=%s small=%s\n", client.runtime.Provider, client.runtime.LargeModel, client.runtime.SmallModel)
		fmt.Fprintln(os.Stderr, "transport: Nirvana websocket; matches the Glider Build Agent web client streaming path")
	} else {
		if opts.CodeAssistWS {
			fmt.Fprintln(os.Stderr, "transport: experimental Code Assist websocket")
		} else {
			fmt.Fprintln(os.Stderr, "transport: web Build Agent gateway; MCP tools should match the web UI")
		}
		mode, _ := normalizeAuthMode(opts.AuthMode)
		fmt.Fprintf(os.Stderr, "auth: %s; pure Go/no browser automation; secrets stay under ~/.ba-cli when persisted\n", mode)
	}

	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := client.Connect(connectCtx); err != nil {
		cancel()
		if appScreenEntered {
			leaveTerminalAppScreen()
		}
		fatal(err)
	}
	cancel()
	defer client.Close()

	if len(opts.Prompts) > 0 {
		for _, prompt := range opts.Prompts {
			if err := runPrompt(ctx, client, prompt, opts.TurnTimeout); err != nil {
				fatal(err)
			}
		}
		return
	}

	defer restoreTerminalFooter()
	if !showTerminalFooterTempMessage(client.statusBarState(), startupReadyFooterMessage(), 5*time.Second) {
		fmt.Fprintln(os.Stderr, "Type a message. Commands: /help, /conversation, /mcp, /workspace, /app, /exit, /quit")
	}
	for {
		status := client.statusBarState()
		line, err := promptCommandLine("ba> ", &status)
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "/exit", "/quit":
			return
		}
		if strings.HasPrefix(line, "/") {
			handled, err := handleSlashCommand(ctx, client, line)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			if handled {
				continue
			}
		}
		client.printLiveUserTurn(line)
		capture := startProcessingInputCapture("ba> ", &status)
		err = runPrompt(ctx, client, line, opts.TurnTimeout)
		capture.Stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}

func startupReadyFooterMessage() string {
	return "Connected · type a message · /help /conversation /mcp /workspace /app · /exit /quit"
}

func parseFlags() Options {
	var prompts multiFlag
	opts := Options{Nirvana: true}
	flag.StringVar(&opts.InstanceURL, "instance", "", "ServiceNow instance URL, e.g. https://dev12345.service-now.com")
	flag.BoolVar(&opts.InstanceList, "instance-list", false, "list configured ServiceNow instances and exit")
	flag.BoolVar(&opts.InstanceList, "instances", false, "alias for --instance-list")
	flag.StringVar(&opts.InstanceDelete, "instance-delete", "", "delete configured instance by profile, FQDN, or URL and exit")
	flag.StringVar(&opts.WSURL, "ws-url", "", "override Build Agent websocket URL")
	flag.StringVar(&opts.Profile, "profile", "default", "profile name; stores config/token under ~/.ba-cli/profiles/<profile>/")
	flag.BoolVar(&opts.ProfileList, "profile-list", false, "list configured profiles and exit")
	flag.StringVar(&opts.ProfileDelete, "profile-delete", "", "delete a named profile and exit; refuses to delete the active --profile")
	flag.BoolVar(&opts.Setup, "setup", false, "prompt for instance URL and save it to the active profile")
	flag.Var(&prompts, "prompt", "message to send after connecting; repeat for multi-turn scripted use")
	flag.StringVar(&opts.Conversation, "conversation", "", "select Build Agent conversation by number, id, id-prefix, 'latest', or 'new' before prompts/REPL")
	flag.StringVar(&opts.Provider, "provider", "", "model provider: openai, bedrock, anthropic, vertex, nowllm; default bedrock")
	flag.StringVar(&opts.Model, "model", "", "large model override, e.g. claude-opus-4-6, gemini_large, gpt_large")
	flag.BoolVar(&opts.NoOpen, "no-open", false, "print OAuth URL but do not try to open a browser")
	flag.BoolVar(&opts.AutoApprove, "auto-approve", false, "auto-approve approval/client prompts with safe defaults")
	flag.BoolVar(&opts.Debug, "debug", false, "print raw websocket JSON frames to stderr")
	flag.BoolVar(&opts.Nirvana, "nirvana", true, "use the Glider Build Agent Nirvana websocket transport for web UI streaming parity (default)")
	flag.BoolVar(&opts.WebGateway, "web-gateway", false, "use legacy web Build Agent gateway/AMB transport instead of Nirvana")
	flag.BoolVar(&opts.CodeAssistWS, "code-assist-ws", false, "experimental: use /sncapps/code/assist/ba/web-socket instead of REST+AMB web gateway")
	flag.StringVar(&opts.AuthMode, "auth", "form", "web gateway auth mode: form, cookie, or basic; ignored with --nirvana")
	flag.StringVar(&opts.BasicUser, "user", "", "ServiceNow username for web gateway basic/form auth; password is prompted and never stored")
	flag.BoolVar(&opts.Logout, "logout", false, "delete the saved web session for the active profile and exit")
	flag.BoolVar(&opts.SessionStatus, "session-status", false, "show saved web session status for the active profile and exit")
	flag.BoolVar(&opts.AdvertiseLocalTools, "advertise-local-tools", false, "advertise tools.execute; local tools are mostly not implemented in this first Go version")
	flag.StringVar(&opts.ApplicationIDList, "application-id-list", "", "comma-separated sys_app ids for Build Agent conversation listing; mirrors the web UI application_id_list query")
	turnTimeout := flag.Duration("turn-timeout", 10*time.Minute, "timeout per agent turn")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "profile" {
			opts.ProfileExplicit = true
		}
	})
	opts.Prompts = prompts
	opts.TurnTimeout = *turnTimeout
	if opts.WebGateway || opts.CodeAssistWS {
		opts.Nirvana = false
	}
	return opts
}

func runPrompt(ctx context.Context, client *Client, prompt string, timeout time.Duration) error {
	if err := client.SendMessage(ctx, prompt); err != nil {
		return err
	}
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.WaitTurn(turnCtx)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
