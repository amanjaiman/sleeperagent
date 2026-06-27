// Package config loads AgentKeeper settings from a TOML file, layered over
// built-in defaults. The adapter patterns live here so that an agent CLI
// format change is a one-line user fix, not a new release.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/amanjaiman/agentkeeper/internal/adapter"
)

// Duration is a time.Duration that decodes from TOML strings like "3s".
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// AgentConfig is the raw adapter config for one agent.
type AgentConfig struct {
	LaunchCmd      string               `toml:"launch_cmd"`
	ResumeCmd      string               `toml:"resume_cmd"`
	LimitPatterns  []string             `toml:"limit_patterns"`
	IdlePattern    string               `toml:"idle_pattern"`
	InjectStyle    string               `toml:"inject_style"`
	TranscriptGlob string               `toml:"transcript_glob"`
	YoloFlag       string               `toml:"yolo_flag"`
	AutoResponses  []AutoResponseConfig `toml:"auto_responses"`
}

// AutoResponseConfig configures one safe prompt/menu response.
type AutoResponseConfig struct {
	Pattern string `toml:"pattern"`
	Keys    string `toml:"keys"`
	Once    bool   `toml:"once"`
}

// RepromptConfig configures the optional local-LLM prompt builder (M4).
type RepromptConfig struct {
	Provider       string   `toml:"provider"`
	Model          string   `toml:"model"`
	BaseURL        string   `toml:"base_url"`
	MaxPromptChars int      `toml:"max_prompt_chars"`
	TailMessages   int      `toml:"tail_messages"`
	Denylist       []string `toml:"denylist"`
}

// Config is the whole AgentKeeper configuration.
type Config struct {
	PollInterval Duration               `toml:"poll_interval"`
	ResetBuffer  Duration               `toml:"reset_buffer"`
	MaxWait      Duration               `toml:"max_wait"`
	Agents       map[string]AgentConfig `toml:"agents"`
	Reprompt     RepromptConfig         `toml:"reprompt"`
}

// Default returns the built-in configuration, including the Claude Code adapter.
func Default() Config {
	return Config{
		PollInterval: Duration(3 * time.Second),
		ResetBuffer:  Duration(60 * time.Second),
		MaxWait:      Duration(24 * time.Hour),
		Agents: map[string]AgentConfig{
			"claude": {
				LaunchCmd: "claude",
				ResumeCmd: "claude -c",
				LimitPatterns: []string{
					`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`,
					`(?i)limit.*?reset[s]?\s+in\s+(?P<dur>[^\r\n.]+)`,
					`(?i)(?:usage|session|weekly|\d+-?hour)\s+limit\s+reached.*?reset[s]?(?:\s+at)?\s+(?P<time>[^\r\n.]+)`,
					`(?i)hit your\s+(?:usage|session|weekly)?\s*limit.*?reset[s]?(?:\s+at)?\s+(?P<time>[^\r\n.]+)`,
				},
				InjectStyle:    adapter.InjectEscTextEnter,
				TranscriptGlob: "~/.claude/projects/*/*.jsonl",
				YoloFlag:       "--dangerously-skip-permissions",
			},
			"codex": {
				LaunchCmd: "codex",
				ResumeCmd: "codex resume",
				LimitPatterns: []string{
					`(?i)try again at (?P<time>.+)`,
					`(?i)try again in (?P<dur>.+)`,
					`(?i)rate limit.*reset[a-z ]*in (?P<dur>.+)`,
				},
				InjectStyle:    adapter.InjectTextEnter,
				TranscriptGlob: "~/.codex/sessions/**/*.jsonl",
				YoloFlag:       "--dangerously-bypass-approvals-and-sandbox",
			},
		},
		Reprompt: RepromptConfig{
			Provider:       "ollama",
			Model:          "llama3.1",
			MaxPromptChars: 600,
			TailMessages:   20,
			Denylist:       []string{"rm -rf", "force push", "--force", "drop table"},
		},
	}
}

// DefaultPath is the conventional config location.
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "agentkeeper", "config.toml")
	}
	return ""
}

// Load reads the config at path, layered over Default(). A missing file is not
// an error: defaults are returned. An empty path uses DefaultPath().
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	if path == "" {
		return cfg, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("stat config %s: %w", path, err)
	}
	// Decode user config into a fresh struct, then overlay onto defaults so a
	// partial file (e.g. only poll_interval) keeps the rest of the defaults.
	var user Config
	if _, err := toml.DecodeFile(path, &user); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	overlay(&cfg, user)
	return cfg, nil
}

func overlay(base *Config, user Config) {
	if user.PollInterval != 0 {
		base.PollInterval = user.PollInterval
	}
	if user.ResetBuffer != 0 {
		base.ResetBuffer = user.ResetBuffer
	}
	if user.MaxWait != 0 {
		base.MaxWait = user.MaxWait
	}
	for name, ac := range user.Agents {
		base.Agents[name] = ac // user-supplied agent fully replaces the default entry
	}
	if user.Reprompt.Provider != "" {
		base.Reprompt.Provider = user.Reprompt.Provider
	}
	if user.Reprompt.Model != "" {
		base.Reprompt.Model = user.Reprompt.Model
	}
	if user.Reprompt.BaseURL != "" {
		base.Reprompt.BaseURL = user.Reprompt.BaseURL
	}
	if user.Reprompt.MaxPromptChars != 0 {
		base.Reprompt.MaxPromptChars = user.Reprompt.MaxPromptChars
	}
	if user.Reprompt.TailMessages != 0 {
		base.Reprompt.TailMessages = user.Reprompt.TailMessages
	}
	if len(user.Reprompt.Denylist) != 0 {
		base.Reprompt.Denylist = user.Reprompt.Denylist
	}
}

// Adapter compiles the named agent's config into a ready-to-use adapter.
func (c Config) Adapter(name string) (*adapter.Adapter, error) {
	ac, ok := c.Agents[name]
	if !ok {
		return nil, fmt.Errorf("no adapter configured for agent %q", name)
	}
	return adapter.Compile(name, adapter.Spec{
		LaunchCmd:      ac.LaunchCmd,
		ResumeCmd:      ac.ResumeCmd,
		LimitPatterns:  ac.LimitPatterns,
		IdlePattern:    ac.IdlePattern,
		InjectStyle:    ac.InjectStyle,
		TranscriptGlob: ac.TranscriptGlob,
		YoloFlag:       ac.YoloFlag,
		AutoResponses:  autoResponseSpecs(ac.AutoResponses),
	})
}

func autoResponseSpecs(in []AutoResponseConfig) []adapter.AutoResponseSpec {
	out := make([]adapter.AutoResponseSpec, 0, len(in))
	for _, ar := range in {
		out = append(out, adapter.AutoResponseSpec{Pattern: ar.Pattern, Keys: ar.Keys, Once: ar.Once})
	}
	return out
}
