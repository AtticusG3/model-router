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
- Idle TTL marks a loaded model stale; it stays resident until another load
  needs the VRAM. Last resort: an idle resident (including ttl=0) is evicted
  if it is not mid-generation.
- Admission uses live nvidia-smi free VRAM, with a per-GPU reservation ledger
  so concurrent loads cannot double-book during spin-up. A node is the sole
  authority over its own GPUs (no split-brain).
- Advertises to peers every few seconds: loaded models (fresh/stale), live
  free VRAM, and free VRAM if stale or idle models were evicted.
- Spills requests to peers when the local node can't serve them (model not
  local, or GPU full): `POST /_router/load` on the candidate, then a streamed
  transparent reverse proxy.
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
| `/_router/activity`, `/_router/activity/{id}` | in-memory generation request log and request/response captures |
| `/ui/` | embedded operator WebUI (dashboard, activity, controls, chat, image, logs/metrics) |
| `/health`, `/metrics` | health + Prometheus (up, running models, GPU free/reclaim, peer freshness) |

## Operator WebUI (`/ui/`)

Catalog **Load** / **Unload** only start and stop **this node's** stanzas.
Remote rows are Mesh routed: the router picks a peer when a request names
that `model_id`. To probe that path (e.g. on Nomad, `agents-a1` then
`qwen3.8-27b`), choose the id in **Chat lab** and send — that is a normal
`/v1/chat/completions` request, so the node `POST`s `/_router/load` on a
fresh peer and streams the reply. Image lab is the same for `api_type:
image` (or Path default for the configured sd.cpp route).

The chat/image selectors keep the current choice across the 3s status poll.
**Activity** lists proxied generation requests on this node (token stats, charts,
click a row for request/response). Captures live in memory only (256 KB per
body, 8 MB total) and reset on restart. Hard-refresh `/ui/` after deploying a
new binary.

## Config

Per-node YAML with `stanzas` (SPEC schema + `device`, `aliases`, `env`,
`port`), optional `pools` (compat spillover ids such as `coding-pool`; not
listed), `peers` (`kind: router` for fleet nodes, `kind: openai` for
proxy-only upstreams like openrouter), `preload`, and `telemetry` tuning.
Staged fleet configs are in `configs/` (plus
`qwen-fixed-chat-template.jinja` for Qwen 3.5/3.6/3.8 llama-server
stanzas). The swap procedure is in `deploy/swap-over.md`.

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
cmd/fake-model/          test backend (OpenAI + sdapi surface)
internal/config/         config loading/validation
internal/router/         matcher, admission, supervisor, telemetry, pools, proxy, server
configs/                 per-node YAML + qwen-fixed-chat-template.jinja
deploy/                  systemd units + swap-over procedure
test/                    smoke-test configs + launcher
```
