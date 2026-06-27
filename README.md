# AgentKeeper

[![CI](https://github.com/amanjaiman/agentkeeper/actions/workflows/ci.yml/badge.svg)](https://github.com/amanjaiman/agentkeeper/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/amanjaiman/agentkeeper.svg)](https://pkg.go.dev/github.com/amanjaiman/agentkeeper)
[![Go Report Card](https://goreportcard.com/badge/github.com/amanjaiman/agentkeeper)](https://goreportcard.com/report/github.com/amanjaiman/agentkeeper)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**A cross-agent watchdog that resumes Claude Code / Codex sessions when their usage limits reset — and gets out of your way the moment you want to take over.**

When a coding agent hits a 5-hour or weekly usage limit it hard-stops until you manually type "continue." If that reset lands while you're asleep, the task sits dead for hours. AgentKeeper runs the agent in a session it can watch, detects the limit, waits for the reset, and re-prompts it automatically — then hands the live session back the moment you show up.

```text
12:58:03 watching. take over any time with: tmux attach -t feature-x
12:58:31 usage limit detected
12:58:31 reset at Sat 17:00 EDT (source=clock, confidence=high); waiting 4h2m29s
17:01:00 reset reached; resuming
17:01:01 injected resume prompt (attempt 1): "Usage limit reset. Continue with the prior task."
17:01:03 resume confirmed; back to running
```

- **Cross-agent** — Claude Code and Codex out of the box; any CLI agent via config-driven patterns. It only waits for *legitimate* resets — no quota circumvention.
- **Cross-platform** — native on **Linux, macOS, and Windows** (no WSL required).
- **Graceful handoff** — hotkeys, `detach`/`stop`, auto-detach when you attach, Ctrl-C that detaches rather than kills.
- **Local-LLM reprompt** *(optional)* — a local Ollama model writes a context-aware continuation instruction from the transcript + git diff, validated before use. Falls back to a static prompt on any doubt.
- **Operable & safe** — background `--daemon` mode, `status`, desktop + webhook notifications, a `parse` command to tune patterns, and unattended tool-calls **off by default**.

See [docs/SPEC.md](docs/SPEC.md) for the full design rationale.

---

## Install

**Prebuilt binary** — download for your OS/arch from the [Releases](https://github.com/amanjaiman/agentkeeper/releases) page.

Put it on your `PATH`:

```bash
./agentkeeper install
```

If the install directory is not already on `PATH`, the command prints the exact `setx PATH` or `export PATH` line to run, plus a reminder to open a new shell.

**With the Go toolchain:**

```bash
go install github.com/amanjaiman/agentkeeper/cmd/agentkeeper@latest
```

**From source** (requires Go 1.23+):

```bash
git clone https://github.com/amanjaiman/agentkeeper && cd agentkeeper
make build      # -> ./agentkeeper (version stamped from git)
make check      # gofmt + go vet + unit tests
```

### Platform support

AgentKeeper needs a "session backend" — something it can run the agent inside, read output from, and type into.

| OS | Default backend | Setup | Handoff |
|---|---|---|---|
| **Linux / macOS** | `tmux` | install `tmux` (`apt install tmux` / `brew install tmux`) | **full** — agent survives detach; `tmux attach` to take over |
| **Linux / macOS** | `pty` (`--backend pty`) | none | reduced — agent is bound to the supervisor |
| **Windows** | `pty` (ConPTY) | none (Windows 10 1809+) | reduced — agent is bound to the supervisor |

Optional extras: a local [Ollama](https://ollama.com) for `--reprompt`; `notify-send` (Linux) / `osascript` (macOS) for desktop notifications (Windows uses a toast).

---

## Quick start

```bash
# Launch claude, watch it, and auto-resume when the limit resets
agentkeeper run --agent claude --name mytask -- claude
```

Everything after `--` is the command to launch (it overrides the adapter default, so you can pass your own flags). Press `d` to detach, or just leave it running. Run `agentkeeper` with no arguments for the built-in help.

---

## Commands

| Command | Description |
|---|---|
| `run [flags] -- <cmd…>` | Launch an agent and watch it. The main mode. |
| `attach-existing --target T [flags]` | Watch an agent **already running** in a tmux session (also the crash-recovery path). |
| `status [--name N]` | Report each instance's state, reset countdown, and prompt preview. |
| `detach --name N` | Stop watching; keep the session (tmux) running. |
| `stop --name N [--kill]` | Stop watching; `--kill` also terminates the session. |
| `agents [--config P]` | List configured adapters and validate that their patterns compile. |
| `parse --agent A "text…"` | Test a captured limit string against an agent's patterns and show the resolved reset. |
| `install [--dir DIR] [--force]` | Copy this binary to a PATH directory. |
| `version` | Print the build version. |

### `run` flags

| Flag | Description |
|---|---|
| `--agent` | Adapter to use: `claude` (default) or `codex`. |
| `--name` | Instance / tmux session name (default `agentkeeper-<agent>`). |
| `--prompt` | Static resume prompt to inject on reset. |
| `--reprompt` | Local-LLM reprompt, e.g. `ollama:llama3.1` (falls back to static). |
| `--backend` | `tmux` (default on Unix) or `pty` (default on Windows). |
| `--daemon` | Run in the background; control via `status`/`detach`/`stop`. |
| `--watch-only` | Notify at the reset but **do not** auto-inject — you resume by hand. |
| `--yolo` | Append the agent's skip-permissions flag (**DANGEROUS** — unattended, no prompts). |
| `--webhook` | POST notifications to this URL as JSON. |
| `--no-auto-detach` | Don't auto-detach when you attach to the session. |
| `--no-notify` | Disable desktop notifications. |
| `--config` | Path to `config.toml` (default: OS config dir). |

### Examples

```bash
# Codex with a custom static resume prompt
agentkeeper run --agent codex --prompt "Continue; run the tests after." -- codex

# Let a local model write the continuation instruction each reset
agentkeeper run --agent claude --reprompt ollama:llama3.1 -- claude

# Run in the background and check on it later (works on all platforms)
agentkeeper run --agent claude --name nightly --daemon -- claude
agentkeeper status

# Just nudge me at the reset — don't auto-resume
agentkeeper run --agent claude --watch-only -- claude

# Watch an agent you started yourself in tmux
agentkeeper attach-existing --agent claude --target mywork:0.1

# Validate a limit pattern against text you copied from your real CLI
agentkeeper parse --agent claude "5-hour limit reached ∙ resets 2pm"
```

---

## Taking over

AgentKeeper is built to get out of your way. How handoff works depends on the backend:

**tmux backend (Linux/macOS):** the agent lives in a tmux session that **outlives the supervisor**, so nothing is lost when you take over.

- **Hotkeys** (foreground run): `d`/`q` detach, `k` kills the session (with a `y` confirm).
- **`agentkeeper detach --name X`** from any other shell.
- **Ctrl-C** detaches — it never kills the session.
- **Auto-detach:** the moment you `tmux attach`, AgentKeeper notices and steps aside so you don't both type (disable with `--no-auto-detach`).
- Reattach anytime with `tmux attach -t <name>`.

**pty / ConPTY backend (default on Windows, optional on Unix):** the agent is a child of the supervisor, so it **can't be handed back interactively**. In the foreground, `detach` gives the terminal back to you until the agent exits; in `--daemon` mode, `detach`/`stop` ends the agent. Use the tmux backend if you need full handoff.

---

## Configuration

Built-in defaults already cover Claude Code and Codex. To override timings or the limit patterns (e.g. after an agent CLI changes its wording), copy [`config.example.toml`](config.example.toml) to your OS config dir — no reinstall needed:

| OS | Config file | State / logs |
|---|---|---|
| Linux | `~/.config/agentkeeper/config.toml` | `~/.local/state/agentkeeper/` |
| macOS | `~/Library/Application Support/agentkeeper/config.toml` | `~/.local/state/agentkeeper/` |
| Windows | `%AppData%\agentkeeper\config.toml` | `%AppData%\agentkeeper\state\` |

`agentkeeper status` reads the per-instance state file, so it works from any shell; a `*` next to a state means the supervisor process is no longer running.

**Limit patterns** are Go regexes with a named group for the reset time, resolved most-reliable-first:

- `(?P<ts>…)` — an explicit unix timestamp (most reliable)
- `(?P<time>…)` — a clock time (`2pm`, `6:34 AM`)
- `(?P<dur>…)` — a relative duration (`in 2h30m`, `in 45 minutes`)

If none parse, AgentKeeper assumes a 5-hour window and flags it low-confidence. Use `agentkeeper parse` to check a pattern against real output, and `agentkeeper agents` to validate your config. Adding a new agent is usually just a new `[agents.<name>]` block — no code.

---

## Local-LLM reprompt *(optional)*

By default AgentKeeper injects a fixed string on reset. With `--reprompt ollama:<model>` it instead asks a **local** model to write the next instruction: it reads the tail of the agent's transcript plus `git diff --stat` / `git log` in the agent's cwd, sends a fixed meta-prompt to Ollama, and **validates** the reply (non-empty, under `max_prompt_chars`, clears the denylist) before injecting.

It's purely additive and safe-by-construction: if Ollama is unreachable, the output is empty/over-long/denylisted, or there's no context, it **falls back to the static prompt** so the session still resumes. Everything stays local — no transcript leaves your machine. Tune it under `[reprompt]` in the config (`model`, `base_url`, `max_prompt_chars`, `tail_messages`, `denylist`); `base_url` also honors `$OLLAMA_HOST`.

## Notifications

Desktop notifications are on by default (best effort; `--no-notify` to disable) and fire when a limit is hit, when the agent resumes, and on detach. Add `--webhook <url>` to also POST each event as JSON (`{title, body, time}`).

---

## Safety

AgentKeeper **waits for legitimate resets**; it does not bypass limits.

Resuming unattended runs tool calls with no human in the loop, so by default the agent keeps its **normal permission prompts** — AgentKeeper does *not* pass `--dangerously-skip-permissions` / full-auto for you. That's an explicit, loud opt-in via `--yolo`; use it only when you understand the risk. Prefer to stay in the loop? `--watch-only` notifies you at the reset and lets you resume by hand. LLM-generated prompts are length-capped and denylist-checked before injection.

## How it works

The reset time isn't exposed in any file or API, so detection is **output-scraping** with config-driven regexes that fail loud when formats change. The supervisor only ever reads the agent's pane and writes to it — it never owns the terminal — which is what makes clean handoff possible.

```text
RUNNING --limit detected--> LIMITED --reset parsed--> WAITING
   ^                                                     |
   |                                              reset reached
   +----------- resume confirmed -- RESUMING <-----------+
```

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). Adding an agent is usually just a config block; reporting a limit-string that stopped matching helps keep the built-in defaults current. The codebase is a small set of `internal/` packages behind the CLI in `cmd/agentkeeper`; run `make check` before a PR.

## License

[MIT](LICENSE) © Aman Jaiman
