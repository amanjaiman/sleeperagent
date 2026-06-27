// Package tmux is the primary session backend. The agent runs inside a tmux
// session that outlives the supervisor, so the supervisor only ever reads
// (capture-pane) and writes (send-keys) — never owns the terminal. That
// decoupling is what makes graceful handoff trivial: "stop listening" is just
// the supervisor going away while `tmux attach` keeps the live conversation.
package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
)

// Client drives a single tmux session identified by its name. The session's
// default window/pane is used as the target.
type Client struct {
	Session string
	bin     string
}

// New returns a Client for the named session. tmuxBin defaults to "tmux".
func New(session, tmuxBin string) *Client {
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	return &Client{Session: session, bin: tmuxBin}
}

// Available reports whether the tmux binary is on PATH.
func (c *Client) Available() error {
	if _, err := exec.LookPath(c.bin); err != nil {
		return fmt.Errorf("tmux not found on PATH (install tmux or use the PTY backend): %w", err)
	}
	return nil
}

func (c *Client) run(args ...string) (string, error) {
	cmd := exec.Command(c.bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// HasSession reports whether the session already exists.
func (c *Client) HasSession() bool {
	err := exec.Command(c.bin, "has-session", "-t", c.Session).Run()
	return err == nil
}

// NewSession starts a detached session running command. command is passed to a
// shell by tmux, so callers may include arguments.
func (c *Client) NewSession(command string) error {
	if c.HasSession() {
		return fmt.Errorf("tmux session %q already exists", c.Session)
	}
	_, err := c.run("new-session", "-d", "-s", c.Session, command)
	return err
}

// Capture returns the pane's contents including the last `scrollback` lines of
// history, so a limit message that scrolled just off-screen is still seen.
func (c *Client) Capture(scrollback int) (string, error) {
	if scrollback < 0 {
		scrollback = 0
	}
	return c.run("capture-pane", "-p", "-t", c.Session, "-S", fmt.Sprintf("-%d", scrollback))
}

// Inject delivers text to the pane using the adapter's inject style: optionally
// an Esc to focus the input box, the literal text, then Enter to submit. The
// text is sent with -l (literal) so it is never interpreted as a key name.
func (c *Client) Inject(text, style string) error {
	if style == adapter.InjectKeys {
		return c.injectKeys(text)
	}
	if style == adapter.InjectEscTextEnter {
		if _, err := c.run("send-keys", "-t", c.Session, "Escape"); err != nil {
			return err
		}
	}
	if _, err := c.run("send-keys", "-t", c.Session, "-l", text); err != nil {
		return err
	}
	_, err := c.run("send-keys", "-t", c.Session, "Enter")
	return err
}

func (c *Client) injectKeys(keys string) error {
	var literal strings.Builder
	flush := func() error {
		if literal.Len() == 0 {
			return nil
		}
		_, err := c.run("send-keys", "-t", c.Session, "-l", literal.String())
		literal.Reset()
		return err
	}
	for _, r := range keys {
		switch r {
		case '\r', '\n':
			if err := flush(); err != nil {
				return err
			}
			if _, err := c.run("send-keys", "-t", c.Session, "Enter"); err != nil {
				return err
			}
		case '\x1b':
			if err := flush(); err != nil {
				return err
			}
			if _, err := c.run("send-keys", "-t", c.Session, "Escape"); err != nil {
				return err
			}
		default:
			literal.WriteRune(r)
		}
	}
	return flush()
}

// Kill terminates the session (and the agent inside it).
func (c *Client) Kill() error {
	_, err := c.run("kill-session", "-t", c.Session)
	return err
}

// ClientAttached reports whether any tmux client is attached to the session,
// i.e. a human is viewing or driving it. The supervisor never attaches, so any
// attached client means the user has taken over.
func (c *Client) ClientAttached() (bool, error) {
	out, err := c.run("list-clients", "-t", c.Session)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Ended reports whether the session is gone: the agent exited or the session
// was killed (e.g. `tmux kill-session`). It is true once tmux no longer has the
// session, which is how the supervisor learns to stop cleanly.
func (c *Client) Ended() (bool, error) {
	return !c.HasSession(), nil
}

// AttachHint is the command a user runs to take over the live session.
func (c *Client) AttachHint() string {
	return fmt.Sprintf("tmux attach -t %s", c.Session)
}
