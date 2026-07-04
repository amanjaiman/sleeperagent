#!/usr/bin/env bash
# M5 end-to-end with the PTY backend (no tmux) + a webhook: a fake agent in a
# pty hits a limit, SleeperAgent detects it on the raw stream, waits, injects the
# resume prompt, and POSTs notifications to a local webhook. Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
PORT=11991
MARKER="$(mktemp)"
HOOKLOG="$(mktemp)"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"

cleanup() {
  [ -n "${HOOK_PID:-}" ] && kill "$HOOK_PID" 2>/dev/null
  [ -n "${SUP:-}" ] && kill "$SUP" 2>/dev/null
  exec 9<&- 2>/dev/null
  rm -rf "$MARKER" "$HOOKLOG" "$CFG" "$AGENT" "$SLEEPERAGENT_STATE_DIR" "$STDIN_FIFO"
}
trap cleanup EXIT

# Webhook sink: append each POSTed title to a log.
python3 - "$PORT" "$HOOKLOG" <<'PY' &
import sys, json, http.server
port, logpath = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); data = self.rfile.read(n)
        try: title = json.loads(data).get('title','')
        except Exception: title = '?'
        open(logpath,'a').write(title + "\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PY
HOOK_PID=$!
sleep 1

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
echo "fake-agent (pty): working..."
sleep 1
echo "Claude AI usage limit reached|\$(( \$(date +%s) + 6 ))"
while IFS= read -r line; do echo "received: \$line"; echo "\$line" >> "$MARKER"; done
EOF
chmod +x "$AGENT"

echo "== launching with --backend pty --webhook =="
# Drive a pty for the supervisor's own stdin via script, so it has a controlling
# tty; force TERM in case the CI shell's is one raw-mode setup doesn't like.
#
# script relays its OWN stdin into the pty. In an interactive shell that stdin
# just sits idle, but under CI (a genuinely non-interactive step backgrounded
# with `&`) it's closed/`/dev/null` and reads as immediate EOF -- which made
# `script` (and the wrapped sleeperagent process with it) exit within ~1-2s,
# long before the limit/wait/resume cycle could run. Give it a fifo opened
# read-write so the read end never sees EOF, even though nothing is written.
STDIN_FIFO="$(mktemp -u)"
mkfifo "$STDIN_FIFO"
exec 9<>"$STDIN_FIFO"
TERM=xterm script -qfc "$BIN run --agent fake --name ak-pty-$$ --backend pty --no-notify --config $CFG --webhook http://127.0.0.1:$PORT --prompt pty-continue -- $AGENT" /tmp/ak_pty.log <&9 >/dev/null 2>&1 &
SUP=$!

# Poll for the full limit -> wait -> resume -> notify cycle to complete instead
# of asserting after one fixed sleep, which was too tight under CI load (the
# resume webhook fires only after the injected prompt is verified as accepted,
# a poll or two after injection itself).
for _ in $(seq 1 30); do
  if grep -q "pty-continue" "$MARKER" 2>/dev/null && grep -qi "resumed" "$HOOKLOG" 2>/dev/null; then
    break
  fi
  sleep 1
done
kill "$SUP" 2>/dev/null; wait "$SUP" 2>/dev/null

echo "== supervisor log =="; cat /tmp/ak_pty.log 2>/dev/null
echo "== marker (what the agent received) =="; cat "$MARKER" 2>/dev/null
echo "== webhook titles =="; cat "$HOOKLOG" 2>/dev/null

ok=1
grep -q "pty-continue" "$MARKER" 2>/dev/null || { echo "FAIL: prompt not injected via pty"; ok=0; }
grep -qi "usage limit hit" "$HOOKLOG" 2>/dev/null || { echo "FAIL: no limit webhook"; ok=0; }
grep -qi "resumed" "$HOOKLOG" 2>/dev/null || { echo "FAIL: no resume webhook"; ok=0; }

if [ "$ok" -eq 1 ]; then echo "RESULT: PASS — pty backend resumed and webhooks fired"; exit 0; else echo "RESULT: FAIL"; exit 1; fi
