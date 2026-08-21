#!/bin/bash
# wget Ornith-1.5 BigBang GGUFs. Usage: wget-ornith15.sh DEST_DIR FILE [FILE...]
set -eu
DEST=${1:?dest dir}
shift
BASE=https://huggingface.co/EryriLabs/Ornith-1.5-35B-A3B-BigBang-MTP-GGUF/resolve/main
mkdir -p "$DEST"
cd "$DEST"
for f in "$@"; do
  echo "GET $f"
  wget -c --tries=0 --retry-connrefused -O "$f" "$BASE/$f"
  ls -lh "$f"
done
echo DONE
