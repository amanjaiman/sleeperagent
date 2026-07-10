#!/usr/bin/env bash
# Run every integration script and summarize. Needs tmux + python3.
cd "$(dirname "$0")/.."
fail=0
for s in integration integration_m2 integration_m2_autodetach \
         integration_attach integration_codex integration_reprompt integration_pty \
         integration_dead_session integration_interactive_attach; do
  sed -i 's/\r$//' "test/$s.sh" 2>/dev/null
  printf '%-30s ' "$s"
  if bash "test/$s.sh" >"/tmp/$s.out" 2>&1 && grep -q 'RESULT: PASS' "/tmp/$s.out"; then
    echo PASS
  else
    echo "FAIL (see /tmp/$s.out)"
    fail=1
  fi
done
exit "$fail"
