//go:build !windows

package main

import "syscall"

// daemonSysProcAttr starts the background child in its own session (setsid) so it
// is detached from the controlling terminal and survives the parent exiting.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
