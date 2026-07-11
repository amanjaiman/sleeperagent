# Changelog

All notable changes to SleeperAgent are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-07-10

### Added
- **Self-update** — new `sleeperagent update` command downloads the latest
  GitHub release for your OS/arch, verifies it against the release's
  `checksums.txt`, and atomically replaces the executable (on Windows the
  running exe is parked as `.old` and swept up on the next start). `--check`
  only reports. Release builds also offer this at startup, Codex-style:
  `run`/`attach-existing` from a real terminal checks for a newer release at
  most once a day and asks `Update now? [Y/n]` before the session launches.
  The check is skipped for source (`dev`) builds and non-TTY runs, never
  blocks startup on a slow or offline network (3s timeout, result cached),
  and can be disabled with `check = false` under `[update]` in config.toml or
  the `SLEEPERAGENT_NO_UPDATE_CHECK` env var. A failed or declined update
  never touches the existing binary.

### Fixed
- CI now runs the integration suite through `test/run_all.sh`, so new
  integration scripts can't silently drift out of CI (dead-session and the
  interactive-attach tests were missing from the workflow's hand-kept list).

## [0.4.0] - 2026-07-10

### Changed
- **tmux runs now start attached** — `sleeperagent run` (and `attach-existing`)
  from a real terminal puts your terminal inside the tmux session immediately
  (via a supervised `tmux attach`), so Linux/macOS now match the Windows/ConPTY
  experience: prompt and use the agent exactly as if you'd launched it
  directly, while the watchdog monitors from the same process and auto-resumes
  after a limit reset. Details:
  - The supervisor's own attach client doesn't trigger auto-detach, but a
    *second* client attaching during your view — or a manual `tmux attach`
    after you detach it — is still treated as a takeover and steps the
    watchdog aside.
  - Detaching your view (tmux prefix + `d`) keeps the watchdog running and
    returns you to its console log with the `d`/`q`/`k` hotkeys active; the
    hotkeys also engage if the initial attach fails (e.g. running from inside
    an existing tmux session skips the auto-attach entirely).
  - Stopping the watchdog while you're inside the view (`sleeperagent
    detach`/`stop`) notifies you via the tmux status line and waits for you to
    detach instead of yanking your terminal out of the session.
  - Supervisor logs go to the instance log file while the view is attached
    (`sleeperagent logs --name N`); after the view detaches, the run logs to
    its terminal again.
  - Non-TTY runs (scripts, CI) are unchanged.

### Added
- **`--detached`** flag on `run` and `attach-existing` — opt out of the new
  attach-on-start behavior and watch from the console with the `d`/`q`/`k`
  hotkeys, as before.

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

[Unreleased]: https://github.com/amanjaiman/sleeperagent/compare/v0.5.0...main
[0.5.0]: https://github.com/amanjaiman/sleeperagent/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/amanjaiman/sleeperagent/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/amanjaiman/sleeperagent/compare/v0.2.0...v0.3.0
