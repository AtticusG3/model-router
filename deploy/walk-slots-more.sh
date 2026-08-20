#!/bin/bash
# One more --parallel step past walk-slots.sh caps (those all won).
set -u
P=/tmp/probe-vram.sh
OUT=/tmp/walk-slots-more.txt
BIN=/opt/ai/bin/llama-server
: > "$OUT"
log() { echo "$@" | tee -a "$OUT"; }
try() {
  local name="$1" smi="$2" t="$3"
  shift 3
  log "=== $name ==="
  if CUDA_VISIBLE_DEVICES="$smi" "$P" "$smi" "$t" "$@"; then
    log "WIN $name"
    return 0
  fi
  log "LOSE $name"
  return 1
}
NODE=$(hostname)
log "START $(date -Is) node=$NODE"
case "$NODE" in
  buster)
    A=/home/kevyn/models
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=1
    try "qwen36-256k-turbo4-extras-p5" "$SMI" 120 "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --jinja --chat-template-file "$TPL" --reasoning-format deepseek --reasoning-preserve --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-F16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 4 --ctx-size 262144 --threads 4 --parallel 5 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "ornith-256k-turbo4-extras-p4" "$SMI" 120 "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf" --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-BF16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 4 --ctx-size 262144 --threads 4 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "qwen38-256k-turbo4-p4" "$SMI" 90 "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --jinja --chat-template-file "$TPL" --reasoning-format deepseek --reasoning-preserve --metrics --split-mode none --device CUDA0 --ctx-size 262144 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "agents-256k-turbo4-extras-p4" "$SMI" 90 "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf" --n-gpu-layers 999 --cont-batching --flash-attn on --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Agents-A1-Uncensored-MTP-BF16.gguf" --spec-type draft-mtp --spec-draft-n-max 1 --ctx-size 262144 --threads 14 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;
  nugget)
    A=/opt/ai/models/llm
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    try "qwen36-256k-turbo4-extras-p5" "$SMI" 120 "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --jinja --chat-template-file "$TPL" --reasoning-format deepseek --reasoning-preserve --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-F16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 4 --ctx-size 262144 --threads 3 --parallel 5 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "ornith-256k-turbo4-extras-p4" "$SMI" 120 "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf" --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-BF16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 3 --ctx-size 262144 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "qwen38-256k-turbo4-p4" "$SMI" 90 "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --jinja --chat-template-file "$TPL" --reasoning-format deepseek --reasoning-preserve --metrics --split-mode none --device CUDA0 --ctx-size 262144 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "agents-256k-turbo4-extras-p4" "$SMI" 120 "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf" --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics --split-mode none --device CUDA0 --mmproj "$A/mmproj-Agents-A1-Uncensored-MTP-BF16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 4 --ctx-size 262144 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;
  digger)
    A=/srv/ssd/models/llm
    SMI=0
    try "ornith-256k-turbo4-extras-p4" "$SMI" 120 "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --no-mmap --mlock --jinja --metrics --cache-reuse 256 --split-mode none --device CUDA0 --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact-F16.gguf" --no-mmproj-offload --spec-type draft-mtp --spec-draft-n-max 3 --ctx-size 262144 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "qwen38-128k-turbo4-p4" "$SMI" 90 "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf" --n-gpu-layers 99 --cont-batching --flash-attn on --no-mmap --mlock --cache-reuse 256 --jinja --chat-template-file /opt/ai/config/qwen-fixed-chat-template.jinja --reasoning-format deepseek --metrics --split-mode none --device CUDA0 --ctx-size 131072 --threads 3 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;
  gareths-homelab)
    BASE=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    try "qwen36-256k-turbo4-p4" "$SMI" 120 "$BIN" --model "$BASE/models/qwen36/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Compact.gguf" --cont-batching --flash-attn on --mlock --no-mmap --jinja --chat-template-file "$TPL" --reasoning-format deepseek --metrics --split-mode none --device CUDA0 --ctx-size 262144 --threads 8 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    try "ornith-256k-turbo4-extras-p4" "$SMI" 120 "$BIN" --model "$BASE/models/ornith-model/Ornith-1.0-35B-MTP-APEX-I-Compact.gguf" --mmproj "$BASE/models/ornith-model/mmproj-F16.gguf" --cont-batching --flash-attn on --mlock --no-mmap --metrics --spec-type draft-mtp --split-mode none --device CUDA0 --ctx-size 262144 --threads 8 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;
  nomad)
    A=/opt/ai/models/llm/qwen35-9b-vision
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    try "qwen35-49k-turbo4-p4" "$SMI" 60 "$BIN" --model "$A/Qwen3.5-9B-Abliterated-Claude-4.6-Opus-Reasoning-Distilled-v2.Q4_K_M.gguf" --mmproj "$A/mmproj-f16.gguf" --n-gpu-layers 99 --flash-attn on --cont-batching --mlock --metrics --jinja --chat-template-file "$TPL" --reasoning-format deepseek --image-min-tokens 2048 --cache-reuse 256 --split-mode none --device CUDA0 --ctx-size 49152 --parallel 4 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;
  *)
    log "unknown hostname $NODE"
    exit 1
    ;;
esac
log "DONE $(date -Is)"
