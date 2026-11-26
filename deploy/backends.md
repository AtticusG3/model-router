# Backend runtime layout

Shared decisions for every backend ticket (transcription, speech, realtime,
video, audio generation) on the GPU host that runs the live config at
`/opt/ai/config/model-router.yaml`. One backend, one environment, one port,
one model directory. A backend ticket that deviates says why in its own
section.

## 1. One environment per backend

Never add a runtime to the shared sidecar venv and never share a venv between
two backends: one backend's dependency bump must not break another's spawn.

| Kind | Where |
| --- | --- |
| Python venv | `/opt/ai/venv-<backend>` |
| Docker image | `mr/<backend>:<version>` |
| Static binary | `/opt/ai/bin` (no venv) |

`<backend>` is one short name used for the venv, the model directory, the hub
cache, the container name and the image tag, so a path can never point at
another backend's files. Examples: `stt`, `tts`, `realtime`, `video`,
`audio-gen`.

## 2. Two torch tracks

Two tracks exist because the cards differ. A wheel built for one track does
not run on the other's older card.

| Track | Wheel index | Pin | Cards it may use |
| --- | --- | --- | --- |
| cu126 | `https://download.pytorch.org/whl/cu126` | `torch==2.7.1+cu126` | the two 32 GB sm_70 cards (GPU 1, GPU 2) and the 24 GB sm_86 card (GPU 0) |
| cu130 | `https://download.pytorch.org/whl/cu130` | `torch==2.12.1+cu130` | the 24 GB sm_86 card (GPU 0) only |

- Anything that must run on the sm_70 cards uses the cu126 track.
- The cu130 track has no sm_70 kernels. On a sm_70 card it fails at kernel
  launch, not at import, so a placement check cannot catch it.
- Do not raise the cu126 pin without checking the arch list; a build can
  drop sm_70 between versions.

Check a freshly built environment in its own venv:

```bash
python -c 'import torch; print(torch.__version__, torch.cuda.get_arch_list())'
```

cu126 must print `sm_70` and `sm_86`; cu130 must print `sm_86`. Put that line
and its output in the backend ticket.

## 3. Weights and the hub cache

- Weights live under `/opt/ai/models/<backend>/`. Checkpoints too large for
  `/opt/ai` may live under `/srv/ssd/models/<backend>/` instead; the stanza
  points at whichever root holds the files. Never share one directory between
  two backends.
- Every Python backend sets `HF_HOME=/opt/ai/hf/<backend>`, via the stanza
  `env` or `-e HF_HOME=/hf` on a docker command. There is no shared or
  user-home `HF_HOME`: two backends sharing one cache fight over lock files
  and a partial download breaks both.
- Download once by hand in the backend environment with `HF_HUB_OFFLINE`
  unset. Once the files are present, add `HF_HUB_OFFLINE=1` the same way; a
  spawn must not touch the network.
- The cache is read-mostly but still takes lock files, so a docker mount for
  `HF_HOME` is read-write even when the weights mount is read-only.

## 4. Foreground launch only

The router spawns the stanza command as a process-group leader and stops it
with SIGTERM to the group, then SIGKILL after 10 seconds. A backend that
daemonises survives that stop and keeps holding GPU memory.

- No `-d`/`--detach`, no `&`, no `nohup`, no launcher that backgrounds the
  server.
- Python: the stanza command is the server itself, not a wrapper that starts
  it in the background.
- Docker: `docker run` without `-d` is the foreground process and forwards the
  signal to the container.

Docker stanza template:

```yaml
  - model_id: <id>
    command: >
      docker run --rm --init --name mr-<id>
      --gpus device={gpu}
      --stop-timeout 5
      -p 127.0.0.1:{port}:{port}
      -v /opt/ai/models/<backend>:/models:ro
      -v /opt/ai/hf/<backend>:/hf
      -e HF_HOME=/hf
      -e HF_HUB_OFFLINE=1
      mr/<backend>:<version>
      --host 0.0.0.0 --port {port}
    device: "0|1|2"
```

- `--gpus device={gpu}` exposes exactly the card the router chose; inside the
  container it is GPU 0. Do not also pass a host index in
  `CUDA_VISIBLE_DEVICES` or `--device CUDA{n}`.
- `--stop-timeout 5` lets the daemon finish the stop before the router's
  10-second SIGKILL; with the default 10 the two race.
- `-p 127.0.0.1:{port}:{port}` publishes the listener the router proxies to.
  The backend binds `0.0.0.0` inside the container (a published port is
  forwarded to the container's network address, not its loopback), while the
  host side stays loopback-only. A non-docker backend binds `127.0.0.1`.
- A shell entrypoint must `exec` the server, or the signal stops the shell and
  the model process is killed only at the stop timeout.

Verify that an unload leaves nothing behind (run on the node):

```bash
nvidia-smi --query-gpu=index,memory.used --format=csv,noheader
curl -s -X POST http://127.0.0.1:18080/_router/unload \
  -H 'Content-Type: application/json' -d '{"model_id":"<id>"}'
sleep 3
docker ps -a --filter 'name=^/mr-<id>$' --format '{{.Names}}'   # empty
nvidia-smi --query-gpu=index,memory.used --format=csv,noheader
nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name --format=csv
```

The unload call returns the router's result. The container listing prints
nothing, the card's used memory returns to the pre-load figure, and the compute
process list holds no process from the container. Record all three in the
backend ticket.

## 5. Ports

Stanza ports are explicit and the router proxies to `127.0.0.1:{port}`.
`port: 0` is auto-allocated from `start_port: 5901`, skipping
`reserved_ports`; extra-instance ports are allocated above every stanza port
and every reserved port.

Ports already taken in the live config:

| Port | Used by |
| --- | --- |
| `18080` | model-router listen |
| `5903, 5904, 5907` | resident 24 GB card stanzas |
| `5915, 5916, 5918, 5919, 5922, 5925, 5927` | image and chat stanzas |
| `18100` | image edit stanza (also listed as reserved) |
| `6333, 8081, 8082, 18095, 18096, 18097, 18099` | sidecars and peer routers in `reserved_ports` |

`5901` is the allocation base; no stanza binds it today.

**Backend block: `5930-5949`.** Reserved so auto-allocation can never hand a
backend port to another model. Add it to `reserved_ports` on the live config:

```yaml
reserved_ports: [8081, 6333, 18095, 18096, 18097, 8082, 18099, 18100,
                 5930, 5931, 5932, 5933, 5934, 5935, 5936, 5937, 5938,
                 5939, 5940, 5941, 5942, 5943, 5944, 5945, 5946, 5947,
                 5948, 5949]
```

Backend tickets take fixed ports from that block, in this order (the research
notes already use them):

| Ports | Type |
| --- | --- |
| `5930-5932` | transcription |
| `5933-5935` | speech |
| `5936-5937` | realtime |
| `5938-5939` | video |
| `5940-5942` | audio generation |
| `5943-5949` | unassigned headroom |

Rules: write `port: <n>` explicitly, never `port: 0`; one port per stanza; a
primary and a fallback for the same type get two ports so they can coexist.
The backend binds `127.0.0.1`; the router owns the public surface.

## 6. Smoke probe

`deploy/probe-backend.sh URL TYPE [MODEL]` checks one backend: the type's
health path, then one tiny request. It prints `PASS ...` and exits 0, or
`FAIL ...` with the HTTP status and the start of the error body.

```bash
# directly on a running backend
./deploy/probe-backend.sh http://127.0.0.1:5930 transcription demo-stt

# through the router (loads the stanza on demand; allow a cold start)
PROBE_TIMEOUT=600 PROBE_HEALTH=/health \
  ./deploy/probe-backend.sh http://127.0.0.1:18080 chat demo-chat
```

`TYPE` is an `api_type`: `chat`, `embedding`, `rerank`, `image`, `speech`,
`transcription`, `realtime`, `video`, `audio_gen` (`decision` is not a probe
type yet; use the curl steps in §8). `MODEL` defaults to
`probe`. `PROBE_HEALTH` overrides the health path (defaults follow the
research notes: `/v1/audio/health` for transcription, `/docs` for realtime,
`/sdapi/v1/sd-models` for video, `/health` otherwise). The tiny request is one
short prompt or 64x64 one-step image, a 0.1 s silent clip for transcription,
and a websocket handshake for realtime. A realtime `101` and any other `2xx`
count as PASS. `video` and `audio_gen` probe the `/v1/videos` and
`/v1/audio/generations` contract, so they only pass once the adapter for that
backend exists; before then probe the native API by hand.

A backend ticket runs this script on the node and pastes the `PASS` line.

## 7. Config mirror: placeholders in git, real values in the overlay

**The repo copy is not byte-identical to the live config, by design (#195).**
Tracked `configs/<node>.yaml` carries placeholder addresses only (LAN as
`10.99.0.x`, tailnet as `203.0.113.x`, last octet preserved). The real per-node
values live in `/opt/ai/config/local/<node>.yaml`, which is untracked (and
`configs/local/` is gitignored) and is merged over the tracked file at load time.

Stanzas are the exception that keeps the old advice: a **stanza** patch goes into
the live `/opt/ai/config/model-router.yaml` first and then into
`configs/<node>.yaml` (with placeholder addresses), because stanzas are the same
on every node and carry no real addresses.

Refreshing from live: use the splitter, never a bare `cp`:

```bash
# writes configs/local/<node>.yaml (real values) and configs/<node>.yaml (scrubbed)
deploy/overlay-split.py --node <node> /opt/ai/config/model-router.yaml

# proves the tracked file has no real address; also runs in go test ./...
deploy/overlay-split.py --node <node> --check configs/<node>.yaml

# validate what the node will actually load (tracked + overlay)
./build/model-router -check -config configs/<node>.yaml \
  --overlay configs/local/<node>.yaml        # prints `overlay: <path>`
```

A bare `cp /opt/ai/config/model-router.yaml configs/<node>.yaml` is now a bug:
it puts real addresses back into git, which is what #184/#195 removed. The
`internal/config` guard test fails the build if one gets committed.

## 8. Decision models

A decision model answers `POST /v1/systemone` (state plus named, typed
questions in, probabilities out). The sources say Kev-9B runs in stock
llama-server from a GGUF with a build that has llama.cpp PR #29818 (October
2026 or newer); that is unverified until the steps below have run. It is
a static binary in `/opt/ai/bin`, so there is no venv. Run the smoke test on
the 24 GB RTX 3090 node: the BF16 file does not fit a 16 GB card.

| Item | Value |
|---|---|
| Card | `jaredpalmer/kev-9b`, revision `b5d8c18e` (v2, Kev 1.0) |
| Weights | BF16 GGUF from `ggml-org/Kev-9B-GGUF`, by hand into `/opt/ai/models/decision/` |
| Stanza | `configs/kev-9b.example.yaml` |
| p50 decision latency | ⚪ not measured yet: fill in after the steps below |
| GGUF revision vs `b5d8c18e` | ⚪ not checked yet |

> **NOTICE:** Stop or evict whatever holds the card first. The router does
> not download weights, so a missing file fails the start, not the request.

```bash
# 1. the build knows decision models (the PR landed 2026-10-02)
/opt/ai/bin/llama-server --version

# 2. start it by hand on a spare port and wait for /health
/opt/ai/bin/llama-server --model /opt/ai/models/decision/Kev-9B-BF16.gguf \
  --port 5960 --host 127.0.0.1 --n-gpu-layers 999 --ctx-size 16384 --parallel 1 &
until curl -sf http://127.0.0.1:5960/health; do sleep 2; done

# 3. one decision (made-up state)
curl -s http://127.0.0.1:5960/v1/systemone -H 'Content-Type: application/json' -d '{
  "model": "kev-9b",
  "state": "Order A-1001 arrived with a cracked lid; the customer asks for their money back.",
  "questions": {
    "intent": {"type": "choice", "options": ["refund", "replace", "other"]},
    "urgent": {"type": "noul", "instructions": "The customer needs an answer today."}
  }}'

# 4. stop the hand-started server, copy the kev-9b stanza into the node
#    config, save step 3's body as decision.json, send it once to load the
#    model, then time 50 warm decisions through the router and take the median
for i in $(seq 50); do
  curl -s -o /dev/null -w '%{time_total}\n' http://127.0.0.1:18080/v1/systemone \
    -H 'Content-Type: application/json' -d @decision.json
done | sort -n | sed -n 25p
```

Write the p50 (seconds, batch 1, warm) and `nvidia-smi` memory use into the
table above and into the
[research page](../docs/research/decision-models.md#latency-claims-and-what-they-measure).
