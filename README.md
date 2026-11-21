# model-router

Backend-agnostic model router for the homelab fleet (replaces llama-swap
fleet-wide). One static binary runs on every node; per-node behaviour comes
entirely from a YAML config. See `SPEC.md` (goals/architecture), `PLAN.md` (fleet survey), and
`docs/vram-fit-ladder.md` (shared `model_id`s and how to fit a weight on a GPU).

## What it does

- Serves OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/embeddings`,
  `/v1/rerank`, `/v1/images/generations`, `/v1/models`, …) and sd.cpp routes
  (`/sdapi/v1/*`) by loading the right backend on demand.
- Supervises backend processes (spawn, health-check polling, crash/restart).
- `idle_ttl_seconds > 0` unloads an idle instance on a 30s sweep once that
  TTL passes and nothing is in flight. `0` is resident and is never swept.
  A load that needs the VRAM can still evict stale models, then other idle
  models, then residents, as long as they are not mid-generation.
- A `preload` model evicted to make room comes back on its own: the node
  remembers it and starts it again, in config order, once it fits without
  evicting anything. A preload the operator unloaded, or one a reload
  dropped from `preload`, stays down.
- Admission uses live nvidia-smi free VRAM, with a per-GPU reservation ledger
  so concurrent loads cannot double-book during spin-up. A node is the sole
  authority over its own GPUs (no split-brain).
- Peers pull `GET /_router/telemetry` every few seconds: the catalog of
  local models it shares, loaded models (fresh/stale), each copy's image
  pipeline caption, live free VRAM, and free VRAM if stale or idle models were
  evicted. Requests reach whatever a router peer's catalog advertises, so for
  routing `peers[].models` is only needed for `kind: openai` peers and older
  routers. A `peer/model` pool target still needs the model in that peer's
  `models` list: pool targets are checked against config only, and an unlisted
  one fails the load or reload with `pool ... unknown target`. Optional LAN discovery scans
  private /24s at startup and on a backoff timer for router peers the config
  does not name.
- Spills requests to peers when the local node can't serve them (model not
  local, or GPU full): `POST /_router/load` on the candidate, then a streamed
  transparent reverse proxy. If no peer is empty right now, the origin still
  picks a node that lists the model and that node waits/evicts instead of 503.
- Works fully offline: a node serves everything in its own stanza catalog even
  if the LAN is gone; unreachable peers are excluded via telemetry staleness.
- Includes an embedded operator WebUI at `/ui/` (see below).

## Build

```bash
# Go toolchain lives at ~/go-toolchain (portable, no system install)
export PATH=$HOME/go-toolchain/go/bin:$PATH
export GOPROXY=https://proxy.golang.org,direct
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/model-router ./cmd/model-router
```

## Run

`configs/buster.yaml` is a **byte-identical mirror of the live production config**
at `/opt/ai/config/model-router.yaml`. Production is the authority; this copy exists
so the repo shows what the box actually runs. Do not hand-edit it — edit
`/opt/ai/config/model-router.yaml` (or here and then install it), and re-sync:

```bash
cp /opt/ai/config/model-router.yaml configs/buster.yaml
diff -q configs/buster.yaml /opt/ai/config/model-router.yaml && echo in sync
```

It drifted once for three weeks (Oct 2026) and ended up describing a llama.cpp
topology that no longer existed, so treat a non-empty `diff` as a bug.

```bash
./build/model-router -config configs/buster.yaml -node buster
./build/model-router -check -config configs/buster.yaml   # validate only
```

Flags: `-config` (default `/opt/ai/config/model-router.yaml`), `-listen`
(override), `-node` (default hostname), `-verbose`, `-check`.

## HTTP API

| Path | Purpose |
|------|---------|
| `/v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings`, `/v1/rerank`, `/v1/images/*`, `/infill`, `/completion` | OpenAI/llama-server passthrough |
| `/sdapi/v1/*` | sd.cpp/A1111 passthrough |
| `/v1/models` | unique mesh models (local + reachable remotes, no selectors) |
| `/_router/load`, `/_router/unload` | local-only control (this node's admission; peers call these) |
| `/_router/status`, `/_router/telemetry`, `/_router/logs` | status, peer telemetry, router/upstream/mesh rings, backend `/metrics` scrapes |
| `/logs`, `/logs/stream[/proxy\|/upstream\|/mesh\|/{model_id}]` | plain-text logs: `/logs` is the buffer, the stream paths stay open (history first unless `?no-history`) |
| `/_router/logs/{model_id}` | one model's ring as JSON, for the Logs page when a proxy buffers the live stream |
| `/_router/activity/stream` | SSE feed for the Activity page: a `snapshot` event shaped like `/_router/activity`, then a `done` event per finished request (local, or a peer request it showed running) and a `progress` event listing the requests running here and on peers whenever that list changes |
| `/_router/activity`, `/_router/activity/{id}`, `/_router/activity/{id}/thumb`, `/_router/activity/{id}/audio`, `/_router/activity/{id}/image/{side}/{n}` | in-memory generation request log, request/response captures, the image thumbnail or speech audio, and request (`req`) or response (`resp`) image previews |
| `/v1/videos`, `/v1/videos/{id}[/content]` | async video jobs: create (JSON or multipart with `input_reference`), poll, fetch, delete |
| `/v1/audio/generations`, `/v1/audio/generations/{id}[/content]` | async music and sound-effect jobs, same job shape as video: create (JSON, or multipart with a reference clip), poll, fetch, delete |
| `/ui/` | embedded operator WebUI (dashboard, activity, controls, Playground tabs including music and sound effects, logs/metrics) |
| `/health`, `/metrics` | health + Prometheus (up, running models, GPU free/reclaim, peer freshness, tokens by kind incl. prompt cache hits, prefill/decode phase tokens and seconds) |

## Video

Video generation is served through the same async job API as the OpenAI video
shape. `POST /v1/videos` returns a job id at once; poll `GET /v1/videos/{id}`
until `completed`, fetch the clip from `GET /v1/videos/{id}/content`, and end
it with `DELETE /v1/videos/{id}`. JSON and multipart submits both work; a
multipart `input_reference` is image-to-video input. A request with no `model`
reaches the `api_type: video` stanza marked `path_default`, or the only video
stanza. Video models advertise `capabilities` such as `audio_out` in
`/v1/models`.

A backend that speaks `/v1/videos` needs no config beyond `api_type: video`. A
backend with its own job shape names an adapter:

```yaml
api_type: video
job: {adapter: sdcpp}   # sd-server /sdcpp/v1; openai is the default
```

The `sdcpp` adapter maps submit/poll/content/cancel onto sd-server's native
`vid_gen` and `jobs` routes; the router still records, holds and routes the job
itself, and an unknown id is always a local `404`. If the backend exits while a
job runs, the job ends `failed` immediately instead of polling until
`job_ttl_minutes`.

## Music and sound effects

`api_type: audio_gen` runs on the same job layer as video, under
`/v1/audio/generations`: `POST` returns a job id at once, poll
`GET /v1/audio/generations/{id}` until `completed`, fetch the audio from
`GET /v1/audio/generations/{id}/content` (streamed, not buffered), and end it
with `DELETE /v1/audio/generations/{id}`. Create with JSON (`prompt`,
`lyrics`, `seconds`, `seed`, `response_format`, ...) or multipart to attach a
`reference_audio` clip; the router reads only `model` and passes the rest on.
A request with no `model` reaches the `audio_gen` stanza marked
`path_default`, or the only one. The router holds the model while the job
runs and logs one Activity row per job.

The backend must speak `/v1/audio/generations` itself, or sit behind a small
adapter in its stanza `command`; there is no `job` block for `audio_gen`.
Backend choice and field mapping: `docs/research/music-sfx-backends.md`. The
Playground **Music & SFX** tab takes a prompt, lyrics, duration and seed,
follows the job, then plays the result and offers a download.

```yaml
  - model_id: music-gen
    command: /opt/ai/bin/music-server --port {port}   # serves /v1/audio/generations
    vram_mb: 12000
    device: 0
    api_type: audio_gen
    capabilities: [audio_in, audio_out]
    match: {path_default: true}
    health_check: /health
    idle_ttl_seconds: 900
    port: 5940
```

## API keys for public clients

Clients on loopback, RFC1918, link-local (`169.254.0.0/16`, `fe80::/10`), IPv6
ULA (`fc00::/7`) and Tailscale (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`)
addresses need no key. Any other client must present a configured key on every
route except `/health`, operator and inference alike. With no keys configured,
public requests get 401.

```yaml
security:
  api_keys: [replace-with-a-long-random-key]
  trusted_proxies: [10.99.0.5, 10.99.0.0/24]   # optional; placeholders, see Overlay below
```

`MODEL_ROUTER_API_KEYS` (comma separated) adds keys from the environment.

The client IP is the socket peer. `X-Forwarded-For` / `X-Real-IP` are believed
only when the peer is in `trusted_proxies`; otherwise they are ignored.

A public client passes the key as `Authorization: Bearer <key>` or
`x-api-key: <key>`. A browser opening `/ui/` from a public address gets the
browser's own sign-in prompt: enter any user name and the key as the password.
The browser then sends it on the UI's own calls. Keys are never logged, and
activity captures redact `Authorization` and `x-api-key`. Once the router has
accepted a request, it drops the browser's `Origin`, `Referer` and
`Sec-Fetch-*` headers before forwarding to a backend or peer when the request
carries no `Origin` or comes from the router's own `/ui/` page; any other
`Origin` stays in place for the backend to judge. The capture still shows the
headers as the client sent them.

Peer routers call each other without a key, so keep `peers[].base_url` on a
private or Tailscale address.

## Operator WebUI (`/ui/`)

Catalog **Load** / **Unload** only start and stop **this node's** stanzas.
The **Peers** cell has one chip per configured peer that serves the id:
`loaded` (with GPU and in-flight/slots), `available` (advertised, not running)
or `stale` (its telemetry is too old to trust); hover a chip for when it was
last seen. Remote rows are mesh routed: the router picks a peer when a request
names that `model_id`. To probe that path (e.g. on Nomad, `agents-a1` then
`qwen3.8-27b`), choose the id in the **Playground** Chat tab and send — that is a normal
`/v1/chat/completions` request, so the node `POST`s `/_router/load` on a
fresh peer and streams the reply. The Image tab is the same for `api_type:
image` (or Path default for the configured sd.cpp route).

The Chat, Transcription, Speech and Video tabs can record from the browser's
mic and camera: photos, clips and voice notes for chat models flagged `vision`,
`video_in` or `audio_in`, a mic take for transcription, a voice clone for
speech, and a camera reference for video. The browser only allows this on
HTTPS or `localhost`. See
[Use the mic and camera in the Playground](docs/playground-mic-and-camera.md).

The Realtime tab opens a websocket session on `/v1/realtime?model=<id>` for an
`api_type: realtime` entry and streams text replies. The router passes the
upgrade through to the backend or peer that serves the model and holds the
model loaded while the session is open. A client that cannot put the model in
the URL can name it in its first `session.update` instead (SPEC.md, Realtime
sessions). A browser sends its UI sign-in key on the handshake.

The Transcription tab uploads a file to `/v1/audio/transcriptions` with the
chosen response format and language (Backend default sends none). An SRT or
WebVTT result gets Copy and Download buttons; Copy needs the page on HTTPS or
localhost, Download works anywhere.

The chat/image selectors keep the current choice across the 3s status poll.
**Activity** lists proxied generation requests on this node (token stats, charts,
click a row for request/response). Requests still running, here or on a
router peer, show above the history with a spinner, elapsed time and tokens so
far, and turn into the finished row in place. A peer's finished row is the
summary from its telemetry, with a link to that peer's UI; a request this node
sent to a peer shows once. A running row that drops out without finishing
(the peer went stale) is marked lost and fades after a minute. Peer rows need the live stream: when a proxy
buffers it and the page falls back to polling, only this node's requests show. Captures live in memory only (256 KB per
body, 8 MB total) and reset on restart. A longer JSON body or SSE stream is
clipped inside so it still parses: the longest strings are shortened, base64
images become `[image 0: png 1.3 MB]`, the middle of a long stream becomes one
`: [clipped N events, …]` comment, and the dialog lists the clipped fields.
Other bodies are cut at 256 KB and marked truncated. Image responses also keep a downscaled
thumbnail (long edge 160 px, JPEG), counted in that 8 MB. A media response
(`audio/*`, `video/*`, `application/octet-stream`) keeps only its content type,
byte count and a body hash; a speech response of at most 256 KB is also kept
for a player. Audio, video, realtime and voice library rows show a media
summary instead of token stats (characters and voice, file name and transcript
preview, job id and state, or one row per realtime session with frame counts
and close code). Hard-refresh `/ui/`
after deploying a new binary.

## Config

Per-node YAML with `stanzas` (SPEC schema + `device`, `aliases`, `env`,
`port`), optional `pools` (compat spillover ids such as `coding-pool`; not
listed), `peers` (`kind: router` for fleet nodes, `kind: openai` for
proxy-only upstreams like openrouter), `preload`, `telemetry` tuning and
`discovery`. Staged fleet configs are in `configs/` (plus
`qwen-fixed-chat-template.jinja` for Qwen 3.5/3.6/3.8 llama-server
stanzas). The swap procedure is in `deploy/swap-over.md`.

### Addresses, placeholders and the per-node overlay

Tracked files carry **placeholder addresses only** (#184/#195): the LAN /24 is
written as `10.99.0.x` and tailnet hosts as `203.0.113.x`, keeping the original
last octet. The real values live in an untracked overlay that is merged over the
tracked config at load time:

```
/opt/ai/config/model-router.yaml         <- tracked config, placeholders
/opt/ai/config/local/buster.yaml         <- overlay, REAL values, gitignored
```

`model-router` looks for `<config dir>/local/<node>.yaml` by default, where
`<node>` is `-node` or the hostname; `-overlay <path>` names one explicitly, and
`-overlay ""` is not needed because the default is the convention. A missing
overlay is normal — a fresh checkout runs on the tracked file alone. Maps merge
recursively and the overlay wins; lists (such as `peers`) and scalars replace the
base value. An overlay that exists but does not parse or validate is an error,
never a silent fallback. `-check` prints `overlay: <path>` when one was merged.

`deploy/overlay-split.py` builds the pair from a live config (it writes
`configs/local/<node>.yaml` and rewrites `configs/<node>.yaml` with
placeholders); `--check` fails if a tracked file still holds a real address. A Go
test (`internal/config`) enforces the same rule in `go test ./...` and `make
check`, so a real address cannot be committed by accident.

`discovery` (default on) scans the local network for router peers at startup
and on a backoff timer. `ports` default to `[18080, 8081, 8082]`; `subnets` is
empty by default and scans each private IPv4 /24 on an up, non-loopback
interface. `exclude` drops CIDRs or bare IPs. The gap starts at
`interval_seconds` (30), doubles to `max_interval_seconds` (600, or
`interval_seconds` when that is larger) while nothing changes, and resets when a
peer appears or drops. Discovered peers are named by
their telemetry `node`, advertise the catalog that telemetry reports, and are
dropped after 3 scans without an answer. Configured peers always win and are
never dropped. `enabled: false` makes no scan connections.

`admit_timeout_seconds` (top level, default 60) is the longest a request waits for
local GPU room while peers are re-tried. After that it gets `503` with
`Retry-After`. A model that pinned models leave no room for fails at once.

`peer_spill` (per stanza, default `auto`) decides when a local model may hand
a request to a peer. `auto` spills as soon as local room is blocked or every
slot is busy. `on_failure` waits for local admission and the load instead,
walking peers only after a hard start failure or an admit timeout. `never`
also waits, but a hard start failure is answered here with `503` instead of
walked. A peer-qualified id and a request proxied in from a peer are
unaffected.

`share: false` (per stanza, default `true`) keeps a model off the mesh: it is
left out of the telemetry catalog and loaded list, its requests are left out of
the telemetry activity feed (`in_progress`, `recent`), and a request proxied in
from a peer (or a peer's `POST /_router/load` or `/upstream/`) for it gets
`404 model not shared`, even if that peer's config still names it, and whether
it is named by id, alias, filter-set id or a pool (a pool skips unshared members
for a peer). Local clients are unaffected, and the local Activity page still
lists them.

**Upgrade every caller before you set `share: false`.** A router on 2252e2b or
earlier does not send `X-Model-Router-Peer` on its `POST /_router/load`, so
this node takes it for a local client. If that caller still lists the model in
its `peers[].models`, its load goes through: the model is loaded here, possibly
evicting another one, and only the proxied request after it gets
`404 model not shared`. Upgrade those callers first, or remove the model from
their `peers[].models`.

`max_request_body_mb` (top level, default 256, at most 1048576 = 1 TiB; a stanza may override it) caps
one request body. Over the cap the client gets `413` before any model is
loaded. A multipart upload is streamed or spooled to a temp file, not held in
RAM. Media responses (`audio/*`, `video/*`, `application/octet-stream`) are
flushed per chunk and their Activity row keeps the content type, byte count and
a body hash instead of the body (a speech response of at most 256 KB is kept
for the Activity player).

`voice_dir` (top level, empty by default) enables the router voice library: one
`<id>.wav` clip and one `<id>.json` sidecar per cloned voice. `voice_max_mb`
(default 10) and `voice_max_seconds` (default 60) cap an uploaded clip. A
speech stanza may set `clone_mode: ref_audio` (default) or `speaker_ref_b64` to
pick the reference shape its backend takes. See Speech and `SPEC.md`.

`stream_load_progress: true` (top level, default off) writes "loading model… N%"
chunks into the reasoning stream of a `stream: true` chat completion while its
model starts, so chat UIs do not look hung. See the Supervisor section of
`SPEC.md`.

`device` picks GPUs:

- `"1"` pins the stanza to one card.
- `"1|2"` lets the router place each load on one listed card (empty means
  any card): a card without a copy of the model first, then the tightest
  fit, then list order. Eviction only touches the chosen card (stale models,
  then idle, then resident; fewest MB). A `"1|2"` stanza must use `{gpu}`,
  usually `CUDA_VISIBLE_DEVICES={gpu}` with `--device CUDA0`.
- `"0,1,2"` splits one model across all listed cards.
- `"cpu"` is a GPU-less backend (a CPU speech or transcription server), usually
  with `vram_mb: 0`. It takes no card, holds no reservation and is never evicted;
  `/_router/status` shows it with `gpu: none`.

`max_instances: N` lets a model run up to N copies on separate cards. An extra
copy starts only when every copy has all its slots busy and a card has room
without evicting. Extra copies get ports above the highest stanza port
(`-check` lists them), and each one idles out on its own.

`api_type` is validated against a fixed vocabulary: `chat`, `embedding`,
`rerank`, `image`, `speech`, `transcription`, `realtime`, `video`, `audio_gen`.
An empty value means `chat`; anything else fails `-check` and lists the valid
values, so a typo like `transcribe` is caught at load. `-check` prints the type
per stanza.

`capabilities` is an optional list per stanza: `vision`, `video_in`,
`audio_in`, `audio_out`, `tools`, `reasoning`, and for video models `duration`,
`resolution`, `seed`. An unknown flag fails load. Capabilities are published in
`/v1/models` and `/_router/status`, so a vision chat model (started with an
`--mmproj`) is `api_type: chat` plus `capabilities: [vision]`. `video_in` marks
a chat model that takes `video_url` parts itself (a vLLM Qwen-VL server, not
llama-server); without it the Playground sends a camera clip as sampled frames.
The video flags control which optional fields the Playground's Video tab shows.
A speech stanza's `clone_mode` is published next to its capabilities.

`job` (only on `api_type: video`) picks the adapter in front of the backend's
async job API: `openai` (default, the backend speaks `/v1/videos`) or `sdcpp`
(sd-server's `/sdcpp/v1`). An unknown adapter fails load with the valid list.

`image_pipeline` (only on `api_type: image`) runs the image request handling
on the node that owns the stanza, for local clients and for requests proxied
in from peers. A preset name selects the steps, or a map names them:

- `sd-server` — the sd-server checks: warm the LoRA cache when the backend
  becomes ready and before a request that names a LoRA, and retry once when
  sd-server still answers `invalid lora path`.
- `ideogram4` — `sd-server` plus ideogram4-format caption enhancement.
- `image_pipeline: {caption: "" | ideogram4, enhancer: {model, timeout_seconds}, retry_blocked: N}`
  — the map form; `caption` is a format name (unknown names fail load), the
  enhancer model and timeout feed the caption step, and `retry_blocked` resends
  a blocked `txt2img` request with a new random seed up to N times (default 0;
  above 5 fails load).
  The reply's `X-SD-Shim-Blocked-Retries` header says how many retries ran. A
  blocked card is a desaturated, near-uniform image: the check combines mean
  channel spread, concentration in a few luminance bins, a near-uniform middle
  band, and a fullest bin in the middle of the range, so plain black or white
  backgrounds and photos with one large flat area such as an overcast sky are
  not cards.

Every preset runs the shim's img2img body checks (see `SPEC.md`): a request
with no usable `init_images[0]` gets 400 with `X-SD-Shim: reject` instead of
quietly running as txt2img. The blocked-card retry, the LoRA warm-up and the
ideogram4 caption step ship in this build too. The caption step asks a chat
model that is already running (locally or on a fresh peer, never cold-loaded)
for the caption, within `enhancer.timeout_seconds` (default 20), and falls
back to a template caption holding the prompt verbatim; the reply's
`X-SD-Shim-Prompt-Source` says which was sent (see `SPEC.md`).

A bare sd-server stanza needs no shim: run it with `sd-server --listen-ip
127.0.0.1 --listen-port {port}` with `health_check: /v1/models` (sd-server has
no `/health`) and `image_pipeline: sd-server`. The Python shim in `deploy/`
stays supported for nodes that still wrap sd-server with it. The Activity
capture keeps the client's original request body; a pipeline step can attach
notes (such as a rewritten prompt) shown with it.

A request with no `model` falls back by path: `match.path_prefix` first, then
the `api_type` that path serves — `/v1/audio/speech` and `/v1/audio/voices` → `speech`,
`/v1/audio/transcriptions` and `/v1/audio/translations` → `transcription`,
`/v1/realtime` → `realtime`, `/v1/videos` → `video`,
`/v1/audio/generations` → `audio_gen`, `/v1/images/*` and `/sdapi/v1/*` →
`image`, `/v1/embeddings` → `embedding`, `/v1/rerank` → `rerank`. That type's
`path_default: true` stanza (or its only stanza) is used; several candidates and
no default is a `400` that names them. Only one stanza per `api_type` may be
`path_default`; `-check` rejects a second.

### Speech

An `api_type: speech` stanza serves `/v1/audio/speech` (and
`/v1/audio/speech/batch`) and the voice routes under `/v1/audio/voices`, by
`model` or the speech default. With `voice_dir` set, the router owns model-less
`/v1/audio/voices` itself: `POST` multipart (`name`, `file`, `ref_text`,
`language`, `consent`) registers a cloned clip, `GET` lists, and
`DELETE /v1/audio/voices/{id}` removes it. The clip and sidecar survive a
restart, and a speech request that names a library voice reaches any local
instance or peer with the reference already attached in the stanza's
`clone_mode`. Without `voice_dir`, voice routes go to the speech default.
`response_format` (`mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`) and the backend's
`Content-Type` pass through, and a streamed answer reaches the client chunk by
chunk. A model-less `GET /v1/audio/voices` without a library merges the voices
of the speech default and every other speech stanza already running, each
tagged with its `model`; it never starts a second model. For a speech stanza
`slots` caps the syntheses in the backend at once: a request over it waits up to
`admit_timeout_seconds`, then gets `503` with `Retry-After`. The Playground
Speech tab lists a running model's voices, offers every format, and plays MP3
or AAC as it arrives where the browser allows. Backend choice:
`docs/research/tts-backends.md`. Example stanza (§6.1 there, command shortened):

```yaml
  - model_id: tts-clone
    command: >
      /opt/ai/venv-tts/bin/tts-serve /opt/ai/models/tts/voice-clone-base
      --served-model-name tts-clone --host 127.0.0.1 --port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
    vram_mb: 9000
    device: 0
    spin_up_seconds: 300
    api_type: speech
    capabilities: [audio_out]
    match: {path_default: true}
    health_check: /health
    idle_ttl_seconds: 900
    slots: 4
    port: 5933
```

### Transcription

`api_type: transcription` serves the OpenAI multipart upload on
`/v1/audio/transcriptions`. The backend picked in `docs/research/stt-backends.md`
is whisper.cpp `whisper-server`; started with
`--request-path /v1/audio --inference-path /transcriptions` it serves the
OpenAI path itself and ignores the `model` field:

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

- Every `response_format` (`json`, `text`, `srt`, `vtt`, `verbose_json`) and
  `timestamp_granularities[]` passes through untouched. `json` and
  `verbose_json` keep `application/json`; `text`, `srt` and `vtt` reach the
  client as `text/plain; charset=utf-8`, whatever type the backend sent.
- This stanza serves transcriptions only: `whisper-server` has no translation
  path. Send `translate=true` on the transcription path, or run a second
  process; `docs/research/stt-backends.md` explains both.
- `slots` is a hard cap for transcription: extra uploads wait in the router
  (not on the backend, which would hold each one in memory) for up to
  `admit_timeout_seconds`, then get `503` with `Retry-After`.
- The router never times out a long transcription itself.
  `request_timeout_seconds` on a stanza (default none) cuts a request off
  with `504` after that long.
- A backend that serves the API under another path gets
  `path_rewrite: [{from: /v1/audio/transcriptions, to: /inference}]` (any
  stanza; first matching prefix wins, local instances only).

## Test

```bash
make check                          # CI gate: gofmt + go vet + go test
go test ./...                       # unit tests (matcher, admission, config)
test/run-smoke.sh start             # two-router + fake-backend smoke test
test/run-smoke.sh stop
```

The smoke test runs on spare ports (18099/18100) with fake backends — it never
touches real models, GPUs, or the running llama-swap.

## Layout

```
SPEC.md                  goals & architecture
PLAN.md                  fleet survey, pitfalls, fitted design, deploy plan
docs/vram-fit-ladder.md  shared model_ids, VRAM ladder, Qwen chat template
cmd/model-router/        the router binary
cmd/fake-model/          test backend (OpenAI + sdapi + audio/voice/realtime/video-job surface)
internal/config/         config loading/validation
internal/router/         matcher, admission, supervisor, telemetry, pools, proxy, server
configs/                 per-node YAML + qwen-fixed-chat-template.jinja
deploy/                  systemd units + swap-over procedure
test/                    smoke-test configs + launcher
```
