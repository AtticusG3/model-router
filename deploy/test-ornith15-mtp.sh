#!/bin/bash
# Load a grafted Ornith-1.5 GGUF with --spec-type draft-mtp and prove decode.
# Does not kill other llama-server processes. Copy as LF.
set -u
PORT=${PROBE_PORT:-18999}
BIN=/opt/ai/bin/llama-server
SMI=${SMI:-0}
OUT=/tmp/test-ornith15-mtp.txt
LOG=/tmp/test-ornith15-mtp.server.log
: > "$OUT"
log() { echo "$@" | tee -a "$OUT"; }

used() {
  nvidia-smi --id="$SMI" --query-gpu=memory.used --format=csv,noheader,nounits | awk '{print int($1)}'
}

NODE=$(hostname)
case "$NODE" in
  gareths-homelab|gareth)
    DIR=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model
    M="$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M-grafted.gguf"
    TH=8
    NGL=${NGL-}
    CTX=${CTX:-8192}
    ;;
  digger)
    DIR=/srv/ssd/models/llm
    M="$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M-grafted.gguf"
    TH=3
    NGL=${NGL:-99}
    CTX=${CTX:-8192}
    ;;
  buster)
    DIR=/home/kevyn/models
    M="$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf"
    TH=4
    SMI=${SMI:-1}
    NGL=${NGL:-99}
    CTX=${CTX:-262144}
    ;;
  nugget)
    DIR=/opt/ai/models/llm
    M="$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf"
    TH=3
    SMI=${SMI:-0}
    NGL=${NGL:-99}
    CTX=${CTX:-262144}
    ;;
  *)
    log "unknown host $NODE"
    exit 1
    ;;
esac

if [ ! -f "$M" ]; then
  log "missing $M"
  exit 1
fi

pkill -f "llama-server.*--port ${PORT}" 2>/dev/null || true
fuser -k "${PORT}/tcp" 2>/dev/null || true
sleep 2
BASE=$(used)
log "START $(date -Is) node=$NODE smi=$SMI ngl=$NGL ctx=$CTX used_base=$BASE"
log "model $M"

NGL_ARGS=()
if [ -n "${NGL}" ]; then
  NGL_ARGS=( --n-gpu-layers "$NGL" )
fi
export CUDA_VISIBLE_DEVICES="$SMI"
"$BIN" --model "$M" "${NGL_ARGS[@]}" --cont-batching --flash-attn on \
  --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 512 \
  --jinja --metrics --split-mode none --device CUDA0 \
  --ctx-size "$CTX" --threads "$TH" --parallel 1 \
  --cache-type-k turbo4 --cache-type-v turbo4 \
  --spec-type draft-mtp --spec-draft-n-max 4 --spec-draft-n-min 1 \
  --port "$PORT" --host 127.0.0.1 \
  >"$LOG" 2>&1 &
PID=$!
ok=0
for i in $(seq 1 180); do
  if ! kill -0 "$PID" 2>/dev/null; then
    log "LOSE dead"
    tail -40 "$LOG" | tee -a "$OUT"
    exit 1
  fi
  if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 1
done
if [ "$ok" != 1 ]; then
  log "LOSE timeout"
  kill "$PID" 2>/dev/null || true
  tail -40 "$LOG" | tee -a "$OUT"
  exit 1
fi
NOW=$(used)
log "HEALTH used=$NOW delta=$((NOW - BASE))"
grep -E "n_layer|nextn|MTP|spec|draft" "$LOG" | head -40 | tee -a "$OUT" || true

python3 - "$PORT" <<'PY'
import json, sys, urllib.request
port = int(sys.argv[1])
url = "http://127.0.0.1:%d/v1/chat/completions" % port
body = json.dumps({
    "messages": [{"role": "user", "content": "Write a Python function that merges two sorted integer lists. Return only the function."}],
    "max_tokens": 128,
    "temperature": 0,
}).encode()
req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
raw = urllib.request.urlopen(req, timeout=180).read()
data = json.loads(raw)
text = data["choices"][0]["message"]["content"]
usage = data.get("usage") or {}
print("GEN", json.dumps({"text": text, "usage": usage})[:800])
PY
log "GEN_RC=$?"

log "--- metrics ---"
curl -sf "http://127.0.0.1:${PORT}/metrics" | grep -Ei "spec|draft|token|predicted|accepted" | tee -a "$OUT" || true
log "--- log tail ---"
tail -60 "$LOG" | tee -a "$OUT"

kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
sleep 2
log "DONE used=$(used)"
