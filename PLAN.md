# Model Router — v0.1 Implementation Plan

This plan is the result of probing the fleet (buster, digger, gareths-homelab, nomad;
nugget offline) and comparing reality against `SPEC.md`. It records what was found,
the gaps/pitfalls in the SPEC, the design decisions that fit this network, and the
deployment path. The SPEC remains the source of truth for goals; this document
records the deltas and the concrete build.

---

## 1. Fleet profile (from live probes, 2026-08-18)

| Node | IP (reach) | GPU(s) | RAM | Disk free | llama-swap | Listen | Role / stanzas |
|------|-----------|--------|-----|-----------|------------|--------|----------------|
| **buster** | 192.168.1.61 (local) | Tesla V100-SXM2-32GB (CUDA1) + RTX 4060 8GB (CUDA0) | 125 Gi | 1.2 TB | v228, `0.0.0.0:18080` | 18080 | Heavyweight: chat, embedding, rerank, image. Stanzas pin `--device CUDA0/CUDA1` |
| **digger** | 192.168.1.36 (root) | RTX PRO 4000 Blackwell 24 GB | 31 Gi | 73 GB (models on /srv data disks) | v (sd config), `0.0.0.0:8082` | 8082 | Image node: ~19 sd.cpp stanzas (Krea2 x5, LTX-2.3, MiniMax-H3, SDXL, Flux2, SD3.5, Z-Image…) + `sdcpp-webui.service` |
| **gareths-homelab** | 100.89.49.44 (root, Tailscale) | RTX 5060 Ti 16 GB | 78 Gi | 58 GB `/` (models on data disks: 251G llama + 54G sd) | v, `127.0.0.1:18080`, nginx `:8081`(LLM)/`:8082`(image)→18080 | 18080 (internal) | LLM + image: llm-* (Laguna, Qwen3.6-35B, Gemma4-26B, Bonsai-27B, daily-driver, coding-model, intent-router, ornith, qwen36-fast/reap…) + sd (sdxl-lightning, photo-edit, z-anime, juggernaut-z, z-image, flux2-klein) + **openrouter peers** |
| **nomad** | 192.168.1.202 (kevyn) | RTX A2000 Laptop 8 GB | 14 Gi | 19 GB | v, `0.0.0.0:8081` | 8081 | Single model: `qwen3.5-9b` (unlisted), TurboQuant llama-server |
| **nugget** | 192.168.1.62 | — | — | — | **offline (no route)** | 8081 | Was a big node: grug-27b/35b, muse-glimmer-30b, qwen3.6-35b, qwopus-35b-coder, agents-a1, gemma-4-12b/26b, jina-reranker, qwen3-embedding-4b… |

### Current llama-swap behaviour observed
- **Model extraction is body/query-only.** llama-swap reads `model` from the JSON
  body, multipart form, or `?model=` query. There is **no path-prefix routing and no
  default-model fallback** — a request without a resolvable `model` returns
  `404 no model id could be identified`. So today, every sd.cpp request on digger
  must carry `model` in the body. This is the single biggest behavioural gap the
  SPEC's `match.path_prefix` must solve (and it must be done carefully — see §3).
- `/v1/models` lists local models, **selectors** (`meta.llamaswap.type=selector`),
  and **peers** (`type=peer`, id `peer/model`). Clients (rag-proxy) depend on this.
- Selectors use `strategy: spillover` with `targets` (local ids and `peer/model`
  ids). buster also uses llama-swap's `matrix` DSL (vars/sets/evict_costs) — that is
  an eviction-policy feature the SPEC defers; v0.1 does not port it.
- Peers are static `proxy: http://ip:port` + `models:` lists. Nomad/digger point at
  buster `:8081` which is **rag-proxy** (python), not llama-swap directly — rag-proxy
  forwards to `:18080`. The router will let peers talk directly to the router port.

### Backend binaries in use
- llama-server: stock `llama.cpp` and **TurboQuant** builds (`llama-cpp-turboquant`),
  plus `llama-server.wrapper` on digger.
- sd-server (`stable-diffusion.cpp`), `colibri-server` (buster), `sd-health-proxy` (buster).
- Python services on buster that must not be disturbed: **rag-proxy** (`:8081`),
  reranker proxy (`:18095`), sparse index (`:18096`), turbovec (`:18097`), qdrant (`:6333`).
- gareth: nginx `:8081`/`:8082` → `127.0.0.1:18080`; **must disable response buffering
  for SSE** (llama-swap sets `X-Accel-Buffering: no`; router must do the same).

---

## 2. Pitfalls & gaps in SPEC vs. fleet

1. **Multi-GPU placement (buster) — SPEC misses it.** `vram_mb` is a single number but
   buster has 2 GPUs and every stanza pins `--split-mode none --device CUDA0|CUDA1`.
   Admission control must be **per-GPU** and telemetry must report **per-GPU free VRAM**.
   → Add `device` to the stanza; admission ledger keyed by GPU index.
   (SPEC's open question is about tensor-split across two V100s — not needed yet; this
   is per-GPU placement, which is required *now*.)
2. **Pools/selectors are required, SPEC omits them.** rag-proxy references
   `EMBED_MODEL=embed-pool`, `INTENT_MODEL=intent-model`, and routes chat to
   `daily-driver`; Open WebUI/Hermes hit `coding-pool`/`daily-driver`. Without
   selectors the router breaks every client. → Add `pools` with `spillover` semantics
   and expose them in `/v1/models` as `type=selector`.
3. **`match.path_prefix` is ambiguous for 15 sd stanzas on digger.** All sd.cpp
   stanzas share `/sdapi/v1`. A bare path prefix matches all of them. Need:
   (a) `body_field` wins when the body carries `model`; (b) otherwise a **default**
   stanza per path prefix; (c) explicit `path_prefix` + `path_default: true` to pick
   the fallback. Keep llama-swap's body-first behaviour so existing clients keep working.
4. **`api_type` lacks `rerank`.** buster runs jina-reranker (`--reranking`) and
   rag-proxy hits it via a separate `:18095` proxy. The router should still model
   `rerank` as an api_type (cheap), even though the fleet's reranker path is external.
5. **`idle_ttl_seconds` needs a "never" value.** Fleet uses `ttl: -1` (keep forever)
   for the always-resident set (intent, embedding, reranker, agents-a1) and `0` to mean
   never-unload in llama-swap. SPEC's field must support `0 = never`.
6. **Preload on startup.** gareth preloads `llm-gemma-waldron`; buster keeps its
   resident set loaded. → Add `preload` list to config.
7. **`unlisted` flag.** nomad's `qwen3.5-9b` is hidden from `/v1/models`. → Add.
8. **Port allocation must dodge the buster sidecar ecosystem.** Dynamic startPort is
   risky next to rag-proxy(8081), reranker(18095), sparse(18096), turbovec(18097),
   qdrant(6333), sdcpp-webui, colibri-server, sd-health-proxy. → Explicit `port` per
   stanza (fixed), validated against a reserved-port list at startup.
9. **Peer wiring currently goes through rag-proxy/nginx.** Peers must talk to the
   *router* port directly for control (`load()`), not through rag-proxy. peers.yaml
   will point at router ports (buster:18080, nomad:8081, digger:8082, gareth:8081 via
   nginx). gareth's nginx needs SSE-safe proxy settings (already partially present).
10. **OpenRouter peers (gareth).** gareth's config lists `openrouter/...` peers —
    plain OpenAI-compatible upstreams with no control endpoint. peers.yaml must
    support `kind: openai` (proxy-only) vs `kind: router` (has control API).
11. **Nugget offline.** Offline peers must be excluded via telemetry staleness (SPEC
    covers this) and requests must fall through with a clear 503. Also: buster's
    selectors reference `nugget/...` targets that will never resolve — the pool
    resolver must skip unreachable peers, not fail the whole selector.
12. **VRAM numbers are estimates.** `vram_mb` reserves worst-case; actual usage differs
    (agents-a1 is a 22 GB file with `-ngl 30`). Treat `vram_mb` as the reservation and
    log actual nvidia-smi deltas for later calibration. Buster's 4060 is nearly full
    today (956 MB free) — admission must handle "no room" gracefully.
13. **Streaming/SSE.** Reverse proxy must flush immediately and set
    `X-Accel-Buffering: no`; long image-gen and slow spin-ups (colibri) need long
    timeouts and must not be killed by a client disconnect mid-spin.
14. **Single-binary requirement vs toolchain.** No Go exists on any node. Resolution:
    portable Go toolchain in `~/go-toolchain` (already installed), static build, binary
    copied to `/opt/ai/bin/model-router` per node. No system-wide installs.

---

## 3. Fitted architecture (deltas to SPEC)

All five SPEC components are implemented in one Go binary (`cmd/model-router`),
stdlib + `gopkg.in/yaml.v3`. Process-supervision patterns are adapted from
llama-swap and credited in source comments where copied.

### Stanza catalog (extended SPEC schema)
```yaml
- model_id: agents-a1
  name: "Agents-A1 ... (agentic coding)"
  command: "/home/kevyn/infra/llama.cpp/build/bin/llama-server -m {model} --port {port} ..."
  vram_mb: 22000
  device: CUDA1            # GPU index/name; empty = auto (largest free)
  spin_up_seconds: 40
  api_type: chat           # chat | embedding | image | rerank
  match:
    body_field: model      # OpenAI-style
    # OR
    path_prefix: /sdapi/v1 # sd.cpp-style; optional path_default: true
  health_check: /health
  idle_ttl_seconds: 0      # 0 = never unload
  unlisted: false
  env: []                  # optional env vars
  port: 5901               # explicit (recommended); else auto from start_port
```

### Pools (selectors), peers
```yaml
pools:
  coding-pool:
    strategy: spillover
    targets: [qwen38-27b, digger/coding-model, gareths-homelab/llm-coding-model-waldron]
    spillover: 1
  embed-pool:
    strategy: spillover
    targets: [qwen3-embedding-4b]
    spillover: 1
peers:
  - name: digger
    kind: router            # router (control API) | openai (proxy-only)
    base_url: http://192.168.1.36:8082
    models: [coding-model, daily-model, intent-model, ornith-model]
preload: [intent-model, qwen3-embedding-4b, jina-reranker-v3.5]
start_port: 5900
listen: 0.0.0.0:18080
telemetry:
  poll_seconds: 2
  peer_sync_seconds: 5
  peer_stale_seconds: 15
```

### Request flow (SPEC §5, with pools + path default)
1. Matcher resolves model id: body `model` → query `model` → pool id → `path_prefix`
   match → `path_default` stanza. Unknown → 404 (llama-swap-compatible message).
2. Pool id → resolve per strategy (spillover: first target with capacity, else next).
3. Local first (SPEC step 3–4): loaded → proxy; in catalog + GPU room → admission →
   spawn → health → proxy.
4. Peer fallback (SPEC step 5): cached telemetry picks candidate → `POST /_router/load`
   → candidate runs its own admission → transparent streamed reverse proxy.
5. No capacity → 503 with reason (never silent hang).

### Admission control (SPEC §, per-GPU)
- Per-GPU reservation ledger, mutex-serialized, owned only by the local node.
- Reserve at `load()` time; release on unload / failed spin-up.
- Peer `load()` calls go through the same ledger → no split-brain double-booking.
- GPU pinned by `device`; unpinned stanzas pick the GPU with most free VRAM.

### Telemetry
- `nvidia-smi --query-gpu=index,name,memory.total,memory.free --format=csv,noheader`
  every 2 s → per-GPU free VRAM + loaded model.
- Peer sync: push/pull of `{free_vram_per_gpu, loaded_models, ts}` every 5 s.
- Stale > 15 s → treat peer as unknown → exclude from candidate selection.

### HTTP API
- OpenAI passthrough: `/v1/chat/completions`, `/v1/completions`, `/v1/responses`,
  `/v1/embeddings`, `/v1/models`, `/v1/images/generations`, `/v1/images/edits`,
  `/v1/messages`, `/v1/rerank`, `/rerank`, `/infill`, `/completion`.
- sd.cpp: `/sdapi/v1/*`.
- Router control: `POST /_router/load`, `POST /_router/unload`, `GET /_router/status`
  (running models + telemetry), `GET /_router/telemetry` (peer-facing), `GET /health`
  (`OK`), `GET /metrics` (basic Prometheus text).
- `/v1/models` mirrors llama-swap shape: local models + `type=selector` pools +
  `type=peer` peers, each with `status.value` (loaded/unloaded/loading).

---

## 4. Build & test

- `~/go-toolchain/go/bin/go build -trimpath -ldflags="-s -w" -o build/model-router ./cmd/model-router`
- Unit tests: matcher (body/path/default), admission ledger (per-GPU, serialized),
  pool spillover, config parsing.
- Smoke test on buster on a **spare port** with a `fake-model` backend (serves
  `/health`, `/v1/chat/completions`, `/sdapi/v1/txt2img`) to exercise spawn → health →
  proxy → TTL unload → peer load without touching real GPUs or the running llama-swap.

## 4b. Implementation status (2026-08-18)

Implemented and verified locally (two-router smoke test on spare ports with fake
backends; real llama-swap untouched):

- ✅ Matcher: body/query model → pool → peer-qualified → path_prefix with
  path_default fallback (llama-swap has no path routing at all — 404s).
- ✅ Admission ledger: per-GPU reservation, device pinning, auto-pick, release on
  unload/failed spin-up; serialized (no split-brain).
- ✅ Supervisor: spawn (process-group aware), health poll to spin-up deadline,
  crash/restart with backoff, idle-TTL unload, SIGTERM→SIGKILL stop.
- ✅ Telemetry: per-GPU nvidia-smi poller (resilient to one sick GPU), peer
  sync with staleness → peer excluded.
- ✅ Pools (selectors) with spillover; peers with `kind: router` vs
  `kind: openai` (openrouter); `aliases` so peer refs like `digger/coding-model`
  resolve to the node's real stanza.
- ✅ OpenAI + sdapi proxy with SSE flushing; `/v1/models` llama-swap-shaped;
  `/_router/{load,unload,status,telemetry}`, `/health`, `/metrics`; `-check`.
- ✅ Staged configs for all five nodes (configs/, including nugget) + systemd
  units + swap-over procedure (deploy/), all validated with `-check`.

Bugs found and fixed during the smoke test (all real, fleet-relevant):

1. **Peer request carried the qualified id**: `digger/remote-model` was
   forwarded to digger unchanged, which doesn't know that id → 404. Fixed by
   rewriting the body's `model` field to the peer-local id before proxying
   (same as llama-swap's ReplaceRequestModel).
2. **Request-scoped context killed backends**: the spawned process was bound to
   the peer `/_router/load` request context; when the request completed, Go
   SIGKILLed the `sh` wrapper, orphaning the real backend on the port → the
   supervisor crash-restarted forever. Fixed by decoupling the process from
   the request context (it lives until Stop/unload).
3. **Health check against a foreign process**: with an orphan on the port, the
   health poll could pass against the wrong process. Fixed by verifying the
   spawned PID is alive (Signal 0) before marking running.
4. **nvidia-smi one-bad-GPU blindness**: buster's 4060 currently returns
   "Unable to determine the device handle"; the old combined query failed
   wholesale. Poller now enumerates via `-L` and queries per-GPU, keeping
   whatever succeeds.
5. **Startup race**: preload ran before the first GPU poll, so admission
   failed. First poll is now synchronous before preload.

Remaining v0.1 gaps to accept before rollout (also in deploy/swap-over.md):

- In-flight work is never preempted. Idle residents (including ttl=0) can be
  evicted last-resort; otherwise the request spills to a peer with the same
  `model_id`.
- `vram_mb` values are measured (2026-08-20 walks) then rounded up; re-check
  after swap.
- Staged only: swap procedure in deploy/swap-over.md is the executable plan.
  Nugget now has `configs/nugget.yaml` + `deploy/model-router.nugget.service`.

## 5. Deployment (staged — NOT executed this pass)

Per node, write config to `/opt/ai/config/model-router.yaml` and
`/opt/ai/config/peers.yaml`, copy the binary to `/opt/ai/bin/model-router`, install a
systemd unit, and swap. Full per-node procedure in `deploy/swap-over.md`; systemd
units in `deploy/`. The swap-over keeps llama-swap as a rollback (unit
`llama-swap.service` is only stopped after the router passes a health check on the
same port).

Order of rollout (least disruptive first): **nomad** (single local model;
Chat lab probes mesh peers) → **gareths-homelab** (nginx in front, easy
rollback) → **digger** (image node) → **nugget** (V100, kevyn unit) →
**buster** (last; rag-proxy depends on :18080, so buster swap is the
riskiest and needs the most pre-flight).
