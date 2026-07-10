#!/usr/bin/env bash
# Verifies takeover detection during the self-view against REAL tmux: a second
# client attaching while the interactive self-view is up is a real takeover, so
# the supervisor auto-detaches — but it must NOT yank the humans out of the
# session; it waits for the view to close before exiting.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
N="ak-i2c-$$"
fail=0

cleanup() { tmux kill-session -t "$N" 2>/dev/null; rm -rf "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

clients() { tmux list-clients -t "$N" 2>/dev/null | grep -c .; }

echo "== start interactive run, then attach a second client =="
TERM=xterm script -qfc "$BIN run --name $N -- 'sleep 600'" /dev/null >/tmp/ak_i2c_run.log 2>&1 &
RUN=$!
attached=0
for _ in $(seq 1 40); do
  if [ "$(clients)" -ge 1 ]; then attached=1; break; fi
  sleep 0.5
done
check "self-view attached" '[ "$attached" -eq 1 ]'

TERM=xterm script -qfc "tmux attach -t $N" /dev/null >/tmp/ak_i2c_att.log 2>&1 &
ATT=$!
two=0
for _ in $(seq 1 40); do
  if [ "$(clients)" -ge 2 ]; then two=1; break; fi
  sleep 0.5
done
check "second client attached" '[ "$two" -eq 1 ]'

echo "== supervisor must detect the takeover and step aside =="
detached=0
for _ in $(seq 1 60); do
  if "$BIN" status --name "$N" 2>/dev/null | grep -q DETACHED; then detached=1; break; fi
  sleep 0.5
done
check "supervisor auto-detached on second client" '[ "$detached" -eq 1 ]'
check "session still alive" 'tmux has-session -t "$N" 2>/dev/null'
check "log mentions auto-detach" 'grep -qi "auto-detach" "$SLEEPERAGENT_STATE_DIR/$N.log"'

echo "== supervisor waits for the view instead of yanking it =="
# The self-view must still be attached (not force-detached) while the
# supervisor parks; only when the clients leave does the process exit.
check "clients not yanked" '[ "$(clients)" -ge 2 ]'
check "supervisor process still parked" 'kill -0 "$RUN" 2>/dev/null'
tmux detach-client -s "$N" 2>/dev/null
exited=0
for _ in $(seq 1 30); do
  if ! kill -0 "$RUN" 2>/dev/null; then exited=1; break; fi
  sleep 0.5
done
check "supervisor exited after view closed" '[ "$exited" -eq 1 ]'

kill "$ATT" 2>/dev/null
echo "---- supervisor log ----"; cat "$SLEEPERAGENT_STATE_DIR/$N.log" 2>/dev/null
if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
