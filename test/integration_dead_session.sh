#!/usr/bin/env bash
# Dead-session exit test (P0-2): the supervisor must stop cleanly when the
# session it is watching goes away. Two cases:
#   1) the tmux session is killed out from under the supervisor;
#   2) the agent process exits on its own.
# In both cases `sleeperagent run` must return within a couple of poll intervals
# instead of looping forever on "capture failed".
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
CFG="$(mktemp --suffix=.toml)"

cleanup() {
  tmux kill-session -t "$S1" 2>/dev/null
  tmux kill-session -t "$S2" 2>/dev/null
  rm -f "$CFG"
}
trap cleanup EXIT

# Fast poll so the supervisor notices the dead session quickly.
cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
max_wait      = "24h"
[agents.fake]
launch_cmd     = "true"
limit_patterns = ["(?i)never-matches-this-pattern"]
inject_style   = "text-enter"
EOF

# waits up to ~10s for pid to exit; returns 0 if it did, 1 on timeout.
wait_exit() {
  local pid=$1 i
  for i in $(seq 1 20); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.5
  done
  return 1
}

fail=0

# --- Case 1: tmux session killed out from under the supervisor ---
S1="ak-dead-kill-$$"
echo "== case 1: kill the tmux session while watching =="
"$BIN" run --agent fake --name "$S1" --config "$CFG" -- bash -c 'echo working; sleep 60' &
SUP1=$!
sleep 3                                   # let it reach the watch loop
tmux kill-session -t "$S1" 2>/dev/null     # session disappears
if wait_exit "$SUP1"; then
  echo "  PASS — supervisor exited after the session was killed"
else
  echo "  FAIL — supervisor still running after session kill"
  kill "$SUP1" 2>/dev/null
  fail=1
fi

# --- Case 2: the agent exits on its own ---
S2="ak-dead-exit-$$"
echo "== case 2: agent process exits on its own =="
"$BIN" run --agent fake --name "$S2" --config "$CFG" -- bash -c 'echo working; sleep 3' &
SUP2=$!
# The agent sleeps 3s then exits; tmux closes the session. The supervisor should
# follow it down.
if wait_exit "$SUP2"; then
  echo "  PASS — supervisor exited after the agent exited"
else
  echo "  FAIL — supervisor still running after agent exit"
  kill "$SUP2" 2>/dev/null
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "RESULT: PASS — supervisor stops cleanly when the session dies"
  exit 0
else
  echo "RESULT: FAIL — supervisor did not exit on a dead session"
  exit 1
fi
