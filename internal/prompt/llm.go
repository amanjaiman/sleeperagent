package prompt

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/amanjaiman/sleeperagent/internal/transcript"
)

// Generator produces a completion from a model. *ollama.Client satisfies it;
// keeping it an interface lets other local backends slot in and makes the
// builder unit-testable without a server.
type Generator interface {
	Generate(ctx context.Context, model, prompt string) (string, error)
}

// LLM builds the resume prompt by asking a local model for a concrete next
// instruction, given the recent transcript and the git diff of work done. It is
// purely additive: on any failure (server down, empty/over-long/denylisted
// output) it returns Fallback, so SleeperAgent always resumes.
type LLM struct {
	Model          string
	Client         Generator
	TranscriptGlob string
	TailMessages   int
	MaxChars       int
	Denylist       []string
	Fallback       string
	Timeout        time.Duration
	Logf           func(string, ...any)

	// gitContext is overridable in tests; nil uses the real git invocation.
	gitContext func(cwd string) string
}

const metaPrompt = `You are resuming a coding agent whose usage limit just reset.
Below are the recent session messages and the git diff of work completed so far.
Summarize to yourself what is done, then write a SINGLE, concrete next instruction
that continues the SAME task. Do not introduce new scope or new features. Reply
with ONLY the instruction text, on one line, no preamble or quotes.

=== RECENT SESSION ===
%s

=== WORK COMPLETED (git) ===
%s
`

func (l LLM) Build(pc Context) (string, error) {
	out, err := l.generate(pc)
	if err != nil {
		l.logf("reprompt: falling back to static prompt (%v)", err)
		return l.Fallback, nil
	}
	l.logf("reprompt: generated a continuation instruction via %s", l.Model)
	return out, nil
}

func (l LLM) generate(pc Context) (string, error) {
	tail, terr := transcript.Tail(l.TranscriptGlob, l.TailMessages)
	if terr != nil {
		l.logf("reprompt: transcript unavailable (%v)", terr)
	}
	diff := l.git(pc.Cwd)
	if strings.TrimSpace(tail) == "" && strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("no transcript or git context to summarize")
	}

	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	raw, err := l.Client.Generate(ctx, l.Model, fmt.Sprintf(metaPrompt, orNone(tail), orNone(diff)))
	if err != nil {
		return "", err
	}
	out := sanitize(raw)
	if err := l.validate(out); err != nil {
		return "", err
	}
	return out, nil
}

// validate enforces non-empty, length cap, and the denylist before anything is
// injected unattended.
func (l LLM) validate(s string) error {
	if s == "" {
		return fmt.Errorf("model returned an empty instruction")
	}
	if l.MaxChars > 0 && len(s) > l.MaxChars {
		return fmt.Errorf("instruction exceeds max_prompt_chars (%d > %d)", len(s), l.MaxChars)
	}
	lower := strings.ToLower(s)
	for _, term := range l.Denylist {
		if term == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(term)) {
			return fmt.Errorf("instruction contains denylisted term %q", term)
		}
	}
	return nil
}

// sanitize trims, unwraps surrounding quotes/backticks, and collapses the result
// to a single line so it injects as one submitted prompt.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func (l LLM) git(cwd string) string {
	if l.gitContext != nil {
		return l.gitContext(cwd)
	}
	return defaultGitContext(cwd)
}

// defaultGitContext returns a short summary of recent work in cwd, or "" when
// cwd is not a git repo. Best effort: any git error yields an empty section.
func defaultGitContext(cwd string) string {
	if cwd == "" {
		return ""
	}
	if out, err := runGit(cwd, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return ""
	}
	var b strings.Builder
	if stat, err := runGit(cwd, "diff", "--stat"); err == nil && strings.TrimSpace(stat) != "" {
		b.WriteString("uncommitted changes:\n")
		b.WriteString(strings.TrimSpace(stat))
		b.WriteString("\n")
	}
	if logs, err := runGit(cwd, "log", "--oneline", "-n", "5"); err == nil && strings.TrimSpace(logs) != "" {
		b.WriteString("recent commits:\n")
		b.WriteString(strings.TrimSpace(logs))
	}
	return strings.TrimSpace(b.String())
}

func runGit(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unavailable)"
	}
	return s
}

func (l LLM) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}
