// Package supervisor is the core loop: it observes the agent's pane, detects the
// usage limit, waits for the reset, and injects a resume prompt. It is written
// against the tmux Client and the adapter so the same loop drives any agent.
package supervisor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
	"github.com/amanjaiman/agentkeeper/internal/parser"
	"github.com/amanjaiman/agentkeeper/internal/prompt"
	"github.com/amanjaiman/agentkeeper/internal/state"
)

// Pane is the session backend the supervisor observes and writes to. tmux.Client
// implements it; a raw-PTY backend (M5) can satisfy the same interface.
type Pane interface {
	Capture(scrollback int) (string, error)
	Inject(text, style string) error
	AttachHint() string
	// Kill terminates the underlying session (and the agent in it).
	Kill() error
	// ClientAttached reports whether a human is currently attached to the
	// session, used to auto-detach when the user takes over.
	ClientAttached() (bool, error)
	// Ended reports whether the underlying session/agent is gone — the agent
	// exited or the session was killed out from under the supervisor. When true,
	// there is nothing left to watch and the supervisor stops cleanly.
	Ended() (bool, error)
}

// Command is an external control request delivered over Options.Commands.
type Command int

const (
	// CmdDetach stops watching but leaves the session (and agent) running.
	CmdDetach Command = iota + 1
	// CmdKill stops watching and terminates the session.
	CmdKill
)

// Snapshot is the supervisor's observable state, handed to Options.OnUpdate so
// the caller can persist it for `agentkeeper status`.
type Snapshot struct {
	State     state.State
	Reset     parser.ResetInfo
	WaitUntil time.Time
}

// fallbackWindow is assumed when the reset time is unparseable: a 5-hour window
// from detection, matching the standard short-cap length.
const fallbackWindow = 5 * time.Hour

// maxLimitCycles bounds how many times we re-wait when a resume immediately
// re-triggers the limit before giving up and detaching.
const maxLimitCycles = 3

// maxInjectAttempts bounds re-injection when the first resume keystroke does not
// visibly change the pane.
const maxInjectAttempts = 3

// reLimitBackoffStep is added per re-limit cycle on top of the parsed reset, so
// a resume that keeps re-hitting the limit waits progressively longer instead of
// hammering the agent.
const reLimitBackoffStep = 30 * time.Second

// scrollback is how many history lines the watcher captures each poll, so a
// limit message that just scrolled off-screen is still seen.
const scrollback = 100

// maxCaptureFails bounds how many consecutive capture errors are tolerated
// before the supervisor gives up and exits. A single hiccup (one failed
// capture-pane) still recovers; a backend that is persistently broken does not
// loop forever.
const maxCaptureFails = 5

var safeStopAndWait = regexp.MustCompile(`(?i)stop and wait for (?:the|your) limit to reset`)

// Options configures a Supervisor.
type Options struct {
	Adapter      *adapter.Adapter
	Tmux         Pane
	Prompt       prompt.Builder
	PollInterval time.Duration
	ResetBuffer  time.Duration
	MaxWait      time.Duration
	Cwd          string
	// AutoDetach, when true, detaches automatically if a human attaches to the
	// session while the supervisor is observing or waiting.
	AutoDetach bool
	// WatchOnly, when true, notifies at reset but never injects: on reaching the
	// reset it logs and detaches, leaving the resume to the human.
	WatchOnly bool
	// Commands delivers external control requests (detach/kill). May be nil.
	Commands <-chan Command
	// OnUpdate, if set, is called whenever the observable state changes so the
	// caller can persist a Snapshot. May be nil.
	OnUpdate func(Snapshot)
	// OnManualAction, if set, is called when a matched auto-response cannot be
	// answered safely and needs the human to choose in the agent UI.
	OnManualAction func(string)
	// Now and Logf are injectable for testing; nil values get sensible defaults.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Supervisor runs the watch/wait/resume loop for one agent session.
type Supervisor struct {
	opt Options
	st  state.State

	lastCapture string
	stableSince time.Time

	// per-limit-event scratch
	groups         map[string]string
	currentMatch   string // the limit line that triggered the active event
	handledMatch   string // last fully-handled limit line, still lingering in scrollback
	reset          parser.ResetInfo
	waitUntil      time.Time
	limitLatched   bool
	limitCycles    int
	injected       bool
	injectAttempts int
	preInject      string
	handledAuto    map[int]string

	killed       bool
	ended        bool
	captureFails int
	lastSnap     Snapshot
	hasSnap      bool

	lastCountdown time.Time
}

// New builds a Supervisor and fills in default Now/Logf.
func New(opt Options) *Supervisor {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Supervisor{opt: opt, st: state.Running}
}

// State returns the current state (for status reporting/tests).
func (s *Supervisor) State() state.State { return s.st }

// SessionKilled reports whether the supervisor terminated the session (vs. a
// plain detach that leaves it running).
func (s *Supervisor) SessionKilled() bool { return s.killed }

// SessionEnded reports whether the supervisor stopped because the session/agent
// went away on its own (exited or was killed externally), as opposed to a
// user-initiated detach or kill.
func (s *Supervisor) SessionEnded() bool { return s.ended }

// Run drives the loop until ctx is cancelled, an external command detaches it,
// or the agent is fully handed off. By default it does not kill the session; a
// CmdKill is the only path that terminates it, so handoff stays clean. A
// cancelled context is treated as a detach (Ctrl-C never destroys the session).
func (s *Supervisor) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.opt.PollInterval)
	defer ticker.Stop()

	// Process once immediately so a session that is already limited is handled
	// without waiting a full poll interval.
	if err := s.tick(); err != nil {
		return err
	}
	if s.stopped() {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			s.opt.Logf("interrupted; detaching (session left running)")
			s.st = state.Detached
			s.emit()
			return nil
		case <-ticker.C:
			if err := s.tick(); err != nil {
				return err
			}
			if s.stopped() {
				return nil
			}
		}
	}
}

// stopped reports whether the supervisor has reached a terminal state and the
// Run loop should return: either a user-initiated detach or the session ending.
func (s *Supervisor) stopped() bool {
	return s.st == state.Detached || s.st == state.Ended
}

func (s *Supervisor) tick() error {
	// External control (detach/kill) takes priority over everything else.
	if s.drainCommand() {
		s.emit()
		return nil
	}

	// If the session/agent went away on its own (the agent exited, or the session
	// was killed out from under us), there is nothing left to watch: stop cleanly.
	// Checked before Capture because some backends (pty) keep returning a stale
	// buffer after the child exits.
	if gone, eerr := s.opt.Tmux.Ended(); eerr == nil && gone {
		s.sessionGone("agent exited / session ended")
		return nil
	}

	capture, err := s.opt.Tmux.Capture(scrollback)
	if err != nil {
		// A transient capture failure shouldn't kill the watchdog; log and retry,
		// but don't loop forever against a persistently broken backend.
		s.captureFails++
		if s.captureFails >= maxCaptureFails {
			s.sessionGone(fmt.Sprintf("capture failed %d times in a row (last: %v)", s.captureFails, err))
			return nil
		}
		s.opt.Logf("capture failed (%d/%d): %v", s.captureFails, maxCaptureFails, err)
		return nil
	}
	s.captureFails = 0
	now := s.opt.Now()

	// Track pane stability for the idle heuristic.
	if capture != s.lastCapture {
		s.stableSince = now
	}

	// Once a handled limit line scrolls out of the captured window, forget it so
	// a genuinely new (identical-looking) limit later is not suppressed.
	if s.handledMatch != "" && !strings.Contains(capture, s.handledMatch) {
		s.handledMatch = ""
	}
	for i, match := range s.handledAuto {
		if match != "" && !strings.Contains(capture, match) {
			delete(s.handledAuto, i)
		}
	}

	// If a human has taken over the session, step aside rather than fighting them
	// for the input. Only meaningful while observing or waiting.
	if s.opt.AutoDetach && (s.st == state.Running || s.st == state.Waiting) {
		if attached, aerr := s.opt.Tmux.ClientAttached(); aerr == nil && attached {
			s.opt.Logf("user attached to the session; auto-detaching. It's all yours: %s", s.opt.Tmux.AttachHint())
			s.st = state.Detached
			s.emit()
			return nil
		}
	}

	switch s.st {
	case state.Running:
		s.onRunning(capture, now)
	case state.Limited:
		s.onLimited(capture, now)
	case state.Waiting:
		s.onWaiting(now)
	case state.Resuming:
		if err := s.onResuming(capture, now); err != nil {
			return err
		}
	}

	s.lastCapture = capture
	s.emit()
	return nil
}

// drainCommand applies one pending external command, if any. It returns true
// when the command moved the supervisor to a terminal (detached) state.
func (s *Supervisor) drainCommand() bool {
	if s.opt.Commands == nil {
		return false
	}
	select {
	case cmd := <-s.opt.Commands:
		switch cmd {
		case CmdDetach:
			s.opt.Logf("detach requested; session left running. Reattach with: %s", s.opt.Tmux.AttachHint())
			s.st = state.Detached
			return true
		case CmdKill:
			s.opt.Logf("kill requested; terminating session (%s)", s.opt.Tmux.AttachHint())
			if err := s.opt.Tmux.Kill(); err != nil {
				s.opt.Logf("kill failed: %v", err)
			} else {
				s.killed = true
			}
			s.st = state.Detached
			return true
		}
	default:
	}
	return false
}

// sessionGone records that the supervised session ended on its own and moves to
// the terminal Ended state, emitting a final snapshot so the caller can persist
// it and fire a notification via the normal transition path.
func (s *Supervisor) sessionGone(reason string) {
	s.opt.Logf("session ended (%s); stopping watch", reason)
	s.ended = true
	s.st = state.Ended
	s.emit()
}

// emit pushes a Snapshot to OnUpdate when the observable state has changed.
func (s *Supervisor) emit() {
	if s.opt.OnUpdate == nil {
		return
	}
	snap := Snapshot{State: s.st, Reset: s.reset, WaitUntil: s.waitUntil}
	if s.hasSnap && snap == s.lastSnap {
		return
	}
	s.lastSnap = snap
	s.hasSnap = true
	s.opt.OnUpdate(snap)
}

func (s *Supervisor) onRunning(capture string, now time.Time) {
	if s.limitLatched {
		return
	}
	match, groups, ok := parser.Detect(s.opt.Adapter.LimitPatterns, capture)
	if !ok {
		s.scanAutoResponses(capture)
		return
	}
	// Ignore the same limit line we already handled and that is still sitting in
	// the captured scrollback — it is not a new limit event.
	if match == s.handledMatch {
		return
	}
	s.limitLatched = true
	s.currentMatch = match
	s.groups = groups
	s.st = state.Limited
	s.opt.Logf("usage limit detected")
	s.scanAutoResponses(capture)
}

func (s *Supervisor) onLimited(capture string, now time.Time) {
	s.scanAutoResponses(capture)
	s.reset = parser.Resolve(s.groups, now, fallbackWindow)
	s.waitUntil = s.reset.Time.Add(s.opt.ResetBuffer)

	// On a re-limit cycle, back off progressively past the parsed reset.
	if s.limitCycles > 0 {
		backoff := time.Duration(s.limitCycles) * reLimitBackoffStep
		s.waitUntil = s.waitUntil.Add(backoff)
		s.opt.Logf("re-limit backoff: +%s (cycle %d)", backoff, s.limitCycles)
	}

	wait := s.waitUntil.Sub(now)
	s.opt.Logf("reset at %s (source=%s, confidence=%s); waiting %s",
		s.reset.Time.Local().Format("Mon 15:04 MST"), s.reset.Source, s.reset.Confidence, wait.Round(time.Second))

	if wait > s.opt.MaxWait {
		s.opt.Logf("reset is beyond max_wait (%s) — likely a weekly cap; detaching instead of sleeping for days. Take over with: %s",
			s.opt.MaxWait, s.opt.Tmux.AttachHint())
		s.st = state.Detached
		return
	}
	s.lastCountdown = time.Time{}
	s.st = state.Waiting
}

func (s *Supervisor) scanAutoResponses(capture string) {
	if len(s.opt.Adapter.AutoResponses) == 0 {
		return
	}
	if s.handledAuto == nil {
		s.handledAuto = make(map[int]string)
	}
	for i, ar := range s.opt.Adapter.AutoResponses {
		match := ar.Pattern.FindString(capture)
		if match == "" {
			continue
		}
		if ar.Once && s.handledAuto[i] == match {
			continue
		}
		if ar.Keys == "" || !safeStopAndWait.MatchString(capture) {
			msg := "manual choice needed at the agent: rate-limit menu matched but no verified stop-and-wait keystrokes are configured"
			s.opt.Logf("%s", msg)
			if s.opt.OnManualAction != nil {
				s.opt.OnManualAction(msg)
			}
			s.handledAuto[i] = match
			continue
		}
		if err := s.opt.Tmux.Inject(ar.Keys, adapter.InjectKeys); err != nil {
			s.opt.Logf("auto-response injection failed: %v", err)
			continue
		}
		s.opt.Logf("auto-response injected for safe stop-and-wait menu")
		if ar.Once {
			s.handledAuto[i] = match
		}
	}
}

func (s *Supervisor) onWaiting(now time.Time) {
	if !now.Before(s.waitUntil) {
		if s.opt.WatchOnly {
			s.opt.Logf("reset reached (watch-only): not injecting — take over with: %s", s.opt.Tmux.AttachHint())
			s.st = state.Detached
			return
		}
		s.opt.Logf("reset reached; resuming")
		s.injected = false
		s.injectAttempts = 0
		s.st = state.Resuming
		return
	}
	// Throttle countdown output to once every 30s.
	if now.Sub(s.lastCountdown) >= 30*time.Second {
		s.lastCountdown = now
		s.opt.Logf("waiting %s until reset", s.waitUntil.Sub(now).Round(time.Second))
	}
}

func (s *Supervisor) onResuming(capture string, now time.Time) error {
	if !s.injected {
		if !s.idle(capture, now) {
			// Don't inject mid-stream; wait for the agent to be ready.
			return nil
		}
		text, err := s.opt.Prompt.Build(prompt.Context{Agent: s.opt.Adapter.Name, Cwd: s.opt.Cwd})
		if err != nil {
			s.opt.Logf("prompt build failed (%v); using default", err)
			text = prompt.DefaultText
		}
		s.preInject = capture
		if err := s.opt.Tmux.Inject(text, s.opt.Adapter.InjectStyle); err != nil {
			return fmt.Errorf("inject resume prompt: %w", err)
		}
		s.injected = true
		s.injectAttempts++
		s.opt.Logf("injected resume prompt (attempt %d): %q", s.injectAttempts, text)
		return nil
	}

	// Verify the injection took: the pane should have changed.
	if capture != s.preInject {
		// Did the resume immediately re-hit the limit? Only a NEW limit line
		// counts — the line that triggered this event still lingers in scrollback
		// and must not be mistaken for a fresh re-hit.
		if match, groups, limited := parser.Detect(s.opt.Adapter.LimitPatterns, capture); limited && match != s.currentMatch {
			s.limitCycles++
			if s.limitCycles >= maxLimitCycles {
				s.opt.Logf("limit re-triggered %d times after resume; detaching. Take over with: %s",
					s.limitCycles, s.opt.Tmux.AttachHint())
				s.st = state.Detached
				return nil
			}
			s.opt.Logf("resume re-hit the limit (cycle %d); re-waiting", s.limitCycles)
			s.currentMatch = match
			s.groups = groups
			s.injected = false
			s.st = state.Limited
			return nil
		}
		s.opt.Logf("resume confirmed; back to running")
		s.handledMatch = s.currentMatch
		s.limitLatched = false
		s.limitCycles = 0
		s.st = state.Running
		return nil
	}

	// No change yet. Re-inject after a couple of stable polls, up to the cap.
	if s.injectAttempts >= maxInjectAttempts {
		s.opt.Logf("resume prompt did not visibly take after %d attempts; assuming sent and resuming watch", s.injectAttempts)
		s.limitLatched = false
		s.st = state.Running
		return nil
	}
	s.injected = false // allow re-injection next tick
	return nil
}

// idle reports whether the agent is ready for input. If the adapter defines an
// idle pattern, it must match; otherwise we treat the pane as idle once it has
// been stable for at least two poll intervals (no spinner activity).
func (s *Supervisor) idle(capture string, now time.Time) bool {
	if s.opt.Adapter.IdlePattern != nil {
		return s.opt.Adapter.IdlePattern.MatchString(capture)
	}
	return now.Sub(s.stableSince) >= 2*s.opt.PollInterval
}
