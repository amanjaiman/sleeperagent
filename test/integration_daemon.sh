#!/usr/bin/env bash
# Verifies --daemon: `run --daemon` returns immediately, the detached child keeps
# watching, status sees it, and detach from another shell stops it while the tmux
# session survives. Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
N="ak-daemon-$$"
CFG="$(mktemp --suffix=.toml)"
fail=0

cleanup() { tmux kill-session -t "$N" 2>/dev/null; rm -rf "$CFG" "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
[agents.fake]
launch_cmd     = "true"
limit_patterns = ["(?i)never-matches-(?P<ts>\\\\d+)"]
inject_style   = "text-enter"
EOF

echo "== run --daemon returns immediately =="
out="$("$BIN" run --agent fake --name "$N" --config "$CFG" --daemon -- "sleep 600" 2>&1)"
echo "$out"
check "parent printed 'started in background'" 'echo "$out" | grep -q "started in background"'

sleep 3
check "tmux session was created by the child" 'tmux has-session -t "$N" 2>/dev/null'
check "status shows the daemon RUNNING" '"$BIN" status --name "$N" | grep -q RUNNING'
check "log file was written" '[ -s "$SLEEPERAGENT_STATE_DIR/$N.log" ]'
DPID="$("$BIN" status --name "$N" >/dev/null 2>&1; cat "$SLEEPERAGENT_STATE_DIR/$N.json" | grep -o "\"pid\": *[0-9]*" | grep -o "[0-9]*")"
check "daemon process is alive" 'kill -0 "$DPID" 2>/dev/null'

echo "== detach from another shell =="
"$BIN" detach --name "$N"
sleep 3
check "daemon process exited after detach" '! kill -0 "$DPID" 2>/dev/null'
check "tmux session still alive (detach kept it)" 'tmux has-session -t "$N" 2>/dev/null'

echo "---- daemon log ----"; cat "$SLEEPERAGENT_STATE_DIR/$N.log"
if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
