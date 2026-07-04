#!/usr/bin/env bash
# Verifies auto-detach-on-user-activity against REAL tmux: when a human attaches
# to the session, the supervisor steps aside (detaches) and leaves it running.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
N="ak-auto-$$"
fail=0

cleanup() { tmux kill-session -t "$N" 2>/dev/null; rm -rf "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

"$BIN" run --name "$N" -- "sleep 600" >/tmp/ak_auto.log 2>&1 &
SUP=$!
sleep 3

echo "== simulate a human attaching (tmux attach in a pty) =="
# CI shells often run with no usable TERM (or one terminfo lacks, e.g. "dumb"),
# which makes tmux refuse the attach ("open terminal failed: terminal does not
# support clear") before it ever registers a client. Force a terminfo tmux/most
# systems ship so the simulated attach actually takes.
TERM=xterm script -qfc "tmux attach -t $N" /dev/null >/tmp/ak_auto_script.log 2>&1 &
ATT=$!

# Wait for tmux to actually register the attached client before asserting
# anything about the supervisor's reaction — on a loaded/CI runner both the
# `script` pty attach and the supervisor's next poll can lag noticeably past a
# fixed sleep, which made this flaky. Poll instead of guessing a duration.
#
# Require the client to be seen on two consecutive checks (100ms apart) so a
# transient/flapping attach (script reattaching, or a race in when the pty is
# fully set up) doesn't get counted as "attached" before it has really settled.
attached=0
for _ in $(seq 1 40); do
  if [ -n "$(tmux list-clients -t "$N" 2>/dev/null)" ]; then
    sleep 0.1
    if [ -n "$(tmux list-clients -t "$N" 2>/dev/null)" ]; then
      attached=1
      break
    fi
  fi
  sleep 0.5
done
if [ "$attached" -ne 1 ]; then
  echo "  FAIL: tmux never saw a sustained attached client (script/tmux attach didn't take)"
  echo "---- script log ----"; cat /tmp/ak_auto_script.log
  echo "---- tmux list-clients (final) ----"; tmux list-clients -t "$N" 2>&1
  fail=1
fi

# Give the supervisor several poll cycles (default poll_interval is 3s; budget
# a generous multiple of that plus process/IO overhead on a loaded runner)
# to notice and detach.
detached=0
for _ in $(seq 1 60); do
  if ! kill -0 "$SUP" 2>/dev/null; then
    detached=1
    break
  fi
  sleep 0.5
done
if [ "$detached" -ne 1 ]; then
  echo "  FAIL: supervisor (pid $SUP) never exited after 30s of polling"
  echo "---- ps of supervisor tree ----"; ps -ef 2>/dev/null | grep -i sleeperagent
  echo "---- tmux list-clients (final) ----"; tmux list-clients -t "$N" 2>&1
fi

check "supervisor auto-detached" '! kill -0 "$SUP" 2>/dev/null'
check "session still alive (handed to user)" 'tmux has-session -t "$N" 2>/dev/null'
check "status shows DETACHED" '"$BIN" status --name "$N" | grep -q DETACHED'
check "log mentions auto-detach" 'grep -qi "auto-detach" /tmp/ak_auto.log'

kill "$ATT" 2>/dev/null
echo "---- supervisor log ----"; cat /tmp/ak_auto.log
if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
