package ptybackend

import (
	"strings"
	"testing"
)

func TestTerminalRestoreSequenceDisablesTUIPrivateModes(t *testing.T) {
	for _, want := range []string{
		"\x1b[?1000l",
		"\x1b[?1002l",
		"\x1b[?1003l",
		"\x1b[?1006l",
		"\x1b[?1015l",
		"\x1b[?1004l",
		"\x1b[?2004l",
		"\x1b[?1049l",
		"\x1b[?25h",
		"\x1b[0m\r\n",
	} {
		if !strings.Contains(terminalRestoreSequence, want) {
			t.Fatalf("restore sequence missing %q in %q", want, terminalRestoreSequence)
		}
	}
}
