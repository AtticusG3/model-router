# Local music and sound-effect generation backends

Date of reading: 2026-11-20. Part of #64, issue #87. Keeps the job API shape
from #83 and the async job layer from #69.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link. A fact that could not be confirmed from the
project's own pages is marked `(verify)`, and anything only the GPU host can
settle is in the checklist (§8).

## 0. Scope and method

The question: which backend should this router start, health-check, place,
evict and proxy for music and sound-effect generation (prompt or lyrics to
audio, up to minutes long, with optional reference audio), and what single
front door clients should use. The cards are the GPU host's one 24 GB sm_86
card (GPU 0) and two 32 GB sm_70 cards (GPU 1 and 2).

**How the facts were gathered.** Page fetches were not available in this run
(web search only, 39 queries). Every external fact comes from search-engine
extracts of the cited pages, read on 2026-11-20, not from opening the files.
Field names, flags and defaults that matter for the stanza are marked `(verify)`
and repeated in the checklist. Speed figures are vendor or third-party; each
says which.

**Related work, linked not redone.**

- The sibling content project's E5 (music scoring and generation, its issue 24)
  and E6 (sound effects, ambience and the final mix, its issue 25) cover *what*
  to generate and how good it must be. Neither had landed as of this reading
  `(verify)`; this run cannot reach the issue tracker. Link them here when they
  land. This note is only about *serving*.
- [`video-backends.md`](video-backends.md) (#83) chose the `/v1/videos` job
  shape and the "adapter in the stanza `command`" pattern. This note reuses
  both.

**Router facts this note relies on.** Read from this repo, not assumed.

- `api_type: audio_gen` already exists and maps `/v1/audio/generations` (prefix
  match) to the `audio_gen` stanza marked `path_default`, or the only one
  (`pathAPITypes`, `internal/router/matcher.go`; `SPEC.md` api_type table).
- `cmd/fake-model` already serves an audio job API with the same shape as
  video: `POST /v1/audio/generations`, `GET /v1/audio/generations/{id}`,
  `GET /v1/audio/generations/{id}/content` (`audio/mpeg`) and
  `DELETE /v1/audio/generations/{id}`, with `object: "audio.generation"`.
- **The async job layer (#69) only knows `/v1/videos`.** `internal/router/jobs.go`
  hard-codes that path in four places: `jobID` (follow-up routing),
  `proxyJobAware` (recording a submit), `pollJob` (the 2-second status poll)
  and the Activity row. A `POST /v1/audio/generations` today is proxied like
  any request: nothing records the id, nothing holds the model while the job
  runs, and a model-less follow-up goes to the type default, not to the
  instance that owns the job. §5.3 is the small router change that fixes this.
- Request bodies are capped by `max_request_body_mb` (default 256). Multipart
  bodies are streamed after the `model` field, so a reference-audio or video
  upload is not buffered whole. Media responses (`audio/*`) are flushed per
  chunk and not captured by Activity.
- The router runs `command` without a shell and without a working directory
  (`splitCommand`, `internal/router/proc.go`). Everything a backend needs must
  be a flag or an `env` entry.
- Every load on `"1|2"` evicts the large chat model, which books both V100s
  (`device: "1,2"`, `vram_mb: 64000`, about 7 minutes to reload). This settles
  most of the V100 question before any benchmark (§4.3).

## 1. Recommendation

**Primary: the official ACE-Step 1.5 REST server (`acestep-api`, from
`ace-step/ACE-Step-1.5`), one process, on the 24 GB card (GPU 0, sm_86)
only.** It is the model authors' own server and the only candidate that
already speaks an async job API (`POST /release_task`, `POST /query_result`,
`GET /v1/audio?path=`), so the front-door adapter is a field-by-field
translation with no state of its own. ACE-Step 1.5 does songs with vocals from
lyrics, instrumentals (`[Instrumental]`), 10 s to 600 s, a seed, and cover,
repaint and reference-audio tasks, at 48 kHz stereo. It is fast: the model
card claims a full song in under 10 s on an RTX 3090. Turbo DiT plus the 1.7B
planning LM fits in about 12 GB `(verify)`, so it sits beside the GPU 0
residents without evicting anything. Licence: MIT, code and weights, with
commercial use of outputs allowed per the model card `(verify)` against the
repo `LICENSE`.

**Fallback: `audio.cpp` (`audiocpp_server`, ggml, Apache-2.0), one model per
process, on any card.** It serves ACE-Step 1.5 (turbo, base, XL turbo, XL SFT),
Stable Audio 3 (Small SFX, Small Music, Medium), HeartMuLa, YuE2 and others
from one C++ binary with GGUF weights and no Python. It is the only practical
route onto the V100s (a CUDA 12 build with sm_70; ggml's fp16 path does not
need bf16 or FlashAttention 2) and the only recommended source of **sound
effects and ambience**, which ACE-Step does not do. It needs a stateful
adapter (§5.4), which is why it is the fallback.

**The rule for choosing:**

1. Music (a song, lyrics, an instrumental bed, a cover or repaint) on GPU 0
   runs on `acestep-api`.
2. Everything else runs on `audio.cpp`: sound effects and ambience (Stable
   Audio 3 Small SFX), any model `acestep-api` does not serve (ACE-Step XL on a
   V100, HeartMuLa, Stable Audio 3 Medium), and any placement on GPU 1 or 2.
3. The router never switches between them on its own. A `model_id` belongs to
   one backend; the client picks the model.

### The four questions, answered

1. **Is there a de-facto HTTP API?** No. Four unrelated shapes exist:
   ACE-Step's own async `release_task` / `query_result`; vLLM-Omni's
   `POST /v1/audio/generate` (sync, binary body, `input` and `audio_length`,
   diffusion text-to-audio only); LocalAI's `POST /v1/sound-generation` (sync,
   binary, `model_id` and `text`, modelled on a hosted sound-effects API); and
   `audio.cpp`'s generic `POST /v1/tasks/run`. None covers lyrics, reference
   audio and long jobs together, and the PyTorch models (YuE2, HeartMuLa,
   SongGeneration, DiffRhythm 2, MMAudio) ship scripts, not servers. So the
   router owns the front door: **`POST /v1/audio/generations` as an async job,
   the same lifecycle and status values as `/v1/videos`** (§3.1), with a thin
   adapter per backend in the stanza `command`. The path is already the
   `audio_gen` default route and already in `fake-model`; the router only has
   to treat it as a job collection (§5.3). Field mappings are in §5.4.
2. **Share a stanza or a process each?** One process per model, by default
   (§5.6). The router books one `vram_mb` per stanza and cannot see which model
   a multi-model server has loaded. Two exceptions are allowed because the
   models share one footprint: ACE-Step DiT variants behind one `acestep-api`
   (it holds up to three DiT slots and one LM), and `audio.cpp` with
   `max_loaded_models: 1`. Every PyTorch-only model needs its own process and
   its own small server, since none ships one.
3. **Practical on the V100s?** Technically yes for the ggml ports
   (`audio.cpp`, `acestep.cpp`, `sa3.cpp`) with a CUDA 12 build that includes
   sm_70 `(verify)`. The official ACE-Step server can run there only with its
   own torch 2.10 cu128 venv and the pre-Ampere fp16 fixes, some of which are
   only in forks (§4.4). YuE2 needs bf16: no. In practice music and effects
   models are small enough for GPU 0, and any V100 load evicts the large chat
   model, so V100 use is for an agreed batch window only (§6.3).
4. **Foley (audio from video)?** Same endpoint, `POST /v1/audio/generations`,
   with a `video` file part in a multipart body. The output is audio, so it is
   not a `/v1/videos` job; putting the track back on the picture is the
   client's mix step (E6). No foley backend is proposed yet: MMAudio's weights
   are non-commercial, HunyuanVideo-Foley's licence excludes the EU, UK and
   South Korea, ThinkSound is research-only, and none ships a server (§2, §7).

## 2. Alternatives

`3090` and `V100` say whether the serving runtime runs on sm_86 or sm_70 at
all, and the expected VRAM. Licence is code / weights.

### 2.1 Serving runtimes

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 (sm_86) | V100 (sm_70) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ACE-Step 1.5 `acestep-api` (FastAPI) | https://github.com/ace-step/ACE-Step-1.5 | MIT / MIT `(verify)`; some third parties say Apache-2.0 | v0.1.8 latest `(verify)` date; v0.1.6 (XL) 2026-04-03 | own JSON job API; not OpenAI-compatible | async (submit + batch poll) | no | yes; DiT-only from 4 GB, DiT + LM from 6 GB with offload; book ~12000 for turbo + 1.7B LM `(verify)` | only with its own cu128 venv and fp16 fixes (§4.4); not proposed | **primary**, GPU 0 |
| audio.cpp `audiocpp_server` (ACE-Step 1.5 incl. XL, Stable Audio 3, HeartMuLa, YuE2, MiniMax Music 3 and TTS/STT families) | https://github.com/0xShug0/audio.cpp | Apache-2.0 / per model | release 0.8.x, 2026-09 (third party `(verify)`); repo active in 2026-11 | own: `/v1/tasks/run`, `/v1/tasks/stream`, `/v1/tasks/batch`, unload routes; OpenAI-style `/v1/audio/speech` and `/v1/audio/transcriptions` for speech only | sync `(verify)` | `/v1/tasks/stream` `(verify)` shape | yes; GGUF 16/Q8 | yes with a CUDA 12.x build and `--cuda-arch 70` (CUDA 13 dropped Volta; release notes say the CUDA 13 package does not cover V100) `(verify)` | **fallback**, any card |
| acestep.cpp `ace-server` | https://github.com/ServeurpersoCom/acestep.cpp | `(verify)` | active; no tagged releases `(verify)` | own: `POST /lm`, `POST /synth`, `POST /understand`, `GET /props`, `GET /health` | sync | no | yes; GGUF Q8 DiT 2.4 GB + LM 4B Q8 4.2 GB | likely, as ggml with sm_70 `(verify)` | skip: ACE-Step only, a two-call API (`/lm` then `/synth`), and `audio.cpp` covers the same model plus effects |
| sa3.cpp `sa3-server` | https://github.com/betweentwomidnights/sa3.cpp | `(verify)` | v0.2.4, 2026-11 `(verify)` day | own: `POST /generate`, `POST /generate/loop`, `GET /poll_status/<id>`, `GET /health` | async (session id + poll; base64 WAV on completion) | no | yes | likely, as ggml with sm_70 `(verify)` | skip: Stable Audio only, init audio by server-local path; `audio.cpp` covers the same models. Listed on the vendor's own Awesome list |
| ComfyUI (native ACE-Step 1.5, ACE-Step XL and Stable Audio 3 nodes) | https://github.com/Comfy-Org/ComfyUI | GPL-3.0 / per model | v0.43.0, 2026-10-27 (per the video note) | own: `POST /prompt`, `/history`, `/view`, `/ws` | async queue | websocket progress | yes | only with a cu126 torch venv `(verify)` | skip: a workflow adapter for models the primary and fallback already serve; cover and repaint were still "coming soon" in its docs `(verify)` |
| vLLM-Omni `POST /v1/audio/generate` (Stable Audio Open 1.0; ACE-Step 1.5 offline only, PR #4010) | https://github.com/vllm-project/vllm-omni | Apache-2.0 / per model | 0.32.0 `(verify)` | OpenAI-style extension: `input`, `audio_length`, `seed`, `negative_prompt`, `guidance_scale`, `num_inference_steps`, `response_format` | sync, binary body | no | yes | **no** (needs compute capability 7.5+) | skip: no lyrics, no online ACE-Step, not on V100; noted as the nearest thing to an open text-to-audio shape |
| LocalAI `POST /v1/sound-generation` (backends `ace-step`, `acestep.cpp`, `audio.cpp`) | https://github.com/mudler/LocalAI | MIT / per model | active | sync, binary; `model_id`, `text`, `instrumental`, `vocal_language`, `caption`, `think` | sync | no | as its backend | as its backend | skip: a second router inside a stanza; only text-to-music exposed (its issue #12721) |

### 2.2 Models

"Server" says whether the model project ships an HTTP server itself. Speed is
wall time against audio length; ">1×" means faster than real time.

| Model | Repo | Licence | Last release | Max duration | Speed vs real time | Inputs | Output | Server | 3090 | V100 | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ACE-Step 1.5 (turbo 2B, XL 4B) | https://github.com/ace-step/ACE-Step-1.5 | MIT / MIT `(verify)` | v1.5 2026-01-28; XL 2026-04-02 | 600 s (quality measured to 240 s per a third party) | "full song" under 10 s on a 3090 (vendor; song length unstated), ~25× or more `(verify)`; V100 song in 1.4 s fp16 with a fix (third party) | prompt (caption), lyrics, duration, seed, BPM, key, time signature, language, reference audio, source audio (cover, repaint), task type, batch 1-8; instrumental is `[Instrumental]` lyrics | mp3 (default), flac, opus, aac, wav, wav32; 48 kHz stereo | **yes** (`acestep-api`) | yes; XL needs ≥20 GB without offload | fp16 NaN issues (§4.4) | **primary** |
| Stable Audio 3 (Small SFX, Small Music, Medium; Large is API-only) | https://github.com/Stability-AI/stable-audio-3 | vendor community licence, free under USD 1M annual revenue | 2026-05-20 | Small 120 s; Medium 380 s | very fast: 120 s in 0.45 s on an H200; Small on a laptop CPU ~6 s for 120 s (vendor table) | prompt, duration, seed, steps, init audio, inpaint; loop with BPM and bars (sa3.cpp) | 44.1 kHz stereo WAV | no (CLI and Python); served by `audio.cpp` and `sa3.cpp` | Small ~1.7-2.4 GB peak; Medium ~5-6.5 GB peak (third party) | via ggml `(verify)` | **fallback's effects model** (Small SFX) |
| YuE2 (YuE v1 on a branch) | https://github.com/multimodal-art-projection/YuE | Apache-2.0 code / **CC BY-NC 4.0** weights (v1: Apache-2.0) | yue2-v0.1.6; weights 2026-09-10 | full song (3.6 min example) | 71 s for 3.6 min on a 4090, ~3× (vendor); v1 ~6 min per 30 s, 0.08× | style prompt, lyrics with section tags; no reference audio in `audio.cpp` | 48 kHz stereo | no | needs a bf16 24 GB card, one request at a time (vendor) | **no** (bf16) | skip: non-commercial weights; reach it through `audio.cpp` if E5 wants it |
| HeartMuLa 3B | https://github.com/HeartMuLa/heartlib | Apache-2.0 / Apache-2.0 (since 2026-01-20) | 3B-happy-new-year 2026-02-13; repo active 2026-11-19 | ~6 min | RTF ≈ 1.0, about real time (vendor) | lyrics and tags (both required); no reference audio | WAV | no (CLI); ComfyUI nodes; `audio.cpp` | 6-24 GB depending on source; ~11 GB for 1 min, ~15 GB for 5 min in bf16 (MLX port) | `(verify)` | skip for now: slower than ACE-Step, no server; available through `audio.cpp` |
| SongGeneration 2 / LeVo 2 | https://github.com/tencent-ailab/SongGeneration | custom vendor terms (`NOASSERTION`) `(verify)` | v2 2026-03 (third party) | 4 min 30 s (large, v2-large) | `(verify)` | lyrics, text description, reference audio | `(verify)` | no | large 22 GB / 28 GB (two modes) | `(verify)` | skip: unclear licence, heavy, no server |
| DiffRhythm 2 | https://github.com/ASLP-lab/DiffRhythm2 | Apache-2.0 / Apache-2.0 | 2025-10-30 | 210 s | `(verify)`; paper claims efficient | lyrics, style prompt or reference audio | `(verify)` | no | ~1B params, F32 weights; VRAM `(verify)` | `(verify)` | skip: shorter, no server, instrumental mode still on its to-do list |
| Stable Audio Open 1.0 | https://github.com/Stability-AI/stable-audio-tools | MIT code / vendor community licence | 2024 `(verify)` | ~47 s | fast `(verify)` | prompt, duration, seed, negative prompt | 44.1 kHz stereo | no; vLLM-Omni serves it | yes | via `sa3.cpp` `(verify)` | skip: superseded by Stable Audio 3 |
| MusicGen / AudioGen (AudioCraft) | https://github.com/facebookresearch/audiocraft | MIT / **CC-BY-NC 4.0** | no GitHub releases; last models 2023-2024 | 30 s per pass | `(verify)` | prompt, melody reference | 32 kHz | no | yes | `(verify)` | skip: non-commercial, instrumental only, superseded |
| AudioLDM 2 | https://github.com/haoheliu/AudioLDM2 | **CC BY-NC-SA 4.0** | 2023 `(verify)` | ~10 s `(verify)` | `(verify)` | prompt | 16 kHz `(verify)` | no | yes | `(verify)` | skip: non-commercial, share-alike, superseded |
| TangoFlux | https://github.com/declare-lab/TangoFlux | MIT code / non-commercial weights `(verify)` | ICLR 2026 | 30 s | 30 s in ~3 s on an A40 (vendor), ~10× | prompt, duration, steps | 44.1 kHz stereo | no (CLI, ComfyUI node) | `(verify)` | `(verify)` | skip: non-commercial weights; Stable Audio 3 Small SFX covers effects |
| MMAudio (video or text to audio) | https://github.com/hkchengrex/MMAudio | MIT / **CC-BY-NC 4.0** | CVPR 2025 | 8 s trained; longer degrades | `(verify)` | video, prompt, negative prompt, duration, seed | 44.1 kHz (large_44k_v2) | no (Gradio demo) | ~6 GB in 16-bit (vendor) | fp16 with a cu126 torch `(verify)` | skip for now: non-commercial; best foley candidate if allowed |
| HunyuanVideo-Foley (XXL, XL) | https://github.com/Tencent-Hunyuan/HunyuanVideo-Foley | vendor community licence; excludes EU, UK, South Korea | XL 2025-09-29 | `(verify)` | `(verify)` | video, prompt | 48 kHz | no | XXL 20 GB (12 GB offload); XL 16 GB (8 GB offload) | `(verify)` | skip for now: licence territory, no server |
| ThinkSound / PrismAudio | https://github.com/FunAudioLLM/ThinkSound | Apache-2.0 text, but "research and educational purposes only" | NeurIPS 2025; PrismAudio ICLR 2026 | ~9 s samples | under 7 s per prediction on an L40S (third party) | video, text, audio | `(verify)` | no | 8-16 GB (third party) | `(verify)` | skip: research-only |

More than six candidates exist. Only two kinds of backend fit this host and
this router: the official ACE-Step server, which is the one model project with
a real job API, and the ggml ports, which run on every card and cover effects.
Everything else is a PyTorch script without a server, a non-commercial
licence, or both.

## 3. API surface

### 3.1 The front door clients use (router-owned, after §5.3)

The same lifecycle and status values as `/v1/videos` (#83, #69), so the router
code and the Playground treat both the same way. Field names reuse
`/v1/videos` (`prompt`, `seconds`) and `/v1/audio/speech` (`response_format`)
where they exist.

| Method and path | Purpose | Request | Response |
| --- | --- | --- | --- |
| `POST /v1/audio/generations` | create a job | JSON, or `multipart/form-data` when a file is attached | `200` or `202` with a job record |
| `GET /v1/audio/generations/{id}` | status | — | job record |
| `GET /v1/audio/generations/{id}/content` | the audio | — | audio bytes, `Content-Type` from the format (`audio/mpeg`, `audio/flac`, `audio/wav`, `audio/ogg`) |
| `DELETE /v1/audio/generations/{id}` | cancel or forget | — | job record, or `409` when a running job cannot be stopped |

Create fields (all optional except `prompt` or `video`):

| Field | Type | Meaning |
| --- | --- | --- |
| `model` | string | the stanza; omitted means the `audio_gen` `path_default` |
| `prompt` | string | style, genre, instruments, mood, or the sound to make |
| `lyrics` | string | words to sing with `[Verse]`, `[Chorus]` tags; empty for none |
| `instrumental` | bool | no vocals; the adapter sends `[Instrumental]` as the lyrics |
| `seconds` | number | target length; each backend clamps to its range |
| `seed` | integer | fixed seed for a repeatable take |
| `negative_prompt` | string | where the backend supports it |
| `response_format` | string | `mp3` (default), `flac`, `wav`, `opus` |
| `task` | string | `generate` (default), `cover`, `repaint`, `extend` |
| `reference_audio` | file | style reference (multipart) |
| `source_audio` | file | the audio a `cover` or `repaint` edits (multipart) |
| `repaint_start`, `repaint_end` | number | seconds, for `repaint` |
| `video` | file | foley input (multipart), for a future video-to-audio model |
| `extra` | object | backend-specific passthrough (ACE-Step `bpm`, `key_scale`, `thinking`, `inference_steps`); the adapter copies known keys and drops the rest |

Job record: `{id, object: "audio.generation", model, status, progress,
created_at, completed_at, seconds, error}`. `status` is `queued`,
`in_progress`, `completed` or `failed`; a cancelled job is `failed` with
`error.code: "cancelled"`, as the video note chose. `n` (batch) is left out:
one job is one take; a client that wants four takes sends four jobs.

### 3.2 The recommended backend (`acestep-api`)

Source: `docs/en/API.md` in the ACE-Step-1.5 repo, its launch scripts, and two
Docker wrappers that document the same server. Field lists come from search
extracts, so the shapes below are `(verify)` against the installed version
(checklist items 3 and 6).

| Method and path | Purpose | Request | Response |
| --- | --- | --- | --- |
| `POST /release_task` | queue a generation | `application/json`, `multipart/form-data` or `application/x-www-form-urlencoded` (below) | wrapper with `data.task_id` |
| `POST /query_result` | batch status and results | `{"task_id_list": ["<id>", ...]}`. The name matters: a wrong key such as `task_ids` silently returns an empty list (wrapper README) | wrapper with `data[]`: `task_id`, `status` (`0` running, `1` succeeded, `2` failed), `result` (a **JSON string** to parse again) |
| `GET /v1/audio?path=...` | download one file | — | the audio file |
| `GET /health` | liveness | — | `200` and a status body |
| `GET /v1/models` | DiT models on the server | — | list |
| `POST /v1/init` | load or switch a DiT (slot 1-3) and optionally the LM | `{model, slot, init_llm, lm_model_path}` | wrapper |
| `GET /v1/stats` | load and average job time | — | wrapper |
| `POST /format_input`, `POST /create_random_sample` | LM helpers (enrich a caption or lyrics, make a random sample) | JSON | wrapper |
| `GET /docs` | FastAPI's generated docs | — | HTML |

**Create fields** (snake_case or camelCase are both accepted; aliases noted):
`prompt` (the caption), `lyrics`, `audio_duration` (alias `duration`,
`target_duration`; 10-600 s; null lets the LM choose), `batch_size` (1-8),
`audio_format` (`mp3` default, `flac`, `opus`, `aac`, `wav`, `wav32`),
`inference_steps` (8 for turbo; 32-64 for base), `task_type`
(`text2music` default, `cover`, `repaint`, `extract`, `lego`, `complete`),
`reference_audio_path`, `src_audio_path`, `repainting_start`,
`repainting_end`, `audio_cover_strength`, `thinking` (LM-enhanced planning),
`model` (DiT choice), and metadata `bpm`, `key_scale`, `time_signature`. The
LM fills only metadata the caller left empty. The seed field name is not in
the extracts `(verify)`; the result echoes `seed_value`. How an uploaded file
maps to `reference_audio_path` in a multipart body is `(verify)`.

**Result** (one array element per take): `file` (`/v1/audio?path=...`, a
relative URL; file names are not the task id), `status`, `create_time`,
`prompt`, `lyrics`, `metas` (`bpm`, `duration`, `genres`, `keyscale`,
`timesignature`), `generation_info`, `seed_value`, `lm_model`, `dit_model`.

**Errors.** Every response is wrapped as `{data, code, error, timestamp}`
(`code: 200` on success, `timestamp` in milliseconds). The HTTP status used for
errors, and the answer for an unknown task id, are `(verify)`. With
`ACESTEP_API_KEY` set, every request needs `Authorization: Bearer <key>`.

**Sizes.** No documented limit on uploads or results `(verify)`. A 4-minute
mp3 is about 4-8 MB; a `wav32` is about 90 MB.

**Sync vs async.** Async only: submit, poll, download. There is no
held-connection route.

**Streaming, websocket.** None. No progress field was found in the extracts
`(verify)`.

**Queue and concurrency.** `ACESTEP_API_WORKERS` defaults to 1. Tasks live in
process memory `(verify)`, so a restart loses them. Output files are written
to `ACESTEP_OUTPUT_DIR` and are not cleaned up by anything found `(verify)`. A
wrapper project warns against retrying `POST /release_task` automatically,
because a retry makes a duplicate job.

**Model list and unload.** `GET /v1/models` lists DiT models; `POST /v1/init`
switches them. There is no unload endpoint the router needs: one process, and
the router unloads by killing the process group.

**Lazy load.** Recent releases load models on the first request, not at start
(release notes). `GET /health` answers before any weights are on the card, so
the first job pays the load time (§4.5).

## 4. Hardware fit

### 4.1 Current occupancy (from the GPU host's node config)

- **24 GB card (GPU 0):** intent model 1500 MB, embedding model 3600 MB,
  reranker 1500 MB, all resident: 6600 MB booked, about 17.9 GB left. Image
  stanzas (12000-22000 MB, `device: "0|1|2"`, `idle_ttl_seconds: 300`) prefer
  this card.
- **32 GB cards (GPU 1 and 2):** the large chat model, split across both,
  resident. **Any** new load on `"1|2"` evicts it.
- The issue asks about sitting beside the large chat model and the embedding
  model. Those are on different cards: the embedding model is on GPU 0, the
  large chat model on GPU 1 and 2.

### 4.2 Per model, per card

"Book" is the proposed `vram_mb`. None of these are measured on this host; the
peak is checklist item 6.

| Model and backend | 3090 (24 GB, sm_86) | V100 (32 GB, sm_70) |
| --- | --- | --- |
| ACE-Step 1.5 turbo + 1.7B LM, `acestep-api`, bf16, `pt` LM backend | book **12000** `(verify)`. Vendor tiers: DiT-only ≥4 GB, DiT + LM ≥6 GB with offload; offload switches on automatically below 20 GB of card memory, so on a 24 GB card everything stays resident | not proposed (§4.4) |
| ACE-Step 1.5 XL turbo (4B) + 1.7B LM, `audio.cpp`, GGUF 16 | ≥20 GB without offload (vendor, PyTorch); would evict the GPU 0 residents | book **20000** `(verify)`; fits a V100 alone |
| Stable Audio 3 Small SFX, `audio.cpp`, GGUF 16 | book **3000**: vendor peak 1.69 GB (5 s) to 2.40 GB (120 s), plus runtime overhead `(verify)` | fits; but evicts the large chat model |
| Stable Audio 3 Medium, `audio.cpp` | ~5.1-6.5 GB peak (third party); book 8000 `(verify)` | fits |

**Quantisation.** `acestep-api` runs bf16 on the 3090; the vendor mentions
quantisation for the 12 GB XL tier `(verify)`. `audio.cpp` ships GGUF 16 and
Q8 for these families. Q8 is the sensible default for a V100.

### 4.3 Can it sit beside the residents?

- **GPU 0, music only:** 6600 + 12000 = 18600 of about 24500 MB. Fits with no
  eviction while no image model is loaded. With an idle image model loaded,
  the image model is evicted first (it is the idle non-resident).
- **GPU 0, music and effects:** 6600 + 12000 + 3000 = 21600 MB. Still fits.
- **The large chat model:** untouched as long as nothing music-related lands on
  `"1|2"`. That is why both proposed default stanzas use `device: 0`.

### 4.4 fp16 fallback on sm_70

- **`acestep-api`:** its Linux x86_64 lock pins torch 2.10.0+cu128, which still
  includes sm_70 (cu128 drops Volta from 2.11) `(verify)`, so its own venv can
  start on a V100. The host's `/opt/ai/venv-sidecars` (cu130) cannot. But fp16
  on pre-Ampere cards is broken in places: songs with lyrics gave all-NaN
  latents and the VAE decoder gave silence. Upstream PRs move the lyric
  encoder to fp32 (#1352, #1377), fall back to fp32 diffusion on pre-Ampere
  cards (#1274) and keep the bundled nano-vllm LM in fp16 range (#1353); merge
  status is `(verify)`. The VAE fp32 fix was seen only in a fork (a wrapper's
  PR #2, 2026-09-29). The SFT model still failed in fp16 with the lyric fix.
  The default LM backend (nano-vllm) does not support sm_70; the `pt` backend
  is the fallback. Verdict: possible, fragile, not proposed.
- **`audio.cpp`:** ggml computes on CUDA in fp16 and fp32, with no bf16 or
  FlashAttention 2 requirement. Volta needs a CUDA 12.x toolkit build with
  `--cuda-arch 70` (the project's Linux build doc shows `--cuda-arch` and
  `CMAKE_CUDA_ARCHITECTURES`; its CUDA 13 package excludes Volta). Whether the
  same fp16 overflow hits ggml's ACE-Step path is unknown `(verify)`
  (checklist item 8).

### 4.5 Speed, CPU-only and cold start

- **Speed.** ACE-Step 1.5 turbo: vendor claims a full song in under 10 s on a
  3090 and under 2 s on an A100; one review says up to 4 minutes of music in
  about 20 s on an A100. `thinking` (LM planning) adds time `(verify)`. Stable
  Audio 3 Small SFX: under half a second on an H200 for any length up to 120 s;
  expect a few seconds on GPU 0 `(verify)`. Both are far faster than real time,
  so a job is seconds to a minute, not the minutes video takes.
- **CPU-only.** Stable Audio 3 Small SFX is built for CPU (vendor: about 6 s
  for 120 s on a laptop CPU). On this host's dual Xeon it is a real option as a
  `device: cpu` stanza with `vram_mb: 0`, which never evicts anything
  `(verify)` speed (checklist item 7). ACE-Step on CPU is possible in all three
  runtimes but slow; not proposed.
- **Cold start.** `acestep-api` answers `/health` after the Python import, in
  roughly 10-30 s `(verify)`; the first job then loads about 10 GB of
  checkpoints from disk, estimated 20-60 s from SSD `(verify)`. `audio.cpp`
  also loads lazily; its `/health` only means the server is up. `/v1/init`
  could load eagerly, but the router has no post-start hook, so the first job
  pays.

## 5. How the router would run it

### 5.1 Spawn

```
/opt/ai/ace-step-1.5/.venv/bin/acestep-api --host 127.0.0.1 --port {port}
```

with the model choice in `env` (`ACESTEP_CONFIG_PATH`,
`ACESTEP_LM_MODEL_PATH`, `ACESTEP_INIT_LLM=true`, `ACESTEP_LM_BACKEND=pt`,
`ACESTEP_OUTPUT_DIR`).

- Foreground process; the process-group kill covers uvicorn and its workers.
- `{port}` through `--port`. `{gpu}` is not used: GPU 0 is fixed with
  `CUDA_VISIBLE_DEVICES=0`, as on the host's resident stanzas.
- Its own venv (`uv sync` in a checkout), not `venv-sidecars`: the lock pins its
  own torch.
- `ACESTEP_LM_BACKEND=pt`: nano-vllm may preallocate a share of the card for
  its KV cache `(verify)`, which would break the `vram_mb` booking; `pt` keeps
  the footprint predictable. Revisit after checklist item 6 if `pt` is slow.
- `HF_HUB_OFFLINE=1`: the server downloads missing checkpoints at first use;
  offline mode makes a missing file fail fast instead of a surprise download
  `(verify)` that the server honours it.
- No `ACESTEP_API_KEY`: the router cannot add an `Authorization` header to
  proxied requests. The server binds to `127.0.0.1`, so it is not reachable
  except through the router.
- Absolute checkpoint paths in `ACESTEP_CONFIG_PATH` work in the Docker
  wrappers; the official docs show model names `(verify)` (checklist item 3).

### 5.2 Stanza fields

| Field | Value | Why |
| --- | --- | --- |
| `health_check` | `/health` | answers once the server is up; models load on the first job |
| `spin_up_seconds` | `60` | Python import and FastAPI start; the model load is in the first job, not the health wait |
| `vram_mb` | `12000` | §4.2; replace with the measured peak |
| `idle_ttl_seconds` | `900` | not resident: GPU 0 is shared with image models. Matches the video note |
| `slots` | `1` | one generation at a time (`ACESTEP_API_WORKERS=1`); it does not bound async jobs once §5.3 lands |
| `match` | `{path_default: true}` | a model-less `POST /v1/audio/generations` lands here; the `audio.cpp` stanzas are named by `model` |
| `api_type` | `audio_gen` | routes `/v1/audio/generations` |
| `capabilities` | `[audio_in, audio_out]` | it takes reference and source audio and makes sound |

### 5.3 What the router has to add (a router change, no new stanza key)

`jobs.go` is generic except for the path. Make `/v1/audio/generations` a second
job collection:

1. A list of job collections, `/v1/videos` and `/v1/audio/generations`.
   `jobID` matches either prefix; `proxyJobAware` records a `2xx` `POST` to
   either collection; the job keeps its collection.
2. `pollJob` polls `<collection>/{id}` instead of `/v1/videos/{id}`.
3. The Activity row's path is the job's collection.
4. `SPEC.md` (Async jobs, HTTP API table) names both paths;
   `jobs_test.go` repeats the submit, hold, follow-up and delete tests against
   `fake-model`'s audio routes, which already exist.

Everything else (hold one occupancy slot, poll every 2 s, route follow-ups by
id, `404` for unknown ids, `job_ttl_minutes`, peers, no persistence) applies
unchanged. ACE-Step task ids are generated by the server; they are
UUID-like `(verify)` and unique per process, which is what the job table
needs.

### 5.4 What the adapters have to translate

Same pattern as the video note: a small process started by the stanza
`command` in front of the backend
(`/opt/ai/bin/audio-gen-adapter {port} -- acestep-api ... --port <private port>`).
It serves `/v1/audio/generations` and `/health` on `{port}` and forwards to the
backend on a private port. One binary can carry both translations below. It
does not exist yet; it is the same piece of work as the video adapter and
should share its code.

**For `acestep-api`** (no state in the adapter):

| `/v1/audio/generations` | `acestep-api` |
| --- | --- |
| `POST` JSON or multipart: `prompt`, `lyrics`, `instrumental`, `seconds`, `seed`, `response_format`, `task`, `reference_audio`, `source_audio`, `repaint_start`, `repaint_end`, `extra` | `POST /release_task`: `prompt`, `lyrics` (`[Instrumental]` when `instrumental`), `audio_duration` (clamped 10-600), seed field `(verify)`, `audio_format`, `task_type` (`generate` → `text2music`, `cover`, `repaint`, `extend` → `complete`), files as multipart parts `(verify)` names, `repainting_start`, `repainting_end`, `batch_size: 1`, plus `extra` keys `bpm`, `key_scale`, `time_signature`, `thinking`, `inference_steps`. Drop `model` |
| response `{id, object: "audio.generation", status: "queued", ...}` | `id` = `data.task_id` |
| `GET /{id}`: `queued`, `in_progress`, `completed`, `failed` | `POST /query_result {"task_id_list": [id]}`: `0` → `in_progress`, `1` → `completed`, `2` → `failed`; an empty `data` → `404` |
| `progress` | none available `(verify)`; report `0` then `100` |
| `GET /{id}/content` | parse `result` (a JSON string), take `[0].file`, stream `GET <file>` from the backend with its `Content-Type` |
| `DELETE /{id}` | no cancel endpoint found `(verify)`: answer `409` while running; a finished job is forgotten. Jobs take seconds, so this rarely matters |
| `GET /health` | `GET /health` |

**For `audio.cpp`** (the adapter keeps state, which is why it is the fallback):
`POST /v1/tasks/run` is synchronous `(verify)`, so the adapter makes the job
itself. On create it returns an id at once, runs the backend request in the
background with the family's fields (task `gen`; for `ace_step`: route
`text2music`, `text` from `prompt`, `lyrics`, duration in seconds; for
`stable_audio`: `text`, duration, seed; exact JSON names `(verify)`, checklist
item 7), keeps the result in a temp file until it is fetched or the
`idle_ttl_seconds` window passes, and answers polls from its own table.
Reference audio must be a server-local path or base64 (`audio` or
`audio_base64`), so the adapter writes uploads to a temp directory first.

### 5.5 What breaks if the router only passes bytes through

Today, with no §5.3 change and no adapter, a careful client can drive the
native API through the router:
`POST /upstream/ace-step-1.5-turbo/release_task`, then
`POST /upstream/ace-step-1.5-turbo/query_result`, then
`GET /upstream/ace-step-1.5-turbo/v1/audio?path=...`. Plainly, what breaks:

- **`/v1/audio/generations` does not work at all**: `acestep-api` has no such
  route, so a model-less create reaches it and gets `404`.
- **The native paths are not passthrough paths.** `/release_task` and
  `/query_result` are not in the router's `/v1/*` list; they need `/upstream/`
  (local only) or `?model=` on every call. `GET /v1/audio?path=` is under
  `/v1/*` but names no model and maps to no `api_type`, so without `?model=`
  it is `404` from the router.
- **A running job can be killed.** The router counts the job only while a
  request is open. Between polls the model is "idle", so another model's load
  can evict it and take the job with it. ACE-Step jobs are short, so this is a
  small window, but real when an image model loads on GPU 0.
- **`slots` means nothing** for async submits; a burst queues inside the
  backend.
- **`/v1/models` collides.** The router answers `/v1/models` itself, so the
  backend's model list is only reachable through `/upstream/`.
- **Nothing cleans the output folder.** Files pile up in `ACESTEP_OUTPUT_DIR`
  across jobs `(verify)` retention.
- **Activity** keeps the create and poll JSON (up to 256 KB) and only the
  content type, size and hash of the audio download.

The router rewrites no path, field or header for the stanzas in §6. Every item
above is fixed by §5.3 plus the adapter, not by rewriting.

### 5.6 One server, many models?

| Backend | Many models in one process? | Proposed |
| --- | --- | --- |
| `acestep-api` | yes: up to three DiT slots (`ACESTEP_CONFIG_PATH2`, `3`), one LM, the request's `model` picks the DiT | one DiT per stanza first; adding turbo-shift3 or SFT to the same process later is fine if `vram_mb` is raised to the sum (one LM is shared) |
| `audio.cpp` | yes: every model in its JSON config; `max_loaded_models` (default 0, keep all) evicts the least recently used | one model per process (one config entry), so `vram_mb` stays honest |
| ComfyUI | yes, by workflow | not proposed |
| YuE2, HeartMuLa, SongGeneration, DiffRhythm 2, MMAudio, HunyuanVideo-Foley | no server at all | a process each, behind a small server of our own, or through `audio.cpp` where it has the family |

## 6. Proposed stanzas

Paths are placeholders under `/opt/ai/`. Ports sit above the video note's
`5938-5939`; check `reserved_ports` before deploying. Every key is a
documented field in the `SPEC.md` config block. **No new key is needed.** The
only router change is §5.3, which adds no key.

These run the native APIs today, used through `/upstream/<model_id>/...`
(§5.5). Once §5.3 and the adapter exist, the only change is the `command`
prefix (`/opt/ai/bin/audio-gen-adapter {port} --`, with the backend moved to a
private port) and `health_check: /health` on the adapter.

### 6.1 Primary, 24 GB card (sm_86): ACE-Step 1.5 turbo

```yaml
  - model_id: ace-step-1.5-turbo
    name: "ACE-Step 1.5 turbo + 1.7B LM (text/lyrics to music, official API server)"
    command: >
      /opt/ai/ace-step-1.5/.venv/bin/acestep-api
      --host 127.0.0.1 --port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0", "ACESTEP_CONFIG_PATH=/opt/ai/models/music/ace-step-1.5/acestep-v15-turbo", "ACESTEP_LM_MODEL_PATH=/opt/ai/models/music/ace-step-1.5/acestep-5Hz-lm-1.7B", "ACESTEP_INIT_LLM=true", "ACESTEP_LM_BACKEND=pt", "ACESTEP_OUTPUT_DIR=/opt/ai/cache/ace-step/outputs", "HF_HUB_OFFLINE=1"]
    vram_mb: 12000
    device: 0
    spin_up_seconds: 60
    api_type: audio_gen
    capabilities: [audio_in, audio_out]
    match: {path_default: true}
    health_check: /health
    idle_ttl_seconds: 900
    slots: 1
    port: 5940
```

### 6.2 Fallback, 24 GB card: Stable Audio 3 Small SFX through `audio.cpp`

```yaml
  - model_id: stable-audio-3-small-sfx
    name: "Stable Audio 3 Small SFX (text to sound effects and ambience, audio.cpp)"
    command: >
      /opt/ai/bin/audiocpp_server
      --config /opt/ai/config/audiocpp-stable-audio-3-small-sfx.json
      --backend cuda
      --host 127.0.0.1 --port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
    vram_mb: 3000
    device: 0
    spin_up_seconds: 60
    api_type: audio_gen
    capabilities: [audio_out]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 900
    slots: 1
    port: 5941
```

The JSON config holds one entry, along the lines of
`{"max_loaded_models": 1, "models": [{"id": "stable-audio-3-small-sfx",
"family": "stable_audio", "path": "/opt/ai/models/music/stable-audio-3-small-sfx",
"device": 0}]}`. The entry shape (`id`, `family`, `path`, `device`) is from the
project's docs; the top-level `models` key and the `--host` and `--port` flags
are `(verify)` (checklist item 4). If the port can only be set in the JSON
file, keep `port: 5941` fixed in both places and drop `{port}` from the
command. `device: 0` in the JSON is the first visible card, which
`CUDA_VISIBLE_DEVICES` makes GPU 0.

If checklist item 7 shows Small SFX is fast enough on the Xeons, the CPU form is
this stanza with `--backend cpu`, `device: cpu`, `vram_mb: 0`, no
`CUDA_VISIBLE_DEVICES`, and the JSON `device` removed. It then never evicts
anything and is never evicted. One `model_id` per node, so choose one.

### 6.3 V100 (sm_70), agreed batch windows only: ACE-Step 1.5 XL turbo

Viable on paper with a CUDA 12 `audio.cpp` build; unmeasured. It evicts the
large chat model (about 7 minutes to reload), so it is for scoring a batch in
an agreed window, not for casual use (open question 3). XL is a different size
from turbo, so it is a different `model_id` under the identity rule and can sit
in the same config as §6.1.

```yaml
  - model_id: ace-step-1.5-xl-turbo
    name: "ACE-Step 1.5 XL turbo (text/lyrics to music, audio.cpp, V100)"
    command: >
      /opt/ai/bin/audiocpp_server
      --config /opt/ai/config/audiocpp-ace-step-1.5-xl-turbo.json
      --backend cuda
      --host 127.0.0.1 --port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 20000
    device: "1|2"
    spin_up_seconds: 60
    api_type: audio_gen
    capabilities: [audio_in, audio_out]
    match: {body_field: model}
    health_check: /health
    idle_ttl_seconds: 600
    slots: 1
    port: 5942
```

One `audiocpp_server` binary built with CUDA 12.x and `--cuda-arch "70;86"`
serves §6.2 and §6.3. The V100s sit on NUMA node 1 (host config); whether this
stanza wants the same `taskset` wrapper as the large chat model is open
`(verify)`.

## 7. Licence and risk flags

Internal use only, but these are the terms to know.

- **ACE-Step 1.5:** MIT for code and weights per the model card and install
  guide; the card says outputs may be used commercially. Some third-party
  pages say Apache-2.0; either is permissive. Read the repo `LICENSE` and the
  bundled text-encoder and LM licences before relying on it `(verify)`.
- **`audio.cpp`:** Apache-2.0. Model weights keep their own licences; the
  project lists them per family in `docs/model_licenses.md`.
- **Stable Audio 3 (open weights):** the vendor's community licence. Free
  commercial use only under USD 1M annual revenue, counted for the whole
  organisation; above that an enterprise licence is needed. The organisation
  that runs this host is likely above that threshold `(verify)`, so treat
  Stable Audio 3 output as internal-only until someone confirms the licence
  position (open question 4). The Medium repo is gated and also needs the
  separate model terms of its bundled text encoder accepted. Training data is
  stated to be licensed.
- **Stable Audio Open 1.0:** the same community licence `(verify)`.
- **Non-commercial weights:** YuE2 (CC BY-NC 4.0, with a carve-out for
  individual creators only), MusicGen and AudioGen (CC-BY-NC 4.0), AudioLDM 2
  (CC BY-NC-SA 4.0, share-alike), MMAudio (CC-BY-NC 4.0), TangoFlux
  (non-commercial research use `(verify)`), ThinkSound and PrismAudio
  (research and education only; also bundles a VAE under the Stable Audio
  community licence).
- **Territory-restricted:** HunyuanVideo-Foley's community licence does not
  apply in the EU, UK or South Korea.
- **Unclear:** SongGeneration (custom terms, `NOASSERTION` on GitHub),
  `acestep.cpp` and `sa3.cpp` (licence files not seen) `(verify)`.
- **Permissive:** HeartMuLa and DiffRhythm 2 (Apache-2.0), YuE v1
  (Apache-2.0), ComfyUI (GPL-3.0; calling it over HTTP does not make an adapter
  a derivative).
- **Output risk.** Generated music can come close to existing songs, and
  lyrics and vocal style can imitate real artists. Use fictional prompts and
  lyrics in tests and demos; do not prompt for a named artist's style or voice.
- **Availability.** A V100 placement takes the large chat model down for about
  7 minutes after the job. Until §5.3 lands, a running job is not protected
  from eviction.
- **Disk.** `acestep-api` writes every take to `ACESTEP_OUTPUT_DIR` and nothing
  found deletes it; the `audio.cpp` adapter's temp files need the same care.
- **Maturity.** ACE-Step's server API has changed shape once already (the older
  doc used `/v1/music/generate` with string statuses). `audio.cpp` adds model
  families monthly. Pin both and re-run checklist item 6 on every upgrade.

## 8. GPU host verification checklist and open questions

Run as the service user. Steps 1 to 5 are read-only. **Steps 6 to 8 change GPU
state and need an agreed window**: they start backends by hand, outside the
router, which does not count that memory in its ledger. Before each GPU step,
check the card's free memory and unload any router stanza that would not fit;
after it, `kill` the process. On a V100 (step 8), expect the large chat model
to be down until preload restore brings it back. Use made-up prompts and
lyrics only. Model downloads for gated repos need a licence click-through in a
browser by the account owner; never paste a token into a ticket or a shell
history line.

```bash
# 1. Cards and what holds them now.
nvidia-smi --query-gpu=index,name,compute_cap,memory.total,memory.used,memory.free --format=csv
curl -s http://127.0.0.1:18080/_router/status | head -c 2000
free -g; df -h /opt/ai

# 2. Toolchains: CUDA 12.x for an sm_70 ggml build, uv for the ACE-Step venv, ffmpeg codecs.
ls -d /usr/local/cuda-12* /usr/local/cuda-13* 2>/dev/null
command -v uv; uv --version
ffmpeg -hide_banner -encoders | grep -E 'libmp3lame|flac|libopus|aac'

# 3. ACE-Step checkout (if present): pinned torch, CLI flags, env var names, endpoint list.
A=/opt/ai/ace-step-1.5
git -C $A log -1 --format='%h %cd' 2>/dev/null
grep -n 'torch==' $A/pyproject.toml
$A/.venv/bin/python -c "import torch; print(torch.__version__, torch.cuda.get_arch_list())"   # expect sm_70 and sm_86 for cu128 2.10
$A/.venv/bin/acestep-api --help 2>&1 | head -40
grep -rn 'ACESTEP_[A-Z_]*' $A/acestep/api_server.py | grep -o 'ACESTEP_[A-Z_0-9]*' | sort -u
grep -n '@app\.\(get\|post\|delete\)' $A/acestep/api_server.py
grep -n 'seed\|use_random_seed\|reference_audio\|src_audio' $A/acestep/api_server.py | head -40

# 4. audio.cpp build (if present): CUDA archs, server flags, config keys.
AC=/opt/ai/audio.cpp
git -C $AC log -1 --format='%h %cd' 2>/dev/null
grep -E 'CMAKE_CUDA_ARCHITECTURES|ENGINE_ENABLE_CUDA' $AC/build/CMakeCache.txt
$AC/build/bin/audiocpp_server --help 2>&1 | grep -E -- '--(host|port|config|backend|max-loaded-models)'
sed -n '1,200p' $AC/app/server/api.md | grep -n -A12 'tasks/run'

# 5. Licence files the note could not read.
cat $A/LICENSE | head -5
cat $AC/docs/model_licenses.md | grep -i -A3 'stable_audio\|ace_step'

# 6. GPU STATE CHANGE (GPU 0). Start acestep-api by hand with the §6.1 env; time health and the first job; record peak VRAM.
#    First: confirm >= 12000 MiB free on GPU 0 (an idle image model may need unloading).
date +%s; env CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=0 <the §6.1 env> \
  $A/.venv/bin/acestep-api --host 127.0.0.1 --port 5998 &
until curl -sf -o /dev/null http://127.0.0.1:5998/health; do sleep 1; done; date +%s    # spin_up_seconds
nvidia-smi --query-gpu=index,memory.used --format=csv -l 1 > /tmp/vram-acestep.csv &    # peak for vram_mb
curl -s -X POST http://127.0.0.1:5998/release_task -H 'Content-Type: application/json' \
  -d '{"prompt":"warm acoustic folk, fingerpicked guitar, soft brushes","lyrics":"[Verse]\nRed dust on a fictional road\n[Chorus]\nWe carry the light home","audio_duration":60,"batch_size":1,"audio_format":"mp3"}'
#    Note the HTTP status and the task_id format.
curl -s -X POST http://127.0.0.1:5998/query_result -H 'Content-Type: application/json' -d '{"task_id_list":["<id>"]}'
#    Repeat until status 1; note wall time for 60 s of audio, and any progress field.
#    Then GET the result's file URL and ffprobe it (codec, 48 kHz, stereo, duration).
curl -s -X POST http://127.0.0.1:5998/query_result -H 'Content-Type: application/json' -d '{"task_id_list":["no-such-id"]}'   # unknown id answer
#    Also: one instrumental ("lyrics":"[Instrumental]"), one 240 s song, one with "thinking":true,
#    one multipart create with a generated test tone as reference audio, and a restart (are tasks gone?).
#    Check whether ACESTEP_OUTPUT_DIR is ever cleaned, and whether HF_HUB_OFFLINE=1 is honoured.

# 7. GPU STATE CHANGE (GPU 0, small). audio.cpp with the §6.2 config on port 5999: health, one 10 s effect,
#    peak VRAM, wall time, and the exact /v1/tasks/run request and response. Then the same with --backend cpu
#    (no GPU) and note wall time for 10 s and 60 s effects.

# 8. GPU STATE CHANGE (one V100; evicts the large chat model, agreed window only). audio.cpp with the §6.3
#    config, CUDA_VISIBLE_DEVICES=1: does ACE-Step XL turbo produce finite audio (not silence, not NaN) in fp16,
#    peak VRAM, wall time for a 120 s song.

# 9. Repo check from a workstation with Go: make check
```

### Open questions for Kevyn

1. **Build the adapter and the §5.3 router change now, or wait?** ACE-Step jobs
   take seconds, so `/upstream/` works for a first trial. The sibling project's
   E5 and E6 should say how much music and effects work is coming.
2. **Sound effects at all, and on GPU or CPU?** If E6 needs effects, §6.2 adds
   about 3 GB on GPU 0, or nothing as a CPU stanza if checklist item 7 is fast
   enough.
3. **Music on the V100s?** Every `"1|2"` placement evicts the large chat model
   for about 7 minutes. If not, drop §6.3; everything else is on GPU 0.
4. **Stable Audio 3 licence.** The free tier stops at USD 1M annual revenue for
   the whole organisation. Is internal use under the community licence
   acceptable, or should effects wait for a licence decision?
5. **Foley.** Is audio-from-video needed? Every candidate is non-commercial,
   territory-restricted or research-only, and none ships a server.
6. **Where does the adapter live?** In this repo under `cmd/`, built with the
   router, and shared with the video adapter?

## 9. Sources

All read on 2026-11-20 through search-engine extracts (no direct page fetch in
this run).

ACE-Step 1.5 (primary)

- https://github.com/ace-step/ACE-Step-1.5
- https://github.com/ace-step/ACE-Step-1.5/blob/main/docs/en/API.md
- https://github.com/ace-step/ACE-Step-1.5/blob/main/docs/en/INSTALL.md, https://ace-step.github.io/ACE-Step-1.5/en/INSTALL
- https://github.com/ace-step/ACE-Step-1.5/blob/main/docs/en/INFERENCE.md
- https://github.com/ace-step/ACE-Step-1.5/blob/main/start_api_server.sh
- https://github.com/ace-step/ACE-Step-1.5/blob/main/pyproject.toml (torch 2.10.0+cu128 on Linux x86_64)
- https://github.com/ace-step/ACE-Step-1.5/releases, https://github.com/ace-step/ACE-Step-1.5/releases/tag/v0.1.6
- https://huggingface.co/ACE-Step/Ace-Step1.5 (licence, speed claims)
- https://huggingface.co/spaces/ACE-Step/Ace-Step-v1.5/blob/0228d482b4800ab116879e76d7ce85f1b034cb04/API.md (older API shape)
- https://github.com/ace-step/ACE-Step-1.5/pull/1353, https://github.com/ace-step/ACE-Step-1.5/issues/1389 (pre-Ampere fixes #1274, #1352, #1377)
- https://github.com/christopherrobertbrooks-tech/volta-ace-step (V100 fp16 NaN, timings; third party)
- https://github.com/sam-dumont/ace-step-1.5, https://github.com/sam-dumont/ace-step-1.5/pull/2 (wrapper: env vars, `task_id_list`, VAE fp32 fix)
- https://github.com/ValyrianTech/ace-step-1.5 (wrapper: endpoint list)
- https://deepwiki.com/ace-step/ACE-Step-1.5/2.3-quick-start-guide (secondary)
- https://note.com/ai_meg/n/n37aade2e9c04?hl=en (client example; third party)
- https://andrew.ooo/posts/ace-step-open-source-music-generation/, https://www.it-jim.com/blog/ai-for-musicians/ (third-party speed claims)

audio.cpp (fallback)

- https://github.com/0xShug0/audio.cpp
- https://github.com/0xShug0/audio.cpp/blob/main/docs/music_generation.md
- https://github.com/0xShug0/audio.cpp/blob/main/app/server/README.md
- https://github.com/0xShug0/audio.cpp/blob/main/app/server/api.md
- https://github.com/0xShug0/audio.cpp/blob/main/docs/build/linux.md
- https://github.com/0xShug0/audio.cpp/releases, https://github.com/0xShug0/audio.cpp/releases/tag/release-0.6 (CUDA 13 package excludes Volta)
- https://ai-tldr.dev/tools/audio-cpp/, https://betterstack.com/community/guides/ai/audio-cpp/ (third party; release history)

Other runtimes

- https://github.com/ServeurpersoCom/acestep.cpp, https://huggingface.co/Serveurperso/ACE-Step-1.5-GGUF, https://github.com/ace-step/acestep.vst3
- https://github.com/betweentwomidnights/sa3.cpp, https://github.com/betweentwomidnights/sa3.cpp/blob/main/docs/SERVER.md, https://github.com/betweentwomidnights/sa3.cpp/releases/tag/v0.2.4
- https://github.com/Stability-AI/Awesome-Stable-Audio
- https://docs.comfy.org/changelog, https://docs.comfy.org/tutorials/audio/ace-step/ace-step-v1-5, https://comfy.org/workflows/use-cases/ai-music-generator/
- https://docs.vllm.ai/projects/vllm-omni/en/stable/serving/audio_generate_api/, https://github.com/vllm-project/vllm-omni/blob/main/docs/serving/audio_generate_api.md
- https://github.com/vllm-project/vllm-omni/issues/1252, https://github.com/vllm-project/vllm-omni/pull/4010 (ACE-Step in vLLM-Omni)
- https://docs.vllm.ai/projects/recipes/en/stable/StabilityAI/Stable-Audio-Open.html
- https://localai.io/features/sound-generation/, https://github.com/mudler/LocalAI/issues/12721

Models

- https://github.com/Stability-AI/stable-audio-3, https://github.com/Stability-AI/stable-audio-3/blob/main/docs/guides/model-overview.md
- https://huggingface.co/stabilityai/stable-audio-3-small-sfx, https://huggingface.co/collections/stabilityai/stable-audio-3
- https://stability.ai/news-updates/meet-stable-audio-3-the-model-family-built-for-artistic-experimentation-with-open-weight-models
- https://nexgpu.net/en/models/stable-audio/ (third party; Medium VRAM)
- https://the-decoder.com/stability-ai-launches-stable-audio-3-0-with-up-to-six-minute-tracks-and-open-weights/ (third party; licence threshold)
- https://github.com/Stability-AI/stable-audio-tools
- https://github.com/multimodal-art-projection/YuE, https://github.com/multimodal-art-projection/YuE/blob/main/docs/generation.md
- https://thakicloud.com/tech-blog/en/owm/yue2-open-source-music-generation/ (third party; YuE2 hardware and licence)
- https://github.com/HeartMuLa/heartlib, https://github.com/Acelogic/heartlib-mlx (memory breakdown)
- https://github.com/tencent-ailab/SongGeneration, https://huggingface.co/tencent/SongGeneration, https://github.com/vllm-project/vllm-omni/issues/3390 (model table)
- https://github.com/ASLP-lab/DiffRhythm2, https://huggingface.co/ASLP-lab/DiffRhythm2, https://arxiv.org/abs/2510.22950
- https://github.com/facebookresearch/audiocraft, https://huggingface.co/facebook/musicgen-large
- https://github.com/haoheliu/AudioLDM2, https://github.com/haoheliu/AudioLDM2/blob/main/LICENSE
- https://github.com/declare-lab/TangoFlux, https://huggingface.co/declare-lab/TangoFlux-base
- https://github.com/hkchengrex/MMAudio, https://huggingface.co/hkchengrex/MMAudio
- https://github.com/Tencent-Hunyuan/HunyuanVideo-Foley, https://huggingface.co/tencent/HunyuanVideo-Foley/blob/main/LICENSE
- https://github.com/FunAudioLLM/ThinkSound, https://replicate.com/zsxkib/thinksound/readme (third party; hardware)

This repo

- [`SPEC.md`](../../SPEC.md) (stanza fields, api_type table, Async jobs, body cap, eviction)
- [`docs/research/video-backends.md`](video-backends.md) (job API shape, adapter pattern, GPU 0 occupancy, port range)
- `internal/router/jobs.go`, `internal/router/matcher.go`, `internal/router/proc.go`,
  `internal/router/router.go`, `cmd/fake-model/main.go`
- the GPU host's node config under `configs/`

## 7. Deployed on buster (2026-11-21)

`ace-step-music` is live: GPU0, port 5941 (shim) with ACE-Step's REST server on
the private port 5942 in the same process group, `api_type: audio_gen`,
`match: {path_default: true}`, `capabilities: [audio_out]`, `vram_mb: 17000`,
`idle_ttl_seconds: 300`, `spin_up_seconds: 120`, **no `hold_seconds`**.

- **`hold_seconds` blocked every handoff.** Both heavy backends share GPU0
  (video books 19 GB, music 17 GB, plus the 3.6 GB embed model), so only one can
  be resident. With `hold_seconds: 900` a stanza stays un-evictable for 15
  minutes after its last request and the other backend gets
  `503 no GPU can take … now and no peer served it` even though nothing is
  running. Dropping the hold and keeping `idle_ttl_seconds: 300` makes the
  handoff work: a music job issued after the video model's TTL returned an id
  and a completed 15 s clip, with the router reclaiming the idle video model to
  make room. The hold exists for realtime sessions, not for job models — a job
  already holds its model through occupancy.

**What the deployment corrected in this note:**

- **`ACE-Step/acestep-5Hz-lm-1.7B` exists; the HF API answered 404 transiently**
  and a retry downloaded the full 3.5 GB. The 1.7B is what runs here: it costs
  ~3.9 GB more VRAM (13.4 -> 17.3 GB) and ~14 s more on a cold 15 s job, and
  replaces the 0.6B's weaker prompt/lyrics metadata.
- **The locked torch build is broken on this host.** `uv sync` pins
  `torch 2.10.0+cu128`, and with it *every* bf16 GEMM fails with
  `CUBLAS_STATUS_INVALID_VALUE` — a bare `4096³` bfloat16 matmul reproduces it,
  so it is the wheel, not the model. `torch 2.11.0+cu130` (and a matching
  `torchvision 0.26.0+cu130`, or the CUDA-version check refuses to import)
  generates normally. cu130 has **no sm_70**, so this venv cannot serve a V100;
  a V100 placement needs its own cu128 venv and the fp16 caveats in §4.4.
- **`ACESTEP_CONFIG_PATH` takes a model name, not a path.** A filesystem path
  is rejected with `Unknown DiT model: <path>`; names resolve under
  `ACESTEP_CHECKPOINTS_DIR`.
- **The router rejects `job:` on anything that is not `api_type: video`.** An
  `audio_gen` stanza is tracked by the generic job collection as long as its
  backend speaks the OpenAI job shape, which is what the shim provides — no
  adapter key.
- **A quoted `bash -lc '…'` in a YAML folded command reaches the backend as a
  broken shell word** (`unexpected EOF while looking for matching '`). The
  launcher is a script in the repo (`deploy/acestep-shim/run.sh`, `PORT` from the
  router) instead.

**Shim.** `deploy/acestep-shim/server.py` turns `POST /v1/audio/generations`,
`GET …/{id}` and `GET …/{id}/content` into `/release_task`, `/query_result` and
`/v1/audio?path=`. Status maps 0 → `in_progress`, 1 → `completed`, 2 → `failed`.
`DELETE` ends the router's hold only: ACE-Step has no per-task cancel route, so
the server finishes what it started.

**Measured, 3090, 48 kHz stereo out:**

| request | LM | wall | ratio | file |
|---|---|---|---|---|
| 15 s (cold: model + LM load on first job) | 0.6B | 60 s | 4.0x | 2.7 MB |
| 30 s | 0.6B | 61 s | 2.0x | 5.8 MB |
| 180 s (3 min) | 0.6B | 182 s | 1.0x | 34.6 MB |
| 15 s (cold) | 1.7B | 74 s | 4.9x | 2.7 MB |
| 30 s (warm) | 1.7B | 12 s | 0.4x | 5.8 MB |

Peak VRAM **17.3 GB** with the 1.7B LM (13.4 GB with the 0.6B), against a
4.9 GB host baseline; the stanza books 17 GB of the 24 GB card and cannot be
resident beside the video stanza. Durations accepted 10-600 s; 30 s and
180 s verified end to end through `:18080`.
