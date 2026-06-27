package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
	"github.com/amanjaiman/agentkeeper/internal/prompt"
	"github.com/amanjaiman/agentkeeper/internal/state"
)

// fakePane is a scriptable Pane: each tick the supervisor reads the current
// screen, and Inject records what was sent and swaps in the next screen.
type fakePane struct {
	screen     string
	injected   []string
	styles     []string
	onInject   func(text string) string // returns the new screen after injection
	attached   bool                     // reported by ClientAttached
	killed     bool
	ended      bool  // reported by Ended
	captureErr error // when set, Capture fails with it
}

func (f *fakePane) Capture(int) (string, error) {
	if f.captureErr != nil {
		return "", f.captureErr
	}
	return f.screen, nil
}
func (f *fakePane) AttachHint() string            { return "tmux attach -t test" }
func (f *fakePane) ClientAttached() (bool, error) { return f.attached, nil }
func (f *fakePane) Ended() (bool, error)          { return f.ended, nil }
func (f *fakePane) Kill() error                   { f.killed = true; return nil }
func (f *fakePane) Inject(text, style string) error {
	f.injected = append(f.injected, text)
	f.styles = append(f.styles, style)
	if f.onInject != nil {
		f.screen = f.onInject(text)
	}
	return nil
}

func testAdapter(t *testing.T) *adapter.Adapter {
	t.Helper()
	ad, err := adapter.Compile("claude", adapter.Spec{
		LaunchCmd:     "claude",
		LimitPatterns: []string{`(?i)limit reached.*resets\s+(?P<time>.+)`},
		InjectStyle:   adapter.InjectEscTextEnter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// driveClock lets the test advance a virtual clock the supervisor reads via Now.
type driveClock struct{ t time.Time }

func (d *driveClock) now() time.Time        { return d.t }
func (d *driveClock) add(dur time.Duration) { d.t = d.t.Add(dur) }

func TestFullCycleStaticResume(t *testing.T) {
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	pane := &fakePane{screen: "working...\n"}
	pane.onInject = func(string) string { return "resumed, working again\n" }

	sup := New(Options{
		Adapter:      testAdapter(t),
		Tmux:         pane,
		Prompt:       prompt.NewStatic("continue"),
		PollInterval: time.Second,
		ResetBuffer:  60 * time.Second,
		MaxWait:      24 * time.Hour,
		Now:          clk.now,
	})

	// 1) Running, no limit yet.
	must(t, sup.tick())
	if sup.State() != state.Running {
		t.Fatalf("state = %s, want RUNNING", sup.State())
	}

	// 2) Limit appears -> detection -> resolves reset -> WAITING.
	pane.screen = "5-hour limit reached ∙ resets 11am\n"
	must(t, sup.tick()) // RUNNING -> LIMITED (latches)
	must(t, sup.tick()) // LIMITED -> WAITING
	if sup.State() != state.Waiting {
		t.Fatalf("state = %s, want WAITING", sup.State())
	}
	// reset 11:00 + 60s buffer.
	wantReset := time.Date(2026, 6, 26, 11, 1, 0, 0, time.Local)
	if !sup.waitUntil.Equal(wantReset) {
		t.Fatalf("waitUntil = %v, want %v", sup.waitUntil, wantReset)
	}

	// 3) Still waiting before reset.
	clk.add(30 * time.Minute)
	must(t, sup.tick())
	if sup.State() != state.Waiting {
		t.Fatalf("state = %s, want WAITING (still)", sup.State())
	}

	// 4) Past reset -> RESUMING. Pane must be idle first (stable >= 2 polls).
	clk.add(45 * time.Minute) // now 11:15, past 11:01
	must(t, sup.tick())       // WAITING -> RESUMING
	if sup.State() != state.Resuming {
		t.Fatalf("state = %s, want RESUMING", sup.State())
	}
	// Pane has been stable; advance clock so idle heuristic passes, then inject.
	clk.add(3 * time.Second)
	must(t, sup.tick()) // idle -> inject
	if len(pane.injected) != 1 || pane.injected[0] != "continue" {
		t.Fatalf("injected = %v, want [continue]", pane.injected)
	}

	// 5) Pane changed after injection -> resume confirmed -> RUNNING, latch clear.
	must(t, sup.tick())
	if sup.State() != state.Running {
		t.Fatalf("state = %s, want RUNNING after resume", sup.State())
	}
	if sup.limitLatched {
		t.Fatal("limit latch should be cleared after a confirmed resume")
	}
}

// TestStaleLimitLineNotReinjected reproduces the integration-test wart: after a
// confirmed resume the limit line is still in tmux scrollback. It must not be
// treated as a fresh limit and re-injected.
func TestStaleLimitLineNotReinjected(t *testing.T) {
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	limitLine := "Claude AI usage limit reached|" + itoa(clk.now().Add(5*time.Second).Unix())
	pane := &fakePane{screen: "working\n"}
	// Injection appends the agent's echo but KEEPS the limit line on screen,
	// mimicking tmux scrollback retaining it.
	pane.onInject = func(text string) string {
		return pane.screen + "agent received: " + text + "\n"
	}

	ad, err := adapter.Compile("claude", adapter.Spec{
		LimitPatterns: []string{`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	sup := New(Options{
		Adapter: ad, Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		Now: clk.now,
	})

	must(t, sup.tick()) // running
	pane.screen += limitLine + "\n"
	must(t, sup.tick()) // -> LIMITED
	must(t, sup.tick()) // -> WAITING
	clk.add(10 * time.Second)
	must(t, sup.tick()) // -> RESUMING
	clk.add(3 * time.Second)
	must(t, sup.tick()) // idle -> inject (attempt 1)
	must(t, sup.tick()) // pane changed, same match -> resume confirmed -> RUNNING

	if sup.State() != state.Running {
		t.Fatalf("state = %s, want RUNNING", sup.State())
	}
	// Several more polls with the stale line still on screen: no new injection.
	for i := 0; i < 5; i++ {
		clk.add(time.Second)
		must(t, sup.tick())
	}
	if len(pane.injected) != 1 {
		t.Fatalf("injected %d times %v, want exactly 1 (stale line must not re-trigger)", len(pane.injected), pane.injected)
	}
}

// TestReLimitBackoff verifies that when a resume immediately re-hits a fresh
// limit, the next wait includes the progressive backoff on top of the reset.
func TestReLimitBackoff(t *testing.T) {
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	ad, err := adapter.Compile("claude", adapter.Spec{
		LimitPatterns: []string{`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	lineA := "Claude AI usage limit reached|" + itoa(clk.now().Add(5*time.Second).Unix())
	tsB := clk.now().Add(100 * time.Second).Unix()
	lineB := "Claude AI usage limit reached|" + itoa(tsB)

	pane := &fakePane{screen: "working\n"}
	pane.onInject = func(string) string { return lineB + "\n" } // resume re-hits a NEW limit

	sup := New(Options{
		Adapter: ad, Tmux: pane, Prompt: prompt.NewStatic("go"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		Now: clk.now,
	})

	must(t, sup.tick()) // running
	pane.screen = lineA + "\n"
	must(t, sup.tick()) // LIMITED
	must(t, sup.tick()) // WAITING (cycle 0, no backoff)
	clk.add(10 * time.Second)
	must(t, sup.tick()) // RESUMING
	clk.add(3 * time.Second)
	must(t, sup.tick()) // inject -> screen becomes lineB
	must(t, sup.tick()) // verify: new limit -> re-limit cycle 1 -> LIMITED

	if sup.limitCycles != 1 {
		t.Fatalf("limitCycles = %d, want 1", sup.limitCycles)
	}
	must(t, sup.tick()) // onLimited resolves B + backoff -> WAITING

	want := time.Unix(tsB, 0).Add(time.Second).Add(reLimitBackoffStep) // reset + buffer + 1*backoff
	if !sup.waitUntil.Equal(want) {
		t.Fatalf("waitUntil = %v, want %v (reset+buffer+backoff)", sup.waitUntil, want)
	}
}

// TestWatchOnlyDetachesAtResetWithoutInjecting verifies watch-only mode: it
// reaches the reset, then detaches instead of injecting.
func TestWatchOnlyDetachesAtResetWithoutInjecting(t *testing.T) {
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	pane := &fakePane{screen: "working\n"}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		Now: clk.now, WatchOnly: true,
	})

	must(t, sup.tick()) // running
	pane.screen = "5-hour limit reached ∙ resets 11am\n"
	must(t, sup.tick()) // LIMITED
	must(t, sup.tick()) // WAITING
	clk.add(2 * time.Hour)
	must(t, sup.tick()) // reset reached -> watch-only -> DETACHED

	if sup.State() != state.Detached {
		t.Fatalf("state = %s, want DETACHED in watch-only at reset", sup.State())
	}
	if len(pane.injected) != 0 {
		t.Fatalf("watch-only must not inject, got %v", pane.injected)
	}
}

func TestAutoResponseInjectsSafeStopAndWaitOnce(t *testing.T) {
	menu := readParserTestdata(t, "claude_rate_limit_options.txt")
	pane := &fakePane{screen: "Claude usage limit reached. Your limit will reset at 11am.\n" + menu}
	ad, err := adapter.Compile("claude", adapter.Spec{
		LimitPatterns: []string{`(?i)usage limit reached.*reset at (?P<time>[^\r\n.]+)`},
		AutoResponses: []adapter.AutoResponseSpec{{
			Pattern: `(?i)rate.?limit.?options|stop and wait for (?:the|your) limit to reset`,
			Keys:    "1\r",
			Once:    true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sup := New(Options{
		Adapter: ad, Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
	})

	must(t, sup.tick())
	must(t, sup.tick())
	must(t, sup.tick())

	if len(pane.injected) != 1 || pane.injected[0] != "1\r" {
		t.Fatalf("auto-response injected = %q, want one %q", pane.injected, "1\\r")
	}
	if pane.styles[0] != adapter.InjectKeys {
		t.Fatalf("style = %q, want %q", pane.styles[0], adapter.InjectKeys)
	}
}

func TestAutoResponseAmbiguousMenuNotifiesWithoutInjecting(t *testing.T) {
	pane := &fakePane{screen: "/rate-limit-options\n1. Upgrade plan\n2. Switch model\n"}
	ad, err := adapter.Compile("claude", adapter.Spec{
		LimitPatterns: []string{`(?i)usage limit reached.*reset at (?P<time>[^\r\n.]+)`},
		AutoResponses: []adapter.AutoResponseSpec{{
			Pattern: `(?i)rate.?limit.?options`,
			Keys:    "1\r",
			Once:    true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var manual []string
	sup := New(Options{
		Adapter: ad, Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		OnManualAction: func(msg string) {
			manual = append(manual, msg)
		},
	})

	must(t, sup.tick())
	must(t, sup.tick())

	if len(pane.injected) != 0 {
		t.Fatalf("ambiguous menu must not inject, got %q", pane.injected)
	}
	if len(manual) != 1 {
		t.Fatalf("manual notifications = %d, want 1", len(manual))
	}
}

func TestWeeklyCapDetaches(t *testing.T) {
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	// Headless-style unix ts 3 days out.
	future := clk.now().Add(72 * time.Hour).Unix()
	pane := &fakePane{screen: "working\n"}

	ad, err := adapter.Compile("claude", adapter.Spec{
		LimitPatterns: []string{`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	sup := New(Options{
		Adapter:      ad,
		Tmux:         pane,
		Prompt:       prompt.NewStatic("continue"),
		PollInterval: time.Second,
		ResetBuffer:  60 * time.Second,
		MaxWait:      24 * time.Hour,
		Now:          clk.now,
	})

	must(t, sup.tick()) // running
	pane.screen = "Claude AI usage limit reached|" + itoa(future)
	must(t, sup.tick()) // -> LIMITED
	must(t, sup.tick()) // LIMITED sees wait > max_wait -> DETACHED
	if sup.State() != state.Detached {
		t.Fatalf("state = %s, want DETACHED for a weekly cap", sup.State())
	}
	if len(pane.injected) != 0 {
		t.Fatalf("should not inject when detaching on a weekly cap, got %v", pane.injected)
	}
}

func TestCommandDetachLeavesSession(t *testing.T) {
	pane := &fakePane{screen: "working\n"}
	cmds := make(chan Command, 1)
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		Commands: cmds,
	})
	cmds <- CmdDetach
	must(t, sup.tick())
	if sup.State() != state.Detached {
		t.Fatalf("state = %s, want DETACHED", sup.State())
	}
	if pane.killed {
		t.Fatal("detach must not kill the session")
	}
}

func TestCommandKillTerminatesSession(t *testing.T) {
	pane := &fakePane{screen: "working\n"}
	cmds := make(chan Command, 1)
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		Commands: cmds,
	})
	cmds <- CmdKill
	must(t, sup.tick())
	if sup.State() != state.Detached {
		t.Fatalf("state = %s, want DETACHED", sup.State())
	}
	if !pane.killed || !sup.SessionKilled() {
		t.Fatal("kill must terminate the session")
	}
}

func TestAutoDetachOnUserAttach(t *testing.T) {
	pane := &fakePane{screen: "working\n", attached: true}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
		AutoDetach: true,
	})
	must(t, sup.tick())
	if sup.State() != state.Detached {
		t.Fatalf("state = %s, want DETACHED when a user is attached", sup.State())
	}
	if pane.killed {
		t.Fatal("auto-detach must not kill the session")
	}
}

func TestSnapshotsEmittedOnChange(t *testing.T) {
	pane := &fakePane{screen: "working\n"}
	var snaps []Snapshot
	clk := &driveClock{t: time.Date(2026, 6, 26, 10, 0, 0, 0, time.Local)}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: 60 * time.Second, MaxWait: 24 * time.Hour,
		Now:      clk.now,
		OnUpdate: func(s Snapshot) { snaps = append(snaps, s) },
	})
	must(t, sup.tick()) // RUNNING (first snapshot)
	must(t, sup.tick()) // still RUNNING, same screen -> no new snapshot
	pane.screen = "5-hour limit reached ∙ resets 11am\n"
	must(t, sup.tick()) // -> LIMITED
	must(t, sup.tick()) // -> WAITING
	if len(snaps) < 3 {
		t.Fatalf("expected snapshots for RUNNING/LIMITED/WAITING, got %d", len(snaps))
	}
	last := snaps[len(snaps)-1]
	if last.State != state.Waiting || last.WaitUntil.IsZero() {
		t.Fatalf("last snapshot = %+v, want WAITING with a wait time", last)
	}
}

// TestSessionEndedStopsCleanly verifies that when the backend reports the
// session is gone (agent exited / session killed), the supervisor moves to the
// terminal ENDED state and Run returns without killing or detaching.
func TestSessionEndedStopsCleanly(t *testing.T) {
	pane := &fakePane{screen: "working\n", ended: true}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
	})
	must(t, sup.tick())
	if sup.State() != state.Ended {
		t.Fatalf("state = %s, want ENDED", sup.State())
	}
	if !sup.SessionEnded() {
		t.Fatal("SessionEnded() should be true")
	}
	if sup.SessionKilled() {
		t.Fatal("a self-ended session is not a kill")
	}
	if !sup.stopped() {
		t.Fatal("stopped() should be true so Run returns")
	}
}

// TestTransientCaptureFailureRecovers verifies a single failed capture is
// tolerated: the supervisor keeps watching once capture succeeds again.
func TestTransientCaptureFailureRecovers(t *testing.T) {
	pane := &fakePane{screen: "working\n", captureErr: errors.New("temporary tmux glitch")}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
	})
	must(t, sup.tick()) // capture fails once
	if sup.State() != state.Running {
		t.Fatalf("state = %s, want RUNNING after a single capture hiccup", sup.State())
	}
	pane.captureErr = nil
	must(t, sup.tick()) // recovers
	if sup.State() != state.Running || sup.captureFails != 0 {
		t.Fatalf("state = %s, captureFails = %d; want RUNNING with reset counter", sup.State(), sup.captureFails)
	}
}

// TestPersistentCaptureFailureEnds verifies that an unrecoverable backend
// (capture keeps failing) makes the supervisor give up after the bound.
func TestPersistentCaptureFailureEnds(t *testing.T) {
	pane := &fakePane{screen: "working\n", captureErr: errors.New("backend down")}
	sup := New(Options{
		Adapter: testAdapter(t), Tmux: pane, Prompt: prompt.NewStatic("continue"),
		PollInterval: time.Second, ResetBuffer: time.Second, MaxWait: 24 * time.Hour,
	})
	for i := 0; i < maxCaptureFails; i++ {
		if sup.State() == state.Ended {
			t.Fatalf("ended early after %d failures (bound is %d)", i, maxCaptureFails)
		}
		must(t, sup.tick())
	}
	if sup.State() != state.Ended || !sup.SessionEnded() {
		t.Fatalf("state = %s (ended=%v), want ENDED after %d consecutive failures",
			sup.State(), sup.SessionEnded(), maxCaptureFails)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readParserTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
