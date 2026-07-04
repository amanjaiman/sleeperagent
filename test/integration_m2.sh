#!/usr/bin/env bash
# M2 end-to-end against REAL tmux: status reporting, `detach` (session must
# survive), and `stop --kill` (session must die). Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
N1="ak-m2a-$$"
N2="ak-m2b-$$"
fail=0

cleanup() {
  tmux kill-session -t "$N1" 2>/dev/null
  tmux kill-session -t "$N2" 2>/dev/null
  rm -rf "$SLEEPERAGENT_STATE_DIR"
}
trap cleanup EXIT

check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

# stopped: true if PID is not a live (running/sleeping) process. Reaps zombies so
# `kill -0` on an already-exited backgrounded child doesn't give a false positive.
stopped() { wait "$1" 2>/dev/null; ! kill -0 "$1" 2>/dev/null; }

echo "== launch + watch (no TTY, hotkeys skipped) =="
"$BIN" run --name "$N1" -- "sleep 600" >/tmp/ak_m2a.log 2>&1 &
SUP1=$!
sleep 3

echo "== status (expect RUNNING) =="
"$BIN" status
check "status shows RUNNING" '"$BIN" status | grep -q RUNNING'

echo "== detach via command =="
"$BIN" detach --name "$N1"
# Poll instead of a fixed sleep: detach goes through the control-file poller
# (pollControl) plus the supervisor's own poll_interval, so a fixed duration
# can race this on a loaded/CI runner. Ceiling is generous.
for _ in $(seq 1 20); do
  stopped "$SUP1" && break
  sleep 0.5
done
check "supervisor exited after detach" 'stopped "$SUP1"'
check "session still alive after detach" 'tmux has-session -t "$N1" 2>/dev/null'
check "status now shows DETACHED" '"$BIN" status --name "$N1" | grep -q DETACHED'

echo "== stop --kill on a second instance =="
"$BIN" run --name "$N2" -- "sleep 600" >/tmp/ak_m2b.log 2>&1 &
SUP2=$!
sleep 3
"$BIN" stop --name "$N2" --kill
# Same rationale as the detach poll above: give the control-file poller and
# supervisor poll_interval a generous, polled ceiling instead of a fixed sleep.
for _ in $(seq 1 20); do
  stopped "$SUP2" && break
  sleep 0.5
done
check "supervisor-2 exited after kill" 'stopped "$SUP2"'
check "session-2 terminated by kill" '! tmux has-session -t "$N2" 2>/dev/null'

echo
echo "---- supervisor 1 log ----"; cat /tmp/ak_m2a.log
echo "---- supervisor 2 log ----"; cat /tmp/ak_m2b.log

if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
