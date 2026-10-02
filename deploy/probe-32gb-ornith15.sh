#!/bin/bash
set -u
echo "HOST $(hostname)"
nvidia-smi --query-gpu=index,name,memory.used,memory.total --format=csv,noheader
df -h /home/kevyn/models /opt/ai/models/llm / 2>/dev/null || df -h
ls -l /home/kevyn/models/Ornith-1.5* /home/kevyn/models/mtpdraft* 2>/dev/null || true
ls -l /opt/ai/models/llm/Ornith-1.5* /opt/ai/models/llm/mtpdraft* 2>/dev/null || true
echo "--- router ---"
curl -sf http://127.0.0.1:18080/health || curl -sf http://127.0.0.1:8081/health || echo no-health
curl -sf http://127.0.0.1:18080/_router/status || curl -sf http://127.0.0.1:8081/_router/status || true
