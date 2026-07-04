// Package config loads SleeperAgent settings from a TOML file, layered over
// built-in defaults. The adapter patterns live here so that an agent CLI
// format change is a one-line user fix, not a new release.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/amanjaiman/sleeperagent/internal/adapter"
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
	LimitPatterns  []string             `toml:"limit_patterns"`
	IdlePattern    string               `toml:"idle_pattern"`
	PromptPattern  string               `toml:"prompt_pattern"`
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

// Config is the whole SleeperAgent configuration.
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
				LimitPatterns: []string{
					`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`,
					`(?i)limit.*?reset[s]?\s+in\s+(?P<dur>[^\r\n.]+)`,
					`(?i)(?:usage|session|weekly|\d+-?hour)\s+limit\s+reached.*?reset[s]?(?:\s+at)?\s+(?P<time>[^\r\n.]+)`,
					`(?i)hit your\s+(?:usage|session|weekly)?\s*limit.*?reset[s]?(?:\s+at)?\s+(?P<time>[^\r\n.]+)`,
				},
				PromptPattern:  `(?ims)(?:^|\n)(?:(?:[^\n]*\n){0,6}\s*(?:[>\x{276F}]\s*)?1\.\s+\S[^\n]*\n\s*2\.\s+\S[^\n]*(?:\n[^\n]*){0,8}\nEnter selection \[[0-9]+-[0-9]+\](?:, or Escape to cancel)?|y\.\s+Yes[^\n]*\nn\.\s+No[^\n]*\nEnter y/n:)`,
				InjectStyle:    adapter.InjectEscTextEnter,
				TranscriptGlob: "~/.claude/projects/*/*.jsonl",
				YoloFlag:       "--dangerously-skip-permissions",
				// Auto-answer Claude's rate-limit menu by choosing the safe
				// "1. Stop and wait for the limit to reset" option, so the user
				// doesn't have to. The supervisor still gates this on the verified
				// stop-and-wait wording before pressing any key (see scanAutoResponses).
				AutoResponses: []AutoResponseConfig{{
					Pattern: `(?i)rate.?limit.?options|stop and wait for (?:(?:the|your) )?limit to reset`,
					Keys:    "1\r",
					Once:    true,
				}},
			},
			"codex": {
				LaunchCmd: "codex",
				// Codex prints its limit banner wrapped across terminal lines, e.g.
				// "...or try again\nat 2:10 PM." The first two patterns anchor on the
				// definitive "hit your usage limit" banner (so a real cap is caught
				// without tripping on Codex merely quoting the phrase in prose), and
				// use \s+ — not a literal space — between "try again" and "at"/"in"
				// so the wrap is tolerated. The generic fallbacks below likewise use
				// \s+ to survive the same line wrap for other Codex phrasings.
				LimitPatterns: []string{
					`(?is)hit your usage limit\b.*?try again\s+at\s+(?P<time>\d{1,2}(?::\d{2})?\s*[ap]m)`,
					`(?is)hit your usage limit\b.*?try again\s+in\s+(?P<dur>[^\r\n.]+)`,
					`(?i)try again\s+at\s+(?P<time>.+)`,
					`(?i)try again\s+in\s+(?P<dur>.+)`,
					`(?i)rate limit.*reset[a-z ]*in (?P<dur>.+)`,
				},
				// Codex's structured question UI: a "Question N/M" header, an
				// arrow-navigable numbered option list with option 1 pre-highlighted
				// ("› 1. …"), and a footer ending in "enter to submit answer". We
				// anchor on the header + that footer so --auto-answer-prompts answers
				// the agent's clarifying questions (bare Enter picks the highlighted
				// first/recommended option) but deliberately does NOT match Codex's
				// command-execution approval prompts, which must not be auto-approved
				// unattended.
				PromptPattern:  `(?is)Question\s+\d+\s*/\s*\d+\b.*?\benter\s+to\s+submit\s+answer\b`,
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
		return filepath.Join(dir, "sleeperagent", "config.toml")
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
		LimitPatterns:  ac.LimitPatterns,
		IdlePattern:    ac.IdlePattern,
		PromptPattern:  ac.PromptPattern,
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
