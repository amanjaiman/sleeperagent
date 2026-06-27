package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultAdaptersCompile(t *testing.T) {
	cfg := Default()
	for _, name := range []string{"claude", "codex"} {
		if _, err := cfg.Adapter(name); err != nil {
			t.Errorf("default adapter %q does not compile: %v", name, err)
		}
	}
	// Codex must carry the relative-duration pattern added in M3.
	codex := cfg.Agents["codex"]
	var hasDur bool
	for _, p := range codex.LimitPatterns {
		if contains(p, "(?P<dur>") {
			hasDur = true
		}
	}
	if !hasDur {
		t.Errorf("codex adapter missing a (?P<dur>) relative-duration pattern: %v", codex.LimitPatterns)
	}
	// Both default adapters define a yolo flag for the explicit --yolo opt-in.
	for _, name := range []string{"claude", "codex"} {
		if cfg.Agents[name].YoloFlag == "" {
			t.Errorf("adapter %q has no yolo_flag configured", name)
		}
		ad, _ := cfg.Adapter(name)
		if ad.YoloFlag == "" {
			t.Errorf("compiled adapter %q dropped the yolo flag", name)
		}
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if cfg.PollInterval.D() != 3*time.Second {
		t.Fatalf("poll_interval = %v, want default 3s", cfg.PollInterval.D())
	}
}

func TestOverlayReplacesAgentKeepsTimingsAndOtherAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
poll_interval = "7s"

[agents.codex]
launch_cmd = "codex"
limit_patterns = ["(?i)custom (?P<time>.+)"]
inject_style = "text-enter"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.D() != 7*time.Second {
		t.Errorf("poll_interval = %v, want overridden 7s", cfg.PollInterval.D())
	}
	if cfg.ResetBuffer.D() != 60*time.Second {
		t.Errorf("reset_buffer = %v, want default 60s preserved", cfg.ResetBuffer.D())
	}
	// User-supplied codex entry fully replaces the default one.
	if pats := cfg.Agents["codex"].LimitPatterns; len(pats) != 1 || pats[0] != "(?i)custom (?P<time>.+)" {
		t.Errorf("codex patterns = %v, want the single user pattern", pats)
	}
	// The default claude adapter is untouched.
	if _, err := cfg.Adapter("claude"); err != nil {
		t.Errorf("claude adapter should survive overlay: %v", err)
	}
}

func TestLoadAutoResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[agents.claude]
launch_cmd = "claude"
limit_patterns = ["(?i)limit reached (?P<time>.+)"]

[[agents.claude.auto_responses]]
pattern = "(?i)stop and wait for the limit to reset"
keys = "1\r"
once = true
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := cfg.Adapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.AutoResponses) != 1 {
		t.Fatalf("auto responses = %d, want 1", len(ad.AutoResponses))
	}
	if ad.AutoResponses[0].Keys != "1\r" || !ad.AutoResponses[0].Once {
		t.Fatalf("auto response = %+v", ad.AutoResponses[0])
	}
	if !ad.AutoResponses[0].Pattern.MatchString("Stop and wait for the limit to reset") {
		t.Fatal("compiled auto-response pattern did not match")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
