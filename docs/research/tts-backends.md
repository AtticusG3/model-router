# Local text-to-speech backends

Date of reading: 2026-11-20. Part of #64, issue #75.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link. A fact that could not be confirmed from the
project's own pages is marked `(verify)`, and anything only the GPU host can
settle is in the checklist (§8).

## 0. Scope and method

The question: which server-style text-to-speech backend should this router
start, health-check, place, evict and proxy the way it already runs
`llama-server` and `sd-server`, on the GPU host's three cards: one 24 GB sm_86
card (GPU 0) and two 32 GB sm_70 cards (GPU 1 and 2).

**How the facts were gathered.** Page fetches were not available in this run
(the sandbox allows web search only), so every external fact comes from
search-engine extracts of the cited pages, read on 2026-11-20, not from opening
the files. Facts that matter for the stanza (flags, field names, defaults) are
marked `(verify)` and repeated in the checklist where only the host can confirm
them.

**The model-selection research is not redone.** The sibling content project's
E2 note ([TTS model selection and hosting][e2], sections 3.2 serving stacks,
4.5 VRAM and latency, 5.4 hosting) already ranks the models, and its E1 note
([voice path diagnosis][e1]) sets the consumer contract. Both live on a Gitea
instance this run could not reach, and the deepening ticket (#47 there) could not
be checked either. So E2's verdicts below are quoted from the issue text, not
first-hand `(verify)`:

- E2's TTS host is a 32 GB Blackwell workstation card. It rules the V100 out,
  and it notes that vLLM-Omni needs compute capability 7.5 or higher.
- E2's shortlist leads with **Qwen3-TTS 1.7B, served by vLLM-Omni or
  faster-qwen3-tts**. It also covers IndexTTS-2.5, VoxCPM2, CosyVoice3, Kokoro,
  Orpheus, Chatterbox, F5-TTS, XTTS-style, Fish, Higgs, Spark and llama.cpp
  `llama-tts`.
- E1's rule for any speech path: **no silent fallback voice, and the language
  is explicit.** A router-served backend must fail loudly rather than speak in a
  default voice or guess the language.

[e1]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e1-voice-path-diagnosis.md
[e2]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md

**Router facts this note relies on** (read from this repo, not assumed):

- `/v1/{path...}` is proxied, so `/v1/audio/speech`, `/v1/audio/voices`,
  `/v1/audio/speech/batch` and websocket upgrades under `/v1` reach a backend
  (`internal/router/server.go`). The proxy flushes immediately
  (`FlushInterval: -1`, `internal/router/router.go`), so chunked PCM and SSE
  pass through unbuffered.
- A model-less `POST /v1/audio/speech` goes to the `api_type: speech` stanza
  marked `path_default`, or the only one (`internal/router/matcher.go`).
  `/v1/audio/voices` is **not** in that path table, so a model-less
  `GET /v1/audio/voices` is a router `404` unless a stanza claims it with
  `match.path_prefix`.
- The body `model` must resolve to a router id. An id containing `/` is read as
  `peer/model`. A Hugging Face id such as `Qwen/Qwen3-TTS-12Hz-1.7B-Base` is
  therefore refused at the router before it is proxied.
- **Filters cannot strip or set `model`** (`internal/config/config.go`,
  `Filters.validate` protects it), so the router never rewrites the model field
  for a local stanza. The backend must accept the router id as its model name,
  or ignore the field.
- `command` is split without a shell (`splitCommand`, `internal/router/proc.go`).
  A double quote toggles grouping and is dropped, and there is no escape, so **a
  JSON argument cannot be passed inline**. Anything JSON-shaped has to go in a
  file.
- A native path outside the route table (llama-server's `/tts`) is only proxied
  when the URL carries `?model=` (`handleRoot`).

## 1. Recommendation

**Primary: vLLM-Omni serving Qwen3-TTS-12Hz-1.7B-Base, on the 24 GB sm_86
card (GPU 0) only.** It is the only candidate with an official, documented
OpenAI speech surface that covers everything the consumer needs:
`POST /v1/audio/speech` with inline reference audio (`ref_audio` as a `data:`
URI plus `ref_text`), a voice registry (`GET`/`POST /v1/audio/voices`), raw PCM
streaming, SSE streaming, a websocket text-in endpoint, explicit OpenAI-shaped
errors, and the standard vLLM `/health` and `/v1/models`. One `vllm serve`
process serves one model, so the router's spawn, health poll, eviction and kill
are the whole lifecycle, and no load logic is duplicated (§5). Apache-2.0 code
and weights. It **does not run on the 32 GB sm_70 cards**: current vLLM-Omni
installation docs require compute capability 7.5 or higher.

**Fallback: `llama-server` (llama.cpp v1.0.0 or later) serving the
Qwen3-TTS-12Hz-1.7B-Base GGUF, on the 32 GB sm_70 cards (`device: "1|2"`), or
on CPU.** It is the same binary family the router already runs for chat,
embedding and rerank, it has `/health` (503 while loading, 200 when ready), and
its CUDA build already runs on the V100s on this host. v1.0.0 (2026-11-13) added
an OpenAI-compatible `/v1/audio/speech` that converts to its native `/tts`
(ggml-org/llama.cpp PR #31763) `(verify)`, with `speaker_ref_b64` for inline
cloning and `stream_format: "audio"` for streaming `(verify)`: these facts come
from release notes on a mirror, not yet checked against the upstream PR.
It serves the **same model weights**, so the same reference clip gives the same
cast voice. MIT code. It is one week old, so it is the fallback, not the
primary.

**The rule for choosing:**

1. Every speech request names `qwen3-tts-1.7b` (the primary) by default.
2. A client uses `qwen3-tts-1.7b-gguf` (the fallback) only when the primary
   answers `503` with `Retry-After` because the 24 GB card is held by an
   image job, or for batch narration on a 32 GB card the operator has freed.
   The client re-sends the same reference clip, `ref_text` and language, in the
   fallback's field names (§5).
3. The router never switches between them on its own: no pool and no shared
   alias. The two take different field names for the reference audio, so an
   automatic swap would drop the clip and speak in a default voice, which is
   the failure E1 forbids.
4. Kokoro (CPU, §6.3) is **not** a fallback for a cast voice. It is a separate,
   explicitly named stock-voice model for previews and system prompts, and a
   client must ask for it by name.

If checklist item 6 (§8) shows the GGUF output is not close enough to the
vLLM-Omni output for the same clip, rule 2 is withdrawn and the fallback becomes
"wait for the 24 GB card".

## 2. Alternatives

`3090` and `V100` say whether the serving runtime runs on sm_86 or sm_70 at
all, and the expected VRAM. Licence is code / weights.

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 (sm_86) | V100 (sm_70) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| vLLM-Omni + Qwen3-TTS 1.7B | https://github.com/vllm-project/vllm-omni, https://github.com/QwenLM/Qwen3-TTS | Apache-2.0 / Apache-2.0 | vLLM-Omni 0.32.0 (date `(verify)`); Qwen3-TTS Jan 2026 | OpenAI `/v1/audio/speech` plus `/v1/audio/voices`, `/batch`, websocket `/stream` | sync (batched inside) | raw PCM/WAV chunks, SSE, websocket | yes; ~3.9 GB used, budget per stage (§4) | **no** (current docs: cc 7.5+) | **primary** |
| llama.cpp `llama-server` + Qwen3-TTS Base GGUF | https://github.com/ggml-org/llama.cpp | MIT / Apache-2.0 | v1.0.0, 2026-11-13 | OpenAI `/v1/audio/speech` (converted to native `/tts`) `(verify)` | sync | `stream_format: "audio"` `(verify)`, upstream PR #31763 | yes; ~3-4 GB `(verify)` | yes (ggml CUDA builds `70-virtual`; host already runs it on the V100s) | **fallback** |
| faster-qwen3-tts | https://github.com/andimarafioti/faster-qwen3-tts | MIT `(verify)` / Apache-2.0 | PyPI 0.5.4 (date `(verify)`) | OpenAI `/v1/audio/speech` in `examples/openai_server.py` | sync | yes (`wav`, `pcm`, `mp3`) | yes; RTF 5.6 on a 4090 | maybe: PyTorch ≥ 2.5.1 with cu126 and fp16 `(verify)` | strong engine, but the server is an example script with no confirmed health or voices route; re-check if the GGUF route disappoints |
| Kokoro-FastAPI (Kokoro-82M) | https://github.com/remsky/Kokoro-FastAPI | Apache-2.0 `(verify)` / Apache-2.0 | v0.3.0, 2026-05-15 | OpenAI `/v1/audio/speech`, `/v1/audio/voices`, `/v1/models` | sync | `stream` flag, chunked `(verify)` | yes; ~2-3 GB `(verify)` | yes (default image is cu126) | **CPU stock-voice stanza** (§6.3), not a cast-voice fallback |
| speaches (Kokoro ONNX, Piper) | https://github.com/speaches-ai/speaches | MIT / per model | v0.9.0-rc.3, 2025-12-27 | OpenAI, multi-model | sync | raw or SSE `(verify)` | yes | yes (ONNX) | skip: model ids carry a `/` the router refuses, and models load lazily after `/health` is already 200 |
| VoxCPM2 (via vLLM-Omni) | https://github.com/OpenBMB/VoxCPM | Apache-2.0 / Apache-2.0 | VoxCPM2 2026-04; `voxcpm` 2.0.3 | OpenAI via vLLM-Omni; `voice` ignored | sync | as vLLM-Omni | yes; ~8 GB | no (vLLM-Omni) | second vLLM-Omni stanza if E2 prefers its 48 kHz output; same runtime, no new work |
| CosyVoice3 (Fun-CosyVoice3-0.5B) | https://github.com/FunAudioLLM/CosyVoice | Apache-2.0 / Apache-2.0 `(verify)` | `-2512` checkpoint | own FastAPI (`--port 50000`), or OpenAI via vLLM-Omni | sync | PCM via vLLM-Omni | yes | own server maybe `(verify)`; vLLM-Omni no | cloning only, no presets; reachable through vLLM-Omni if wanted |
| IndexTTS-2 / 2.5 | https://github.com/index-tts/index-tts | bilibili Model Use Licence (code and weights) | 2.5 released 2026-08-10 | none upstream; community OpenAI wrappers | sync | wrappers only | yes | 2.5 defaults to bf16 (no); 2 has `use_fp16` `(verify)` | skip: restrictive licence, no upstream server |
| Chatterbox (Original, Multilingual, Turbo) | https://github.com/resemble-ai/chatterbox, server https://github.com/devnen/Chatterbox-TTS-Server | MIT `(verify)` / MIT `(verify)` | server v2.0.0 (date `(verify)`) | OpenAI `/v1/audio/speech`, `/v1/audio/voices` | sync | `(verify)`; the AGPL-3.0 `travisvn/chatterbox-tts-api` has chunked WAV and SSE | yes | likely, fp32 by default `(verify)` | viable third-party server; model chosen by config, not request; no published VRAM |
| Orpheus 3B | https://github.com/canopyai/Orpheus-TTS, server https://github.com/Lex-au/Orpheus-FastAPI | Apache-2.0 / base model's community licence `(verify)` | Orpheus-FastAPI v1.2.0, 2025-04-12 | OpenAI `/v1/audio/speech` front end | sync | `(verify)` | yes | GGUF token server likely `(verify)` | skip: two processes per model (front end plus token server), no cloning |
| F5-TTS | https://github.com/SWivid/F5-TTS | MIT / **CC-BY-NC-4.0** | v1 Base weights 2025-03-12 | none upstream | — | — | yes | `(verify)` | skip: non-commercial weights, no server |
| XTTS-v2 (coqui-tts fork, AllTalk) | https://github.com/idiap/coqui-ai-TTS, https://github.com/erew123/alltalk_tts | MPL-2.0 / **CPML, non-commercial incl. output** | `coqui-tts` 0.27.x `(verify)` | AllTalk has OpenAI `/v1/audio/speech` | sync | own streaming API | yes | likely `(verify)` | skip: output-restricting licence; AllTalk is a whole app |
| Fish Speech / OpenAudio | https://github.com/fishaudio/fish-speech | Apache-2.0 / **CC-BY-NC-SA-4.0** or research licence | `(verify)` | own `POST /v1/tts` (msgpack), `/v1/references/*` | sync | `(verify)` | yes | `(verify)` | skip: not OpenAI-shaped, non-commercial weights |
| Higgs Audio v2 | https://github.com/boson-ai/higgs-audio | Apache-2.0 / own community licence `(verify)` | `(verify)` | OpenAI via its vLLM image | sync | yes | yes; ~9 GB one speaker, ~14 GB two | no (recent vLLM) | skip: cloning through its vLLM path reported broken (issue #151), large |
| Spark-TTS 0.5B | https://github.com/SparkAudio/Spark-TTS | Apache-2.0 / **CC-BY-NC-SA-4.0** | `(verify)` | CLI and web UI only | — | — | yes | `(verify)` | skip: non-commercial weights, no server |

More than six candidates are viable on paper. Only the first four ship an
OpenAI speech endpoint from the model's own project or a maintained server, and
only the first two serve E2's lead model with a health endpoint the router can
trust.

**One process per model, not one multi-model server.** vLLM-Omni can serve
many TTS models, but **one model per `vllm serve` process**, which is exactly the
router's unit. speaches and Chatterbox-TTS-Server load and swap models inside
one process. That duplicates the router's load/unload, and it hides real VRAM
use from the ledger (the process's `vram_mb` would have to cover whichever model
it loads next). Run each model as its own stanza. The router already owns
placement, idle sweep and eviction.

## 3. API surface of the recommended backend (vLLM-Omni)

Source: the vLLM-Omni [Speech API reference][omni-speech] and the
[text-to-speech online-serving guide][omni-tts]. Field names come from search
extracts of those pages, so the whole table is `(verify)` against the installed
version's `/docs` (checklist item 4).

[omni-speech]: https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/speech_api/
[omni-tts]: https://docs.vllm.ai/projects/vllm-omni/en/latest/user_guide/examples/online_serving/text_to_speech/

### Endpoints the router must proxy

| Method and path | Purpose | Body / response |
| --- | --- | --- |
| `POST /v1/audio/speech` | synthesise | JSON in; audio bytes, chunked PCM/WAV, or SSE out |
| `POST /v1/audio/speech/batch` | several inputs in one call | JSON in, JSON out `(verify)` |
| `GET /v1/audio/speech/stream` (websocket) | text streamed in, audio streamed out | session config, then text messages, then `input.done` |
| `GET /v1/audio/voices` | list preset and uploaded voices | JSON |
| `POST /v1/audio/voices` | upload a reference clip under a name (Base model) | multipart; field names `(verify)` |
| `DELETE /v1/audio/voices/{name}` | remove an uploaded voice | JSON |
| `GET /v1/models` | served model name | OpenAI list |
| `GET /health` | readiness | 200 with an empty body once the engine is up |
| `GET /metrics` | Prometheus | standard vLLM `(verify)` |

There is **no unload endpoint**. The router unloads by killing the process
group, which also takes the per-stage worker processes vLLM-Omni starts.

### `POST /v1/audio/speech` request

- `model`: optional when one model is served. If present, it **must equal**
  `--served-model-name`, or the server answers `404` (below).
- `input`: the text. No maximum length is documented `(verify)`.
  `max_new_tokens` bounds the output, and out-of-range values are rejected (PR
  #4749).
- `voice`: a preset (CustomVoice checkpoints, for example `vivian`) or an
  uploaded or `custom_voice_dir` name. Presets are listed by `GET
  /v1/audio/voices`.
- `language`: for example `"English"`, `"Chinese"`. The consumer must always
  send it (E1).
- `task_type`: `CustomVoice` (default), `VoiceDesign` or `Base`. Uploaded and
  precomputed voices always run as `Base`, so they need a Base checkpoint.
- `ref_audio`: an inline `data:audio/wav;base64,...` URI or a URL. `ref_text`
  holds its transcript. Inline goes through vLLM's audio loader (WAV, MP3, AAC,
  WebM, MP4). An uploaded clip used to accept only what `soundfile` reads
  (fixed in PR #10673 `(verify)`).
- `instructions`: style prompt (CustomVoice and VoiceDesign).
- `response_format`: `wav`, `mp3`, `flac`, `pcm`, `aac`, `opus`.
- `speed`: must be `1.0` or absent when streaming. Qwen3-TTS has no real speed
  control.
- `stream` (bool) and `stream_format` (`"audio"` for raw bytes, `"sse"` for
  events). Both streaming modes need `response_format` `pcm` or `wav`.
- `word_timestamps: true`: timings come back in an `X-Word-Timestamps`
  **response header**.
- `non_streaming_mode`: prompt construction only. It does not change HTTP
  streaming.

### Response

- Non-streaming: the whole file, `Content-Type` per format (for example
  `audio/wav`).
- `stream_format: "audio"`: raw 16-bit 24 kHz mono PCM (or WAV) in chunks as
  decoded. Issue #3326 reports an older build concatenating two generations in
  this mode (about twice the WAV duration). Check it on the installed version
  (checklist item 5).
- `stream_format: "sse"` (or `stream: true` with SSE): OpenAI
  `speech.audio.delta` events, `{"type":"speech.audio.delta","audio":"<base64>","response_format":"pcm"}`,
  then a completion event `(verify)`.
- Websocket: per session `stream_audio` (default `false`, so audio arrives at
  the end) and `text_input_mode` (`buffered` default; `commitment` needs
  Qwen3-TTS and an explicit Chinese or English `language`).

### Errors

The OpenAI error envelope:
`{"error":{"message":...,"type":...,"param":...,"code":...}}`.

- `404 NotFoundError`, `"The model xxx does not exist."`, `param: "model"`:
  `model` does not match the served name.
- `400 Invalid voice '<name>'`: unknown preset. The OmniVoice example hit this
  with `"default"`.
- `400 "TTS model did not produce audio output."` (issue #1650): typically the
  wrong checkpoint for the `task_type`.
- A fork reports that a Base request with **no** voice fell back to the preset
  `vivian`, raised `Unsupported speaker`, and killed the engine, so every later
  request failed until the process restarted (ht-vllm-omni #60, a downstream
  fork; upstream status `(verify)`). For the router this means a crash and
  restart with backoff. For the consumer it means **always send `ref_audio` or
  an uploaded `voice` to a Base model.**

### Sync and size

Each HTTP request is held until the audio is done or the stream ends. The
router holds the client connection the same way, and there is no job API.
Requests are batched inside the engine up to each stage's `max_num_seqs`. Sizes:
a 10-second 24 kHz 16-bit mono reference is about 480 KB raw, or about 640 KB as
base64 in the JSON body. The router's matcher reads the whole body into memory,
which is fine at that size.

## 4. Hardware fit

### Current occupancy (from the GPU host's node config)

- **24 GB card (GPU 0):** the intent model (1500 MB), the embedding model
  (3600 MB) and the reranker (1500 MB) are resident (`idle_ttl_seconds: 0`),
  6600 MB booked. Image stanzas (12000 to 22000 MB, `idle_ttl_seconds: 300`)
  prefer this card.
- **32 GB cards (GPU 1 and 2):** the large chat model is split across both
  (`device: "1,2"`, `vram_mb: 64000`, resident, unpinned, about a 7-minute cold
  start).
- **Correction to the issue text:** the issue asks about co-residency "with
  the large chat model and the embedding model on the 3090". The node config
  places the large chat model on the two 32 GB cards, not the 3090. The 3090
  holds the intent, embedding and reranker trio. The CPU-affinity note in the
  same config also mentions a second large model on the 3090's NUMA node with
  no stanza. Whether anything outside the router holds VRAM on GPU 0 is
  checklist item 1.

### Primary: vLLM-Omni + Qwen3-TTS 1.7B (24 GB card only)

| Item | Value |
| --- | --- |
| Weights | talker 3.66 GiB + code2wav 0.45 GiB (bf16) |
| Measured use | ~3.9 GB on a 3090 for 1.7B CustomVoice, same as the reference backend; a third-party blog says 5.6 GB; one user saw ~8 GB `(verify)` |
| What vLLM reserves | each stage takes `total × gpu_memory_utilization`, capped at free memory when stages share a card. Weights plus KV cache and activations must fit inside that |
| Proposed budget | stage 0 (talker) `0.22`, stage 1 (code2wav) `0.08` → ~7.4 GB plus per-process CUDA context → **book `vram_mb: 9000`** until measured |
| Precision | bf16 by default; fine on sm_86 |
| Cold start | the reference backend loads in ~11 s; a 3090 benchmark puts vLLM-Omni at ~10× that, so **~100-120 s** `(verify)`; slow stage init is why some recipes pass `--stage-init-timeout` |
| sm_70 | **not supported**: current installation docs say compute capability 7.5 or higher; docs for v0.12 to v0.28 said 7.0, and nothing found says when or why the floor moved `(verify)` |

**Co-residency on GPU 0:** 6600 (trio) + 9000 (TTS) = 15600 MB booked, leaving
about 8.9 GB. The trio and TTS coexist. Any image stanza (12000 MB or more)
needs the TTS process gone. TTS is a non-resident with a TTL, so the eviction
picker takes it before any resident. Expect ping-pong when image and speech jobs
interleave, with each TTS return costing a ~2-minute cold start. A longer TTS
`idle_ttl_seconds` does not stop that: eviction for a load ignores the TTL. Only
`pin` or `hold_seconds` does, and neither is proposed here, because the image
stanzas need the card more.

**Venv:** the existing sidecar venv (torch 2.12.1 cu130) is unrelated.
vLLM-Omni must match vLLM's major and minor version exactly. Give it its own
venv (`/opt/ai/venv-vllm-omni`), or run the `vllm/vllm-omni` Docker image in the
foreground. The image needs `--gpus` mapped from `{gpu}`, and the router's
process-group kill does not reach the container, so prefer the venv.

### Fallback: llama-server + Qwen3-TTS 1.7B Base GGUF (32 GB cards or CPU)

| Item | Value |
| --- | --- |
| Files | backbone `Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf` plus `--mmproj mmproj-Qwen3-TTS-12Hz-1.7B-Base-bf16.gguf` (speaker encoder and code predictor); file names `(verify)` against ggml-org/llama.cpp PR #26254 and the published GGUF repo |
| VRAM | ~3-4 GB at Q8_0 `(verify)`; book `vram_mb: 4000` until measured |
| sm_70 | ggml CUDA builds `70-virtual` for CUDA < 13, and the host's `llama-server` already serves chat on the V100s. The **bf16 mmproj** on Volta is `(verify)`; if it fails, convert the mmproj to f16 |
| CPU | `--n-gpu-layers 0` and `device: cpu`; speed **unknown**, has to be measured (checklist item 8) |
| Cold start | seconds, like the other `llama-server` stanzas `(verify)` |
| Version | needs **v1.0.0 or later** for `/v1/audio/speech` (ggml-org/llama.cpp PR #31763 `(verify)`) and `/tts` (since v0.8.0, PR #26603). The host's build version is checklist item 2 |

**Placement cost on the 32 GB cards:** the large chat model books 64000 MB of
the two cards' ~65500 MB. A 4000 MB TTS load on `"1|2"` therefore evicts it.
Preload restore only brings it back once it fits without evicting, so chat is
down for the whole TTS session, plus the 5 minute idle TTL before the TTS
stanza stops, plus the ~7 minute reload. That is why the fallback is for when an image job
holds GPU 0, or when the operator has deliberately freed a 32 GB card. It is not
a routine second home.

### CPU-only option: Kokoro-82M (Kokoro-FastAPI)

- 82M parameters, Apache-2.0 weights, 54 voices in 8 languages (US and UK
  English, Spanish, French, Hindi, Italian, Japanese, Brazilian Portuguese,
  Mandarin). The model card warns that non-English support is thin.
- Published speed: GPU 35-100× realtime on a 4060 Ti; **CPU about 2.4×
  realtime** with the ONNX path; first chunk ~3.5 s on an older i7 at chunk size
  200, under 1 s on an M3 Pro (Kokoro-FastAPI README, older and current
  revisions `(verify)`).
- Other small CPU models, for scale: Kitten TTS (Apache-2.0, ~25 MB, ~5×
  realtime user report), Supertonic 3 (99M, 31 languages, MIT code with
  OpenRAIL-M weights, 5× realtime on a 16-thread CPU per its benchmark). Neither
  has a server the router could front without a wrapper.
- **CPU budget on this host:** the node config pins the large chat model to
  NUMA 1 cores (`14-26,42-54`) and reserves `13,27,41,55` for the system. A CPU
  TTS stanza competes for cores. Pin it with `taskset` (§6.3), and decide which
  cores in open question 3.

## 5. How the router would run it

### Spawn (primary)

```
/opt/ai/venv-vllm-omni/bin/vllm serve /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base
  --omni
  --deploy-config /opt/ai/config/vllm-omni/qwen3_tts_24gb.yaml
  --served-model-name qwen3-tts-1.7b
  --host 127.0.0.1 --port {port}
  --trust-remote-code --enforce-eager
```

- Foreground: `vllm serve` stays in the foreground and forks its stage workers
  into the same process group, so the router's SIGTERM-to-group stop covers
  them.
- `{gpu}` is not needed: the stanza is fixed to GPU 0 (`device: 0`,
  `CUDA_VISIBLE_DEVICES=0`), like the trio.
- **Per-stage memory goes in a deploy file**, not on the command line.
  `--stage-overrides '{"0":{...}}'` cannot be written in a stanza, because the
  router's command splitter drops double quotes and has no escape. Copy the
  shipped `vllm_omni/deploy/qwen3_tts.yaml` to
  `/opt/ai/config/vllm-omni/qwen3_tts_24gb.yaml` and set stage 0
  `gpu_memory_utilization: 0.22` and stage 1 `0.08`, with `max_num_seqs: 4` on
  each (exact key layout `(verify)` against the shipped file).
- Local weights path plus `HF_HUB_OFFLINE=1`: a spawn never downloads.
- `--enforce-eager` follows the official example. It starts faster and uses
  less memory. The performance guide prefers CUDA graphs on the talker, so try
  both (checklist item 7).

### Stanza fields

| Field | Primary | Fallback | Why |
| --- | --- | --- | --- |
| `health_check` | `/health` | `/health` | vLLM's API server listens only after the stages are up, so a 200 means ready; llama-server answers 503 while loading. Neither loads lazily on first request |
| `spin_up_seconds` | `300` | `60` | ~2 min estimated cold start, doubled; neither reports `progress` |
| `vram_mb` | `9000` | `4000` | §4 budgets; replace with measured peaks |
| `idle_ttl_seconds` | `900` | `300` | a 2-minute reload deserves a longer idle window; eviction for an image load ignores it anyway |
| `slots` | `4` | `1` | matches `max_num_seqs: 4`; llama-server `--parallel 1` (the router derives 1 anyway) |
| `match` | `{path_prefix: /v1/audio/voices, path_default: true}` | `{body_field: model}` | the primary owns model-less `/v1/audio/speech` (api_type default) and model-less `/v1/audio/voices` (prefix). A request naming a model still wins |
| `api_type` | `speech` | `speech` | |
| `capabilities` | `[audio_out]` | `[audio_out]` | |

### What has to be rewritten: nothing, but only because of three settings

The router proxies bytes. It rewrites no path, no `model` and no header for a
local stanza. That works only if:

1. **`--served-model-name` equals the router `model_id`.** Otherwise every
   request that names the model gets the backend's `404 "The model
   qwen3-tts-1.7b does not exist."` after the router has already resolved and
   loaded it. A client that sends the Hugging Face id fails earlier, at the
   router, because of the slash. Aliases such as `tts-1` have the same problem:
   the alias resolves at the router, then reaches vLLM-Omni unchanged. Either
   list each alias in `--served-model-name` too (vLLM accepts several names
   `(verify)`), or do not alias.
2. **GET requests carry `?model=`** when they are not for the default stanza:
   `GET /v1/audio/voices?model=kokoro`, and the websocket
   `/v1/audio/speech/stream?model=...`. The matcher reads GET models from the
   query only. A websocket upgrade with no query falls to the speech api_type
   default, which is the primary.
3. **The client always sends the reference audio and the language.** The
   router cannot add or check either (filters cannot express "required", and a
   default would be the silent fallback E1 forbids).

What breaks with pure passthrough, plainly:

- **Primary to fallback is not a drop-in swap.** vLLM-Omni takes `ref_audio`
  (a `data:` URI) plus `ref_text`. llama-server takes `speaker_ref_b64` (and
  `lang`, not `language`) `(verify)` against ggml-org/llama.cpp PR #31763. A request built for one and sent to the other
  probably ignores the unknown field and speaks with no reference `(verify)`.
  That is why rule 3 in §1 forbids a pool. Making a pool safe needs a **router
  change**: a per-stanza field mapping (rename `ref_audio` to
  `speaker_ref_b64`, strip the `data:` prefix), which the current `Filters`
  (set and strip only) cannot do.
- **llama-server has no `/v1/audio/voices`.** Its voices are in `GET /props`
  (proxied, needs `?model=`). A client listing voices against the fallback gets
  the backend's 404.
- **llama-server's native `/tts`** (multipart `speaker_ref` upload) is outside
  the router's route table. It is reachable only as `POST
  /tts?model=qwen3-tts-1.7b-gguf`.
- **Activity capture** keeps 256 KB per body, so the operator UI shows a
  truncated audio response. Cosmetic only.

No router change is required to ship the primary and the fallback as separate,
explicitly named stanzas.

## 6. Proposed stanzas

Paths, file names and ports are placeholders. Ports sit above the node config's
current allocations and the STT note's `5930-5932`; check `reserved_ports`
before deploying. Every key is a documented `Stanza` field (`SPEC.md` config
block; `internal/config/config.go` YAML tags). No new key is needed.

### 6.1 Primary, 24 GB card (sm_86)

```yaml
  - model_id: qwen3-tts-1.7b
    name: "Qwen3-TTS 1.7B Base (voice clone, vLLM-Omni, speech)"
    command: >
      /opt/ai/venv-vllm-omni/bin/vllm serve /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base
      --omni
      --deploy-config /opt/ai/config/vllm-omni/qwen3_tts_24gb.yaml
      --served-model-name qwen3-tts-1.7b
      --host 127.0.0.1 --port {port}
      --trust-remote-code --enforce-eager
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0", "HF_HUB_OFFLINE=1"]
    vram_mb: 9000
    device: 0
    spin_up_seconds: 300
    api_type: speech
    capabilities: [audio_out]
    match: {path_prefix: /v1/audio/voices, path_default: true}
    health_check: /health
    idle_ttl_seconds: 900
    slots: 4
    port: 5933
```

### 6.2 Fallback, 32 GB cards (sm_70)

```yaml
  - model_id: qwen3-tts-1.7b-gguf
    name: "Qwen3-TTS 1.7B Base (voice clone, GGUF Q8_0, llama-server, speech)"
    command: >
      /opt/ai/bin/llama-server
      --model /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf
      --mmproj /opt/ai/models/tts/mmproj-Qwen3-TTS-12Hz-1.7B-Base-bf16.gguf
      --host 127.0.0.1 --port {port}
      --n-gpu-layers 999 --parallel 1 --metrics
      --split-mode none --device CUDA0
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 4000
    device: "1|2"
    spin_up_seconds: 60
    api_type: speech
    capabilities: [audio_out]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 300
    slots: 1
    port: 5934
```

Do not add `--tts-speaker-file`: it would give every request without a
reference a fixed voice, which is a silent fallback voice. If the operator wants
a named cast voice served without a clip per request, make that its own stanza
(`model_id` = the voice) with the speaker file, so the voice is chosen by name.

The CPU variant of the fallback is the same stanza with `device: cpu`,
`vram_mb: 0`, `--n-gpu-layers 0` and no `CUDA_VISIBLE_DEVICES`. Propose it only
after checklist item 8 gives a usable speed.

### 6.3 Optional: stock voices on CPU (Kokoro-FastAPI)

```yaml
  - model_id: kokoro
    name: "Kokoro 82M (stock voices, CPU, speech)"
    command: >
      taskset -c 13,27,41,55
      /opt/ai/venv-kokoro/bin/uvicorn api.src.main:app
      --app-dir /opt/ai/kokoro-fastapi
      --host 127.0.0.1 --port {port} --workers 1
    env:
      - USE_GPU=false
      - USE_ONNX=false
      - PYTHONPATH=/opt/ai/kokoro-fastapi:/opt/ai/kokoro-fastapi/api
      - MODEL_DIR=/opt/ai/kokoro-fastapi/api/src/models
      - VOICES_DIR=/opt/ai/kokoro-fastapi/api/src/voices/v1_0
    vram_mb: 0
    device: cpu
    spin_up_seconds: 120
    api_type: speech
    capabilities: [audio_out]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 0
    slots: 1
    port: 5935
```

- `model_id` must be literally `kokoro`. Kokoro-FastAPI rejects any `model`
  outside `kokoro`, `tts-1` and `tts-1-hd` with
  `{"error":"invalid_model",...}`, and the router passes the id through
  unchanged.
- The upstream start script hard-codes `--port 8880` and relative model
  directories, and the router has no working-directory key. Hence calling
  `uvicorn` directly and passing absolute directories in `env`. Whether those
  variables accept absolute paths is `(verify)`. If not, a two-line wrapper
  script that `cd`s first is the fallback, not a router change.
- The taskset cores are the system cores from the node config (open question
  3).
- `/health` returning `{"status":"healthy"}` is from a client project, not the
  upstream source `(verify)`. Uvicorn does not accept connections until the app
  start-up (model load and warm-up) has finished, so 200 means ready.

## 7. Licence and risk flags

Internal use only, but these are the terms to know if audio ever leaves the
LAN.

- **Qwen3-TTS** code and the 1.7B Base weights: **Apache-2.0** (repo
  `LICENSE`; model card tag `license:apache-2.0`, not gated). One secondary
  source says only "open source" for the 1.7B CustomVoice card. Check that card
  if CustomVoice is used `(verify)`.
- **vLLM-Omni**: Apache-2.0. **llama.cpp**: MIT. **faster-qwen3-tts**: MIT
  `(verify)`. **Kokoro-82M** weights: Apache-2.0. **Kokoro-FastAPI**: Apache-2.0
  `(verify)`. **VoxCPM2**: Apache-2.0. **CosyVoice3**: Apache-2.0 `(verify)`.
- **Non-commercial or restricted**: F5-TTS weights CC-BY-NC-4.0; Spark-TTS
  weights CC-BY-NC-SA-4.0; Fish Speech / OpenAudio weights CC-BY-NC-SA-4.0 or a
  research licence; XTTS-v2 under the Coqui Public Model Licence, which covers
  the **generated audio**, and the issuer has shut down, so no commercial
  licence can be bought; IndexTTS-2/2.5 under the bilibili Model Use Licence
  (commercial use needs written permission); Higgs Audio v2 and Orpheus carry
  community licences inherited from their base models `(verify)`.
  `travisvn/chatterbox-tts-api` is AGPL-3.0, which matters only if it is
  modified and offered over a network.
- **Voice cloning risk**: Base models clone from about 3 seconds of audio. The
  router has no auth beyond its API keys, so anyone who can reach
  `/v1/audio/speech` can clone any clip they hold. Keep TTS stanzas behind the
  router's `security.api_keys` if the LAN is not trusted.
- **URL fetch in `ref_audio`**: vLLM-Omni accepts a URL as well as a `data:`
  URI. The backend then fetches from the GPU host's network position. Prefer
  `data:` URIs. URI schemes are validated since PR #4749 `(verify)`.
- **Engine death on a bad request**: the downstream fork report above (Base
  model, no voice) took the engine down. The router's restart-with-backoff
  covers recovery, but every in-flight request on that process fails. Clients
  must send a reference or an uploaded voice.
- **Maturity**: llama-server's speech endpoint shipped on 2026-11-13. The
  fallback is a week old.

## 8. GPU host verification checklist and open questions

Run as the service user. Steps 1 and 2 are read-only. **Steps 3 to 8 change GPU
state and need an agreed window**: they start backends by hand, outside the
router, which does not count that memory in its ledger. Before each GPU step,
check the card's free memory and unload any router stanza on it; after it,
`kill` the process. Expect the unloaded models (and, on the V100s, the large
chat model) to be down until preload restore brings them back.

```bash
# 1. Cards, compute capability, and what holds GPU 0 right now (anything not a router stanza?).
nvidia-smi --query-gpu=index,name,compute_cap,memory.total,memory.used,memory.free --format=csv
nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv
curl -s http://127.0.0.1:18080/_router/status | head -c 2000

# 2. llama-server build: needs v1.0.0+ for /v1/audio/speech; CUDA arch list must include 70.
/opt/ai/bin/llama-server --version
/opt/ai/bin/llama-server --help | grep -i -E 'tts|mmproj'

# 3. vLLM-Omni venv (to be created): versions must match; sm_86 only.
/opt/ai/venv-vllm-omni/bin/python -c "import vllm, vllm_omni, torch; print(vllm.__version__, vllm_omni.__version__, torch.__version__, torch.cuda.get_arch_list())"

# 4. GPU STATE CHANGE. Start the primary by hand on GPU 0, time the cold start, check the API surface.
#    First: no image job running; unload every router stanza on GPU 0 (see /_router/status), then
#    confirm >= 10000 MiB free on GPU 0 before starting.
curl -s -X POST http://127.0.0.1:18080/_router/unload -d '{"model_id":"<each stanza on GPU 0>"}'
nvidia-smi -i 0 --query-gpu=memory.free --format=csv
date +%s; CUDA_VISIBLE_DEVICES=0 HF_HUB_OFFLINE=1 /opt/ai/venv-vllm-omni/bin/vllm serve \
  /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base --omni \
  --deploy-config /opt/ai/config/vllm-omni/qwen3_tts_24gb.yaml \
  --served-model-name qwen3-tts-1.7b --host 127.0.0.1 --port 5960 \
  --trust-remote-code --enforce-eager &
VLLM_PID=$!
until curl -sf http://127.0.0.1:5960/health; do sleep 2; done; date +%s   # spin_up_seconds
curl -s http://127.0.0.1:5960/v1/models
curl -s http://127.0.0.1:5960/v1/audio/voices
curl -s http://127.0.0.1:5960/openapi.json | python3 -c "import json,sys; print(sorted(json.load(sys.stdin)['paths']))"

# 5. Clone, non-stream and stream; the wrong model name must 404; streamed PCM length ≈ WAV length.
REF=$(base64 -w 0 /tmp/ref-fictional-speaker.wav)
curl -s http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"model\":\"qwen3-tts-1.7b\",\"input\":\"Testing one two three.\",\"language\":\"English\",\"task_type\":\"Base\",\"ref_audio\":\"data:audio/wav;base64,$REF\",\"ref_text\":\"<transcript of the clip>\",\"response_format\":\"wav\"}" -o /tmp/tts.wav
curl -s http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"model\":\"qwen3-tts-1.7b\",\"input\":\"Testing one two three.\",\"language\":\"English\",\"task_type\":\"Base\",\"ref_audio\":\"data:audio/wav;base64,$REF\",\"ref_text\":\"<transcript of the clip>\",\"response_format\":\"pcm\",\"stream\":true,\"stream_format\":\"audio\"}" -o /tmp/tts.pcm
ls -l /tmp/tts.wav /tmp/tts.pcm
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d '{"model":"wrong-name","input":"x","language":"English"}'                      # expect 404
# Base model with no voice and no ref_audio: does it 400, or kill the engine (then /health fails)?
curl -s http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' -d '{"input":"x","language":"English"}'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5960/health
kill $VLLM_PID   # step 7's GPU 0 peak must be read before this

# 6. GPU STATE CHANGE. Fallback on a V100 with the same clip; compare by ear with /tmp/tts.wav.
#    The large chat model books 64000 MB across both V100s: unload it first (chat is down until
#    preload restore), then confirm >= 5000 MiB free on the card you use. Pick the card, do not assume 1.
curl -s -X POST http://127.0.0.1:18080/_router/unload -d '{"model_id":"qwen38-flash-next"}'
nvidia-smi --query-gpu=index,memory.free --format=csv
CUDA_VISIBLE_DEVICES=<free V100 index> /opt/ai/bin/llama-server \
  --model /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf \
  --mmproj /opt/ai/models/tts/mmproj-Qwen3-TTS-12Hz-1.7B-Base-bf16.gguf \
  --host 127.0.0.1 --port 5961 --n-gpu-layers 999 --parallel 1 &
LLAMA_PID=$!
until curl -sf http://127.0.0.1:5961/health; do sleep 2; done
curl -s http://127.0.0.1:5961/props | head -c 1500                                  # voices, modalities
curl -s http://127.0.0.1:5961/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"model\":\"qwen3-tts-1.7b-gguf\",\"input\":\"Testing one two three.\",\"lang\":\"en\",\"speaker_ref_b64\":\"$REF\"}" -o /tmp/tts-gguf.wav
# Same request with vLLM-Omni's field names: is the reference silently ignored?
curl -s http://127.0.0.1:5961/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"input\":\"Testing one two three.\",\"language\":\"English\",\"ref_audio\":\"data:audio/wav;base64,$REF\"}" -o /tmp/tts-gguf-wrongfields.wav
kill $LLAMA_PID   # after step 7's peak reading

# 7. Peak VRAM for vram_mb (both backends), eager vs CUDA graphs for the primary.
nvidia-smi --query-gpu=index,memory.used --format=csv -l 1   # run during steps 5 and 6, note the peak

# 8. CPU speed: Kokoro and the GGUF fallback on the system cores (audio seconds / wall seconds).
#    Start Kokoro-FastAPI by hand first (the §6.3 command and env, --port 5962), then:
time curl -s http://127.0.0.1:5962/v1/audio/speech -H 'Content-Type: application/json' \
  -d '{"model":"kokoro","input":"A fictional sentence of about twenty words, read aloud to measure the real-time factor on this host.","voice":"af_heart","response_format":"wav"}' -o /tmp/kokoro.wav
#    Repeat step 6's request against the §6.2 command with --n-gpu-layers 0 under the same taskset.
#    kill both CPU processes afterwards; they take cores the router's CPU stanzas use.

# 9. Through the router once stanzas are staged: model-less default, voices default, wrong id.
curl -s http://127.0.0.1:18080/v1/models | grep -o -E 'qwen3-tts-1.7b(-gguf)?|kokoro'
curl -s http://127.0.0.1:18080/v1/audio/voices | head -c 500
```

`/tmp/ref-fictional-speaker.wav` is any clip of a consenting speaker. Use a
recording made for the test, not a real person's voice from another source.

### Open questions for the operator

1. **Is the 24 GB card the TTS home**, accepting that image jobs evict TTS and
   each return costs ~2 minutes? The alternative is a 32 GB card, which means
   evicting the large chat model (~7 minutes).
2. **Base, CustomVoice or both?** The consumer clones cast voices, so Base is
   proposed. CustomVoice (nine presets plus `instructions`) would be a second
   vLLM-Omni stanza of the same size, and it cannot share the card with Base
   beside the trio and an image job.
3. **Which cores may a CPU speech stanza use?** The system cores
   (`13,27,41,55`), or the NUMA 0 range the affinity note gives to a model that
   has no stanza?
4. **Should the router grow a field mapping** so a pool can fail over between
   the primary and the fallback without the client knowing? Without it, rule 3
   in §1 stands.
5. **Is Kokoro wanted at all?** It is only useful if something consumes stock
   voices (previews, system prompts). E1's contract makes it unusable as a
   substitute voice.
6. **E2's newest verdict**: confirm against the deepening ticket (#47) that
   Qwen3-TTS 1.7B is still the lead. If E2 moved to VoxCPM2 or CosyVoice3, the
   primary stays vLLM-Omni and only the model path changes. The fallback then
   disappears, because neither has a confirmed `llama-server` GGUF path
   `(verify)`.

## 9. Sources

All read on 2026-11-20 through search-engine extracts (no direct page fetch in
this run).

Consumer contract and model selection (not reachable from this run)

- https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md
- https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e1-voice-path-diagnosis.md

vLLM-Omni and Qwen3-TTS

- https://github.com/vllm-project/vllm-omni
- https://github.com/vllm-project/vllm-omni/releases
- https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/speech_api/
- https://github.com/vllm-project/vllm-omni/blob/main/docs/serving/speech_api.md
- https://docs.vllm.ai/projects/vllm-omni/en/latest/user_guide/examples/online_serving/text_to_speech/
- https://docs.vllm.ai/projects/vllm-omni/en/v0.18.0/user_guide/examples/online_serving/qwen3_tts/
- https://docs.vllm.ai/projects/vllm-omni/en/latest/getting_started/installation/gpu/
- https://github.com/vllm-project/vllm-omni/blob/main/docs/getting_started/installation/gpu/cuda.inc.md
- https://docs.vllm.ai/projects/vllm-omni/en/latest/configuration/gpu_memory_utilization/
- https://docs.vllm.ai/projects/vllm-omni/en/latest/configuration/stage_configs/
- https://docs.vllm.ai/projects/vllm-omni/en/latest/design/qwen3_omni_tts_performance_optimization/
- https://github.com/vllm-project/vllm-omni/issues/3326 (stream PCM concatenation)
- https://github.com/vllm-project/vllm-omni/issues/1650 (no audio output, 400)
- https://github.com/vllm-project/vllm-omni/issues/3821 (invalid voice, 400)
- https://github.com/vllm-project/vllm-omni/issues/2115 (TTS roadmap, websocket)
- https://github.com/vllm-project/vllm-omni/issues/7993 (websocket session limits)
- https://github.com/vllm-project/vllm-omni/pull/4749 (request validation)
- https://github.com/vllm-project/vllm-omni/pull/7795 (voice delete)
- https://github.com/vllm-project/vllm-omni/pull/10673 (voice upload formats)
- https://github.com/heiervang-technologies/ht-vllm-omni/issues/60 (engine death, downstream fork)
- https://arxiv.org/pdf/2610.09307 (vLLM-Omni technical report, fused profile)
- https://aws.amazon.com/blogs/machine-learning/deploying-real-time-personalized-speech-with-qwen3-tts-on-amazon-sagemaker-ai/ (stage weights, memory split)
- https://github.com/nari-labs/benchmarks/blob/main/reports/Qwen3-TTS-1.7B-CustomVoice/vLLM-Omni/README.md
- https://github.com/groxaxo/Qwen3-TTS-Openai-Fastapi/blob/main/BENCHMARK_RESULTS.md (3090 VRAM and cold start)
- https://github.com/QwenLM/Qwen3-TTS
- https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice
- https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign
- https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice/discussions/18 (~8 GB report)

llama.cpp

- https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
- https://github.com/ggml-org/llama.cpp/blob/master/tools/tts/README.md
- https://github.com/ggml-org/llama.cpp/pull/26254 (Qwen3-TTS in mtmd)
- https://github.com/ggml-org/llama.cpp/pull/31763 (`/v1/audio/speech`; field names `(verify)` here)
- https://github.com/ggml-org/llama.cpp/pull/26603 (`/tts`)
- https://github.com/ggml-org/llama.cpp/releases/tag/v1.0.0 (release notes; read only via a mirror so far, `(verify)` upstream)
- https://github.com/dwain-barnes/llama-tts-server (third-party wrapper; Base has no voice of its own)

faster-qwen3-tts

- https://github.com/andimarafioti/faster-qwen3-tts
- https://github.com/andimarafioti/faster-qwen3-tts/blob/main/BLOG.md
- https://github.com/andimarafioti/faster-qwen3-tts/pull/99
- https://pypi.org/project/faster-qwen3-tts/

Kokoro, speaches and small CPU models

- https://github.com/remsky/Kokoro-FastAPI
- https://github.com/remsky/Kokoro-FastAPI/blob/master/CHANGELOG.md
- https://github.com/remsky/Kokoro-FastAPI/blob/master/api/src/routers/openai_compatible.py
- https://github.com/remsky/Kokoro-FastAPI/blob/master/start-gpu.sh
- https://github.com/remsky/Kokoro-FastAPI/blob/master/docker/gpu/docker-compose.yml
- https://github.com/remsky/Kokoro-FastAPI/blob/master/docs/configuration.md
- https://huggingface.co/hexgrad/Kokoro-82M
- https://github.com/speaches-ai/speaches
- https://speaches.ai/usage/text-to-speech/
- https://speaches.ai/usage/model-discovery/
- https://speaches.ai/configuration/
- https://github.com/KittenML/kitten-tts
- https://github.com/supertone-inc/supertonic

Other candidates

- https://github.com/OpenBMB/VoxCPM
- https://huggingface.co/openbmb/VoxCPM2
- https://openbmb.cn/docs/en/guides/voxcpm/voxcpm-2/deploy
- https://github.com/FunAudioLLM/CosyVoice
- https://github.com/index-tts/index-tts
- https://github.com/index-tts/index-tts/blob/main/LICENSE
- https://github.com/resemble-ai/chatterbox
- https://github.com/devnen/Chatterbox-TTS-Server
- https://github.com/travisvn/chatterbox-tts-api
- https://github.com/canopyai/Orpheus-TTS
- https://github.com/Lex-au/Orpheus-FastAPI
- https://github.com/SWivid/F5-TTS
- https://huggingface.co/SWivid/F5-TTS
- https://github.com/idiap/coqui-ai-TTS
- https://github.com/erew123/alltalk_tts
- https://github.com/fishaudio/fish-speech
- https://speech.fish.audio/inference/
- https://github.com/boson-ai/higgs-audio
- https://github.com/boson-ai/higgs-audio/issues/151
- https://github.com/boson-ai/higgs-audio/issues/17
- https://github.com/SparkAudio/Spark-TTS
- https://huggingface.co/SparkAudio/Spark-TTS-0.5B

This repo

- [`SPEC.md`](../../SPEC.md) (stanza fields, matcher, eviction)
- [`docs/research/stt-backends.md`](stt-backends.md) (port range, sibling backend shape)
- `internal/router/server.go`, `internal/router/matcher.go`,
  `internal/router/filters.go`, `internal/router/proc.go`,
  `internal/router/router.go`, `internal/config/config.go`
- the GPU host's node config under `configs/` (card occupancy, CPU affinity)
