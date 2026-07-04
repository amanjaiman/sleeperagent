# SleeperAgent — Unattended Answers & Status Cleanup (post-M1 field testing, round 2)

*Remediation spec for four issues found during the second hands-on test on Windows (ConPTY backend, Claude Code).*

Draft v0.1 · June 2026 · supplements [SPEC.md](SPEC.md) and [SPEC-002-first-run-fixes.md](SPEC-002-first-run-fixes.md)

---

## 0. Context

A second real-world run (Windows 11, ConPTY backend, Claude Code) surfaced four
more issues. Two are *unattended-input* gaps (the watchdog should answer prompts
the human isn't there to answer), and two are *status hygiene* gaps (`status`
shows confusing leftover state). They continue the letter scheme from SPEC-002
(which ended at F):

| # | Issue | Bucket | Status |
|---|-------|--------|--------|
| G | Claude's `/rate-limit-options` menu is not auto-selected to "1" in practice | Unattended input | Partially landed; gating still wrong |
| H | No way to auto-answer *arbitrary* agent questions with the first option | Unattended input | New feature |
| I | A session that ENDED on its own lingers as `ENDED*` in `status` | Status hygiene | New |
| J | After a successful resume, `status` shows `WAITING` again with a *next-day* reset | Status hygiene / correctness | Snapshot-clear landed; re-detect still possible |

G and J already have in-flight work in the working tree (the built-in
`auto_responses` default, and `resumeConfirmed()` clearing the stale snapshot —
see [supervisor.go](../internal/supervisor/supervisor.go)). This spec records
what remains for each so the fix is actually observable in the field.

As with SPEC-002, this is the design + acceptance contract, not the code. G, H,
and J all depend on **real captured Claude screens** (the menu text, a sample
tool-permission prompt, and a persistent reset banner) — capture those into
`internal/parser/testdata/` first and drive the regexes/tests from them.

---

## G. `/rate-limit-options` is still not auto-selected

### Symptom
When Claude shows `/rate-limit-options` and asks what to do, SleeperAgent does
**not** press `1` ("Stop and wait for the limit to reset"). The user must select
it by hand — defeating the unattended promise.

### Root cause
SPEC-002 §E added the auto-response mechanism, and the working tree now ships the
rule **on by default** for the `claude` adapter
([config.go:89-97](../internal/config/config.go),
[config.example.toml](../config.example.toml)). So the rule exists. The reason it
still doesn't fire is the **double gate** in `scanAutoResponses`
([supervisor.go:389-422](../internal/supervisor/supervisor.go)):

```go
if ar.Keys == "" || !safeStopAndWait.MatchString(capture) {
    // notify-only; do NOT press a key
}
```

A key is pressed only if **both**:
1. the auto-response `pattern` matches, **and**
2. a *separate*, hard-coded safety regex matches the capture:
   `safeStopAndWait = (?i)stop and wait for (?:the|your) limit to reset`
   ([supervisor.go:92](../internal/supervisor/supervisor.go)).

The safety gate is deliberate (we never press a key unless we can see the
verified "stop and wait" wording — see [SPEC.md §7](SPEC.md)). But if the **live
`/rate-limit-options` menu does not contain that exact phrasing**, gate 2 fails,
the code takes the notify-only branch, and no `1` is ever sent. The menu wording
is the unverified assumption here — it has never been captured from the real CLI.
The `pattern` alone matching `rate.?limit.?options` is not enough to press a key.

A second, subtler trap: the gate keys off the *menu/option text*, but the
adapter `pattern` also matches the slash-command echo `rate-limit-options`. If
the option text differs from the gate phrase (e.g. "1. Wait until your limit
resets", "1. Pause until reset"), the rule is permanently stuck in notify-only.

### Fix
1. **Capture the real menu.** Use `sleeperagent parse` / a transcript grab to
   record the exact `/rate-limit-options` screen (numbered list vs. arrow
   selector, and the precise wording of the safe option) into
   `internal/parser/testdata/claude/rate-limit-menu.txt`.
2. **Align the safety gate to the captured wording.** Broaden `safeStopAndWait`
   (or replace it with a per-auto-response `safe_when` pattern in config) so it
   matches the real option text — covering the plausible variants
   ("stop and wait", "wait …until… reset", "pause until …reset"). The gate must
   still be *specific to the safe option*, never matching a paid/upgrade/model
   choice.
3. **Verify the keystroke.** Confirm `1\r` actually selects the safe option on
   the live menu. If the menu is an **arrow-key selector** with the safe choice
   pre-highlighted, `\r` (accept default) is correct and `1` may be wrong; if the
   safe option is not the default, arrow navigation (`\x1b[B` … `\r`) is needed.
   Encode whatever is verified as the default `keys`.
4. **Make a gate failure observable.** When the pattern matches but the safety
   gate does not, the existing notify-only path fires "manual choice needed"
   ([supervisor.go:404-411](../internal/supervisor/supervisor.go)). Ensure that
   notification (and a logfile line) names *why* (gate phrase not found), so a
   future wording drift is diagnosable from the field instead of silent.

### Acceptance criteria
- A `testdata` fixture of the real `/rate-limit-options` screen exists and a test
  asserts the default rule both **matches** and **passes the safety gate** on it.
- Running Claude unattended to the menu (or replaying the fixture through the
  supervisor) results in `1` (or the verified keystroke) being sent automatically;
  the agent enters its wait without human input.
- The gate still refuses to press a key on a synthetic menu whose safe-option
  wording is absent, and emits the diagnostic notification instead.

---

## H. Optional flag to auto-answer *any* agent question with the first option

### Symptom
> "When Claude asks a question, we should add an optional flag … that
> automatically answers any Claude question with the first answer. This can
> interrupt workflows because Claude may suddenly ask something."

The §G auto-response only covers the one verified, safe rate-limit menu. During a
long unattended run Claude may surface *other* interactive prompts (tool-use
permission, a clarifying choice, a yes/no confirmation). Today those block
forever — the agent parks at the prompt and the run stalls until a human returns.

### Design — `--auto-answer-prompts` (default on, easy opt-out)
Add a run/attach flag that, **when the agent is waiting on an interactive
prompt**, selects the **first / default** option automatically:

```
--auto-answer-prompts   answer any interactive agent prompt with its first/default
                        option so the run doesn't stall while you're away
                        (default true; pass --auto-answer-prompts=false to disable)
```

This is the *general* analogue of §G's single safe rule, so it must be:

- **On by default, with a clear log line and an easy opt-out.** Unlike `--yolo`
  ([SPEC.md §7](SPEC.md), [main.go](../cmd/sleeperagent/main.go)), which bypasses
  permission prompts entirely and stays an explicit opt-in, `--auto-answer-prompts`
  only answers prompts that are already showing — it defaults to `true` because
  running an agent unattended already means accepting it can act without a human
  in the loop each cycle; without this flag, an agent that hits a question while
  the user is away just sits stuck, defeating the point of unattended auto-resume.
  Log at launch (whenever the option is enabled, which is now the common case)
  that prompts — including ones that run tool calls — will be auto-accepted, and
  that `--auto-answer-prompts=false` disables it.
- **Distinct from `--yolo`.** `--yolo` removes the prompts up front (it passes
  the agent's skip-permissions flag); `--auto-answer-prompts` leaves prompts in
  place but answers them. A user may want one, the other, or neither. (Note: with
  `--yolo`, Claude shows far fewer prompts, so the two are largely independent.)

### Mechanism
Reuse the auto-response machinery, but driven by a **generic "agent is asking"
detector** rather than a fixed phrase:

1. Add an adapter field `PromptPattern *regexp.Regexp` (config: `prompt_pattern`)
   that recognizes Claude's interactive-prompt shape — e.g. a numbered option
   list (`❯ 1.`, `1. …` / `2. …`) or a `(y/n)` confirmation. Capture the real
   shapes into `internal/parser/testdata/claude/` first; this detector is the
   risky part and must not false-positive on ordinary TUI chrome.
2. When `--auto-answer-prompts` is set **and** `PromptPattern` matches **and** the
   pane is `idle` (the prompt is settled, not mid-render —
   [supervisor.go:557-562](../internal/supervisor/supervisor.go)), inject the
   "first option" keystroke (`1\r`, or `\r` if the first option is pre-selected —
   verify against the captured menu, as in §G step 3).
3. **Dedupe and re-arm exactly like `handledAuto`** so a lingering prompt in
   scrollback isn't answered repeatedly ([supervisor.go:255-259, 389-422](../internal/supervisor/supervisor.go)).
4. **Scope the safety carve-out.** Even in this mode, *never* auto-answer a prompt
   the config marks as money/plan/model-changing if such a prompt is detectable;
   when in doubt the §G "manual choice needed" notification still fires. The flag
   buys "answer the routine stuff," not "click anything."

`--auto-answer-prompts` should be accepted by both `run` and `attach-existing`
and threaded through `watchParams` → `supervisor.Options` (a new
`AutoAnswerPrompts bool`).

### Open question
Whether "first option" is always the safe/desired answer is agent-specific. For
Claude's permission prompt the first option is typically "Yes" (proceed once) —
which is what an unattended user wants — but this must be **verified against the
live prompt**, and documented as the explicit risk of the flag. If the first
option is destructive on some prompt, that is the user's accepted trade-off for
enabling an unattended auto-accept; the warning text must say so.

### Acceptance criteria
- With `--auto-answer-prompts=false`, behavior is unchanged from the pre-flag
  world: non-rate-limit prompts are left for the human (the watchdog only
  handles the verified §G menu).
- By default (flag on), a replayed Claude permission/clarify prompt is
  answered with its first option automatically, and the run continues; the action
  is logged.
- The flag logs what it's doing on launch whenever enabled (i.e. by default,
  unless explicitly disabled).

### Note — default flipped to on
This spec originally proposed `--auto-answer-prompts` as an off-by-default,
loudly opt-in flag, matching `--yolo`'s posture. That default was later flipped
to **on** (see CHANGELOG `[Unreleased]`): without it, an agent that hits an
interactive prompt while the user is away stalls indefinitely, which defeats
the purpose of unattended auto-resume. Choosing to run an agent unattended at
all already means accepting it can act without a human in the loop; this just
extends that same acceptance to routine prompts instead of stalling on them.
`--auto-answer-prompts=false` remains the way to disable it.
- The prompt detector has positive and **negative** tests (must not fire on
  normal agent output, spinners, or the input box).

---

## I. A self-ended session lingers as `ENDED*` in `status`

### Symptom
After the user exited Claude, `status` showed:

```
NAME             AGENT   STATE    RESET                 PROMPT
divvy-ui-fixes   claude  ENDED*   19:11 (in 21h34m38s)  Limit has been reset. Continue the impl…

* supervisor process not running; shown state is the last persisted value
```

The session is over, but its record persists and is flagged `*` (process gone),
which reads like "something needs my attention" when nothing does.

### Root cause
When the agent exits on its own, the supervisor reaches `ENDED` and
`watchSession` returns — but it only **removes** the state record on a *kill*,
not on a self-end. Compare the two branches
([main.go:382-392](../cmd/sleeperagent/main.go)):

```go
if sup.SessionKilled() {
    statefile.Remove(p.instance)   // record cleaned up
    ...
}
if sup.SessionEnded() {
    log.Printf("session %q ended …")   // record LEFT on disk
    return nil
}
```

So the final `ENDED` snapshot that `OnUpdate` persisted
([main.go:321-341](../cmd/sleeperagent/main.go)) is never cleaned up. `status`
then shows it indefinitely, and because the supervisor PID is gone, `anyStale`
appends the `*` and the footnote ([main.go:744-752, 876-883](../cmd/sleeperagent/main.go)).
(The stray `RESET` countdown in the line is issue **J**, below.)

### Fix
Treat a self-end like a kill for cleanup purposes: **remove the record** once the
agent is gone and the final notification has fired. In the `SessionEnded()` branch
of `watchSession`, call `statefile.Remove(p.instance)` after logging.

Sequencing/visibility considerations:
- The `ENDED → notify` transition already fires via the last `OnUpdate`
  ([main.go:601-603](../cmd/sleeperagent/main.go)), so the user is still told the
  session ended *before* the record disappears. Removing the record after that is
  safe and is the desired "don't leave me confused" behavior.
- This mirrors what `stop`/`detach`/`rm` already do for dead records via
  `cleanupIfDead` ([main.go:771-777](../cmd/sleeperagent/main.go)) — so the manual
  and automatic paths converge on "a dead session leaves no record."
- Keep the `ENDED*` rendering + footnote in `status` for the **transient** window
  where a supervisor died *uncleanly* (crash) without going through the
  `SessionEnded` path; those genuinely are stale and the `*` is the right signal.
  `rm --all` remains the manual broom for that case.

### Acceptance criteria
- After the agent exits in a watched session (pty or tmux), a subsequent
  `sleeperagent status` does **not** list that instance.
- The "session ended" desktop/webhook notification still fires.
- A supervisor that crashes without ending cleanly still shows `ENDED*`/stale and
  is removable via `rm`.

---

## J. After resume, `status` shows `WAITING` again with a *next-day* reset

### Symptom
After SleeperAgent resumed the session once the limit reset, a later `status`
still showed `WAITING` with the **same clock time but the next day** (e.g. the
`19:11 (in 21h34m38s)` above). Expected: `RUNNING` with no reset (`—`).

### Root cause (two parts)
**Part 1 — stale snapshot (landed).** Previously, returning to `RUNNING` after a
resume did not clear the resolved `reset`/`waitUntil`, so the last persisted
record kept a `WAITING`-era countdown. The working tree fixes this:
`resumeConfirmed()` now zeroes `reset` and `waitUntil` and emits a clean
`RUNNING` snapshot ([supervisor.go:523-533](../internal/supervisor/supervisor.go)).
This is necessary but only fixes the moment of resume.

**Part 2 — spurious re-detection (still open).** The *next-day* time is the tell.
`ParseClock` resolves a bare clock time to the **next future occurrence**, rolling
forward 24h if that time already passed today
([parser.go:96-134](../internal/parser/parser.go)). The original reset (19:11)
has, by definition, just passed when we resume. So if the supervisor **re-detects
the same limit banner** in `RUNNING` — Claude keeps "…resets 7:11pm" on screen —
`Detect`→`Resolve` now rolls it to **19:11 tomorrow**, re-arming `LIMITED`→`WAITING`
with a next-day countdown. That exactly reproduces the symptom.

Why the existing dedupe can miss it: re-detection in `onRunning` is suppressed
only while the matched substring is **byte-identical** to `handledMatch` *and*
still present in the captured window ([supervisor.go:252-254, 342-362](../internal/supervisor/supervisor.go)).
That guard is fragile against a **persistent reset banner whose text mutates**
(a live "resets in 3h59m" countdown changes every poll) or one that scrolls out
of the 100-line window and is later re-rendered. Any such change yields
`match != handledMatch` → a "fresh" limit.

### Fix
Make a post-resume false re-arm impossible, with defense in depth:

1. **Keep Part 1** (`resumeConfirmed()` clearing the snapshot) — already landed.
2. **Reject a re-detected reset that resolves into the window we just waited
   through.** When a limit is detected in `RUNNING` and its resolved reset is
   ~24h out *because it rolled forward* from a time at/just-before now, treat it
   as the **already-handled** banner, not a new event. Concretely: remember the
   last satisfied reset (`lastReset`), and in `onRunning` ignore a detection whose
   resolved time equals `lastReset + 24h` (within a small tolerance) — that is the
   rolled-forward ghost of the limit we already served.
3. **Add a short post-resume cooldown.** For one or two poll intervals after
   `resumeConfirmed()`, suppress limit *re-arming* in `onRunning` (still observe,
   still scan auto-responses). This covers the mutating-countdown banner case
   without depending on exact-string dedupe.
4. **Harden dedupe normalization.** Normalize whitespace (and strip a trailing
   live-countdown fragment) before comparing to `handledMatch`, so a banner that
   only differs by its ticking countdown is recognized as the same event.

Recommendation: implement (2) + (3) — they are robust and small; (4) is a nice
belt-and-suspenders but secondary. A genuinely new limit (a real second cap hit
later in the run) resolves to a *different, future* time and is unaffected by (2),
and arrives well after the (3) cooldown, so neither guard suppresses a real event.

### Acceptance criteria
- A supervisor that resumes through a reset and keeps the limit banner on screen
  stays in `RUNNING` with `reset = —`; `status` never flips back to `WAITING`
  with a next-day time.
- A test replays "limit banner persists / countdown mutates after resume" and
  asserts the supervisor does **not** re-enter `WAITING`.
- A genuinely new limit hit *after* a resume (different, future reset) is still
  detected and waited on normally.

---

## Priority & sequencing

1. **I** — one-line cleanup, immediately removes the confusing `ENDED*` line.
   Independent; land first.
2. **J** — completes the resume-correctness fix already started in the working
   tree; pairs naturally with I (both are about `status` telling the truth).
3. **G** — finishes the unattended rate-limit promise; gated on capturing the
   real `/rate-limit-options` menu. Land with its `testdata` fixture.
4. **H** — largest new surface (new flag + generic prompt detector); depends on
   the same captured-prompt corpus as G and shares the auto-response plumbing, so
   do it after G.

## Cross-cutting test note
G, H, and J all need **real captured Claude screens**, not hand-written guesses:
the `/rate-limit-options` menu, a tool-permission / clarify prompt, and a
persistent reset banner (ideally one with a live countdown). Capture them into
`internal/parser/testdata/claude/` and drive the regexes, the safety gate, the
generic prompt detector, and the re-detection guard from those fixtures. The
existing `sleeperagent parse` command remains the capture/tuning tool.
