# Local video generation backends

Date of reading: 2026-11-20. Part of #64, issue #83. Feeds the async job
design in #69.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link. A fact that could not be confirmed from the
project's own pages is marked `(verify)`, and anything only the GPU host can
settle is in the checklist (§8).

## 0. Scope and method

The question: which backend should this router start, health-check, place,
evict and proxy for video generation (text to video, image to video, first and
last frame, video to video or extension, and joint audio-video), and which job
API the router should standardise on before #69 builds an async job layer. The
cards are the GPU host's one 24 GB sm_86 card (GPU 0) and two 32 GB sm_70 cards
(GPU 1 and 2).

**How the facts were gathered.** Page fetches were not available in this run
(web search only, 36 queries). Every external fact comes from search-engine
extracts of the cited pages, read on 2026-11-20, not from opening the files.
Field names, flags and defaults that matter for the stanza are marked `(verify)`
and repeated in the checklist. Speed figures are mostly third-party or derived;
each says which.

**Baseline already in this repo.** The 24 GB image node's config in `configs/`
runs LTX-2.3 (22B distilled, Q5_K_M) and MiniMax-H3 (Q4_K_M, text to
audio-video) through `sd-server`, booked at `vram_mb: 22000`,
`idle_ttl_seconds: 0`, `api_type: image`, `match: {path_prefix: /sdapi/v1}`.
That label is only UI metadata. The stanzas themselves are the right shape;
§1 keeps `sd-server` as the main path and §6 relabels them `video`.

**Related work, linked not redone.**

- The landscape note ([`oss-model-router-landscape.md`](oss-model-router-landscape.md))
  says "skip a job-polling API" and hold the connection instead. That verdict is
  about chat and slow spin-up. Video is the exception: the generation itself
  runs for minutes and the recommended backend only offers an async job API
  (§3), so a held connection is not available. This is the case #69 exists for.
- The sibling content project's F2 (camera motion and selective image to video,
  its issue 27) and G4 (compute and hosting plan, its issue 32) notes had not
  landed as of this reading `(verify)`; this run cannot reach the issue
  tracker. Their image-to-video need is met by `init_image` (plus `end_image`
  for first and last frame) on LTX-2.3, Wan 2.2 and MiniMax-H3 FL2VA (§3).
  Link those notes here when they land.

**Router facts this note relies on.** Read from this repo, not assumed. A Go
toolchain run is in §8 item 10; the behaviour below comes from reading the code.

- `api_type: video` already exists and maps `/v1/videos` to the video stanza
  marked `path_default`, or the only one (`pathAPITypes`,
  `internal/router/matcher.go`). `/v1/videos/{id}` and `/v1/videos/{id}/content`
  match the same prefix.
- `cmd/fake-model` already serves the OpenAI-style job API: `POST /v1/videos`,
  `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`,
  `DELETE /v1/videos/{id}`. Its ids are sequential per process.
- **Occupancy lasts one HTTP request.** `proxyLocal` takes an occupancy slot,
  proxies, and releases it when the response ends
  (`internal/router/router.go`). A job that keeps running after its `202` is
  invisible to the idle sweep and to eviction.
- **A poll with no model goes to the type default**, not to the instance that
  holds the job. A poll that reaches a stopped model starts it first.
- `/sdcpp/v1/*` is not in the passthrough list (`internal/router/server.go`).
  It reaches a backend only through `/`, which needs `?model=` in the query, or
  through `/upstream/{model_id}/...`, which strips the prefix, proxies any
  method to a local model, and loads it if needed.
- **Request bodies are read whole into memory**, once by the matcher
  (`readBody`) and again by the Activity capture (`wrapProxy`). There is no size
  cap. Responses stream; Activity keeps at most 256 KB of each.
- The HTTP server has no `WriteTimeout`, so a long content download is not cut
  off.
- Shutdown unloads every local model (`SPEC.md`), so a router restart kills
  every backend and every job in it.
- `pin` is a stanza field in `internal/config/config.go` but is not documented
  in `SPEC.md`, so the proposed stanzas do not use it.

## 1. Recommendation

**Primary: `stable-diffusion.cpp` `sd-server`, one stanza per video model,
using its native async job API (`POST /sdcpp/v1/vid_gen`, poll
`GET /sdcpp/v1/jobs/{id}`, `POST /sdcpp/v1/jobs/{id}/cancel`).** It runs on all
three cards: the GPU host's `sd-server` build already targets sm_70 and sm_86
(checked with `cuobjdump`, per the node config comment), it uses GGUF weights
and fp16 or fp32 compute, and its flash attention is ggml's own, not
FlashAttention 2. It covers the families that matter now: LTX-2.3 (and 2.5)
with joint audio, MiniMax-H3 (text, first/last frame and reference to
audio-video), Wan 2.1 and 2.2 (text, image, first/last frame, VACE video to
video) and HunyuanVideo 1.5. It is the same binary family the router already
runs for images, and the other image node already runs LTX-2.3 and MiniMax-H3
on it. Licence: MIT.

**Fallback: ComfyUI, headless (`main.py --listen 127.0.0.1 --port {port}`),
with one saved API-format workflow per model, on the 24 GB card (GPU 0) only.**
It runs every open video model the day it ships (Wan, LTX, HunyuanVideo, Mochi,
CogVideoX, SkyReels, FramePack through a wrapper), with the best offload and
tiling for 24 GB. It does not run on the V100s with the host's existing
cu130 PyTorch, and some of its newer CUDA paths need cu130 (sd.cpp issue #2370
reports MiniMax-H3 in ComfyUI failing on a V100 for that reason), so it is not
proposed for GPU 1 or 2. Licence: GPL-3.0. It needs a thin adapter before it
fits the router's job API (§5.4).

**The rule for choosing:**

1. A video model that `sd-server` can load runs on `sd-server`. That is
   LTX-2.x, MiniMax-H3, Wan 2.x and HunyuanVideo 1.5 today.
2. ComfyUI is used only for a model or feature `sd-server` does not have:
   FramePack, SkyReels V3 reference-to-video and audio-driven avatar, Mochi,
   CogVideoX, LTX's IC-LoRA or keyframe nodes. It runs on GPU 0 only.
3. The router never switches between them on its own. They take different
   request fields and give different outputs, so a model id belongs to one
   backend.

### The six questions, answered

1. **Job API and front door.** Every serious backend is async (§2). Yes, the
   OpenAI-style `/v1/videos` job API (create, retrieve, content, delete) is the
   right front door: the router already maps it (`api_type: video`), the test
   backend already speaks it, and vLLM-Omni and SGLang Diffusion serve it
   natively. OpenAI itself retired its hosted Videos API on 2026-09-24
   (third-party reports quoting its deprecations page `(verify)`), so the shape
   is now a de facto standard kept alive by those servers and the OpenAI SDKs,
   not a live hosted product. **Recommendation for #69:** the router speaks only
   `/v1/videos` to clients and to backends, and does the generic job work
   itself (§5.3): record job id → instance on create, hold an occupancy slot
   until the job ends, route polls by id, answer `404` for unknown ids.
   Backends that speak something else (`sd-server`, ComfyUI) get a small
   adapter process in the stanza `command`, the same pattern as the host's
   existing `sd-router-shim` wrapper. The translation each adapter does is in
   §5.4. The router does not learn backend-specific job shapes.
2. **VRAM per card** is in §4. In short: on the 24 GB card, LTX-2.3 Q5_K_M,
   Wan 2.2 (A14B at Q4-Q8 or TI2V-5B), HunyuanVideo 1.5 and MiniMax-H3 Q4_K_M
   all run with `--offload-to-cpu`, `--vae-tiling` and `--temporal-tiling`. On
   a 32 GB V100, the same GGUF files run in fp16 with ggml flash attention;
   upstream has measured MiniMax-H3 notes for that exact card (sd.cpp PR #2234).
   Expect tens of seconds to several minutes of wall time per second of video.
   fp8 matters only for ComfyUI on the 3090, and only as a storage format.
3. **A 22 GB LTX-class model coexists with nothing much.** On GPU 0 it needs
   22000 of about 24 GB while the intent, embedding and reranker models book
   6600, so it evicts any image model there plus the embedding model and one of
   the 1.5 GB residents. On a V100 it evicts the large chat model, which holds
   both V100s (about 7 minutes to reload). A `--max-vram` cap may let it sit
   beside the trio on GPU 0 at some speed cost; that is unmeasured `(verify)`
   (§4.3).
4. **Input.** `sd-server` takes JSON with images as raw base64 or data URLs
   (`init_image`, `end_image`, `ref_images`, `control_frames`). There is no
   multipart and no documented size limit. ComfyUI takes files through
   multipart `POST /upload/image` first. OpenAI-style servers take multipart
   with an `input_reference` file. The router buffers every request body whole,
   twice, with no cap (§0).
5. **Output.** `sd-server` writes WebM (default), animated WebP or MJPEG AVI
   with its own writers, no ffmpeg. With an audio track it muxes raw PCM into
   WebM, which browsers drop (issue #2233), so audio outputs need an ffmpeg
   remux. The file comes back base64 inside the job JSON, not as a stream or a
   content endpoint. ComfyUI writes MP4 (with audio when the workflow uses
   `CreateVideo`) to its `output/` folder and serves it from `/view`.
6. **Survival.** Both keep their queue and results in process memory only.
   `sd-server` drops a finished result after `--completed-ttl-seconds`
   (default 600). ComfyUI's queue and history are lost on restart; output files
   stay on disk. A router restart kills every backend, so **no job survives a
   router restart and the router cannot recover a job id afterwards.** #69
   should answer `404` for an unknown id and let the client resubmit. Do not
   build persistence.

## 2. Alternatives

`3090` and `V100` say whether the serving runtime runs on sm_86 or sm_70 at all,
and the expected VRAM for a 720p-class clip. Licence is code / weights.

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 (sm_86) | V100 (sm_70) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| stable-diffusion.cpp `sd-server` (Wan 2.1/2.2, LTX-2.3/2.5, MiniMax-H3, HunyuanVideo 1.5) | https://github.com/leejet/stable-diffusion.cpp | MIT / per model | `master-1106-532d2c1`, 2026-11-16 | own JSON job API `/sdcpp/v1/vid_gen`; OpenAI images and A1111 routes for stills only | async (`202` + poll) | no; result is base64 in the job JSON; per-pass progress in the job record | yes; GGUF with offload and tiling, booked 22000 MB for LTX-2.3 or MiniMax-H3 on the other 24 GB node | yes (host build has sm_70); MiniMax-H3 measured upstream on a V100 32 GB (PR #2234) | **primary** |
| ComfyUI (native Wan, LTX, Hunyuan, Mochi, CogVideoX nodes; GGUF via ComfyUI-GGUF) | https://github.com/Comfy-Org/ComfyUI | GPL-3.0 / per model | v0.43.0, 2026-10-27 | own: `POST /prompt`, `GET /history/{id}`, `GET /view`, `/ws` | async queue | websocket progress; file on disk via `/view` | yes; widest 24 GB coverage (LTX-2.3 GGUF Q6_K for 24 GB, Wan 2.2 14B ~14-16 GB at Q8) | only with a cu126 torch venv, and not for every model (sd.cpp issue #2370) `(verify)` | **fallback**, GPU 0 only |
| vLLM-Omni (Wan 2.2, LTX-2, LTX-2.5) | https://github.com/vllm-project/vllm-omni | Apache-2.0 / per model | 0.32.0 on PyPI `(verify)` date | OpenAI-style `/v1/videos` create, retrieve, list, content, delete; plus `/v1/videos/sync` | async (and a sync route) | content endpoint | Wan 2.2 TI2V-5B fits; 14B and LTX-2 at bf16 do not without offload `(verify)` | **no** (needs compute capability 7.5+) | skip for now; it is the reference shape for the front door |
| SGLang Diffusion (Wan 2.1/2.2, HunyuanVideo) | https://github.com/sgl-project/sglang | Apache-2.0 / per model | active `(verify)` | OpenAI-style `/v1/videos` subset (create JSON or multipart, list, retrieve, content) | async | content endpoint (MP4) | yes, small Wan models; 14B needs offload `(verify)` | not confirmed `(verify)` | skip: same niche as vLLM-Omni, no sm_70 evidence |
| Wan 2.2 official (`generate.py`) and Diffusers pipelines | https://github.com/Wan-Video/Wan2.2 | Apache-2.0 / Apache-2.0 | repo updated 2026-09-21; no GitHub releases | scripts; no HTTP server | — | — | TI2V-5B "at least 24 GB" with `--offload_model --convert_model_dtype --t5_cpu`; one 4090 run OOMed at the VAE decode | fp16 with cu126 torch `(verify)` | skip as a server; run Wan through `sd-server` |
| LTX-2 official (`ltx-pipelines`, trainer) | https://github.com/Lightricks/LTX-2 | Apache-2.0 code (from v1.6.0) / LTX-2 Community Licence (2.3), LTX-2.x Community Licence (2.5) | v1.6.0 `(verify)` date | Python package; no HTTP server found | — | — | official: 32 GB at bf16, 24 GB with sequential offload | bf16-first `(verify)` | skip as a server; run LTX through `sd-server` |
| HunyuanVideo 1.5 official | https://github.com/Tencent-Hunyuan/HunyuanVideo-1.5 | Tencent Hunyuan Community Licence | 2025-11-20 release, active | scripts and Diffusers; no server | — | — | 14 GB minimum with offload (official) | `(verify)` | skip as a server; run it through `sd-server` |
| MiniMax-H3 official | https://github.com/MiniMax-AI/MiniMax-H3 | MiniMax H3 Community Licence (excludes US, EU, UK, South Korea) | weights 2026-08-02 | scripts | — | — | Q4_K_M via `sd-server` (other node) | via `sd-server` (PR #2234) | run through `sd-server` |
| FramePack (HunyuanVideo-based I2V, F1) | https://github.com/lllyasviel/FramePack | Apache-2.0 | F1 2025-05-03; P1 preview 2025-06-26 | Gradio demo only | — | — | 6 GB minimum, constant with length | **no**: needs fp16 and bf16, RTX 30xx and up | skip: image to video only, no audio, no API; ComfyUI wrapper exists |
| SkyReels V2 / V3 | https://github.com/SkyworkAI/SkyReels-V2, https://github.com/SkyworkAI/SkyReels-V3 | `(verify)` | V3 weights 2026-01-29 | scripts | — | — | 14B bf16, GGUF exists; needs offload | `(verify)` | skip; reach it through ComfyUI if reference-to-video or avatar is needed |
| WanGP (Wan2GP) | https://github.com/deepbeepmeep/WanGP | `(verify)` | v20.00, 2026-11-12 | Gradio app, headless batch from a saved queue, Python API | own queue | — | yes, very low VRAM (H3 15 s 1080p in 11 GB, per its notes) | `(verify)` | skip: no HTTP job API; a good place to borrow low-VRAM settings |
| LocalAI `/video` | https://github.com/mudler/LocalAI | MIT / per model | active | own `POST /video` JSON; backends include `stablediffusion` and `vllm-omni` | sync | file in the response | as its backend | CUDA 12 image only (per the realtime note) | skip: a second router inside a stanza |
| Mochi 1, CogVideoX 1.5 | https://github.com/genmoai/mochi `(verify)`, https://github.com/zai-org/CogVideo `(verify)` | Apache-2.0; CogVideoX-5B under its own licence | no release since 2024 | scripts | — | — | Mochi ~20 GB at fp8; CogVideoX-5B fits 24 GB | `(verify)` | skip: superseded; ComfyUI if ever needed |
| stable-diffusion.cpp-restapi (third party) | https://github.com/fszontagh/stable-diffusion.cpp-restapi | `(verify)` | `(verify)` | own `POST /txt2vid`, `GET /queue/{id}`, `/ws` | async, **persistent** queue | websocket | as sd.cpp | as sd.cpp | skip: not upstream; noted because it is the only one with a persistent queue |

More than six candidates exist. Only two kinds of backend fit this host and
this router: `sd-server`, which runs every card and the models that matter, and
the PyTorch servers (ComfyUI, vLLM-Omni, SGLang), which are 3090-only in
practice. Of those, ComfyUI covers the most models within 24 GB.

## 3. API surface of the recommended backend (`sd-server`)

Source: `examples/server/api.md` in the stable-diffusion.cpp repo, plus PRs
#1884 and #2303 (progress) and two client projects. Field lists come from
search extracts, so the shapes below are `(verify)` against the installed build
(checklist items 2 and 5).

### Endpoints the router must proxy

| Method and path | Purpose | Request | Response |
| --- | --- | --- | --- |
| `POST /sdcpp/v1/vid_gen` | queue one video job | `application/json` (below) | `202` with the job record: `id` (`job_` + a ULID-like string), `status: "queued"`, `poll_url` |
| `GET /sdcpp/v1/jobs/{id}` | status, progress and result | — | job record (below) |
| `POST /sdcpp/v1/jobs/{id}/cancel` | cancel | — | job record; a client project reports `cancel_queued: true`, `cancel_generating: false`, so a running job finishes `(verify)` |
| `GET /sdcpp/v1/capabilities` | features, samplers, schedulers, the loaded model, recipe defaults | — | JSON |
| `POST /sdcpp/v1/img_gen`, `POST /sdcpp/v1/upscale` | stills and upscaling | JSON | job record; not needed for video |
| `/sdapi/v1/*`, `/v1/images/*` | A1111 and OpenAI image routes (sync) | — | stills only; not video |
| health | readiness | — | `GET /sdapi/v1/sd-models` answers `200` once the model is loaded (the other node's stanzas use it) |

There is no model switch the router needs: one process serves one model, and
the router unloads by killing the process group. Recent builds refuse to load
or unload a model while jobs are pending `(verify)`; the router never asks.

### Create request (`vid_gen`)

JSON. The same base fields as `img_gen` (`prompt`, `negative_prompt`, `width`,
`height`, `seed`, sampling parameters, `init_image`) plus:

- `video_frames` and `fps`. **Always send both.** Without `video_frames` a
  custom model falls back to 1 frame, a still (Lemonade PR #3506). Frame counts
  snap to each family's grid: 4k+1 for LTX-2, 17k+5 (minimum 5) for MiniMax-H3.
- `init_image` and `end_image`: first and last frame (image to video, first/last
  frame control). Raw base64 or a `data:image/...;base64,` URL.
- `control_frames` and `vace_strength`: video to video (Wan VACE) as frames, not
  as a video file `(verify)` exact shape.
- `ref_images`: MiniMax-H3 Ref2VA reference images; other families ignore them.
- `high_noise_sample_params`, `moe_boundary`: Wan 2.2 A14B's two experts.
- `output_format`: `webm` (default; needs a build with WebM support, else
  `400`), `webp` (animated), `avi` (MJPEG). `output_compression`.
- `ref_videos`, `ref_audios`, `ref_video_audios` (video and audio references for
  MiniMax-H3): PR #2194, merge status unknown `(verify)`.

No `batch_count`: one job is one clip. An unknown `model` field in the body is
probably ignored `(verify)` (checklist item 5); it matters only if a client
sends one through `/` with `?model=`.

### Job record

- `status`: `queued`, `generating`, `completed`, `failed`, `cancelled`.
- `queue_position`.
- `progress`: `stage`, `detail`, `pass`, `completed`, `total`, plus
  `stage_elapsed_ms` and `elapsed_ms` (queue time excluded). The ratio is the
  **current pass**, not the whole job (PR #2303).
- `completed`: a millisecond timestamp, or `null`.
- `result`: `null` until completed. For video, one object with `kind: "vid_gen"`,
  `mime_type` (for example `video/webm`), `b64_json` (the **whole container
  file**, base64), `output_format`, `frame_count`, `fps`.

### Errors

Failed and cancelled jobs have `result: null`. Create-time errors seen by
client projects: `400` when WebM is not built in, "loaded model does not support
vid_gen", and `queue_full` when `--max-pending-jobs` is reached. The HTTP status
for `queue_full` is `(verify)`.

### Sizes, sync, streaming, queue

- **Sizes:** no documented limit. The server is cpp-httplib, so any cap is
  httplib's payload setting `(verify)`. Base64 adds a third: a 30 MB result is
  about 40 MB of JSON, sent again on every poll after completion.
- **Sync vs async:** video is async only. There is no held-connection video
  route.
- **Streaming:** none. No websocket, no SSE. Previews, when enabled, update in
  the job record.
- **Queue:** in process memory. `--max-pending-jobs` (default 64) caps queued
  plus generating jobs; `--completed-ttl-seconds` (default 600) is how long a
  finished job and its result are kept. Jobs run one at a time `(verify)`.

## 4. Hardware fit

### 4.1 Current occupancy (from the GPU host's node config)

- **24 GB card (GPU 0):** intent model 1500 MB, embedding model 3600 MB,
  reranker 1500 MB, all resident (`idle_ttl_seconds: 0`): 6600 MB booked, about
  17.9 GB left. Image stanzas (12000 to 22000 MB, `device: "0|1|2"`,
  `idle_ttl_seconds: 300`) prefer this card.
- **32 GB cards (GPU 1 and 2):** the large chat model is split across both
  (`device: "1,2"`, `vram_mb: 64000`, resident, unpinned, about a 7-minute cold
  start). **Any** new load on `"1|2"` evicts it.
- The issue asks about sitting beside the large chat model and the embedding
  model. Those are on different cards: the embedding model on GPU 0, the large
  chat model on GPU 1 and 2.

### 4.2 Per model, per card (sd-server, GGUF)

"Book" is the proposed `vram_mb`. None of these are measured on this host; the
peak to book is checklist items 5 to 7.

| Model | Modes | 3090 (24 GB, sm_86) | V100 (32 GB, sm_70) |
| --- | --- | --- | --- |
| LTX-2.3 22B distilled Q5_K_M, 12B text encoder Q4, video and audio VAEs | text or image to audio-video, 8 steps | book 22000 (the other 24 GB node's figure); 720p with `--offload-to-cpu --vae-tiling --temporal-tiling` | runs, fp16, ggml flash attention; book 24000 `(verify)` |
| MiniMax-H3 33B Q4_K_M, Qwen3-VL-32B Q4_K_M text encoder | text, first/last frame, reference to audio-video | book 22000 (other node) at 1280x720 with offload | upstream V100 notes: without `--diffusion-fa` it OOMs at 1024x576 above 39 frames; with `--diffusion-fa --offload-to-cpu --mmap` it runs 124+ frames at 0.59 MP and 209 frames at 1344x768; about 85 MiB per frame at 1344x768. Book 30000 |
| Wan 2.2 A14B (two experts) Q4_K_M to Q8_0 | text or image to video, first/last frame | about 14-16 GB at Q8 (ComfyUI figures, third party) | fits |
| Wan 2.2 TI2V-5B | text or image to video, 720p 24 fps | fits at Q8 `(verify)`; the official fp16 script peaked at 24037 of 24564 MiB on a 4090 and OOMed at decode | fits |
| HunyuanVideo 1.5 8.3B | text or image to video, 480p and 720p | 14 GB minimum with offload (official) | fits |

**Resident vs peak.** With `--offload-to-cpu` the weights live in host RAM and
move to the card block by block, so idle residency is small and the peak comes
during sampling and the VAE decode. Booking the peak is what the ledger needs.
`--mmap` keeps load time and RAM down. Host RAM (157 GiB) is ample for two
offloaded video models.

**Quantisation.** GGUF Q4_K_M to Q8_0 everywhere in `sd-server`; no fp8 path.
fp8 matters only for ComfyUI on the 3090, and only as storage: Ampere has no
fp8 tensor cores, so fp8 weights are upcast per layer and save VRAM, not time.
LTX's MXFP8 file is the variant guides suggest for a 3090. Avoid fp8 on the
V100s.

**fp16 fallback on sm_70.** `sd-server` computes in fp16 and fp32 on CUDA, so
bf16 is not needed. A bf16 safetensors VAE may be converted on load or may fail
on Volta `(verify)` (checklist item 7). PyTorch servers need a cu126 build for
sm_70 (cu128 from 2.11 and the host's cu130 drop Volta), then fp16, then a
non-FlashAttention-2 attention path. ComfyUI's community V100 notes show it is
possible but slow (fp32 by default on V100).

**Time per second of video** (wall time; all estimates or third-party):

| Model and setting | 3090 | V100 |
| --- | --- | --- |
| LTX-2.3 distilled GGUF, ~1280x736, 8 steps | ~22 s per video second (337 s for 15 s in ComfyUI, third party); `sd-server` unmeasured | slower; unmeasured |
| Wan 2.2 A14B + 4-step distill LoRA, 480p, 81 frames | ~20-40 s per video second (estimate from 4090 and 16 GB figures) | unmeasured |
| Wan 2.2 TI2V-5B, 720p, 50 steps | ~200-260 s per video second (9-11 min per 5 s on a 4090, about twice that on a 3090) | unmeasured |
| HunyuanVideo 1.5 step-distilled 480p | ~30 s per video second (75 s per clip on a 4090, about twice on a 3090) | unmeasured |
| MiniMax-H3 Q4_K_M | unmeasured; a 16 GB card took ~6.6 min per clip (issue #1865) | measured table in PR #2234 `(verify)` values |

A second V100 does not speed up one job: a layer split runs sequentially
(PR #2234).

**CPU-only.** `sd-server` has a CPU backend, but a 14B-33B video model on CPU
takes hours per clip (estimate, no source). Not proposed.

**Cold start.** The other node uses `spin_up_seconds: 120`. Files are about
16 GB (LTX-2.3 Q5_K_M) plus 7 GB (text encoder), or about 40 GB for MiniMax-H3.
Estimate 60-180 s from SSD with `--mmap` `(verify)` (checklist item 5).

### 4.3 Can a 22 GB LTX-class model coexist?

- **On GPU 0:** 22000 + 6600 = 28600 MB, more than the card. Eviction takes idle
  image models first, then residents: the embedding model (3600) and one of the
  intent or reranker models (1500). The embedding model comes back by preload
  restore when the video model stops; the other comes back on its next request
  (seconds). While a video job runs, embedding requests wait for a cold start.
- **On a V100:** it evicts the large chat model (about 7 minutes to reload).
  With that gone, a V100 has room for the video model plus a ~10 GB image model.
- **With a cap:** `--max-vram 16` (GiB) with `--offload-to-cpu` should keep the
  peak near 17.5 GB, which fits beside the trio. The upstream MiniMax-H3 doc
  says the active block's working memory must also fit in that budget, and
  issue #1865 reports wrong output from MiniMax-H3 under segmented offload. Not
  proposed until checklist item 6 shows a capped LTX-2.3 run gives the same
  clip and an acceptable speed. Then book `vram_mb: 17500`.

## 5. How the router would run it

### 5.1 Spawn

```
/home/kevyn/infra/stable-diffusion.cpp/build/bin/sd-server
  --diffusion-model <model>.gguf --vae <video vae> --audio-vae <audio vae>
  --llm <text encoder>.gguf --embeddings-connectors <connectors>
  --diffusion-fa --offload-to-cpu --vae-tiling --temporal-tiling --mmap
  --max-pending-jobs 2 --completed-ttl-seconds 900
  --threads 14
  -l 127.0.0.1 --listen-port {port}
```

- Foreground process, one model per process; the process-group kill covers it.
- `{port}` through `--listen-port`; `{gpu}` through `CUDA_VISIBLE_DEVICES`, as
  on the host's image stanzas (`CUDA_DEVICE_ORDER=PCI_BUS_ID`,
  `GGML_CUDA_PDL=0`).
- `--max-pending-jobs 2`: the router's `slots` cannot bound async jobs (§5.5),
  so the backend's own queue is the cap. Two keeps a forgotten batch from
  queuing an hour of work. Both flag names come from search extracts of
  `api.md` `(verify)` (checklist item 2).
- `--completed-ttl-seconds 900` matches `idle_ttl_seconds` so a result outlives
  the idle window.
- The binary needs WebM support built in and must include the async job API and
  PR #2303 progress (checklist item 2). The host's `sd-router-shim` wrapper is
  not used: whether it forwards `/sdcpp/v1/*` is unknown (checklist item 3).

### 5.2 Stanza fields

| Field | Value | Why |
| --- | --- | --- |
| `health_check` | `/sdapi/v1/sd-models` | `200` once the model is loaded; the other node's video stanzas use it |
| `spin_up_seconds` | `180` (3090), `300` (V100, larger files) | estimates; the other node uses 120 for the same LTX-2.3 files `(verify)` (checklist item 5) |
| `vram_mb` | `22000` (LTX-2.3, GPU 0), `30000` (MiniMax-H3, V100) | §4.2; replace with measured peaks |
| `idle_ttl_seconds` | `900` | not resident: 22 GB on GPU 0 would starve the image stanzas. The other node uses `0`, which only protects a running job from the idle sweep, not from eviction |
| `slots` | `1` | the honest value for one generation at a time; it does not bound async jobs (§5.5) |
| `match` | `{path_default: true}` on the GPU 0 stanza, `{body_field: model}` on the other | after #69, a model-less `POST /v1/videos` lands on the GPU 0 stanza; at most one video default per node |
| `api_type` | `video` | correct label; routes `/v1/videos` once a backend speaks it |
| `capabilities` | `[audio_out]` | both models generate sound. `vision` would describe image input loosely; left out |

### 5.3 What #69 has to add (generic, no backend shapes)

Each item is small and lives in the router. No new stanza key is needed.

1. **Record the job.** On a `2xx` from `POST /v1/videos`, read `id` from the
   JSON response and keep `id → target` (local instance key, or peer) in memory.
2. **Hold the instance while the job runs.** Take one occupancy slot at create
   and release it when the job ends. The router polls the backend's
   `GET /v1/videos/{id}` itself every few seconds; `completed`, `failed`, a
   `404` or a fixed maximum job time ends the hold. The idle sweep and eviction
   already skip anything with occupancy, so a running job is safe and the idle
   clock starts when the job ends. `idle_ttl_seconds` becomes the window for
   the client to fetch the content.
3. **Route by id.** `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content` and
   `DELETE /v1/videos/{id}` go to the recorded target. **An unknown id is a
   `404` from the router**, never a type-default route and never a cold start.
   A `DELETE` that ends the job releases the hold.
4. **Peers.** A job created on a peer is recorded with that peer as target;
   polls follow it with `X-Model-Router-Peer` as today.
5. **No id rewrite.** Every recommended backend makes globally unique ids
   (`sd-server` `job_` + ULID, ComfyUI UUIDv4, vLLM-Omni `(verify)`), so the
   router passes them through. `fake-model`'s sequential ids would collide
   across two instances; give it random ids in #69's tests.
6. **No persistence.** A router restart kills the backends, so the table and the
   jobs go together. A client polling an old id gets `404` and resubmits.
7. **Optional, a router change:** a body cap on `/v1/videos` creates, so a
   large upload is refused before it is buffered twice. Not needed to ship.

### 5.4 What the adapter has to translate

The adapter is a small process started by the stanza `command` in front of the
backend (`video-adapter {port} -- sd-server ...`), the same pattern as the
host's `sd-router-shim`. It serves `/v1/videos` on `{port}` and forwards to the
backend on a private port. It holds no state of its own.

**For `sd-server`:**

| `/v1/videos` | `sd-server` |
| --- | --- |
| `POST /v1/videos` (JSON or multipart): `prompt`, `size` (`WxH`), `seconds`, `input_reference` file; extensions `num_frames`, `fps`, `seed`, `negative_prompt`, `end_image`, `num_inference_steps` (the vLLM-Omni names) | `POST /sdcpp/v1/vid_gen` JSON: `width`, `height`, `video_frames` (from `num_frames`, or `seconds × fps`), `fps`, `init_image` (file to base64), `end_image`, sampling steps. Drop `model` |
| response `{id, object: "video", model, status, progress, created_at, size, seconds}` | from the job record |
| status `queued`, `in_progress`, `completed`, `failed` | `queued`; `generating` → `in_progress`; `completed`; `failed`; `cancelled` → `failed` with an error code `cancelled` (the OpenAI shape has no cancelled state `(verify)`) |
| `progress` 0-100 | `progress.completed / progress.total` of the current pass; approximate. Never return `b64_json` here |
| `GET /v1/videos/{id}/content` | decode `result.b64_json`, `Content-Type` from `mime_type`. When the clip has audio, remux with ffmpeg to MP4 (H.264 copy is not possible from VP8/VP9, so `-c:v libx264 -c:a aac`) or to WebM with Opus, because of issue #2233 |
| `DELETE /v1/videos/{id}` | `POST /sdcpp/v1/jobs/{id}/cancel`. A running job cannot be cancelled `(verify)`; answer `409` then. A finished job is purged by the TTL |
| `GET /health` | `GET /sdapi/v1/sd-models` |

**For ComfyUI** (the fallback; more work, which is why it is the fallback):

| `/v1/videos` | ComfyUI |
| --- | --- |
| `POST /v1/videos` | load the model's API-format workflow template; upload `input_reference` with multipart `POST /upload/image`; fill the prompt, seed, size, frame count and image name into the template's node inputs; `POST /prompt` with `{prompt, client_id, prompt_id}` where the adapter sets `prompt_id` to a fresh UUIDv4 (ComfyUI accepts a client-supplied one and validates it). That UUID is the job id |
| status | `GET /history/{id}`: present means finished (`status.status_str` `success` or `error`). Absent: look in `GET /queue` (`queue_running` → `in_progress`, `queue_pending` → `queued`); in neither means lost → `404` |
| `progress` | only from websocket `progress` events (`value`, `max`) on `/ws?clientId=`. The adapter keeps one websocket open, or reports no progress |
| content | from the history entry's `SaveVideo` output `{filename, subfolder, type}`, stream `GET /view?filename=&subfolder=&type=output` |
| delete | queued: `POST /queue {"delete": [id]}`. Running: `POST /interrupt`, which stops **whatever is running**, so check the running id first. Finished: `POST /history {"delete": [id]}`; the file stays in `output/` |
| health | `GET /system_stats` `(verify)` |

### 5.5 What breaks if the router only passes bytes through

Today, with no #69 and no adapter, a client can drive the native API through
the router: `POST /upstream/<model_id>/sdcpp/v1/vid_gen`, then poll
`GET /upstream/<model_id>/sdcpp/v1/jobs/<id>` (or the bare paths with
`?model=<model_id>` on every call). That works for a careful client on one node.
Plainly, what breaks:

- **A running job can be killed.** The router counts the job only while a
  request is open. Another model's load can evict the video model between polls
  (it is "idle"), and a client that submits and walks away lets the idle sweep
  unload it after `idle_ttl_seconds`. Either way the job and its result are
  gone. `idle_ttl_seconds: 0` (as on the other node) stops the sweep but not
  eviction.
- **Polls go to the wrong place.** `GET /v1/videos/{id}` and
  `/sdcpp/v1/jobs/{id}` carry no model. The first goes to the video default;
  the second is a `404` from the router unless `?model=` is added. With two
  video stanzas, or a job on a peer, a model-less poll reaches the wrong
  backend. `/upstream/` is local-only, so a peer job cannot be polled that way.
- **A poll can cold-start a model** just to get `404` for a job that died with
  the old process.
- **`slots` means nothing.** Each create returns `202` at once, so the router
  sees short requests and never queues. The backend's `--max-pending-jobs` is
  the only cap. Over-queuing shows up as the backend's `queue_full` error.
- **`/v1/videos` does not work at all against `sd-server`**: it has no such
  route, so a model-less `POST /v1/videos` reaches it and gets `404`.
- **Large bodies.** Base64 reference images or frames are buffered whole, twice,
  with no cap. Every poll after completion re-sends the whole clip as base64.
- **Audio.** A WebM with audio plays silent in browsers (issue #2233) until it
  is remuxed.
- **Activity capture** keeps 256 KB of each body: a create shows its fields, a
  completed poll shows a truncated base64 blob, and there is no thumbnail.

The router rewrites no path, field or header for the stanzas in §6. Every item
above is fixed by #69 (§5.3) plus the adapter (§5.4), not by rewriting.

## 6. Proposed stanzas

Paths are placeholders under `/opt/ai/models/video/` (the model files are on the
other node today; copy them, checklist item 4). Ports sit above the speech
notes' `5930-5937`; check `reserved_ports` before deploying. Every key is a
documented field in the `SPEC.md` config block. **No new key is needed.**

These run the native API today, used through `/upstream/<model_id>/sdcpp/v1/...`
(§5.5). Once #69 and the adapter exist, the only change is the `command` prefix
(`/opt/ai/bin/video-adapter {port} --`, with `sd-server` moved to a private
port) and `health_check: /health`.

### 6.1 Primary, 24 GB card (sm_86): LTX-2.3

```yaml
  - model_id: ltx-2.3-video
    name: "LTX-2.3 22B distilled (text/image to audio-video, sd.cpp)"
    command: >
      /home/kevyn/infra/stable-diffusion.cpp/build/bin/sd-server
      --diffusion-model /opt/ai/models/video/ltx-2.3/ltx-2.3-22b-distilled-1.1-UD-Q5_K_M.gguf
      --vae /opt/ai/models/video/ltx-2.3/ltx-2.3-22b-distilled_video_vae.safetensors
      --audio-vae /opt/ai/models/video/ltx-2.3/ltx-2.3-22b-distilled_audio_vae.safetensors
      --llm /opt/ai/models/video/ltx-2.3/text-encoder-UD-Q4_K_XL.gguf
      --embeddings-connectors /opt/ai/models/video/ltx-2.3/ltx-2.3-22b-distilled_embeddings_connectors.safetensors
      --diffusion-fa
      --offload-to-cpu
      --vae-tiling
      --temporal-tiling
      --mmap
      --max-pending-jobs 2
      --completed-ttl-seconds 900
      --threads 14
      -l 127.0.0.1 --listen-port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0", "GGML_CUDA_PDL=0"]
    vram_mb: 22000
    device: 0
    spin_up_seconds: 180
    api_type: video
    capabilities: [audio_out]
    match: {path_default: true}
    health_check: /sdapi/v1/sd-models
    idle_ttl_seconds: 900
    slots: 1
    port: 5938
```

The V100 form of the same model is this stanza with `device: "1|2"`,
`CUDA_VISIBLE_DEVICES={gpu}` and `vram_mb: 24000`. One `model_id` per node, so
choose one; GPU 0 is the default because a V100 load evicts the large chat
model.

### 6.2 Primary, 32 GB cards (sm_70): MiniMax-H3

Viable, and upstream has measured it on this card. It evicts the large chat
model (about 7 minutes to reload), so it is for an agreed batch window, not for
casual use (open question 2).

```yaml
  - model_id: minimax-h3-video
    name: "MiniMax-H3 (text/frame/reference to audio-video, sd.cpp, V100)"
    command: >
      /home/kevyn/infra/stable-diffusion.cpp/build/bin/sd-server
      -M vid_gen
      --diffusion-model /opt/ai/models/video/minimax-h3/minimax_h3_fl2va-Q4_K_M.gguf
      --vae /opt/ai/models/video/minimax-h3/minimax_h3_video_vae_fp16.safetensors
      --audio-vae /opt/ai/models/video/minimax-h3/minimax_h3_audio_vae_fp32.safetensors
      --llm /opt/ai/models/video/minimax-h3/text-encoder-Q4_K_M.gguf
      --diffusion-fa
      --offload-to-cpu
      --vae-tiling
      --temporal-tiling
      --mmap
      --rng cpu
      --max-pending-jobs 2
      --completed-ttl-seconds 900
      --threads 14
      -l 127.0.0.1 --listen-port {port}
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES={gpu}", "GGML_CUDA_PDL=0"]
    vram_mb: 30000
    device: "1|2"
    spin_up_seconds: 300
    api_type: video
    capabilities: [audio_out]
    match: {body_field: model}
    health_check: /sdapi/v1/sd-models
    idle_ttl_seconds: 900
    slots: 1
    port: 5939
```

The text encoder file names are placeholders: LTX-2.3 uses a 12B
encoder and MiniMax-H3 the Qwen3-VL-32B encoder, both as GGUF. `--threads 14`
copies the host's image stanzas. The V100s sit on NUMA node 1 and the 3090 on
node 0 (host config); whether this stanza wants a `taskset` wrapper, as the
large chat model has, is open `(verify)`.

### 6.3 Fallback, 24 GB card only: ComfyUI

Not proposed as YAML until the adapter exists: without it the router can only
pass ComfyUI's own API through `/upstream/`, and every client would need the
workflow JSON. When it is built, the stanza is `command: /opt/ai/bin/video-adapter
{port} --comfyui <workflow dir> -- /opt/ai/venv-comfyui/bin/python main.py
--listen 127.0.0.1 --port <private port> --disable-auto-launch`, `device: 0`,
`api_type: video`, `health_check: /health`, sized per workflow.

## 7. Licence and risk flags

Internal use only, but these are the terms to know.

- **stable-diffusion.cpp:** MIT. **ComfyUI:** GPL-3.0. Calling it over HTTP
  from an adapter does not make the adapter a derivative; do not copy its code
  into this repo.
- **LTX-2.3 weights: LTX-2 Community Licence** (dated 2026-01-05). **LTX-2.5:
  LTX-2.x Community Licence** (2026-08-11, amended 2026-11-11). Free under USD
  10M annual revenue, counted across the whole corporate group; above that a
  paid licence is needed. The 2.x licence also bars use in a product that
  competes with Lightricks' own offerings. The exact revenue clause in the
  older LTX-2 licence was not seen `(verify)`. The text encoder has its own
  licence, separate from the LTX weights.
- **MiniMax-H3: MiniMax H3 Community Licence.** Royalty-free for research and
  commercial use, but it **does not apply in the US, EU, UK or South Korea**.
  Australia is not excluded. Flag it if anyone outside Australia uses the
  outputs or the service. The open release stops at 768p short side; 2K
  regeneration is platform-only.
- **HunyuanVideo 1.5: Tencent Hunyuan Community Licence.** Excludes the EU, UK
  and South Korea; a separate licence above 100M monthly active users; outputs
  may not be used to improve other models.
- **Wan 2.2:** Apache-2.0. **FramePack:** Apache-2.0. **Mochi 1:** Apache-2.0.
  **CogVideoX-5B:** its own licence with prohibited uses. **SkyReels**, **WanGP**:
  not confirmed `(verify)`.
- **Likeness and consent.** Image to video and reference to video animate real
  photos. Use only fictional or consenting subjects; the MiniMax-H3 exclusions
  exist because of likeness and content rules.
- **Availability.** A video job holds a 22-30 GB card for minutes. Until #69,
  a job is not protected from eviction (§5.5), and a V100 placement takes the
  large chat model down for about 7 minutes after the job.
- **Disk and memory.** Outputs are tens to hundreds of MB. `sd-server` keeps
  each finished clip in RAM, base64 in JSON, for the completed TTL. ComfyUI's
  `output/` folder grows until someone cleans it.
- **Maturity.** `sd-server`'s video API moves fast (progress reporting in
  PR #2303, reference video in PR #2194, the WebM audio bug #2233 open as of
  this reading). Pin a build and re-run checklist item 5 on every upgrade.

## 8. GPU host verification checklist and open questions

Run as the service user. Steps 1 to 4 are read-only. **Steps 5 to 8 change GPU
state and need an agreed window**: they start backends by hand, outside the
router, which does not count that memory in its ledger. Before each GPU step,
check the card's free memory and unload any router stanza on it; after it,
`kill` the process. Expect the unloaded models (and, on the V100s, the large
chat model) to be down until preload restore brings them back. Use a made-up
prompt and, for image to video, a generated test image saved as
`/tmp/fictional-frame.png`.

```bash
# 1. Cards and what holds them now.
nvidia-smi --query-gpu=index,name,compute_cap,memory.total,memory.used,memory.free --format=csv
curl -s http://127.0.0.1:18080/_router/status | head -c 2000
free -g; df -h /opt/ai

# 2. sd-server build: commit, async video API, queue flags, WebM, CUDA archs (need 70 and 86).
SD=/home/kevyn/infra/stable-diffusion.cpp
git -C $SD log -1 --format='%h %cd'
$SD/build/bin/sd-server --help 2>&1 | grep -E -- '--(max-pending-jobs|completed-ttl-seconds|temporal-tiling|audio-vae|embeddings-connectors|max-vram|listen-port|mmap)'
grep -E 'CMAKE_CUDA_ARCHITECTURES|WEBM|WEBP' $SD/build/CMakeCache.txt

# 3. The existing wrapper: what it does and whether it forwards /sdcpp/v1/*.
/opt/ai/bin/sd-router-shim --help 2>&1 | head -40
file /opt/ai/bin/sd-router-shim

# 4. Model files present on this host (copy from the other image node if not), and ffmpeg codecs for the remux.
ls -l /opt/ai/models/video/ltx-2.3/ /opt/ai/models/video/minimax-h3/
ffmpeg -hide_banner -encoders | grep -E 'libx264|aac|libopus'

# 5. GPU STATE CHANGE (GPU 0). Start LTX-2.3 by hand; time the cold start; run one job; record peak VRAM.
#    First: no image job running; unload every router stanza on GPU 0, confirm >= 22000 MiB free.
date +%s; CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=0 GGML_CUDA_PDL=0 $SD/build/bin/sd-server \
  <the flags from §6.1> -l 127.0.0.1 --listen-port 5998 &
until curl -sf -o /dev/null http://127.0.0.1:5998/sdapi/v1/sd-models; do sleep 2; done; date +%s   # spin_up_seconds
curl -s http://127.0.0.1:5998/sdcpp/v1/capabilities | head -c 3000
nvidia-smi --query-gpu=index,memory.used --format=csv -l 1 > /tmp/vram-ltx.csv &                    # peak for vram_mb
curl -s -X POST http://127.0.0.1:5998/sdcpp/v1/vid_gen -H 'Content-Type: application/json' \
  -d '{"model":"ignored","prompt":"a paper kite over a fictional salt lake at dusk, wind sound","width":768,"height":512,"video_frames":49,"fps":24,"seed":7}'
#    Note: status code (202?), id format, whether the "model" field is accepted.
curl -s http://127.0.0.1:5998/sdcpp/v1/jobs/<id> | python3 -c "import json,sys; d=json.load(sys.stdin); d.get('result') and d['result'].pop('b64_json',None); print(json.dumps(d,indent=1))"
#    Repeat until completed; note progress fields, wall time (time per second of video).
curl -s http://127.0.0.1:5998/sdcpp/v1/jobs/<id> | python3 -c "import json,sys,base64; r=json.load(sys.stdin)['result']; open('/tmp/ltx.'+r['output_format'],'wb').write(base64.b64decode(r['b64_json']))"
ffprobe -hide_banner /tmp/ltx.webm      # video codec; audio codec (pcm_s16le means issue #2233 still applies)
#    Also: submit 3 jobs (expect queue_full on the third with --max-pending-jobs 2; note the HTTP status),
#    cancel a queued one and a running one (does the running one stop?), and send one with
#    "init_image":"data:image/png;base64,..." from /tmp/fictional-frame.png and one with "end_image".

# 6. GPU STATE CHANGE (GPU 0). Same model with --max-vram 16: peak VRAM, wall time, and whether the clip
#    matches step 5 with the same seed (issue #1865 saw wrong output under segmented offload).

# 7. GPU STATE CHANGE (one V100; evicts the large chat model, agreed window only). MiniMax-H3 from §6.2
#    on port 5999 with CUDA_VISIBLE_DEVICES=1; same measurements as step 5 at 1280x720, 121 frames.
#    Also LTX-2.3 on the V100: does a bf16 VAE load on sm_70, and how much slower than GPU 0?

# 8. GPU STATE CHANGE. Through a staged router stanza (§6.1): native API via /upstream works today;
#    the bare poll without a model does not.
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18080/upstream/ltx-2.3-video/sdcpp/v1/vid_gen -H 'Content-Type: application/json' -d '{"prompt":"test","video_frames":9,"fps":8,"width":256,"height":256}'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/sdcpp/v1/jobs/<id>                     # expect 404
curl -s -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:18080/sdcpp/v1/jobs/<id>?model=ltx-2.3-video" # expect 200

# 9. ComfyUI fallback (only if open question 1 needs it): a venv with the host's torch, and the routes the adapter uses.
/opt/ai/venv-comfyui/bin/python -c "import torch; print(torch.__version__, torch.cuda.get_arch_list())"
#    After starting it on GPU 0: curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:<port>/system_stats
#    and the default for --max-upload-size in comfy/cli_args.py.

# 10. Repo check from a workstation with Go (code reading in §0): make check
```

### Open questions for Kevyn

1. **Which models first?** LTX-2.3 on GPU 0 covers text and image to
   audio-video; MiniMax-H3 adds first/last frame and reference to audio-video
   at a V100 cost. Is anything needed that only ComfyUI
   runs (FramePack, SkyReels avatar)? If not, the fallback is not built.
2. **Video on the V100s at all?** Every `"1|2"` placement evicts the large chat
   model for about 7 minutes. If not, drop §6.2 and keep GPU 0.
3. **GPU 0 booking.** At 22000 MB a video job evicts the embedding model and
   one small resident. Accept that, or wait for checklist item 6 and try the
   `--max-vram 16` cap (17500 MB)?
4. **Front door.** Confirm `/v1/videos` for #69, given OpenAI retired its
   hosted Videos API in September 2026 `(verify)`. Should the adapter live in
   this repo under `cmd/`, built with the router, or beside `sd-router-shim`?
5. **MiniMax-H3 licence territory.** It excludes the US, EU, UK and South
   Korea. Is anyone outside Australia using the service or its outputs?
6. **The other image node.** Its LTX-2.3 and MiniMax-H3 stanzas say
   `api_type: image`. Relabel them `video` (a config change for a separate
   issue)? Which client calls them today, and through which path? The adapter
   must not break it.

## 9. Sources

All read on 2026-11-20 through search-engine extracts (no direct page fetch in
this run).

stable-diffusion.cpp (primary)

- https://github.com/leejet/stable-diffusion.cpp
- https://github.com/leejet/stable-diffusion.cpp/releases (`master-1106-532d2c1`, 2026-11-16)
- https://github.com/leejet/stable-diffusion.cpp/blob/master/examples/server/api.md
- https://github.com/leejet/stable-diffusion.cpp/tree/master/examples/server
- https://github.com/leejet/stable-diffusion.cpp/blob/master/examples/cli/README.md
- https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/ltx2.md
- https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/minimax_h3.md
- https://github.com/leejet/stable-diffusion.cpp/pull/1884 (progress on async jobs)
- https://github.com/leejet/stable-diffusion.cpp/pull/2303 (progress by stage)
- https://github.com/leejet/stable-diffusion.cpp/pull/2194 (reference video and audio for `vid_gen`)
- https://github.com/leejet/stable-diffusion.cpp/pull/2234 (measured V100 notes for MiniMax-H3)
- https://github.com/leejet/stable-diffusion.cpp/issues/2233 (WebM with raw PCM audio)
- https://github.com/leejet/stable-diffusion.cpp/issues/1865 (MiniMax-H3 wrong output under segmented offload)
- https://github.com/leejet/stable-diffusion.cpp/issues/2370 (MiniMax-H3 in ComfyUI on a V100 needs cu130)
- https://github.com/lemonade-sdk/lemonade/pull/3506, https://github.com/lemonade-sdk/lemonade/discussions/3527 (client experience: `video_frames`, states, cancel)
- https://gist.github.com/FNGarvin/96dbcaf2688a7047c2e6c30e9941c7da (client polling example)
- https://deepwiki.com/leejet/stable-diffusion.cpp/5.2-server-api (secondary)
- https://docs.qvac.tether.io/ai-capabilities/video-generation.md (fork docs: temporal tiling, 4k+1 frames)
- https://www.ojambo.com/run-ltx-2-3-video-generation-locally-with-stable-diffusion-cpp-on-amd-gpu-for-zero-cloud-costs (third-party CLI example)
- https://github.com/fszontagh/stable-diffusion.cpp-restapi

ComfyUI (fallback)

- https://github.com/Comfy-Org/ComfyUI
- https://docs.comfy.org/development/comfyui-server/comms_routes
- https://docs.comfy.org/changelog
- https://github.com/Comfy-Org/ComfyUI/blob/master/server.py (client `prompt_id`, UUID validation)
- https://github.com/Comfy-Org/ComfyUI/issues/15965, https://github.com/Comfy-Org/ComfyUI/pull/17330 (history lost on restart; opt-in persistence)
- https://github.com/co5dt/ComfyUI-Persistent-Queue
- https://comfy.org/hub/workflows/ltx-2-text-to-video-40770c9eb41b/ (LTX-2 audio nodes, `SaveVideo` MP4)
- https://docs.ltx.io/open-source-model/integration-tools/comfy-ui
- https://github.com/lotus0000/ComfyUI-Qwen-V100-MixedPrecision (V100 runs fp32 by default)
- https://docs.clore.ai/guides/getting-started/cuda-pytorch-compatibility (Volta dropped from cu128 at 2.11)

OpenAI-style `/v1/videos` servers

- https://platform.openai.com/docs/api-reference/videos/create
- https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/videos_api/
- https://docs.vllm.ai/projects/vllm-omni/en/latest/user_guide/examples/online_serving/text_to_video/
- https://pypi.org/project/vllm-omni/
- https://docs.sglang.io/docs/sglang-diffusion/api/openai_api
- https://docs.sglang.io/docs/sglang-diffusion
- https://localai.io/docs/features/video-generation/
- https://platform.openai.com/docs/deprecations (the page that third-party reports of the 2026-09-24 retirement quote; not itself read in this run)

Models

- https://github.com/Lightricks/LTX-2, https://github.com/Lightricks/LTX-2/blob/main/LICENSE-2, https://github.com/Lightricks/LTX-2/blob/main/LICENSE-2_x
- https://huggingface.co/unsloth/LTX-2.3-GGUF
- https://ltxworkflow.com/guide/vram-requirements (third party)
- https://prompthero.com/ai-models/ltx-23-video-generation-distilled-gguf-workflows-2444935-download (third party; 3090 timing)
- https://github.com/MiniMax-AI/MiniMax-H3
- https://atoms.dev/blog/minimax-h3-open-weights-multimodal-video-model (third party; licence territory)
- https://github.com/Wan-Video/Wan2.2
- https://computingforgeeks.com/run-wan-video-generation-locally/ (third party; 4090 OOM at decode)
- https://wan27.org/blog/wan-2-2-comfyui-workflow-guide (third party; 3090 figures)
- https://huggingface.co/docs/diffusers/api/pipelines/wan
- https://github.com/Tencent-Hunyuan/HunyuanVideo-1.5
- https://willitrunai.com/blog/hunyuanvideo-1-5-vram-requirements (third party)
- https://github.com/lllyasviel/FramePack
- https://github.com/SkyworkAI/SkyReels-V2, https://github.com/SkyworkAI/SkyReels-V3
- https://github.com/deepbeepmeep/WanGP, https://github.com/deepbeepmeep/WanGP/blob/main/docs/CLI.md
- https://www.thundercompute.com/blog/best-open-source-ai-video-generation-models (third party; Mochi and CogVideoX status)

This repo

- [`SPEC.md`](../../SPEC.md) (stanza fields, request flow, eviction, shutdown)
- [`docs/research/oss-model-router-landscape.md`](oss-model-router-landscape.md) (hold vs job polling)
- [`docs/research/realtime-backends.md`](realtime-backends.md), [`docs/research/tts-backends.md`](tts-backends.md) (port range, checklist conventions)
- `internal/router/matcher.go`, `internal/router/server.go`,
  `internal/router/router.go`, `internal/router/activity.go`,
  `internal/config/config.go`, `cmd/fake-model/main.go`
- the GPU host's and the 24 GB image node's configs under `configs/`
