#!/bin/bash
set -u
OUT=/tmp/walk-ornith15-32.txt
BIN=/opt/ai/bin/llama-server
P=/tmp/probe-vram.sh
: > "$OUT"
log() { echo "$@" | tee -a "$OUT"; }

NODE=$(hostname)
case "$NODE" in
  buster)
    M=/home/kevyn/models/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf
    SMI=1
    TH=4
    ROUTER=http://127.0.0.1:18080
    EXTRA=( --split-mode none --device CUDA0 )
    ;;
  nugget)
    M=/opt/ai/models/llm/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf
    SMI=0
    TH=3
    ROUTER=http://127.0.0.1:8081
    EXTRA=( --split-mode none )
    ;;
  *)
    log "bad host $NODE"
    exit 1
    ;;
esac

used() {
  nvidia-smi --id="$SMI" --query-gpu=memory.used --format=csv,noheader,nounits | awk '{print int($1)}'
}

log "START $(date -Is) node=$NODE used=$(used)"
curl -sS -X POST "$ROUTER/_router/unload" -H 'Content-Type: application/json' -d '{"model_id":"agents-a1"}' | tee -a "$OUT"
echo | tee -a "$OUT"
for i in $(seq 1 30); do
  u=$(used)
  log "wait_vram used=$u"
  if [ "$u" -lt 4000 ]; then
    break
  fi
  sleep 2
done
log "gpu used=$(used)"

log "=== 1x256k mtp gen ==="
if SMI="$SMI" CTX=262144 NGL=99 bash /tmp/test-ornith15-mtp.sh; then
  log "WIN 1x256k-mtp"
  cat /tmp/test-ornith15-mtp.txt >> "$OUT"
else
  log "LOSE 1x256k-mtp"
  tail -20 /tmp/test-ornith15-mtp.txt >> "$OUT"
  log "DONE $(date -Is)"
  exit 1
fi

try() {
  local name="$1" t="$2"
  shift 2
  log "=== $name ==="
  if CUDA_VISIBLE_DEVICES="$SMI" "$P" "$SMI" "$t" "$@"; then
    log "WIN $name"
    return 0
  fi
  log "LOSE $name"
  grep -E "out of memory|MTP context|failed to allocate|LOSE" /tmp/probe-vram.server.log | tail -8 | tee -a "$OUT"
  return 1
}

BASE=( "$BIN" --model "$M" --n-gpu-layers 99 --cont-batching --flash-attn on
  --no-mmap --jinja --metrics --cache-reuse 256
  --batch-size 512 --ubatch-size 512
  --cache-type-k turbo4 --cache-type-v turbo4
  --spec-type draft-mtp --spec-draft-n-max 4 --spec-draft-n-min 1
  --threads "$TH" "${EXTRA[@]}" )

try "o15-q5-graft-2x256k-turbo4-mtp" 180 "${BASE[@]}" \
  --ctx-size 524288 --parallel 2 \
|| try "o15-q5-graft-2x256k-turbo4-nomtp" 180 "$BIN" --model "$M" --n-gpu-layers 99 \
  --cont-batching --flash-attn on --no-mmap --jinja --metrics --cache-reuse 256 \
  --batch-size 512 --ubatch-size 512 --cache-type-k turbo4 --cache-type-v turbo4 \
  --threads "$TH" "${EXTRA[@]}" --ctx-size 524288 --parallel 2 \
|| true

log "DONE $(date -Is) used=$(used)"
grep -E '^(WIN|LOSE|OK used|=== |DONE|HEALTH|GEN )' "$OUT"
