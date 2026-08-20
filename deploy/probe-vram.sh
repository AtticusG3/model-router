#!/bin/bash
# Probe one llama-server config. Usage: probe-vram.sh GPU_INDEX TIMEOUT args...
# Prints: OK used=<mb> delta=<mb>   or LOSE <reason>
set -u
SMI=${1:?gpu index}
TIMEOUT=${2:?seconds}
shift 2
PORT=${PROBE_PORT:-18999}
ulimit -l unlimited 2>/dev/null || true

used() {
  nvidia-smi --id="$SMI" --query-gpu=memory.used --format=csv,noheader,nounits | awk '{print int($1)}'
}

pkill -f "llama-server.*--port ${PORT}" 2>/dev/null || true
fuser -k "${PORT}/tcp" 2>/dev/null || true
sleep 2
BASE=$(used)

export CUDA_VISIBLE_DEVICES="${CUDA_VISIBLE_DEVICES:-$SMI}"
"$@" --port "$PORT" --host 127.0.0.1 >/tmp/probe-vram.server.log 2>&1 &
PID=$!
cleanup() {
  kill "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  pkill -f "llama-server.*--port ${PORT}" 2>/dev/null || true
  sleep 2
}
trap cleanup EXIT

ok=0
for _ in $(seq 1 "$TIMEOUT"); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "LOSE dead (see /tmp/probe-vram.server.log)"
    exit 1
  fi
  if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 1
done
if [ "$ok" != 1 ]; then
  echo "LOSE timeout ${TIMEOUT}s"
  exit 1
fi
NOW=$(used)
DELTA=$((NOW - BASE))
if [ "$DELTA" -lt 0 ]; then
  DELTA=0
fi
echo "OK used=${NOW} delta=${DELTA} base=${BASE}"
exit 0
