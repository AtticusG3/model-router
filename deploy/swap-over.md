# Fleet swap-over: llama-swap → model-router (v0.1)

STAGED — nothing here has been executed. Every step below is the procedure to
run per node, in order. Each node's old llama-swap stays installed as rollback
(`systemctl stop llama-swap.service` is only executed after the router passes
its health checks on the same port).

Rollout order (least disruptive first):

1. **nomad** (single local model, 8 GB; use `/ui/` Chat lab to probe mesh peers)
2. **gareths-homelab** (nginx fronts the router; trivial rollback)
3. **digger** (image node; sdcpp-webui runs alongside, untouched)
4. **nugget** (V100; unit is `deploy/model-router.nugget.service`)
5. **buster** (LAST — rag-proxy, Open WebUI, Hermes depend on :18080)

---

## Per-node steps (run on the node, as root)

```bash
# 0. Pre-flight: confirm the ports the router will take over are the ones
#    llama-swap currently owns, and that no sidecar collides.
ss -tlnp | grep -E ':(18080|8081|8082)\b'   # per-node port from the table below

# 1. Install the binary + configs.
install -m 0755 build/model-router /opt/ai/bin/model-router
install -m 0644 configs/<node>.yaml /opt/ai/config/model-router.yaml
# (peers are inline in the config for v0.1; the SPEC's separate peers.yaml can
#  be introduced later without schema changes)
# nomad also needs configs/qwen-fixed-chat-template.jinja → /opt/ai/config/

# 2. Validate.
/opt/ai/bin/model-router -check -config /opt/ai/config/model-router.yaml

# 3. Install the unit (llama-swap.service is NOT touched).
install -m 0644 deploy/model-router.service /etc/systemd/system/
systemctl daemon-reload

# 4. Rollback safety: stop llama-swap BEFORE the router starts, so both never
#    fight over the same port/GPU. Do this and step 5 back-to-back.
systemctl stop llama-swap.service
systemctl start model-router.service

# 5. Verify (adjust port per node):
curl -s http://127.0.0.1:18080/health                 # OK
curl -s http://127.0.0.1:18080/v1/models | head -c 400 # unique mesh model_ids
curl -s http://127.0.0.1:18080/_router/status          # loaded models + telemetry

# 6. Smoke-test the real models (per-node list below), e.g.:
curl -s -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"agents-a1","messages":[{"role":"user","content":"ping"}]}'
# Expect a real completion; watch `journalctl -u model-router -f` for
# spawn + health messages.

# 7. Enable on boot after a successful soak (minutes to hours):
systemctl enable model-router.service
```

## Rollback (per node)

```bash
systemctl stop model-router.service
systemctl start llama-swap.service
# llama-swap reads its original config from /opt/ai/config/llama-swap.yaml
# (untouched). The router's config + binary remain installed for the next try.
```

## Per-node specifics

| Node | Router port | Verify against | Notes |
|------|-------------|----------------|-------|
| nomad | :8081 | local `qwen3.5-9b`; Chat lab `agents-a1` then `qwen3.8-27b` (mesh) | unlisted local stanza; catalog Load/Unload is local-only — remote ids are Mesh routed via Chat lab |
| nugget | :8081 | `agents-a1` chat (preload), `qwen3-0.6b-instruct` | Swap as user kevyn; unit is `deploy/model-router.nugget.service` (memlock + TurboQuant lib path). Single V100. |
| gareths-homelab | 127.0.0.1:18080 (nginx :8081/:8082) | `qwen2.5-1.5b-instruct` (preload), `krea-2-turbo` via /sdapi/v1/txt2img | nginx blocks already proxy both ports → 18080; confirm `proxy_buffering off` for SSE |
| digger | :8082 | `qwen3-0.6b-instruct` (preload), `krea-2-turbo` via /sdapi/v1/txt2img | image stanzas with `"model": "<id>"` in body; no body model → krea-2-turbo (path_default) |
| buster | :18080 | `agents-a1`, `qwen3-0.6b-instruct` (alias intent-router), /v1/embeddings | rag-proxy keeps pointing at 127.0.0.1:18080 — NO rag-proxy change needed. Reranker/sparse/turbovec (:18095/:18096/:18097) untouched |

## Known v0.1 limitations (accept before swapping)

1. **Last-resort idle eviction, not in-flight preemption.** Stale occupants
   go first; if that is not enough, an idle resident (including ttl=0) is
   unloaded unless a request is mid-generation. In-flight work is never
   killed. If this node still cannot fit, the request spills to a peer that
   advertises the same `model_id`.
2. **VRAM numbers are measured then rounded up** (`docs/vram-fit-ladder.md`).
   After swap, compare `nvidia-smi` used to `vram_mb`. The router treats
   `vram_mb` as a hard reservation (fail-closed).
3. **Peer wiring goes directly to router ports** (buster :18080, not the old
   :8081 rag-proxy path). Update all peer base_urls at once so no node talks
   to a half-migrated fleet with a stale port.
4. **nvidia-smi per-GPU resilience:** a single sick GPU (like the 4060's
   current "Unable to determine device handle") no longer blinds the whole
   poller — healthy GPUs still report. But a stanza pinned to the sick GPU
   cannot be admitted (fail-closed) until the driver recovers.
5. **Client disconnect during spin-up:** the request goroutine waits out the
   full spin-up window; the model still finishes loading and serves the next
   request. (SPEC open question: hold vs async polling.)

## Post-swap checklist

- [ ] `_router/status` shows loaded models + per-GPU free VRAM on every node
- [ ] peers see each other: `_router/status` → `peers` map has fresh timestamps
- [ ] cross-node request: from nomad `/ui/` Chat lab, `agents-a1` then `qwen3.8-27b` stream via a peer
- [ ] catalog Load/Unload on a **local** stanza actually start/stop the backend
- [ ] offline tolerance: `systemctl stop model-router` on one node; the others
      still 503 cleanly (no hang) for its models and keep serving local ones
- [ ] streaming chat (Open WebUI) shows tokens live through the router
- [ ] sd.cpp: Open WebUI image gen on digger/gareth works via /sdapi/v1
- [ ] `vram_mb` calibrated against nvidia-smi deltas
