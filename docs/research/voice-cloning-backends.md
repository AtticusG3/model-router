# Voice cloning and a voice library across router nodes

Date of reading: 2026-11-20. Part of #64, issue #77.

Research only. Nothing here changes code, configs or the running service. Every
candidate has a primary-source link. A fact that could not be confirmed from the
project's own pages is marked `(verify)`. Anything only the GPU host can settle
is in the checklist (§8).

## 0. Scope and method

A cloned voice is **state**. A reference clip, its transcript and a consent
record are created once and then used by name. The router can run the same
`model_id` on several instances and several nodes. A voice registered with one
backend process may therefore be missing on the process that serves the next
request. This note answers where that state should live, and which
cloning-capable backend the router should front.

It builds on [the TTS backend note](tts-backends.md) (#75), which already
chose the serving runtime and wrote the base stanzas. That note's speech request
fields, errors, VRAM budget and cold-start figures are not repeated here. This
note covers only cloning state, the voice API, voice design, consent and
first-request latency.

**How the facts were gathered.** Page fetches were not available in this run.
This run had web search only, so every external fact comes from
search-engine extracts of the cited pages, read on 2026-11-20, not from opening
the files. Field names, flags and defaults that matter for the stanza or the
router design are marked `(verify)` when only an extract supports them. They
are repeated in the checklist where the GPU host can confirm them.

**The consumer's constraint.** The issue points to the sibling content
project's E2 note ([TTS model selection and hosting][e2]), sections 5.2 (voice
assets) and 5.3 (service contract). Per the issue text, they propose
**seed audio per line**: the consumer keeps each cast voice's reference clip
and sends it with every line, so the TTS service stays stateless. That Gitea
instance was not reachable from this run, so the summary is quoted from the
issue, not read first-hand `(verify)`. Casting (E3, content project #22) and
performance direction (E4, #23) are open there and are not redone here.

[e2]: https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md

**Router facts this note relies on** (read from this repo):

- `/v1/{path...}` is proxied unchanged. The router rewrites the body `model`
  only on a peer hop, to the peer-local id (`SPEC.md`, request flow step 5).
- A model-less request is routed by path. `/v1/audio/speech` falls to the
  `api_type: speech` default (`internal/router/matcher.go`).
  `/v1/audio/voices` is **not** in that path table, so a model-less request
  there is a router `404`, unless a stanza claims it with `match.path_prefix`.
- Multipart bodies are matched on their `model` part, then on the query.
  A GET or DELETE is matched on the `model` query parameter only.
- `filters` can set or strip top-level JSON keys, per stanza or per alias
  (`setParamsByID`), but never `model` (`internal/config/config.go`). They
  cannot read a file, look anything up, or rename a field.
- The matcher reads the whole request body into memory. Only the control
  routes (`/_router/*`) have a size cap. Activity keeps 256 KB per body.

## 1. Recommendation

**Primary backend: vLLM-Omni serving Qwen3-TTS-12Hz-1.7B-Base, on the 24 GB
sm_86 card (GPU 0) only.** This is the same stanza as the TTS note's primary.
Cloning needs no second model. It is the only candidate with a documented,
OpenAI-shaped cloning surface that covers both inline cloning (`ref_audio` +
`ref_text` on `/v1/audio/speech`) and a voice registry. The registry is
`POST/GET/DELETE /v1/audio/voices` with a required `consent` field, voices kept
on disk and restored at restart. Apache-2.0 code and weights. It **does not
run on the 32 GB sm_70 cards** (vLLM-Omni needs compute capability 7.5 or
higher).

**Fallback backend: `llama-server` (llama.cpp v1.0.0 or later) serving the
Qwen3-TTS-12Hz-1.7B-Base GGUF, on the 32 GB sm_70 cards (`device: "1|2"`) or
on CPU.** Same weights, so the same clip gives the same voice. It clones inline
only (`speaker_ref_b64`) and has no voice registry. MIT code.

**The rule for choosing between them** is the TTS note's rule, unchanged. Name
the primary by default. Use the fallback by name only when the primary answers
`503` with `Retry-After` because GPU 0 is held, or for batch work on a 32 GB
card the operator has freed. The router never swaps between them on its own,
because the two take the reference under different field names.

**Where voice state lives: (a) a router-side voice library.** Do not use the
backend registry, and do not replicate it.

- The router keeps each voice as two files: the clip (WAV) and a JSON record
  holding transcript, language and consent (§7.2). On a speech request that
  names a library voice, the router puts the clip and transcript into the body
  in the serving stanza's field names, just before it proxies.
- Every request then reaches the backend fully self-contained. Any instance,
  any node, the primary or the fallback can serve it. A backend restart, the
  idle sweep or an eviction loses nothing.
- This is a **router change** (§5.3). Until it lands, clients do what E2
  already does: send `ref_audio` and `ref_text` inline on every request. That
  works today on every instance and peer with no change, and the library keeps
  working for clients that never adopt it.

Why not (b), the backend registry with replication: a vLLM-Omni voice lives in
one process's `SPEAKER_SAMPLES_DIR` on one node. The fallback has no registry at
all. Creating a voice would cold-start a 9 GB model just to save a file.
Deleting it would have to reach every node, and consent would sit in a file
header the router cannot query (§5.2).

## 2. Alternatives

`3090` and `V100` say whether the serving runtime runs on sm_86 or sm_70 at
all, and the expected VRAM. Licence is code / weights. Dates are the last
release or model drop found.

| Name | Repo | Licence | Last release / commit | API shape | Sync / async | Streaming | 3090 (sm_86) | V100 (sm_70) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| vLLM-Omni + Qwen3-TTS 1.7B Base | https://github.com/vllm-project/vllm-omni, https://github.com/QwenLM/Qwen3-TTS | Apache-2.0 / Apache-2.0 | vLLM-Omni 0.32.0 (Oct 2026), 0.33.0rc1 on PyPI 2026-11-04; Qwen3-TTS Jan 2026 | OpenAI `/v1/audio/speech` + `/v1/audio/voices` (OpenAI custom-voice shape) | sync | PCM/WAV chunks, SSE, websocket | yes; ~3.9 GB measured, book 9000 MB (TTS note §4) | **no** (cc 7.5+) | **primary** |
| llama.cpp `llama-server` + Qwen3-TTS Base GGUF | https://github.com/ggml-org/llama.cpp | MIT / Apache-2.0 | v1.0.0, 2026-11-13 | OpenAI `/v1/audio/speech` and native `/tts`; inline `speaker_ref_b64` or multipart `speaker_ref`; no registry | sync | `stream_format: "audio"` | yes; ~3-4 GB `(verify)` | yes (host already runs `llama-server` on the V100s) | **fallback** |
| vLLM-Omni + VoxCPM2 (2B) | https://github.com/OpenBMB/VoxCPM | Apache-2.0 / Apache-2.0 | VoxCPM2 Apr 2026 | as vLLM-Omni; `ref_audio` + `ref_text`, `instructions`, uploaded and precomputed voices | sync | as vLLM-Omni | yes; ~8 GB `(verify)` | no | **the stanza to add if E4 needs delivery direction on a cloned voice** (§3.4); same runtime, same library |
| vLLM-Omni + IndexTTS-2.5 | https://github.com/index-tts/index-tts, recipe https://github.com/vllm-project/vllm-omni/blob/main/recipes/IndexTeam/IndexTTS-2_5.md | bilibili Model Use Licence (code and weights) | 2026-08-10 | as vLLM-Omni; emotion in `extra_params` | sync | as vLLM-Omni | yes; ~6 GB per model card | no (vLLM-Omni); upstream 2.5 is bf16 | skip: best emotion control, but commercial use needs written permission |
| CosyVoice3 (Fun-CosyVoice3-0.5B) | https://github.com/FunAudioLLM/CosyVoice | Apache-2.0 / Apache-2.0 `(verify)` | `-2512` checkpoint | own FastAPI (no registry route found), or vLLM-Omni `(verify)` | sync | `(verify)` | yes | own server maybe, fp16 `(verify)` | skip: the speaker cache (`add_zero_shot_spk`) is a Python call, not an HTTP route |
| Chatterbox Multilingual v3 (devnen Chatterbox-TTS-Server) | https://github.com/resemble-ai/chatterbox, https://github.com/devnen/Chatterbox-TTS-Server | MIT / MIT | v3 weights 2026-06-10; upstream **retired Aug 2026** | OpenAI `/v1/audio/speech` (`voice` = a file name) + `/upload_reference` | sync | `(verify)` | yes | likely, fp32 `(verify)` | skip: upstream archived; voices are files in one server's folder |
| Fish Speech / S2-Pro | https://github.com/fishaudio/fish-speech | Fish Audio Community Licence / **Fish Audio Research Licence (non-commercial)** | S2-Pro Mar 2026; licence change Oct 2026 | own `POST /v1/tts` (msgpack) + `/v1/references/{add,list,update,delete}` | sync | `(verify)` | 4.4B; size `(verify)` | `(verify)` | skip: non-commercial weights, not OpenAI-shaped |
| Higgs Audio v2 / v3 | https://github.com/boson-ai/higgs-audio, recipe https://github.com/vllm-project/vllm-omni/blob/main/recipes/BosonAI/Higgs-Audio-V3-TTS.md | v2 community licence from its base model; **v3 research and non-commercial** `(verify)` | v3 on vLLM-Omni (date `(verify)`) | as vLLM-Omni (v3); `ref_audio` + optional `ref_text` | sync | as vLLM-Omni | v2 ~9-14 GB; v3 recipe targets an 80 GB card | no | skip: large, and v3 terms |
| F5-TTS | https://github.com/SWivid/F5-TTS | MIT / **CC-BY-NC-4.0** | v1 Base weights 2025-03-12 | Python and CLI only (`ref_audio`, `ref_text`) | — | — | yes | `(verify)` | skip: non-commercial weights, no server |
| XTTS-v2 (coqui-tts fork; xtts-api-server) | https://github.com/idiap/coqui-ai-TTS, https://github.com/daswer123/xtts-api-server | MPL-2.0 / **CPML, non-commercial incl. output** | `coqui-tts` 0.27.x `(verify)`; xtts-api-server inactive | own; speakers folder of WAVs plus cached latents | sync | own streaming mode | yes | likely `(verify)` | skip: output-restricting licence, unmaintained server |
| OpenVoice V2 | https://github.com/myshell-ai/OpenVoice | MIT / MIT (since Apr 2024) | V2 Apr 2024 | Python API and Gradio demo; no server | — | — | yes, small | likely `(verify)` | skip: no server; tone-colour conversion on a separate base speaker |
| SoVITS project (RVC-Boss) | https://github.com/RVC-Boss | MIT `(verify)` | v2Pro / v4 (date `(verify)`) | own `api_v2.py`: `/tts` with `ref_audio_path` (a **server-side path**), `/set_*_weights` | sync | chunked (v1/v2/v2Pro) | yes | `(verify)` | skip: voice switching swaps global weights per process, and its weight-loading routes are unauthenticated |
| ZONOS3-Mini | https://github.com/Zyphra/ZONOS3-Mini | MIT / Apache-2.0 | 2026-11-09 to 11-12 | own servers, OpenAI `/v1/audio/speech`; saved voices as `speaker_embedding_id`, `GET /v2/speakers` `(verify)` | sync | yes | small (100M active) `(verify)` | `(verify)` | watch: permissive and portable, but about ten days old |

Thirteen candidates. Six or more run as a server, but only vLLM-Omni has both
inline cloning and a registry in the OpenAI shape. Its registry copies OpenAI's
own custom-voice API: multipart `audio_sample`, `name` and `consent` on
`POST /v1/audio/voices`. That is the shape the router library should present
too (§5.3).

### 2.1 Cloning API per backend (scope question 1)

| Backend | Inline per request | Upload and name | Embedding / precomputed files | Reference limits | Survives restart |
| --- | --- | --- | --- | --- | --- |
| vLLM-Omni (Qwen3-TTS Base, VoxCPM2, IndexTTS-2.5, Higgs v3) | `ref_audio` (`data:` URI, http(s) URL, or `file://` under `--allowed-local-media-path`), `ref_text`, `language`; Qwen3-TTS also `x_vector_only_mode`, `speaker_embedding` (2048 floats for 1.7B) | `POST /v1/audio/voices` multipart: `audio_sample` (≤ 10 MB; wav, mp3, flac, ogg, aac, webm, mp4), `name`, `consent` (**required**, ≤ 1024 chars), `ref_text` (optional, ≤ 8192; without it only the speaker embedding is kept), `speaker_description` | `custom_voice_dir` in the deploy config: manifest + one `.safetensors` per voice, made offline with `precompute_custom_voice.py` (`--mode icl` or `xvec`) | Qwen3-TTS: one clip, any rate (resampled to 24 kHz mono), **≤ 60 s**; 3 s minimum, 10-20 s advised `(verify)` | **yes**: uploads are one `.safetensors` each in `SPEAKER_SAMPLES_DIR` (default `~/.cache/vllm-omni/speakers`), rescanned at start-up; `custom_voice_dir` is read at start-up |
| llama-server | `speaker_ref_b64` (JSON) or multipart `speaker_ref`; `lang`; no `ref_text` found `(verify)` | none | `--tts-speaker-file` (one fixed voice per process) | wav, mp3 | n/a (nothing stored) |
| CosyVoice3 | prompt wav + prompt text per call | Python `add_zero_shot_spk` (+ `save_spkinfo` on v2) | `spk2info` file | `(verify)` | only if saved `(verify)` |
| Chatterbox-TTS-Server | `/tts` `voice_mode: clone` + `reference_audio_filename` (a file already on the server) | `/upload_reference` (wav, mp3) | none | `(verify)` | yes (files in `reference_audio/`) |
| Fish Speech | `references` in the msgpack body | `/v1/references/add` (`id`, `audio`, `text`) | `references/<id>/` audio + `.lab` | `(verify)` | yes (folder) |
| F5-TTS | `ref_audio` + `ref_text` (Python/CLI) | none | none | **< 12 s**, longer clips are clipped; ~1 s trailing silence | n/a |
| XTTS-v2 | `speaker_wav` (one or several) | speakers folder; coqui-tts ≥ 0.27 can cache a named speaker `(verify)` | latents JSON (fork) | `(verify)` | yes (folders) |
| SoVITS (RVC-Boss) | `ref_audio_path` (server file), `prompt_text`, `prompt_lang` | `/set_refer_audio` (process-global) | per-voice fine-tuned weights | `(verify)` | weights yes; reference no |
| ZONOS3-Mini | reference clip, no transcript needed | saved embeddings (`speaker_embedding_id`) `(verify)` | 192-dim speaker embedding | ≤ 60 s | `(verify)` |

The pattern: **inline reference is universal**. Registries differ in every
field name, and most are a folder on one machine's disk.

## 3. API surface of the recommended backend (vLLM-Omni, cloning parts)

Sources: the vLLM-Omni [Speech API reference][omni-speech] and the
[text-to-speech serving guide][omni-tts]. The full `/v1/audio/speech` field
list, response formats, streaming, websocket and error envelope are in the TTS
note §3 and are not repeated. Everything below comes from search extracts, so
the whole section is `(verify)` against the installed version's
`/openapi.json` (checklist item 3).

[omni-speech]: https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/speech_api/
[omni-tts]: https://docs.vllm.ai/projects/vllm-omni/en/latest/user_guide/examples/online_serving/text_to_speech/

### 3.1 Endpoints

| Method and path | Purpose | Router must proxy? |
| --- | --- | --- |
| `POST /v1/audio/speech` | synthesise; JSON in, audio out | **yes** (all designs) |
| `POST /v1/audio/speech/batch` | several inputs, one reference | yes |
| `GET /v1/audio/speech/stream` (websocket) | text in, audio out | yes |
| `GET /v1/audio/voices` | presets plus `uploaded_voices` | only under (b); under (a) the router answers it |
| `POST /v1/audio/voices` | upload a voice (multipart) | only under (b) |
| `DELETE /v1/audio/voices/{name}` | delete an uploaded voice; response body not documented | only under (b) |
| `POST /v1/audio/voices/{name}/cache` | precompute the speaker embedding and reference codes for an uploaded Qwen3-TTS voice; answers `{"voice":...,"cache_status":"ready",...}` (PR #2457) | only under (b) |
| `GET /v1/models` | served model name | yes (router merges) |
| `GET /health` | 200, empty body, once every stage is up | router polls it |

There is **no unload endpoint** and no voice endpoint for the fallback. The
router unloads by killing the process group.

### 3.2 Cloning fields on `POST /v1/audio/speech`

```json
{
  "model": "qwen3-tts-1.7b",
  "input": "A fictional line for the narrator.",
  "language": "English",
  "task_type": "Base",
  "ref_audio": "data:audio/wav;base64,UklGR...",
  "ref_text": "The exact words spoken in the reference clip.",
  "response_format": "wav"
}
```

- `ref_audio` + `ref_text`: in-context cloning (best quality). `ref_text` must
  match the recording. Without `ref_text`, set `x_vector_only_mode: true`
  (speaker embedding only, lower similarity).
- `voice: "<name>"` instead of `ref_audio` uses an uploaded or precomputed
  voice. Uploaded and precomputed Qwen3-TTS voices always run as `Base`
  whatever `task_type` says, so they need the Base checkpoint.
- `speaker_embedding`: a raw 2048-float vector (1.7B). Base only.
- Whether a Base request that carries both `ref_audio` and an unknown `voice`
  name is refused (`400 Invalid voice`) or ignores `voice` is not documented.
  For VoxCPM2 the docs say `voice` is ignored unless it names an uploaded
  voice. Checklist item 5 settles it for Qwen3-TTS. The router library must
  **remove `voice`** when it injects (§5.3), so either answer is safe.

### 3.3 Upload: `POST /v1/audio/voices`

```
curl -X POST http://127.0.0.1:{port}/v1/audio/voices \
  -F "audio_sample=@narrator-a.wav" -F "name=narrator-a" \
  -F "consent=consent-0001" -F "ref_text=The exact words spoken in the clip."
```

- Response: `name`, `consent`, `created_at`, `mime_type`, `file_size`,
  `ref_text`, `speaker_description`.
- Name clash with a built-in or precomputed voice: `400`. Re-upload of an
  existing name overwrites it, unless
  `VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable` (then `400` until deleted).
  Any other value of that variable fails start-up.
- `SPEAKER_MAX_UPLOADED` (default 1000) caps stored voices.
  `VLLM_OMNI_SPEAKER_CACHE_MAX_BYTES` (default 512 MiB) caps the in-memory
  reference cache.
- With `--api-server-count > 1`, upload and delete are disabled in the current
  docs. PR #8424 (in review, Oct 2026) shares the directory between API
  processes and refreshes it on each speech request. Not relied on here.
- The consent string is written into the stored file name after sanitising
  (PR #4749 review). It is an opaque label, not a record anyone can query.

### 3.4 Voice design and delivery control (scope question 3)

| Model (runtime) | Text prompt → new voice | Delivery change that keeps a cloned identity | Notes |
| --- | --- | --- | --- |
| Qwen3-TTS 1.7B **VoiceDesign** (vLLM-Omni) | yes: `task_type: "VoiceDesign"`, `instructions` required | no (designs, does not clone) | each call samples a voice; to keep it, **design then clone**: save one designed clip into the library and clone it with Base |
| Qwen3-TTS **Base** (vLLM-Omni, llama-server) | no | **no instructions**; delivery follows the reference clip | for a sad or angry line, keep a second clip of the same speaker in that delivery; the client example never sends `instructions` for Base, and server behaviour if sent is `(verify)` |
| Qwen3-TTS **CustomVoice** | no | `instructions` on nine presets only | not cloning |
| VoxCPM2 (vLLM-Omni) | yes: `instructions` alone | **yes**: reference + `instructions` ("voice direction") | full cloning (`ref_audio` + `ref_text`) turns instructions off; 48 kHz output |
| IndexTTS-2.5 (vLLM-Omni) | no | **yes**: `extra_params.emo_vector` (8 values: happy, angry, sad, afraid, disgusted, melancholic, surprised, calm; 0-1.2), `emo_audio`, `emo_text`, `emo_alpha` | precedence `use_emo_text` > `emo_vector` > `emo_audio` > the reference's own emotion; licence restricted |
| CosyVoice3 | no | yes: `inference_instruct2` with a cached `zero_shot_spk_id` | Python API; HTTP support `(verify)` |
| Higgs Audio v3 | `(verify)` | tokens inside the text | non-commercial `(verify)` |
| ZONOS3-Mini | `(verify)` | "voice control" by activation steering `(verify)` | new |
| F5-TTS, XTTS-v2, OpenVoice V2, SoVITS, Chatterbox | no | no text control (Chatterbox has an exaggeration knob `(verify)`) | — |

So the primary clones but cannot be directed. If E4 needs "same voice, angrier",
the permissive answer is a **VoxCPM2 stanza on the same runtime**. It reads the
same library entries, so nothing about voice state changes (open question 4).

## 4. Hardware fit

The TTS note §4 has the measured and budgeted VRAM, card occupancy and cold
start. In short:

| | Primary (vLLM-Omni + Qwen3-TTS 1.7B Base) | Fallback (llama-server GGUF) | VoxCPM2 option |
| --- | --- | --- | --- |
| Card | GPU 0 (24 GB, sm_86) only | GPU 1 or 2 (32 GB, sm_70), or CPU | GPU 0 only |
| Resident / peak | ~3.9 GB measured; **book 9000 MB** (stage memory fractions 0.22 + 0.08) | ~3-4 GB at Q8_0 `(verify)`; book 4000 MB | ~8 GB `(verify)`; book 12000 MB until measured |
| Precision | bf16 (fine on sm_86) | Q8_0 backbone; bf16 mmproj on Volta `(verify)`, else convert to f16 | bf16 |
| sm_70 fp16 fallback | none: the runtime refuses cc < 7.5 | ggml CUDA builds include sm_70 | none |
| CPU-only | no | `--n-gpu-layers 0`; speed unmeasured (checklist item 8) | no |
| Beside the large chat model and embedding model | yes: the large chat model is on GPUs 1-2. GPU 0 holds the intent, embedding and reranker models (6600 MB); +9000 leaves ~8.9 GB | only by evicting the large chat model (64000 MB across both V100s) | 6600 + 12000 fits alone, not beside the primary **and** an image job |
| Cold start | ~100-120 s `(verify)` | seconds `(verify)` | `(verify)` |

**What cloning adds to the hardware picture.** A voice library costs no VRAM. An
inline reference adds one speaker-encoder and codec pass per new clip.
vLLM-Omni's reference cache (512 MiB default, inside the process) holds the
encoded result, keyed by the `data:` URI string or by a `file://` path's mtime
and size. Under the backend-registry design, each uploaded voice is also a
file on disk in the backend's format.

### 4.1 Latency of the first request after a voice is created (scope question 5)

| Step | (a) router library | (b) backend registry |
| --- | --- | --- |
| Create the voice | write two files on the router node: milliseconds, no GPU | `POST /v1/audio/voices` to a running backend; if it is not loaded, the router cold-starts it first (**~100-120 s** primary) |
| Optional warm-up | none needed | `POST /v1/audio/voices/{name}/cache` precomputes embedding and codes |
| First speech request with it | backend cold start if unloaded, then one reference encode (cache miss) | backend cold start if unloaded; reference encode unless `/cache` was called |
| Later requests | cache hit on the same `data:` URI string, until the process restarts or the cache evicts | cache hit |
| Measured cost | the vLLM-Omni technical report gives a median time to first packet of ~131 ms at concurrency 1 on a freshly started server, whose per-clip caches start empty `(verify)` that this includes the encode; VoxCPM2 cloning adds ~0.017 RTF over zero-shot | same |

The reference encode is small next to the cold start in both designs. The real
first-request cost is whether the model is loaded. A voice library does not
change that. The backend registry makes it worse, because creating a voice
needs the model loaded.

## 5. How the router would run it

### 5.1 Spawn and stanza fields

Spawn, health, `spin_up_seconds`, `vram_mb`, `idle_ttl_seconds` and `slots` are
the TTS note's §5 values, unchanged: `vllm serve ... --omni --deploy-config
... --served-model-name qwen3-tts-1.7b --host 127.0.0.1 --port {port}` in the
foreground on GPU 0. `health_check: /health`, `spin_up_seconds: 300`,
`vram_mb: 9000`, `idle_ttl_seconds: 900`, `slots: 4`. The fallback is
`llama-server` on `"1|2"` with `{gpu}` in `CUDA_VISIBLE_DEVICES`.

Two changes for cloning, both in §6:

1. **`match: {path_default: true}`**, dropping the TTS note's
   `path_prefix: /v1/audio/voices`. If the router routes model-less
   `/v1/audio/voices` to the backend, any client can upload a voice straight
   into the backend registry. That voice would live on one node only, with
   only an opaque consent label. Under (a) the router answers that path itself
   (§5.3). Until then, a model-less `/v1/audio/voices` is a plain router `404`.
2. **`SPEAKER_SAMPLES_DIR` set explicitly** to a known directory, with
   `VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable`. Anything that does reach
   the backend registry (a request with `?model=`) then lands in one audited
   place and cannot silently overwrite a voice. There is no documented switch
   that turns uploads off `(verify)`, checklist item 7.

### 5.2 What breaks if the router only passes bytes through

Inline references (`ref_audio` per request) **work today** on every instance,
node and restart. Nothing is stored, so nothing can be missing. What breaks is
everything stateful:

- **A voice uploaded on one node is unknown on another.** Node A's process
  keeps it in A's `SPEAKER_SAMPLES_DIR`. A request that names it and spills to
  node B (slot spill, eviction, a peer hop) gets `400 Invalid voice`. The TTS
  note also records a downstream report of a Base request with an unusable
  voice killing the engine `(verify)`.
- **A voice uploaded through one instance is unknown to a second instance on
  the same node**, unless both share the directory and PR #8424's refresh is
  in the installed build `(verify)`.
- **The fallback has no registry.** A voice name that works on the primary
  cannot reach `llama-server`. The client must resend the clip as
  `speaker_ref_b64`.
- **Uploads and deletes need the model loaded**, so a router cold-starts a
  9 GB model to save or remove a file.
- **Routing:** a multipart upload with no `model` part and no `?model=`, a
  model-less `GET /v1/audio/voices`, and a `DELETE /v1/audio/voices/{name}`
  all return router `404`, unless a stanza claims the prefix. This is the
  same trap as production-stack issue #1210 for vLLM's own router.
- **Deletion** must reach every node's directory and every running process's
  in-memory cache. The router cannot do either.
- **Size:** inline references make each request ~0.6 MB (10 s, 24 kHz, 16-bit
  mono, base64) up to ~3.8 MB (60 s). The router buffers whole bodies with no
  cap on proxied routes. That is acceptable at this size, but a hostile client
  could send far more. Activity shows the body truncated at 256 KB, which is
  cosmetic.

### 5.3 Proposed router change for (a), the voice library

None of this is in this PR. Each item is a **router change**.

1. **Top-level `voices_dir`** (new key), for example
   `/opt/ai/state/voices`. One `<id>.wav` and one `<id>.json` per voice. No
   database.
2. **The router owns `/v1/audio/voices`**, in vLLM-Omni's (and OpenAI's)
   upload shape:
   - `POST` multipart `audio_sample`, `name`, `consent`, `ref_text`,
     `language`. Required: `name`, `consent`, `ref_text` and `language`.
     A clip without a transcript gives worse clones. A missing language is the
     guess E1 forbids.
   - `GET` lists the records, without audio.
   - `DELETE /v1/audio/voices/{name}` removes both files and logs who did it.
   - Limits checked before anything is written: WAV only (both backends read
     it), ≤ 10 MB, ≤ 60 s from the WAV header, name `[a-z0-9-]{1,64}`. The
     router does not transcode.
3. **Injection on `POST /v1/audio/speech` and `/batch`.** If `voice` names a
   library entry, the router does this before the stanza's own `filters`:
   - remove `voice`;
   - set `language` from the record, unless the client set one;
   - add the reference in the serving stanza's format.

   That format is a new per-stanza key, for example
   `voice_format: ref_audio | speaker_ref_b64`:
   - `ref_audio` writes `ref_audio` (`data:audio/wav;base64,...`) and
     `ref_text` (vLLM-Omni);
   - `speaker_ref_b64` writes raw base64 and sets `lang` (`llama-server`).

   A name the library does not hold is a router `400 voice_not_found`, never a
   default voice. A body with `ref_audio` already set is left alone (E2's
   inline path).
4. **Inject on the node that runs the backend, not the node the client hit.**
   On a peer hop the body keeps `voice`, and the serving node injects in its own
   stanza's format. So the voice must exist on every node that may serve it.
   Replication is a one-way `rsync` of `voices_dir` from the node where voices
   are created. It is plain files, needs no mesh protocol, and a few hundred
   MB at most. A miss on the serving node is the loud `400` above.
   - The alternative, injecting at the origin, needs no sync. But the origin
     would have to know the peer stanza's `voice_format`, which telemetry does
     not publish. That is a bigger change, so this note does not take it.
5. **The same injection makes the TTS note's pool safe.** Its open question 4
   (primary-to-fallback failover) failed because the field names differ. With
   `voice_format` per stanza, a pool of `qwen3-tts-1.7b` and
   `qwen3-tts-1.7b-gguf` can carry a library voice to either.

**Weighing (a) against (b):**

| | (a) router library | (b) backend registry + replication |
| --- | --- | --- |
| Disk growth | the WAV as uploaded: ~0.5 MB per 10 s voice, so ~0.5 GB per 1000 voices; 1.3 TB free | the same per node, in each backend's own format, again for every backend family |
| Consent and deletion | one record per voice; one delete per node's directory (rsync `--delete`); the backend's in-memory cache clears at its next restart or eviction | consent is an opaque label in a file name; delete per node, per backend, while each is loaded |
| Peer sync | rsync of plain files; nothing else | rsync of a backend-internal format, plus a restart or a refresh PR to pick it up |
| Request size | ~0.6-3.8 MB from router to backend; the client sends ~100 bytes | small everywhere |
| Fallback | works (`speaker_ref_b64`) | none |
| E2 statelessness | kept: the backend still sees a self-contained request; inline clients are untouched | broken: the backend holds state |
| Router code | new: ~3 routes, one injection hook, two config keys | none, but every failure in §5.2 |

## 6. Proposed stanzas

Every key below is a documented `Stanza` field (`SPEC.md` config block and the
YAML tags in `internal/config/config.go`). **No new key is used.** The voice
library keys in §5.3 (`voices_dir`, `voice_format`) are a future router change
and are deliberately absent. These stanzas **replace** the TTS note's §6.1 and
§6.2 for the same `model_id`s. They are not extra stanzas. Paths and ports are
placeholders, the same as that note's.

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
    env:
      - CUDA_DEVICE_ORDER=PCI_BUS_ID
      - CUDA_VISIBLE_DEVICES=0
      - HF_HUB_OFFLINE=1
      - SPEAKER_SAMPLES_DIR=/opt/ai/state/vllm-omni-speakers
      - VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable
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

Unchanged from the TTS note. No `--tts-speaker-file`: a fixed speaker would be a
silent default voice for any request that forgets its reference.

## 7. Licence and risk flags

Internal use only. These terms matter if any generated audio is published.

### 7.1 Licences and what they say about cloning

- **Qwen3-TTS** Base and VoiceDesign: Apache-2.0, not gated. No acceptable-use
  or anti-impersonation section was found on the model card `(verify)`. The
  licence puts no limit on cloning. Consent is the operator's job.
- **vLLM-Omni**: Apache-2.0. It requires a `consent` value on upload but does
  not check it. Opt-in AudioSeal watermarking is proposed (RFC #7365, PR
  #7366, off by default). Whether it is merged is `(verify)`.
- **llama.cpp**: MIT. No cloning terms.
- **VoxCPM2**: Apache-2.0. **ZONOS3-Mini**: Apache-2.0 weights, MIT code.
  **OpenVoice V2**: MIT. **Chatterbox**: MIT, upstream archived since Aug 2026,
  and every output carries an inaudible PerTh watermark.
- **Non-commercial or restricted**:
  - F5-TTS weights: CC-BY-NC-4.0.
  - Fish S2-Pro weights: Fish Audio Research Licence. The code licence since
    Oct 2026 also bars use as a hosted TTS service.
  - XTTS-v2: CPML, which covers the **output**.
  - IndexTTS-2/2.5: bilibili Model Use Licence; commercial use needs written
    permission.
  - Higgs Audio v3: research and non-commercial `(verify)`.
  - OmniVoice (on vLLM-Omni): CC BY-NC `(verify)`.
- **SoVITS `api_v2.py`**: its two `/set_*_weights` routes load any
  checkpoint path and are unauthenticated. One code scan rates that as remote
  code execution for anyone who can place a file on the host. Do not expose it.
- **EU AI Act Article 50** (applies from 2 August 2026): deployers must
  disclose deep-fake audio that resembles a real person, and providers must
  mark synthetic output in a machine-readable way. It matters only if audio
  reaches an EU audience. A watermark does not discharge the disclosure duty.

### 7.2 Minimal consent record the router should keep

One JSON file per voice, beside its clip (§5.3):

```json
{
  "id": "narrator-a",
  "language": "English",
  "ref_text": "The exact words spoken in the clip.",
  "sha256": "<hash of the WAV as stored>",
  "speaker": "Fictional Voice Actor A",
  "consent_ref": "consent-0001",
  "consent_scope": "internal synthetic narration only",
  "consent_given_at": "2026-11-20",
  "created_at": "2026-11-20T09:00:00+10:00",
  "created_by": "<API key label, never the key>"
}
```

- **The router stores; a person decides.** `consent_ref` points at wherever
  the signed or recorded consent lives. The router does not judge it, but it
  refuses an upload without one.
- **Deletion is real.** `DELETE` removes the clip and the record. One log line
  keeps the id, `sha256`, time and who deleted it, so an audit can show the
  deletion without keeping the voice. Running backends drop their cached copy
  at the next restart or eviction. A rush delete can `POST /_router/unload`
  the speech stanza.
- **Who may be cloned**: only speakers who gave consent for this use. No public
  figures. No clips taken from elsewhere. Test clips are recorded for the test
  (checklist).
- **Exposure**: anyone who can reach `/v1/audio/speech` can clone any clip they
  hold, library or not. Keep speech stanzas behind `security.api_keys` if the
  LAN is not trusted.

## 8. GPU host verification checklist and open questions

Run as the service user. Items 2 to 8 start a backend by hand on a spare port
and a free card, then stop it. Nothing else is changed. `/tmp/voice-test` is
scratch space.

```bash
# 1. Versions: vLLM-Omni must be new enough for persistent voices; llama-server needs v1.0.0+.
/opt/ai/venv-vllm-omni/bin/python -c "import vllm, vllm_omni; print(vllm.__version__, vllm_omni.__version__)"
/opt/ai/bin/llama-server --version

# 2. Start the primary by hand with a scratch voice directory.
mkdir -p /tmp/voice-test/speakers
CUDA_VISIBLE_DEVICES=0 HF_HUB_OFFLINE=1 SPEAKER_SAMPLES_DIR=/tmp/voice-test/speakers \
  VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable \
  /opt/ai/venv-vllm-omni/bin/vllm serve /opt/ai/models/tts/Qwen3-TTS-12Hz-1.7B-Base --omni \
  --deploy-config /opt/ai/config/vllm-omni/qwen3_tts_24gb.yaml \
  --served-model-name qwen3-tts-1.7b --host 127.0.0.1 --port 5960 \
  --trust-remote-code --enforce-eager &
until curl -sf http://127.0.0.1:5960/health; do sleep 2; done

# 3. Voice routes present in this build?
curl -s http://127.0.0.1:5960/openapi.json | python3 -c "import json,sys; print([p for p in json.load(sys.stdin)['paths'] if 'voice' in p or 'speech' in p])"

# 4. First vs second inline clone: time to first byte (reference encode vs cache hit).
REF=$(base64 -w 0 /tmp/ref-fictional-speaker.wav)
for i in 1 2; do curl -s -o /tmp/voice-test/inline-$i.wav -w 'ttfb %{time_starttransfer}s total %{time_total}s\n' \
  http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"model\":\"qwen3-tts-1.7b\",\"input\":\"Testing one two three.\",\"language\":\"English\",\"task_type\":\"Base\",\"ref_audio\":\"data:audio/wav;base64,$REF\",\"ref_text\":\"<transcript of the clip>\",\"response_format\":\"wav\"}"; done

# 5. Base request with ref_audio plus an unknown voice name: 400 or ignored? And with instructions?
curl -s -w ' %{http_code}\n' -o /tmp/voice-test/unknown-voice.out http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"input\":\"x\",\"language\":\"English\",\"task_type\":\"Base\",\"voice\":\"no-such-voice\",\"ref_audio\":\"data:audio/wav;base64,$REF\",\"ref_text\":\"<transcript>\"}"
curl -s -w ' %{http_code}\n' -o /tmp/voice-test/base-instr.out http://127.0.0.1:5960/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"input\":\"x\",\"language\":\"English\",\"task_type\":\"Base\",\"instructions\":\"angry\",\"ref_audio\":\"data:audio/wav;base64,$REF\",\"ref_text\":\"<transcript>\"}"
curl -s -o /dev/null -w 'health %{http_code}\n' http://127.0.0.1:5960/health   # engine still alive?

# 6. Registry: upload, list, file on disk, restart, list again, duplicate name, delete.
curl -s http://127.0.0.1:5960/v1/audio/voices -F "audio_sample=@/tmp/ref-fictional-speaker.wav" \
  -F name=test-voice -F consent=test-consent-0001 -F "ref_text=<transcript>"
curl -s http://127.0.0.1:5960/v1/audio/voices
ls -la /tmp/voice-test/speakers                                    # file name: does it carry the consent label?
#    stop the server (kill %1), repeat step 2, then:
curl -s http://127.0.0.1:5960/v1/audio/voices                      # test-voice restored?
curl -s -w ' %{http_code}\n' http://127.0.0.1:5960/v1/audio/voices -F "audio_sample=@/tmp/ref-fictional-speaker.wav" \
  -F name=test-voice -F consent=test-consent-0001                  # immutable: expect 400
curl -s -X POST http://127.0.0.1:5960/v1/audio/voices/test-voice/cache
curl -s -X DELETE http://127.0.0.1:5960/v1/audio/voices/test-voice; ls -la /tmp/voice-test/speakers

# 7. Can uploads be switched off? Restart with SPEAKER_MAX_UPLOADED=0 and repeat the upload.

# 8. Fallback: inline clone on a free V100; is any transcript field accepted?
/opt/ai/bin/llama-server --help | grep -i -E 'tts|speaker'
#    start the §6.2 command by hand on port 5961 with CUDA_VISIBLE_DEVICES=1, then:
curl -s http://127.0.0.1:5961/v1/audio/speech -H 'Content-Type: application/json' \
  -d "{\"model\":\"qwen3-tts-1.7b-gguf\",\"input\":\"Testing one two three.\",\"lang\":\"en\",\"speaker_ref_b64\":\"$REF\"}" -o /tmp/voice-test/gguf.wav
#    Same with --n-gpu-layers 0 under taskset for the CPU real-time factor.

# 9. Through the router (once staged): model-less voices path is a 404, not the backend.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/v1/audio/voices
```

`/tmp/ref-fictional-speaker.wav` is a clip recorded for the test by someone who
agreed to it. It is not a real person's voice taken from elsewhere.

### Open questions for the operator

1. **Approve the router change for the voice library (§5.3)?** That means
   `voices_dir`, `voice_format`, the three `/v1/audio/voices` routes and the
   injection hook. If not, clients keep sending the clip inline, and named
   voices across nodes stay unsupported.
2. **Which node creates voices**, and may it `rsync` `voices_dir` to the other
   nodes that run a speech stanza?
3. **Consent policy:** who may record a voice, what `consent_ref` must point
   at, and who may delete. Is internal narration the only allowed scope?
4. **Delivery direction (E4):** if a cast voice must change delivery from a
   text instruction, add a VoxCPM2 stanza (~8 GB, GPU 0, same runtime, same
   library). If per-emotion reference clips are enough, stay on Qwen3-TTS Base
   alone.
5. **Drop `path_prefix: /v1/audio/voices`** from the TTS note's primary stanza,
   as §6.1 does?
6. **Watermark:** turn on vLLM-Omni's AudioSeal option once it ships, for any
   audio that may leave the LAN?

## 9. Sources

All read on 2026-11-20 through search-engine extracts (no direct page fetch in
this run).

Consumer contract (not reachable from this run)

- https://git.kevynwatkins.com/hermes/slop-forge/src/branch/master/docs/research/e2-tts-model-selection-hosting.md

vLLM-Omni (primary)

- https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/speech_api/
- https://github.com/vllm-project/vllm-omni/blob/main/docs/serving/speech_api.md
- https://docs.vllm.ai/projects/vllm-omni/en/latest/user_guide/examples/online_serving/text_to_speech/
- https://github.com/vllm-project/vllm-omni/blob/main/docs/user_guide/examples/online_serving/text_to_speech.md
- https://docs.vllm.ai/projects/vllm-omni/en/v0.18.0/user_guide/examples/online_serving/qwen3_tts/
- https://github.com/vllm-project/vllm-omni
- https://pypi.org/project/vllm-omni/
- https://github.com/vllm-project/vllm-omni/pull/4749 (upload validation, consent and length caps)
- https://github.com/vllm-project/vllm-omni/pull/2457 (`/v1/audio/voices/{name}/cache`)
- https://github.com/vllm-project/vllm-omni/pull/8424 (shared voice registry across API processes)
- https://github.com/vllm-project/vllm-omni/pull/8852 (precomputed voices on CustomVoice)
- https://github.com/vllm-project/vllm-omni/pull/7883 (reference cache limits)
- https://github.com/vllm-project/vllm-omni/pull/10795 (length-limit truncation)
- https://github.com/vllm-project/vllm-omni/issues/5789 (`--allowed-local-media-path` not propagated, one model path)
- https://github.com/vllm-project/vllm-omni/issues/7365 and https://github.com/vllm-project/vllm-omni/pull/7366 (AudioSeal watermark)
- https://arxiv.org/pdf/2610.09307 (vLLM-Omni technical report, cold-cache time to first packet)
- https://github.com/vllm-project/production-stack/issues/1210 (`GET /v1/audio/voices` has no body to route on)
- https://github.com/johnrobinsn/qwen3-tts-stream-demo (precomputed voices via `custom_voice_dir`)

Qwen3-TTS

- https://github.com/QwenLM/Qwen3-TTS
- https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-Base
- https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign
- https://qwen.ai/blog?id=qwen3tts-0115
- https://ocdevel.com/blog/20260302-qwen-tts-voice-cloning (reference length guidance, third party)

OpenAI custom voices (the shape vLLM-Omni copies)

- https://developers.openai.com/api/docs/guides/custom-voices
- https://developers.openai.com/api/reference/resources/audio/subresources/voice_consents/methods/create

llama.cpp

- https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
- https://github.com/ggml-org/llama.cpp/blob/master/tools/tts/README.md
- https://github.com/ggml-org/llama.cpp/pull/26254
- https://github.com/dwain-barnes/llama-tts-server

Other candidates

- https://github.com/OpenBMB/VoxCPM
- https://huggingface.co/openbmb/VoxCPM2
- https://recipes.vllm.ai/openbmb/VoxCPM2
- https://github.com/index-tts/index-tts
- https://huggingface.co/IndexTeam/IndexTTS-2.5
- https://github.com/vllm-project/vllm-omni/blob/main/recipes/IndexTeam/IndexTTS-2_5.md
- https://github.com/FunAudioLLM/CosyVoice
- https://huggingface.co/spaces/FunAudioLLM/Fun-CosyVoice3-0.5B/blob/86aee4e98da97f217b765666d6895330c39981f1/cosyvoice/cli/cosyvoice.py
- https://github.com/resemble-ai/chatterbox
- https://github.com/devnen/Chatterbox-TTS-Server
- https://github.com/devnen/Chatterbox-TTS-Server/blob/main/documentation.md
- https://github.com/fishaudio/fish-speech
- https://speech.fish.audio/inference/
- https://fish.audio/blog/what-we-mean-by-open-source-for-s2/
- https://github.com/boson-ai/higgs-audio
- https://github.com/vllm-project/vllm-omni/blob/main/recipes/BosonAI/Higgs-Audio-V3-TTS.md
- https://github.com/SWivid/F5-TTS
- https://github.com/idiap/coqui-ai-TTS
- https://github.com/daswer123/xtts-api-server
- https://github.com/myshell-ai/OpenVoice
- https://github.com/RVC-Boss (SoVITS project; `api_v2.py` in its repository)
- https://github.com/Zyphra/ZONOS3-Mini

Regulation

- https://www.reedsmith.com/our-insights/blogs/viewpoints/102nfqg/always-read-the-label-eu-ai-transparency-rules-and-questions-that-remain-unanswe/

This repo

- [`SPEC.md`](../../SPEC.md) (stanza fields, matcher, peer hop)
- [`docs/research/tts-backends.md`](tts-backends.md) (runtime choice, VRAM, cold start, base stanzas)
- `internal/router/matcher.go`, `internal/router/filters.go`,
  `internal/config/config.go`
- the GPU host's node config under `configs/` (card occupancy, ports)
