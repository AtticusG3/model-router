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
- Admission uses live nvidia-smi free VRAM, with a per-GPU reservation ledger
  so concurrent loads cannot double-book during spin-up. A node is the sole
  authority over its own GPUs (no split-brain).
- Peers pull `GET /_router/telemetry` every few seconds: loaded models
  (fresh/stale), live free VRAM, and free VRAM if stale or idle models were
  evicted.
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
| `/_router/activity`, `/_router/activity/{id}`, `/_router/activity/{id}/thumb` | in-memory generation request log, request/response captures, and the image thumbnail |
| `/ui/` | embedded operator WebUI (dashboard, activity, controls, chat, image, logs/metrics) |
| `/health`, `/metrics` | health + Prometheus (up, running models, GPU free/reclaim, peer freshness) |

## API keys for public clients

Clients on loopback, RFC1918, link-local (`169.254.0.0/16`, `fe80::/10`), IPv6
ULA (`fc00::/7`) and Tailscale (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`)
addresses need no key. Any other client must present a configured key on every
route except `/health`, operator and inference alike. With no keys configured,
public requests get 401.

```yaml
security:
  api_keys: [replace-with-a-long-random-key]
  trusted_proxies: [10.0.0.5, 10.99.0.0/24]   # optional
```

`MODEL_ROUTER_API_KEYS` (comma separated) adds keys from the environment.

The client IP is the socket peer. `X-Forwarded-For` / `X-Real-IP` are believed
only when the peer is in `trusted_proxies`; otherwise they are ignored.

A public client passes the key as `Authorization: Bearer <key>` or
`x-api-key: <key>`. A browser opening `/ui/` from a public address gets the
browser's own sign-in prompt: enter any user name and the key as the password.
The browser then sends it on the UI's own calls. Keys are never logged, and
activity captures redact `Authorization` and `x-api-key`.

Peer routers call each other without a key, so keep `peers[].base_url` on a
private or Tailscale address.

## Operator WebUI (`/ui/`)

Catalog **Load** / **Unload** only start and stop **this node's** stanzas.
Remote rows are Mesh routed: the router picks a peer when a request names
that `model_id`. To probe that path (e.g. on Nomad, `agents-a1` then
`qwen3.8-27b`), choose the id in the **Playground** Chat tab and send — that is a normal
`/v1/chat/completions` request, so the node `POST`s `/_router/load` on a
fresh peer and streams the reply. The Image tab is the same for `api_type:
image` (or Path default for the configured sd.cpp route).

The Realtime tab opens a websocket session on `/v1/realtime?model=<id>` for an
`api_type: realtime` entry and streams text replies. The router only passes the
upgrade through to the backend or peer that serves the model; it adds no
protocol of its own. A browser sends its UI sign-in key on the handshake.

The chat/image selectors keep the current choice across the 3s status poll.
**Activity** lists proxied generation requests on this node (token stats, charts,
click a row for request/response). Captures live in memory only (256 KB per
body, 8 MB total) and reset on restart. Image responses also keep a downscaled
thumbnail (long edge 160 px, JPEG), counted in that 8 MB. Hard-refresh `/ui/`
after deploying a new binary.

## Config

Per-node YAML with `stanzas` (SPEC schema + `device`, `aliases`, `env`,
`port`), optional `pools` (compat spillover ids such as `coding-pool`; not
listed), `peers` (`kind: router` for fleet nodes, `kind: openai` for
proxy-only upstreams like openrouter), `preload`, and `telemetry` tuning.
Staged fleet configs are in `configs/` (plus
`qwen-fixed-chat-template.jinja` for Qwen 3.5/3.6/3.8 llama-server
stanzas). The swap procedure is in `deploy/swap-over.md`.

`admit_timeout_seconds` (top level, default 60) is the longest a request waits for
local GPU room while peers are re-tried. After that it gets `503` with
`Retry-After`. A model that pinned models leave no room for fails at once.

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

`max_instances: N` lets a model run up to N copies on separate cards. An extra
copy starts only when every copy has all its slots busy and a card has room
without evicting. Extra copies get ports above the highest stanza port
(`-check` lists them), and each one idles out on its own.

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
