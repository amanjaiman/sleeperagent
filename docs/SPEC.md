# AgentKeeper — Technical Specification

*A cross-agent watchdog that resumes Claude Code / Codex sessions when usage limits reset — and gets out of your way the moment you want to take over.*

Draft v0.1 · June 2026

---

## 1. Problem & goals

Coding agents (Claude Code, Codex CLI) hard-stop when a 5-hour or weekly usage limit is hit. If the reset falls while you're asleep, the task sits dead until you manually type "continue." Existing tools ([terryso/claude-auto-resume](https://github.com/terryso/claude-auto-resume), [henryaj/autoclaude](https://github.com/henryaj/autoclaude)) solve this for a single agent but each has gaps: single-agent only, tmux-only, or no way to hand the live session back to a human cleanly.

**AgentKeeper** is a supervisor process that:

1. Runs the agent inside a session it can observe and inject input into (tmux pane or PTY).
2. Detects the usage-limit message and parses the reset time.
3. Sleeps until reset (+ buffer), then re-prompts the agent to continue.
4. Optionally uses a **local LLM (Ollama)** to generate a context-aware continuation prompt instead of a static "continue."
5. Lets the user **gracefully detach** — stop the watchdog without killing the agent session — so an attentive user can take over the moment the limit resets.

### Non-goals (v1)

- Not a proxy/API key sharer or quota circumvention tool. It waits for legitimate resets; it does not bypass limits.
- Not a full agent orchestrator. One supervised session per process instance (multiple instances may run).
- No GUI/desktop-app automation. It drives the **CLI agent in a terminal** only (the desktop apps expose no injection point).

---

## 2. Why this architecture

Two facts from research drive the design:

- **You cannot inject into a desktop-app agent session.** Only a terminal-hosted CLI agent can be observed and fed keystrokes. So the agent must run in a PTY/tmux session the supervisor controls.
- **The reset timestamp is not in any readable file or API.** The authoritative `anthropic-ratelimit-unified-*-reset` headers are held in memory only (open issues anthropics/claude-code #27915, #50518). The only practical signals are the strings the agent prints. So detection is **output-scraping**, and the parser must be config-driven and fail loud when formats change.

The graceful-handoff requirement pushes us toward **tmux as the primary backend**: the agent lives in a tmux session that outlives the supervisor, so "stop listening" is just the supervisor detaching — the user runs `tmux attach` and keeps the exact conversation. A self-managed PTY mode is offered as a fallback for users without tmux, with reduced handoff capability.

---

## 3. Architecture overview

```
┌──────────────────────────────────────────────────────────┐
│                      AgentKeeper (supervisor)             │
│                                                           │
│  ┌────────────┐   ┌──────────────┐   ┌─────────────────┐  │
│  │  Watcher   │──▶│ State Machine│──▶│  Injector       │  │
│  │ (poll pane │   │ RUNNING /    │   │ (send-keys or   │  │
│  │  output)   │   │ LIMITED /    │   │  pty write)     │  │
│  └────────────┘   │ WAITING /    │   └─────────────────┘  │
│        │          │ RESUMING /   │           ▲            │
│        ▼          │ DETACHED)    │           │            │
│  ┌────────────┐   └──────────────┘   ┌─────────────────┐  │
│  │ LimitParser│                      │ PromptBuilder   │  │
│  │ (regex +   │                      │ (static | LLM)  │  │
│  │  ts parse) │                      └─────────────────┘  │
│  └────────────┘                              │            │
│        │                                     ▼            │
│        │                              ┌─────────────────┐ │
│        │                              │ Ollama client   │ │
│        │                              │ (optional)      │ │
│        ▼                              └─────────────────┘ │
│  ┌──────────────────────────────────────────────────┐    │
│  │ Agent Adapter (Claude Code | Codex)               │    │
│  │  - launch cmd   - limit regexes   - idle detect   │    │
│  │  - resume cmd   - transcript path - inject style  │    │
│  └──────────────────────────────────────────────────┘    │
└───────────────────────────┬──────────────────────────────┘
                            │  observes / injects
                            ▼
                 ┌─────────────────────┐
                 │  tmux session       │   ◀── user can `tmux attach`
                 │  └ claude / codex   │       at any time
                 └─────────────────────┘
```

The supervisor never owns the agent's terminal exclusively. The agent runs in tmux; the supervisor reads with `tmux capture-pane` and writes with `tmux send-keys`. This decoupling is what makes graceful handoff trivial.

---

## 4. Component detail

### 4.1 Agent Adapter (the cross-agent abstraction)

A small interface so the core loop is agent-agnostic. Adapters are data + a few hooks, defined in config and code:

| Field | Claude Code | Codex CLI |
|---|---|---|
| `launch_cmd` | `claude` | `codex` |
| `resume_cmd` | `claude -c` (continue last) or re-inject into live pane | `codex resume` or live pane inject |
| `limit_patterns` | `limit reached ∙ resets (?P<time>.+)` (TUI); `Claude AI usage limit reached\|(?P<ts>\d+)` (headless) | `try again at (?P<time>.+)`; rate-limit banner |
| `reset_parse` | unix ts (headless) or local clock time like `2pm` (TUI) | clock time like `6:34 AM` |
| `idle_pattern` | prompt box / `>` ready indicator, no spinner | input prompt ready |
| `transcript_glob` | `~/.claude/projects/<cwd-hash>/<session>.jsonl` | `~/.codex/sessions/**/*.jsonl` |
| `inject_style` | `Esc` → text → `Enter` | text → `Enter` |

Adding a third agent later = one new adapter entry, no core changes.

### 4.2 Watcher

Polls pane content every `poll_interval` (default 3s, matching autoclaude). Captures the last N lines via `tmux capture-pane -p -t <pane>`. Diffs against the previous capture to avoid reprocessing. Feeds new content to the LimitParser and idle detector. Polling (not streaming) is deliberate — it's robust to redraws and works identically across tmux/PTY.

### 4.3 LimitParser & reset resolution

Two-step:

1. **Detect** — match any `limit_patterns` for the active adapter.
2. **Resolve reset time** — three sources in priority order:
   - **Explicit unix timestamp** if present (Claude headless `…|<ts>`). Most reliable.
   - **Clock time** (`2pm`, `6:34 AM`) → resolve to the next future occurrence in the user's local tz; if the parsed time is earlier than now, assume tomorrow.
   - **Fallback heuristic** — if nothing parseable, assume a 5-hour window from detection time, flag low-confidence, and surface a warning.

Add a configurable `reset_buffer` (default 60s) added to every reset time to avoid racing the server clock. Cap any single wait at `max_wait` (default 24h) as a safety stop, since a drained **weekly** cap can put the reset days out — in that case notify and optionally exit rather than sleep silently for days.

### 4.4 State machine

```
        ┌─────────┐  limit detected   ┌──────────┐
        │ RUNNING │──────────────────▶│ LIMITED  │
        └─────────┘                   └────┬─────┘
             ▲                             │ reset parsed
             │ idle + resume sent          ▼
        ┌────┴─────┐   reset time reached ┌──────────┐
        │ RESUMING │◀─────────────────────│ WAITING  │
        └──────────┘                      └────┬─────┘
                                               │ user detach
        any state ──user detach──▶ DETACHED ───┘ (supervisor idle,
                                                  agent untouched)
```

- **RUNNING** → just observe.
- **LIMITED** → parse reset, transition to WAITING. If unparseable after retries, notify + DETACHED.
- **WAITING** → sleep until `reset + buffer`, show countdown. Watch for user activity (see §4.6) → DETACHED.
- **RESUMING** → confirm agent pane is idle/ready, build prompt, inject once, verify it took (output changed), go RUNNING. Guard against double-injection with a one-shot latch per limit event.
- **DETACHED** → supervisor passive; agent session fully owned by the user.

### 4.5 PromptBuilder (static + optional local LLM)

**Static mode (default):** inject a configurable string (`"Usage limit reset. Continue with the prior task."` or user-supplied).

**LLM mode (`--reprompt ollama:<model>`):** at resume time, build context and ask a local model to write the next instruction:

1. Read the tail of the active session transcript (`transcript_glob`, last K messages).
2. Optionally run `git diff --stat` / `git log --oneline -n 5` in the agent's cwd to see what landed.
3. Send a fixed meta-prompt to Ollama: *"Here is the recent session and the diff of work completed. Summarize what's done and write a single, concrete next instruction to continue the task. Do not introduce new scope. Output only the instruction."*
4. Validate the response: non-empty, under `max_prompt_chars`, passes a denylist (no `rm -rf`, no force-push, no scope-expansion phrases). On any failure → fall back to the static prompt.

LLM mode is purely additive: if Ollama is unreachable or low-confidence, the tool still resumes with the static prompt. This is the main differentiator vs. existing tools.

### 4.6 Graceful detach / handoff (key requirement)

Three ways to detach, all of which **leave the agent session running**:

1. **Hotkey** in the supervisor's status view: `d` = detach (stop listening, keep session), `q` = quit supervisor *and* leave session, `k` = kill session too (with confirm).
2. **Command:** `agentkeeper detach` (or `stop --keep-session`) signals a running supervisor over its control socket / pidfile to enter DETACHED and exit.
3. **Auto-detach on user activity:** while WAITING, if the watcher sees the user has attached to the tmux session and typed (pane input changed from a human, not the injector), AgentKeeper assumes the user is taking over and auto-detaches with a printed note. This directly serves the "I'll be at my desk when it resets" case.

Because the agent lives in tmux, after any detach the user just runs `tmux attach -t agentkeeper/<name>` (printed on detach) and continues the exact conversation — nothing is lost. On SIGINT (Ctrl-C), default behavior is **detach, not kill**, so an accidental Ctrl-C never destroys the session; killing requires explicit `k`/`--kill`.

### 4.7 State persistence

A small state file (`~/.local/state/agentkeeper/<name>.json`) holds: agent type, tmux target, current state, parsed reset time, injection latch, and prompt config. Lets `agentkeeper status` report from any shell and lets a crashed supervisor recover the pending wait on restart.

---

## 5. CLI / UX

```bash
# Start: launch claude inside a managed tmux session and watch it
agentkeeper run --agent claude --name feature-x -- claude

# Codex with a custom static resume prompt
agentkeeper run --agent codex --prompt "Continue; run the tests after." -- codex

# Use a local model to regenerate the continuation prompt each resume
agentkeeper run --agent claude --reprompt ollama:llama3.1 -- claude

# Attach to an already-running agent in tmux pane (don't launch)
agentkeeper attach-existing --agent claude --target mywork:0.1

# Inspect / control
agentkeeper status            # state, reset countdown, which prompt mode
agentkeeper detach            # stop listening, keep the session
agentkeeper stop --kill       # stop listening and kill the session
```

Live status view (when run in foreground) shows: agent, state, reset countdown, next prompt preview, and the hotkey legend. Mirrors autoclaude's countdown UX but adds the prompt preview and detach affordances.

### Config file (`~/.config/agentkeeper/config.toml`)

User-editable adapter patterns so a CLI format change is a one-line fix, not a release:

```toml
poll_interval = "3s"
reset_buffer  = "60s"
max_wait      = "24h"

[agents.claude]
launch_cmd      = "claude"
resume_cmd      = "claude -c"
limit_patterns  = [
  "limit reached . resets (?P<time>.+)",
  "Claude AI usage limit reached\\|(?P<ts>\\d+)",
]
inject_style    = "esc-text-enter"
transcript_glob = "~/.claude/projects/*/*.jsonl"

[agents.codex]
launch_cmd     = "codex"
resume_cmd     = "codex resume"
limit_patterns = ["try again at (?P<time>.+)"]
inject_style   = "text-enter"

[reprompt]
provider         = "ollama"
model            = "llama3.1"
max_prompt_chars = 600
denylist         = ["rm -rf", "force push", "--force", "drop table"]
```

---

## 6. Edge cases & failure modes

- **Format drift.** The limit/idle strings are undocumented and break on agent updates. Mitigation: patterns in config; on a limit-like-but-unparseable screen, notify + DETACHED rather than guess.
- **Weekly cap, not 5-hour.** Reset may be days away. Detect via `max_wait` cap → notify and ask (or exit) instead of silently sleeping for days.
- **Double-injection.** One-shot latch per limit event; verify pane output changed after injecting before clearing the latch.
- **Injecting while not idle.** Only inject in RESUMING after the `idle_pattern` confirms the agent is waiting for input; otherwise keystrokes land mid-stream.
- **Clock/timezone skew.** Always add `reset_buffer`; parse clock times against local tz; if reset parses to the past, roll forward.
- **Resume still limited.** If the first post-reset prompt re-triggers the limit string, treat as a fresh LIMITED event and re-wait (with backoff, max 3 cycles before notifying).
- **User attached mid-wait.** Auto-detach (§4.6) so the supervisor and human don't both type.
- **Multiple sessions.** Each `agentkeeper run` is its own named instance/state file; no shared global lock.

---

## 7. Security & safety

- **Unattended execution is the real risk.** Resuming overnight means tool calls run with no human in the loop. Do **not** silently enable `--dangerously-skip-permissions` / Codex full-auto — make it an explicit, loud opt-in flag (`--yolo`) with a printed warning, and default to the agent's normal permission prompts.
- **Auto-responses must be conservative.** Menu/prompt auto-responses are config-driven and may only answer verified safe choices such as Claude's "stop and wait for the limit to reset" option. They must not select paid overage, plan upgrade, model-switch, or permission-bypass choices; ambiguous menus require human action.
- **LLM-generated prompts** pass the denylist + length cap before injection; on any doubt, fall back to static.
- **Scope guidance.** Encourage specific resume prompts ("continue implementing X in file Y") over open-ended "do everything."
- **No quota circumvention.** The tool only waits for legitimate resets; document this clearly to avoid ToS gray areas.
- **Local-only by default.** Ollama runs locally; no transcript leaves the machine.

---

## 8. Tech stack recommendation

- **Language: Go.** Single static binary, easy `brew`/`go install` distribution, strong tmux/PTY ecosystem, and it matches autoclaude so prior art transfers. (Python with `pexpect`/`ptyprocess` is a faster prototype path if you'd rather validate the loop first — build a throwaway Python spike, then port.)
- **Backends:** tmux (primary, enables clean handoff) via `tmux capture-pane`/`send-keys`; raw PTY (`creack/pty`) as a no-tmux fallback with reduced handoff.
- **TUI:** Bubble Tea (as autoclaude uses) for the status view.
- **LLM:** Ollama HTTP API (`/api/generate`), provider-pluggable so LM Studio / others can slot in.

---

## 9. Milestones

1. **M1 — Core loop, Claude Code, tmux.** Launch in tmux, detect limit, parse reset, wait, inject static "continue." Reach parity with claude-auto-resume.
2. **M2 — Graceful detach.** Hotkeys, `detach` command, SIGINT-detaches-not-kills, auto-detach on user activity, state file + `status`.
3. **M3 — Codex adapter.** Generalize parser/inject via the adapter interface; ship config-driven patterns.
4. **M4 — Local-LLM reprompt.** Transcript tail + git diff → Ollama → validated prompt, with static fallback.
5. **M5 — Polish.** PTY fallback, notifications (desktop + optional webhook), backoff on re-limit, docs.

---

## 10. Open questions

- Foreground status view vs. background daemon + `status` polling as the default run mode?
- For LLM reprompt, is transcript-tail enough context, or worth summarizing the whole session into a rolling "task memory" file?
- Should auto-detach-on-user-activity be default-on or opt-in?
- Worth a `--watch-only` mode that just notifies you at reset (no auto-inject) for users who want a nudge, not automation?
