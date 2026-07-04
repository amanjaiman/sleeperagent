#!/usr/bin/env bash
# M3 end-to-end against REAL tmux: the same supervisor loop driving the CODEX
# adapter, resolving a RELATIVE-duration reset ("try again in 6s") and injecting
# the resume prompt with codex's text-enter style. Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
SESSION="ak-codex-$$"
MARKER="$(mktemp)"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"

cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null; rm -rf "$MARKER" "$CFG" "$AGENT" "$SLEEPERAGENT_STATE_DIR"; }
trap cleanup EXIT

# Fast timings only; the default codex adapter patterns are used as-is.
cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
max_wait      = "24h"
EOF

# Fake codex: work, hit a relative-duration rate limit, then echo injected lines.
cat > "$AGENT" <<EOF
#!/usr/bin/env bash
echo "codex: drafting changes..."
sleep 1
echo "Rate limit reached. Try again in 6s."
while IFS= read -r line; do
  echo "codex received: \$line"
  echo "\$line" >> "$MARKER"
done
EOF
chmod +x "$AGENT"

echo "== launching supervisor with --agent codex =="
"$BIN" run --agent codex --name "$SESSION" --config "$CFG" --prompt "codex-continue" -- "$AGENT" >/tmp/ak_codex.log 2>&1 &
SUP=$!
sleep 14

echo "== final pane =="
tmux capture-pane -p -t "$SESSION" 2>/dev/null || echo "(session gone)"

kill "$SUP" 2>/dev/null; wait "$SUP" 2>/dev/null

echo "== supervisor log =="; cat /tmp/ak_codex.log
echo "== marker (what codex received) =="; cat "$MARKER" 2>/dev/null

if grep -q "codex-continue" "$MARKER" 2>/dev/null && grep -qi "source=relative" /tmp/ak_codex.log; then
  echo "RESULT: PASS — codex adapter resumed via a relative-duration reset"
  exit 0
else
  echo "RESULT: FAIL"
  exit 1
fi
