// Package statefile persists one supervisor instance's state to disk so that
// `sleeperagent status` can report from any shell, and so a control command
// (detach/stop) can reach a running supervisor without an open socket. The
// control channel is a tiny file the supervisor polls — deliberately simple and
// cross-platform (no ports, no OS-specific socket code); latency is bounded by
// poll_interval, which is fine for a watchdog.
package statefile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Record is the on-disk snapshot of a supervisor instance.
type Record struct {
	Name            string    `json:"name"`
	Agent           string    `json:"agent"`
	Session         string    `json:"tmux_session"`
	State           string    `json:"state"`
	ResetTime       time.Time `json:"reset_time,omitempty"`
	ResetSource     string    `json:"reset_source,omitempty"`
	ResetConfidence string    `json:"reset_confidence,omitempty"`
	WaitUntil       time.Time `json:"wait_until,omitempty"`
	PromptMode      string    `json:"prompt_mode"`
	PromptText      string    `json:"prompt_text"`
	PID             int       `json:"pid"`
	AttachHint      string    `json:"attach_hint"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Dir is the directory holding state and control files. An explicit
// SLEEPERAGENT_STATE_DIR overrides everything (used in tests); otherwise it
// prefers the XDG state location on Unix and the OS config dir on Windows.
func Dir() string {
	if d := os.Getenv("SLEEPERAGENT_STATE_DIR"); d != "" {
		return d
	}
	if runtime.GOOS != "windows" {
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			return filepath.Join(x, "sleeperagent")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "state", "sleeperagent")
		}
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfg, "sleeperagent", "state")
	}
	return filepath.Join(os.TempDir(), "sleeperagent")
}

// safeName rejects names that would escape the state dir.
func safeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("instance name is required")
	}
	if strings.ContainsAny(name, `/\:`) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid instance name %q", name)
	}
	return name, nil
}

// Path returns the state-file path for an instance name.
func Path(name string) (string, error) {
	n, err := safeName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(Dir(), n+".json"), nil
}

// ControlPath returns the control-file path for an instance name.
func ControlPath(name string) (string, error) {
	n, err := safeName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(Dir(), n+".control"), nil
}

// Write atomically persists rec (write to a temp file, then rename).
func Write(rec Record) error {
	path, err := Path(rec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read loads one instance's record.
func Read(name string) (Record, error) {
	path, err := Path(name)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

// List returns every instance record, sorted by name.
func List() ([]Record, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if rec, err := Read(name); err == nil {
			recs = append(recs, rec)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	return recs, nil
}

// Remove deletes an instance's state and control files (best effort).
func Remove(name string) {
	if p, err := Path(name); err == nil {
		os.Remove(p)
	}
	if p, err := ControlPath(name); err == nil {
		os.Remove(p)
	}
}

// WriteControl drops a one-shot command ("detach" or "kill") for a running
// supervisor to pick up.
func WriteControl(name, command string) error {
	path, err := ControlPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(command), 0o644)
}

// TakeControl reads and removes any pending control command. ok is false when no
// command is waiting.
func TakeControl(name string) (command string, ok bool, err error) {
	path, perr := ControlPath(name)
	if perr != nil {
		return "", false, perr
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", false, nil
		}
		return "", false, rerr
	}
	os.Remove(path)
	return strings.TrimSpace(string(b)), true, nil
}
