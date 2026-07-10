// Command sleeperagent is a cross-agent watchdog that resumes Claude Code / Codex
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
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/amanjaiman/sleeperagent/internal/adapter"
	"github.com/amanjaiman/sleeperagent/internal/config"
	"github.com/amanjaiman/sleeperagent/internal/hotkeys"
	"github.com/amanjaiman/sleeperagent/internal/notify"
	"github.com/amanjaiman/sleeperagent/internal/ollama"
	"github.com/amanjaiman/sleeperagent/internal/parser"
	"github.com/amanjaiman/sleeperagent/internal/prompt"
	"github.com/amanjaiman/sleeperagent/internal/ptybackend"
	"github.com/amanjaiman/sleeperagent/internal/statefile"
	"github.com/amanjaiman/sleeperagent/internal/supervisor"
	"github.com/amanjaiman/sleeperagent/internal/tmux"
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
		fmt.Printf("sleeperagent %s\n", version)
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
	fmt.Fprint(os.Stderr, `sleeperagent — resume coding agents when usage limits reset

Usage:
  sleeperagent run             [flags] [-- launch-command...]  launch + watch an agent
  sleeperagent attach-existing --target T [flags]              watch an already-running tmux session
  sleeperagent agents          [--config PATH]                 list configured adapters
  sleeperagent parse           --agent A "limit text..."       test a limit string against patterns
  sleeperagent install         [--dir DIR] [--force]           copy this binary to a PATH directory
  sleeperagent status          [--name NAME]                   report state / countdown
  sleeperagent logs            --name NAME [--follow]          print / tail the instance log
  sleeperagent detach          --name NAME                     stop watching, keep session
  sleeperagent stop            --name NAME [--kill]             stop watching (optionally kill)
  sleeperagent rm              --name NAME [--force] | --all    remove a stale/ended instance record
  sleeperagent version                                         print version

Run flags:
  --agent    string  agent adapter to use (default "claude")
  --name     string  instance / tmux session name (default "sleeperagent-<agent>")
  --prompt   string  static resume prompt to inject on reset
  --reprompt string  local-LLM reprompt, e.g. "ollama:llama3.1" (falls back to static)
  --backend  string  session backend: "tmux" or "pty" (Unix falls back to pty if tmux is missing)
  --detached         tmux backend: don't attach this terminal to the session;
                     watch from the console instead (default is to attach when
                     run from a real terminal, so you can prompt the agent
                     directly while the watchdog monitors)
  --webhook  string  POST notifications to this URL
  --config   string  path to config.toml (default: OS config dir)
  --yolo             append the agent's skip-permissions flag (DANGEROUS, unattended)
  --auto-answer-prompts
                     answer interactive prompts with the first/default option
                     so the run doesn't stall while you're away (default true
                     — pass --auto-answer-prompts=false to disable)
  --no-notify        disable desktop notifications

The trailing "-- launch-command..." is optional: omit it to use the adapter's
default command (the "claude" adapter runs "claude"). Pass it only to launch
something different — your own flags, a wrapper, or another binary.

Examples:
  sleeperagent run --agent claude --name feature-x
  sleeperagent run --agent codex --prompt "Continue; run the tests."
  sleeperagent run --agent claude -- claude --model opus   # custom launch command
  sleeperagent attach-existing --agent claude --target mywork:0.1
  sleeperagent parse --agent claude "5-hour limit reached ∙ resets 2pm"
  sleeperagent status
`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent adapter to use")
	name := fs.String("name", "", "instance / tmux session name")
	promptText := fs.String("prompt", "", "static resume prompt")
	cfgPath := fs.String("config", "", "path to config.toml")
	reprompt := fs.String("reprompt", "", `local-LLM reprompt, e.g. "ollama:llama3.1"`)
	backend := fs.String("backend", defaultBackend(), `session backend: "tmux" or "pty"`)
	detached := fs.Bool("detached", false, "tmux backend: do not attach this terminal to the session; watch from the console instead")
	webhookURL := fs.String("webhook", "", "POST notifications to this URL")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	yolo := fs.Bool("yolo", false, "append the agent's skip-permissions flag (DANGEROUS)")
	autoAnswerPrompts := fs.Bool("auto-answer-prompts", true, "answer interactive prompts with the first/default option so the run does not stall while you're away (default true, pass =false to disable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	backendExplicit := flagWasSet(fs, "backend")

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
	if *autoAnswerPrompts {
		log.Printf("auto-answer-prompts is on by default: interactive prompts will be answered with their first/default option, including ones that approve tool calls, so the run does not stall while you're away. Pass --auto-answer-prompts=false to disable.")
	}

	instance := *name
	if instance == "" {
		instance = "sleeperagent-" + *agent
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
	var interactiveAttach bool
	var viewDone <-chan struct{}
	var hotkeysFallback <-chan struct{}
	selectedBackend := *backend
	if selectedBackend == "tmux" && !backendExplicit {
		if err := tmux.New(instance, "").Available(); err != nil {
			log.Printf("tmux not found on PATH; falling back to --backend pty (install tmux or pass --backend tmux for full handoff)")
			selectedBackend = "pty"
		}
	}
	switch selectedBackend {
	case "tmux":
		tx := tmux.New(instance, "")
		if err := tx.Available(); err != nil {
			return err
		}
		if tx.HasSession() {
			return fmt.Errorf("tmux session %q already exists; resume watching it with "+
				"`sleeperagent attach-existing --agent %s --target %s`, or pick another --name",
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

		// Interactive attach (the default in a real terminal): put the user's
		// terminal inside the session so they can prompt the agent directly,
		// while the supervisor keeps polling in this process. Opt out with
		// --detached; non-TTY contexts (scripts, CI) keep the detached behavior,
		// and so does a run from inside tmux (nested attach is refused by tmux).
		if interactiveWanted(*detached) {
			ia, ierr := setupInteractiveAttach(tx, instance, afterDetach)
			if ierr != nil {
				return ierr
			}
			defer ia.restoreLogs()
			pane, foreground, afterDetach = ia.pane, ia.foreground, ia.afterDetach
			viewDone, hotkeysFallback = ia.viewDone, ia.hotkeysFallback
			interactiveAttach = true
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
		return fmt.Errorf("unknown backend %q (use \"tmux\" or \"pty\")", selectedBackend)
	}

	return watchSession(watchParams{
		instance:          instance,
		agent:             *agent,
		adapter:           ad,
		pane:              pane,
		attachHint:        attachHint,
		startSession:      startSession,
		foreground:        foreground,
		afterDetach:       afterDetach,
		builder:           builder,
		promptMode:        promptMode,
		resumeText:        resumeText,
		cfg:               cfg,
		cwd:               cwd,
		autoAnswerPrompts: *autoAnswerPrompts,
		notifier:          buildNotifier(*noNotify, *webhookURL),
		transparentTTY:    selectedBackend == "pty",
		interactiveAttach: interactiveAttach,
		viewDone:          viewDone,
		hotkeysFallback:   hotkeysFallback,
	})
}

// interactiveWanted reports whether to attach the user's terminal to the tmux
// session: not opted out, a real terminal on both ends, and not already inside
// tmux (tmux refuses a nested attach).
func interactiveWanted(detached bool) bool {
	if detached || os.Getenv("TMUX") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// interactiveView is the wiring for an interactive-attach run: the user's
// terminal lives inside the tmux session (a supervised `tmux attach` child)
// while the supervisor watches from this process.
type interactiveView struct {
	pane        supervisor.Pane
	foreground  func(context.Context)
	afterDetach func(context.Context)
	restoreLogs func() // idempotent; moves logging back to the console
	// viewDone closes when the attach child has exited (view detached, session
	// gone, or attach failed) — always after restoreLogs has run.
	viewDone <-chan struct{}
	// hotkeysFallback closes when the view exited but the watchdog is still
	// running, so the caller should start console hotkeys.
	hotkeysFallback <-chan struct{}
}

// setupInteractiveAttach redirects supervisor logs to the instance log file
// (they must not scribble over the agent's TUI) and builds the closures that
// run and tear down the self-view. baseAfterDetach is invoked for the final
// user-facing detach message so there is a single source for its wording.
func setupInteractiveAttach(tx *tmux.Client, instance string, baseAfterDetach func(context.Context)) (*interactiveView, error) {
	restore, err := redirectLogsToInstanceFile(instance)
	if err != nil {
		return nil, err
	}
	var restoreOnce sync.Once
	viewDone := make(chan struct{})
	hotkeysFallback := make(chan struct{})
	v := &interactiveView{
		restoreLogs:     func() { restoreOnce.Do(restore) },
		viewDone:        viewDone,
		hotkeysFallback: hotkeysFallback,
	}

	// viewing is set before the supervisor starts so its first polls already
	// treat our own attach client as the self-view (see attachSuppressingPane).
	viewing := &atomic.Bool{}
	viewing.Store(true)
	stopping := &atomic.Bool{}
	v.pane = attachSuppressingPane{Pane: tx, viewing: viewing, clientCount: tx.ClientCount}

	v.foreground = func(ctx context.Context) {
		err := tx.Attach(ctx)
		viewing.Store(false)
		v.restoreLogs()
		if ctx.Err() == nil && !stopping.Load() && tx.HasSession() {
			if err != nil {
				log.Printf("could not attach the view (%v); watching from the console instead", err)
			} else {
				log.Printf("view detached; still watching session %q. Reattach with: %s", instance, tx.AttachHint())
			}
			close(hotkeysFallback)
		}
		close(viewDone)
	}

	v.afterDetach = func(ctx context.Context) {
		stopping.Store(true)
		// If the user is still in the self-view, don't yank them out of a live
		// session mid-keystroke: tell them via the tmux status line and wait
		// for them to detach on their own (mirrors the pty backend, where the
		// agent keeps the terminal until it exits).
		if viewing.Load() {
			_ = tx.DisplayMessage("sleeperagent stopped watching — the session is all yours; detach (prefix + d) to close the watchdog process")
			select {
			case <-viewDone:
			case <-ctx.Done():
			}
		}
		v.restoreLogs()
		baseAfterDetach(ctx)
	}
	return v, nil
}

// attachSuppressingPane keeps the supervisor's auto-detach check meaningful
// while our own attach client exists: the self-view alone is "the user driving
// the agent while the watchdog watches", not a takeover — but any client
// beyond it is a real takeover. Once the self-view exits, the underlying
// check applies as usual.
type attachSuppressingPane struct {
	supervisor.Pane
	viewing     *atomic.Bool
	clientCount func() (int, error)
}

func (p attachSuppressingPane) ClientAttached() (bool, error) {
	if p.viewing.Load() {
		n, err := p.clientCount()
		if err != nil {
			return false, err
		}
		return n > 1, nil
	}
	return p.Pane.ClientAttached()
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// watchParams bundles everything watchSession needs so both `run` and
// `attach-existing` share the same observe/wait/resume core.
type watchParams struct {
	instance          string
	agent             string
	adapter           *adapter.Adapter
	pane              supervisor.Pane
	attachHint        string
	startSession      func() error // nil when the session already exists (attach-existing)
	foreground        func(ctx context.Context)
	afterDetach       func(ctx context.Context)
	builder           prompt.Builder
	promptMode        string
	resumeText        string
	cfg               config.Config
	cwd               string
	autoAnswerPrompts bool
	notifier          notify.Notifier
	transparentTTY    bool
	interactiveAttach bool
	// viewDone / hotkeysFallback are set for interactive-attach runs; see
	// interactiveView. Both may be nil.
	viewDone        <-chan struct{}
	hotkeysFallback <-chan struct{}
}

// waitViewExit blocks (bounded) until the interactive self-view has torn down,
// so final messages land on the user's console instead of racing the view's
// log restore. No-op for non-interactive runs.
func (p watchParams) waitViewExit() {
	if p.viewDone == nil {
		return
	}
	select {
	case <-p.viewDone:
	case <-time.After(5 * time.Second):
	}
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
	if p.autoAnswerPrompts {
		log.Printf("auto-answer-prompts: enabled; interactive prompts may be accepted without a human")
	}
	if p.promptMode != "static" {
		log.Printf("reprompt: %s enabled (falls back to static prompt on any failure)", p.promptMode)
	}
	if p.transparentTTY {
		log.Printf("pty pass-through: stdin/stdout are reserved for the agent; from another shell use `sleeperagent logs --name %s -f`, `sleeperagent status`, or `sleeperagent detach/stop --name %s`", p.instance, p.instance)
	} else if p.interactiveAttach {
		log.Printf("interactive attach: your terminal is inside the tmux session; detach the view with the tmux prefix + d (the watchdog keeps running). From another shell: `sleeperagent status`, `sleeperagent detach/stop --name %s`", p.instance)
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
	if !p.transparentTTY && !p.interactiveAttach && hotkeys.Listen(ctx, cmds, log.Printf) {
		log.SetOutput(crlfWriter{os.Stderr})
		defer log.SetOutput(os.Stderr)
	}

	// Interactive attach owns stdin while the view is up, so hotkeys start only
	// if the view goes away with the watchdog still running (user detached the
	// view, or the attach failed) — restoring the classic console controls.
	if p.hotkeysFallback != nil {
		go func() {
			select {
			case <-ctx.Done():
			case <-p.hotkeysFallback:
				if hotkeys.Listen(ctx, cmds, log.Printf) {
					log.SetOutput(crlfWriter{os.Stderr})
					log.Printf("%s", hotkeys.Legend)
				}
			}
		}()
	}

	// Control-file poller: `sleeperagent detach`/`stop` from another shell drops a
	// command file that we pick up here and forward to the supervisor.
	go pollControl(ctx, p.instance, cmds)

	sup := supervisor.New(supervisor.Options{
		Adapter:           p.adapter,
		Tmux:              p.pane,
		Prompt:            p.builder,
		PollInterval:      p.cfg.PollInterval.D(),
		ResetBuffer:       p.cfg.ResetBuffer.D(),
		MaxWait:           p.cfg.MaxWait.D(),
		Cwd:               p.cwd,
		AutoAnswerPrompts: p.autoAnswerPrompts,
		Commands:          cmds,
		OnUpdate:          writeRecord,
		OnManualAction: func(msg string) {
			p.notifier.Notify(notify.Event{
				Title: "SleeperAgent [" + p.instance + "]: manual choice needed",
				Body:  msg,
			})
		},
		Logf: log.Printf,
	})

	if err := sup.Run(ctx); err != nil {
		return err
	}

	if sup.SessionKilled() {
		p.waitViewExit()
		statefile.Remove(p.instance)
		log.Printf("session %q killed.", p.instance)
		return nil
	}
	if sup.SessionEnded() {
		// The agent exited or the session was killed out from under us; there is
		// no live session to hand back, so skip the reattach hint.
		p.waitViewExit()
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
	detached := fs.Bool("detached", false, "do not attach this terminal to the session; watch from the console instead")
	promptText := fs.String("prompt", "", "static resume prompt")
	cfgPath := fs.String("config", "", "path to config.toml")
	reprompt := fs.String("reprompt", "", `local-LLM reprompt, e.g. "ollama:llama3.1"`)
	webhookURL := fs.String("webhook", "", "POST notifications to this URL")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	autoAnswerPrompts := fs.Bool("auto-answer-prompts", true, "answer interactive prompts with the first/default option so the run does not stall while you're away (default true, pass =false to disable)")
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
	if *autoAnswerPrompts {
		log.Printf("auto-answer-prompts is on by default: interactive prompts will be answered with their first/default option, including ones that approve tool calls, so the run does not stall while you're away. Pass --auto-answer-prompts=false to disable.")
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

	var pane supervisor.Pane = tx
	var foreground func(context.Context)
	var interactiveAttach bool
	var viewDone, hotkeysFallback <-chan struct{}
	afterDetach := func(context.Context) {
		log.Printf("detached. target %q left running — reattach with: %s", *target, tx.AttachHint())
	}
	if interactiveWanted(*detached) {
		ia, ierr := setupInteractiveAttach(tx, instance, afterDetach)
		if ierr != nil {
			return ierr
		}
		defer ia.restoreLogs()
		pane, foreground, afterDetach = ia.pane, ia.foreground, ia.afterDetach
		viewDone, hotkeysFallback = ia.viewDone, ia.hotkeysFallback
		interactiveAttach = true
	}

	return watchSession(watchParams{
		instance:          instance,
		agent:             *agent,
		adapter:           ad,
		pane:              pane,
		attachHint:        tx.AttachHint(),
		startSession:      nil, // already running
		foreground:        foreground,
		afterDetach:       afterDetach,
		builder:           builder,
		promptMode:        promptMode,
		resumeText:        resumeText,
		cfg:               cfg,
		cwd:               cwd,
		autoAnswerPrompts: *autoAnswerPrompts,
		notifier:          buildNotifier(*noNotify, *webhookURL),
		interactiveAttach: interactiveAttach,
		viewDone:          viewDone,
		hotkeysFallback:   hotkeysFallback,
	})
}

// notifyTransition fires a notification on meaningful state changes.
func notifyTransition(n notify.Notifier, prev string, snap supervisor.Snapshot, instance string) {
	cur := string(snap.State)
	if cur == prev {
		return
	}
	tag := "SleeperAgent [" + instance + "]"
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
		return fmt.Errorf("provide the limit text to test, e.g. sleeperagent parse --agent claude \"...resets 2pm\"")
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
		fmt.Printf("no limit pattern matched for agent %q.\nrun `sleeperagent agents` to see the patterns.\n", *agent)
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
		fmt.Println("no SleeperAgent instances found")
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
		return fmt.Errorf("%q still has a running supervisor (pid %d); stop it with `sleeperagent stop --name %s --kill` first, or pass --force", *name, rec.PID, *name)
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
