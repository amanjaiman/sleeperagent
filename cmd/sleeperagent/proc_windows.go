//go:build windows

package main

import "os"

// processAlive on Windows has no cheap portable liveness probe (os.FindProcess
// always succeeds), so we conservatively assume the process is alive. SleeperAgent
// primarily targets Unix where the signal-0 probe is exact.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
