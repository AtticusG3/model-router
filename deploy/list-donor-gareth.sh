#!/bin/bash
set -eu
python3 /tmp/fix-lf.py /tmp/list-gguf-tensors.py
export GGUF_PY=/srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/llama-cpp-turboquant-fresh/gguf-py
python3 /tmp/list-gguf-tensors.py /srv/dev-disk-by-uuid-abf69297-f944-4276-b8b1-5ef03fcdc8e8/llama-cpp/models/ornith-model/mtpdraft-Q8_0.gguf
