# Open-source model-router landscape

Date of reading: 2026-10-25. Default-branch docs and source unless a URL says otherwise.

## 1. Scope and method

This note compares `model-router` (this repo) with other open-source programs that load local models on demand, proxy an OpenAI-shaped API, or place a model onto GPUs. The space, as used here, is: one or more machines with limited VRAM, a client that names a model, and a process that starts or selects a backend so that request can be served without a human restarting servers.

Claims below come from official READMEs, official docs, GitHub repo metadata, or source files fetched from GitHub or the project's own docs site. Blogs and listicles were not used. If a behavior was not in a file that was opened, the cell says `?` (unverified). `gpustack-ai/GPUStack` is not the repo that exists; the project is [gpustack/gpustack](https://github.com/gpustack/gpustack).

`model-router` today, from [README.md](../../README.md), [SPEC.md](../../SPEC.md), [PLAN.md](../../PLAN.md), and [internal/config/config.go](../../internal/config/config.go): OpenAI and sd.cpp passthrough, a process supervisor, idle TTL that marks a model stale but leaves it resident until VRAM is needed, live `nvidia-smi` admission plus a per-GPU reservation ledger, device pin / `1|2` placement / `0,1` split, `max_instances`, slots from `--parallel`/`-np`, a static peer mesh with router vs openai peers, pools, aliases, preload, an embedded `/ui/`, and Prometheus `/health` and `/metrics`. Non-goals that bound the recommendations: no central control plane, no consensus, a node must serve its own catalog with the LAN down, backend-agnostic, one static binary. No auth or TLS on the LAN. No dynamic discovery, autoscaling, or in-flight preemption. SPEC still leaves "hold the connection vs async job polling for slow spin-up" open.

## 2. Project index

Pushed-at is the GitHub API `pushed_at` on the default branch's repo, not a release tag, except where noted. License is the API `license.spdx_id`.

| Name | URL | What it is | License | Pushed (UTC) |
| --- | --- | --- | --- | --- |
| model-router | this repo | Homelab router: on-demand backends, VRAM admission, static peer spill | (internal) | (local tree) |
| llama-swap | https://github.com/mostlygeek/llama-swap | Single static binary that swaps any OpenAI/Anthropic-compatible server on demand | MIT | 2026-10-24 |
| llama.cpp server router | https://github.com/ggml-org/llama.cpp | `llama-server` with no `-m` loads several GGUF children and proxies by `model` | MIT | 2026-10-24 |
| LocalAI | https://github.com/mudler/LocalAI | Multi-backend local OpenAI engine, galleries, optional P2P | MIT | 2026-10-24 |
| GPUStack | https://github.com/gpustack/gpustack | Central server plus workers that schedule vLLM/SGLang/custom engines | Apache-2.0 | 2026-10-23 |
| Xinference | https://github.com/xorbitsai/inference | Supervisor/worker cluster; explicit `launch_model` onto engines | Apache-2.0 | 2026-10-24 |
| tabbyAPI | https://github.com/theroyallab/tabbyAPI | ExLlamaV3 OpenAI server: one loaded model, admin load/unload, draft, LoRA | AGPL-3.0 | 2026-10-23 |
| Ollama | https://github.com/ollama/ollama | Local runner with a VRAM scheduler, keep-alive, and parallel slots | MIT | 2026-10-24 |
| LiteLLM | https://github.com/BerriAI/litellm | AI gateway. Only router settings were read, not the whole proxy | API: NOASSERTION | 2026-10-24 |
| exo | https://github.com/exo-explore/exo | Home-cluster discovery and tensor/pipeline split, MLX backend | Apache-2.0 | 2026-10-13 |
| ananke | https://github.com/philpax/ananke | Linux NVML-aware proxy; llama.cpp template or arbitrary command | MIT | 2026-10-23 |
| llmux | https://github.com/doublewordai/llmux | OpenAI proxy that drain/sleep/wake via user scripts | API: none | 2026-02-25 |
| RamaLama | https://github.com/containers/ramalama | Container CLI. Multi-model serve wraps llama.cpp router mode | MIT | 2026-10-24 |
| speaches | https://github.com/speaches-ai/speaches | OpenAI audio server; loads STT/TTS/VAD models and unloads on TTL | MIT | 2026-10-22 |
| distributed-llama | https://github.com/b4rtaz/distributed-llama | Root plus workers, tensor-parallel slices, power-of-two node counts | MIT | 2026-10-14 |
| large-model-proxy | https://github.com/perk11/large-model-proxy | Per-port and OpenAI proxy; declared resource pools; LRU stop | GPL-2.0 | 2026-10-13 |

Also confirmed, not given matrix columns:

- [dollthing/goldseam](https://github.com/dollthing/goldseam) (the `avafloww/goldseam` URL redirects here). API description: warm KV-transfer llama.cpp swaps, TypeScript policy, per-GPU budget. MIT. Pushed 2026-06-22, zero stars. Its own README was not opened, so behavior past that description is unverified. Quiet enough that it is not a lift candidate.
- [lemonade-sdk/lemonade](https://github.com/lemonade-sdk/lemonade). API: local LLM server for GPUs and NPUs. Apache-2.0. Pushed 2026-10-24. No headless multi-model swap API was read, so it is omitted from the matrix.

Not read, so not scored: kubeai, llmaz, AIBrix, Dynamo, the SGLang router, mistral.rs, Aphrodite, Jan, LM Studio CLI, and gguf-switchboard (a README turned up in search; license and push time were not confirmed). llama-swap's README has no related-projects section. ananke's README compares llama-swap, large-model-proxy, and Goldseam; those comparisons were checked against the other projects' own files where those files were opened.

## 3. Feature matrix

Cells: `y` documented and implemented, `p` a related but different mechanism, `n` absent in the sources read, `?` not verified. Columns: MR model-router, swap llama-swap, lcpp llama.cpp router, LAI LocalAI, GPU GPUStack, Xinf Xinference, Tab tabbyAPI, Oll Ollama, Lite LiteLLM router settings, exo, ank ananke, mux llmux, rama RamaLama, sp speaches, dll distributed-llama, lmp large-model-proxy.

| Capability | MR | swap | lcpp | LAI | GPU | Xinf | Tab | Oll | Lite | exo | ank | mux | rama | sp | dll | lmp |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Load on the request that names the model | y | y | y | y | n | n | p | y | n | p | y | y | p | y | n | y |
| Backend is an arbitrary command | y | y | n | p | p | p | n | n | n | n | y | p | p | n | n | y |
| OpenAI chat passthrough | y | y | y | y | y | y | y | y | y | y | y | y | y | n | ? | y |
| Image or sd.cpp routes | y | y | n | y | y | y | n | ? | n | p | y | n | ? | n | n | y |
| Admission uses live free VRAM | y | n | n | p | p | ? | n | y | n | p | y | n | n | n | n | p |
| Per-model declared footprint | y | n | n | p | y | p | p | p | n | p | p | n | n | n | p | y |
| TTL marks stale; weights stay until pressure | y | n | n | n | n | n | n | n | n | n | n | n | n | n | n | n |
| Eviction skips in-flight work | y | ? | y | y | ? | ? | n | y | n | ? | y | y | ? | ? | n | y |
| Several models resident at once | y | y | y | y | y | y | n | y | y | p | y | n | p | y | n | y |
| Chooses a GPU, not only a whole machine | y | p | p | ? | y | y | y | y | n | y | y | n | n | p | n | y |
| Extra copies of one model | y | n | n | n | y | y | n | n | y | ? | ? | n | n | n | n | n |
| Concurrency cap short of the OS | y | y | y | y | ? | y | p | y | y | ? | ? | p | p | p | n | ? |
| Multi-node, no central server | y | p | n | p | n | n | n | n | n | y | n | n | n | n | n | n |
| Local catalog works with the network down | y | y | p | p | n | p | y | y | n | y | y | y | p | p | p | y |
| API keys | n | y | y | y | y | p | y | p | y | ? | ? | n | ? | y | n | ? |
| Operator UI in the process | y | y | y | y | y | y | ? | ? | ? | y | y | n | y | y | n | y |

Footnotes, one per column family:

- **swap.** [README](https://github.com/mostlygeek/llama-swap/blob/main/README.md) and [docs/config.example.yaml](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml) (the example says it lists every option). On-demand swap, `cmd`, OpenAI and `/sdapi/v1/*`, groups or matrix for concurrency, `ttl` that unloads (`0` never, global default `0`), `concurrencyLimit` (values `> 0` override an internal default of 10 and answer `429`), peers are proxy base URLs. No `vram` field, so no live VRAM admission. GPU choice is whatever the user puts in `env` / `cmd`. `apiKeys` default empty (default-allow). `/ui` and `/metrics`.
- **lcpp.** [tools/server/README.md](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md) "Using multiple models", and [server-models.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-models.cpp) `pick_victim` / `unload_lru`. Router mode is `llama-server` with no model, sources cache / `--models-dir` / `--models-preset`, `--models-max` default 4 (`0` unlimited), `--models-autoload` default on, `--parallel` is slots inside a child. LRU eviction is by resident count. `pick_victim` skips `req_count != 0` and models that are not ready or sleeping. `--sleep-idle-seconds` unloads weights from RAM. `--rpc` is a server flag; the rpc-server protocol was not read. Presets can set `load-on-startup`. Not an arbitrary-command supervisor.
- **LAI.** [core/cli/run.go](https://github.com/mudler/LocalAI/blob/master/core/cli/run.go). `--max-active-backends` (`0` unlimited) evicts LRU. `--watchdog-idle` default off, timeout `15m`, stops idle backends. `LOCALAI_VRAM_BUDGET` caps allocation (percent or absolute). Memory reclaimer default off, threshold `0.95`. `ForceEvictionWhenBusy` default false, with retries. `LOCALAI_SIZE_AWARE_EVICTION` default false evicts the largest model. `LOCALAI_MODEL_LOAD_FAILURE_COOLDOWN` default `10s`. P2P and federated flags default off. Galleries autoload by default. `api_keys.json` is named in the config-dir help.
- **GPU.** [README](https://github.com/gpustack/gpustack/blob/main/README.md): one server, workers join with `--server-url` and a token, catalog deploy downloads weights. [docs/scheduler.md](https://github.com/gpustack/gpustack/blob/main/docs/scheduler.md): filter then score; try single-GPU, then multi-GPU on one worker, then distributed, then CPU offload for backends that allow it; binpack or spread plus a model-file locality score. Replicas are a deployment property ([model deployment management](https://github.com/gpustack/gpustack/blob/main/docs/user-guide/model-deployment-management.md)). This is a control plane, not a request-time swapper.
- **Xinf.** [Using Xinference](https://inference.readthedocs.io/en/latest/getting_started/using_xinference.html): `xinference-local` starts a supervisor and a worker together; a cluster is one `xinference-supervisor` and N `xinference-worker` processes pointed at it. [launch_model](https://inference.readthedocs.io/en/latest/reference/generated/xinference.client.Client.launch_model.html): `replica` (default 1), `n_worker` (default 1), `n_gpu`, `gpu_idx`, `request_limits`, `worker_ip`. Launch is an explicit API call. [Distributed inference](https://inference.readthedocs.io/en/latest/user_guide/distributed_inference.html): `n_worker > 1` shards across workers (vLLM TP/PP). Engines include llama.cpp, vLLM, SGLang, MLX.
- **Tab.** [README](https://github.com/theroyallab/tabbyAPI/blob/main/README.md), [config_sample.yml](https://github.com/theroyallab/tabbyAPI/blob/main/config_sample.yml), [wiki: Usage](https://github.com/theroyallab/tabbyAPI/wiki/03.-Usage). OpenAI API, admin `POST /v1/model/load` and `/v1/model/unload`. Loading a different name unloads the current model. Unload terminates pending generations. Draft model block and LoRA list are config. `gpu_split` is an array of GB. Auth is admin key vs API key, with `disable_auth`.
- **Oll.** [envconfig/config.go](https://github.com/ollama/ollama/blob/main/envconfig/config.go) and [server/sched.go](https://github.com/ollama/ollama/blob/main/server/sched.go). `OLLAMA_KEEP_ALIVE` default 5 minutes (negative = forever, zero = unload when idle). `OLLAMA_MAX_LOADED_MODELS` default `0`, and the scheduler then uses `3 * GPU count` as a global loaded-runner cap (`defaultModelsPerGPU = 3`). A positive value is compared to `len(s.loaded)`, not per GPU, despite the env map string saying "per GPU". `OLLAMA_NUM_PARALLEL` default 1. `OLLAMA_GPU_OVERHEAD` reserves bytes per GPU (default 0). `findRunnerToUnload` prefers `refCount == 0`, otherwise the shortest-lived runner, and the caller waits until unload. `OLLAMA_SCHED_SPREAD` forces all GPUs. `OLLAMA_AUTH` exists and defaults off.
- **Lite.** [router settings](https://docs.litellm.ai/docs/proxy/config_settings), [routing](https://docs.litellm.ai/docs/routing), [reliability](https://docs.litellm.ai/docs/proxy/reliability). Strategies include `simple-shuffle`, `least-busy`, `usage-based-routing`, `latency-based-routing`. `allowed_fails` and `cooldown_time` pull a deployment out. `fallbacks` move to another model group after retries. `order` on deployments is ordered failover. Redis is for sharing counters across proxy instances. No local process supervisor.
- **exo.** [README](https://github.com/exo-explore/exo/blob/main/README.md). Devices discover each other (Zenoh namespace to isolate clusters). Placement preview is `GET /instance/previews` (`memory_delta_by_node`, pipeline or tensor). The user `POST /instance`s a placement, then calls `/v1/chat/completions`. `EXO_OFFLINE` uses local files. Linux builds run on CPU; GPU on Linux is described as in development. RDMA is Thunderbolt on macOS.
- **ank.** [README](https://github.com/philpax/ananke/blob/main/README.md) and [docs/memory-model.md](https://github.com/philpax/ananke/blob/main/docs/memory-model.md). First request loads; default idle unload is 10 minutes. NVML required for GPU placement. Templates `llama-cpp` and `command`. The memory model is a fitted llama.cpp (and ik_llama) estimator in `crates/estimate/`, not a generic `vram_mb`. The README's own comparison table says eviction is priority-based and switches are drain-then-restart.
- **mux.** [README](https://github.com/doublewordai/llmux/blob/main/README.md) as returned from the repo page. One active model. A request for another drains in-flight work, runs the current model's `sleep` script, runs the target `wake` script, then proxies. The process is not supervised by llmux. Last push 2026-02-25 and the API reports no license.
- **rama.** [ramalama-serve(1)](https://github.com/containers/ramalama/blob/main/docs/ramalama-serve.1.md). One model uses llama.cpp, vLLM, or MLX inside a container. Two or more model arguments, or none, start llama.cpp router mode (`--models-dir` behavior) and require a container runtime. Missing models are pulled. No VRAM ledger of its own.
- **sp.** [README](https://github.com/speaches-ai/speaches/blob/master/README.md) and [src/speaches/config.py](https://github.com/speaches-ai/speaches/blob/master/src/speaches/config.py). Request names a model; it loads and later unloads. `stt_model_ttl` and `tts_model_ttl` default 300 (`-1` never, `0` immediate). `vad_model_ttl` default `-1`. Optional `api_key`. Audio API, not a chat/image fleet router. Whether unload waits for an in-flight transcription was not re-read in current source (an old issue described a race).
- **dll.** [README](https://github.com/b4rtaz/distributed-llama/blob/main/README.md). Root loads weights and synchronizes; workers only compute a slice. Node count must be a power of two. Root is mandatory. `launch.py` downloads a named model. Not an on-demand multi-model proxy.
- **lmp.** [README](https://github.com/perk11/large-model-proxy/blob/main/README.md). On connect or OpenAI `model`, check declared resources, stop the LRU service if short, start, healthcheck, proxy. If the victims are in use, the new request waits. `ResourcesAvailable.*.CheckCommand` can shell out to `nvidia-smi` and poll; without it, amounts are not checked against real usage. `reserved_by_starting_services` is a start-up reservation. `KillCommand` for Docker. Single machine.

### llama-swap config, field by field

From [docs/config.example.yaml](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml) on `main`, which says it lists the available options. The old `proxy/config/model_config.go` path 404s on `main`; the example plus the README are the contract used here.

| Asked item | On main today |
| --- | --- |
| Groups | `routing.router.use: group` (the default). Per group: `swap`, `exclusive`, `persistent`, `members`. A model is in one group. `persistent: true` stops other groups from unloading those members. |
| Swap strategies | `group` or `matrix`. Matrix: `vars`, `evict_costs` (default 1), `sets` with `&`, `|`, `()`, `+ref`. Solver sums evict costs of running models outside each candidate set and picks the cheapest. Subsets of a set are allowed. A model in no set runs alone. `scheduler.use` documents only `fifo`, with optional per-model `priority`. |
| Hooks | Only `hooks.on_startup`: `profile` and `preload`. |
| Filters | Model and peer `filters.stripParams` and `setParams`. Model `setParamsByID` creates aliases that rewrite the body (a key ending in `?` is set-if-absent). `model` cannot be stripped or overridden. |
| Macros | Global and per-model. Reserved names `PID`, `PORT`, `MODEL_ID`. `${env.VAR}` fails config load if unset. |
| ttl | Per model, default `-1` (use global). `globalTTL` default `0` (never unload). Model `ttl: 0` never unloads. Positive ttl unloads. This is unload, not "mark stale and keep". |
| sendLoadingState | Global default false in the comments, example sets true. Injects loading status into the reasoning stream. Per-model override. |
| Concurrency | `concurrencyLimit` per model; `> 0` replaces an internal default of 10; excess is HTTP 429, not a queue. `globalConcurrencyLimit` default 0 (off) is one shared semaphore, also 429. |
| Health | `checkEndpoint` default `/health`, or `none`. Non-200 JSON `progress` is shown in the UI. `healthCheckTimeout` default 120, minimum 15, and the countdown restarts when reported progress changes. |
| Metrics | `/metrics`. Optional sqlite `store.path`. `metricsMaxInMemory` default 1000. `captureBuffer` default 10 MB. |
| UI | `/ui`, activity, logs stream, profiles API. |
| API keys | `apiKeys` default empty, default-allow. Peer `apiKey` is injected as `Authorization: Bearer` and `x-api-key`. |
| proxy | A URL field, default `http://localhost:${PORT}`. There is no `@proxy` model syntax in the example. Remote OpenAI servers are `peers.<id>.proxy`. Tailcat peers use `tailcat://` tokens. |
| depend | Not in the example. |
| checkEndpoint | Yes, see health. |
| cmdStop | Optional. Empty means SIGTERM on POSIX (Windows default `taskkill`). `unloadTimeout` default 10 seconds, then force. `${PID}` is available. |
| persistent | Group field, not a per-model flag. |
| LoRA | No LoRA stanza field. `/sdapi/v1/loras` is proxied and needs `model` in the body so the right upstream is selected. |
| Selectors | `strategy: warm`, `pin`, or `spillover`. `warm` uses the first ready target, else one already starting, else cold-starts the first. `spillover` has a per-target reservation count (default 1). |
| Profiles | Runtime pins from one id to another. `hooks.on_startup.profile` selects one. Not persisted across restart beyond that hook. |
| Other | `unlisted`, `aliases`, `useModelName`, `env`, `${PORT}` from `startPort` default 5800, `capabilities` with upstream auto-discovery, per-model `timeouts`, `compat.ignoreWebsockets`. |

`model-router` already has the subset the fleet needed: command, port macros, aliases, env, health path, ttl-as-resident (`idle_ttl_seconds: 0`), preload, spillover pools, peers, unlisted-accepted. It does not have matrix, groups, filters, sendLoadingState, api keys, profiles, or progress-extended health timeouts.

## 4. Ideas worth lifting

Ranked. Each one is something the sources actually implement. "Already" means `model-router` as described in the README, SPEC, PLAN, and `config.go`.

### 1. Failed-load cooldown — lift

LocalAI's `LOCALAI_MODEL_LOAD_FAILURE_COOLDOWN` (default 10s, doubles per consecutive failure up to 5 minutes, reset on success, `0` disables) refuses another load of a model that just failed to start, and returns 503 with `Retry-After`. The comment in [run.go](https://github.com/mudler/LocalAI/blob/master/core/cli/run.go) says this stops a polling client from respawning a crashing backend on every request.

`model-router` restarts a crashed process with backoff, and a failed spin-up releases the VRAM reservation. A client or a peer that retries immediately can still stack failed spawns, and a peer that fails `POST /_router/load` can be chosen again on the next request because telemetry only goes stale when the peer stops answering, not when one model fails to start.

This fleet has wrapper binaries and slow image servers. A crash loop on one stanza should not burn the GPU or pin a peer. Sit it on the supervisor for local failures, and on peer selection for a failed load or dial. Do not cooldown a peer for a normal generation error after the model is up.

**Verdict: lift.** Small, matches machines that already crash-restart, and does not add a control plane.

### 2. VRAM headroom, not 100% of free — lift

Ollama's `OLLAMA_GPU_OVERHEAD` reserves a byte count per GPU before the scheduler treats memory as free ([envconfig/config.go](https://github.com/ollama/ollama/blob/main/envconfig/config.go)). LocalAI's `LOCALAI_VRAM_BUDGET` caps allocation at a percent or an absolute size, empty meaning all detected VRAM ([run.go](https://github.com/mudler/LocalAI/blob/master/core/cli/run.go)).

`model-router` admits against live `nvidia-smi` free minus the reservation ledger and the stanza's `vram_mb`. That can still book the last free megabyte. CUDA context, a sick 4060, and fragmentation need a margin. Buster's 4060 was already described in PLAN as nearly full.

Sit it on admission: a node-level reserve (bytes or percent) subtracted from free before a load is accepted. Keep `vram_mb` as the per-stanza reservation. Do not replace the ledger with a percent-of-total guess.

**Verdict: lift.** It is the same admission path, with a constant the fleet can set per box.

### 3. Health progress extends the spin-up deadline — lift

llama-swap's `healthCheckTimeout` (default 120s, floor 15s) restarts its countdown when the health body reports a changed `progress` value. A non-200 body like `{"progress": 0.42, "message": "loading tensors"}` is treated as alive ([config.example.yaml](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml)).

`model-router` has a fixed `spin_up_seconds` (default 60 in code, per stanza in YAML) and polls `health_check`. A colibri or a large GGUF that is still making progress dies at the deadline and looks the same as a hung process.

Sit it on the supervisor. Only treat a changing progress field as a deadline reset. A flat non-200 still expires. Backends that do not report progress keep today's deadline.

**Verdict: lift.** It is the difference between a slow load and a dead one, which this fleet already has.

### 4. Hold the connection and stream load status — lift

SPEC leaves hold-vs-async open. llama-swap's `sendLoadingState` holds the HTTP request and writes loading lines into the reasoning stream so a chat UI shows progress instead of silence ([README](https://github.com/mostlygeek/llama-swap/blob/main/README.md), [config example](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml)). llama.cpp's router queues the request until a slot exists and does not answer with a job id ([server-models.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-models.cpp) `join` / `tick`). large-model-proxy holds the client connection, bounded by `ClientRequestBufferLimitBytes`, until health passes.

`model-router` already holds through spawn and health, then proxies. Clients see silence during colibri and image spin-up. An async job API would break Open WebUI, rag-proxy, and Hermes, which post a chat completion and read a stream.

Sit it on the proxy, in front of an already-held load. Default off, like llama-swap, so non-chat clients are unchanged. This is the hold side of the open question, not a second queueing product.

**Verdict: lift**, as the answer to that open question. Skip a job-polling API.

### 5. Request filters — lift

llama-swap `stripParams`, `setParams`, and `setParamsByID` rewrite the JSON body before the upstream sees it. `setParamsByID` mints aliases (`${MODEL_ID}:high`) that only change sampling or `chat_template_kwargs`, so one resident process serves several behaviors. A key ending in `?` fills the field only when the client omitted it. `model` is protected ([config example](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml)).

`model-router` has aliases that map ids onto a stanza, and it rewrites `model` to the peer-local id. It does not enforce sampler or template kwargs. The fleet already carries a Qwen chat template file and several names for one weight. Filters would stop a client from disabling that template, without a second process.

Sit it on the proxy, after the matcher, before the upstream dial. Peer `kind: openai` (OpenRouter) is the other user: strip fields that upstream rejects.

**Verdict: lift.** Backend-agnostic, config-only, no new process model.

### 6. Pool error fallback, separate from capacity spillover — lift

LiteLLM `fallbacks` try another model group after the configured retries, and deployment `order` fails over on connection errors, 404, and 429 ([reliability](https://docs.litellm.ai/docs/proxy/reliability), [load balancing](https://docs.litellm.ai/docs/proxy/load_balancing)). Cooldown (`allowed_fails`, `cooldown_time`) stops the router from picking a deployment that is already failing ([config settings](https://docs.litellm.ai/docs/proxy/config_settings)).

`model-router` pools spill when a target is at its slot cap, and unreachable peers drop out when telemetry is stale. SPEC/PLAN/README do not describe "this target accepted the load, then the dial or the upstream 5xx'd, try the next target." A peer that is up but whose backend just crashed still looks fresh for `peer_stale_seconds`.

Sit it on the pool resolver and the peer client. One failed attempt, then the next target. Combine with idea 1 so the bad target is skipped for a short window. Do not import latency routing, usage-based TPM, or Redis. Do not fail over mid-stream after tokens have started.

**Verdict: lift.** The mesh already has the target list. This is what happens when the chosen target is sick rather than full.

### 7. Queue while every victim is mid-generation — later

llama.cpp `pick_victim` returns empty when every resident has `req_count != 0` or is stopping; `tick` then waits instead of killing the request ([server-models.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-models.cpp)). LocalAI waits (`LRU_EVICTION_MAX_RETRIES` default 30, interval 1s) and only kills busy work if `ForceEvictionWhenBusy` is set, default false. Ollama's `findRunnerToUnload` prefers an idle runner and otherwise waits out the one with the shortest keep-alive ([sched.go](https://github.com/ollama/ollama/blob/main/server/sched.go)).

`model-router` already refuses to kill in-flight work, then spills, and 503s when nothing can take the request. The open SPEC item is priority and in-flight preemption. A bounded wait, only when the mesh has no other node, matches those three schedulers and the "do not preempt" choice.

Sit it on admission, on the held connection from idea 4. Cap the wait with `spin_up_seconds` or a new deadline so a long image job cannot stall chat forever. Do not add LocalAI's force-evict flag.

**Verdict: later.** Right behavior, but it changes the 503 contract and needs a product decision on how long to hold. Not required to replace llama-swap.

### 8. Optional sleep/wake instead of kill — later

vLLM sleep mode releases GPU memory without exiting the process. Level 1 offloads weights to CPU and drops KV; level 2 drops both. Official docs: [Sleep Mode](https://docs.vllm.ai/en/latest/features/sleep_mode/). The server exposes `POST /sleep` and `POST /wake_up`. llmux does the same shape with user `sleep` and `wake` scripts and drains in-flight work first ([llmux README](https://github.com/doublewordai/llmux/blob/main/README.md)). llama.cpp `--sleep-idle-seconds` is weaker: it unloads from RAM and pays a full reload ([server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)).

`model-router` stops the process (SIGTERM then SIGKILL). That is correct for llama-server and sd.cpp. A vLLM stanza on a box with enough host RAM could sleep instead and wake faster than a cold start. Nomad has 14 GiB RAM in PLAN, so level 1 (weights in CPU) does not fit there. Buster at 125 GiB might.

Sit it on the supervisor as optional per-stanza commands, the llmux shape, not a vLLM client baked in. Default remains kill. Skip llama.cpp idle-sleep as a default: it fights stale-until-pressure.

**Verdict: later.** Useful only for backends and hosts that can pay the RAM. Shipping it now specializes the supervisor for one engine.

### 9. GGUF estimate as a check, not the reservation — later

ananke's [memory model](https://github.com/philpax/ananke/blob/main/docs/memory-model.md) predicts llama.cpp VRAM and host RAM from GGUF metadata plus fitted coefficients, including a different curve for ik_llama. Ollama fits from the GGUF and `updateFreeSpace` rather than a user `vram_mb`. GPUStack parses GGUF for resource estimates and uses `weight * 1.2 + framework footprint` otherwise ([compatibility check](https://github.com/gpustack/gpustack/blob/main/docs/user-guide/compatibility-check.md)).

`model-router` trusts `vram_mb`. PLAN already says those numbers are measured and rounded up. ananke's own doc shows mainline and ik_llama accounting for the same Qwen MoE diverging by tens of GiB, and TurboQuant is a third runtime on this fleet. sd.cpp and vLLM are outside that estimator.

Sit a future check next to `-check`: warn when a llama-server stanza's `vram_mb` is far from the estimate. Admission stays on the declared number plus live free VRAM. Do not auto-size, and do not run the estimator for non-GGUF commands.

**Verdict: later.** A wrong estimate that becomes the reservation will OOM a GPU the ledger thought was safe.

### 10. Mesh binpack vs spread — later

GPUStack's scale-up scorer is binpack (pack onto fewer GPUs) or spread (even out workers), after it has already preferred a single GPU over a multi-GPU placement ([scheduler.md](https://github.com/gpustack/gpustack/blob/main/docs/scheduler.md)). Local placement also stops at the first strategy that fits, so it will not split a model across cards if one card is enough.

`model-router` already does the local version: `1|2` prefers an empty card, then the tightest fit, and a comma list is an explicit split. Peer choice uses cached telemetry (loaded models, free VRAM, reclaimable VRAM) and, if nobody is empty, a node that lists the model. SPEC does not name binpack vs spread across the mesh.

Sit any change on peer selection only. The local device rules should stay. Do not add GPUStack's worker registry or label selectors.

**Verdict: later.** Worth a pass once real spill logs show whether peer choice fragments the large cards. Copying the scorer before that is speculation.

### 11. Runtime profiles — later

llama-swap profiles pin an id to another id, alias, peer model, or selector, switched from the UI or `PUT /api/profiles/active`. Startup profile comes from `hooks.on_startup.profile` and is not saved across restart ([config example](https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml)).

Pools and aliases already give clients stable ids (`coding-pool`, `daily-driver`). A profile is an operator switch on top of that, so the daily driver can change without editing every client. The UI already has catalog controls.

Sit it on the matcher, in front of pools. One active profile, config-defined pins, no persistence beyond the config. Skip until someone is actually swapping the daily driver by hand often enough to care.

**Verdict: later.** Real feature, low pressure next to admission and peer failure.

### 12. Count-based LRU and matrix sets — skip

llama.cpp `--models-max` evicts the least recently used idle model when the count is hit, ignoring VRAM ([unload_lru](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-models.cpp)). A 4-slot cap will refuse a second small model or, worse, keep a count slot "free" while one huge model has already filled the GPU. The README does not describe a VRAM ledger. (A 2026 pull request proposed evicting on child OOM; that path was not present in the `unload_lru` read on master, so it is unverified as shipped.)

llama-swap's matrix picks the cheap evict among sets the operator enumerated. It does not look at free VRAM. PLAN already deferred it. Groups (`swap` / `exclusive` / `persistent`) are the same idea with less syntax. `ttl: 0` and `persistent` groups both mean "do not unload," which `idle_ttl_seconds: 0` plus stale-until-pressure already covers, with the extra ability to evict an idle resident when nothing else fits.

**Verdict: skip.** The fleet's scarce resource is VRAM on a named GPU, which the ledger already models. A count cap or a hand-written set list would fight that.

## 5. Do not copy

These are implemented, and they fight the non-goals.

- **Central schedulers.** GPUStack is a server process workers dial into with a token ([README](https://github.com/gpustack/gpustack/blob/main/README.md)). Xinference's cluster mode is one supervisor and N workers; `xinference-local` only colocates them ([using Xinference](https://inference.readthedocs.io/en/latest/getting_started/using_xinference.html)). If that process is down, the workers do not serve the catalog on their own. That is the SPOF SPEC rules out.
- **Consensus and discovery.** exo finds peers by itself and isolates clusters with a Zenoh namespace ([README](https://github.com/exo-explore/exo/blob/main/README.md)). LocalAI P2P / federated mode is optional libp2p with a token and a network id ([run.go](https://github.com/mudler/LocalAI/blob/master/core/cli/run.go)). Both replace the static peer list and the "LAN down, local catalog still serves" rule with a membership protocol. exo on Linux is CPU-only in that README, so it also does not run this NVIDIA fleet.
- **Cross-node tensor split as a router feature.** distributed-llama requires a root and `2^n` nodes ([README](https://github.com/b4rtaz/distributed-llama/blob/main/README.md)). Xinference `n_worker > 1` shards one launch across workers that the supervisor picked. exo shards one instance across discovered devices. A model that only runs if every node is up is not a local catalog entry. Same-node `device: "0,1"` is already the split this fleet needs. llama.cpp `--rpc` exists as a flag; pointing a stanza command at an rpc-server does not require the router to grow a partitioner.
- **Galleries and weight download.** LocalAI autoloads model and backend galleries and stages downloads ([run.go](https://github.com/mudler/LocalAI/blob/master/core/cli/run.go)). GPUStack's quick start downloads from a catalog. llama.cpp router mode can download via `POST /models` and the HF cache. RamaLama pulls a model that is not local. distributed-llama's `launch.py` downloads. This fleet's weights are already on disk, and the box must work offline.
- **Cloud cost routers.** LiteLLM's usage-based and latency-based strategies, Redis-shared TPM, and provider fallbacks are for many billed HTTP deployments ([config settings](https://docs.litellm.ai/docs/proxy/config_settings)). The transferable pieces are cooldown and ordered failover (ideas 1 and 6). The rest assumes a central gateway and prices.
- **Patched-runtime warm KV.** goldseam's repo description is KV snapshot/restore on a patched llama.cpp. That is one backend, not llama-server / TurboQuant / vLLM / sd.cpp, and it is not a stock binary. Left unverified past the description on purpose.
- **In-process engines.** tabbyAPI, Ollama, and llama.cpp router mode are the inference runtime. Copying LoRA hot-swap, draft models, or GGUF loading into the router would abandon backend-agnostic commands. Draft and LoRA stay flags in the stanza `command` (tabby documents them as load options for ExLlama, [wiki](https://github.com/theroyallab/tabbyAPI/wiki/03.-Usage)).
- **Auth, TLS, API keys.** llama-swap, LocalAI, GPUStack, tabbyAPI, and speaches all have them. SPEC lists auth and TLS as non-goals on this LAN. Revisit only if a listener leaves the current network.
- **Force-kill busy work.** tabbyAPI unload terminates pending generations. LocalAI can, default off. llama.cpp's older bug reports showed LRU interrupting a long prompt; current `pick_victim` skips busy models. SPEC still forbids in-flight preemption.

## 6. Source list

Fetched and used:

- https://github.com/mostlygeek/llama-swap/blob/main/README.md
- https://raw.githubusercontent.com/mostlygeek/llama-swap/main/README.md
- https://github.com/mostlygeek/llama-swap/blob/main/docs/config.example.yaml
- https://github.com/mostlygeek/llama-swap/blob/main/docs/configuration.md
- https://api.github.com/repos/mostlygeek/llama-swap
- https://api.github.com/repos/mostlygeek/llama-swap/contents/
- https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
- https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/server-models.cpp
- https://api.github.com/repos/ggml-org/llama.cpp
- https://github.com/mudler/LocalAI/blob/master/core/cli/run.go
- https://raw.githubusercontent.com/mudler/LocalAI/master/core/cli/run.go
- https://api.github.com/repos/mudler/LocalAI
- https://github.com/gpustack/gpustack/blob/main/README.md
- https://raw.githubusercontent.com/gpustack/gpustack/main/README.md
- https://github.com/gpustack/gpustack/blob/main/docs/scheduler.md
- https://github.com/gpustack/gpustack/blob/main/docs/user-guide/model-deployment-management.md
- https://github.com/gpustack/gpustack/blob/main/docs/user-guide/compatibility-check.md
- https://api.github.com/repos/gpustack/gpustack
- https://github.com/xorbitsai/inference/blob/main/README.md
- https://raw.githubusercontent.com/xorbitsai/inference/main/README.md
- https://inference.readthedocs.io/en/latest/getting_started/using_xinference.html
- https://inference.readthedocs.io/en/latest/user_guide/distributed_inference.html
- https://inference.readthedocs.io/en/latest/user_guide/launch.html
- https://inference.readthedocs.io/en/latest/reference/generated/xinference.client.Client.launch_model.html
- https://api.github.com/repos/xorbitsai/inference
- https://github.com/theroyallab/tabbyAPI/blob/main/README.md
- https://raw.githubusercontent.com/theroyallab/tabbyAPI/main/README.md
- https://github.com/theroyallab/tabbyAPI/blob/main/config_sample.yml
- https://github.com/theroyallab/tabbyAPI/wiki/03.-Usage
- https://api.github.com/repos/theroyallab/tabbyAPI
- https://github.com/ollama/ollama/blob/main/server/sched.go
- https://github.com/ollama/ollama/blob/main/envconfig/config.go
- https://raw.githubusercontent.com/ollama/ollama/main/envconfig/config.go
- https://api.github.com/repos/ollama/ollama
- https://docs.litellm.ai/docs/proxy/config_settings
- https://docs.litellm.ai/docs/routing
- https://docs.litellm.ai/docs/proxy/reliability
- https://docs.litellm.ai/docs/proxy/load_balancing
- https://api.github.com/repos/BerriAI/litellm
- https://github.com/exo-explore/exo/blob/main/README.md
- https://raw.githubusercontent.com/exo-explore/exo/main/README.md
- https://api.github.com/repos/exo-explore/exo
- https://github.com/philpax/ananke/blob/main/README.md
- https://raw.githubusercontent.com/philpax/ananke/main/README.md
- https://github.com/philpax/ananke/blob/main/docs/memory-model.md
- https://raw.githubusercontent.com/philpax/ananke/main/docs/memory-model.md
- https://api.github.com/repos/philpax/ananke
- https://github.com/doublewordai/llmux/blob/main/README.md
- https://api.github.com/repos/doublewordai/llmux
- https://github.com/containers/ramalama/blob/main/docs/ramalama-serve.1.md
- https://api.github.com/repos/containers/ramalama
- https://github.com/speaches-ai/speaches/blob/master/README.md
- https://github.com/speaches-ai/speaches/blob/master/src/speaches/config.py
- https://raw.githubusercontent.com/speaches-ai/speaches/master/src/speaches/config.py
- https://api.github.com/repos/speaches-ai/speaches
- https://github.com/b4rtaz/distributed-llama/blob/main/README.md
- https://raw.githubusercontent.com/b4rtaz/distributed-llama/main/README.md
- https://api.github.com/repos/b4rtaz/distributed-llama
- https://github.com/perk11/large-model-proxy/blob/main/README.md
- https://raw.githubusercontent.com/perk11/large-model-proxy/main/README.md
- https://api.github.com/repos/perk11/large-model-proxy
- https://docs.vllm.ai/en/latest/features/sleep_mode/
- https://api.github.com/repos/dollthing/goldseam
- https://api.github.com/repos/lemonade-sdk/lemonade
- https://api.github.com/repos/avafloww/goldseam (redirects to dollthing/goldseam)

Local files read, not fetched: `README.md`, `SPEC.md`, `PLAN.md`, `internal/config/config.go`.

Opened and not used as current behavior, because they are old commits or design notes rather than `main`/`master` docs: llama-swap `proxy/config/model_config.go` at `625b2967` (path 404s on main), llama.cpp `server-models.cpp` at `0b1bad14` and `d59d455f`, tabbyAPI `backends/exllamav2/model.py` at `ed7cd3cb`, llama-swap `internal/router/design.md` (a design note, not the shipped config contract).
