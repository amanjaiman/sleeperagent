// Command agentkeeper is a cross-agent watchdog that resumes Claude Code / Codex
// sessions when their usage limits reset. It launches an agent inside a managed
// tmux session, detects the limit, waits for the reset, and injects a resume
// prompt — and gets out of the way the moment a human takes over.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
	"github.com/amanjaiman/agentkeeper/internal/config"
	"github.com/amanjaiman/agentkeeper/internal/hotkeys"
	"github.com/amanjaiman/agentkeeper/internal/notify"
	"github.com/amanjaiman/agentkeeper/internal/ollama"
	"github.com/amanjaiman/agentkeeper/internal/parser"
	"github.com/amanjaiman/agentkeeper/internal/prompt"
	"github.com/amanjaiman/agentkeeper/internal/ptybackend"
	"github.com/amanjaiman/agentkeeper/internal/statefile"
	"github.com/amanjaiman/agentkeeper/internal/supervisor"
	"github.com/amanjaiman/agentkeeper/internal/tmux"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "attach-existing":
		err = attachExistingCmd(os.Args[2:])
	case "agents":
		err = agentsCmd(os.Args[2:])
	case "parse":
		err = parseCmd(os.Args[2:])
	case "install":
		err = installCmd(os.Args[2:])
	case "status":
		err = statusCmd(os.Args[2:])
	case "logs":
		err = logsCmd(os.Args[2:])
	case "detach":
		err = controlCmd(os.Args[2:], "detach")
	case "stop":
		err = stopCmd(os.Args[2:])
	case "rm":
		err = rmCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("agentkeeper %s\n", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentkeeper — resume coding agents when usage limits reset

Usage:
  agentkeeper run             [flags] [-- launch-command...]  launch + watch an agent
  agentkeeper attach-existing --target T [flags]              watch an already-running tmux session
  agentkeeper agents          [--config PATH]                 list configured adapters
  agentkeeper parse           --agent A "limit text..."       test a limit string against patterns
  agentkeeper install         [--dir DIR] [--force]           copy this binary to a PATH directory
  agentkeeper status          [--name NAME]                   report state / countdown
  agentkeeper logs            --name NAME [--follow]          print / tail the instance log
  agentkeeper detach          --name NAME                     stop watching, keep session
  agentkeeper stop            --name NAME [--kill]             stop watching (optionally kill)
  agentkeeper rm              --name NAME [--force] | --all    remove a stale/ended instance record
  agentkeeper version                                         print version

Run flags:
  --agent    string  agent adapter to use (default "claude")
  --name     string  instance / tmux session name (default "agentkeeper-<agent>")
  --prompt   string  static resume prompt to inject on reset
  --reprompt string  local-LLM reprompt, e.g. "ollama:llama3.1" (falls back to static)
  --backend  string  session backend: "tmux" (default) or "pty" (no-tmux fallback)
  --webhook  string  POST notifications to this URL
  --config   string  path to config.toml (default: OS config dir)
  --daemon           run in the background; control via status/detach/stop
                     (tmux backend keeps full handoff; pty backend ends on detach)
  --watch-only       notify at reset but do NOT auto-inject
  --yolo             append the agent's skip-permissions flag (DANGEROUS, unattended)
  --no-auto-detach   do NOT auto-detach when you attach to the session
  --no-notify        disable desktop notifications

The trailing "-- launch-command..." is optional: omit it to use the adapter's
default command (the "claude" adapter runs "claude"). Pass it only to launch
something different — your own flags, a wrapper, or another binary.

Examples:
  agentkeeper run --agent claude --name feature-x
  agentkeeper run --agent codex --prompt "Continue; run the tests."
  agentkeeper run --agent claude -- claude --model opus   # custom launch command
  agentkeeper attach-existing --agent claude --target mywork:0.1
  agentkeeper parse --agent claude "5-hour limit reached ∙ resets 2pm"
  agentkeeper status
`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent adapter to use")
	name := fs.String("name", "", "instance / tmux session name")
	promptText := fs.String("prompt", "", "static resume prompt")
	cfgPath := fs.String("config", "", "path to config.toml")
	noAutoDetach := fs.Bool("no-auto-detach", false, "do not auto-detach on user activity")
	reprompt := fs.String("reprompt", "", `local-LLM reprompt, e.g. "ollama:llama3.1"`)
	backend := fs.String("backend", defaultBackend(), `session backend: "tmux" or "pty"`)
	webhookURL := fs.String("webhook", "", "POST notifications to this URL")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	watchOnly := fs.Bool("watch-only", false, "notify at reset but do NOT auto-inject")
	yolo := fs.Bool("yolo", false, "append the agent's skip-permissions flag (DANGEROUS)")
	daemon := fs.Bool("daemon", false, "run in the background; control via status/detach/stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ad, err := cfg.Adapter(*agent)
	if err != nil {
		return err
	}

	launch := ad.LaunchCmd
	if rest := fs.Args(); len(rest) > 0 {
		launch = strings.Join(rest, " ")
	}
	if *yolo {
		if ad.YoloFlag == "" {
			return fmt.Errorf("--yolo requested but agent %q has no yolo_flag configured", *agent)
		}
		launch += " " + ad.YoloFlag
		log.Printf("⚠ --yolo: launching with %q — tool calls will run UNATTENDED with no permission prompts", ad.YoloFlag)
	}

	instance := *name
	if instance == "" {
		instance = "agentkeeper-" + *agent
	}

	// Background mode: re-exec ourselves detached from the terminal and return.
	// The child (marked by the env var) skips this branch and runs normally,
	// logging to a file. Control is then purely via status/detach/stop.
	if *daemon && os.Getenv(daemonEnv) == "" {
		switch *backend {
		case "tmux":
			// full handoff: the session survives the supervisor
		case "pty":
			fmt.Println("note: --daemon with the pty backend — the agent is bound to the supervisor, " +
				"so detach/stop ends it (no reattach). Use the tmux backend for full handoff.")
		default:
			return fmt.Errorf("unknown backend %q (use \"tmux\" or \"pty\")", *backend)
		}
		return daemonize(instance, args)
	}

	cwd, _ := os.Getwd()
	resumeText := *promptText
	if resumeText == "" {
		resumeText = prompt.DefaultText
	}

	builder, promptMode, err := buildBuilder(cfg, ad, *reprompt, resumeText)
	if err != nil {
		return err
	}

	// Select the session backend: tmux (default, full handoff) or a self-managed
	// pty (no-tmux fallback, reduced handoff).
	var pane supervisor.Pane
	var startSession func() error
	var foreground func(ctx context.Context)
	var afterDetach func(ctx context.Context)
	var attachHint string
	var restoreLogOutput func()
	switch *backend {
	case "tmux":
		tx := tmux.New(instance, "")
		if err := tx.Available(); err != nil {
			return err
		}
		if tx.HasSession() {
			return fmt.Errorf("tmux session %q already exists; resume watching it with "+
				"`agentkeeper attach-existing --agent %s --target %s`, or pick another --name",
				instance, *agent, instance)
		}
		pane, attachHint = tx, tx.AttachHint()
		startSession = func() error {
			log.Printf("launching %q in tmux session %q", launch, instance)
			return tx.NewSession(launch)
		}
		afterDetach = func(context.Context) {
			log.Printf("detached. session %q left running — reattach with: %s", instance, tx.AttachHint())
		}
	case "pty":
		restore, err := redirectLogsToInstanceFile(instance)
		if err != nil {
			return err
		}
		restoreLogOutput = restore
		defer restoreLogOutput()
		pc, perr := ptybackend.New()
		if perr != nil {
			return perr
		}
		defer pc.Close()
		pane, attachHint = pc, pc.AttachHint()
		startSession = func() error {
			log.Printf("launching %q in a pty", launch)
			return pc.Start(launch)
		}
		foreground = func(ctx context.Context) {
			_ = pc.Foreground(ctx)
		}
		afterDetach = func(ctx context.Context) {
			log.Printf("detached. watchdog stopped; agent keeps this terminal until it exits")
			pc.Wait(ctx)
		}
	default:
		return fmt.Errorf("unknown backend %q (use \"tmux\" or \"pty\")", *backend)
	}

	return watchSession(watchParams{
		instance:       instance,
		agent:          *agent,
		adapter:        ad,
		pane:           pane,
		attachHint:     attachHint,
		startSession:   startSession,
		foreground:     foreground,
		afterDetach:    afterDetach,
		builder:        builder,
		promptMode:     promptMode,
		resumeText:     resumeText,
		cfg:            cfg,
		cwd:            cwd,
		autoDetach:     !*noAutoDetach,
		watchOnly:      *watchOnly,
		notifier:       buildNotifier(*noNotify, *webhookURL),
		transparentTTY: *backend == "pty",
	})
}

// watchParams bundles everything watchSession needs so both `run` and
// `attach-existing` share the same observe/wait/resume core.
type watchParams struct {
	instance       string
	agent          string
	adapter        *adapter.Adapter
	pane           supervisor.Pane
	attachHint     string
	startSession   func() error // nil when the session already exists (attach-existing)
	foreground     func(ctx context.Context)
	afterDetach    func(ctx context.Context)
	builder        prompt.Builder
	promptMode     string
	resumeText     string
	cfg            config.Config
	cwd            string
	autoDetach     bool
	watchOnly      bool
	notifier       notify.Notifier
	transparentTTY bool
}

// watchSession runs the supervisor loop against an already-prepared backend.
func watchSession(p watchParams) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if p.startSession != nil {
		if err := p.startSession(); err != nil {
			return err
		}
	}
	if p.foreground != nil {
		go p.foreground(ctx)
	}
	log.Printf("watching. take over any time with: %s", p.attachHint)
	if p.watchOnly {
		log.Printf("watch-only: will notify at reset but NOT auto-inject")
	}
	if p.promptMode != "static" {
		log.Printf("reprompt: %s enabled (falls back to static prompt on any failure)", p.promptMode)
	}
	if p.transparentTTY {
		log.Printf("pty pass-through: stdin/stdout are reserved for the agent; from another shell use `agentkeeper logs --name %s -f`, `agentkeeper status`, or `agentkeeper detach/stop --name %s`", p.instance, p.instance)
	} else {
		log.Printf("%s", hotkeys.Legend)
	}

	var prevState string
	writeRecord := func(snap supervisor.Snapshot) {
		rec := statefile.Record{
			Name:            p.instance,
			Agent:           p.agent,
			Session:         p.instance,
			PromptMode:      p.promptMode,
			PromptText:      p.resumeText,
			PID:             os.Getpid(),
			AttachHint:      p.attachHint,
			State:           string(snap.State),
			ResetTime:       snap.Reset.Time,
			ResetSource:     snap.Reset.Source,
			ResetConfidence: snap.Reset.Confidence,
			WaitUntil:       snap.WaitUntil,
		}
		if err := statefile.Write(rec); err != nil {
			log.Printf("warning: could not write state file: %v", err)
		}
		notifyTransition(p.notifier, prevState, snap, p.instance)
		prevState = string(snap.State)
	}
	writeRecord(supervisor.Snapshot{State: "RUNNING"})

	cmds := make(chan supervisor.Command, 4)

	// Foreground hotkeys (no-op when stdin isn't a TTY). Raw mode needs CRLF, so
	// route the logger through a translating writer while hotkeys are active.
	if !p.transparentTTY && hotkeys.Listen(ctx, cmds, log.Printf) {
		log.SetOutput(crlfWriter{os.Stderr})
		defer log.SetOutput(os.Stderr)
	}

	// Control-file poller: `agentkeeper detach`/`stop` from another shell drops a
	// command file that we pick up here and forward to the supervisor.
	go pollControl(ctx, p.instance, cmds)

	sup := supervisor.New(supervisor.Options{
		Adapter:      p.adapter,
		Tmux:         p.pane,
		Prompt:       p.builder,
		PollInterval: p.cfg.PollInterval.D(),
		ResetBuffer:  p.cfg.ResetBuffer.D(),
		MaxWait:      p.cfg.MaxWait.D(),
		Cwd:          p.cwd,
		AutoDetach:   p.autoDetach,
		WatchOnly:    p.watchOnly,
		Commands:     cmds,
		OnUpdate:     writeRecord,
		OnManualAction: func(msg string) {
			p.notifier.Notify(notify.Event{
				Title: "AgentKeeper [" + p.instance + "]: manual choice needed",
				Body:  msg,
			})
		},
		Logf: log.Printf,
	})

	if err := sup.Run(ctx); err != nil {
		return err
	}

	if sup.SessionKilled() {
		statefile.Remove(p.instance)
		log.Printf("session %q killed.", p.instance)
		return nil
	}
	if sup.SessionEnded() {
		// The agent exited or the session was killed out from under us; there is
		// no live session to hand back, so skip the reattach hint.
		statefile.Remove(p.instance)
		log.Printf("session %q ended — the agent exited. Nothing left to watch.", p.instance)
		return nil
	}
	p.afterDetach(ctx)
	return nil
}

// buildBuilder constructs the resume-prompt builder (static or local-LLM).
func buildBuilder(cfg config.Config, ad *adapter.Adapter, repromptSpec, resumeText string) (prompt.Builder, string, error) {
	if repromptSpec == "" {
		return prompt.NewStatic(resumeText), "static", nil
	}
	provider, model, _ := strings.Cut(repromptSpec, ":")
	if provider != "ollama" {
		return nil, "", fmt.Errorf("unsupported reprompt provider %q (only \"ollama\" is supported)", provider)
	}
	if model == "" {
		model = cfg.Reprompt.Model
	}
	b := prompt.LLM{
		Model:          model,
		Client:         ollama.New(cfg.Reprompt.BaseURL),
		TranscriptGlob: ad.TranscriptGlob,
		TailMessages:   cfg.Reprompt.TailMessages,
		MaxChars:       cfg.Reprompt.MaxPromptChars,
		Denylist:       cfg.Reprompt.Denylist,
		Fallback:       resumeText,
		Logf:           log.Printf,
	}
	return b, "ollama:" + model, nil
}

// buildNotifier assembles the desktop + webhook notifiers.
func buildNotifier(noNotify bool, webhookURL string) notify.Notifier {
	n := notify.Multi{}
	if !noNotify {
		n = append(n, notify.Desktop{})
	}
	if webhookURL != "" {
		n = append(n, notify.Webhook{URL: webhookURL})
	}
	return n
}

// defaultBackend picks the session backend that works out of the box on the host:
// tmux on Unix, the native ConPTY backend on Windows (where tmux is usually absent).
func defaultBackend() string {
	if runtime.GOOS == "windows" {
		return "pty"
	}
	return "tmux"
}

// daemonEnv marks a re-executed background child so it does not re-daemonize.
const daemonEnv = "AGENTKEEPER_DAEMONIZED"

// daemonize re-executes agentkeeper detached from the controlling terminal, with
// stdout/stderr redirected to a per-instance log file, and returns immediately.
// The child runs the same `run` command with daemonEnv set. The detach mechanism
// is platform-specific (see detach_unix.go / detach_windows.go), so this works on
// Linux, macOS, and Windows.
func daemonize(instance string, runArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := instanceLogPath(instance)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, append([]string{"run"}, runArgs...)...)
	cmd.Env = append(os.Environ(), daemonEnv+"=1")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = daemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background process: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // detach; do not wait

	// Write an initial record so `status` works immediately; the child (same PID)
	// overwrites it with full detail once it starts watching.
	_ = statefile.Write(statefile.Record{Name: instance, State: "RUNNING", PID: pid})

	fmt.Printf("agentkeeper: %q started in background (pid %d)\n", instance, pid)
	fmt.Printf("  logs:   agentkeeper logs --name %s --follow   (file: %s)\n", instance, logPath)
	fmt.Printf("  status: agentkeeper status --name %s\n", instance)
	fmt.Printf("  stop:   agentkeeper detach --name %s   (or: stop --name %s --kill)\n", instance, instance)
	return nil
}

func redirectLogsToInstanceFile(instance string) (func(), error) {
	logPath := instanceLogPath(instance)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	prev := log.Writer()
	log.SetOutput(logFile)
	return func() {
		log.SetOutput(prev)
		_ = logFile.Close()
	}, nil
}

// attachExistingCmd watches an agent already running in a tmux session, without
// launching it. This is also the crash-recovery path: re-attach to the live
// session and the supervisor re-observes (re-detecting a still-shown limit).
func attachExistingCmd(args []string) error {
	fs := flag.NewFlagSet("attach-existing", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent adapter to use")
	name := fs.String("name", "", "instance name for status/state (default: target)")
	target := fs.String("target", "", "tmux target to watch, e.g. mywork:0.1 (required)")
	promptText := fs.String("prompt", "", "static resume prompt")
	cfgPath := fs.String("config", "", "path to config.toml")
	noAutoDetach := fs.Bool("no-auto-detach", false, "do not auto-detach on user activity")
	reprompt := fs.String("reprompt", "", `local-LLM reprompt, e.g. "ollama:llama3.1"`)
	webhookURL := fs.String("webhook", "", "POST notifications to this URL")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	watchOnly := fs.Bool("watch-only", false, "notify at reset but do NOT auto-inject")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("--target is required (the tmux session/pane to watch)")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ad, err := cfg.Adapter(*agent)
	if err != nil {
		return err
	}
	instance := *name
	if instance == "" {
		instance = *target
	}

	tx := tmux.New(*target, "")
	if err := tx.Available(); err != nil {
		return err
	}
	if !tx.HasSession() {
		return fmt.Errorf("no tmux session/pane at target %q", *target)
	}

	cwd, _ := os.Getwd()
	resumeText := *promptText
	if resumeText == "" {
		resumeText = prompt.DefaultText
	}
	builder, promptMode, err := buildBuilder(cfg, ad, *reprompt, resumeText)
	if err != nil {
		return err
	}

	log.Printf("attaching to existing target %q", *target)
	return watchSession(watchParams{
		instance:     instance,
		agent:        *agent,
		adapter:      ad,
		pane:         tx,
		attachHint:   tx.AttachHint(),
		startSession: nil, // already running
		afterDetach: func(context.Context) {
			log.Printf("detached. target %q left running — reattach with: %s", *target, tx.AttachHint())
		},
		builder:    builder,
		promptMode: promptMode,
		resumeText: resumeText,
		cfg:        cfg,
		cwd:        cwd,
		autoDetach: !*noAutoDetach,
		watchOnly:  *watchOnly,
		notifier:   buildNotifier(*noNotify, *webhookURL),
	})
}

// notifyTransition fires a notification on meaningful state changes.
func notifyTransition(n notify.Notifier, prev string, snap supervisor.Snapshot, instance string) {
	cur := string(snap.State)
	if cur == prev {
		return
	}
	tag := "AgentKeeper [" + instance + "]"
	switch cur {
	case "WAITING":
		body := "waiting until reset"
		if !snap.WaitUntil.IsZero() {
			body = "reset ~ " + snap.WaitUntil.Local().Format("Mon 15:04")
		}
		n.Notify(notify.Event{Title: tag + ": usage limit hit", Body: body})
	case "RUNNING":
		if prev == "RESUMING" {
			n.Notify(notify.Event{Title: tag + ": resumed", Body: "agent continued after reset"})
		}
	case "DETACHED":
		n.Notify(notify.Event{Title: tag + ": detached", Body: "supervisor stopped"})
	case "ENDED":
		n.Notify(notify.Event{Title: tag + ": session ended", Body: "the agent exited; watch stopped"})
	}
}

// pollControl forwards detach/kill commands written by other invocations.
func pollControl(ctx context.Context, instance string, cmds chan<- supervisor.Command) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cmd, ok, err := statefile.TakeControl(instance)
			if err != nil || !ok {
				continue
			}
			switch cmd {
			case "kill":
				cmds <- supervisor.CmdKill
			case "detach":
				cmds <- supervisor.CmdDetach
			}
		}
	}
}

func agentsCmd(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	bad := 0
	for _, name := range names {
		ac := cfg.Agents[name]
		// Compiling validates the patterns the same way `run` would, so this
		// command doubles as a config check for format-drift fixes.
		_, cerr := cfg.Adapter(name)
		status := "ok"
		if cerr != nil {
			status = "INVALID: " + cerr.Error()
			bad++
		}
		fmt.Printf("%s  [%s]\n", name, status)
		fmt.Printf("    launch : %s\n", ac.LaunchCmd)
		fmt.Printf("    resume : %s\n", ac.ResumeCmd)
		fmt.Printf("    inject : %s\n", injectStyleOrDefault(ac.InjectStyle))
		for _, p := range ac.LimitPatterns {
			fmt.Printf("    limit  : %s\n", p)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d adapter(s) have invalid patterns", bad)
	}
	return nil
}

// parseCmd lets a user test a captured limit string against an agent's patterns,
// so they can validate/tune patterns against their real CLI without launching it.
func parseCmd(args []string) error {
	fs := flag.NewFlagSet("parse", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent adapter to use")
	cfgPath := fs.String("config", "", "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return fmt.Errorf("provide the limit text to test, e.g. agentkeeper parse --agent claude \"...resets 2pm\"")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ad, err := cfg.Adapter(*agent)
	if err != nil {
		return err
	}

	match, groups, ok := parser.Detect(ad.LimitPatterns, text)
	if !ok {
		fmt.Printf("no limit pattern matched for agent %q.\nrun `agentkeeper agents` to see the patterns.\n", *agent)
		return fmt.Errorf("no match")
	}
	fmt.Printf("matched: %s\n", match)
	for k, v := range groups {
		fmt.Printf("  group %-5s = %q\n", k, v)
	}
	ri := parser.Resolve(groups, time.Now(), 5*time.Hour)
	fmt.Printf("reset: %s\n  source=%s confidence=%s (in %s)\n",
		ri.Time.Local().Format("Mon 2006-01-02 15:04 MST"),
		ri.Source, ri.Confidence, time.Until(ri.Time).Round(time.Second))
	return nil
}

func injectStyleOrDefault(s string) string {
	if s == "" {
		return "text-enter (default)"
	}
	return s
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	name := fs.String("name", "", "show only this instance")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var recs []statefile.Record
	if *name != "" {
		rec, err := statefile.Read(*name)
		if err != nil {
			return fmt.Errorf("no such instance %q (%v)", *name, err)
		}
		recs = []statefile.Record{rec}
	} else {
		var err error
		if recs, err = statefile.List(); err != nil {
			return err
		}
	}
	if len(recs) == 0 {
		fmt.Println("no AgentKeeper instances found")
		return nil
	}

	fmt.Printf("%-20s %-8s %-9s %-22s %s\n", "NAME", "AGENT", "STATE", "RESET", "PROMPT")
	for _, r := range recs {
		state := r.State
		if !processAlive(r.PID) && r.State != "DETACHED" {
			state += "*" // process appears gone; state is the last persisted value
		}
		fmt.Printf("%-20s %-8s %-9s %-22s %s\n",
			r.Name, r.Agent, state, resetCol(r), truncate(r.PromptText, 40))
	}
	if anyStale(recs) {
		fmt.Println("\n* supervisor process not running; shown state is the last persisted value")
	}
	return nil
}

func resetCol(r statefile.Record) string {
	if r.WaitUntil.IsZero() {
		return "—"
	}
	d := time.Until(r.WaitUntil)
	if d <= 0 {
		return r.WaitUntil.Local().Format("15:04") + " (due)"
	}
	return fmt.Sprintf("%s (in %s)", r.WaitUntil.Local().Format("15:04"), d.Round(time.Second))
}

// cleanupIfDead removes an instance's stale record when its supervisor process
// is no longer running, returning true if it handled the (dead) instance. This
// is what lets stop/detach tidy up an instance that already ended on its own
// instead of writing a control command no live supervisor will ever read.
func cleanupIfDead(rec statefile.Record) bool {
	if processAlive(rec.PID) {
		return false
	}
	statefile.Remove(rec.Name)
	return true
}

func controlCmd(args []string, command string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	name := fs.String("name", "", "instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	rec, err := statefile.Read(*name)
	if err != nil {
		return fmt.Errorf("no such instance %q", *name)
	}
	if cleanupIfDead(rec) {
		fmt.Printf("%q was not running (last state: %s); cleaned up its record\n", *name, rec.State)
		return nil
	}
	if err := statefile.WriteControl(*name, command); err != nil {
		return err
	}
	fmt.Printf("%s requested for %q\n", command, *name)
	return nil
}

func stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	name := fs.String("name", "", "instance name")
	kill := fs.Bool("kill", false, "also kill the session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	rec, err := statefile.Read(*name)
	if err != nil {
		return fmt.Errorf("no such instance %q", *name)
	}
	if cleanupIfDead(rec) {
		fmt.Printf("%q was not running (last state: %s); cleaned up its record\n", *name, rec.State)
		if *kill && rec.Session != "" {
			fmt.Printf("(if a tmux session %q is somehow still alive, kill it with: tmux kill-session -t %s)\n", rec.Session, rec.Session)
		}
		return nil
	}
	cmd := "detach"
	if *kill {
		cmd = "kill"
	}
	if err := statefile.WriteControl(*name, cmd); err != nil {
		return err
	}
	fmt.Printf("%s requested for %q\n", cmd, *name)
	return nil
}

// rmCmd removes instance records. By default it refuses to remove a record whose
// supervisor still appears to be running (that would orphan it from status and
// control); --force overrides. --all prunes every record with no live supervisor.
func rmCmd(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	name := fs.String("name", "", "instance name")
	all := fs.Bool("all", false, "remove every record whose supervisor is not running")
	force := fs.Bool("force", false, "remove even if the supervisor appears to be running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		recs, err := statefile.List()
		if err != nil {
			return err
		}
		removed := 0
		for _, r := range recs {
			if *force || !processAlive(r.PID) {
				statefile.Remove(r.Name)
				removed++
			}
		}
		fmt.Printf("removed %d record(s)\n", removed)
		return nil
	}
	if *name == "" {
		return fmt.Errorf("--name or --all is required")
	}
	rec, err := statefile.Read(*name)
	if err != nil {
		return fmt.Errorf("no such instance %q", *name)
	}
	if processAlive(rec.PID) && !*force {
		return fmt.Errorf("%q still has a running supervisor (pid %d); stop it with `agentkeeper stop --name %s --kill` first, or pass --force", *name, rec.PID, *name)
	}
	statefile.Remove(*name)
	fmt.Printf("removed record for %q\n", *name)
	return nil
}

func anyStale(recs []statefile.Record) bool {
	for _, r := range recs {
		if !processAlive(r.PID) && r.State != "DETACHED" {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// crlfWriter translates "\n" to "\r\n" so log lines render correctly while the
// terminal is in raw mode for hotkeys.
type crlfWriter struct{ w *os.File }

func (c crlfWriter) Write(p []byte) (int, error) {
	_, err := c.w.Write([]byte(strings.ReplaceAll(string(p), "\n", "\r\n")))
	return len(p), err
}
