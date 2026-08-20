# Model Router — v0.1 SPEC

Internal tool. Not a public repo. Replaces llama-swap fleet-wide (Nomad, Buster,
Nugget, Gareth's homelab). Single binary, same build runs on every node.

## Credits

Process-supervision patterns (spawn, log capture, crash/restart, health-check
polling, port allocation) are adapted from **mostlygeek/llama-swap**. Where
code is copied directly rather than reimplemented, the source file will carry
a comment crediting llama-swap and its license.

The sd.cpp backend integration (API surface, request/response shape) is
built against **leejet/stable-diffusion.cpp**, credited in the same way
wherever its API contract shapes the code directly.

## Goals

- Replace llama-swap on every node with one backend-agnostic router.
- No central point of failure: each node is a full instance (agent +
  coordinator merged), not a worker reporting to a separate control plane.
- A node fully serves any model in its own local stanza catalog even if every
  other node/the LAN is unreachable — this is the explicit requirement driving
  Gareth's-homelab-offline resilience.
- Cross-node placement when the local node can't serve the request (model not
  local, or local GPU(s) full) — falls through to a peer, transparently
  proxied.
- Backend-agnostic: llama-server, TurboQuant, vLLM, sd.cpp/stable-diffusion.cpp
  all look the same to the router — a command to spawn, a VRAM budget, a
  health check, and a way to match an incoming request to this model.

## Non-goals for v0.1

- No distributed consensus (Raft, etc.) — see Admission Control below for why
  it isn't needed at this scale.
- No dynamic peer discovery — static `peers.yaml` per node.
- No multi-replica load balancing (one instance of a given model per node,
  max).
- No auth/TLS between nodes. LAN-only trust model. Revisit before any
  exposure beyond the current network.
- No autoscaling or preemption policy (see Open Questions).

## Architecture

Each node runs five logical components inside the one binary:

1. **Stanza Catalog** — local, static YAML: every model this node is capable
   of running.
2. **Process Supervisor** — spawns/monitors/kills backend processes
   (llama-swap-derived).
3. **GPU Telemetry Poller** — local nvidia-smi/pynvml poll; per-GPU free
   VRAM + currently-loaded model + load timestamp.
4. **Local HTTP API** — OpenAI-compatible passthrough for chat/embeddings,
   plus sd.cpp-style routes for image generation. This is what clients
   (Open WebUI, rag-proxy, Hermes, etc.) hit on `localhost`.
5. **Peer Client** — talks to other nodes' Local HTTP APIs / control
   endpoints when a request can't be served locally.

## Stanza schema

```yaml
- model_id: qwopus-27b-fusion
  command: "/opt/ai/bin/llama-server -m {model_path} --port {port} -ngl 99"
  vram_mb: 18000
  spin_up_seconds: 12
  api_type: chat          # chat | embedding | image
  match:
    body_field: model     # OpenAI-style: model id in JSON body
  health_check: /health
  idle_ttl_seconds: 600

- model_id: sdxl-turbo
  command: "/opt/ai/bin/sd-server -m {model_path} --port {port}"
  vram_mb: 9000
  spin_up_seconds: 6
  api_type: image
  match:
    path_prefix: /sdapi/v1   # A1111/sd.cpp-style: no model field in body
  health_check: /sdapi/v1/options
  idle_ttl_seconds: 300
```

`match` is deliberately a discriminated field, not always `body_field`,
because sd.cpp-style backends typically don't carry a model id in the request
body at all — model selection happens by which route/port you hit. The
Route Matcher checks `match` per stanza to figure out what's being asked for
before it does anything else.

## Request flow

1. Client hits this node's Local HTTP API.
2. Route Matcher resolves the request to a `model_id` (via body field or path
   prefix, per stanza `match`).
3. **Local first**: if `model_id` is already loaded on this node → proxy
   directly, no decision-making needed.
4. If not loaded but in the local Stanza Catalog and local free VRAM ≥
   `vram_mb` → local admission control (below) → spawn → wait for
   `health_check` → proxy.
5. If not servable locally (not in catalog, or no local VRAM) → consult
   cached peer telemetry from `peers.yaml` → pick a candidate node → call
   that node's `load()` → candidate runs its **own** local admission control
   → on success, this node transparently reverse-proxies the client's request
   through to the candidate (streamed) → response flows back through this
   node to the client.
6. No local capacity and no reachable/capable peer → `503` with a clear
   reason. No silent hangs.

## Admission control (avoids split-brain)

Every node is the sole authority over its own GPU state. Other nodes only
ever hold a cached, possibly-stale copy of that state via periodic telemetry
sync — never treated as ground truth.

- A local VRAM reservation ledger lives only on the owning node.
- Reservation increments at `load()` call time, decrements on unload or
  failed spin-up.
- A remote node's cached view of a peer is used only to *pick a candidate* —
  the actual accept/reject decision always happens on the node that owns the
  GPU, at the moment `load()` is called, against its own live state.
- This means two near-simultaneous requests routed to the same peer can't
  double-book that peer's VRAM: the peer itself serializes its own
  admission decisions.

## Telemetry

- Local poll interval (nvidia-smi/pynvml) — TBD interval, default candidate
  2s.
- Peer sync: lightweight periodic push/pull of {free VRAM per GPU, loaded
  models} to/from each `peers.yaml` entry.
- Staleness: cached peer telemetry older than a threshold is treated as
  "unknown" and that peer is excluded from candidate selection until it
  refreshes — this is what lets a node keep functioning correctly (by simply
  not offering unreachable peers as candidates) when the LAN or a specific
  peer drops.

## Static asset / WebUI proxying

Lesson carried over from rag-proxy: rewriting a bundled WebUI's asset paths
under a sub-path prefix breaks once the UI bundle is unzipped and serves
absolute-path asset references. v0.1 does not attempt path-prefix rewriting
for any bundled UI proxied through this router — any WebUI exposed through
it gets its own dedicated port/host binding rather than a rewritten
sub-path, until a proper base-href rewrite pass is designed.

## Config conventions

- Per-node stanza YAML follows the naming convention already used across the
  fleet: generic pool/selector IDs, specific/descriptive model entry IDs.
- `peers.yaml`: static list of `{name, ip, port}` — no dynamic discovery.

## Open questions for v0.2+

- Hold-connection vs. async job-polling for slow spin-ups (e.g. colibri on
  Buster) and for image-gen jobs, which have a different duration profile
  than chat completions.
- Multi-GPU tensor-split stanzas (relevant once Buster gets a second V100).
- Eviction/preemption policy — what happens when a higher-priority request
  needs VRAM currently held by an idle-but-not-yet-TTL'd model.
- Metrics/observability endpoint.
