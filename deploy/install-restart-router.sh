#!/bin/bash
set -eu
python3 /tmp/fix-lf.py /tmp/model-router.yaml
sudo install -m 0644 /tmp/model-router.yaml /opt/ai/config/model-router.yaml
sudo /opt/ai/bin/model-router -check -config /opt/ai/config/model-router.yaml
sudo systemctl restart model-router.service
sleep 2
curl -sf http://127.0.0.1:18080/health || curl -sf http://127.0.0.1:8081/health
echo
curl -sf http://127.0.0.1:18080/v1/models || curl -sf http://127.0.0.1:8081/v1/models
echo
