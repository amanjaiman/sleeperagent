package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/amanjaiman/sleeperagent/internal/adapter"
	"github.com/amanjaiman/sleeperagent/internal/config"
	"github.com/amanjaiman/sleeperagent/internal/notify"
	"github.com/amanjaiman/sleeperagent/internal/prompt"
	"github.com/amanjaiman/sleeperagent/internal/statefile"
)

type watchTestPane struct {
	screen string
	ended  bool
}

func (p *watchTestPane) Capture(int) (string, error)   { return p.screen, nil }
func (p *watchTestPane) Inject(string, string) error   { return nil }
func (p *watchTestPane) AttachHint() string            { return "tmux attach -t test" }
func (p *watchTestPane) Kill() error                   { return nil }
func (p *watchTestPane) ClientAttached() (bool, error) { return false, nil }
func (p *watchTestPane) Ended() (bool, error)          { return p.ended, nil }

type captureNotifier struct {
	events []notify.Event
}

func (n *captureNotifier) Notify(e notify.Event) {
	n.events = append(n.events, e)
}

func TestWatchSessionRemovesRecordAfterCleanSessionEnd(t *testing.T) {
	t.Setenv("SLEEPERAGENT_STATE_DIR", t.TempDir())
	ad, err := adapter.Compile("claude", adapter.Spec{
		LaunchCmd:     "claude",
		LimitPatterns: []string{`(?i)limit reached.*resets\s+(?P<time>.+)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	notifier := &captureNotifier{}

	err = watchSession(watchParams{
		instance:    "clean-end",
		agent:       "claude",
		adapter:     ad,
		pane:        &watchTestPane{screen: "bye\n", ended: true},
		attachHint:  "tmux attach -t test",
		builder:     prompt.NewStatic("continue"),
		promptMode:  "static",
		resumeText:  "continue",
		cfg:         config.Default(),
		afterDetach: func(context.Context) {},
		notifier:    notifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statefile.Read("clean-end"); !os.IsNotExist(err) {
		t.Fatalf("state record should be removed after clean end; read err=%v", err)
	}
	var ended bool
	for _, e := range notifier.events {
		if strings.Contains(e.Title, "session ended") {
			ended = true
		}
	}
	if !ended {
		t.Fatalf("session-ended notification did not fire; events=%v", notifier.events)
	}
}

// TestAutoAnswerPromptsFlagDefaultsToTrue mirrors how runCmd and
// attachExistingCmd declare --auto-answer-prompts and confirms it now
// defaults to true, and that --auto-answer-prompts=false still opts out.
func TestAutoAnswerPromptsFlagDefaultsToTrue(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	autoAnswerPrompts := fs.Bool("auto-answer-prompts", true, "answer interactive prompts with the first/default option so the run does not stall while you're away (default true, pass =false to disable)")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if !*autoAnswerPrompts {
		t.Fatalf("--auto-answer-prompts default = %v, want true", *autoAnswerPrompts)
	}
}

func TestAutoAnswerPromptsFlagCanBeDisabled(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	autoAnswerPrompts := fs.Bool("auto-answer-prompts", true, "answer interactive prompts with the first/default option so the run does not stall while you're away (default true, pass =false to disable)")
	if err := fs.Parse([]string{"--auto-answer-prompts=false"}); err != nil {
		t.Fatal(err)
	}
	if *autoAnswerPrompts {
		t.Fatalf("--auto-answer-prompts=false did not disable the flag")
	}
}
