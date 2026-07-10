# Changelog

All notable changes to SleeperAgent are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- **tmux runs now start attached** — `sleeperagent run` from a real terminal
  puts your terminal inside the tmux session immediately (via a supervised
  `tmux attach`), so Linux/macOS now match the Windows/ConPTY experience:
  prompt and use the agent exactly as if you'd launched it directly, while the
  watchdog monitors from the same process and auto-resumes after a limit
  reset. The supervisor's own attach client no longer triggers
  auto-detach-on-attach; detaching your view (tmux prefix + `d`) keeps the
  watchdog running and returns you to its console log, and a *subsequent*
  manual `tmux attach` still auto-detaches the watchdog as before. Supervisor
  logs are written to the instance log file while the view is attached
  (`sleeperagent logs --name N`). Non-TTY runs (scripts, CI) are unchanged.

### Added
- **`--detached`** flag on `run` — opt out of the new attach-on-start behavior
  and watch from the console with the `d`/`q`/`k` hotkeys, as before.

## [0.3.0] - 2026-07-04

This release also folds in everything previously listed under
`[Unreleased]` that had never been version-stamped alongside a tagged
release (M1-M5, Codex support, native Windows support, etc.) — see
`### Added`/`### Fixed` below.

### Removed
- **`--daemon`** — re-exec/background-detach mode. On the pty backend (default
  on Windows) it never provided real crash-safety (the agent was still bound
  to the supervisor either way), and on tmux the same outcome is available by
  just leaving the terminal open and checking `status` from another shell.
- **`--watch-only`** — notify-at-reset-without-injecting mode. It undercut the
  core auto-resume pitch and had no test coverage.
- **`--no-auto-detach`** — opt-out of auto-detach-on-attach. Auto-detach is now
  always on; no known use case relied on disabling it.
- **`resume_cmd` config field** — vestigial from an earlier design; the actual
  resume mechanism is prompt-injection, not this field, which was never read
  except by a debug printout.

These are breaking changes for anyone passing the removed flags or setting
`resume_cmd` in `config.toml` — remove them from scripts/configs before
upgrading.

### Changed
- **`--auto-answer-prompts` now defaults to `true`** on both `run` and
  `attach-existing`. Previously off by default, the flag now answers detected
  interactive agent prompts (a numbered menu, a y/n prompt) with the
  first/default option unless explicitly disabled. Rationale: without it, if
  the agent asks a question while you're away, it simply sits stuck — the
  point of SleeperAgent is unattended auto-resume, and a stalled prompt
  defeats that. Running an agent unattended at all already means accepting
  that it may take actions without a human in the loop each time; this just
  extends that same acceptance to routine prompts instead of stalling on
  them. Pass `--auto-answer-prompts=false` to restore the old (stall-prone)
  behavior.

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

[Unreleased]: https://github.com/amanjaiman/sleeperagent/compare/v0.3.0...main
[0.3.0]: https://github.com/amanjaiman/sleeperagent/compare/v0.2.0...v0.3.0
