// Package hotkeys provides the supervisor's foreground key controls: d/q to
// detach (keep the session), k to kill it (with confirmation). It reads single
// keypresses in raw mode and is a no-op when stdin is not a terminal, so a
// backgrounded `sleeperagent run` is unaffected.
package hotkeys

import (
	"context"
	"os"

	"golang.org/x/term"

	"github.com/amanjaiman/sleeperagent/internal/supervisor"
)

// Legend is the one-line hint printed alongside the status output.
const Legend = "[d]etach (keep session)  [q]uit  [k]ill session"

// Listen reads keypresses and forwards supervisor commands until ctx is done or
// stdin closes. confirmKill is consulted before a kill is sent. It restores the
// terminal on exit. Returns immediately (active=false) when stdin is not a TTY.
func Listen(ctx context.Context, cmds chan<- supervisor.Command, logf func(string, ...any)) (active bool) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return false
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}

	go func() {
		defer term.Restore(fd, oldState)
		buf := make([]byte, 1)
		awaitKillConfirm := false
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			c := buf[0]
			if awaitKillConfirm {
				awaitKillConfirm = false
				if c == 'y' || c == 'Y' {
					logf("kill confirmed")
					send(cmds, supervisor.CmdKill)
					return
				}
				logf("kill cancelled")
				continue
			}
			switch c {
			case 'd', 'q':
				logf("detaching (session left running)")
				send(cmds, supervisor.CmdDetach)
				return
			case 'k':
				logf("kill session? press y to confirm, any other key to cancel")
				awaitKillConfirm = true
			case 3: // Ctrl-C in raw mode: treat as detach, never kill
				logf("interrupt; detaching (session left running)")
				send(cmds, supervisor.CmdDetach)
				return
			}
		}
	}()
	return true
}

func send(cmds chan<- supervisor.Command, c supervisor.Command) {
	select {
	case cmds <- c:
	default:
	}
}
