# Changelog

All notable changes to SleeperAgent are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- **`--auto-answer-prompts` now defaults to `true`** on both `run` and
  `attach-existing`. Previously off by default, the flag now answers detected
  interactive agent prompts (a numbered menu, a y/n prompt) with the
  first/default option unless explicitly disabled. Rationale: without it, if
  the agent asks a question while you're away, it simply sits stuck — the
  point of SleeperAgent is unattended auto-resume, and a stalled prompt
  defeats that. This is an accepted risk tradeoff: the match pattern for some
  adapters is broad enough that a real tool-call permission prompt could
  plausibly be auto-approved. Pass `--auto-answer-prompts=false` to restore
  the old (safer, but stall-prone) behavior.

### Added
- **Core loop (M1)** — launch a coding agent in a managed tmux session, detect the
  usage-limit message, parse the reset time, wait, and inject a static resume prompt.
- **Graceful detach (M2)** — `d`/`q`/`k` hotkeys, `detach`/`stop` commands, a
  per-instance state file with `status`, auto-detach when a human attaches, and
  Ctrl-C that detaches rather than kills.
- **Codex adapter (M3)** — a second agent driven by the same loop via config-driven
  patterns; relative-duration reset parsing (`in 2h30m`); `agents` command.
- **Local-LLM reprompt (M4)** — `--reprompt ollama:<model>` reads the transcript
  tail + `git diff`, asks a local model for a continuation instruction, validates it
  (length + denylist), and falls back to the static prompt on any failure.
- **Polish (M5)** — `--backend pty` no-tmux fallback (Unix), desktop + `--webhook`
  notifications, progressive re-limit backoff.
- **Operability** — `attach-existing` (watch/recover a running session),
  `--yolo` (explicit unattended opt-in), `parse` (test limit strings against
  patterns), and a `version` command.
- **Native Windows support** — a ConPTY-based `pty` backend (the default on
  Windows) runs a native Windows agent in a pseudoconsole, so SleeperAgent works on
  Windows with no WSL. Linux/macOS/Windows are all first-class.
- **Dead-session detection** — the supervisor now stops cleanly (new `ENDED`
  state + notification) when the agent exits or the tmux session is killed out
  from under it, instead of looping forever on `capture failed`. Consecutive
  capture failures are bounded as a safety net for a persistently broken backend.

### Fixed
- The watch loop no longer spins indefinitely when the supervised session
  disappears; `run` exits within a couple of poll intervals.
- **Rate-limit menu auto-handled** — the built-in `claude` adapter now auto-selects
  "1. Stop and wait for the limit to reset" when Claude's rate-limit menu appears,
  so you no longer have to choose it yourself (it still only presses a key once the
  verified stop-and-wait wording is on screen).
- **Resume prompt is actually submitted** — on the pty/ConPTY backends the resume
  prompt's Enter is now sent as a separate keypress after a short settle, so the
  agent's TUI accepts it as a submit instead of swallowing it into the paste. The
  supervisor also verifies the prompt left the input box and re-presses Enter (only)
  if it is still sitting there unsent, instead of treating any pane change as success.
- **`status` no longer shows a stale `WAITING`/reset** — the resolved reset and wait
  times are cleared once the agent resumes, so `status` reports `RUNNING` with no
  leftover countdown.

[Unreleased]: https://github.com/amanjaiman/sleeperagent/commits/main
