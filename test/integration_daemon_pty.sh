#!/usr/bin/env bash
# Verifies --daemon with the pty backend (no tmux): the background supervisor
# runs the agent headless in a pty, detects the limit, and injects the resume
# prompt — the same path the Windows ConPTY backend takes. Run from WSL/Linux.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"
N="ak-dpty-$$"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"
MARKER="$(mktemp)"
fail=0

cleanup() { rm -rf "$CFG" "$AGENT" "$MARKER" "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
[agents.fake]
launch_cmd     = "true"
limit_patterns = ["(?i)Claude AI usage limit reached\\\\|(?P<ts>\\\\d+)"]
inject_style   = "text-enter"
EOF

cat > "$AGENT" <<EOF
#!/usr/bin/env bash
( sleep 13; kill \$\$ ) >/dev/null 2>&1 &
echo "agent: working..."
sleep 1
echo "Claude AI usage limit reached|\$(( \$(date +%s) + 6 ))"
while IFS= read -r line; do echo "got: \$line"; echo "\$line" >> "$MARKER"; done
EOF
chmod +x "$AGENT"

echo "== run --daemon --backend pty (returns immediately) =="
out="$("$BIN" run --agent fake --name "$N" --backend pty --daemon --no-notify --config "$CFG" --prompt pty-daemon-continue -- "bash $AGENT" 2>&1)"
echo "$out"
check "parent printed the pty-daemon note" 'echo "$out" | grep -qi "pty backend"'
check "parent printed 'started in background'" 'echo "$out" | grep -q "started in background"'

sleep 15
echo "== daemon log =="; cat "$SLEEPERAGENT_STATE_DIR/$N.log" 2>/dev/null
echo "== marker =="; cat "$MARKER" 2>/dev/null

check "daemon injected the resume prompt via pty" 'grep -q "pty-daemon-continue" "$MARKER" 2>/dev/null'
check "log shows the limit was detected" 'grep -qi "usage limit detected" "$SLEEPERAGENT_STATE_DIR/$N.log" 2>/dev/null'

if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
