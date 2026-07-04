//go:build !windows

package main

import (
	"os"
	"syscall"
)

// processAlive reports whether a process with the given PID is running, using
// the signal-0 liveness probe.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
