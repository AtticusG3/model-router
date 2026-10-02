#!/bin/bash
set -eu
python3 /tmp/fix-lf.py /tmp/list-gguf-tensors.py /tmp/gguf_mtp_graft.py
if [ ! -x /tmp/gguf-venv/bin/python ]; then
  python3 -m venv --system-site-packages /tmp/gguf-venv
  /tmp/gguf-venv/bin/pip install -q gguf
fi
/tmp/gguf-venv/bin/python -c "import gguf; print('gguf', gguf.__file__)"
echo "=== donor tensors ==="
unset GGUF_PY
/tmp/gguf-venv/bin/python /tmp/list-gguf-tensors.py /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/mtpdraft-Q8_0.gguf
echo "=== graft ==="
/tmp/gguf-venv/bin/python /tmp/gguf_mtp_graft.py \
  /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M.gguf \
  /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/mtpdraft-Q8_0.gguf \
  /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/Ornith-1.5-35B-A3B-BigBang-MTP-Q4_K_M-grafted.gguf
