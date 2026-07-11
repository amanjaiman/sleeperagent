#!/usr/bin/env bash
# Verifies interactive attach against REAL tmux: `run` from a terminal attaches
# the user's terminal to the session automatically, the supervisor does NOT
# mistake its own attach client for a human takeover, detaching the view keeps
# the watchdog running, and a real (second) attach after that still triggers
# auto-detach as before.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
# This test gives the binary a real TTY, which would arm the startup update
# prompt on a version-stamped build; keep the test hermetic.
export SLEEPERAGENT_NO_UPDATE_CHECK=1
N="ak-ia-$$"
fail=0

cleanup() { tmux kill-session -t "$N" 2>/dev/null; rm -rf "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

echo "== run inside a pty so interactive attach engages =="
# Same TERM trick as the autodetach test: give tmux a terminfo it can attach with.
TERM=xterm script -qfc "$BIN run --name $N -- 'sleep 600'" /dev/null >/tmp/ak_ia_script.log 2>&1 &
SCRIPT_PID=$!

# The supervisor should create the session AND attach our pty to it.
attached=0
for _ in $(seq 1 40); do
  if [ -n "$(tmux list-clients -t "$N" 2>/dev/null)" ]; then
    attached=1
    break
  fi
  sleep 0.5
done
check "session auto-attached a client" '[ "$attached" -eq 1 ]'

# Give the supervisor several poll cycles (default 3s): it must NOT auto-detach
# on its own client. The old behavior would flip the state file to DETACHED.
sleep 10
check "supervisor still watching (state RUNNING)" '"$BIN" status --name "$N" | grep -q RUNNING'
check "no auto-detach on self view" '! grep -qi "auto-detach" "$SLEEPERAGENT_STATE_DIR/$N.log"'

echo "== detach the view; the watchdog must keep running =="
tmux detach-client -s "$N" 2>/dev/null
viewgone=0
for _ in $(seq 1 20); do
  if [ -z "$(tmux list-clients -t "$N" 2>/dev/null)" ]; then
    viewgone=1
    break
  fi
  sleep 0.5
done
check "view client detached" '[ "$viewgone" -eq 1 ]'
sleep 8
check "still RUNNING after view detach" '"$BIN" status --name "$N" | grep -q RUNNING'
# After the view detaches, logging returns to the console (the script pty),
# so the "still watching" hint lands in the script log, not the file log.
check "console mentions still watching" 'grep -qia "still watching" /tmp/ak_ia_script.log'
# Hotkeys fall back on once the view is gone, restoring the classic console
# controls; the legend is printed when they engage.
sleep 2
check "hotkey legend printed after view detach" 'grep -qa "\[d\]etach" /tmp/ak_ia_script.log'

echo "== a real re-attach after the self-view must auto-detach (old behavior) =="
TERM=xterm script -qfc "tmux attach -t $N" /dev/null >/tmp/ak_ia_reattach.log 2>&1 &
ATT=$!
detached=0
for _ in $(seq 1 60); do
  if "$BIN" status --name "$N" 2>/dev/null | grep -q DETACHED; then
    detached=1
    break
  fi
  sleep 0.5
done
check "supervisor auto-detached on real re-attach" '[ "$detached" -eq 1 ]'
check "session still alive (handed to user)" 'tmux has-session -t "$N" 2>/dev/null'

kill "$ATT" 2>/dev/null
kill "$SCRIPT_PID" 2>/dev/null
echo "---- supervisor log ----"; cat "$SLEEPERAGENT_STATE_DIR/$N.log" 2>/dev/null
if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
