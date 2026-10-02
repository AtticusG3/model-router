#!/bin/bash
# Print where Ornith 1.5 Q4 tensors actually went (GPU vs CPU).
set -eu
BIN=/opt/ai/bin/llama-server
M=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M.gguf
LOG=/tmp/ornith15-fit.log
PORT=18998
pkill -f "llama-server.*--port ${PORT}" 2>/dev/null || true
sleep 1
: > "$LOG"
"$BIN" --model "$M" --port "$PORT" --host 127.0.0.1 \
  --cont-batching --flash-attn on --mlock --no-mmap --jinja --metrics \
  --split-mode none --device CUDA0 \
  --ctx-size 262144 --threads 8 --parallel 1 \
  --cache-type-k turbo4 --cache-type-v turbo4 \
  --verbosity 1 >"$LOG" 2>&1 &
PID=$!
ok=0
for _ in $(seq 1 120); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo DEAD
    tail -40 "$LOG"
    exit 1
  fi
  if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 1
done
echo "=== nvidia-smi ==="
nvidia-smi --query-gpu=memory.used,memory.free,memory.total --format=csv
echo "=== offload / buffer / cpu ==="
grep -E "offload|Offloaded|buffer size|CPU_REPACK|CUDA0|model size|llm_load_print|n_gpu_layers|gpu_layers|MOE|expert" "$LOG" | head -80
echo "=== health ==="
curl -s "http://127.0.0.1:${PORT}/health"
echo
kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
