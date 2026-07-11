#!/usr/bin/env bash
# Verifies the self-update flow end to end against a fake release server:
# `update --check` reports the newer version, `update` downloads the asset,
# verifies it against checksums.txt, and atomically replaces the executable.
# Requires sleeperagent-linux to be built with a version stamp, e.g.
#   go build -ldflags "-X main.version=0.0.0-test" -o sleeperagent-linux ./cmd/sleeperagent
# (a "dev" build refuses to self-update by design).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/sleeperagent-linux"
TMP="$(mktemp -d)"
export SLEEPERAGENT_STATE_DIR="$TMP/state"
fail=0
SRV_PID=""

cleanup() { [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT
check() { if eval "$2"; then echo "  ok: $1"; else echo "  FAIL: $1"; fail=1; fi; }

if ! "$BIN" version | grep -qE '[0-9]+\.[0-9]+'; then
  echo "SKIP-FAIL: $BIN reports a non-semver version; build it with -ldflags '-X main.version=0.0.0-test'"
  echo "RESULT: FAIL"
  exit 1
fi

case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64)        ARCH=amd64 ;;
  *) echo "unsupported test arch $(uname -m)"; echo "RESULT: FAIL"; exit 1 ;;
esac
ASSET="sleeperagent_9.9.9_linux_${ARCH}.tar.gz"

echo "== build a fake v9.9.9 release =="
mkdir -p "$TMP/payload"
printf '#!/bin/sh\necho fake-v9.9.9\n' > "$TMP/payload/sleeperagent"
chmod +x "$TMP/payload/sleeperagent"
DL="$TMP/srv/amanjaiman/sleeperagent/releases/download/v9.9.9"
mkdir -p "$DL"
tar -C "$TMP/payload" -czf "$DL/$ASSET" sleeperagent
(cd "$DL" && sha256sum "$ASSET" > checksums.txt)

PORT=$((21000 + RANDOM % 20000))
python3 - "$TMP/srv" "$PORT" <<'EOF' &
import http.server, socketserver, sys
root, port = sys.argv[1], int(sys.argv[2])
class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=root, **kw)
    def do_GET(self):
        if self.path.rstrip('/').endswith('/releases/latest'):
            self.send_response(302)
            self.send_header('Location',
                'http://127.0.0.1:%d/amanjaiman/sleeperagent/releases/tag/v9.9.9' % port)
            self.end_headers()
            return
        super().do_GET()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(('127.0.0.1', port), H) as s:
    s.serve_forever()
EOF
SRV_PID=$!
up=0
for _ in $(seq 1 40); do
  if bash -c "exec 3<>/dev/tcp/127.0.0.1/$PORT" 2>/dev/null; then up=1; break; fi
  sleep 0.25
done
check "fake release server up" '[ "$up" -eq 1 ]'
export SLEEPERAGENT_UPDATE_BASE_URL="http://127.0.0.1:$PORT"

echo "== update --check reports without installing =="
cp "$BIN" "$TMP/sleeperagent"
OUT="$("$TMP/sleeperagent" update --check 2>&1)"
echo "$OUT"
check "check reports v9.9.9" 'echo "$OUT" | grep -q "update available: v9.9.9"'
check "check does not install" '! "$TMP/sleeperagent" 2>/dev/null | grep -q fake-v9.9.9'

echo "== update installs and swaps the executable =="
OUT="$("$TMP/sleeperagent" update 2>&1)"
echo "$OUT"
check "update reports success" 'echo "$OUT" | grep -q "updated to v9.9.9"'
check "executable replaced" '[ "$("$TMP/sleeperagent")" = "fake-v9.9.9" ]'

echo "== a corrupted asset is refused =="
cp "$BIN" "$TMP/sleeperagent2"
printf 'tampered' >> "$DL/$ASSET"   # checksum no longer matches
OUT="$("$TMP/sleeperagent2" update 2>&1)"
echo "$OUT"
check "tampered download rejected" 'echo "$OUT" | grep -qi "checksum mismatch"'
check "binary untouched on failure" '"$TMP/sleeperagent2" version | grep -qE "[0-9]+\.[0-9]+"'

if [ "$fail" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
exit "$fail"
