#!/bin/bash
set -eu
python3 /tmp/fix-lf.py /tmp/gguf_mtp_graft.py
if [ ! -x /tmp/gguf-venv/bin/python ]; then
  python3 -m venv --system-site-packages /tmp/gguf-venv
  /tmp/gguf-venv/bin/pip install -q gguf
fi
DIR=/opt/ai/models/llm
OUT="$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf"
if [ -f "$OUT" ]; then
  echo "already have $OUT"
  /tmp/gguf-venv/bin/python /tmp/gguf_mtp_graft.py --verify "$OUT"
  exit 0
fi
/tmp/gguf-venv/bin/python /tmp/gguf_mtp_graft.py \
  "$DIR/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M.gguf" \
  "$DIR/mtpdraft-Q8_0.gguf" \
  "$OUT"
