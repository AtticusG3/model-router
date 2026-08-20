#!/bin/bash
# Sweep --spec-draft-n-max 1,2,3,4 on MTP stanzas via llama-server.
# llama-bench cannot take spec-draft flags; this is a timed /v1/chat/completions loop.
# Uses current stanza threads/batch. Stop model-router first. Copy as LF.
set -u
OUT=/tmp/bench-spec
PORT=18999
BIN=/opt/ai/bin/llama-server
mkdir -p "$OUT"
log() { echo "$@" | tee -a "$OUT/run.log"; }

used() {
  nvidia-smi --id="$SMI" --query-gpu=memory.used --format=csv,noheader,nounits | awk '{print int($1)}'
}

kill_probe() {
  pkill llama-server 2>/dev/null || true
  fuser -k "${PORT}/tcp" 2>/dev/null || true
  sleep 2
}

run_n() {
  local name="$1" nmax="$2"
  shift 2
  kill_probe
  export CUDA_VISIBLE_DEVICES="$SMI"
  "$@" --port "$PORT" --host 127.0.0.1 --spec-type draft-mtp --spec-draft-n-max "$nmax" \
    >"$OUT/${name}-n${nmax}.server.log" 2>&1 &
  local pid=$!
  local ok=0 i
  for i in $(seq 1 180); do
    if ! kill -0 "$pid" 2>/dev/null; then
      log "LOSE $name n=$nmax dead"
      return 1
    fi
    if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
      ok=1
      break
    fi
    sleep 1
  done
  if [ "$ok" != 1 ]; then
    log "LOSE $name n=$nmax timeout"
    kill "$pid" 2>/dev/null || true
    return 1
  fi
  python3 - "$name" "$nmax" "$PORT" <<'PY'
import json, sys, time, urllib.request
name, nmax, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
url = "http://127.0.0.1:%d/v1/chat/completions" % port
body = json.dumps({
    "model": name,
    "messages": [{"role": "user", "content": "Write a Python function that merges two sorted integer lists. Return only the function."}],
    "max_tokens": 128,
    "temperature": 0,
}).encode()
req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
# warmup (CUDA graphs)
urllib.request.urlopen(req, timeout=180).read()
times = []
toks = []
for _ in range(3):
    t0 = time.time()
    raw = urllib.request.urlopen(req, timeout=180).read()
    dt = time.time() - t0
    data = json.loads(raw)
    n = int(data.get("usage", {}).get("completion_tokens") or 0)
    times.append(dt)
    toks.append(n)
    tps = (n / dt) if dt > 0 else 0
    print("OK %s nmax=%s run toks=%d sec=%.3f tps=%.2f" % (name, nmax, n, dt, tps))
avg_t = sum(times) / len(times)
avg_n = sum(toks) / len(toks)
tps = (avg_n / avg_t) if avg_t > 0 else 0
print("AVG %s nmax=%s toks=%.1f sec=%.3f tps=%.2f" % (name, nmax, avg_n, avg_t, tps))
PY
  local rc=$?
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  kill_probe
  return $rc
}

NODE=$(hostname)
log "START $(date -Is) node=$NODE"

case "$NODE" in
  buster)
    SMI=1
    A=/home/kevyn/models
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    COMMON=( --flash-attn on --cont-batching --mlock --no-mmap --metrics
      --cache-type-k turbo4 --cache-type-v turbo4 --cache-reuse 256
      --split-mode none --device CUDA0 )
    for n in 1 2 3 4; do
      run_n agents-a1 "$n" "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf" \
        --n-gpu-layers 999 --ctx-size 262144 --threads 14 --parallel 1 \
        --batch-size 512 --ubatch-size 256 "${COMMON[@]}" || true
    done
    for n in 1 2 3 4; do
      run_n ornith-1.0-35b-heretic "$n" "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf" \
        --n-gpu-layers 999 --ctx-size 262144 --threads 4 --parallel 1 \
        --batch-size 512 --ubatch-size 256 "${COMMON[@]}" || true
    done
    for n in 1 2 3 4; do
      run_n qwen3.6-35b-a3b "$n" "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf" \
        --n-gpu-layers 99 --ctx-size 524288 --threads 4 --parallel 2 \
        --jinja --chat-template-file "$TPL" --reasoning-format deepseek "${COMMON[@]}" || true
    done
    ;;
  nugget)
    SMI=0
    A=/opt/ai/models/llm
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    COMMON=( --flash-attn on --cont-batching --mlock --no-mmap --metrics
      --cache-type-k turbo4 --cache-type-v turbo4 --cache-reuse 256
      --split-mode none --device CUDA0 )
    for n in 1 2 3 4; do
      run_n agents-a1 "$n" "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf" \
        --n-gpu-layers 999 --ctx-size 262144 --threads 3 --parallel 1 \
        --batch-size 512 --ubatch-size 256 "${COMMON[@]}" || true
    done
    for n in 1 2 3 4; do
      run_n ornith-1.0-35b-heretic "$n" "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf" \
        --n-gpu-layers 999 --ctx-size 262144 --threads 3 --parallel 1 \
        --batch-size 512 --ubatch-size 256 "${COMMON[@]}" || true
    done
    for n in 1 2 3 4; do
      run_n qwen3.6-35b-a3b "$n" "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf" \
        --n-gpu-layers 99 --ctx-size 524288 --threads 3 --parallel 2 \
        --jinja --chat-template-file "$TPL" --reasoning-format deepseek "${COMMON[@]}" || true
    done
    ;;
  digger)
    SMI=0
    A=/srv/ssd/models/llm
    COMMON=( --flash-attn on --cont-batching --mlock --no-mmap --jinja --metrics
      --cache-type-k turbo4 --cache-type-v turbo4 --cache-reuse 256
      --split-mode none --device CUDA0 )
    for n in 1 2 3 4; do
      run_n ornith-1.0-35b-heretic "$n" "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact.gguf" \
        --n-gpu-layers 99 --ctx-size 262144 --threads 3 --parallel 1 "${COMMON[@]}" || true
    done
    ;;
  gareths-homelab)
    SMI=0
    BASE=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp
    COMMON=( --flash-attn on --cont-batching --mlock --no-mmap --metrics
      --cache-type-k turbo4 --cache-type-v turbo4
      --split-mode none --device CUDA0 )
    for n in 1 2 3 4; do
      run_n ornith-1.0-35b "$n" "$BIN" --model "$BASE/models/ornith-model/Ornith-1.0-35B-MTP-APEX-I-Compact.gguf" \
        --ctx-size 262144 --threads 8 --parallel 1 "${COMMON[@]}" || true
    done
    ;;
  nomad)
    log "skip: qwen3.5-9b has no --spec-type draft-mtp"
    ;;
  *)
    log "unknown hostname $NODE"
    exit 1
    ;;
esac

log "DONE $(date -Is)"
