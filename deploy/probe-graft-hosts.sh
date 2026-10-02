#!/bin/bash
set -u
python3 /tmp/fix-lf.py /tmp/gguf_mtp_graft.py /tmp/test-ornith15-mtp.sh /tmp/gguf-meta.py 2>/dev/null || python3 /tmp/fix-lf.py /tmp/gguf_mtp_graft.py /tmp/test-ornith15-mtp.sh
echo "HOST $(hostname)"
nvidia-smi --query-gpu=index,name,memory.used,memory.total --format=csv,noheader
python3 - <<'PY'
import numpy
print("numpy", numpy.__version__)
PY
echo "--- models ---"
ls -lh /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/Ornith-1.5* /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/mtpdraft* 2>/dev/null || true
ls -lh /srv/ssd/models/llm/Ornith-1.5* /srv/ssd/models/llm/mtpdraft* 2>/dev/null || true
echo "--- gguf-py ---"
ls /opt/ai/src/llama-cpp-turboquant-fresh/gguf-py 2>/dev/null || true
ls /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/llama-cpp-turboquant-fresh/gguf-py 2>/dev/null || true
echo "--- router ---"
curl -sf http://127.0.0.1:18080/health || curl -sf http://127.0.0.1:8082/health || echo no-router-health
ss -tlnp | grep -E "18080|8082|5972" || true
