#!/usr/bin/env bash
# End-to-end smoke test against REAL tmux: a fake agent prints a usage-limit
# message with a reset ~6s out, then echoes whatever SleeperAgent injects. We
# assert the static "continue" prompt actually arrived through tmux send-keys.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
SESSION="ak-itest-$$"
MARKER="$(mktemp)"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"

cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null; rm -f "$MARKER" "$CFG" "$AGENT"; }
trap cleanup EXIT

# Fast timings so the test runs in seconds.
cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
max_wait      = "24h"
[agents.fake]
launch_cmd     = "true"
limit_patterns = ["(?i)Claude AI usage limit reached\\\\|(?P<ts>\\\\d+)"]
inject_style   = "text-enter"
EOF

# Fake agent: work, hit the limit (unix ts ~6s ahead), then echo injected lines
# into the marker file so we can prove injection went through real tmux.
cat > "$AGENT" <<EOF
#!/usr/bin/env bash
echo "fake-agent: working on the task..."
sleep 1
echo "Claude AI usage limit reached|\$(( \$(date +%s) + 6 ))"
while IFS= read -r line; do
  echo "fake-agent received: \$line"
  echo "\$line" >> "$MARKER"
done
EOF
chmod +x "$AGENT"

echo "== launching supervisor against real tmux =="
"$BIN" run --agent fake --name "$SESSION" --config "$CFG" --prompt "continue" -- "$AGENT" &
SUP=$!

# Poll for the limit -> wait -> inject -> echo cycle to complete instead of a
# fixed sleep: the cycle is detect (1 poll) + wait (~6s reset + 1s buffer) +
# inject + the agent echoing it back, and a fixed duration raced this too
# tightly under a loaded/CI runner. Ceiling is generous; the marker or an early
# supervisor exit both end the wait promptly.
for _ in $(seq 1 40); do
  if grep -q "continue" "$MARKER" 2>/dev/null; then
    break
  fi
  if ! kill -0 "$SUP" 2>/dev/null; then
    break
  fi
  sleep 0.5
done

echo "== final pane =="
tmux capture-pane -p -t "$SESSION" 2>/dev/null || echo "(session gone)"

kill "$SUP" 2>/dev/null
wait "$SUP" 2>/dev/null

echo "== marker (what the agent received) =="
cat "$MARKER" 2>/dev/null

if grep -q "continue" "$MARKER" 2>/dev/null; then
  echo "RESULT: PASS — resume prompt injected through real tmux"
  exit 0
else
  echo "RESULT: FAIL — no injected prompt reached the agent"
  exit 1
fi
