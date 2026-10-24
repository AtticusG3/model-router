# Model Router — SPEC

Internal tool. Not a public repo. Replaces llama-swap fleet-wide (Nomad, Buster,
Nugget, Digger, Gareth's homelab). One static binary, same build on every node.
Per-node behaviour comes from one YAML config.

`docs/vram-fit-ladder.md` is how to choose a shared `model_id` and fit a weight
on a GPU. `PLAN.md` is the fleet survey that produced this design. Where they
disagree with this file, this file and the code win.

## Credits

Process-supervision patterns (spawn, log capture, crash/restart, health-check
polling, port allocation) are adapted from **mostlygeek/llama-swap**. Where
code is copied directly rather than reimplemented, the source file carries a
comment crediting llama-swap and its license.

The sd.cpp backend integration (API surface, request/response shape) is
built against **leejet/stable-diffusion.cpp**, credited in the same way
wherever its API contract shapes the code directly.

## Goals

- Replace llama-swap on every node with one backend-agnostic router.
- No central point of failure: each node is a full instance. It does not
  report to a separate control plane.
- A node fully serves any model in its own stanza catalog even if every other
  node and the LAN are unreachable.
- Cross-node placement when the local node cannot serve the request (model
  not local, local GPUs cannot admit it, or every local slot is busy). The
  client still talks to the node it hit; that node reverse-proxies.
- Backend-agnostic: llama-server (stock or TurboQuant), sd.cpp, and any other
  process are the same thing to the router — a command, a VRAM budget, a
  health check, and a way to match an incoming request to a `model_id`.

## Non-goals

- No distributed consensus (Raft, etc.). Each node is the only authority for
  its own GPUs. See Admission control.
- No dynamic peer discovery. Peers are a static list in the node config.
- No auth or TLS between nodes. LAN-only trust. Revisit before any exposure
  beyond the current network.
- No node autoscaling.
- No killing of an in-flight generation. A request that is mid-proxy holds
  the model until it finishes.
- No sub-path rewrite of a third-party WebUI bundle. The operator UI is
  embedded in the binary at `/ui/`. `/upstream/{model_id}/` proxies to a
  local backend with that prefix stripped, on the router's own port.

## Identity

The mesh lists and routes by `model_id`. The same architecture, finetune, and
size share one id on every node that can run it, including when the quant
differs. A different finetune is a different id. One stanza per `model_id`
per node.

Aliases are other ids that resolve to that stanza (legacy client names such
as `coding-pool` or `daily-driver`). They are not separate catalog rows.

Pools are a compatibility spillover list for clients that still POST a
selector id. They are not catalog names and they are not listed in
`/v1/models` or the operator UI.

`unlisted` is accepted so old llama-swap configs still parse. The catalog
lists every local stanza anyway.

## What runs in the binary

1. **Stanza catalog** — the local YAML: every model this node can run, plus
   pools, peers, and preload.
2. **Process supervisor** — spawns, health-polls, restarts, and stops one
   backend process per instance.
3. **GPU poller** — `nvidia-smi` only (no pynvml, so the binary stays
   dependency-free). One sick GPU does not hide the others: enumerate with
   `-L`, then query each index.
4. **Admission ledger** — per-GPU VRAM reservations. Lives only on this node.
5. **HTTP API** — OpenAI-compatible and sd.cpp passthrough, control routes,
   and the embedded operator UI.
6. **Peer client** — pulls peer telemetry, calls `POST /_router/load`, then
   reverse-proxies the client request.

Startup order: parse config, bind HTTP, poll GPUs once (so preload sees real
free VRAM), then preload in config order while the API is already up.
Shutdown stops accepting work, then unloads every local model.

## Config

One file per node (`-config`, default `/opt/ai/config/model-router.yaml`).
`-listen` overrides `listen`. `-node` overrides the advertised node name
(default hostname). `-check` validates and prints the derived catalog,
including extra-instance ports, then exits.

```yaml
listen: 0.0.0.0:18080
start_port: 5800          # base for auto-allocated ports
reserved_ports: [8081, 6333]
preload: [intent-model]

telemetry:
  poll_seconds: 2         # nvidia-smi
  peer_sync_seconds: 5    # pull GET /_router/telemetry
  peer_stale_seconds: 15  # older than this → peer is not a candidate

stanzas:
  - model_id: qwopus-27b-fusion
    name: "Qwopus 27B fusion"
    aliases: [coding-model]
    command: "/opt/ai/bin/llama-server -m {model_path} --port {port} --parallel 2"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    model_path: /opt/ai/models/qwopus.gguf   # optional; fills {model} and {model_path}
    vram_mb: 18000
    device: "1|2"          # see Placement
    max_instances: 2       # 0 or 1 = one copy
    slots: 2               # 0 = read --parallel / -np from command, else 1
    spin_up_seconds: 60    # 0 = default 60
    api_type: chat         # chat | embedding | image | rerank (UI metadata)
    match:
      body_field: model    # XOR path_prefix
    health_check: /health
    idle_ttl_seconds: 600  # 0 = resident, never swept
    port: 5901             # 0 = next free port from start_port
    proxy: ""              # default http://127.0.0.1:{port}; forbidden when max_instances > 1

  - model_id: sdxl-turbo
    command: "/opt/ai/bin/sd-server -m /opt/ai/models/sdxl.gguf --port {port}"
    vram_mb: 9000
    api_type: image
    match:
      path_prefix: /sdapi/v1
      path_default: true   # fallback when the body has no model id
    health_check: /sdapi/v1/options
    idle_ttl_seconds: 300

pools:
  embed-pool:
    strategy: spillover    # the only strategy
    targets: [qwen3-embedding-4b, digger/qwen3-embedding-4b]
    spillover: 0           # 0 = local target's slots, or 1 for a peer

peers:
  - name: digger
    kind: router           # router | openai
    base_url: http://192.168.1.36:8082
    models: [sdxl-turbo, qwen3-embedding-4b]
    body_field: model      # field rewritten to the peer-local id before proxy
  - name: openrouter
    kind: openai           # proxy only; no /_router, no telemetry
    base_url: https://openrouter.ai/api/v1
    models: [some-upstream-id]
```

`match.body_field` and `match.path_prefix` are mutually exclusive. A stanza
with neither matches JSON field `model`. The matcher always understands
`model` as well, so a normal OpenAI body still resolves. Several image
stanzas may share one `path_prefix`; at most one of them is `path_default`.

Command and env macros: `{port}` or `${PORT}`, `{model}` or `{model_path}`,
and `{gpu}` (the index chosen at spawn). A multi-instance stanza needs
`{port}` in the command or env. A device that is a choice of cards needs
`{gpu}`.

`model_id` and aliases must not contain `#`. That character marks instance
keys (`model#2`). Instance 0 keeps the bare id.

## Request flow

1. The client hits this node's HTTP API.
2. The matcher reads the body, then the URL.
   - JSON: the first configured body field that is a non-empty string
     (`model`, plus any stanza `body_field`).
   - Form and multipart: the `model` field, then the query.
   - GET: query `model`.
   - If that id is a local stanza, an alias, a pool, `peer/model`, or a bare
     id some peer lists, it wins over path matching.
   - Otherwise `path_prefix`: the `path_default` stanza for that prefix, or
     the first stanza in config order.
   - Anything else is `404` with `no model id could be identified`.
3. Resolve the id:
   - Local stanza or alias → this node.
   - Pool → spillover, targets in order. A target is eligible while its
     in-flight count is under the spillover cap and it is running or can
     start (local), or the peer is eligible (below).
   - `peer/model` → that peer, if its `models` list contains the id.
   - Bare id that only a peer lists → mesh pick (below).
4. **Local.** If the request arrived from another router
   (`X-Model-Router-Peer`), it stays here. No spill back.
   - If this node cannot serve it now, try a spill first: later targets of
     any pool that names this model, then a peer that lists the same
     `model_id`.
   - Prefer a running instance with a free slot, then a starting one. If
     every copy is at its slot cap, start another copy only when
     `max_instances` allows and a GPU fits that copy without evicting
     anything. Otherwise slot-spill to a peer that has a free slot or can
     admit a fresh load. If nothing can take it, queue on the least-loaded
     local instance.
   - Load if needed (admission, below), wait until `health_check` returns
     200, then stream the reverse proxy. The client connection is held for
     the generation. `X-Accel-Buffering: no`.
5. **Peer.** `kind: router`: `POST /_router/load` with `{model_id}`. That
   node runs its own admission. On success, rewrite the JSON model field to
   the peer-local id (the peer's `body_field`), set `X-Model-Router-Peer` to
   this node's name, and stream the original path to the peer. `kind: openai`:
   no load call; proxy straight to `base_url`.
6. No local capacity and no reachable peer that lists the model → `503` with
   the reason. No silent hang.

Mesh pick walks peers in config order:

- A router peer that already has the model loaded with a free slot is used
  immediately.
- An `openai` peer that lists the model is used as soon as the walk reaches it.
- Otherwise prefer a fresh router peer that can admit a new load, then one
  that lists the model but whose card is busy (that node waits and evicts;
  the origin does not 503 just because the cached free-VRAM number looks
  tight), then a loaded peer whose slots are full.
- Router peers with stale or missing telemetry are skipped. Unknown VRAM on
  a fresh peer is still tried; that node admits for real.

A peer's cached view is only a candidate hint. Accept or reject always
happens on the node that owns the GPU, inside `POST /_router/load`.

`POST /_router/load` is detached from the caller's cancel. A dropped client
does not abort a spin-up that is already useful. The wait is capped at
`max(spin_up_seconds, 90s) + 8 minutes`, or until this process shuts down.
A peer load joins an instance that is already live instead of starting
another copy.

## Placement

`device` says which GPUs a stanza may use:

| Value | Meaning |
|-------|---------|
| empty | Any GPU on the node. One card per instance. |
| `"1"` or `"CUDA1"` | That card only. |
| `"1\|2"` | One of the listed cards, chosen per load. Command or env must contain `{gpu}`. |
| `"0,1,2"` | One process across every listed card. No `{gpu}`. `max_instances` must be 1. |

Single-card ranking, best first: a card that is not already running another
copy of this model, then the tightest fit (leftover VRAM in 1 GB buckets, so
nvidia-smi noise does not override device order), then the order written in
`device`.

A split stanza's `vram_mb` is divided across the listed cards in proportion
to each card's available VRAM, so a resident on one card keeps its bytes and
the emptier cards take a larger share. The backend does its own tensor split
(llama.cpp `--fit`, for example). The router only reserves the shares.

`vram_mb` is the admission budget, not a live measurement. Actual nvidia-smi
use can be lower. Fit is the tighter of ledger free (`total - reservations`)
and live free (`nvidia-smi` free, plus credit for reservations just released
whose free-memory reading has not caught up).

## Admission control

Every node is the sole authority over its own GPU state. Other nodes hold a
cached, possibly stale copy and never treat it as ground truth.

- The reservation ledger is local and serialized.
- `load` reserves before spawn and keeps the reservation until unload or a
  failed start, so a crash-restart does not have to reserve again and two
  concurrent loads cannot double-book the same free bytes.
- A child that dies or never goes healthy gets one retry. That retry does
  not evict neighbours: the failure was this model.
- If the ledger cannot reserve and the caller is allowed to wait, eviction
  runs (below), then the load waits until a reservation frees or the wait
  is cancelled.
- A model is unschedulable only when every candidate card is known and
  smaller than `vram_mb` (for a split, when some card is smaller than its
  share). Missing telemetry waits. An occupied card that is large enough
  waits or evicts.
- An extra copy never evicts and never waits for VRAM. If its card is gone,
  the request queues on a sibling that is already up.
- `vram_mb <= 0` with no GPU telemetry is allowed (a CPU process). A positive
  `vram_mb` is refused until the first successful poll.

## Eviction

Something that is mid-request, still starting, or already stopping is never
unloaded. A stanza with `vram_mb <= 0` is never unloaded to make room; it
does not free a GPU.

Two mechanisms:

- **Idle sweep.** Every 30 seconds, each instance with `idle_ttl_seconds > 0`
  that has had no request for that long is unloaded. `idle_ttl_seconds: 0`
  is resident and is never swept. Each copy ages on its own, so an idle extra
  copy can go while a busy sibling stays. The sweep exists so VRAM returns
  to the machine even when no new load is waiting.
- **Make room for a load.** Stale non-residents first, then other idle
  non-residents, then residents (`ttl <= 0`), and only when they are not
  mid-request. For a single-GPU model the router plans every candidate card
  and runs only the gentlest plan: lowest eviction kind, then fewest MB,
  then device order. A split model walks every listed card until each share
  fits.

## Supervisor

One supervised process per instance, on that instance's port. Health is
polled until `spin_up_seconds`. The process must still be alive when health
passes, so a leftover occupant of the port cannot be mistaken for this
spawn. The process is not bound to the HTTP request that triggered it.

An unexpected exit while the model is supposed to be running restarts it
with backoff from 1 second, doubling to 30 seconds, until unload or
shutdown. Stop is SIGTERM to the process group, then SIGKILL. On Windows the
stop path is process-kill only.

Extra-instance ports are allocated above every stanza port and every
`reserved_ports` entry, in config order. `-check` prints them.

## Telemetry

Local poll default 2 seconds. Peer sync is a pull of `GET /_router/telemetry`
every 5 seconds. Cached telemetry older than 15 seconds is unknown, and that
router peer is left out of candidate selection. `kind: openai` peers are not
polled.

The snapshot a node publishes:

- Per GPU: index, name, total MB, free MB, free MB if stale models were
  evicted, free MB if idle models were evicted.
- Each running instance: `model_id`, instance key when it is not instance 0,
  `fresh` or `stale`, VRAM budget, GPU, slot count, in-flight count.

Candidate selection treats a card as empty enough to try when idle-evicted
free is within 2 GB of the card total. nvidia-smi never reports a fully idle
card. A peer that lists the model still fits when its card is empty even if
this node's own `vram_mb` is larger than that card: the hint is local, and
the peer admits against its own budget.

## HTTP API

| Path | Purpose |
|------|---------|
| `/v1/*`, `/v/*`, `/sdapi/v1/*`, `/rerank`, `/reranking`, `/infill`, `/completion`, `/props`, `/slots`, `/tokenize`, `/detokenize`, `/apply-template` | Passthrough to the resolved backend. `/` is the same when a model id is present. |
| `/v1/models` | One row per mesh `model_id`: every local stanza, plus models listed by fresh router peers and by openai peers. No aliases, no pools, no `peer/model` ids. |
| `POST /_router/load`, `POST /_router/unload` | This node's admission. Body `{"model_id":"..."}`. Unload of a model id stops every instance. |
| `GET /_router/status` | Node name, catalog, local telemetry, cached peers. |
| `GET /_router/telemetry` | The snapshot peers pull. |
| `GET /_router/logs` | Router, upstream, and mesh rings, plus a scrape of each running backend's `/metrics`. |
| `GET /_router/activity`, `GET /_router/activity/{id}` | In-memory generation log and request/response captures. |
| `GET /upstream/{model_id}/...` | Strip the prefix and proxy to that local backend. |
| `GET /ui/` | Embedded operator UI. |
| `GET /health` | `OK`. |
| `GET /metrics` | Prometheus: up, running models, GPU free and reclaimable, peer freshness. |

`api_type` does not choose a route. The UI uses it to decide chat lab vs
image lab. Routing is only the matcher plus the catalog.

## Operator UI

`/ui/` is HTML, CSS, and script embedded in the binary. Catalog Load and
Unload start and stop this node's stanzas only. Choosing a remote `model_id`
in the chat or image lab is an ordinary `/v1/...` request, so this node
spills or proxies. Activity keeps the last 200 proxied requests, with at
most 256 KB per body and 8 MB of captures, in memory only. It resets on
restart. Authorization and cookie headers are not stored.

## Still out of scope

- Preempting a generation that is already in flight.
- Auth or TLS on the mesh.
- A separate job API for slow spin-up or image generation. The client
  connection is held for the proxied request; only the peer `load` call is
  detached from client cancel, and only up to the cap above.
- Dynamic discovery, consensus, and node autoscaling.
