// Package adapter holds the cross-agent abstraction: the data and hooks that
// let the core supervisor loop drive any CLI coding agent (Claude Code, Codex,
// ...) without agent-specific branching in the core.
package adapter

import (
	"fmt"
	"regexp"
)

// InjectStyle describes how keystrokes are delivered to the agent's prompt.
const (
	// InjectEscTextEnter sends Esc, then the literal text, then Enter. Claude
	// Code's TUI wants an Esc first to ensure the input box has focus.
	InjectEscTextEnter = "esc-text-enter"
	// InjectTextEnter sends the literal text, then Enter.
	InjectTextEnter = "text-enter"
	// InjectKeys sends the configured keystrokes exactly as provided. It is used
	// for prompt/menu auto-responses where the config owns the Enter key.
	InjectKeys = "keys"
)

// Adapter describes how to drive a particular coding agent: how to launch it,
// how to recognize its usage-limit message, when it is idle, and how to inject
// a resume prompt. Adapters are plain data so adding an agent is a config/data
// change, not a core change.
type Adapter struct {
	Name           string
	LaunchCmd      string
	ResumeCmd      string
	LimitPatterns  []*regexp.Regexp
	IdlePattern    *regexp.Regexp // may be nil; nil => fall back to stability heuristic
	InjectStyle    string
	TranscriptGlob string
	// YoloFlag is the agent's flag that skips permission prompts / enables
	// full-auto. Appended to the launch command only with the explicit --yolo
	// opt-in. Empty means the agent has no such flag configured.
	YoloFlag string
	// AutoResponses are safe, explicitly configured prompt/menu responses the
	// supervisor may send while watching.
	AutoResponses []AutoResponse
}

// Spec is the raw, un-compiled form of an adapter as it appears in config.
type Spec struct {
	LaunchCmd      string
	ResumeCmd      string
	LimitPatterns  []string
	IdlePattern    string
	InjectStyle    string
	TranscriptGlob string
	YoloFlag       string
	AutoResponses  []AutoResponseSpec
}

// AutoResponse is a compiled rule that injects keystrokes when a safe prompt or
// menu appears in the agent output.
type AutoResponse struct {
	Pattern *regexp.Regexp
	Keys    string
	Once    bool
}

// AutoResponseSpec is the raw config form of AutoResponse.
type AutoResponseSpec struct {
	Pattern string
	Keys    string
	Once    bool
}

// Compile turns a raw Spec into a ready-to-use Adapter, compiling every regex.
// It fails loudly on a bad pattern rather than silently dropping it, because a
// missing limit pattern means the watchdog would sleep forever.
func Compile(name string, s Spec) (*Adapter, error) {
	a := &Adapter{
		Name:           name,
		LaunchCmd:      s.LaunchCmd,
		ResumeCmd:      s.ResumeCmd,
		InjectStyle:    s.InjectStyle,
		TranscriptGlob: s.TranscriptGlob,
		YoloFlag:       s.YoloFlag,
	}
	if a.InjectStyle == "" {
		a.InjectStyle = InjectTextEnter
	}
	if len(s.LimitPatterns) == 0 {
		return nil, fmt.Errorf("adapter %q: at least one limit_pattern is required", name)
	}
	for i, p := range s.LimitPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: limit_pattern[%d] %q: %w", name, i, p, err)
		}
		a.LimitPatterns = append(a.LimitPatterns, re)
	}
	if s.IdlePattern != "" {
		re, err := regexp.Compile(s.IdlePattern)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: idle_pattern %q: %w", name, s.IdlePattern, err)
		}
		a.IdlePattern = re
	}
	for i, ar := range s.AutoResponses {
		if ar.Pattern == "" {
			return nil, fmt.Errorf("adapter %q: auto_responses[%d]: pattern is required", name, i)
		}
		re, err := regexp.Compile(ar.Pattern)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: auto_responses[%d].pattern %q: %w", name, i, ar.Pattern, err)
		}
		a.AutoResponses = append(a.AutoResponses, AutoResponse{Pattern: re, Keys: ar.Keys, Once: ar.Once})
	}
	return a, nil
}
