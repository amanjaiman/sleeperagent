# SleeperAgent — First-Run Fixes (post-M1 field testing)

*Remediation spec for the six issues found during the first hands-on test on Windows (ConPTY backend).*

Draft v0.1 · June 2026 · supplements [SPEC.md](SPEC.md)

---

## 0. Context

The first real-world test (Windows 11, ConPTY backend, Claude Code) surfaced six
problems. They fall into three buckets:

| # | Issue | Bucket |
|---|---|---|
| A | Default Claude limit patterns miss the real message ("session limit"); user had to hand-edit `config.toml` | Detection |
| B | `sleeperagent` is not on `PATH`; user had to invoke the full `.exe` path | Install / UX |
| C | Countdown logs every 30s — noise with no value | Logging |
| D | Agent TUI and supervisor logs share one terminal → corrupted UI; agent output invisible on resume | Backend / UX |
| E | Claude's `/rate-limit-options` interactive menu blocks an unattended supervisor | Detection / unattended |
| F | After killing the session the terminal floods with mouse-tracking escape sequences | Backend teardown |

D and F are the most severe (they make the ConPTY backend feel broken). A and E
are correctness gaps in the core promise (unattended resume). B and C are polish.

This document is the design + acceptance criteria for each. It does **not**
include the code; it is the contract the implementation must satisfy.

---

## A. Default Claude limit patterns are incomplete

### Symptom
Out of the box, SleeperAgent never detected the limit. The user had to add to
`%APPDATA%\sleeperagent\config.toml`:

```toml
[agents.claude]
limit_patterns = [
  "(?i)hit your session limit.*resets\\s+(?P<time>[^\\r\\n]+)",
  "(?i)limit reached.*resets\\s+(?P<time>.+)",
  "(?i)Claude AI usage limit reached\\|(?P<ts>\\d+)",
]
```

### Root cause
The built-in defaults ([config.go:72-75](../internal/config/config.go)) only
match two phrasings:

```go
`(?i)limit reached.*resets\s+(?P<time>.+)`,
`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`,
```

Claude Code's current TUI says **"hit your session limit … resets …"** (and
other variants), none of which contain the literal "limit reached". The watchdog
therefore sat in RUNNING forever.

Secondary nit: `(?P<time>.+)` is greedy to end-of-line. The user defensively
switched to `[^\r\n]+`. The pty backend already strips `\r` and splits on `\n`
([pty_common.go:10](../internal/ptybackend/pty_common.go),
[pty_windows.go:202](../internal/ptybackend/pty_windows.go)), and Go's `.`
excludes `\n`, so this is cosmetic — but trailing punctuation (`resets 3pm.`)
should be trimmed so the clock parser gets a clean token.

### Fix
1. **Broaden the default Claude `limit_patterns`** so the common variants match
   with zero config. Required coverage (capture real strings into a corpus — see
   below — before finalizing the regexes):

   - `5-hour limit reached ∙ resets 3pm`
   - `You've hit your session limit … resets <time>`
   - `Claude usage limit reached. Your limit will reset at <time>.`
   - `Weekly limit reached … resets <time>`
   - headless: `Claude AI usage limit reached|<unix-ts>`
   - relative form: `… resets in 2h30m`

   Proposed defaults (to validate against the corpus):

   ```toml
   limit_patterns = [
     "(?i)Claude AI usage limit reached\\|(?P<ts>\\d+)",
     "(?i)(?:usage|session|weekly|\\d+-?hour)\\s+limit\\s+reached.*?reset[s]?(?:\\s+at)?\\s+(?P<time>[^\\r\\n.]+)",
     "(?i)hit your\\s+(?:usage|session|weekly)?\\s*limit.*?reset[s]?(?:\\s+at)?\\s+(?P<time>[^\\r\\n.]+)",
     "(?i)limit.*?reset[s]?\\s+in\\s+(?P<dur>[^\\r\\n.]+)",
   ]
   ```

2. **Guard against the proactive "approaching limit" warning.** Claude shows a
   non-blocking banner like `Approaching usage limit · resets 5pm` *before* the
   hard stop. Patterns MUST require a hard-stop token (`reached` / `hit your …
   limit`) and MUST NOT fire on `approaching`. Add a negative test for this — a
   false positive sends the watchdog into WAITING while the agent is still able
   to work.

3. **Trim trailing punctuation** from the captured `time`/`dur` token (e.g. a
   trailing `.`) in `parser.Detect` or `Resolve` so `resets 3pm.` resolves
   cleanly. (`Detect` already `TrimSpace`s groups; extend to strip a trailing
   `.`/`,`.)

### Acceptance criteria
- A `testdata/` corpus of real, anonymized Claude limit screens (one file per
  variant) lives in `internal/parser/testdata/` and each is asserted to Detect +
  Resolve to a sane reset time.
- `sleeperagent parse --agent claude "<each variant>"` matches with **default
  config, no user edits**.
- The "approaching limit" banner does **not** match.
- Existing parser tests still pass.

---

## B. `sleeperagent` must run as a bare command

### Symptom
The user had to type `C:\Users\amanj\Documents\sleeperagent\sleeperagent.exe …`
because agents are started from the project directory, not the repo. `sleeperagent`
alone was "command not found".

### Root cause
No install path puts the binary on `PATH`. `make install` shells out to
`go install` ([Makefile:12-13](../Makefile)) — needs the Go toolchain and
`GOBIN` on `PATH`, neither of which a binary-download user (or this machine — Go
isn't on `PATH`, see project memory) has.

### Fix
Primary: add a self-installing subcommand so a downloaded binary can put itself
on `PATH` with one command.

```
sleeperagent install [--dir DIR]   copy this binary to a PATH directory
```

Behavior:
- Resolve the running executable (`os.Executable()`), copy it to a per-user
  bin dir, `chmod +x` on Unix.
- Default target dir:
  - **Windows:** `%LOCALAPPDATA%\Microsoft\WindowsApps` (already on `PATH` for
    most users) or `%LOCALAPPDATA%\Programs\sleeperagent` (then print the exact
    `setx PATH` / Settings step if not yet on `PATH`).
  - **macOS/Linux:** `~/.local/bin` (print the `export PATH` line if absent).
- Detect whether the chosen dir is already on `PATH`; if not, print the precise,
  copy-pasteable command to add it (per-OS) and a one-line "open a new shell"
  reminder. Do **not** silently rewrite shell profiles.
- Be idempotent and refuse to overwrite a *different* tool of the same name
  without `--force`.

Secondary (release hygiene, separate work item):
- `goreleaser` should publish per-OS archives + `checksums.txt`.
- Provide `scripts/install.ps1` and `scripts/install.sh` one-liners.
- Consider `scoop` / `winget` / Homebrew tap later.

Docs: README "Install" gains a "Put it on your PATH" subsection pointing at
`sleeperagent install`.

### Acceptance criteria
- On a clean machine, after downloading the binary, `./sleeperagent install`
  followed by opening a new shell makes `sleeperagent version` work from any dir.
- If the target dir isn't on `PATH`, the command prints exact remediation and
  does not claim success.

---

## C. Stop logging the countdown every 30s

### Symptom
While WAITING, SleeperAgent prints `waiting <d> until reset` every 30 seconds.
The user finds this pointless — they can run `status` on demand.

### Root cause
[supervisor.go:378-382](../internal/supervisor/supervisor.go) throttles a
countdown log to every 30s during WAITING.

### Fix
- **Remove the periodic countdown log.** The one-time "reset at … waiting <d>"
  on entry to WAITING ([supervisor.go:352](../internal/supervisor/supervisor.go))
  stays — it states the plan once.
- The live countdown remains available via `sleeperagent status` (already reads
  `WaitUntil` from the state file, [main.go:697-706](../cmd/sleeperagent/main.go)).
- Optional: a very sparse heartbeat (e.g. hourly) only under a future
  `--verbose` flag. Not on by default.
- Remove now-dead `lastCountdown` bookkeeping
  ([supervisor.go:131,361](../internal/supervisor/supervisor.go)).

### Acceptance criteria
- A WAITING supervisor emits exactly one "reset at … waiting" line on entry and
  nothing further until the reset is reached (or a state change).
- `status` still shows a live `(in <d>)` countdown.

---

## D. Agent TUI and supervisor logs collide in one terminal

### Symptom
> "Where it opens the agent is the same place it outputs its logs. It messes up
> what the UI looks like in the terminal. Also … when it resumes the agent
> session you can't see the agent output."

### Root cause
The tmux backend keeps the agent in its own session (clean separation — the
whole reason tmux is primary, [SPEC.md §2](SPEC.md)). The **pty/ConPTY backend
has no such separation**:
- `pump()` echoes the agent's full-screen TUI to `os.Stdout`
  ([pty_unix.go:56-58](../internal/ptybackend/pty_unix.go),
  [pty_windows.go:177-179](../internal/ptybackend/pty_windows.go)).
- `log.Printf` writes supervisor lines to `os.Stderr` — the **same** terminal.

A TUI that owns the screen (alt-screen, absolute cursor moves) interleaved with
scrolling log lines = corruption. Worse, during watching `hotkeys.Listen` puts
stdin in raw mode and consumes it ([main.go:312-315](../cmd/sleeperagent/main.go)),
so the user can *see* the agent but cannot *type* to it until detach.

This is inherent to a single-terminal backend; on Windows there is no tmux, so
this path must be made first-class, not treated as a degraded fallback.

### Fix — "transparent pass-through" mode for pty/ConPTY
Make the pty/ConPTY backend behave as if the user had run the agent directly,
with SleeperAgent watching silently in the background:

1. **Supervisor logs go to the per-instance logfile, not the TTY**, whenever the
   backend is echoing the agent to that same TTY. Reuse the daemon logfile path
   (`<state-dir>/<instance>.log`, [main.go:414](../cmd/sleeperagent/main.go)).
   The TTY shows **only the agent**.
2. **State transitions surface out-of-band**, not as inline log spam:
   desktop/webhook notifications (already wired,
   [main.go:522-545](../cmd/sleeperagent/main.go)) + the logfile + `status`.
   Optionally a single non-scrolling status line (terminal title bar via
   `\x1b]0;…\x07`, which does not disturb the TUI body).
3. **Forward stdin to the agent during RUNNING/WAITING**, not only after detach.
   The human can use the agent at any time. The injector and the human rarely
   race: injection only happens in RESUMING after a reset (human presumed away);
   if the human is present they would just continue it themselves.
4. **Rework hotkeys for pass-through.** Raw-stdin hotkeys conflict with handing
   stdin to the agent. On pty/ConPTY:
   - drop the stdin hotkey listener;
   - control via `sleeperagent detach` / `stop` from another shell (already works
     via the control file, [main.go:548-568](../cmd/sleeperagent/main.go));
   - forward Ctrl-C to the agent (so the user can interrupt the agent normally).
     Supervisor detach-on-Ctrl-C is a tmux-mode affordance; document that on
     pty/ConPTY you detach with the `detach` command. (Alternative: a rare escape
     prefix like `Ctrl-A d`; deferred — the `detach` command is enough for v1.)

The tmux backend is unchanged (it already separates cleanly).

This single change fixes both halves of the symptom: the TUI renders correctly
(no interleaved logs), and the agent stays fully visible across resume.

### Acceptance criteria
- Running `sleeperagent run -- claude` on Windows shows Claude's TUI **uncorrupted**
  for the entire lifecycle (running → limited → waiting → resuming → running).
- No supervisor log lines appear on the TTY in pty/ConPTY mode; they are in the
  logfile, and transitions fire notifications.
- The user can type to the agent at any time while SleeperAgent watches.
- The injected resume prompt is visible in the agent on reset.

---

## E. Handle Claude's `/rate-limit-options` interactive menu

### Symptom
On hitting the limit, Claude Code presents an interactive menu
(`/rate-limit-options`) requiring a choice; option **1 = "Stop and wait for the
limit to reset"**. Unattended, nobody selects it, so the agent parks at the menu
and never enters the wait the watchdog is counting on. Codex may have an
analogous approval prompt.

### Root cause
The supervisor only ever *reads* output and *injects at resume*. It has no
concept of answering an interactive prompt that appears at limit time.

### Fix — config-driven auto-response rules
Add an adapter capability: **auto-responses**, a list of `{pattern → keystrokes}`
the supervisor applies while RUNNING/LIMITED.

Config / `adapter.Spec` / `adapter.Adapter` gain:

```toml
[[agents.claude.auto_responses]]
pattern = "(?i)rate.?limit.?options|stop and wait for (?:the|your) limit to reset"
keys    = "1\r"          # select "stop and wait"
once    = true           # don't re-fire while the same menu lingers in scrollback
```

Supervisor behavior:
- In `onRunning` (and `onLimited`), after limit detection, scan `auto_responses`.
  On a match not already handled (dedupe exactly like `handledMatch`,
  [supervisor.go:235-237,330](../internal/supervisor/supervisor.go)), inject
  `keys` via the existing `Inject` path and log it to the logfile.
- Re-arm once the matched text scrolls out of the captured window.

**Safety (hard requirement):** auto-response is unattended input, so it MUST be
constrained:
- Only the explicitly-configured **"stop and wait"** option is ever selected.
- Never auto-select anything that spends money, upgrades a plan, switches to a
  paid/overage tier, or changes models. Those require the human.
- If the menu is matched but the safe option's mapping is uncertain, do
  **nothing** and fire a notification ("manual choice needed at the agent").
- This is governed by the same unattended-safety posture as `--yolo`
  ([SPEC.md §7](SPEC.md)); document it there.

**Unknown exact keystrokes:** the menu may be an arrow-key selector (highlighted
default) rather than a numbered list — in which case `"1\r"` may be wrong and
`"\r"` (accept default, *if* the default is "wait") or arrow navigation is
needed. The exact `keys` MUST be verified against the live menu (capture it via
the corpus tooling from §A) before shipping the default rule. Until verified,
ship the rule **commented in `config.example.toml`** with instructions, rather
than a default that might select the wrong option.

### Acceptance criteria
- With the verified rule, an unattended session that hits the `/rate-limit-options`
  menu auto-selects "stop and wait", the agent enters its wait, and normal reset
  detection + resume proceeds end-to-end.
- No auto-response ever selects a paid/model-switch option.
- An unrecognized/ambiguous menu results in a notification, not a guess.

---

## F. Terminal floods with escape sequences after a kill

### Symptom
After killing the SleeperAgent session, the shell (a *different* project's prompt
in the screenshot) is flooded with sequences like `^[[<35;30;34M`,
`^[[<32;36;38m`, etc., that never stop.

### Root cause
Those are **SGR mouse-tracking reports** (`CSI < … M/m`, DECSET 1006). Claude's
TUI enabled mouse tracking / bracketed paste / the alternate screen by emitting
DECSET sequences, which `pump()` echoed to the **real** terminal. On kill/exit,
the pty/ConPTY backend tears down the child and handles
([Close](../internal/ptybackend/pty_windows.go),
[Close](../internal/ptybackend/pty_unix.go)) but **never sends the matching
DECRST (disable) sequences** to the real terminal. The terminal stays in
mouse-reporting mode, so every mouse movement prints a report forever.

### Fix — sanitize the terminal on teardown
When the backend has been echoing to the TTY (`c.echo == true`), on **every**
exit path write a fixed "terminal restore" sequence to `os.Stdout`:

```
\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l   disable mouse tracking
\x1b[?1004l                                               disable focus reporting
\x1b[?2004l                                               disable bracketed paste
\x1b[?1049l                                               leave alternate screen
\x1b[?25h                                                 show cursor
\x1b[0m\r\n                                               reset SGR
```

Requirements:
- Centralize as `restoreTerminal()` in
  [pty_common.go](../internal/ptybackend/pty_common.go) (identical for Unix pty
  and Windows ConPTY — both echo to stdout).
- Call it from `Close()` and ensure `Close()` runs on all exit paths: normal
  exit, `detach`, `kill`/`CmdKill`, Ctrl-C/SIGTERM (the signal handler in
  [watchSession](../cmd/sleeperagent/main.go) must reach `pc.Close()` — today
  `defer pc.Close()` is in `runCmd` so it covers the signal path; verify it also
  covers the kill-from-control-file path).
- Also restore stdin termios if raw mode was set (it's deferred in `Foreground`,
  but the watching phase may also set raw via hotkeys — once §D removes the
  stdin hotkey listener on pty, this simplifies).
- Idempotent and safe to send even if the agent had not enabled those modes.

### Acceptance criteria
- After `sleeperagent stop --kill` (or Ctrl-C, or the agent exiting) in
  pty/ConPTY mode, the shell prompt is clean: moving the mouse prints nothing,
  the cursor is visible, and SGR colors are reset.
- Verified on Windows (the reported case) and on a Unix pty.

---

## Priority & sequencing

1. **F** and **D** — the ConPTY backend is unusable without them (highest user
   impact, and they share the pty teardown/echo code).
2. **A** — without it the core feature doesn't trigger on default config.
3. **E** — required for true unattended operation; gated on verifying the live
   menu keystrokes.
4. **C** — trivial, do alongside D (both touch logging).
5. **B** — independent; pairs with release packaging.

A–F are independent enough to land as separate PRs except D+F+C, which all touch
the pty backend / logging and should land together.

## Cross-cutting test note
Several fixes (A, E) depend on **real captured strings** from current Claude
Code. Build an `internal/parser/testdata/` corpus first (limit screens, the
approaching-limit banner, the `/rate-limit-options` menu) and drive the regex /
auto-response defaults + regression tests from it. The existing `sleeperagent
parse` command is the capture/tuning tool.
