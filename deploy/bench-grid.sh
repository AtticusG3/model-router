#!/bin/bash
# Sweep threads x batch x ubatch with llama-bench (comma lists = cartesian).
# --ctx-size is not a llama-bench knob; this is pp512/tg128, not the 256k serve ctx.
# Stop model-router first. Copy as LF.
set -u
OUT=/tmp/bench-grid
mkdir -p "$OUT"
log() { echo "$@" | tee -a "$OUT/run.log"; }

NODE=$(hostname)
case "$NODE" in
  buster)
    BINDIR=/home/kevyn/infra/llama-cpp-turboquant-fresh/build-fresh/bin
    SMI=1
    THREADS=3,6,9,12
    A=/home/kevyn/models
    MODELS=(
      "agents-a1|$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf|999"
      "qwen3.8-27b|$A/Qwen3.8-27B-UD-Q5_K_XL.gguf|99"
      "ornith-1.0-35b-heretic|$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf|999"
      "qwen3.6-35b-a3b|$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf|99"
    )
    ;;
  nugget)
    BINDIR=/home/kevyn/llama-cpp-turboquant-fresh/build-fresh/bin
    SMI=0
    THREADS=2,3,4
    A=/opt/ai/models/llm
    MODELS=(
      "agents-a1|$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf|999"
      "qwen3.8-27b|$A/Qwen3.8-27B-UD-Q5_K_XL.gguf|99"
      "ornith-1.0-35b-heretic|$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf|999"
      "qwen3.6-35b-a3b|$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf|99"
    )
    ;;
  digger)
    BINDIR=/opt/ai/src/llama-cpp-turboquant-fresh/build-fresh/bin
    SMI=0
    THREADS=2,4,6
    A=/srv/ssd/models/llm
    MODELS=(
      "ornith-1.0-35b-heretic|$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact.gguf|99"
      "qwen3.8-27b|$A/Qwen3.8-27B-UD-Q5_K_XL.gguf|99"
    )
    ;;
  gareths-homelab)
    BINDIR=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/llama-cpp-turboquant-fresh/build-fresh/bin
    SMI=0
    THREADS=4,8,12
    BASE=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp
    MODELS=(
      "qwen3.6-35b-a3b|$BASE/models/qwen36/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Compact.gguf|99"
      "ornith-1.0-35b|$BASE/models/ornith-model/Ornith-1.0-35B-MTP-APEX-I-Compact.gguf|99"
    )
    ;;
  nomad)
    BINDIR=/home/kevyn/llama-cpp-turboquant-fresh/build-fresh/bin
    SMI=0
    THREADS=2,3,4
    A=/opt/ai/models/llm/qwen35-9b-vision
    MODELS=(
      "qwen3.5-9b|$A/Qwen3.5-9B-Abliterated-Claude-4.6-Opus-Reasoning-Distilled-v2.Q4_K_M.gguf|99"
    )
    ;;
  *)
    echo "unknown hostname $NODE" >&2
    exit 1
    ;;
esac

BENCH="$BINDIR/llama-bench"
export LD_LIBRARY_PATH="$BINDIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export CUDA_VISIBLE_DEVICES="$SMI"
if [ ! -x "$BENCH" ]; then
  echo "missing $BENCH — cmake --build $BINDIR/.. --target llama-bench" >&2
  exit 1
fi

# ubatch always <= min batch so the cartesian is valid.
BATCH=512,1024,2048
UBATCH=128,256,512

log "START $(date -Is) node=$NODE threads=$THREADS batch=$BATCH ubatch=$UBATCH"
for spec in "${MODELS[@]}"; do
  IFS='|' read -r name model ngl <<<"$spec"
  log "=== $name ==="
  CUDA_VISIBLE_DEVICES="$SMI" "$BENCH" \
    -m "$model" \
    -ngl "$ngl" \
    -fa on \
    -ctk turbo4 -ctv turbo4 \
    -sm none \
    -t "$THREADS" \
    -b "$BATCH" \
    -ub "$UBATCH" \
    -p 512 -n 128 \
    -r 3 \
    -o md \
    >"$OUT/${name}.md" || log "FAIL $name"
  log "wrote $OUT/${name}.md"
done
log "DONE $(date -Is)"
log "tables: $OUT/*.md"
