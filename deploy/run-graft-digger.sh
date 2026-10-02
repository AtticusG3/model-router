#!/bin/bash
set -eu
python3 /tmp/fix-lf.py /tmp/gguf_mtp_graft.py
if [ ! -x /tmp/gguf-venv/bin/python ]; then
  python3 -m venv --system-site-packages /tmp/gguf-venv
  /tmp/gguf-venv/bin/pip install -q gguf
fi
/tmp/gguf-venv/bin/python -c "import gguf; print('gguf', gguf.__file__)"
unset GGUF_PY
/tmp/gguf-venv/bin/python /tmp/gguf_mtp_graft.py \
  /srv/ssd/models/llm/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M.gguf \
  /srv/ssd/models/llm/mtpdraft-Q8_0.gguf \
  /srv/ssd/models/llm/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M-grafted.gguf
