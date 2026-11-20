# Local realtime and speech-to-speech backends

Date of reading: 2026-11-20. Part of #64, issue #80.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link. A fact that could not be confirmed from the
project's own pages is marked `(verify)`, and anything only the GPU host can
settle is in the checklist (§8).

## 0. Scope and method

The question: which backend should this router start, health-check, place,
evict and proxy for `/v1/realtime`, so that a voice client (the Playground's
Realtime tab, an OpenAI SDK realtime client) talks to a local model the same way
it would talk to the hosted API. The cards are the GPU host's one 24 GB sm_86
card (GPU 0) and two 32 GB sm_70 cards (GPU 1 and 2).

**How the facts were gathered.** Page fetches were not available in this run
(web search only). Every external fact comes from
search-engine extracts of the cited pages, read on 2026-11-20, not from opening
the files. Field names, flags and defaults that matter for the stanza are marked
`(verify)` and repeated in the checklist.

**Not redone here.** The cascade's parts are covered by the sibling notes:
speech-to-text in [`stt-backends.md`](stt-backends.md) (#72) and text-to-speech
in [`tts-backends.md`](tts-backends.md) (#75). The sibling content project's E1
and E2 notes ([voice path diagnosis][e1], [TTS model selection][e2]) cover
speech output only; realtime is not one of its needs.

[e1]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e1-voice-path-diagnosis.md
[e2]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md

**Router facts this note relies on.** These were read from this repo, not
assumed. A Go toolchain was not available in this run, so the occupancy and
pool findings below come from reading the code. They are not test results
(checklist item 9 confirms them).

- `/v1/{path...}` is proxied, so a websocket upgrade on `/v1/realtime` reaches a
  backend through `httputil.ReverseProxy` (`internal/router/server.go`,
  `proxyTo` in `internal/router/router.go`). The router never reads a websocket
  frame. It only sees the HTTP upgrade request.
- The matcher reads a GET's model from the query `model` only
  (`modelFromRequest`, `internal/router/matcher.go`). With no `?model=`, an
  upgrade on `/v1/realtime` now falls back to the `api_type: realtime` stanza
  marked `path_default`, or the only one (`pathAPITypes`, added in #66). So
  **a client that sends no model in the URL already works** when the node has
  one realtime default. The shortcut comment in `server.go` predates #66 and
  overstates the gap (§5.3).
- **An open session is already counted as in-flight.** This corrects the issue
  text. `serveLocalTarget` and `proxyLocal` hold an occupancy slot until
  `proxyLocalHeld` returns. For an upgrade, `ReverseProxy.ServeHTTP` returns
  only when one side closes the connection. The idle sweep (`claimForSweep`)
  and eviction (`evictableLocked`, `protected`) skip anything with occupancy
  above zero. `markUsed` runs at the start and again (deferred) at the end, so
  the idle clock starts when the session closes. The `model_router_inflight_requests`
  gauge counts the session too. The issue's "the idle sweep can unload a model
  with an open session" does not match the code (checklist item 9).
- **A pool id cannot upgrade.** `servePool` wraps the writer in `attemptWriter`,
  which has no `Hijack` and no `Unwrap`. The reverse proxy then reports
  "can't switch protocols", `attemptWriter` swallows that as a 5xx, the pool
  walks on, and the client gets a `503`. A direct local or `peer/model` id
  works: `activityWriter` and `loadStream` both `Unwrap`, and the peer hop
  rewrites the query `model` (`realtime_test.go`).
- `slots` is not a hard cap for a local model. When every instance is busy,
  `acquireInstance(..., queue=true)` first tries a new instance
  (`max_instances`) or a peer slot-spill. Failing those, it passes the session
  to the least-loaded instance anyway, and the backend has to queue or refuse
  it.
- The health poll needs an HTTP `200` on `health_check` (default `/health`),
  fetched with a plain GET (`Managed.health`, `internal/router/supervisor.go`).
  A websocket-only route does not count.
- The HTTP server has no `WriteTimeout` and no `ReadTimeout`
  (`cmd/model-router/main.go`), so the router puts no limit on how long a
  session lasts. `IdleTimeout` does not apply to a hijacked connection.
- Loopback clients need no router key (`README.md`, security section), so a
  backend on the GPU host can call back into `http://127.0.0.1:18080/v1`
  without one.
- `command` is split without a shell (`splitCommand`): no quoting, no JSON
  arguments inline.

## 1. Recommendation

**Primary: `speech-to-speech` (the Hugging Face voice-agent pipeline) in its
`serve` realtime mode, as one stanza that runs VAD, speech-to-text
(faster-whisper) and text-to-speech (Kokoro) in-process and calls the router's
own `/v1/chat/completions` for the LLM.** It runs on the 24 GB sm_86 card
(GPU 0), and also on the 32 GB sm_70 cards given a cu126 PyTorch build
`(verify)`. It is a single long-running process on one port. It speaks a
"tested core subset" of the **OpenAI Realtime GA** websocket protocol at
`/v1/realtime`, with server VAD, barge-in (`speech_started` cancels the
response) and function calls (`response.function_call_arguments.done`). Its
licence is Apache-2.0. The answer comes from a chat model the router already
manages, so the stanza adds only the two small speech models (~4 GB booked) and
never a third copy of an LLM.

**Fallback: `llama-server` (llama.cpp v0.8.0 or later) serving the
LFM3-Audio-2B GGUF on `/v1/realtime`, on any card: the 24 GB card, either 32 GB
card (`device: "1|2"`), or CPU.** It is one model doing speech in and speech
out, not a cascade. It is the same binary family the router already runs, with a
`/health` that answers 503 while loading and 200 when ready, and its CUDA build
already runs on the V100s on this host. Its realtime endpoint follows the
OpenAI event shapes, including tool calls (PR #31287). It does **not** do server
VAD: it ignores the client's `turn_detection`, so clients end each turn with
`input_audio_buffer.commit` then `response.create` `(verify)`. The endpoint is
about a month old and marked experimental.

**The rule for choosing:**

1. Clients connect to `voice-cascade` (the primary) by default. It is the
   realtime `path_default`, so `/v1/realtime` with no `?model=` lands there.
2. A client names `lfm3-audio-realtime` (the fallback) only when (a) the
   primary's LLM is not loaded and the client cannot wait for its cold start,
   or (b) it is a push-to-talk client that wants one small model and the lowest
   latency, and accepts a 2B model's answers.
3. The router never switches between them on its own: no pool (a pool id cannot
   upgrade, §5.3) and no shared alias. The two differ in turn-taking (server VAD
   versus client commit), so a silent swap would leave a hands-free client
   waiting for a reply that never comes.

If checklist item 4 shows `speech-to-speech serve` has **no** HTTP route that
answers `200`, the primary cannot pass the router's health poll. The fallback
then serves alone until the router gets a TCP or "any response" health mode,
which is a router change (§5.2).

### The five questions, answered

1. **OpenAI-Realtime-compatible, so the Playground and SDK clients work
   unchanged:** `speech-to-speech` (GA, core subset), LocalAI (GA event names,
   `response.cancel`, `conversation.item.truncate`, tools), SGLang-Omni (typed
   events projected to GA), and `llama-server` (GA shapes, no server VAD).
   Kyutai Unmute is "based on" the protocol but says it is not fully
   compatible. vLLM-Omni's turn-based profile still uses older event names
   (`response.audio.delta`). vLLM's `/v1/realtime` is transcription only.
   speaches implements the protocol but has no cancel or truncate. Moshi,
   Vocalis, RealtimeVoiceChat, FastRTC and Wyoming speak their own protocols
   (§2). **The Playground itself is not GA-clean**: it sends `response.create`
   with the beta `modalities: ['text']`, where GA uses `output_modalities`. A
   strict GA backend may ignore or reject that field `(verify)`. Fixing it is a
   Playground change, not part of this note.
2. **Cascade shape: one stanza that runs speech-to-text and text-to-speech
   itself and calls the router for the LLM.** Neither "one process with all
   three models" nor "the router assembles the transcription, chat and speech
   stanzas". Reasons and VRAM are in §4.4.
3. **Model id:** the query string, `?model=<router id>`, or no model at all for
   the realtime default. The router cannot read `session.update` without
   terminating the websocket itself. Do not build that. A pool id cannot
   upgrade today (§5.3).
4. **Session lifetime:** the router already holds an occupancy slot for the
   whole session, so the sweep and eviction leave it alone. Stickiness comes
   from the single TCP connection. "Idle" means no open session for
   `idle_ttl_seconds` after the last one closed. The real gap is the opposite
   problem: a forgotten open session pins the card forever (§5.4).
5. **WebRTC: websocket only.** WebRTC media runs over UDP between the browser
   and the backend's ICE candidates, which an HTTP reverse proxy cannot carry.
   Every self-hosted client found uses websocket (§5.5).

## 2. Alternatives

`3090` and `V100` say whether the serving runtime runs on sm_86 or sm_70 at
all, and the expected VRAM. Licence is code / weights. "GA" means OpenAI
Realtime GA event names over websocket `/v1/realtime`.

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 (sm_86) | V100 (sm_70) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| speech-to-speech (cascade: Silero VAD → faster-whisper/Parakeet → any OpenAI LLM → Kokoro/Qwen3-TTS) | https://github.com/huggingface/speech-to-speech | Apache-2.0 / per model | README updated ~2026-10-24; package 1.1.0 `(verify)` | GA websocket `/v1/realtime` (core subset) plus WebRTC | async session | PCM16 deltas both ways | yes; ~4 GB with faster-whisper + Kokoro `(verify)` | yes with cu126 torch and CT2 `(verify)` | **primary** |
| llama.cpp `llama-server` + LFM3-Audio-2B | https://github.com/ggml-org/llama.cpp | MIT / LFM Open License v1.0 | `/v1/realtime` PR #30636 (2026-10-20), conversation #31269, tools #31287; v0.8.0 | GA shapes; conversation and `intent=transcription`; experimental | async session | yes | yes; ~3-4 GB `(verify)` | yes (ggml `70-virtual`; host already runs it on the V100s) | **fallback** |
| LocalAI realtime pipeline | https://github.com/mudler/LocalAI | MIT / per model | v4.12.1 image; 5.0.0 docs 2026-11-18 | GA websocket `/v1/realtime?model=`, WebRTC `POST /v1/realtime/calls` | async session | yes | yes | yes, CUDA 12 image only (issue #9948) | strongest feature set (VAD, tools, cancel, truncate), but a whole multi-model server with its own LLM inside; open barge-in bugs #12445, #12493 |
| SGLang-Omni (Qwen3-Omni turn-based; MiniCPM-o 4.5, PersonaPlex, NemotronLabs VoiceChat full-duplex) | https://github.com/sgl-project/sglang-omni | Apache-2.0 `(verify)` / per model | v0.2.0, 2026-10 | GA websocket `/v1/realtime` with `--enable-realtime`; `/v1/realtime/capabilities` | async session | yes, full-duplex for some models | Qwen3-Omni 30B does not fit; MiniCPM-o 4.5 ~19 GB fp16 or 11 GB INT4 `(verify)` | no confirmed sm_70 support `(verify)` | best full-duplex path, but no model fits beside the trio on 24 GB; revisit if a card is freed |
| vLLM-Omni realtime (Qwen2.5/3-Omni; duplex MiniCPM-o, Qwen3-Omni, Nemotron VoiceChat) | https://github.com/vllm-project/vllm-omni | Apache-2.0 / per model | active; RFCs #6592, #7223 for a GA profile | `/v1/realtime` "qwen3-legacy" profile (`response.audio.delta`); `?duplex=1` extensions; tools (PR #3344) | async session | yes | Qwen2.5-Omni-3B probably fits `(verify)`; Qwen3-Omni 30B needs ~79 GB | **no** (cc 7.5 or higher) | skip until the GA profile lands; SDK clients do not work unchanged |
| Kyutai Unmute (cascade: Kyutai STT → any OpenAI LLM → Kyutai TTS) | https://github.com/kyutai-labs/unmute | `(verify)` / STT and TTS weights CC-BY-4.0 `(verify)` | announced 2025-05-28 | websocket `/v1/realtime`, subprotocol `realtime`, "based on" OpenAI Realtime, not fully compatible | async session | Opus 24 kHz `(verify)` | yes; ≥16 GB for all three (STT 2.5 GB, TTS 5.3 GB, LLM 6.1 GB) | not confirmed `(verify)` | skip: three processes plus a backend (does not fit one stanza), no tools, semantic VAD is its strength |
| Kyutai Moshi (full-duplex 7B) | https://github.com/kyutai-labs/moshi | MIT and Apache-2.0 / CC-BY-4.0 | `moshi` crate 0.6.4 | own websocket (Opus in Ogg `(verify)`) | async session | full-duplex, Mimi 24 kHz, 80 ms frames | yes; ~24 GB in PyTorch, less with candle q8 | no: bf16 by default, no fp16 path found `(verify)` | skip: own protocol, no tools, no OpenAI client works |
| speaches realtime (faster-whisper + Kokoro, LLM via `CHAT_COMPLETION_BASE_URL`) | https://github.com/speaches-ai/speaches | MIT / per model | v0.9.0-rc.3, 2025-12-27 | websocket and WebRTC `/v1/realtime` | async session | yes | yes | yes (CT2, ONNX) | skip: URL `model` names the LLM, which collides with the router's use of the same field; no cancel or truncate; stale |
| vLLM `/v1/realtime` (Voxtral Mini 4B Realtime) | https://github.com/vllm-project/vllm | Apache-2.0 / Apache-2.0 | 2026 | own transcription events (`transcription.delta`), PCM16 **16 kHz**, model **required in `session.update`** | async session | text out only | yes | no (official vLLM needs cc 8.0+, per the STT note) | not speech-to-speech; a possible STT stage only |
| LiveKit Agents | https://github.com/livekit/agents | Apache-2.0 `(verify)` | active | worker that joins WebRTC rooms on a LiveKit server | — | WebRTC | n/a | n/a | not a backend: dials out to a media server. Can use the router as its STT, LLM and TTS |
| Pipecat | https://github.com/pipecat-ai/pipecat | BSD-2-Clause `(verify)` | active | framework; SmallWebRTC, websocket and telephony transports; no OpenAI Realtime server | — | yes | n/a | n/a | not a backend; a client of `/v1/realtime` |
| Vocalis | https://github.com/Lex-au/Vocalis | Apache-2.0 | — | own websocket (React app) | — | yes | — | — | an app, a client of the router's LLM and TTS |
| RealtimeVoiceChat | https://github.com/KoljaB/RealtimeVoiceChat | Apache-2.0 (from PR #57, 2026-11-10; MIT before) | 2026-11-10 | own websocket | — | yes | — | — | demo app, own protocol |
| Wyoming (Home Assistant voice) | https://github.com/OHF-Voice/wyoming | MIT `(verify)` | — | JSONL events over raw TCP | turn-based | PCM chunks | — | — | not HTTP; the router cannot carry it |
| FastRTC | https://github.com/gradio-app/fastrtc | Apache-2.0 or MIT `(verify)` | — | library; own websocket JSON or WebRTC | — | yes | — | — | build-your-own handler, not a server |
| Step-Audio 2 mini, Kimi-Audio, GLM-4-Voice | https://github.com/stepfun-ai/Step-Audio2, https://github.com/MoonshotAI/Kimi-Audio, https://github.com/THUDM/GLM-4-Voice | Apache-2.0; MIT; Apache-2.0 code `(verify)` | 2024-2025 | inference scripts and demos, no realtime server | — | — | ~24 GB each `(verify)` | bf16 | skip: no server |
| LLaMA-Omni2 | https://github.com/ictnlp/LLaMA-Omni2 | Apache-2.0 / **research only** | 2025 | demo server | — | — | yes | `(verify)` | skip: non-commercial weights |
| Ultravox | https://github.com/fixie-ai/ultravox | MIT / MIT plus base LLM terms | — | vLLM OpenAI chat; audio in, **text out** | sync | text | yes | as vLLM | not speech-to-speech; STT+LLM half only |

More than six candidates exist. Only four serve the GA protocol as a server
process. Of those four, only the first two fit one router stanza and run on the
sm_70 cards.

## 3. API surface of the recommended backend (`speech-to-speech serve`)

Sources: the repo README and the engine README at
`src/speech_to_speech/api/openai_realtime/README.md`. Endpoint details come
from search extracts, so the table is `(verify)` against the installed version
(checklist item 4).

### Endpoints the router must proxy

| Method and path | Purpose | Shape |
| --- | --- | --- |
| `GET /v1/realtime` (websocket upgrade) | the realtime session | OpenAI Realtime GA JSON events in text frames |
| `POST /v1/realtime/calls` or similar (WebRTC SDP) | WebRTC session | `application/sdp`; **not proxied usefully**, see §5.5 |
| health | readiness | **none documented**. A FastAPI app normally serves `GET /docs` with a `200` `(verify)`. The "Ready" log line and the websocket accepting connections are the documented signals |

There is no model list, no unload endpoint and no metrics. The router unloads
by killing the process group.

### Session protocol

- **Connect:** `ws://127.0.0.1:{port}/v1/realtime`. The OpenAI SDK passes
  `?model=`. The upstream guide connects with `model="local"`, which suggests
  the URL model is ignored `(verify)`, so the router id passes through
  harmlessly.
- **Server first:** `session.created`. The client then sends `session.update`
  with `type: "realtime"`, `instructions`, `tools`, `audio.input.format`,
  `audio.input.turn_detection` and `audio.output.voice` (GA layout).
- **Audio in:** `input_audio_buffer.append` with base64 PCM16 mono. The GA
  default is 24 kHz (`{"type":"audio/pcm","rate":24000}`). What rate the STT
  receives after resampling is `(verify)`.
- **Turn detection:** server VAD (Silero). It emits
  `input_audio_buffer.speech_started` and `speech_stopped`. Threshold and
  `interrupt_response` are honoured (issue #712 `(verify)` that the fix is
  released).
- **Barge-in:** `speech_started` cancels the current response.
  `output_audio_buffer.clear` works on WebRTC only.
- **Audio out:** `response.output_audio.delta` (base64 PCM16) and the
  transcript deltas `(verify)` exact GA names against the engine README.
- **Function calling:** `response.function_call_arguments.done`. While a tool
  call is pending, the conversation blocks (issue #760, fix in PR #772,
  merged `(verify)`).
- **Errors:** an `error` server event on the socket. Before the upgrade, a setup
  failure (for example an unreachable STT endpoint) stops the server accepting
  sessions at all.
- **Sizes:** no documented frame or session limit `(verify)`. OpenAI's own
  hosted sessions stop at 60 minutes. This backend documents no cap.
- **Sync vs async:** one long-lived async session per connection. There is no
  HTTP request/response API for voice.

### What the backend calls

`--llm_backend chat-completions` with `--responses_api_base_url` points at any
OpenAI-compatible base URL. Here that is the router,
`http://127.0.0.1:18080/v1`, with `--model_name` set to a router chat id or
alias. Each assistant turn is one streamed `POST /v1/chat/completions` through
the router. That request takes and releases a slot on the chat stanza like any
other client, and the chat model's tools and template apply. The `responses-api`
backend (the default) needs `/v1/responses` on the chat backend. Whether the
host's chat backends serve it is `(verify)`, so the stanza uses
`chat-completions`.

### The fallback's surface (`llama-server`)

- `GET /v1/realtime` upgrade: conversation by default, `?intent=transcription`
  for a transcription session. An API key can go in the
  `Sec-WebSocket-Protocol` value `openai-insecure-api-key.<key>`. Not needed
  behind the router on loopback.
- It ignores `session.model`. In single-model mode, the model is whatever was
  loaded, so the router id passes through harmlessly (third-party report
  `(verify)`).
- It ignores `turn_detection` and always echoes `null`. A client ends each turn
  with `input_audio_buffer.commit` then `response.create` (third-party reports
  `(verify)`). `response.done` is sent after the committed audio drains.
- Audio: PCM16 mono. Input is resampled to the model's 16 kHz rate when the
  session sets a different rate. An earlier build read 24 kHz as 16 kHz (issue
  #31696, reported fixed `(verify)`). Output is 24 kHz.
- Tool calls use the chat template's parser and grammar (PR #31287).
- `/health` (503 while loading, 200 when ready), `/v1/models`, `/props`,
  `/metrics` with `--metrics`. There is no unload endpoint.

## 4. Hardware fit

### 4.1 Current occupancy (from the GPU host's node config)

- **24 GB card (GPU 0):** the intent model (1500 MB), the embedding model
  (3600 MB) and the reranker (1500 MB) are resident (`idle_ttl_seconds: 0`),
  6600 MB booked. Image stanzas (12000 to 22000 MB, `device: "0|1|2"`,
  `idle_ttl_seconds: 300`) prefer this card.
- **32 GB cards (GPU 1 and 2):** the large chat model is split across both
  (`device: "1,2"`, `vram_mb: 64000`, resident, unpinned, about a 7-minute cold
  start). That leaves about 1.5 GB of the two cards' ~65.5 GB, so **any** new
  load on `"1|2"` evicts it.
- The issue asks about sitting "beside the large chat model and the embedding
  model". Those two are on different cards: the embedding model is on GPU 0,
  the large chat model on GPU 1 and 2.

### 4.2 Primary: `speech-to-speech` with faster-whisper + Kokoro

| Item | Value |
| --- | --- |
| Models in the process | Silero VAD (small, CPU or GPU), faster-whisper (CTranslate2), Kokoro-82M (PyTorch) |
| VRAM | faster-whisper large-v3-turbo fp16 ~1.5-2 GB, Kokoro ~0.5-1 GB, plus two CUDA contexts (PyTorch and CT2) → **book `vram_mb: 4000`** until measured `(verify)` |
| Peak | rises with concurrent sessions; one session assumed (`slots: 1`) |
| Precision | CT2 fp16 is fine on sm_70 and sm_86; Kokoro runs fp32 by default |
| sm_70 | needs a PyTorch build that still ships sm_70 (cu126), in its own venv; the existing sidecar venv (torch 2.12.1 cu130) will not run on the V100s. CT2 wheels include `70-real` (STT note) |
| Defaults to avoid | the upstream default STT (Parakeet via NeMo) and TTS (Qwen3-TTS) are heavier and bf16-leaning; pass `--stt faster-whisper --tts kokoro` |
| CPU-only | `--device cpu` works (a third-party CPU image exists). Kokoro is ~2.4× realtime on CPU (TTS note) and faster-whisper int8 on CPU is usable for short turns. Turn latency on this host's CPUs is unmeasured (checklist item 7) |
| Cold start | model load before the socket opens; first run downloads models. Estimated 30-90 s from local files `(verify)` |
| The LLM | not in this process. It is a router chat stanza, already resident or loaded on demand |

**Co-residency on GPU 0:** 6600 (trio) + 4000 (cascade) = 10600 MB booked,
leaving about 13.9 GB. Image stanzas of 12000 MB fit beside it. The larger ones
(15000 to 22000 MB) need the cascade gone, which eviction does only while no
session is open (§5.4).

**On the 32 GB cards it defeats itself** when its LLM is the large chat model:
loading the cascade on GPU 1 or 2 evicts the large chat model, which is the very
LLM the cascade calls. The V100 stanza (§6.2) only makes sense when the
cascade's LLM is the intent model on GPU 0, or the large chat model has been
moved.

### 4.3 Fallback: `llama-server` + LFM3-Audio-2B GGUF

| Item | Value |
| --- | --- |
| Model | 2B, 32K context, audio in (16 kHz) and text or 24 kHz speech out, 18 languages |
| Files | Q4_K_M backbone plus the audio projector (and possibly a detokeniser file) from `LiquidAI/LFM3-Audio-2B-GGUF`; exact file set `(verify)` (checklist item 5) |
| VRAM | ~3-4 GB at Q4_K_M `(verify)`; book `vram_mb: 4000` until measured |
| sm_70 | ggml CUDA builds `70-virtual` for CUDA < 13; the host's `llama-server` already serves chat on the V100s. A bf16 projector on Volta is `(verify)`; convert to f16 if it fails |
| CPU | `--n-gpu-layers 0`, `device: cpu`; realtime speed unknown, has to be measured (checklist item 7) |
| Cold start | seconds, like the other `llama-server` stanzas `(verify)` |
| Version | needs v0.8.0 or later (realtime conversation, PR #31269; tools, PR #31287). The host's build version is checklist item 2 |

### 4.4 Cascade shape and the three-models question

Three ways to build a cascade:

| Shape | VRAM | Router work | Verdict |
| --- | --- | --- | --- |
| **A. One process with STT, LLM and TTS** (LocalAI pipeline, Unmute) | all three in one stanza: Unmute's own figures are 2.5 + 6.1 + 5.3 ≈ 14 GB, and the LLM is a second copy beside the router's chat models | none | wastes a card on a private LLM; the router cannot see or evict the parts |
| **B. The router assembles it** from the transcription, chat and speech stanzas | the same three models, each its own stanza | the router would have to terminate the websocket and run the whole Realtime state machine (VAD, buffers, conversation items, response events, cancel, truncate) in Go | a new subsystem, not a proxy feature; no |
| **C. One stanza with STT and TTS, LLM through the router** (recommended) | ~4 GB for the speech pair; the LLM is a chat stanza that is already there | none | adds two small models, shares the LLM, keeps the router a proxy |

With C, "three models resident at once" is true, but only two are new, and they
are small enough to sit beside the trio on GPU 0. The LLM's VRAM is already
booked by its own stanza.

**What C costs.** The turn's LLM call goes back through the router. It is
admitted, queued and evicted like any chat request:

- If the chat stanza's slots are busy, the voice reply waits.
- If an image load evicts the large chat model between turns (it is evictable
  as a last resort), the next turn waits ~7 minutes.

That is why rule 2a in §1 exists, and why open question 1 asks which chat model
the cascade should call.

### 4.5 Other candidates, by card

- **3090 only:** vLLM-Omni (cc 7.5+), SGLang-Omni (no sm_70 evidence), Moshi
  and PersonaPlex (bf16, ~24 GB, would need the whole card), Unmute's Kyutai
  STT/TTS (sm_70 unconfirmed).
- **Any card:** LocalAI (CUDA 12 image for the V100s), speaches.
- **Does not fit beside the trio:** Qwen3-Omni 30B (~66 GB of bf16 weights),
  MiniCPM-o 4.5 at fp16 (~19 GB) `(verify)`.

## 5. How the router would run it

### 5.1 Spawn (primary)

```
/opt/ai/venv-s2s/bin/speech-to-speech serve
  --host 127.0.0.1 --port {port}
  --device cuda
  --stt faster-whisper --tts kokoro
  --llm_backend chat-completions
  --responses_api_base_url http://127.0.0.1:18080/v1
  --responses_api_api_key unused
  --model_name voice-llm
```

- Foreground: `serve` runs uvicorn in the foreground. The process-group kill
  covers it.
- `--host`, `--port`, `--device`, `--stt`, `--tts`, `--llm_backend`,
  `--responses_api_base_url`, `--responses_api_api_key` and `--model_name`
  appear in the upstream README and a third-party image's run command. The
  exact spelling on the installed version is `(verify)` (checklist item 4).
- `--responses_api_api_key unused`: loopback needs no router key, but the
  backend may refuse an empty key `(verify)`.
- `voice-llm` is a **router alias** added to whichever chat stanza Kevyn picks
  (open question 1). The alias resolves at the router. The chat backend then
  receives `model: "voice-llm"`, which `llama-server` ignores. Whether the
  large chat model's server accepts an unknown model name is `(verify)`.
- `HF_HUB_OFFLINE=1` and pre-downloaded models: a spawn never downloads.

### 5.2 Stanza fields

| Field | Primary | Fallback | Why |
| --- | --- | --- | --- |
| `health_check` | `/docs` `(verify)` | `/health` | the router needs a plain-GET `200`. `/docs` is FastAPI's default page and exists only after the app is up. If the models load before uvicorn binds (the docs say "Loading models…" then "Ready"), a `200` means ready. **If there is no `200` route at all, that is a router change**: a health mode that accepts a TCP connect or any HTTP status |
| `spin_up_seconds` | `180` | `60` | estimated cold start, doubled; neither reports `progress` |
| `vram_mb` | `4000` | `4000` | §4 budgets; replace with measured peaks |
| `idle_ttl_seconds` | `900` | `600` | counted from the end of the last session (§5.4); a longer window spares the next caller a cold start |
| `slots` | `1` | `1` | one session per slot for the whole session; concurrent sessions per process are unconfirmed for both `(verify)`. `llama-server --parallel 1` |
| `match` | `{path_default: true}` | `{body_field: model}` | the primary takes model-less upgrades (realtime type default). A `?model=` naming either id wins |
| `api_type` | `realtime` | `realtime` | |
| `capabilities` | `[audio_in, audio_out, tools]` | `[audio_in, audio_out, tools]` | |

### 5.3 Model id, first frames and pools

- **OpenAI clients put the model in the URL** (`/v1/realtime?model=...`).
  OpenAI's GA protocol binds the model at connect. With WebRTC it is bound
  inside the ephemeral client secret. The router reads that query today.
- **vLLM is the exception.** Its transcription socket requires `model` inside
  `session.update` and rejects a session without it. Routing it by
  `session.update` would mean reading frames.
- **The router cannot pick a backend from the first frames without terminating
  the websocket.** The protocol is server-first: the backend sends
  `session.created` before the client says anything. To read a client's first
  `session.update`, the router would have to:
  1. answer the upgrade itself;
  2. fake or delay `session.created`;
  3. parse websocket framing (the standard library has none);
  4. dial the backend;
  5. replay the buffered frames and swallow the backend's own `session.created`.

  That is a websocket-terminating proxy, not a pass-through. **Recommendation:
  do not build it.** A client that cannot put the model in the URL connects
  with no model and gets the realtime `path_default` stanza. That works today
  (§0). Update the `server.go` shortcut comment when the code is next touched;
  this note does not edit code.
- **A pool id cannot upgrade.** It fails with a `503` after walking every
  target (§0). Making it work is a small router change: for an upgrade request,
  `servePool` would commit to the first eligible target without
  `attemptWriter`, because a `101` cannot be retried anyway. It is not needed
  for this recommendation, which uses no pool (rule 3).
- **Peers work.** `peer/model` in the query is rewritten to the peer-local id
  (`TestRealtimeUpgradeIsProxiedToThePeer`).

### 5.4 Session lifetime, stickiness and idle

- **How a backend reports an open session:** neither recommended backend
  exposes a session count over HTTP `(verify)`. `llama-server`'s `/slots` may
  show a busy slot during a session `(verify)`. The router does not need either
  report: it holds the occupancy slot for as long as the proxied connection is
  open (§0).
- **Stickiness** is the TCP connection itself. Every frame of a session goes to
  the instance that accepted the upgrade. Neither backend resumes a session
  after a reconnect `(verify)`, so a reconnect is a new session and may land on
  any instance. No session table is needed.
- **Idle** means no open session, for `idle_ttl_seconds` counted from when the
  last session closed (`markUsed` is deferred to the end of the proxy call). A
  silent but open session is **busy**, not idle.
- **The real gap: an abandoned session pins VRAM.** A browser tab left open
  holds the slot. Neither the sweep nor eviction will touch the model, so a
  waiting image job queues until `admit_timeout_seconds` and then gets `503`.
  OpenAI's hosted sessions stop at 60 minutes. Neither local backend documents
  a cap. Fixing it needs either:
  - a backend-side session timeout (none documented); or
  - **a router change: a per-stanza maximum session duration** (new key, for
    example `max_session_seconds`) that closes the hijacked connection.

  Not proposed in the stanzas below. Open question 3.
- **Over-subscription:** with `slots: 1` and `max_instances` unset, a second
  session is still proxied to the same backend (§0). The backend must queue or
  refuse it `(verify)` (checklist item 6).
- **Shutdown and reload:** `http.Server.Shutdown` does not wait for hijacked
  connections, so a router restart drops open sessions. A config apply that
  unloads the stanza waits on the session's occupancy, which is the "one stuck
  stream holds the reload" shortcut in `apply.go`.

### 5.5 WebRTC: not in the router

Evidence:

- **OpenAI's WebRTC flow is three legs.** A server mints an ephemeral key with
  `POST /v1/realtime/client_secrets` (model bound in the session config). The
  browser posts its SDP offer to `/v1/realtime/calls` with
  `Content-Type: application/sdp`. Media then flows over **UDP/SRTP between the
  browser and the backend's ICE candidates**. The router can pass the two HTTP
  posts (they are under `/v1`), but:
  - it cannot route them by model: the matcher reads JSON, form and multipart
    bodies, not SDP, and the model sits inside a key;
  - it cannot carry the media.

  The media would go around the router, bypassing its keys and accounting, to
  whatever address the backend advertises.
- **Clients that point at a self-hosted endpoint use websocket:**
  - the OpenAI Agents SDK for Python supports websocket only;
  - the JS Agents SDK has a `websocket` transport with a custom `url`;
  - Open WebUI's voice mode opens a server-side websocket to
    `<base URL>/realtime?model=`;
  - Pipecat's `OpenAIRealtimeLLMService` takes a `wss://` `base_url`;
  - the `speech-to-speech` demo uses the stock websocket transport;
  - the Playground uses a websocket.
- **The candidates that offer WebRTC** (`speech-to-speech`, LocalAI, speaches)
  also offer the websocket.

Conclusion: websocket only. A browser that insists on WebRTC can reach a
backend's WebRTC endpoint directly on the LAN, outside the router. That is not
a router feature.

### 5.6 What breaks with pure pass-through

The router rewrites no path, no field and no header for these stanzas. That
works, but plainly:

- **The model in `session.update` is invisible to the router.** It only matters
  when a node has two realtime stanzas and the client names the non-default one
  only in `session.update`; that client gets the default. Both recommended
  backends ignore the in-session model.
- **No pool, no automatic failover** between primary and fallback (§5.3, rule
  3).
- **Turn-taking differs.** A hands-free SDK client (server VAD by default) on
  the fallback waits forever for a reply, because the fallback ignores
  `turn_detection` `(verify)`. The router cannot fix that by rewriting, since it
  never sees frames.
- **The Playground's text turn** sends the beta `modalities` field (§1, Q1).
  Whether each backend accepts text-only input items is `(verify)` (checklist
  item 8).
- **Activity capture** sees only the upgrade request and the `101`. Frames are
  not captured, so the Activity view shows one entry per session, with its
  duration.
- **Abandoned sessions pin the card** (§5.4).

Nothing in this list needs a router change to ship the two stanzas below. The
router changes named in this note are all optional:

- a TCP or any-status health mode, only if `/docs` fails;
- pool upgrade;
- a maximum session duration.

## 6. Proposed stanzas

Paths, file names and ports are placeholders. Ports sit above the STT note's
`5930-5932` and the TTS note's `5933-5935`; check `reserved_ports` before
deploying. Every key is a documented `Stanza` field (`SPEC.md` config block;
`internal/config/config.go` YAML tags). **No new key is needed.** The only
companion change is an alias on an existing chat stanza:

```yaml
    aliases: [voice-llm]   # on the chat stanza the cascade should call (open question 1)
```

### 6.1 Primary, 24 GB card (sm_86)

```yaml
  - model_id: voice-cascade
    name: "Voice cascade (faster-whisper + Kokoro, LLM via router, realtime)"
    command: >
      /opt/ai/venv-s2s/bin/speech-to-speech serve
      --host 127.0.0.1 --port {port}
      --device cuda
      --stt faster-whisper --tts kokoro
      --llm_backend chat-completions
      --responses_api_base_url http://127.0.0.1:18080/v1
      --responses_api_api_key unused
      --model_name voice-llm
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0", "HF_HUB_OFFLINE=1"]
    vram_mb: 4000
    device: 0
    spin_up_seconds: 180
    api_type: realtime
    capabilities: [audio_in, audio_out, tools]
    match: {path_default: true}
    health_check: /docs
    idle_ttl_seconds: 900
    slots: 1
    port: 5936
```

### 6.2 Primary, 32 GB cards (sm_70)

Viable only when `voice-llm` is **not** the large chat model, because this
placement evicts it (§4.2). Requires the cu126 venv.

```yaml
  - model_id: voice-cascade
    name: "Voice cascade (faster-whisper + Kokoro, LLM via router, realtime, V100)"
    command: >
      /opt/ai/venv-s2s/bin/speech-to-speech serve
      --host 127.0.0.1 --port {port}
      --device cuda
      --stt faster-whisper --tts kokoro
      --llm_backend chat-completions
      --responses_api_base_url http://127.0.0.1:18080/v1
      --responses_api_api_key unused
      --model_name voice-llm
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}", "HF_HUB_OFFLINE=1"]
    vram_mb: 4000
    device: "1|2"
    spin_up_seconds: 180
    api_type: realtime
    capabilities: [audio_in, audio_out, tools]
    match: {path_default: true}
    health_check: /docs
    idle_ttl_seconds: 900
    slots: 1
    port: 5936
```

### 6.3 Fallback, 24 GB card (sm_86)

```yaml
  - model_id: lfm3-audio-realtime
    name: "LFM3-Audio 2B (speech-to-speech, llama-server, realtime)"
    command: >
      /opt/ai/bin/llama-server
      --model /opt/ai/models/realtime/LFM3-Audio-2B-Q4_K_M.gguf
      --mmproj /opt/ai/models/realtime/mmproj-LFM3-Audio-2B-f16.gguf
      --host 127.0.0.1 --port {port}
      --n-gpu-layers 999 --parallel 1 --metrics
      --split-mode none --device CUDA0
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
    vram_mb: 4000
    device: 0
    spin_up_seconds: 60
    api_type: realtime
    capabilities: [audio_in, audio_out, tools]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 600
    slots: 1
    port: 5937
```

### 6.4 Fallback, 32 GB cards (sm_70)

Same evict-the-large-chat-model cost as §6.2 (~7 minutes of chat). Use it when
GPU 0 is held by an image job and the operator accepts that cost.

```yaml
  - model_id: lfm3-audio-realtime
    name: "LFM3-Audio 2B (speech-to-speech, llama-server, realtime, V100)"
    command: >
      /opt/ai/bin/llama-server
      --model /opt/ai/models/realtime/LFM3-Audio-2B-Q4_K_M.gguf
      --mmproj /opt/ai/models/realtime/mmproj-LFM3-Audio-2B-f16.gguf
      --host 127.0.0.1 --port {port}
      --n-gpu-layers 999 --parallel 1 --metrics
      --split-mode none --device CUDA0
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 4000
    device: "1|2"
    spin_up_seconds: 60
    api_type: realtime
    capabilities: [audio_in, audio_out, tools]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 600
    slots: 1
    port: 5937
```

The GGUF file names are placeholders. The real set (backbone, projector, and any
audio detokeniser that needs its own flag) is checklist item 5. The CPU variant
is the same stanza with `device: cpu`, `vram_mb: 0`, `--n-gpu-layers 0` and no
`CUDA_VISIBLE_DEVICES`. Propose it only after checklist item 7 gives a usable
speed.

## 7. Licence and risk flags

Internal use only, but these are the terms to know.

- **`speech-to-speech`**: Apache-2.0. **faster-whisper**: MIT. **Kokoro-82M**
  weights: Apache-2.0. **Silero VAD**: MIT `(verify)`. Whisper weights: MIT.
- **llama.cpp**: MIT. **LFM3-Audio-2B weights: LFM Open License v1.0.** It is
  not Apache-2.0. Commercial use is free only for organisations under USD 10M
  annual revenue, counted across the whole corporate group. Above that, a paid
  licence is needed. Fine for a homelab. Flag it if the fallback ever serves a
  business.
- **LocalAI**: MIT. **SGLang-Omni** and **vLLM-Omni**: Apache-2.0 (SGLang-Omni
  `(verify)`). **Moshi**: MIT/Apache-2.0 code, CC-BY-4.0 weights.
  **PersonaPlex**: NVIDIA Open Model Licence (gated). **NemotronLabs
  VoiceChat**: OpenMDW 1.1 `(verify)`. **MiniCPM-o 4.5**: licence not found
  `(verify)`.
- **Non-commercial or restricted:** LLaMA-Omni/LLaMA-Omni2 weights are research
  only. GLM-4-Voice weights may carry the GLM-4 model licence `(verify)`.
  RealtimeVoiceChat moved from MIT to Apache-2.0 on 2026-11-10; older copies
  stay MIT.
- **An open microphone is a wiretap.** A realtime stanza takes live audio from
  anyone who can reach `/v1/realtime`. Keep it behind `security.api_keys` if the
  LAN is not trusted. Browser clients send the key on the handshake, as the
  Playground does.
- **Tool calls run with the LLM's tools.** Function calling in a voice session
  means spoken input can trigger tools. The client owns tool execution in the
  OpenAI protocol, so the risk sits with the client, not the router. Still, do
  not hand a voice session tools with side effects without confirmation.
- **Abandoned sessions hold a card** (§5.4): an availability risk, not data
  loss.
- **Maturity:** llama-server's realtime endpoint is about a month old and
  marked experimental. `speech-to-speech` calls its realtime support a "tested
  core subset" and has open tool-call issues. LocalAI has open barge-in bugs
  (#12445 drops the user's turn, #12493 ignores `interrupt_response`).

## 8. GPU host verification checklist (read-only) and open questions

Run as the service user. Steps 3 to 8 start a backend by hand on a spare port
and a card with room, then stop it. Nothing else is changed. Use a recording
made for the test, of a consenting speaker, saved as
`/tmp/fictional-question.wav` (16-bit mono).

```bash
# 1. Cards and what holds them now.
nvidia-smi --query-gpu=index,name,compute_cap,memory.total,memory.used,memory.free --format=csv
curl -s http://127.0.0.1:18080/_router/status | head -c 2000

# 2. llama-server build: needs v0.8.0+ for /v1/realtime conversation and tools.
/opt/ai/bin/llama-server --version
/opt/ai/bin/llama-server --help | grep -i -E 'realtime|mmproj|audio'

# 3. Cascade venv (to be created with a cu126 torch): arch list must include sm_70 for the V100 stanza.
/opt/ai/venv-s2s/bin/python -c "import torch, ctranslate2; print(torch.__version__, torch.cuda.get_arch_list(), ctranslate2.__version__)"
/opt/ai/venv-s2s/bin/speech-to-speech serve --help | grep -E -- '--(host|port|device|stt|tts|llm_backend|responses_api_base_url|responses_api_api_key|model_name)'

# 4. Start the primary by hand on GPU 0; time the cold start; find a plain-GET 200 route.
date +%s; CUDA_VISIBLE_DEVICES=0 HF_HUB_OFFLINE=1 /opt/ai/venv-s2s/bin/speech-to-speech serve \
  --host 127.0.0.1 --port 5966 --device cuda --stt faster-whisper --tts kokoro \
  --llm_backend chat-completions --responses_api_base_url http://127.0.0.1:18080/v1 \
  --responses_api_api_key unused --model_name <an existing chat model id> &
until curl -sf -o /dev/null http://127.0.0.1:5966/docs; do sleep 2; done; date +%s   # spin_up_seconds
for p in /docs /openapi.json /health /v1/models; do printf '%s ' $p; curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5966$p; done
curl -s http://127.0.0.1:5966/openapi.json | python3 -c "import json,sys; print(sorted(json.load(sys.stdin)['paths']))"

# 5. Fallback files: list what the GGUF repo ships (backbone, projector, detokeniser?) and their dtypes.
ls -l /opt/ai/models/realtime/
#    Then start it by hand on a card with room:
CUDA_VISIBLE_DEVICES=0 /opt/ai/bin/llama-server --model /opt/ai/models/realtime/<backbone>.gguf \
  --mmproj /opt/ai/models/realtime/<projector>.gguf --host 127.0.0.1 --port 5967 \
  --n-gpu-layers 999 --parallel 1 --metrics &
until curl -sf http://127.0.0.1:5967/health; do sleep 1; done
curl -s http://127.0.0.1:5967/props | head -c 1500                      # modalities

# 6. Protocol smoke test against each backend (5966 and 5967) with the upstream example clients:
#    speech-to-speech:  speech-to-speech talk --url ws://127.0.0.1:5966/v1/realtime
#    llama.cpp:         python3 examples/realtime_client.py (from the llama.cpp tree) against ws://127.0.0.1:5967/v1/realtime
#    Note for each: session.created contents, whether ?model=anything is accepted, whether
#    server_vad is honoured or echoed null, whether a second concurrent connection is
#    refused, queued or served, and whether a reconnect resumes anything.

# 7. Peak VRAM for vram_mb, and CPU speed for the CPU variants (turn latency, audio seconds / wall seconds).
nvidia-smi --query-gpu=index,memory.used --format=csv -l 1   # run during step 6, note the peak

# 8. Playground text turn: does each backend accept conversation.item.create with input_text and
#    response.create with response.modalities ['text'] (beta field), or only output_modalities?
#    Open the Realtime tab through a test router config pointing at 5966 / 5967 and send one line.

# 9. Router behaviour (from code reading in §0) with a staged stanza: while one session is open,
#    in_flight must be 1 and the model must survive an idle sweep past its TTL; a pool id must 503.
curl -s http://127.0.0.1:18080/_router/status | python3 -c "import json,sys; d=json.load(sys.stdin); print(json.dumps(d, indent=1)[:3000])" | grep -E -A3 'voice-cascade|lfm3-audio-realtime'
curl -s http://127.0.0.1:18080/metrics | grep model_router_inflight_requests
```

### Open questions for Kevyn

1. **Which chat model does the cascade call (`voice-llm`)?**
   - The large chat model gives the best answers and tool calling. But it sits
     on the V100s (so the cascade must stay on GPU 0), and an image eviction
     costs a 7-minute wait mid-conversation.
   - The intent model is resident on GPU 0 and fast, but small.

   Recommended: the large chat model, with the cascade pinned to GPU 0 (§6.1).
2. **Is realtime wanted on the V100s at all?** Every `"1|2"` placement evicts the
   large chat model. If not, drop §6.2 and §6.4 and keep GPU 0 plus CPU.
3. **Should the router cap session length** (new `max_session_seconds` key, a
   router change), or is an abandoned session holding GPU 0 acceptable for a
   homelab?
4. **Is the LFM Open License acceptable** for the fallback? It is free under the
   USD 10M revenue cap.
5. **Should the Playground's Realtime tab move to GA fields** (`output_modalities`,
   `session.type: "realtime"`) and gain audio capture? That is a UI change for a
   separate issue, and it decides whether the Playground "works unchanged"
   against a GA-strict backend.
6. **Full-duplex later?** SGLang-Omni or vLLM-Omni with MiniCPM-o 4.5 or
   PersonaPlex is the upgrade path. It needs most of the 3090, so it would
   compete with the trio and image jobs.

## 9. Sources

All read on 2026-11-20 through search-engine extracts (no direct page fetch in
this run).

OpenAI Realtime protocol (reference shape)

- https://developers.openai.com/api/docs/guides/realtime-conversations (60-minute session cap)
- https://developers.openai.com/api/reference/resources/realtime/client-events
- https://developers.openai.com/api/reference/resources/realtime/server-events
- https://developers.openai.com/api/docs/guides/realtime-transcription
- https://developers.openai.com/api/docs/guides/realtime (WebRTC, client secrets, `/v1/realtime/calls`)
- https://developers.openai.com/blog/realtime-api
- https://github.com/TanStack/ai/issues/586 (beta shape disabled)
- https://openai.github.io/openai-agents-js/guides/voice-agents/transport/
- https://openai.github.io/openai-agents-python/realtime/transport/

speech-to-speech (primary)

- https://github.com/huggingface/speech-to-speech
- https://github.com/huggingface/speech-to-speech/blob/main/src/speech_to_speech/api/openai_realtime/README.md
- https://github.com/huggingface/speech-to-speech/blob/main/demo/README.md
- https://github.com/huggingface/speech-to-speech/blob/main/docs/openai-compatible-stt.md
- https://github.com/huggingface/speech-to-speech/issues/363 (proposed health routes, not shipped)
- https://github.com/huggingface/speech-to-speech/issues/712
- https://github.com/huggingface/speech-to-speech/pull/772
- https://github.com/Aurial/speech-to-speech-docker (third-party; CLI flags, CPU image)
- https://deepwiki.com/huggingface/speech-to-speech/6-usage-guide (secondary)

llama.cpp realtime and LFM3-Audio (fallback)

- https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
- https://github.com/ggml-org/llama.cpp/pull/31269 (conversation and transcription on `/v1/realtime`)
- https://github.com/ggml-org/llama.cpp/issues/31696 (24 kHz read as 16 kHz)
- https://github.com/y0sif/whisrs/pull/243 (third-party: `turn_detection` echoed null, `session.model` ignored)
- https://github.com/stanier/stoa/pull/6 (third-party: server VAD not confirmed)
- https://huggingface.co/LiquidAI/LFM3-Audio-2B-GGUF
- https://docs.liquid.ai/lfm/models/lfm3-audio-2b
- https://jasonvsthenoise.com/repowatch/2026-10-29-llama-cpp-v0-8-0/ (secondary; v0.8.0 notes)

LocalAI

- https://localai.io/docs/features/openai-realtime/
- https://localai.io/docs/advanced/model-configuration/
- https://github.com/mudler/LocalAI/pull/8790 (WebRTC)
- https://github.com/mudler/LocalAI/pull/12958 (LFM3-Audio on llama.cpp)
- https://github.com/mudler/LocalAI/issues/12445
- https://github.com/mudler/LocalAI/issues/12493
- https://github.com/mudler/LocalAI/issues/9948 (CUDA 13 images do not run on V100)
- https://github.com/lbedner/aegis-stack/issues/2057 (third-party comparison with speaches)

SGLang-Omni and vLLM-Omni

- https://github.com/sgl-project/sglang-omni
- https://github.com/sgl-project/sglang-omni/pull/2070
- https://www.sglang.io/blog/omni-interactive-runtime
- https://sgl-project.github.io/sglang-omni/basic_usage/omni_router.html
- https://huggingface.co/openbmb/MiniCPM-o-4_5
- https://github.com/tc-mb/llama.cpp-omni
- https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/full_duplex_api/
- https://github.com/vllm-project/vllm-omni/blob/main/examples/online_serving/qwen3_omni/openai_realtime_client.py
- https://github.com/vllm-project/vllm-omni/issues/6592
- https://github.com/vllm-project/vllm-omni/issues/7223
- https://github.com/vllm-project/vllm-omni/pull/3344
- https://github.com/vllm-project/vllm-omni/pull/8600 (server VAD for turn-based realtime)
- https://docs.vllm.ai/projects/vllm-omni/en/latest/models/supported_models/
- https://github.com/QwenLM/Qwen3-Omni/blob/main/README.md

vLLM transcription realtime

- https://docs.vllm.ai/en/latest/examples/speech_to_text/realtime/
- https://docs.vllm.ai/en/latest/api/vllm/entrypoints/speech_to_text/realtime/api_router/
- https://github.com/vllm-project/vllm-project.github.io/blob/main/_posts/2026-01-31-streaming-realtime.md

Kyutai

- https://github.com/kyutai-labs/moshi
- https://crates.io/crates/moshi
- https://github.com/kyutai-labs/moshi/issues/427 (bf16 by default)
- https://huggingface.co/kyutai/moshiko-candle-q8
- https://github.com/kyutai-labs/unmute
- https://github.com/kyutai-labs/unmute/issues/77
- https://github.com/kyutai-labs/delayed-streams-modeling/
- https://github.com/NVIDIA/personaplex
- https://github.com/NVIDIA-NeMo/Speech/tree/nemotron-labs-voicechat

speaches

- https://github.com/speaches-ai/speaches
- https://speaches.ai/usage/realtime-api/
- https://speaches.ai/usage/voice-chat/
- https://speaches.ai/configuration/
- https://github.com/speaches-ai/speaches/releases

Frameworks and other protocols

- https://github.com/livekit/agents
- https://docs.livekit.io/agents/server/startup-modes/
- https://docs.livekit.io/agents/models/llm/openai-compatible-llms/
- https://github.com/pipecat-ai/pipecat
- https://docs.pipecat.ai/server/services/transport/small-webrtc
- https://docs.pipecat.ai/server/services/s2s/openai
- https://github.com/Lex-au/Vocalis
- https://github.com/KoljaB/RealtimeVoiceChat
- https://github.com/KoljaB/RealtimeVoiceChat/pull/57
- https://github.com/OHF-Voice/wyoming
- https://github.com/roryeckel/wyoming_openai
- https://github.com/gradio-app/fastrtc
- https://docs.openwebui.com/features/chat-conversations/chat-features/voice-mode/

Other speech-to-speech models

- https://github.com/stepfun-ai/Step-Audio2
- https://huggingface.co/stepfun-ai/Step-Audio-2-mini
- https://github.com/MoonshotAI/Kimi-Audio
- https://github.com/THUDM/GLM-4-Voice
- https://github.com/ictnlp/LLaMA-Omni2
- https://github.com/fixie-ai/ultravox

This repo

- [`SPEC.md`](../../SPEC.md) (stanza fields, request flow, eviction)
- [`docs/research/stt-backends.md`](stt-backends.md), [`docs/research/tts-backends.md`](tts-backends.md) (cascade parts, port range)
- `internal/router/server.go`, `internal/router/router.go`,
  `internal/router/matcher.go`, `internal/router/instances.go`,
  `internal/router/supervisor.go`, `internal/router/realtime_test.go`,
  `internal/router/ui/app.js`, `cmd/model-router/main.go`,
  `internal/config/config.go`
- the GPU host's node config under `configs/` (card occupancy)
