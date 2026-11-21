#!/bin/bash
# Probe one backend. Usage: probe-backend.sh URL TYPE [MODEL]
# TYPE is an api_type: chat embedding rerank image speech transcription
# realtime video audio_gen. MODEL defaults to "probe".
# Prints PASS or FAIL and exits 0 or 1.
#
# PROBE_HEALTH overrides the health path; PROBE_TIMEOUT is the per-request
# limit in seconds (default 300, enough for a cold load through the router).
set -u

URL=${1:?usage: probe-backend.sh URL TYPE [MODEL]}
TYPE=${2:?usage: probe-backend.sh URL TYPE [MODEL]}
MODEL=${3:-probe}
URL=${URL%/}
TIMEOUT=${PROBE_TIMEOUT:-300}
TMP=$(mktemp -d) || exit 1
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "FAIL $*"
  exit 1
}

# request METHOD PATH [curl args...]; sets CODE and $TMP/body.
request() {
  local method=$1 path=$2
  shift 2
  CODE=$(curl -sS -o "$TMP/body" -w '%{http_code}' --max-time "$TIMEOUT" \
    -X "$method" "$@" "$URL$path" 2>"$TMP/err") ||
    fail "$TYPE $method $path: $(head -c 200 "$TMP/err")"
}

case $TYPE in
  chat|embedding|rerank|image|speech|transcription|realtime|video|audio_gen) ;;
  *) fail "unknown type $TYPE" ;;
esac

case $TYPE in
  transcription) HEALTH=${PROBE_HEALTH:-/v1/audio/health} ;;
  realtime) HEALTH=${PROBE_HEALTH:-/docs} ;;
  video) HEALTH=${PROBE_HEALTH:-/sdapi/v1/sd-models} ;;
  *) HEALTH=${PROBE_HEALTH:-/health} ;;
esac

request GET "$HEALTH"
case $CODE in
  2??) ;;
  *) fail "$TYPE health $HEALTH: http $CODE $(head -c 200 "$TMP/body")" ;;
esac
HEALTH_CODE=$CODE

json='Content-Type: application/json'
case $TYPE in
  chat)
    request POST /v1/chat/completions -H "$json" \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1,\"stream\":false}"
    ;;
  embedding)
    request POST /v1/embeddings -H "$json" \
      -d "{\"model\":\"$MODEL\",\"input\":\"ping\"}"
    ;;
  rerank)
    request POST /v1/rerank -H "$json" \
      -d "{\"model\":\"$MODEL\",\"query\":\"ping\",\"documents\":[\"ping\",\"pong\"]}"
    ;;
  image)
    request POST /sdapi/v1/txt2img -H "$json" \
      -d '{"prompt":"ping","steps":1,"width":64,"height":64,"cfg_scale":1}'
    ;;
  speech)
    # A checkpoint with no preset voices (Qwen3-TTS Base) rejects a stock OpenAI
    # voice name with "does not support task_type='CustomVoice'", so a deployed
    # clone backend is probed with a voice it accepts: PROBE_VOICE=<library id>.
    request POST /v1/audio/speech -H "$json" \
      -d "{\"model\":\"$MODEL\",\"input\":\"ping\",\"voice\":\"${PROBE_VOICE:-alloy}\"}"
    ;;
  transcription)
    python3 - "$TMP/probe.wav" <<'PY' || fail "$TYPE needs python3 to write the probe clip"
import sys
import wave

with wave.open(sys.argv[1], "wb") as w:
    w.setnchannels(1)
    w.setsampwidth(2)
    w.setframerate(16000)
    w.writeframes(b"\x00\x00" * 1600)
PY
    request POST /v1/audio/transcriptions \
      -F "file=@$TMP/probe.wav" -F "model=$MODEL"
    ;;
  realtime)
    # A 101 switches protocols, so curl reports an empty reply when the probe
    # stops reading; the status line is the whole check.
    CODE=$(curl -sS -o "$TMP/body" -w '%{http_code}' --max-time 5 \
      -H 'Connection: Upgrade' \
      -H 'Upgrade: websocket' \
      -H 'Sec-WebSocket-Version: 13' \
      -H "Sec-WebSocket-Key: $(head -c 16 /dev/urandom | base64)" \
      "$URL/v1/realtime?model=$MODEL" 2>"$TMP/err") || true
    ;;
  video)
    request POST /v1/videos -H "$json" \
      -d "{\"model\":\"$MODEL\",\"prompt\":\"ping\",\"seconds\":1}"
    ;;
  audio_gen)
    request POST /v1/audio/generations -H "$json" \
      -d "{\"model\":\"$MODEL\",\"prompt\":\"ping\",\"duration\":1}"
    ;;
esac

if [ "$TYPE" = realtime ]; then
  [ "$CODE" = 101 ] || fail "$TYPE upgrade: http $CODE $(head -c 200 "$TMP/err") $(head -c 200 "$TMP/body")"
else
  case $CODE in
    2??) ;;
    *) fail "$TYPE: http $CODE $(head -c 200 "$TMP/body")" ;;
  esac
fi

if [ "$TYPE" = speech ] && [ ! -s "$TMP/body" ]; then
  fail "$TYPE: empty audio body"
fi

echo "PASS health=$HEALTH_CODE $TYPE=$CODE"
