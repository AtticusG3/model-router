#!/bin/bash
# Unload intent, walk Ornith 1.5, reload intent.
set -eu
curl -sf -X POST http://127.0.0.1:18080/_router/unload \
  -H 'Content-Type: application/json' \
  -d '{"model_id":"qwen2.5-1.5b-instruct"}' || true
sleep 4
echo "GPU after unload:"
nvidia-smi --query-gpu=memory.used,memory.free --format=csv
bash /tmp/walk-ornith15.sh
echo "WALK_EXIT=$?"
curl -sf -X POST http://127.0.0.1:18080/_router/load \
  -H 'Content-Type: application/json' \
  -d '{"model_id":"qwen2.5-1.5b-instruct"}' || true
echo "RELOADED_INTENT"
