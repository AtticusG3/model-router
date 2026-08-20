# model-router

Backend-agnostic model router for the homelab fleet (replaces llama-swap
fleet-wide). One static binary runs on every node; per-node behaviour comes
entirely from a YAML config. See `SPEC.md` (goals/architecture) and `PLAN.md`
(fleet survey, pitfalls, and the decisions that fit this network).

## What it does

- Serves OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/embeddings`,
  `/v1/rerank`, `/v1/images/generations`, `/v1/models`, …) and sd.cpp routes
  (`/sdapi/v1/*`) by loading the right backend on demand.
- Supervises backend processes (spawn, health-check polling, crash/restart).
  Idle TTL marks a loaded model stale; it stays resident until another load
  needs the VRAM (reload is expensive, unload is cheap).
- Admission uses live nvidia-smi free VRAM, with a per-GPU reservation ledger
  so concurrent loads cannot double-book during spin-up. A node is the sole
  authority over its own GPUs (no split-brain).
- Advertises to peers every few seconds: loaded models (fresh/stale), live
  free VRAM, and free VRAM if stale models were evicted.
- Spills requests to peers when the local node can't serve them (model not
  local, or GPU full): `POST /_router/load` on the candidate, then a streamed
  transparent reverse proxy.
- Works fully offline: a node serves everything in its own stanza catalog even
  if the LAN is gone; unreachable peers are excluded via telemetry staleness.
- Includes an embedded operator WebUI at `/ui/` with model/GPU dashboards,
  load/unload controls, streaming chat, image generation, live logs, and metrics.

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
| `/_router/load`, `/_router/unload` | peer-facing control (load = this node's own admission control) |
| `/_router/status`, `/_router/telemetry`, `/_router/logs` | status, peer telemetry, and recent router events |
| `/ui/` | embedded operator WebUI (dashboard, controls, chat, image, logs/metrics) |
| `/health`, `/metrics` | health + minimal Prometheus text |

## Config

Per-node YAML with `stanzas` (SPEC schema + `device`, `aliases`, `env`,
`port`), optional `pools` (compat spillover ids such as `coding-pool`; not
listed), `peers` (`kind: router` for fleet nodes, `kind: openai` for
proxy-only upstreams like openrouter), `preload`, and `telemetry` tuning.
Staged fleet configs are in `configs/`; the swap procedure is in
`deploy/swap-over.md`.

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
cmd/model-router/        the router binary
cmd/fake-model/          test backend (OpenAI + sdapi surface)
internal/config/         config loading/validation
internal/router/         matcher, admission, supervisor, telemetry, pools, proxy, server
configs/                 staged per-node configs (buster, nomad, digger, gareths-homelab)
deploy/                  systemd unit + swap-over procedure
test/                    smoke-test configs + launcher
```
