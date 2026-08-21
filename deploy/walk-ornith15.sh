#!/bin/bash
# VRAM ladder for Ornith-1.5-35B-A3B-BigBang (MoE). MTP extra is --model-draft.
# Run after model-router has released this GPU. Copy as LF.
set -u
P=/tmp/probe-vram.sh
OUT=/tmp/walk-ornith15.txt
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
  tail -20 /tmp/probe-vram.server.log | tee -a "$OUT" >/dev/null
  return 1
}

NODE=$(hostname)
log "START $(date -Is) node=$NODE"

case "$NODE" in
  buster)
    A=/home/kevyn/models
    SMI=1
    M="$A/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M.gguf"
    D="$A/mtpdraft-Q8_0.gguf"
    BASE=( "$BIN" --model "$M" --n-gpu-layers 99 --cont-batching --flash-attn on
      --mlock --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 512
      --jinja --metrics --split-mode none --device CUDA0 )
    MTP=( --model-draft "$D" --spec-draft-n-max 4 --spec-draft-n-min 1 )
    if try "o15-q5-1x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
      --ctx-size "$C256" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "o15-q5-2x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-2x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    else
      try "o15-q5-1x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C256" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "o15-q5-2x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 4 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-1x128k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
        --ctx-size "$C128" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-1x128k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C128" --threads 4 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi
    ;;

  nugget)
    A=/opt/ai/models/llm
    SMI=0
    M="$A/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M.gguf"
    D="$A/mtpdraft-Q8_0.gguf"
    BASE=( "$BIN" --model "$M" --n-gpu-layers 99 --cont-batching --flash-attn on
      --mlock --no-mmap --cache-reuse 256 --batch-size 512 --ubatch-size 512
      --jinja --metrics --split-mode none )
    MTP=( --model-draft "$D" --spec-draft-n-max 4 --spec-draft-n-min 1 )
    if try "o15-q5-1x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "o15-q5-2x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-2x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    else
      try "o15-q5-1x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "o15-q5-2x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-1x128k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      || try "o15-q5-1x128k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi
    ;;

  digger)
    A=/srv/ssd/models/llm
    SMI=0
    M="$A/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M.gguf"
    # mtpdraft-Q8_0.gguf is MTP head-only (blk.40); --model-draft segfaults. Skip.
    BASE=( "$BIN" --model "$M" --n-gpu-layers 99 --cont-batching --flash-attn on
      --no-mmap --mlock --jinja --metrics --cache-reuse 256
      --batch-size 512 --ubatch-size 512 --split-mode none --device CUDA0 )
    if try "o15-q4-1x256k-turbo4" "$SMI" 180 "${BASE[@]}" \
      --ctx-size "$C256" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "o15-q4-2x256k-turbo4" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      try "o15-q4-1x128k-turbo4" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C128" --threads 3 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
      && try "o15-q4-2x128k-turbo4" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X128" --threads 3 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
      || true
    fi
    ;;

  gareths-homelab)
    BASEDIR=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model
    SMI=0
    M="$BASEDIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M.gguf"
    D="$BASEDIR/mtpdraft-Q8_0.gguf"
    BASE=( "$BIN" --model "$M" --cont-batching --flash-attn on --mlock --no-mmap
      --jinja --metrics --split-mode none --device CUDA0 )
    MTP=( --model-draft "$D" --spec-draft-n-max 4 --spec-draft-n-min 1 )
    # 21GB Q4 will not fit 16GB fully on GPU; drop MTP then n-cpu-moe.
    if try "o15-q4-1x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
      --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "o15-q4-2x256k-turbo4-mtp" "$SMI" 180 "${BASE[@]}" "${MTP[@]}" \
        --ctx-size "$C2X256" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    elif try "o15-q4-1x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
      --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
      try "o15-q4-2x256k-turbo4-nomtp" "$SMI" 180 "${BASE[@]}" \
        --ctx-size "$C2X256" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 || true
    else
      WINN=""
      for n in 4 8 12 16 20 24 32 40; do
        if try "o15-q4-1x256k-turbo4-moeoff$n" "$SMI" 240 "${BASE[@]}" \
          --n-cpu-moe "$n" --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4; then
          WINN=$n
          break
        fi
      done
      if [ -n "$WINN" ]; then
        try "o15-q4-2x256k-turbo4-moeoff${WINN}" "$SMI" 240 "${BASE[@]}" \
          --n-cpu-moe "$WINN" --ctx-size "$C2X256" --threads 8 --parallel 2 --cache-type-k turbo4 --cache-type-v turbo4 \
        || true
        try "o15-q4-1x256k-turbo4-moeoff${WINN}-mtp" "$SMI" 240 "${BASE[@]}" "${MTP[@]}" \
          --n-cpu-moe "$WINN" --ctx-size "$C256" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
        || true
      else
        for n in 8 16 24 32 40; do
          try "o15-q4-1x128k-turbo4-moeoff$n" "$SMI" 240 "${BASE[@]}" \
            --n-cpu-moe "$n" --ctx-size "$C128" --threads 8 --parallel 1 --cache-type-k turbo4 --cache-type-v turbo4 \
          && break || true
        done
      fi
    fi
    ;;

  *)
    log "skip node=$NODE (cannot run Ornith 1.5 Q4/Q5 on this GPU)"
    exit 0
    ;;
esac

log "DONE $(date -Is)"
log "--- summary ---"
grep -E '^(WIN|LOSE|OK used|=== )' "$OUT" | tee -a "$OUT"
