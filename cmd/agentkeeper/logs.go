package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/amanjaiman/agentkeeper/internal/statefile"
)

// instanceLogPath is where a pty/daemon instance's supervisor log is written.
func instanceLogPath(instance string) string {
	return filepath.Join(statefile.Dir(), instance+".log")
}

// logsCmd prints (and optionally follows) an instance's supervisor log. The log
// exists for pty-backend and --daemon runs, where supervisor output is kept off
// the terminal so it doesn't corrupt the agent's TUI.
func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	name := fs.String("name", "", "instance name (required)")
	follow := fs.Bool("follow", false, "keep printing as the log grows (like tail -f)")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}

	path := instanceLogPath(*name)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no log file for instance %q at %s\n"+
				"(logs are written by the pty backend and --daemon runs; a tmux foreground run logs to its own terminal)",
				*name, path)
		}
		return err
	}
	defer f.Close()

	// Print everything written so far, then optionally keep streaming appends.
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	if !*follow {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			// io.Copy resumes from the current offset, so each tick prints only
			// the bytes appended since the last read.
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return err
			}
		}
	}
}
