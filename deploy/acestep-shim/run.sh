#!/usr/bin/env bash
# Starts ACE-Step's REST server on a private port and then serves the
# OpenAI-shaped audio_gen shim on $PORT (the port the router allocated).
# Both processes are in this process group, so the router's group kill takes
# the pair down together.
set -euo pipefail

: "${PORT:?PORT must be set by the router}"

export CUDA_DEVICE_ORDER=PCI_BUS_ID
export CUDA_VISIBLE_DEVICES=0
export HF_HOME=/opt/ai/hf/acestep
export ACESTEP_CHECKPOINTS_DIR=/opt/ai/ace-step-1.5/checkpoints
export ACESTEP_CONFIG_PATH=acestep-v15-turbo
export ACESTEP_LM_MODEL_PATH=acestep-5Hz-lm-1.7B
export ACESTEP_INIT_LLM=true
export ACESTEP_LM_BACKEND=pt
export ACESTEP_OUTPUT_DIR=/opt/ai/ace-step-1.5/outputs

cd /opt/ai/ace-step-1.5
.venv/bin/acestep-api --host 127.0.0.1 --port 5942 >>/tmp/acestep-api.log 2>&1 &

cd /home/kevyn/projects/model-router
exec env ACESTEP_URL=http://127.0.0.1:5942 \
  /opt/ai/ace-step-1.5/.venv/bin/python -m uvicorn \
  --app-dir deploy/acestep-shim server:app \
  --host 127.0.0.1 --port "$PORT"
