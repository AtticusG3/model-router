#!/usr/bin/env python3
"""OpenAI-shaped speech front end for KittenTTS 2.

The router's `speech` stanzas speak `POST /v1/audio/speech` and, for a voice
that names a record in its library, hand the backend a reference inline:
`ref_audio` as a `data:audio/wav;base64,...` URI plus `ref_text`. KittenTTS 2
has no HTTP server of its own (the `kittenml` package is a library, and the
upstream `kitten-tts-2-cpp` fork is a CLI), so this is the adapter — one file,
no framework beyond FastAPI, and it speaks exactly the two shapes the router
and the Playground already use.

Precedence for the speaker, matching the router's own contract:
  1. `ref_audio` (a clone) — also accepts a bare base64 blob or a file path.
  2. `voice` naming one of the 115 built-in voices.
  3. Neither: KittenTTS 2's own default voice.

`KITTEN_MODEL_DIR` points at a local snapshot so the service never touches the
network on a cold start; `KITTEN_DEVICE=cuda` keeps the 954 MB ternary body on
the 3090, where a load is ~60 s and a warm sentence is a few seconds.
"""
import base64
import io
import json
import os
import re
import subprocess
import tempfile
import threading
import time

import numpy as np
import soundfile as sf
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response

MODEL_DIR = os.environ.get("KITTEN_MODEL_DIR", "/opt/ai/models/tts/kitten-tts-2")
MODEL_ID = os.environ.get("KITTEN_MODEL_ID", "kitten-tts-2")
DEVICE = os.environ.get("KITTEN_DEVICE", "cuda")
DEFAULT_PRESET = os.environ.get("KITTEN_PRESET") or None

DATA_URI = re.compile(r"^data:[^,]*;base64,", re.I)

app = FastAPI(title="kitten-tts-shim", version="1.0")
_model = None
_load_lock = threading.Lock()
_gen_lock = threading.Lock()


def model():
    """Load once. The first call pays ~60 s; the router's spin_up_seconds covers it."""
    global _model
    if _model is None:
        with _load_lock:
            if _model is None:
                from kittenml import KittenTTS

                t0 = time.time()
                _model = KittenTTS(MODEL_DIR, device=DEVICE)
                print(json.dumps({"event": "loaded", "model": MODEL_ID,
                                  "seconds": round(time.time() - t0, 2),
                                  "sample_rate": _model.sample_rate,
                                  "voices": len(_model.available_voices)}), flush=True)
    return _model


def _error(msg, code=400, kind="invalid_request_error"):
    return JSONResponse(status_code=code,
                        content={"error": {"message": msg, "type": kind, "code": code}})


@app.get("/health")
def health():
    m = model()
    return {"status": "ok", "model": MODEL_ID, "device": DEVICE,
            "sample_rate": m.sample_rate, "voices": len(m.available_voices)}


@app.get("/v1/audio/voices")
def voices():
    m = model()
    return {"object": "list",
            "data": [{"id": v, "object": "voice", "builtin": True} for v in m.available_voices]}


@app.get("/v1/models")
def models():
    return {"object": "list", "data": [{"id": MODEL_ID, "object": "model"}]}


def _reference_path(ref):
    """Turn the router's injected reference into a file KittenTTS can read."""
    if not isinstance(ref, str) or not ref.strip():
        return None
    ref = ref.strip()
    if DATA_URI.match(ref):
        raw = base64.b64decode(ref.split(",", 1)[1])
    elif os.path.exists(ref):
        return ref
    else:
        try:
            raw = base64.b64decode(ref, validate=True)
        except Exception:
            return None
    fd, path = tempfile.mkstemp(suffix=".wav", prefix="kitten-ref-")
    with os.fdopen(fd, "wb") as fh:
        fh.write(raw)
    return path


def _encode(wav, sample_rate, fmt):
    buf = io.BytesIO()
    if fmt in ("mp3", "opus", "aac", "flac"):
        sf.write(buf, wav, sample_rate, format="WAV", subtype="PCM_16")
        buf.seek(0)
        args = ["ffmpeg", "-v", "error", "-i", "pipe:0", "-f", "mp3" if fmt == "mp3" else fmt,
                "-"]
        proc = subprocess.run(args, input=buf.getvalue(), capture_output=True)
        if proc.returncode != 0:
            raise RuntimeError(proc.stderr.decode()[:300])
        return proc.stdout, "mp3" if fmt == "mp3" else fmt
    sf.write(buf, wav, sample_rate, format="WAV", subtype="PCM_16")
    return buf.getvalue(), "wav"


@app.post("/v1/audio/speech")
async def speech(request: Request):
    try:
        body = await request.json()
    except Exception:
        return _error("body must be JSON")
    text = (body.get("input") or body.get("text") or "").strip()
    if not text:
        return _error("'input' is required")
    fmt = (body.get("response_format") or "wav").lower()
    voice = body.get("voice")
    if isinstance(voice, str):
        voice = voice.strip() or None
    else:
        voice = None
    ref = body.get("ref_audio") or body.get("reference")
    ref_text = body.get("ref_text") or body.get("reference_text") or None
    # The router writes "language?" into the payload for some clone modes; a
    # language hint means nothing to KittenTTS 2 (the voice carries the accent).
    preset = body.get("preset") or DEFAULT_PRESET

    m = model()
    known = set(m.available_voices)
    ref_path = _reference_path(ref)
    kwargs = {}
    if ref_path:
        kwargs["reference"] = ref_path
        if ref_text:
            kwargs["reference_text"] = ref_text
    elif voice in known:
        kwargs["voice"] = voice
    elif voice:
        return _error(f"unknown voice {voice!r}: this backend takes a library clone "
                      f"(ref_audio) or one of its {len(known)} built-in voices")
    if preset:
        kwargs["preset"] = preset

    t0 = time.time()
    with _gen_lock:  # one GPU, one generation at a time
        try:
            wav = m.generate(text, **kwargs)
        except Exception as exc:  # surface the engine's message instead of a bare 500
            return _error(f"generation failed: {exc}", 500, "server_error")
    wav = np.asarray(wav, dtype=np.float32)
    if wav.ndim > 1:
        wav = wav.mean(axis=1) if wav.shape[1] > 1 else wav.reshape(-1)
    try:
        payload, kind = _encode(wav, m.sample_rate, fmt)
    except Exception as exc:
        return _error(f"encode failed: {exc}", 500, "server_error")
    audio_s = len(wav) / float(m.sample_rate)
    print(json.dumps({"event": "speech", "chars": len(text), "audio_s": round(audio_s, 2),
                      "seconds": round(time.time() - t0, 2),
                      "rtf": round((time.time() - t0) / audio_s, 3) if audio_s else None,
                      "clone": bool(ref_path), "voice": voice, "format": kind}), flush=True)
    if ref_path and ref_path.startswith(tempfile.gettempdir()):
        try:
            os.unlink(ref_path)
        except OSError:
            pass
    return Response(content=payload, media_type=f"audio/{kind}")
