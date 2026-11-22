# Changelog

All notable changes to this project are recorded here.
Format: [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/). Versions: [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html).
Dates are YYYY-MM-DD (AEST).

The repo has no release tags yet. `0.1.0` is back-filled from the merges on `master` up to 2026-11-22. Tag it `v0.1.0` when the first release is cut.

## [Unreleased]

### Fixed

- `z-anime-turbo` no longer advertises `img2img`. The live sd-server build aborts on Z-Image edit because positional embeddings omit the reference tokens (#203). The Playground Image tab offers Generate only for that model. A tested patch and build script are in `third_party/stable-diffusion.cpp`. The stanza stays generate-only until a deploy of that binary is approved.
- `/_router/status` and `/v1/models` always publish a `capabilities` array, including an empty one, so a model with no flags is not treated as "inherit every other model's flags". The Image tab follows the selected model's flags (or the `path_default` stanza on Path default).
- A stanza whose backend dies mid-request is reloaded on the next request, without a manual unload. Covered by a test (#203).

### Changed

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
