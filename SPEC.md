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
  the model until it finishes, and so does a running async job (see Async
  jobs).
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
headroom_mb: 0            # MB left unbooked on every GPU; default 0
max_request_body_mb: 256  # cap on one request body; over it is 413 before any load; default 256
job_table_max: 256        # async jobs remembered; a full table drops the oldest finished one; default 256
job_ttl_minutes: 60       # longest one async job holds its model; default 60
session_max_minutes: 0    # close a realtime session open longer (code 1001); 0 = no cap (default)
preload: [intent-model]   # load at startup; restored in config order if evicted

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
    api_type: chat         # see api_type below; empty means chat
    capabilities: [vision] # optional: vision, audio_in, audio_out, tools, reasoning
    match:
      body_field: model    # XOR path_prefix
    health_check: /health
    idle_ttl_seconds: 600  # 0 = resident, never swept
    hold_seconds: 0        # seconds after healthy during which eviction skips it
    peer_spill: auto       # auto | on_failure | never; see Local below
    max_request_body_mb: 256  # optional: override the top-level body cap
    port: 5901             # 0 = next free port from start_port
    proxy: ""              # default http://127.0.0.1:{port}; forbidden when max_instances > 1

  - model_id: sdxl-turbo
    command: "/opt/ai/bin/sd-server -m /opt/ai/models/sdxl.gguf --listen-ip 127.0.0.1 --listen-port {port}"
    vram_mb: 9000
    api_type: image
    image_pipeline: sd-server  # preset or map; see below
    match:
      path_prefix: /sdapi/v1
      path_default: true   # fallback when the body has no model id
    health_check: /v1/models # sd-server has no /health; no shim needed
    idle_ttl_seconds: 300

  - model_id: stt-large
    command: "/opt/ai/bin/stt-server --port {port}"
    vram_mb: 0             # CPU-only: no card, no reservation, never evicted
    device: cpu
    api_type: transcription
    slots: 1               # a transcription stanza never overbooks its slots
    request_timeout_seconds: 0   # 0 = no limit; else 504 after this long
    path_rewrite:          # optional; the backend's own path for a client path
      - {from: /v1/audio/transcriptions, to: /inference}

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

`api_type` is one of `chat`, `embedding`, `rerank`, `image`, `speech`,
`transcription`, `realtime`, `video`, `audio_gen`. Empty means `chat`. An
unknown value is a load error that lists the valid values, so a typo is caught
by `-check` instead of silently hiding the model. `-check` prints the type per
stanza.

`capabilities` is an optional list per stanza from `vision`, `audio_in`,
`audio_out`, `tools`, `reasoning`; an unknown flag is a load error. It is
published in `/v1/models` and `/_router/status`. A vision chat model (started
with an `--mmproj`) stays `api_type: chat` and adds `capabilities: [vision]`.

`image_pipeline` (only on `api_type: image`) runs the image request handling on
the node that owns the stanza, for requests from local clients and for requests
proxied in from peers. It is a preset name or a map:

```yaml
image_pipeline: sd-server   # the sd-server checks
image_pipeline: ideogram4   # sd-server plus ideogram4-format caption enhancement
image_pipeline:
  caption: "" | ideogram4    # caption format; "" = no caption step
  enhancer: {model: enhancer-model, timeout_seconds: 30}
  retry_blocked: 0-5         # blocked retries; 0 disables, the default
```

`caption` is a format name, so another format can be added later; an unknown
name is a load error. The enhancer names the model and timeout the caption step
uses. `sd-server` and `ideogram4` are the presets; an unknown preset, or
`image_pipeline` on a stanza that is not `api_type: image`, fails load with a
message naming the valid values.

The blocked-card retry ships in this build, and so does the LoRA warm-up:
the router warms sd-server's LoRA cache when the backend becomes ready and
before a request whose body names a LoRA, and retries one `invalid lora path`
reply after warming again. The other preset steps parse and validate but
change no request yet, so caption enhancement and the img2img body checks
still need the shim until they land.

A bare sd-server stanza needs no shim for LoRA requests: run it with
`--listen-ip 127.0.0.1 --listen-port {port}` and `health_check: /v1/models`
(sd-server has no `/health`). Keep the shim if the stanza needs caption
enhancement or a blocked-card retry. The Python shim in `deploy/` stays
supported for nodes that still wrap sd-server with it.

The request body kept in Activity is the client's original, even when a
pipeline step rewrites it; a step can attach notes (for example the rewritten
prompt) that are listed with the capture. A response step may add headers or
ask for one retry.

`job` selects the adapter in front of a stanza's async job API. It is only
valid on `api_type: video`. `adapter` is `openai` (the default, the backend
speaks `/v1/videos` itself) or `sdcpp` (sd-server's `/sdcpp/v1` job routes).
An unknown adapter is a load error that lists the valid values. See Async
jobs.

An `api_type` gives a model-less request a default route. The type serves these
API paths:

| api_type | paths |
|----------|-------|
| `speech` | `/v1/audio/speech` (and `/v1/audio/speech/batch`), `/v1/audio/voices` |
| `transcription` | `/v1/audio/transcriptions`, `/v1/audio/translations` |
| `realtime` | `/v1/realtime` |
| `video` | `/v1/videos` |
| `audio_gen` | `/v1/audio/generations` |
| `image` | `/v1/images/*`, `/sdapi/v1/*` |
| `embedding` | `/v1/embeddings` |
| `rerank` | `/v1/rerank` |

When a request reaches one of those paths with no `model`, the router serves it
with the stanza of that type marked `path_default: true`, or with the only
stanza of that type. Several candidates and no default is a `400` that names
them. At most one stanza per `api_type` may be `path_default`, whatever its
`path_prefix`; a second is a load error. A stanza's explicit `match.path_prefix` still wins over this fallback.

Command and env macros: `{port}` or `${PORT}`, `{model}` or `{model_path}`,
and `{gpu}` (the index chosen at spawn). A multi-instance stanza needs
`{port}` in the command or env. A device that is a choice of cards needs
`{gpu}`.

`model_id` and aliases must not contain `#`. That character marks instance
keys (`model#2`). Instance 0 keeps the bare id.

`max_request_body_mb` (top level, default 256, at most 1048576 = 1 TiB) caps one request body; a stanza
may override it. Over the cap the client gets `413` before any model is loaded.
JSON and form bodies are read into memory (the filters rewrite them). A
multipart body is read only as far as its `model` field and the rest is
streamed to the backend; when the `model` part comes after a file part the body
is spooled to a temp file (deleted when the request ends) instead of RAM. A
`?model=` query skips that spool, so it wins over a `model` part past the
first 64 KB. A
media response (`audio/*`, `video/*`, `application/octet-stream`) is flushed per
chunk and never captured as text; its Activity row keeps the content type,
byte count and a body hash prefix (see Operator UI for the speech player).

`filters` (on a stanza or a peer): `renameParams` moves a field to the name
the backend takes (`{ref_audio: speaker_ref_b64}`), `stripParams` drops
top-level fields, `setParams` sets them (a key ending in `?` only fills a field
the client omitted), and a stanza's `setParamsByID` mints alias ids with their
own set. Renames run first, so strip and set use the backend's names. They
apply to JSON object bodies and to the fields of
`application/x-www-form-urlencoded` and `multipart/form-data` bodies. A rename
may move a file part (`{file: audio}`); strip and set never touch one. In a
form, a non-string value is sent as its JSON text (`0`, `true`). `model` and
the `body_field` cannot be filtered or renamed.

`path_rewrite` (stanza only) sends a request to the backend's own route:
`[{from: /v1/audio/speech, to: /tts}]` maps a path starting with `from` to `to`
plus the rest of the path. The first matching `from` wins, so
`/v1/audio/speech/batch` needs its own entry before the shorter one if it
should not become `/tts/batch`. Both sides must start with `/`. Matching,
Activity rows and metrics keep the client's path; only the proxied request
changes. Filters and `path_rewrite` are the shared mechanism for audio
backends that need a different path or field names.

Before a request goes to a peer, its model field (the peer's `body_field`) is
set to the peer's id in JSON, urlencoded and multipart bodies, and so is a
`model` query param on any method, so a `peer/model` id or an alias reaches the
peer as an id it serves. A multipart body that a filter or this rewrite touches
keeps its boundary and is streamed part by part, never buffered, so it goes out
chunked (no `Content-Length`). If such a body turns out not to be valid
multipart partway through (a bad part header, no closing boundary), the client
gets 400, not a 502.

## Request flow

1. The client hits this node's HTTP API.
2. The matcher reads the body (JSON and form bodies in full, up to
   `max_request_body_mb`; a multipart body only as far as its `model` field),
   then the URL.
   - JSON: the first configured body field that is a non-empty string
     (`model`, plus any stanza `body_field`).
   - Form and multipart: the `model` field, then the query.
   - GET: query `model`.
   - If that id is a local stanza, an alias, a pool, `peer/model`, or a bare
     id some peer lists, it wins over path matching.
   - Otherwise `path_prefix`: the `path_default` stanza for that prefix, or
     the first stanza in config order.
   - Otherwise the path's `api_type` (table above): its `path_default` stanza,
     or its only one. Several candidates and no default is `400` naming them.
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
     local instance. A `transcription` stanza instead waits in the router
     for a free slot (see Transcription).
   - While the model cannot be placed here (for example pinned models hold
     every card it may use), the spill is retried every few seconds. A model
     that can never fit because pinned models hold the room fails at once.
     `admit_timeout_seconds` (top level, default 60) caps the wait for local
     room; when it runs out the client gets `503` with `Retry-After`. The
     journal's mesh stream says why no peer was chosen (cooling, stale
     telemetry, not advertising).
   - `peer_spill` (per stanza) decides when this local target may spill.
     `auto` (default) spills as soon as local room is blocked or every slot
     is busy. `on_failure` waits for local admission and the load instead,
     and walks peers only when the local start fails hard (process died,
     health timed out, start cooldown) or the admit wait runs out. `never`
     also waits, but a hard start failure is answered here with `503`
     instead of walked. A peer-qualified id and a request that arrived from
     a peer are unaffected.
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

A refused load walks on to the next router peer in that order (ADR 0003). The
walk covers a direct `peer/model` target, the mesh hop after a local model
could not start, and a slot spill. A peer that answered `Retry-After` for a
model is skipped for that model until the window ends; the origin stores the
window the peer sent and runs no doubling clock of its own. A refusal with no
`Retry-After` is forgotten. The walk stops once a peer has written response
bytes, including an error body after it accepted the load. A slot spill that
finds no router peer queues on the local model. With no local model and every
router peer refusing, the client gets `503` whose body is the last failure and
whose `Retry-After` is the smallest window any refusal sent, or absent if none
sent one. An `openai` peer is a single hop. Pools keep their configured order
and do not consult remembered windows.

`POST /_router/load` is detached from the caller's cancel. A dropped client
does not abort a spin-up that is already useful. The wait is capped at
`max(spin_up_seconds, 90s) + 8 minutes`, or until this process shuts down.
A peer load joins an instance that is already live instead of starting
another copy.

## Speech

An `api_type: speech` stanza serves text to speech. The backend choice and its
measured limits are in `docs/research/tts-backends.md`.

- **Routes.** `POST /v1/audio/speech` and `POST /v1/audio/speech/batch` go to
  the stanza named by `model`, or to the speech default (the `path_default`
  stanza, or the only one). So do `GET /v1/audio/voices`,
  `GET /v1/audio/voices/{id}`, `POST /v1/audio/voices` and
  `DELETE /v1/audio/voices/{id}`; a GET or DELETE names its model in
  `?model=`. The router does not rewrite `model` for a local stanza, so the
  backend must accept the stanza id as its model name.
- **Voice list.** A model-less `GET /v1/audio/voices` on a node with more than
  one speech stanza asks the speech default (loading it if needed) and every
  other speech stanza that is already running, and merges the lists:
  `{"object":"list","data":[{"id":"narrator","model":"tts-clone"}, ...]}`. A
  voice the backend gives as a bare string becomes `{"id": ...}`; an object
  keeps its fields and gains `model`. A stanza that fails or has no voice list
  is left out (logged); when every one fails the answer is `502`. Listing
  voices never starts a second speech model. With one candidate the backend's
  own answer passes through unchanged.
- **Formats.** `response_format` (`mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`)
  passes through, and so does the backend's `Content-Type`.
- **Streaming.** The proxy flushes every chunk. With `stream: true`, or any
  chunked answer, the first audio reaches the client while the backend is
  still synthesising. The Activity writer counts and hashes the bytes but
  never holds them back.
- **Slots.** For a speech stanza `slots` caps the syntheses in each instance's
  backend at once. A request over the cap waits at the router for a free slot,
  up to `admit_timeout_seconds` (one budget with any wait for GPU room), then
  gets `503` with `Retry-After`. Other api_types still queue on the backend.
- **Other routes and field names.** `path_rewrite` and `filters.renameParams`
  (see Config) fit a backend that serves speech on its own path or takes
  other field names.

Example stanza for the 24 GB card, from `docs/research/tts-backends.md` §6.1
with the command shortened (see the research note for the real flags):

```yaml
  - model_id: tts-clone
    name: "Voice-clone speech model (24 GB card)"
    command: >
      /opt/ai/venv-tts/bin/tts-serve /opt/ai/models/tts/voice-clone-base
      --served-model-name tts-clone
      --host 127.0.0.1 --port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
    vram_mb: 9000
    device: 0
    spin_up_seconds: 300
    api_type: speech
    capabilities: [audio_out]
    match: {path_default: true}   # model-less speech and voice requests
    health_check: /health
    idle_ttl_seconds: 900
    slots: 4                      # the backend's own batch size
    port: 5933
```

A backend that serves the API on its own route and field names adds, for
example:

```yaml
    path_rewrite: [{from: /v1/audio/speech, to: /tts}]
    filters: {renameParams: {ref_audio: speaker_ref_b64, language: lang}}
```

## Async jobs

Video generation runs for minutes, longer than clients and fronting proxies
hold a connection, so its backends answer a submit at once and run the job in
the background. The router speaks one job API for this, the OpenAI-style
`/v1/videos` (create, retrieve, content, delete), to clients and to backends.
A backend that already speaks that job API is proxied unchanged. A stanza
whose backend has its own job shape sets `job.adapter`:

```yaml
job:
  adapter: openai   # default: the backend speaks /v1/videos
  adapter: sdcpp    # sd-server /sdcpp/v1/vid_gen + /sdcpp/v1/jobs/{id}
```

The adapter translates the same four calls; no backend-specific code lives
outside it. `openai` is the default for a stanza with no `job` block. The
`sdcpp` adapter maps JSON or multipart submits onto `vid_gen` fields and
statuses (`generating` → `in_progress`, `cancelled` → `failed` with an error
code), serves `result.b64_json` from `GET /v1/videos/{id}/content`, and sends
`DELETE` to the cancel route. Remix is not available through the `sdcpp`
adapter. A multipart `input_reference` becomes `init_image`; a 20 MB image
rides the normal streaming upload path.

- **Submit.** A `2xx` answer to `POST /v1/videos` whose JSON has an `id`
  records the job: id, model, the local instance or peer that took it, and the
  time. The router records it before the answer reaches the client, so a poll
  can never beat it. The submit is detached from client cancel: a client that
  drops mid-submit still leaves a job the router knows about.
- **Hold.** The job takes one occupancy slot on its instance, the same count a
  mid-proxy request takes, so the idle sweep and eviction leave the model
  alone and a competing load waits or spills. The router polls
  `GET /v1/videos/{id}` on the target every 2 seconds. `completed`, `failed`,
  `cancelled`, a `404`, the model being unloaded, or `job_ttl_minutes`
  (default 60) ends the hold. The idle clock starts when the job ends, so
  `idle_ttl_seconds` is the window to fetch the content. `slots` does not
  bound jobs: a second submit while one runs queues on the same instance.
- **Follow-ups.** `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content` and
  `DELETE /v1/videos/{id}` (any path under `/v1/videos/{id}`) name no model.
  The router looks the id up and proxies to the recorded instance or peer. It
  never loads a model for a follow-up. An unknown id is `404` with a hint to
  submit again; a job whose model is no longer loaded is `404` too. A
  `POST /v1/videos/{id}/remix` makes a child job, which the router records
  and holds like a submit.
- **Cancel.** Closing the client connection does not stop a job. A `2xx`
  answer to `DELETE` ends the job, frees the hold and forgets the id.
- **Peers.** A job that spilled to a router peer is recorded with that peer.
  Follow-ups and the router's polls go there with `X-Model-Router-Peer`; the
  peer records the same job against its own instance and holds it.
- **Table.** In memory only, like Activity. `job_table_max` (default 256) caps
  it: a full table drops its oldest finished job. Running jobs are never
  dropped, so the table can run over the cap while every entry is running.
- **Restart.** A router restart stops every backend and forgets every job.
  A client polling an old id gets `404` and submits again.
- **Visibility.** `/_router/status` has `jobs` (running jobs per model id) and
  `/metrics` has `model_router_jobs_running{model}`. Activity logs each job
  as one `POST /v1/videos` row, written when it ends, with `job_id`,
  `job_state` and the duration from submit to end.

## Realtime sessions

`GET /v1/realtime` with a websocket upgrade is one long-lived session on one
instance. The router never reassembles messages. Frames, binary audio
included, are copied straight through. `docs/research/realtime-backends.md`
has the backend notes.

- **Model.** `?model=` routes like any GET. With no `model` in the URL the
  router answers the handshake itself, then waits up to 5 seconds for the
  client's first frame. If that is a text frame with `session.model` (as in
  `session.update`), the id is copied into the query and the session routes as
  if the client had sent `?model=`. The frames read so far go to the backend
  unchanged, ahead of the rest, and the backend's own `101` is dropped. With no
  model in the frame, or no frame in time, the realtime `path_default` stanza
  (or the only one) serves it. If nothing can serve it, the client gets a close
  frame instead of an HTTP status, because the `101` is already out: `1008`
  with the reason for a `4xx` (unknown model), `1011` otherwise. The router
  echoes the client's first offered subprotocol and agrees no extensions.
- **Pools** upgrade on the member they resolve to. Once the backend switches
  protocols the attempt is committed, so there is no retry on the next target.
- **Peers.** A session for a peer's model upgrades through the peer, with the
  query `model` rewritten to the peer-local id. A model read from the first
  frame is in the query by then, so it is rewritten the same way. The frame
  itself is not edited.
- **Hold.** An open session holds an occupancy slot on its instance until the
  socket closes, the same as a request mid-generation. The idle sweep and
  eviction leave the model alone, and a load that needs its VRAM waits or
  spills. The idle clock starts when the session closes.
- **Cap.** `session_max_minutes` (top level, default 0, no cap) closes a
  session that stays open longer. The client gets close code `1001`, sent
  between frames. After a second the router drops the connection to the
  backend.
- **Visibility.** `/metrics` has `model_router_realtime_sessions{model}`, the
  sessions open now. Activity logs one row per session, written when it closes
  (see Operator UI).
- **Restart.** `http.Server.Shutdown` does not wait for hijacked connections,
  so a router restart drops open sessions.

## Transcription

`api_type: transcription` serves the OpenAI `/v1/audio/transcriptions`
multipart upload. The backend chosen in `docs/research/stt-backends.md` is
whisper.cpp `whisper-server`, started on the OpenAI path, so it needs no
rewrite:

```yaml
  - model_id: whisper-large-v3-turbo
    name: "Whisper large-v3 Turbo (ggml Q5_0, STT)"
    aliases: [whisper-1, stt, transcription]
    command: >
      /opt/ai/bin/whisper-server
      --model /opt/ai/models/ggml-large-v3-turbo-q5_0.bin
      --vad-model /opt/ai/models/ggml-silero-v6.2.0.bin
      --host 127.0.0.1 --port {port}
      --request-path /v1/audio --inference-path /transcriptions
      --language auto --convert --vad --flash-attn --threads 8
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 2000
    device: 0
    spin_up_seconds: 60
    api_type: transcription
    match: {body_field: model}
    health_check: /v1/audio/health
    idle_ttl_seconds: 300
    slots: 1
```

- **Routing.** By the multipart `model` field (or `?model=`), else the
  `path_default` transcription stanza, else the only one. `peer/model` and a
  spill rewrite the multipart `model` part and stream the file part unchanged.
- **Formats.** Every `response_format` (`json`, `text`, `srt`, `vtt`,
  `verbose_json`) and `timestamp_granularities[]` passes through. `json` and
  `verbose_json` keep the backend's `application/json`. `text`, `srt` and
  `vtt` reach the client as `text/plain; charset=utf-8`, whatever type the
  backend sent (`whisper-server` answers `text/html`, `application/x-subrip`
  and `text/vtt`). A streamed (SSE or chunked) answer is flushed per chunk.
- **Translations.** This stanza serves transcriptions only: `whisper-server`
  has no translation path. Send `translate=true` on the transcription path, or
  run a second process; `docs/research/stt-backends.md` explains both.
- **Slots.** A transcription backend runs one upload at a time and holds each
  upload it accepts in memory, so the router never sends a stanza more than
  `slots` uploads at once. The rest wait in the router, unread, for up to
  `admit_timeout_seconds`, then get `503` with `Retry-After`. Other types still
  queue on the backend.
- **Long audio.** The router sets no timeout on a proxied request. A stanza's
  `request_timeout_seconds` (default none) cuts its requests off with `504`
  after that long; the load before it does not count.
- **Other paths.** `path_rewrite` (any stanza) is a list of `{from, to}`; the
  first `from` that prefixes the request path is replaced by `to` on the way to
  a local instance. Activity and metrics keep the client's path. A peer gets
  the client's path and applies its own rewrite.

## Placement

`device` says which GPUs a stanza may use:

| Value | Meaning |
|-------|---------|
| empty | Any GPU on the node. One card per instance. |
| `"1"` or `"CUDA1"` | That card only. |
| `"1\|2"` | One of the listed cards, chosen per load. Command or env must contain `{gpu}`. |
| `"0,1,2"` | One process across every listed card. No `{gpu}`. `max_instances` must be 1. |
| `"cpu"` | No card at all (a CPU speech or transcription backend). Usually with `vram_mb: 0`. |

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
- `headroom_mb` is subtracted once from each GPU's available VRAM (the
  tighter of live and ledger free) before any `vram_mb` check. A split
  model's share is checked against each card after that card's headroom.
  It does not change a stanza's `vram_mb` and is not a reservation.
- `vram_mb <= 0` with no GPU telemetry is allowed (a CPU process). A positive
  `vram_mb` is refused until the first successful poll.
- A `device: cpu` stanza never touches the ledger: `Reserve` returns no GPU
  index, so it is never admitted against a card and never evicts to fit. See
  CPU-only placement in the Placement section above.

## Eviction

Something that is mid-request, holding a running async job, still starting,
or already stopping is never unloaded. A stanza with `vram_mb <= 0` is never unloaded to make room; it
does not free a GPU. `hold_seconds` (default 0) also skips eviction for that
many seconds after the instance first reports healthy, so a long cold start
is not reclaimed before a client can reach it. The idle sweep skips a hold
the same way.

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

**Preload restore.** A model listed in `preload` that was evicted to make
room comes back on its own. The node remembers the eviction (stale, idle or
resident) with the instance that caused it, and when a model stops — the
idle sweep, or `POST /_router/unload` — starts the preload models again in
config order. A restore runs only when the model fits without evicting
anything (`CanAdmit`) and is not already starting, so it never evicts to
bring a preload back and never competes with a load in flight; a model that
still has no room keeps its mark and is retried on a later free. A preload
the operator unloaded itself, or one a reload dropped from `preload`, stays
down. Each restore logs `restoring preload <id> after eviction by <id>`.

## Supervisor

One supervised process per instance, on that instance's port. Health is
polled until `spin_up_seconds`. While the backend is still loading, a health
body with a numeric `progress` that differs from the last one moves the
deadline to now plus `spin_up_seconds`; there is no second cap. The same
number again, or no number, expires on the deadline already set. A 200
response ends the wait whatever the body says. The process must still be alive when health
passes, so a leftover occupant of the port cannot be mistaken for this
spawn. The process is not bound to the HTTP request that triggered it.

**Load progress in the stream.** `stream_load_progress: true` (top level,
default `false`) makes a held `stream: true` `POST /v1/chat/completions` to a
local backend show life while the model starts. Each second the router writes
an SSE chunk (`role: assistant`, `reasoning_content: "loading model… 42%"`)
whenever the text changes; the percentage is the last health `progress`
(read as a 0-1 fraction). Once the backend is healthy the real stream follows
on the same response. If the start fails after the first chunk, the client gets
a `data: {"error": ...}` event and `data: [DONE]`, and the router does not try
a spill target, because the `200` is already sent. A load that finishes before
the first tick writes nothing. Non-streaming requests and every other route are
unchanged: the connection is held and the body stays empty until the response.

An unexpected exit while the model is supposed to be running restarts it
with backoff from 1 second, doubling to 30 seconds, until unload or
shutdown. Stop is SIGTERM to the process group, then SIGKILL. On Windows the
stop path is process-kill only.

A model that never becomes healthy is a different event from one that crashes
after it was healthy (ADR 0001). After the one retry inside a start, the
failed start opens a **start cooldown** for that model on this node: 10
seconds, doubling on each consecutive failed start up to 5 minutes. A start
that becomes healthy resets the next window to 10 seconds. The windows are
fixed, not config. Every copy of the model shares the window.

- A request inside the window gets `503` with `Retry-After` set to the whole
  seconds left. It spawns nothing and evicts nothing.
- `POST /_router/load` from a peer gets the same `503` and `Retry-After`, and
  this node does not call another node. A request proxied from a peer is
  refused here and not sent onward.
- Other models on the node start normally, and a generation already in
  flight keeps running.
- A crash restart does not open or wait on a start cooldown.

Extra-instance ports are allocated above every stanza port and every
`reserved_ports` entry, in config order. `-check` prints them.

## Telemetry

Local poll default 2 seconds. Peer sync is a pull of `GET /_router/telemetry`
every 5 seconds. Cached telemetry older than 15 seconds is unknown, and that
router peer is left out of candidate selection. `kind: openai` peers are not
polled.

The snapshot a node publishes:

- Per GPU: index, name, total MB, free MB, free MB if stale models were
  evicted, free MB if idle models were evicted. All three free figures are
  after `headroom_mb`.
- Each running instance: `model_id`, instance key when it is not instance 0,
  `fresh` or `stale`, VRAM budget, GPU, slot count, in-flight count. A
  `device: cpu` instance has no GPU entry (`gpu: none`).

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
| `GET`, `DELETE /v1/videos/{id}`, `GET /v1/videos/{id}/content` | Async job follow-ups, routed by job id to the instance or peer that owns the job. Unknown id: 404. See Async jobs. |
| `GET /_router/status` | Node name, catalog, local telemetry, cached peers, running async jobs per model (`jobs`). |
| `GET /_router/telemetry` | The snapshot peers pull. |
| `GET /_router/logs` | Router, upstream, and mesh rings, plus a scrape of each running backend's `/metrics`. |
| `GET /_router/logs/{model_id}` | One model's log ring as JSON (`lines`), for the UI's polling fallback when a proxy holds the live stream open (ids may contain slashes). Unknown id: 404. |
| `GET /logs` | Recent combined log as `text/plain`; `Accept: text/html` redirects to `/ui/`. |
| `GET /logs/stream`, `/logs/stream/proxy`, `/logs/stream/upstream`, `/logs/stream/mesh` | Live plain-text stream: combined, router-only, all backend output, mesh. History first unless `?no-history`. |
| `GET /logs/stream/{model_id}` | One model's backend output (ids may contain slashes). Unknown id: 404. A model named `proxy`, `upstream` or `mesh` is shadowed by the filter paths. |
| `GET /_router/activity`, `GET /_router/activity/{id}`, `GET /_router/activity/{id}/thumb`, `GET /_router/activity/{id}/audio` | In-memory generation log, request/response captures, and the image thumbnail or speech audio kept for the row (404 when nothing was kept). |
| `GET /upstream/{model_id}/...` | Strip the prefix and proxy to that local backend. |
| `GET /ui/` | Embedded operator UI. |
| `GET /health` | `OK`. |
| `GET /metrics` | Prometheus: up, running models, running async jobs per model, GPU free and reclaimable, peer freshness, requests, latency, tokens by kind (`prompt` includes cache hits, `cached`, `completion`), and prefill/decode phase tokens and seconds (`model_router_phase_tokens_total` / `model_router_phase_seconds_total{phase="prefill"|"decode"}`; tokens ÷ seconds is pp or tg speed). |

`api_type` and `capabilities` are published in `/v1/models` and
`/_router/status` (each catalog row carries `api_type` and `capabilities`), and
the UI uses them to pick which models the Playground tabs offer. `api_type` also
gives a model-less request its default route (see the api_type table in the
Config section). A request that names a model, or a stanza's explicit
`match.path_prefix`, still wins over that fallback.

## Operator UI

`/ui/` is HTML, CSS, and script embedded in the binary. Catalog Load and
Unload start and stop this node's stanzas only. Choosing a remote `model_id`
in the Playground is an ordinary `/v1/...` request, so this node
spills or proxies. Activity keeps the last 200 proxied requests, with at
most 256 KB per body and 8 MB of captures, in memory only. An image response
also keeps a downscaled thumbnail (160 px long edge, JPEG) in that budget. A
media response (`audio/*`, `video/*`, `application/octet-stream`) is not
captured as text; its row keeps the content type, byte count and a body hash
prefix, and a generation response of at most 256 KB is kept whole in the
budget for the speech player. Audio, video, realtime and voice library rows
carry a `media` summary instead of token stats: speech (characters, voice,
length from the WAV header), transcription (file name, upload bytes, output
characters, language, first 200 characters), video and audio_gen jobs (job
id, state, length; the submit response only), realtime (one row per session,
written when the websocket closes, with frames each way and the close code),
and `admin` for `POST /v1/audio/voices` and `DELETE /v1/audio/voices/{id}`.
`/metrics` adds `model_router_media_requests_total` and
`model_router_media_seconds_total` per `api_type`.
It resets on restart. Authorization and cookie headers are not stored.

## Still out of scope

- Preempting a generation that is already in flight.
- Auth or TLS on the mesh.
- Dynamic discovery, consensus, and node autoscaling.
