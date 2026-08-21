#!/bin/bash
# Walk grafted Q4 + draft-mtp: 240k, 224k, 192k. First WIN stops. Copy as LF.
set -u
OUT=/tmp/walk-ornith15-graft-ctx.txt
: > "$OUT"
log() { echo "$@" | tee -a "$OUT"; }
log "START $(date -Is)"
WINCTX=""
for pair in 245760:240k 229376:224k 196608:192k; do
  ctx=${pair%%:*}
  name=${pair##*:}
  log "=== try $name ctx=$ctx ==="
  if CTX="$ctx" NGL=99 bash /tmp/test-ornith15-mtp.sh; then
    log "WIN $name"
    WINCTX=$ctx
    cat /tmp/test-ornith15-mtp.txt >> "$OUT"
    break
  fi
  log "LOSE $name"
  tail -15 /tmp/test-ornith15-mtp.txt >> "$OUT" || true
done
log "DONE $(date -Is) winctx=${WINCTX:-none}"
