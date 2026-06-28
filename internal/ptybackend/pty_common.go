package ptybackend

import (
	"os"
	"regexp"
)

// ringSize bounds how much recent pty/conpty output is retained for Capture.
const ringSize = 64 * 1024

// ansi matches OSC sequences, CSI sequences, two-char escapes, CR and NUL — the
// control noise a real TUI emits, stripped so limit-string matching sees text.
var ansi = regexp.MustCompile(`\x1b\][^\x07]*(\x07|\x1b\\)|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]|[\r\x00]`)

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

const terminalRestoreSequence = "" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l" +
	"\x1b[?2004l" +
	"\x1b[?1049l" +
	"\x1b[?25h" +
	"\x1b[0m\r\n"

func restoreTerminal() {
	_, _ = os.Stdout.WriteString(terminalRestoreSequence)
}
