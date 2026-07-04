#!/usr/bin/env bash
# M5+ end-to-end: `attach-existing` (watch a session we did NOT launch) and
# `--watch-only` (notify at reset but do not inject), then a normal attach that
# DOES inject — also exercising the crash-recovery re-attach path. Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
SESSION="ak-att-$$"
MARKER="$(mktemp)"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
fail=0

cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null; rm -rf "$MARKER" "$CFG" "$AGENT" "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
max_wait      = "24h"
[agents.fake]
launch_cmd     = "true"
limit_patterns = ["(?i)Claude AI usage limit reached\\\\|(?P<ts>\\\\d+)"]
inject_style   = "text-enter"
EOF

cat > "$AGENT" <<EOF
#!/usr/bin/env bash
echo "agent: working..."
sleep 1
echo "Claude AI usage limit reached|\$(( \$(date +%s) + 6 ))"
while IFS= read -r line; do echo "got: \$line"; echo "\$line" >> "$MARKER"; done
EOF
chmod +x "$AGENT"

# We launch the session ourselves; SleeperAgent only attaches.
tmux new-session -d -s "$SESSION" "bash $AGENT"
sleep 2

echo "== attach-existing --watch-only (should NOT inject) =="
"$BIN" attach-existing --agent fake --target "$SESSION" --config "$CFG" --watch-only --no-notify --prompt wo-prompt >/tmp/ak_wo.log 2>&1 &
WO=$!
sleep 12
wait "$WO" 2>/dev/null
check "watch-only did not inject" '! grep -q "wo-prompt" "$MARKER" 2>/dev/null'
check "watch-only logged it is not injecting" 'grep -qi "watch-only" /tmp/ak_wo.log'
check "session still alive after watch-only" 'tmux has-session -t "$SESSION" 2>/dev/null'

echo "== attach-existing (normal) on the same live session SHOULD inject =="
"$BIN" attach-existing --agent fake --target "$SESSION" --config "$CFG" --no-notify --prompt real-prompt >/tmp/ak_inj.log 2>&1 &
INJ=$!
sleep 7
kill "$INJ" 2>/dev/null; wait "$INJ" 2>/dev/null
check "normal attach injected the resume prompt" 'grep -q "real-prompt" "$MARKER" 2>/dev/null'

echo "---- watch-only log ----"; cat /tmp/ak_wo.log
echo "---- inject log ----"; cat /tmp/ak_inj.log
if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
