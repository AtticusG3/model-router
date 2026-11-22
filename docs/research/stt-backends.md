[Docs home](../README.md) › Research › Local speech-to-text backends

# Local speech-to-text backends

> **Type:** Explanation · **Audience:** maintainers choosing a backend · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-20 (all URLs below were opened that day). Part of #64.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link; a fact that could not be confirmed from the
project's own files rather than a secondary write-up is marked `(verify)`, and
anything that only the GPU host can settle is in the checklist (§8).

## 0. Scope and method

The question: which locally runnable, server-style speech-to-text backend should
this router start and proxy the way it already starts `llama-server` and
`sd-server`, one foreground process per model, a `{port}` and a `{gpu}`, an HTTP
`health_check`, a `vram_mb` reservation, a kill to unload.

Constraints taken as given (from the issue, `README.md`, `SPEC.md` and the
staged node configs):

- three cards: one 24 GB sm_86 and two 32 GB sm_70. sm_70 means no bf16 and no
  FlashAttention-2; Python CUDA wheels built for sm_75+ do not run there, but
  CTranslate2 and `ggml` CUDA both still compile for `sm_70`.
- the router sets `CUDA_VISIBLE_DEVICES={gpu}` at spawn and proxies `/v1/*`
  bytes-unchanged; it resolves the stanza from a JSON or multipart `model`
  field, or from `match.path_prefix`. It has **no path-rewrite feature**
  (`internal/router/server.go` route table; only `/upstream/{model_id}/`
  strips a prefix), and its JSON filters do **not** touch multipart or
  form bodies (`internal/router/filters.go`).
- the client, per the issue, is a captioning/subtitling pipeline that needs
  **English and Japanese**, word-level timing, and sane behaviour on long files.
- the router already has a speech-shaped surface: `cmd/fake-model/main.go`
  serves `/v1/audio/speech`, `/v1/audio/transcriptions`,
  `/v1/audio/translations`, `/v1/audio/voices` and `/v1/realtime`, and the
  Playground has Speech / Transcription / Realtime tabs. Nothing real serves
  those on the GPU host today.

What this note does **not** redo: the router-landscape survey in
[`oss-model-router-landscape.md`](oss-model-router-landscape.md) (which already
covers speaches' load/unload shape), and the speech-*output* research in the
sibling captioning project ([voice-path diagnosis][sf-e1], [TTS hosting][sf-e2]).

[sf-e1]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e1-voice-path-diagnosis.md
[sf-e2]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md

## 1. Recommendation

**Primary: whisper.cpp `whisper-server`.** One process per model, started and
killed by the router exactly like `llama-server`: a C++ binary with no Python
runtime, `--model` at spawn, and a `/health` that only answers `200` once the
weights are loaded. It runs on all three cards (the CUDA build includes
`70-virtual`; Volta flash-attention kernels exist). It is MIT. It speaks the
OpenAI transcription path itself once started with `--request-path /v1/audio
--inference-path /transcriptions`, so **the router needs no code change** for
transcription. It ignores the `model` form field, which matters because the
router requires that field to resolve to one of *its* ids (§5).

**Fallback: speaches.** The OpenAI-shaped Python/faster-whisper server. It is
the only candidate surveyed here whose own API schema carries
`/v1/audio/translations`, Server-Sent-Events streaming transcription **and** a
speaker-diarization endpoint as first-class routes (the other multi-endpoint
option in §2, the containerised ScribeCrate front-end, is a wrapper whose
diarization is an optional extra, and vLLM's translations route is whisper-only).
It runs on both card families: CTranslate2 wheels include `70-real`.

**The rule:** run `whisper-server` unless the request needs an endpoint it does
not have, `/v1/audio/translations`, SSE streaming transcription, or
`/v1/audio/diarization`, or unless the only copy of a model you need is a
CTranslate2/Hugging-Face whisper conversion with no `ggml` build. Translation
for captions is not needed today (the client needs EN→EN and JA→JA
transcription, not x→EN), so the fallback exists mostly for diarization and
for multi-model hosting without a process per model.

Cards: `whisper-server` runs on the 24 GB sm_86 card and on both 32 GB sm_70
cards. speaches runs on all three too, but its diarization dependency
(pyannote 4.0) peaks above 9.5 GB and must not share a card (§4).

The one thing the router would need if you want the fallback's multi-model
behaviour is a multipart `model` rewrite; that is called out in §5 and §8.

## 2. Alternatives

`API shape` is what the process puts on the wire, not what the model is.
`3090`/`V100` is whether the serving runtime runs on sm_86 / sm_70 at all.

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 | V100 | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| whisper.cpp `whisper-server` | https://github.com/ggml-org/whisper.cpp | MIT | v1.9.5, 2026-10-07; latest commit 2026-11-20 | own `/inference`; OpenAI path via `--request-path` | sync | no | yes | yes | **primary** |
| speaches (was faster-whisper-server) | https://github.com/speaches-ai/speaches | MIT | v0.9.0-rc.3, 2025-12-27 (stable v0.8.3, 2025-09-19) | OpenAI | sync + SSE | yes (SSE, websocket realtime) | yes | yes | **fallback** |
| faster-whisper-server forks | https://github.com/andyjjrt/faster-whisper-server | not read `(verify)` | not read `(verify)` | OpenAI, multi-model | sync | no | yes | yes | skip, speaches supersedes; only worth it if you must pin the old name |
| WhisperX | https://github.com/m-bain/whisperX | BSD-2 | v3 line; context-aware batching 2026 | library, no server | sync | no | yes (cu126) | yes (cu126/int8) | aligner, not a server |
| whisper-asr-webservice | https://github.com/ahmetoner/whisper-asr-webservice | MIT | v1.10.0, 2026-08-09 | own `/asr`, `/detect-language` | sync | no | yes | yes | not an OpenAI path |
| NeMo Speech (Parakeet, Canary) | https://github.com/NVIDIA-NeMo/Speech | Apache-2.0 | v3.0.0, 2026-08-07 | library; `nemo-speech serve` (C++) is OpenAI-shaped | sync | websocket realtime (C++ server) | yes | Parakeet v2/v3 and canary-1b-flash list Volta; canary-1b-v2 does not | accuracy option for EN only |
| Qwen3-ASR + ForcedAligner | https://github.com/QwenLM/Qwen3-ASR | Apache-2.0 | models `Qwen/Qwen3-ASR-{1.7B,0.6B}`, `Qwen/Qwen3-ForcedAligner-0.6B` | vLLM/SGLang OpenAI transcription | sync | yes | yes | unofficial (fp16 + `--enforce-eager`, third-party) | best open EN WER that also covers JA; 3090-first |
| Voxtral Mini/Small | https://github.com/vllm-project/vllm | Apache-2.0 (model) | vLLM v0.34.0, 2026-11-16 | vLLM OpenAI transcription | sync | vLLM realtime (4B-Realtime) | yes | official vLLM dropped CC<8.0 (v0.9+) | no JA, no timestamps |
| Granite Speech 4.1-2b-plus | https://huggingface.co/ibm-granite/granite-speech-4.1-2b-plus | Apache-2.0 | v2610 update | vLLM / llama.cpp OpenAI | sync | no | yes | unknown `(verify)` | speaker-attributed ASR + word timestamps in one model |
| Moonshine | https://github.com/moonshine-ai/moonshine | MIT (legacy non-EN non-commercial) | streaming models current | SDK/CLI, no server | sync | yes (library) | yes (ORT) | yes (ORT) | needs a server written |
| Distil-Whisper | https://github.com/huggingface/distil-whisper | MIT | v3.5 | weights only | n/a | n/a | yes | yes | English speed, no server |
| Kyutai STT | https://github.com/kyutai-labs/delayed-streams-modeling | CC-BY-4.0 | en_fr 1B / en 2.6B | websocket only | sync stream | yes | yes (torch cu126) | yes | EN/FR only, no HTTP surface |
| WhisperLiveKit | https://github.com/QuentinFuxa/WhisperLiveKit | Apache-2.0 | current | OpenAI transcription + Deepgram websocket | sync + stream | yes | yes | yes (faster-whisper backend) | best streaming front-end; Sortformer diarization |
| sherpa-onnx servers | https://github.com/k2-fsa/sherpa-onnx | Apache-2.0 | active | websocket binary protocol, no HTTP | stream + offline | yes | yes (onnxruntime-gpu) | yes (onnxruntime-gpu) | diarization + Japanese zipformer; needs a front-end |
| Wyoming faster-whisper | https://github.com/rhasspy/wyoming-faster-whisper | MIT | 3.9.1 | JSONL over TCP | stream | yes | yes | yes (CT2) | voice-assistant protocol, not HTTP |
| FunASR / SenseVoice | https://github.com/modelscope/FunASR | Apache-2.0 (checkpoints vary) | `funasr-server` ships | OpenAI (`/v1/audio/transcriptions`) | sync | no | yes | yes | JA-capable and CPU-friendly; zh-first |
| ScribeCrate (docker-whisper) | https://github.com/hwdsl2/docker-whisper | MIT | active 2026 | OpenAI transcription + translations | sync + SSE | yes | yes (CC 6.0+) | yes | containerised OpenAI front over whisper/faster-whisper |

Also seen, not scored: `PhilHem/stt-server` (Go over sherpa-onnx, OpenAI
`/v1/audio/transcriptions`, `--max-file-size` 100 MB, `--max-audio-duration`
600 s default) and `IodeSystems/oidio` (Go, CPU-only, OpenAI shape incl.
`/v1/realtime`). Neither publishes a licence line in the README I read, so treat
both as `(verify)` before use.

Collection of facts per candidate is in §3 (primary), §4 (hardware) and §7
(licence).

### Diarization and alignment: same stanza or second stanza

**Word/character timing belongs in the transcription stanza. Speaker
diarization belongs in a second stanza.**

- `whisper-server` already emits `words[]` (`word`, `start`, `end`, `t_dtw`,
  `probability`) inside `verbose_json` and can compute token-level timing with
  `-dtw`, no second process, no extra VRAM.
- Its `--diarize` flag is **stereo channel energy only** (it sums `pcmf32s[0]`
  against `pcmf32s[1]` per segment and only runs when the audio has two
  channels), and `--tinydiarize` needs a `-tdrz` model and only marks speaker
  *turns*. Neither diarizes a mono mix.
- Real diarization is a different model with a different VRAM budget (pyannote
  4.0 peaks above 9.5 GB), so it must not be co-resident with transcription on
  a shared card, and it must be reachable at a different endpoint
  (`POST /v1/audio/diarization` on speaches, or a Sortformer process).

So: one transcription stanza per model, optionally one diarization stanza, and
if a client needs speaker-attributed words, join the two responses (or use a
single model that emits both, e.g. Granite Speech 4.1-2b-plus's
`[Speaker N]: [T:N]` output).

## 3. API surface of the recommended backend (`whisper-server`)

Source: [`examples/server/server.cpp`](https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/examples/server/server.cpp)
and [`examples/server/README.md`](https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/examples/server/README.md)
(read 2026-11-20). Defaults: `request_path = ""`, `inference_path =
"/inference"`, port 8080.

Routes are built as `request_path + suffix`, so `--request-path /v1/audio
--inference-path /transcriptions` yields exactly the OpenAI path table below.

| Route | Purpose | Notes |
| --- | --- | --- |
| `POST /v1/audio/transcriptions` (`request_path + inference_path`) | transcribe an upload | multipart; `file` required |
| `OPTIONS /v1/audio/transcriptions` | CORS preflight | |
| `POST /v1/audio/load` (`request_path + "/load"`) | reload a different model file | `-F model=<path>`; re-inits the context |
| `GET /v1/audio/health` (`request_path + "/health"`) | readiness | `{"status":"ok"}` 200 when ready, `{"status":"loading model"}` 503 while loading |
| `GET /v1/audio/` (`request_path + "/"`) | static files from `--public` | |

There is **no** `/v1/models`, no unload/pause endpoint, no metrics endpoint and
no SSE/websocket route. Unload is the router killing the process (that is the
intended lifecycle: one process per model, `vram_mb` released on kill).

### Request (multipart only)

`file` (required), plus any of: `offset_t`, `offset_n`, `duration`,
`max_context`, `max_len`, `best_of`, `beam_size`, `audio_ctx`, `word_thold`,
`entropy_thold`, `logprob_thold`, `no_speech_thold`, `debug_mode`, `translate`,
`diarize`, `tinydiarize`, `split_on_word`, `no_timestamps`, `token_timestamps`,
`language`, `detect_language`, `prompt`, `carry_initial_prompt`,
`response_format`, `temperature`, `temperature_inc`, `suppress_nst`,
`vad`, `vad_threshold`, `vad_min_speech_duration_ms`,
`vad_min_silence_duration_ms`, `vad_max_speech_duration_s`, `vad_speech_pad_ms`,
`vad_samples_overlap`, `no_language_probabilities`.

- A missing `file` is `400 {"error":"no 'file' field in the request"}`.
- Unknown fields are ignored, which is what makes an OpenAI-shaped client
  safe here: it will send `model`, and that is not read.
- **`model` is not read.** The process serves exactly the file it was started
  with. This is the single most important fact for the router (§5).
- Booleans are `true|1|yes|y` (`parse_str_to_bool`).
- Language defaults to `en`, so a Japanese upload without `language=ja`
  transcribes as English. Start the stanza with `--language auto` (or pass
  `language` per request).

### Response

| `response_format` | Body | Content-Type |
| --- | --- | --- |
| `json` (default) | `{"text": "..."}` (+ `language` if `detect_language`) | `application/json` |
| `text` | segment text concatenated | `text/html; charset=utf-8` (not `text/plain`) |
| `srt` | numbered cues | `application/x-subrip` |
| `vtt` | `WEBVTT` block | `text/vtt` |
| `verbose_json` | `task`, `language`, `duration`, `text`, `segments[]` | `application/json` |

`verbose_json` segments carry `id`, `text`, optional `start`/`end`,
`tokens[]`, `words[]`, `temperature`, `avg_logprob`, `no_speech_prob`, and a
`speaker` field when stereo diarization is on. `words[]` entries have `word`,
`probability` and, when timestamps and token timestamps are on, `start`, `end`,
`t_dtw`. `compression_ratio` is not implemented (the source leaves a TODO).
It is close to OpenAI's `verbose_json` but not identical, a client that reads
`words[]` is fine; one that requires OpenAI's exact extra fields is not.

### Sizes, sync/async, streaming, errors

- **Sync only.** The handler runs the whole file and answers at the end; the
  client connection is held for the duration. There is no job id and no
  progress channel (the callbacks print to the process's own stdout).
- **One request at a time.** The inference path takes a mutex, so concurrent
  uploads queue inside the process; `-p/--processors N` parallelises the
  *audio* of a single request (it is not a request slot count). The router
  should therefore book `slots: 1`.
- **No size or duration cap in the server.** `read_timeout`/`write_timeout` are
  600 s; the bundled HTTP library's payload cap is `size_t` max, and the whole
  upload is held in memory (`req.get_file_value("file")`). The audio is
  processed in Whisper's 30 s windows internally; there is no chunking API.
  Practical limits are RAM and the client's own timeout.
- **Non-WAV/MP3/FLAC input needs `--convert`** (or a `WHISPER_COMMON_FFMPEG`
  build), which shells out to `ffmpeg` and requires it on the host; the
  project's own README carries a warning about accepting uploads and running
  ffmpeg sandboxed.
- **Errors** are JSON (`{"error":"..."}`) with 400 for a missing file / bad
  `model` on `/load`, 499 when the client disconnects mid-inference, 500 on
  decode failure, and the default `404 File Not Found (<path>)` text for an
  unrouted path (this is what an OpenAI client sees if the server was started
  without `--request-path`).

### Word timing and VAD flags

`-dtw MODEL` (token-level timestamps via DTW aheads presets), `-ml N`
(`max-len`; `-ml 1` gives one word per cue), `--split-on-word`,
`-wt N` (`word-thold`, default 0.01). VAD is `--vad` plus a VAD model:
`--vad-model models/ggml-silero-v6.2.0.bin` (download with
`./models/download-vad-model.sh silero-v6.2.0`), with `--vad-threshold`,
`--vad-min-speech-duration-ms`, `--vad-min-silence-duration-ms`,
`--vad-max-speech-duration-s`, `--vad-speech-pad-ms`, `--vad-samples-overlap`.
VAD is the standard mitigation for Whisper's silence/music hallucination. No
VAD model path is baked in, so the stanza must name one.

## 4. Hardware fit

### whisper.cpp (primary)

| Weight | Disk | Resident VRAM (estimate) | Cards | Cold start |
| --- | --- | --- | --- | --- |
| `ggml-large-v3-turbo-q5_0.bin` | 547 MiB | ~1.5 GB | all three | a few seconds `(verify)` |
| `ggml-large-v3-turbo.bin` | 1.5 GiB | ~2.5 GB | all three | a few seconds `(verify)` |
| `ggml-large-v3-q5_0.bin` | 1.1 GiB | ~2.5 GB | all three | ~5 s `(verify)` |
| `ggml-large-v3.bin` | 2.9 GiB | ~3.9 GB (working-memory column of the README's table) | all three | ~5-10 s `(verify)` |
| `ggml-small.bin` | 466 MiB | ~0.9 GB | all three | ~2 s `(verify)` |

Disk sizes are from
[`models/README.md`](https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/models/README.md);
the Mem column of the README's usage table (`tiny 273 MB` … `large 3.9 GB`) is
model working memory, not a card measurement. VRAM figures here are estimates
and must be replaced by a measured `nvidia-smi` value (`-check` does not measure
them; §8 has the command).

- **Quantisation:** `q5_0` files are published for `large-v2`, `large-v3` and
  `large-v3-turbo`; `q8_0` is a supported quant type but has no prebuilt file,
  so it has to be generated with `whisper-quantize`. Quantised ggml is
  compute-heavy on the encoder; if the card has room, fp16 (the plain `.bin`)
  is the safer default.
- **sm_70:** the CUDA arch list in `ggml/src/ggml-cuda/CMakeLists.txt` appends
  `70-virtual` for CUDA < 13 builds, and `fattn.cu` has explicit Volta paths
  (`cc == GGML_CUDA_CC_VOLTA`) with no sm_70 exclusion in
  `ggml_cuda_flash_attn_ext_supported`. The v1.9.5 changelog includes Volta
  flash-attention fixes. There is no bf16 requirement anywhere. If the sm_70
  build misbehaves, `--no-flash-attn` is the first thing to try `(verify)`.
- **CPU-only:** `--no-gpu` (alias `-ng`) runs on the Xeons; `vram_mb: 0` makes
  the router treat it as a CPU process (never evicted to make room). Throughput
  is not published by the project, the README has no numbers, only a
  `whisper-bench` pointer, so a CPU figure has to be measured on the host
  `(verify)`. Note the host's CPUs are already assigned to the large chat
  model, so a CPU STT stanza competes for cores, not just latency.
- **Co-residency:** on the 24 GB card the resident set (intent + embedding +
  reranker) is ~6.6 GB, so a 1.5-2.5 GB transcription process fits beside it
  with ~15 GB to spare. On the 32 GB cards the large chat model takes both
  cards (`device: "1,2"`, `vram_mb: 64000`), so an STT stanza there means
  evicting it, and a reload of that model is minutes, not seconds. Practical
  placement: **the 24 GB card**, where the STT process competes only with the
  image stanzas' 300 s TTL.

### speaches (fallback)

- Weights: CTranslate2 whisper `int8_float16` or `float16` for `large-v3`.
  CTranslate2 ships Linux wheels compiled for
  `53-real;60-real;61-real;70-real;75-real;80-real;86;100-real;120-real`, so
  sm_70 and sm_86 are both covered; `bfloat16` needs sm_80+, so on the V100 use
  `WHISPER__COMPUTE_TYPE=int8_float16`. No published VRAM figure for speaches
  itself `(verify)`; a `large-v3` CT2 model is in the low single-digit GB.
- The process is heavier than the model: it installs `torch` and
  `pyannote-audio>=4.0.3` for the diarization endpoint, and the CUDA image is
  based on a CUDA 12.6 base image.
- **Diarization is the VRAM problem.** `pyannote/speaker-diarization-community-1`
  under pyannote.audio 4.0.3 peaks at >9.5 GB (a maintainer-labelled wontfix),
  versus ~1.6 GB for the 3.3.2/3.1 combination. That is fine on the 24 GB card
  alone and impossible beside the large chat model. It also needs a gated
  Hugging Face token (`HF_TOKEN`).
- `speaches` publishes no throughput or accuracy numbers for any model.

### Qwen3-ASR, if the accuracy ceiling matters (24 GB card only)

Open weights exist (`Qwen/Qwen3-ASR-1.7B`, `-0.6B`) under Apache-2.0, with a
separate `Qwen/Qwen3-ForcedAligner-0.6B` (~0.92B) for word/character timestamps
(its own limit is 5 minutes of speech per call; the ASR model's native limit is
20 minutes). English LibriSpeech clean/other WER is 1.63/3.38 for 1.7B, the
best open number found. It is served by vLLM or SGLang; the vLLM path needs
CUDA wheels that no longer build for sm_70 (official vLLM dropped compute
capability < 8.0 at v0.9), so this is a **24 GB card** option, with a
third-party report of it working on V100 only by pinning fp16 plus
`--enforce-eager`. Its open-weight accuracy for Japanese specifically is not
published by the model authors `(verify)`.

## 5. How the router would run it

### Spawn

`command` is a foreground process with `{port}` and `{gpu}`; `env` carries
`CUDA_VISIBLE_DEVICES={gpu}` (and `CUDA_DEVICE_ORDER=PCI_BUS_ID`) whenever
`device` is a choice of cards. The important detail is the two path flags:

```
/opt/ai/bin/whisper-server \
  --model /opt/ai/models/ggml-large-v3-turbo-q5_0.bin \
  --vad-model /opt/ai/models/ggml-silero-v6.2.0.bin \
  --host 127.0.0.1 --port {port} \
  --request-path /v1/audio --inference-path /transcriptions \
  --language auto --convert --vad --flash-attn --threads 8
```

### Stanza fields

| Field | Value | Why |
| --- | --- | --- |
| `health_check` | `/v1/audio/health` | the health route is `request_path + "/health"`, so adding a request path moves it; it answers `503 {"status":"loading model"}` until the weights are in, which is exactly the router's spin-up contract |
| `spin_up_seconds` | `60` | comfortable for a 0.5-3 GiB ggml load plus CUDA init; the router also restarts the deadline on a changing `progress` value |
| `vram_mb` | measured `nvidia-smi` used, rounded up (~2000 for turbo q5_0 on the 24 GB card) | the router reserves this against the card before spawn |
| `idle_ttl_seconds` | `300` | matches the image stanzas; `0` only if the model should never be swept |
| `slots` | `1` | the process serialises inference; it has no `--parallel`/`-np`, so the router's derivation would give 1 anyway |
| `match` | `{body_field: model}` | OpenAI clients send `model` in the multipart body and the matcher reads it; the backend ignores it. With a single STT stanza, `{path_prefix: /v1/audio, path_default: true}` also works |
| `api_type` | `transcription` | UI metadata only; it selects the Playground tab |

### What needs rewriting: nothing, if you pass the flags

The router proxies `/v1/audio/transcriptions` byte-for-byte and does not rewrite
the path, the multipart body or any header. That is fine here **only because
the server is started at that path**. Two consequences are worth stating
plainly:

1. **Without `--request-path`/`--inference-path`, transcription breaks** while
   still resolving: the matcher picks the stanza from the `model` field, the
   proxy forwards `/v1/audio/transcriptions`, and the backend answers its own
   `404 File Not Found (/v1/audio/transcriptions)`. The router cannot fix this
   with config, because its route table only proxies `/v1/*`, `/v/*`,
   `/sdapi/v1/`, `/rerank`, `/reranking`, `/infill`, `/completion`, `/props`,
   `/slots`, `/tokenize`, `/detokenize`, `/apply-template`, a bare `/inference`
   never reaches a stanza.
2. **The router requires the multipart `model` value to resolve to one of its
   own ids** (`resolveRef`: an id containing `/` is read as `peer/model`, and an
   unknown qualified id fails with `no model id could be identified`). For the
   primary this costs nothing (the backend ignores `model`). For the fallback it
   is the main constraint: an OpenAI client sends the Hugging Face id
   (`Systran/faster-whisper-large-v3`), which contains a slash and is not a
   router id, so the request `404`s at the router before it is proxied.

Three router changes are therefore *candidates*, none required for the primary:

- **Bound or stream the matcher's body read.** `readBody` is `io.ReadAll`, so a
  long upload is fully buffered in the router's RAM to find the `model` field,
  on top of the backend's own copy. Reading only the first multipart part (or
  accepting `model` from the query) removes that. This is the one change worth
  doing irrespective of backend.
- **Multipart `model` rewrite.** `rewriteBodyModel` and the JSON filters are
  JSON/query only; form and multipart bodies pass through untouched. A rewrite
  would let the fallback serve any Hugging Face id, and would let a `peer/model`
  request keep working when ids differ per node.
- **A translations stanza helper.** `whisper-server` offers translation only as
  a `translate=true` field on the transcription path; OpenAI's separate
  `/v1/audio/translations` route would need a second process (same binary,
  `--request-path /v1/audio/translations`) with its own `model_id`, or the
  fallback, or a small shim. Nothing else in the transcription path needs one.

### Long files

The client connection is held for the whole file, and the router has no job
API for slow work (deliberately out of scope in `SPEC.md`). With `slots: 1` a
second upload queues inside the router, not inside the backend. The idle sweep
skips in-flight work, so a two-hour transcription cannot be swept mid-request,
but it does hold the card for its duration, and an eviction waiting on that
card waits too.

## 6. Proposed stanza

Paths and model files are placeholders. Ports are above the allocations in the
staged node configs; check `reserved_ports` before deploying.

### 24 GB card (sm_86)

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
    port: 5930
```

### 32 GB cards (sm_70), if one is free

Same stanza with `device: "1|2"` (which is why `env` must carry `{gpu}`), a
bigger weight, and no `--flash-attn` if Volta flash-attention misbehaves:

```yaml
  - model_id: whisper-large-v3
    name: "Whisper large-v3 (ggml Q5_0, STT)"
    aliases: [whisper-1-large, stt-large]
    command: >
      /opt/ai/bin/whisper-server
      --model /opt/ai/models/ggml-large-v3-q5_0.bin
      --vad-model /opt/ai/models/ggml-silero-v6.2.0.bin
      --host 127.0.0.1 --port {port}
      --request-path /v1/audio --inference-path /transcriptions
      --language auto --convert --vad --threads 8
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 2600
    device: "1|2"
    spin_up_seconds: 60
    api_type: transcription
    match: {body_field: model}
    health_check: /v1/audio/health
    idle_ttl_seconds: 300
    slots: 1
    port: 5931
```

Note both stanzas cannot share `aliases: [whisper-1]`: an alias resolves to one
stanza, and a client that wants the large model must send its own id.

### Fallback (speaches), only with the slash caveat in mind

```yaml
  - model_id: whisper-1
    name: "Speaker-aware STT service (CTranslate2 whisper, CUDA)"
    command: >
      /opt/ai/venv-speaches/bin/uvicorn --factory speaches.main:create_app
      --host 127.0.0.1 --port {port}
    env:
      - CUDA_DEVICE_ORDER=PCI_BUS_ID
      - CUDA_VISIBLE_DEVICES={gpu}
      - WHISPER__COMPUTE_TYPE=int8_float16
      - STT_MODEL_TTL=-1
      - HF_TOKEN=<read token, needed by the diarization endpoint>
    vram_mb: 4000
    device: 0
    spin_up_seconds: 120
    api_type: transcription
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 300
    slots: 1
    port: 5932
```

`model_id: whisper-1` is deliberate: it is slash-free (so the router resolves
it) and it is an alias the server itself maps to
`Systran/faster-whisper-large-v3`. Any other served model needs a slash-free id,
which means either a multipart `model` rewrite in the router or a shim in front.
The diarization endpoint lives under the same `/v1/audio` tree
(`POST /v1/audio/diarization`), so one stanza covers it once the token is set;
it is not a second stanza, but it is a second model inside the same process and
carries the >9.5 GB peak from §4.

## 7. Licence and risk flags

- `whisper-server` and `whisper.cpp`: **MIT**. Whisper weights: MIT. The
  VAD model is Silero, also MIT. The `--convert` path shells out to `ffmpeg` on
  user-supplied uploads, the upstream README itself warns about running the
  server with privileges; keep it on `127.0.0.1` and let the router own the
  socket. Uploads are held in memory, so a hostile client can grow the
  process's RSS; the router's own `readBody` does the same on its side.
- Transcription accuracy risk: no numeric WER is published by whisper.cpp for
  any model, and no numeric Japanese figure exists for `large-v3`/`turbo`, the
  only published numbers are the 2022 paper's `large`/`large-v2` figures
  (LibriSpeech clean 2.7 WER; Japanese is scored as character error rate, 9.4
  for `large` and 9.1 for `large-v2` on one FLEURS split, 6.4/5.3 on another)
  and a "10-20% fewer errors than large-v2" claim for large-v3. Treat the JA
  number as unverified until measured on the host's own audio `(verify)`.
- Whisper hallucinates on silence and music by design of its training data
  (documented by the model authors), which is why the stanza runs VAD. This
  matters for a captioning pipeline: gibberish on a music-only stretch is a
  content bug, not a cosmetic one.
- `speaches`: **MIT** code. Its diarization weights
  (`pyannote/speaker-diarization-community-1`) are **CC-BY-4.0 and gated** (a
  token and a personal-information form on Hugging Face), with a paid hosted
  tier from the same authors. `pyannote/speaker-diarization-3.1` is gated with
  MIT weights. NeMo's `diar_sortformer_4spk-v1` is
  **CC-BY-NC-4.0 (non-commercial)**; the streaming `...v2` is CC-BY-4.0.
  `torchaudio`'s `MMS_FA` aligner and the default `ctc-forced-aligner` model
  are **CC-BY-NC-4.0**; the `ctc-forced-aligner` code is BSD-2. Silero VAD is
  MIT (the PyPI metadata and README badge still say CC-BY-NC-4.0, and the
  maintainer has stated MIT in discussion, worth a look if it matters).
  Internal use only anyway, but these are the terms to avoid tripping over if a
  caption ever leaves the LAN.
- Model licences in the table: whisper MIT, distil-whisper MIT, Moonshine MIT
  (legacy non-English non-streaming checkpoints are non-commercial), Voxtral and
  Granite Apache-2.0, Qwen3-ASR + ForcedAligner Apache-2.0, Parakeet/Canary
  CC-BY-4.0 (attribution), Kyutai CC-BY-4.0.
- The 32 GB cards have no bf16. Do not plan any backend there that hard-requires
  bf16 or FlashAttention-2 (that rules out recent vLLM builds and vLLM-Omni,
  which needs cc 7.5+).

## 8. Verification checklist (read-only) and open questions

Run as the service user on the GPU host. Everything here is read-only; the
transcription test writes one temp WAV and one temp output file.

```bash
# 1. Which cards, and what is free right now.
nvidia-smi -L
nvidia-smi --query-gpu=index,name,compute_cap,memory.total,memory.used,memory.free \
  --format=csv

# 2. Whisper binary and CUDA build (expect 'CUDA' and no sm_75 floor).
/opt/ai/bin/whisper-server -h | head -60
ls -l /opt/ai/bin/whisper-server

# 3. The two weights and the VAD model exist and are the expected sizes.
ls -l /opt/ai/models/ggml-large-v3-turbo-q5_0.bin \
      /opt/ai/models/ggml-large-v3-q5_0.bin \
      /opt/ai/models/ggml-silero-v6.2.0.bin

# 4. Does the request-path trick really move the health and inference routes?
#    (Start by hand on a spare port and a free card, then stop it.)
CUDA_VISIBLE_DEVICES=0 /opt/ai/bin/whisper-server \
  --model /opt/ai/models/ggml-large-v3-turbo-q5_0.bin \
  --vad-model /opt/ai/models/ggml-silero-v6.2.0.bin \
  --host 127.0.0.1 --port 5960 \
  --request-path /v1/audio --inference-path /transcriptions \
  --language auto --convert --vad &
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5960/v1/audio/health   # expect 200
curl -s http://127.0.0.1:5960/health                                             # expect 404

# 5. English and Japanese, word timestamps, formats, and the model field being ignored.
ffmpeg -f lavfi -i 'flite=text=hello world:voice=slt' -ar 16000 -ac 1 /tmp/en.wav
curl -s http://127.0.0.1:5960/v1/audio/transcriptions \
  -F file=@/tmp/en.wav -F model=whatever -F response_format=verbose_json \
  -F timestamp_granularities[]=word | head -c 800
# repeat with real JA audio, -F language=ja, and check words[].start/end look sane

# 6. Peak VRAM for the stanza's vram_mb (round up).
nvidia-smi --query-gpu=memory.used --format=csv -l 1 &
curl -s http://127.0.0.1:5960/v1/audio/transcriptions -F file=@/tmp/en.wav >/dev/null
# kill the sampler, read the peak, set vram_mb to it rounded up

# 7. Cold start seconds (the value for spin_up_seconds).
#    Time from process start to the first 200 on /v1/audio/health.

# 8. V100 (sm_70) sanity: load and transcribe on card 1 with --no-flash-attn first,
#    then again with --flash-attn, and compare speed and output.

# 9. Router-side: the stanza resolves and the model field is read from multipart.
curl -s http://127.0.0.1:18080/v1/models | grep -o whisper-large-v3-turbo
curl -s http://127.0.0.1:18080/v1/audio/transcriptions \
  -F file=@/tmp/en.wav -F model=whisper-large-v3-turbo -F response_format=json
```

Open questions for the operator:

1. Which card should own transcription permanently, the 24 GB card (fits
   today, competes with image jobs on TTL) or a 32 GB card (needs the large chat
   model evicted, and its reload is minutes)?
2. Is English-plus-Japanese a hard requirement for one weight, or is it
   acceptable to run `whisper-large-v3` for Japanese and a smaller/faster model
   for English? That decides whether one stanza or two.
3. Does anything actually consume `/v1/audio/translations`? If not, the
   whisper.cpp route needs no second stanza and the fallback's main advantage
   disappears.
4. Is speaker diarization wanted at all (the sub-titling use case does not need
   it, the existing captioning stack carries it)? If it is, decide between a
   gated pyannote endpoint and a CC-BY-4.0 Sortformer, and give it its own VRAM
   budget.
5. Should the router grow the multipart `model` rewrite, or is "one backend,
   id-agnostic" the rule? The primary does not need it; the fallback does.

## 9. Sources

All opened 2026-11-20.

whisper.cpp / `whisper-server`

- https://github.com/ggml-org/whisper.cpp (repo, licence badge)
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/README.md
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/LICENSE
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/examples/server/README.md
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/examples/server/server.cpp
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/examples/server/httplib.h
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/models/README.md
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/models/download-vad-model.sh
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/ggml/src/ggml-cuda/CMakeLists.txt
- https://raw.githubusercontent.com/ggml-org/whisper.cpp/master/ggml/src/ggml-cuda/fattn.cu
- https://github.com/ggml-org/whisper.cpp/releases.atom (v1.9.5, 2026-10-07)
- https://github.com/ggml-org/whisper.cpp/commits/master.atom (2026-11-20)
- https://github.com/openai/whisper (weights, hallucination note)
- https://raw.githubusercontent.com/openai/whisper/main/model-card.md
- https://ar5iv.labs.arxiv.org/html/2212.04356 (WER/CER tables)

speaches

- https://github.com/speaches-ai/speaches
- https://raw.githubusercontent.com/speaches-ai/speaches/master/README.md
- https://raw.githubusercontent.com/speaches-ai/speaches/master/docs/openapi.json
- https://raw.githubusercontent.com/speaches-ai/speaches/master/src/speaches/config.py
- https://raw.githubusercontent.com/speaches-ai/speaches/master/src/speaches/routers/stt.py
- https://raw.githubusercontent.com/speaches-ai/speaches/master/src/speaches/routers/diarization.py
- https://raw.githubusercontent.com/speaches-ai/speaches/master/src/speaches/executors/pyannote_diarization.py
- https://raw.githubusercontent.com/speaches-ai/speaches/master/docs/usage/realtime-api.md
- https://raw.githubusercontent.com/speaches-ai/speaches/master/Dockerfile
- https://raw.githubusercontent.com/OpenNMT/CTranslate2/master/python/tools/prepare_build_environment_linux.sh
- https://opennmt.net/CTranslate2/hardware_support.html

NeMo, Parakeet, Canary

- https://github.com/NVIDIA-NeMo/Speech
- https://raw.githubusercontent.com/NVIDIA/NeMo-Speech.cpp/main/docs/server.md
- https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2
- https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3
- https://huggingface.co/nvidia/parakeet-tdt-1.1b
- https://huggingface.co/nvidia/parakeet-tdt_ctc-0.6b-ja
- https://huggingface.co/nvidia/canary-1b-v2
- https://huggingface.co/nvidia/canary-1b-flash
- https://huggingface.co/nvidia/canary-qwen-2.5b
- https://docs.nvidia.com/nim/speech/latest/reference/support-matrix/asr.html
- https://github.com/Alex-Wengg/vllm-parakeet-plugin
- https://huggingface.co/nvidia/diar_streaming_sortformer_4spk-v2
- https://huggingface.co/nvidia/diar_sortformer_4spk-v1

Qwen3-ASR

- https://github.com/QwenLM/Qwen3-ASR
- https://huggingface.co/Qwen/Qwen3-ASR-1.7B
- https://huggingface.co/Qwen/Qwen3-ForcedAligner-0.6B
- https://docs.vllm.ai/projects/recipes/en/latest/Qwen/Qwen3-ASR.html
- https://github.com/sgl-project/sglang-omni/blob/main/docs/cookbook/qwen3_asr.md
- https://huggingface.co/zai-org/GLM-ASR-Nano-2512
- https://github.com/QwenLM/Qwen3-ASR-Toolkit

Voxtral, Granite, Kyutai, other LLM-style ASR

- https://huggingface.co/mistralai/Voxtral-Mini-3B-2507
- https://huggingface.co/mistralai/Voxtral-Small-24B-2507
- https://docs.vllm.ai/en/latest/models/supported_models
- https://docs.vllm.ai/en/latest/serving/online_serving/speech_to_text
- https://github.com/vllm-project/vllm/releases/tag/v0.34.0
- https://raw.githubusercontent.com/vllm-project/production-stack/main/tutorials/25-v100-legacy-gpu-deployment.md
- https://github.com/ggml-org/llama.cpp/pull/14862 (Voxtral in mtmd)
- https://github.com/ggml-org/llama.cpp/pull/21863 (server OpenAI transcriptions)
- https://github.com/ggml-org/llama.cpp/issues/25496 (Voxtral WER)
- https://huggingface.co/ibm-granite/granite-speech-4.1-2b-plus
- https://huggingface.co/ibm-granite/granite-speech-5.0-2b
- https://huggingface.co/kyutai/stt-1b-en_fr
- https://github.com/kyutai-labs/delayed-streams-modeling

Small stacks

- https://github.com/moonshine-ai/moonshine
- https://raw.githubusercontent.com/moonshine-ai/moonshine/main/docs/models/accuracy.md
- https://github.com/huggingface/distil-whisper
- https://github.com/QuentinFuxa/WhisperLiveKit
- https://github.com/k2-fsa/sherpa-onnx
- https://github.com/rhasspy/wyoming-faster-whisper
- https://github.com/rhasspy/wyoming
- https://github.com/modelscope/FunASR
- https://arxiv.org/html/2407.04051v3 (SenseVoice, Japanese CER)
- https://github.com/ahmetoner/whisper-asr-webservice
- https://github.com/hwdsl2/docker-whisper
- https://github.com/PhilHem/stt-server
- https://github.com/IodeSystems/oidio

Diarization, VAD, alignment, accuracy references

- https://github.com/pyannote/pyannote-audio
- https://huggingface.co/pyannote/speaker-diarization-community-1
- https://huggingface.co/pyannote/speaker-diarization-3.1
- https://github.com/pyannote/pyannote-audio/issues/1963 (VRAM spike)
- https://github.com/modelscope/3D-Speaker
- https://github.com/narcotic-sh/senko
- https://github.com/juanmc2005/diart
- https://github.com/wenet-e2e/wespeaker
- https://github.com/snakers4/silero-vad
- https://github.com/TEN-framework/ten-vad
- https://huggingface.co/funasr/fsmn-vad
- https://github.com/linto-ai/whisper-timestamped
- https://github.com/MahmoudAshraf97/ctc-forced-aligner
- https://pytorch.org/audio/main/generated/torchaudio.pipelines.MMS_FA.html
- https://huggingface.co/spaces/hf-audio/open_asr_leaderboard (English WER table, 2026-11-18 revision)
- https://raw.githubusercontent.com/kotoba-tech/kotoba-whisper/main/README.md (Japanese CER table)

This repo

- [`SPEC.md`](../../SPEC.md), [`README.md`](../../README.md), [`PLAN.md`](../../PLAN.md)
- [`docs/research/oss-model-router-landscape.md`](oss-model-router-landscape.md)
- `internal/router/server.go`, `internal/router/matcher.go`,
  `internal/router/filters.go`, `internal/router/router.go`,
  `cmd/fake-model/main.go`

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Main README](../../README.md): the router features this research feeds.

## Linked from

<!-- backlinks:start -->
- [Local realtime and speech-to-speech backends](realtime-backends.md)
- [Local text-to-speech backends](tts-backends.md)
- [model-router docs](../README.md)
- [Use the mic and camera in the Playground](../playground/mic-and-camera.md)
<!-- backlinks:end -->
