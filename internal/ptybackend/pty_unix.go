//go:build !windows

// Package ptybackend is the no-tmux fallback: it runs the agent in a pseudo-
// terminal owned by the supervisor. Handoff is reduced versus tmux (the agent
// is bound to this process), but detection/wait/resume work the same, and on
// detach the terminal is handed to the user until the agent exits.
package ptybackend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
	"github.com/creack/pty"
	"golang.org/x/term"
)

// Client implements supervisor.Pane over a pseudo-terminal.
type Client struct {
	cmd  *exec.Cmd
	ptmx *os.File
	echo bool // stream agent output to stdout (only when stdout is a terminal)
	mu   sync.Mutex
	ring []byte
	done chan struct{}
}

// New returns an unstarted PTY backend.
func New() (*Client, error) { return &Client{done: make(chan struct{})}, nil }

// Start launches command in a pty and begins streaming its output to the user's
// terminal (when present) and an internal ring buffer.
func (c *Client) Start(command string) error {
	c.cmd = exec.Command("sh", "-c", command)
	f, err := pty.Start(c.cmd)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	c.ptmx = f
	c.echo = term.IsTerminal(int(os.Stdout.Fd()))
	go c.pump()
	return nil
}

func (c *Client) pump() {
	defer close(c.done)
	buf := make([]byte, 4096)
	for {
		n, err := c.ptmx.Read(buf)
		if n > 0 {
			c.appendRing(buf[:n])
			if c.echo {
				os.Stdout.Write(buf[:n]) // no tmux to attach to, so show it live
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) appendRing(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring = append(c.ring, p...)
	if len(c.ring) > ringSize {
		c.ring = c.ring[len(c.ring)-ringSize:]
	}
}

// Capture returns the last `scrollback` lines of output with ANSI control
// sequences stripped, so limit-string matching works on a raw stream.
func (c *Client) Capture(scrollback int) (string, error) {
	c.mu.Lock()
	raw := string(c.ring)
	c.mu.Unlock()

	lines := strings.Split(stripANSI(raw), "\n")
	if scrollback > 0 && len(lines) > scrollback {
		lines = lines[len(lines)-scrollback:]
	}
	return strings.Join(lines, "\n"), nil
}

// Inject writes the resume keystrokes to the pty (Enter is a carriage return).
func (c *Client) Inject(text, style string) error {
	if style == adapter.InjectKeys {
		_, err := c.ptmx.Write([]byte(text))
		return err
	}
	if style == "esc-text-enter" {
		if _, err := c.ptmx.Write([]byte{0x1b}); err != nil {
			return err
		}
	}
	if _, err := c.ptmx.Write([]byte(text)); err != nil {
		return err
	}
	_, err := c.ptmx.Write([]byte("\r"))
	return err
}

// Kill terminates the agent process.
func (c *Client) Kill() error {
	if c.cmd != nil && c.cmd.Process != nil {
		return c.cmd.Process.Kill()
	}
	return nil
}

// ClientAttached always reports false: a self-managed pty has no notion of a
// separate human client, so auto-detach-on-attach is unavailable in this mode.
func (c *Client) ClientAttached() (bool, error) { return false, nil }

// Ended reports whether the agent process has exited. The pump goroutine closes
// c.done when the pty reaches EOF (the child exited), so the supervisor stops
// instead of polling a stale ring buffer forever.
func (c *Client) Ended() (bool, error) {
	select {
	case <-c.done:
		return true, nil
	default:
		return false, nil
	}
}

// AttachHint explains the reduced-handoff reality of PTY mode.
func (c *Client) AttachHint() string {
	return "PTY mode: the agent is bound to this process (no tmux re-attach)"
}

// Foreground hands the terminal to the user: raw stdin is forwarded to the pty
// until the agent exits or ctx is cancelled. Output already streams via pump.
func (c *Client) Foreground(ctx context.Context) error {
	if c.ptmx == nil {
		return nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil // no terminal to hand off to (e.g. running in the background)
	}
	if old, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, old)
	}
	go func() { _, _ = io.Copy(c.ptmx, os.Stdin) }()
	select {
	case <-ctx.Done():
	case <-c.done:
	}
	return nil
}

// Close releases the pty.
func (c *Client) Close() {
	if c.ptmx != nil {
		c.ptmx.Close()
	}
}
