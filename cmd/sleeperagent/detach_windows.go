//go:build windows

package main

import "syscall"

// Windows process creation flags (not all are exported by the syscall package).
const (
	createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	detachedProcess       = 0x00000008 // DETACHED_PROCESS
)

// daemonSysProcAttr starts the background child detached from the parent console
// and in a new process group, so it survives the parent exiting.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
