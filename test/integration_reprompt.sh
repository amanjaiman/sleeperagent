#!/usr/bin/env bash
# M4 end-to-end against REAL tmux + a FAKE Ollama server: on reset, SleeperAgent
# reads the transcript tail, asks the (fake) local model for a next instruction,
# validates it, and injects THAT generated instruction instead of the static
# prompt. Run from WSL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
SESSION="ak-rp-$$"
PORT=11987
LLM_TEXT="Continue implementing the parser in parser.go"
MARKER="$(mktemp)"
CFG="$(mktemp --suffix=.toml)"
AGENT="$(mktemp --suffix=.sh)"
TRANS="$(mktemp -d)"
export SLEEPERAGENT_STATE_DIR="$(mktemp -d)"

cleanup() {
  tmux kill-session -t "$SESSION" 2>/dev/null
  [ -n "${OLLAMA_PID:-}" ] && kill "$OLLAMA_PID" 2>/dev/null
  rm -rf "$MARKER" "$CFG" "$AGENT" "$TRANS" "$SLEEPERAGENT_STATE_DIR"
}
trap cleanup EXIT

# A transcript for the reprompt builder to summarize.
echo '{"role":"user","content":"build a config-driven limit parser"}' > "$TRANS/session.jsonl"

# Fake Ollama: always returns a fixed instruction on POST /api/generate.
python3 - "$PORT" "$LLM_TEXT" <<'PY' &
import sys, json, http.server
port, text = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); self.rfile.read(n)
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(json.dumps({"response": text, "done": True}).encode())
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PY
OLLAMA_PID=$!
sleep 1

cat > "$CFG" <<EOF
poll_interval = "1s"
reset_buffer  = "1s"
max_wait      = "24h"
[agents.fake]
launch_cmd      = "true"
limit_patterns  = ["(?i)Claude AI usage limit reached\\\\|(?P<ts>\\\\d+)"]
inject_style    = "text-enter"
transcript_glob = "$TRANS/*.jsonl"
[reprompt]
provider         = "ollama"
base_url         = "http://127.0.0.1:$PORT"
max_prompt_chars = 600
tail_messages    = 10
denylist         = ["rm -rf", "--force"]
EOF

cat > "$AGENT" <<EOF
#!/usr/bin/env bash
echo "agent: working..."
sleep 1
echo "Claude AI usage limit reached|\$(( \$(date +%s) + 6 ))"
while IFS= read -r line; do echo "agent received: \$line"; echo "\$line" >> "$MARKER"; done
EOF
chmod +x "$AGENT"

echo "== launching with --reprompt ollama:test =="
"$BIN" run --agent fake --name "$SESSION" --config "$CFG" --reprompt ollama:test -- "$AGENT" >/tmp/ak_rp.log 2>&1 &
SUP=$!
sleep 14

kill "$SUP" 2>/dev/null; wait "$SUP" 2>/dev/null
echo "== supervisor log =="; cat /tmp/ak_rp.log
echo "== marker (what the agent received) =="; cat "$MARKER" 2>/dev/null

if grep -qF "$LLM_TEXT" "$MARKER" 2>/dev/null; then
  echo "RESULT: PASS — LLM-generated instruction was injected"
  exit 0
else
  echo "RESULT: FAIL — generated instruction not found"
  exit 1
fi
