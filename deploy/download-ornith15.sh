#!/bin/bash
# Download EryriLabs Ornith-1.5 BigBang GGUF + MTP draft into $1.
# Usage: download-ornith15.sh DEST_DIR FILE [FILE...]
set -euo pipefail
DEST=${1:?dest dir}
shift
REPO=EryriLabs/Ornith-1.5-35B-A3B-BigBang-MTP-GGUF
mkdir -p "$DEST"
cd "$DEST"

HF_BIN=""
if command -v hf >/dev/null 2>&1; then
  HF_BIN=$(command -v hf)
elif [ -x "$HOME/.local/bin/hf" ]; then
  HF_BIN="$HOME/.local/bin/hf"
fi

if [ -z "$HF_BIN" ]; then
  python3 -m pip install -q --user -U "huggingface_hub[cli]" >/tmp/hf-pip.log 2>&1 || true
  if [ -x "$HOME/.local/bin/hf" ]; then
    HF_BIN="$HOME/.local/bin/hf"
  fi
fi

echo "DEST=$DEST HF=$HF_BIN files=$*"
for f in "$@"; do
  if [ -f "$f" ]; then
    sz=$(stat -c%s "$f")
    echo "HAVE $f bytes=$sz"
    continue
  fi
  echo "GET $f"
  if [ -n "$HF_BIN" ]; then
    "$HF_BIN" download "$REPO" "$f" --local-dir "$DEST"
  else
    python3 - <<PY
from huggingface_hub import hf_hub_download
hf_hub_download(repo_id="$REPO", filename="$f", local_dir="$DEST")
PY
  fi
  ls -lh "$f"
done
echo DONE
