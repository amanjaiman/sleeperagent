//go:build windows

// Package ptybackend's Windows implementation runs the agent inside a ConPTY
// (Windows pseudoconsole, available on Windows 10 1809+). This makes the no-tmux
// backend work natively on Windows — no WSL — with the same detection/wait/resume
// behavior as the Unix pty backend and the same reduced handoff (the agent is
// bound to this process).
package ptybackend

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// ConPTY + attribute-list entry points, loaded lazily so the package links on
// older Windows; New() reports unavailability instead of failing at runtime.
var (
	kernel32                              = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole               = kernel32.NewProc("CreatePseudoConsole")
	procClosePseudoConsole                = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
)

// procThreadAttributePseudoConsole = PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE.
const procThreadAttributePseudoConsole = 0x00020016

// Client implements supervisor.Pane over a Windows pseudoconsole.
type Client struct {
	hpcon   windows.Handle
	inWrite *os.File // we write injected keystrokes here
	outRead *os.File // we read the agent's terminal output here
	procH   windows.Handle
	threadH windows.Handle
	echo    bool   // stream agent output to stdout (only when stdout is a terminal)
	attrBuf []byte // backing store for the proc-thread attribute list (kept alive)

	mu       sync.Mutex
	ring     []byte
	done     chan struct{}
	doneOnce sync.Once
}

// markEnded closes done exactly once (the child exited or we tore the pty down).
func (c *Client) markEnded() { c.doneOnce.Do(func() { close(c.done) }) }

// New returns an unstarted ConPTY backend, or an error if ConPTY is unavailable.
func New() (*Client, error) {
	if err := procCreatePseudoConsole.Find(); err != nil {
		return nil, fmt.Errorf("ConPTY is not available on this Windows version (needs Windows 10 1809+): %w", err)
	}
	return &Client{done: make(chan struct{})}, nil
}

// coord packs width/height into the DWORD that CreatePseudoConsole expects.
func coord(width, height int16) uintptr {
	return uintptr(uint32(uint16(width)) | uint32(uint16(height))<<16)
}

// Start launches command in a ConPTY and begins streaming its output to the
// console and an internal ring buffer.
func (c *Client) Start(command string) error {
	// Two pipes: one feeds the console's input, one drains its output.
	var ptyIn, ptyOut windows.Handle // console-side ends
	var inWrite, outRead windows.Handle
	if err := windows.CreatePipe(&ptyIn, &inWrite, nil, 0); err != nil {
		return fmt.Errorf("create input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &ptyOut, nil, 0); err != nil {
		windows.CloseHandle(ptyIn)
		windows.CloseHandle(inWrite)
		return fmt.Errorf("create output pipe: %w", err)
	}

	// Create the pseudoconsole from the console-side pipe ends.
	r, _, _ := procCreatePseudoConsole.Call(coord(120, 30), uintptr(ptyIn), uintptr(ptyOut), 0, uintptr(unsafe.Pointer(&c.hpcon)))
	windows.CloseHandle(ptyIn) // the console duplicated these; drop our copies
	windows.CloseHandle(ptyOut)
	if r != 0 {
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return fmt.Errorf("CreatePseudoConsole failed (HRESULT 0x%x)", r)
	}
	c.inWrite = os.NewFile(uintptr(inWrite), "conpty-in")
	c.outRead = os.NewFile(uintptr(outRead), "conpty-out")

	// Build a STARTUPINFOEX whose attribute list carries the pseudoconsole.
	if err := c.buildAttributeList(); err != nil {
		c.Close()
		return err
	}
	si := new(windows.StartupInfoEx)
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(*si))
	// Required so the child attaches to the pseudoconsole (which supplies its std
	// handles) rather than inheriting the parent's console.
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = (*windows.ProcThreadAttributeList)(unsafe.Pointer(&c.attrBuf[0]))

	cmdline, err := windows.UTF16PtrFromString(command)
	if err != nil {
		c.Close()
		return err
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil,      // application name (parsed from cmdline; searches PATH)
		cmdline,  // command line
		nil, nil, // process/thread security
		false, // inherit handles (ConPTY is wired via the attribute, not inheritance)
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		nil, // environment
		nil, // current dir
		&si.StartupInfo,
		&pi,
	)
	if err != nil {
		c.Close()
		return fmt.Errorf("CreateProcess %q: %w", command, err)
	}
	c.procH = pi.Process
	c.threadH = pi.Thread
	c.echo = term.IsTerminal(int(os.Stdout.Fd()))

	go c.pump()
	// conhost (not the child) holds the output pipe's write end, so the pipe never
	// EOFs on child exit. Detect exit by waiting on the process handle instead.
	go func() {
		windows.WaitForSingleObject(c.procH, windows.INFINITE)
		c.markEnded()
	}()
	return nil
}

// buildAttributeList allocates a proc-thread attribute list holding the
// pseudoconsole. Uses the raw procs so the HPCON is passed as a plain uintptr,
// avoiding an unsafe uintptr->Pointer conversion of a handle (which go vet flags).
func (c *Client) buildAttributeList() error {
	var size uintptr
	// First call reports the required size (it "fails" with a NULL buffer).
	procInitializeProcThreadAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return fmt.Errorf("InitializeProcThreadAttributeList: zero size")
	}
	c.attrBuf = make([]byte, size)
	r, _, err := procInitializeProcThreadAttributeList.Call(
		uintptr(unsafe.Pointer(&c.attrBuf[0])), 1, 0, uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return fmt.Errorf("InitializeProcThreadAttributeList: %w", err)
	}
	r, _, err = procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&c.attrBuf[0])), 0,
		procThreadAttributePseudoConsole,
		uintptr(c.hpcon), unsafe.Sizeof(c.hpcon),
		0, 0)
	if r == 0 {
		return fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}
	return nil
}

func (c *Client) pump() {
	defer c.markEnded()
	buf := make([]byte, 4096)
	for {
		n, err := c.outRead.Read(buf)
		if n > 0 {
			c.appendRing(buf[:n])
			if c.echo {
				os.Stdout.Write(buf[:n]) // no tmux to attach to: show the agent live
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

// Capture returns the last `scrollback` lines of output, ANSI-stripped.
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

// Inject writes the resume keystrokes to the console input (Enter = CR).
func (c *Client) Inject(text, style string) error {
	if c.inWrite == nil {
		return fmt.Errorf("conpty not started")
	}
	if style == adapter.InjectKeys {
		_, err := c.inWrite.Write([]byte(text))
		return err
	}
	if style == "esc-text-enter" {
		if _, err := c.inWrite.Write([]byte{0x1b}); err != nil {
			return err
		}
	}
	if _, err := c.inWrite.Write([]byte(text)); err != nil {
		return err
	}
	_, err := c.inWrite.Write([]byte("\r"))
	return err
}

// Kill terminates the agent process.
func (c *Client) Kill() error {
	if c.procH != 0 {
		return windows.TerminateProcess(c.procH, 1)
	}
	return nil
}

// ClientAttached always reports false: a self-managed ConPTY has no separate
// human client, so auto-detach-on-attach is unavailable in this mode.
func (c *Client) ClientAttached() (bool, error) { return false, nil }

// Ended reports whether the agent has exited (the pump closes done at EOF).
func (c *Client) Ended() (bool, error) {
	select {
	case <-c.done:
		return true, nil
	default:
		return false, nil
	}
}

// AttachHint explains the reduced-handoff reality of ConPTY mode.
func (c *Client) AttachHint() string {
	return "ConPTY mode: the agent is bound to this process (no re-attach)"
}

// Foreground hands the terminal to the user: raw stdin is forwarded to the
// console input until the agent exits or ctx is cancelled.
func (c *Client) Foreground(ctx context.Context) error {
	if c.inWrite == nil {
		return nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil // no terminal to hand off to (e.g. running in the background)
	}
	if old, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, old)
	}
	go func() { _, _ = io.Copy(c.inWrite, os.Stdin) }()
	select {
	case <-ctx.Done():
	case <-c.done:
	}
	return nil
}

// Close tears down the pseudoconsole and releases all handles.
func (c *Client) Close() {
	if c.hpcon != 0 {
		procClosePseudoConsole.Call(uintptr(c.hpcon))
		c.hpcon = 0
	}
	if c.attrBuf != nil {
		procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&c.attrBuf[0])))
		c.attrBuf = nil
	}
	if c.inWrite != nil {
		c.inWrite.Close()
		c.inWrite = nil
	}
	if c.outRead != nil {
		c.outRead.Close()
		c.outRead = nil
	}
	if c.threadH != 0 {
		windows.CloseHandle(c.threadH)
		c.threadH = 0
	}
	if c.procH != 0 {
		windows.CloseHandle(c.procH)
		c.procH = 0
	}
}
