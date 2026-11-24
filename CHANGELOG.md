# Changelog

All notable changes to this project are recorded here.
Format: [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/). Versions: [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html).
Dates are YYYY-MM-DD (AEST).

Tags: `v0.1.0` (back-filled from the merges on `master` up to 2026-11-22, re-pointed onto the rewritten history) and `v0.2.0` (22 Nov 2026). Cut a release by tagging `master` and pasting the version's section into the release body.

## [Unreleased]

### Changed

- Deploy docs now describe the fleet they deploy to: the four nodes that run the router, the binary/config pairing rule with its real failure message, the tracked-vs-live diff recipe, and a flowchart for the binary upgrade. The `model_id` catalog and the VRAM ladder's hardware note follow the live configs (#222).

### Added

- `deploy/swap-over.md`: "Config and binary move together" and "Check a tracked config against a live node" (#222).
- `api_type: decision` with `decision_style: systemone`: decision models (Kev-9B on llama-server) are served on `POST /v1/systemone`, passed through unchanged. A malformed request is a 400 and a malformed reply a 502, both naming the model; probabilities and confidence reach the client byte for byte, and a 402 or 403 is never retried. A model-less `/v1/systemone` request takes the decision default. The Playground has a Decision tab, Activity rows show each answer and the lowest confidence, and `cmd/fake-model` answers `/v1/systemone`. Example stanza `configs/kev-9b.example.yaml`; research in `docs/research/decision-models.md`, design in ADR 0004. No node config changes (#225).

## [0.3.0] - 2026-11-22

### Added

- `image_pipeline.caption: z-image`: txt2img prompts for Z-Image stanzas are expanded into detailed prose by the enhancer model, falling back to the original prompt verbatim. Enabled on the tracked Z-Image base and Juggernaut Z Fast stanzas (#205).
- `image_pipeline.caption: z-anime`: txt2img prompts for Z-Anime stanzas, including booru-style tag lists, are rewritten into front-loaded descriptive prose, falling back to the original prompt verbatim. Enabled on the tracked `z-anime-turbo` stanzas (#206).
- `image_pipeline.caption: krea2`: txt2img prompts for Krea 2 Turbo are expanded into one natural-language paragraph following the vendor's expansion rules, falling back to the original prompt verbatim. Enabled on the tracked `krea-2-turbo` stanzas; the character creator, Ostris edit and Krea 2 fine-tune stanzas stay unenhanced (#207).
- `image_pipeline.caption: flux2-klein`: txt2img prompts for FLUX.2 Klein are expanded into one English prose paragraph following the vendor's Klein guidance (subject first, detailed lighting, quoted text and hex colours kept exact), falling back to the original prompt verbatim. Klein does no prompt upsampling of its own. Enabled on the two tracked `flux2-klein` stanzas; edits stay as written (#209).
- `image_pipeline.caption: fibo-2.0`: txt2img prompts for Fibo 2.0 are rewritten into Bria's structured VGL JSON caption, checked against the researched schema and sent compact; a prompt that is already JSON passes through, and a failed enhancer call sends the original prompt verbatim. img2img and inpaint edits are left alone. Enabled on the tracked `fibo-2.0-edit-turbo` stanzas (#211).
- `image_pipeline.caption: flux3-klein`: txt2img prompts for FLUX.3 Image Klein are expanded into the long scene caption the vendor's prompt upsampler writes, falling back to the original prompt verbatim. Enabled on the tracked `flux3-klein` stanzas; img2img and edit prompts stay unenhanced (#210).
- `image_pipeline.caption: qwen-image-2.1`: txt2img prompts for Qwen-Image 2.1 are expanded into one long English description following the vendor's text-to-image rewriter, with on-image text kept exact and in its own script, falling back to the original prompt verbatim. Enabled on the tracked `qwen-image-2.1` stanza; `qwen-image-edit-2511` stays unenhanced (#208).

### Fixed

- `z-anime-turbo` no longer advertises `img2img`. The live sd-server build aborts on Z-Image edit because positional embeddings omit the reference tokens (#203). The Playground Image tab offers Generate only for that model. A tested patch and build script are in `third_party/stable-diffusion.cpp`. The stanza stays generate-only until a deploy of that binary is approved.
- `/_router/status` and `/v1/models` always publish a `capabilities` array, including an empty one, so a model with no flags is not treated as "inherit every other model's flags". The Image tab follows the selected model's flags (or the `path_default` stanza on Path default).
- A stanza whose backend dies mid-request is reloaded on the next request, without a manual unload. Covered by a test (#203).

### Changed

- The image caption step is now a table of prompt profiles keyed by `image_pipeline.caption`; ideogram4 behaviour is unchanged (#205).

- Brought the docs up to the docs standard: a docs index, diagrams, page stamps and backlinks, and the docs rule in `AGENTS.md` (#185).

## [0.1.0] - 2026-11-22

### Added

- One static router binary per node, configured by YAML: on-demand backend loading, supervision, idle unload and a per-GPU VRAM ledger.
- Peer mesh: telemetry pull, spill to a peer when the local node is full or lacks the model, and optional LAN discovery.
- Per-GPU `headroom_mb` the node never books (#4).
- Start cooldown that refuses a model which just failed to start (#2).
- A failed load walks on to the next router peer (#5).
- Load progress streamed to chat clients while a model starts (#17).
- Operator WebUI: live Logs page (#14), tabbed Playground (#24), activity previews and live views (#56), media-aware Activity rows for audio, video and realtime (#70).
- Async video and music or sound-effect job APIs, speech with a voice library, transcription and realtime sessions.
- Deployed stanzas on the main node for transcription (#74), speech and voice cloning (#79) and the realtime voice cascade (#82).

### Fixed

- Activity shows only generation requests, not metrics or static asset polling (#6).
- Activity thumbnails load for image generations whose response capture was truncated (#62).
- Activity origin and peer filters no longer hide every row (#186).
