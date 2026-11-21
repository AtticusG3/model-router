"""OpenAI-shaped audio-generation front end for ACE-Step 1.5's REST server.

The router's `audio_gen` job API expects the OpenAI async-job shape
(`POST /v1/audio/generations`, `GET .../{id}`, `GET .../{id}/content`).
ACE-Step speaks its own task API (`/release_task`, `/query_result`,
`/v1/audio?path=`), so this file is the translation layer and holds no model
of its own: every call goes to the ACE-Step server named by ACESTEP_URL.

One task id per request; status maps 0 reading -> in_progress, 1 -> completed,
2 -> failed.
"""

from __future__ import annotations

import asyncio
import json
import os
import time
import urllib.parse
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, Response

ACESTEP_URL = os.environ.get("ACESTEP_URL", "http://127.0.0.1:5942").rstrip("/")
DEFAULT_STEPS = int(os.environ.get("ACESTEP_STEPS", "8"))
DEFAULT_DURATION = int(os.environ.get("ACESTEP_DEFAULT_SECONDS", "30"))
PRESET = os.environ.get("ACESTEP_PRESET", "ace-step-1.5-turbo")

app = FastAPI(title="ace-step audio_gen shim", version="1.0")
client = httpx.AsyncClient(base_url=ACESTEP_URL, timeout=httpx.Timeout(30.0, read=120.0))

# task_id -> what the client asked for, so a status poll can answer without
# parsing ACE-Step's echoed request metadata.
_meta: dict[str, dict[str, Any]] = {}


def _int(value: Any, default: int) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _status(code: Any) -> str:
    return {0: "in_progress", 1: "completed", 2: "failed"}.get(code, "queued")


@app.get("/health")
async def health() -> dict[str, Any]:
    try:
        r = await client.get("/health")
        upstream = r.json().get("data", {}).get("status") if r.status_code == 200 else None
    except httpx.HTTPError:
        upstream = None
    ok = upstream == "ok"
    return JSONResponse(
        {"status": "ok" if ok else "degraded", "upstream": upstream, "backend": "ace-step", "preset": PRESET},
        status_code=200 if ok else 503,
    )


@app.post("/v1/audio/generations")
async def create(request: Request) -> JSONResponse:
    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(400, "body must be JSON")
    prompt = (body.get("prompt") or body.get("input") or "").strip()
    if not prompt:
        raise HTTPException(400, "prompt is required")
    seconds = _int(body.get("seconds") or body.get("duration"), DEFAULT_DURATION)
    seconds = max(10, min(600, seconds))
    lyrics = body.get("lyrics") or "[Instrumental]"
    payload = {
        "prompt": prompt,
        "lyrics": lyrics,
        "audio_duration": seconds,
        "inference_steps": _int(body.get("num_inference_steps") or body.get("steps"), DEFAULT_STEPS),
        "audio_format": body.get("response_format") or "wav",
        "batch_size": max(1, min(8, _int(body.get("n"), 1))),
    }
    if body.get("seed") not in (None, ""):
        payload["seed"] = _int(body["seed"], 0)
    try:
        r = await client.post("/release_task", json=payload)
    except httpx.HTTPError as exc:
        raise HTTPException(502, f"ace-step unreachable: {exc}")
    if r.status_code != 200:
        raise HTTPException(502, f"ace-step release_task {r.status_code}: {r.text[:300]}")
    data = r.json().get("data") or {}
    task_id = data.get("task_id")
    if not task_id:
        raise HTTPException(502, f"ace-step returned no task id: {r.text[:300]}")
    _meta[task_id] = {"model": body.get("model") or PRESET, "seconds": seconds, "created": int(time.time())}
    doc = _meta[task_id] | {"id": task_id, "object": "audio.generation", "status": "queued"}
    return JSONResponse(doc, status_code=200)


async def _result(task_id: str) -> dict[str, Any] | None:
    r = await client.post("/query_result", json={"task_id_list": [task_id]})
    if r.status_code != 200:
        raise HTTPException(502, f"ace-step query_result {r.status_code}: {r.text[:300]}")
    items = r.json().get("data") or []
    return items[0] if items else None


@app.get("/v1/audio/generations/{task_id}")
async def status(task_id: str) -> JSONResponse:
    try:
        item = await _result(task_id)
    except httpx.HTTPError as exc:
        raise HTTPException(502, f"ace-step unreachable: {exc}")
    if item is None:
        raise HTTPException(404, f"unknown job {task_id}")
    state = _status(item.get("status"))
    meta = _meta.get(task_id, {})
    doc: dict[str, Any] = {
        "id": task_id,
        "object": "audio.generation",
        "model": meta.get("model", PRESET),
        "status": state,
        "seconds": meta.get("seconds"),
        "created_at": meta.get("created", item.get("create_time")),
        "progress": int(float(item.get("progress") or 0.0) * 100),
    }
    if state == "failed":
        doc["error"] = {"code": "generation_failed", "message": (item.get("error") or "ace-step reported a failure")[:500]}
    return JSONResponse(doc)


@app.get("/v1/audio/generations/{task_id}/content")
async def content(task_id: str) -> Response:
    try:
        item = await _result(task_id)
    except httpx.HTTPError as exc:
        raise HTTPException(502, f"ace-step unreachable: {exc}")
    if item is None:
        raise HTTPException(404, f"unknown job {task_id}")
    state = _status(item.get("status"))
    if state != "completed":
        raise HTTPException(409, f"job is {state}")
    # ACE-Step echoes an internal path; fetch it through its own file route.
    file_ref = ""
    try:
        results = json.loads(item.get("result") or "[]")
        if results:
            file_ref = results[0].get("file") or ""
    except json.JSONDecodeError:
        raise HTTPException(502, "ace-step returned an unreadable result")
    if not file_ref:
        raise HTTPException(502, "ace-step completed without a file")
    r = await client.get(file_ref if file_ref.startswith("/") else "/v1/audio?path=" + urllib.parse.quote(file_ref))
    if r.status_code != 200:
        raise HTTPException(502, f"ace-step audio fetch {r.status_code}")
    ctype = r.headers.get("content-type", "audio/wav")
    return Response(content=r.content, media_type=ctype, headers={"Content-Length": str(len(r.content))})


@app.delete("/v1/audio/generations/{task_id}")
async def delete(task_id: str) -> JSONResponse:
    # ACE-Step has no cancel route for a single task; the router ends its own
    # hold and the server finishes the job it already started.
    _meta.pop(task_id, None)
    return JSONResponse({"id": task_id, "object": "audio.generation", "deleted": True})


@app.get("/v1/models")
async def models() -> dict[str, Any]:
    return {"object": "list", "data": [{"id": PRESET, "object": "model", "owned_by": "ace-step"}]}
