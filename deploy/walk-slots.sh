#!/bin/bash
# Per-slot VRAM ladder walk. --ctx-size is TOTAL KV: n slots at 256k each
# is --ctx-size 524288 --parallel 2. Copy as LF (not PowerShell-sed).
# Run after model-router is stopped.
set -u
P=/tmp/probe-vram.sh
OUT=/tmp/walk-slots.txt
BIN=/opt/ai/bin/llama-server
C256=262144
C2X256=524288
C128=131072
C2X128=262144
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
    Q36=( "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --jinja --chat-template-file "$TPL"
      --reasoning-format deepseek --reasoning-preserve --metrics
      --split-mode none --device CUDA0 )
    MMP36=( --mmproj "$A/mmproj-Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-F16.gguf" --no-mmproj-offload )
    MTP36=( --spec-type draft-mtp --spec-draft-n-max 4 )
    if try "qwen36-1x256k-turbo4-extras" "$SMI" 120 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
      --ctx-size "$C256" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen36-2x256k-turbo4-extras" "$SMI" 180 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "qwen36-2x256k-turbo4-nomtp" "$SMI" 180 "${Q36[@]}" "${MMP36[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "qwen36-2x256k-turbo4-text" "$SMI" 180 "${Q36[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    else
      try "qwen36-1x128k-turbo4-extras" "$SMI" 120 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C128" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "qwen36-2x128k-turbo4-extras" "$SMI" 150 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C2X128" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi

    OR=( "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf"
      --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics
      --split-mode none --device CUDA0 )
    MMPOR=( --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-BF16.gguf" --no-mmproj-offload )
    MTPOR=( --spec-type draft-mtp --spec-draft-n-max 4 )
    if try "ornith-1x256k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
      --ctx-size "$C256" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "ornith-2x256k-turbo4-extras" "$SMI" 180 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "ornith-1x128k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C128" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi

    Q38=( "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --jinja --chat-template-file "$TPL"
      --reasoning-format deepseek --reasoning-preserve --metrics
      --split-mode none --device CUDA0 )
    if try "qwen38-1x256k-turbo4" "$SMI" 90 "${Q38[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen38-2x256k-turbo4" "$SMI" 150 "${Q38[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "qwen38-1x128k-turbo4" "$SMI" 90 "${Q38[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi

    AG=( "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf"
      --n-gpu-layers 999 --cont-batching --flash-attn on
      --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics
      --split-mode none --device CUDA0 )
    MMPAG=( --mmproj "$A/mmproj-Agents-A1-Uncensored-MTP-BF16.gguf" )
    MTPAG=( --spec-type draft-mtp --spec-draft-n-max 1 )
    if try "agents-1x256k-turbo4-extras" "$SMI" 90 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
      --ctx-size "$C256" --threads 14 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "agents-2x256k-turbo4-extras" "$SMI" 150 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
        --ctx-size "$C2X256" --threads 14 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "agents-1x128k-turbo4-extras" "$SMI" 90 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
        --ctx-size "$C128" --threads 14 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi
    ;;

  nugget)
    A=/opt/ai/models/llm
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    Q36=( "$BIN" --model "$A/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Quality.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --jinja --chat-template-file "$TPL"
      --reasoning-format deepseek --reasoning-preserve --metrics
      --split-mode none --device CUDA0 )
    MMP36=( --mmproj "$A/mmproj-Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-F16.gguf" --no-mmproj-offload )
    MTP36=( --spec-type draft-mtp --spec-draft-n-max 4 )
    if try "qwen36-1x256k-turbo4-extras" "$SMI" 120 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen36-2x256k-turbo4-extras" "$SMI" 180 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "qwen36-2x256k-turbo4-nomtp" "$SMI" 180 "${Q36[@]}" "${MMP36[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "qwen36-2x256k-turbo4-text" "$SMI" 180 "${Q36[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    else
      try "qwen36-1x128k-turbo4-extras" "$SMI" 120 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "qwen36-2x128k-turbo4-extras" "$SMI" 150 "${Q36[@]}" "${MMP36[@]}" "${MTP36[@]}" \
        --ctx-size "$C2X128" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi

    OR=( "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Quality.gguf"
      --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics
      --split-mode none --device CUDA0 )
    MMPOR=( --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-BF16.gguf" --no-mmproj-offload )
    MTPOR=( --spec-type draft-mtp --spec-draft-n-max 3 )
    if try "ornith-1x256k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "ornith-2x256k-turbo4-extras" "$SMI" 180 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "ornith-1x128k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi

    Q38=( "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --jinja --chat-template-file "$TPL"
      --reasoning-format deepseek --reasoning-preserve --metrics
      --split-mode none --device CUDA0 )
    if try "qwen38-1x256k-turbo4" "$SMI" 90 "${Q38[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen38-2x256k-turbo4" "$SMI" 150 "${Q38[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "qwen38-1x128k-turbo4" "$SMI" 90 "${Q38[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi

    AG=( "$BIN" --model "$A/Agents-A1-Uncensored-MTP-APEX-I-Quality.gguf"
      --n-gpu-layers 999 --cont-batching --flash-attn on --mlock --no-mmap
      --cache-reuse 256 --batch-size 512 --ubatch-size 256 --metrics
      --split-mode none --device CUDA0 )
    MMPAG=( --mmproj "$A/mmproj-Agents-A1-Uncensored-MTP-BF16.gguf" --no-mmproj-offload )
    MTPAG=( --spec-type draft-mtp --spec-draft-n-max 4 )
    if try "agents-1x256k-turbo4-extras" "$SMI" 120 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "agents-2x256k-turbo4-extras" "$SMI" 180 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "agents-1x128k-turbo4-extras" "$SMI" 120 "${AG[@]}" "${MMPAG[@]}" "${MTPAG[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi
    ;;

  digger)
    A=/srv/ssd/models/llm
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    OR=( "$BIN" --model "$A/Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --no-mmap --mlock --jinja --metrics
      --cache-reuse 256 --split-mode none --device CUDA0 )
    MMPOR=( --mmproj "$A/mmproj-Ornith-1.0-35B-Heretic-MTP-APEX-I-Compact-F16.gguf" --no-mmproj-offload )
    MTPOR=( --spec-type draft-mtp --spec-draft-n-max 3 )
    if try "ornith-1x256k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "ornith-2x256k-turbo4-extras" "$SMI" 180 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "ornith-1x128k-turbo4-extras" "$SMI" 120 "${OR[@]}" "${MMPOR[@]}" "${MTPOR[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi

    Q38=( "$BIN" --model "$A/Qwen3.8-27B-UD-Q5_K_XL.gguf"
      --n-gpu-layers 99 --cont-batching --flash-attn on --no-mmap --mlock
      --cache-reuse 256 --jinja --chat-template-file "$TPL"
      --reasoning-format deepseek --metrics --split-mode none --device CUDA0 )
    if try "qwen38-1x256k-turbo4" "$SMI" 90 "${Q38[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen38-2x256k-turbo4" "$SMI" 150 "${Q38[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "qwen38-1x128k-turbo4" "$SMI" 90 "${Q38[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi
    ;;

  gareths-homelab)
    BASE=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    Q36=( "$BIN" --model "$BASE/models/qwen36/Qwen3.6-35B-A3B-uncensored-heretic-Native-MTP-Preserved-APEX-I-Compact.gguf"
      --cont-batching --flash-attn on --mlock --no-mmap
      --jinja --chat-template-file "$TPL" --reasoning-format deepseek --metrics
      --split-mode none --device CUDA0 )
    if try "qwen36-1x256k-turbo4" "$SMI" 120 "${Q36[@]}" \
      --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "qwen36-2x256k-turbo4" "$SMI" 180 "${Q36[@]}" \
        --ctx-size "$C2X256" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "qwen36-1x128k-turbo4" "$SMI" 120 "${Q36[@]}" \
        --ctx-size "$C128" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "qwen36-2x128k-turbo4" "$SMI" 150 "${Q36[@]}" \
        --ctx-size "$C2X128" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi

    OR=( "$BIN" --model "$BASE/models/ornith-model/Ornith-1.0-35B-MTP-APEX-I-Compact.gguf"
      --mmproj "$BASE/models/ornith-model/mmproj-F16.gguf"
      --cont-batching --flash-attn on --mlock --no-mmap --metrics
      --spec-type draft-mtp --split-mode none --device CUDA0 )
    if try "ornith-1x256k-turbo4-extras" "$SMI" 120 "${OR[@]}" \
      --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "ornith-2x256k-turbo4-extras" "$SMI" 180 "${OR[@]}" \
        --ctx-size "$C2X256" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "ornith-1x128k-turbo4-extras" "$SMI" 120 "${OR[@]}" \
        --ctx-size "$C128" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    fi
    ;;

  nomad)
    A=/opt/ai/models/llm/qwen35-9b-vision
    TPL=/opt/ai/config/qwen-fixed-chat-template.jinja
    SMI=0
    Q=( "$BIN" --model "$A/Qwen3.5-9B-Abliterated-Claude-4.6-Opus-Reasoning-Distilled-v2.Q4_K_M.gguf"
      --mmproj "$A/mmproj-f16.gguf" --n-gpu-layers 99 --flash-attn on --cont-batching --mlock --metrics
      --jinja --chat-template-file "$TPL" --reasoning-format deepseek --image-min-tokens 2048
      --cache-reuse 256 --split-mode none --device CUDA0 )
    try "qwen35-1x49k-turbo4" "$SMI" 60 "${Q[@]}" \
      --ctx-size 49152 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 || true
    ;;

  *)
    log "unknown hostname $NODE"
    exit 1
    ;;
esac

log "DONE $(date -Is)"
log "--- summary ---"
grep -E '^(WIN|LOSE|OK used)' "$OUT" | tee -a "$OUT"
