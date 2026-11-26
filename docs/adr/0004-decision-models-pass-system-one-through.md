[Docs home](../README.md) › Decisions › Decision models pass System One through

# Decision models pass System One through

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-26

Decision models (Kev-9B, d1-Omni-2B, NeoHorse-Jev-4B, Laya) do not speak `/v1/chat/completions`. Every one that resolved in the [research](../research/decision-models.md) answers the System One API, `POST /v1/systemone`, and upstream llama.cpp PR #29818 added `/v1/systemone` for Kev-9B and d1-Omni-2B. The smoke test in [backends §8](../../deploy/backends.md#8-decision-models) confirmed Kev-9B runs on buster's RTX 3090 using `/home/kevyn/infra/llama.cpp-olala/build/bin/llama-server` (`0.5.0-dev`, commit `55c22dc`), while `/opt/ai/bin/llama-server` (`llama.cpp-unified`, `10881`) lacks `/v1/systemone`. Because the router only passes the System One shape through, pointing `command` at the `llama.cpp-olala` build (or another System One server) needs no router change. So a decision stanza is `api_type: decision` with `decision_style: systemone`, and the router passes the client's System One body to the backend and the backend's reply to the client without rewriting either (a peer hop sets `model` and re-encodes the request JSON, as for every `api_type`). It checks only the envelope: `Content-Type: application/json`, a JSON object with a `state` and a non-empty `questions` object whose entries each have a `type` (else 400 naming the model, before any load), and a 2xx reply that is a JSON object with an `answers` object (else 502 naming the model). Any other status, 402 and 403 included, reaches the client once and is never retried; only a pool tries its next target on a 5xx, as for any pool. Probabilities, confidence and any reported latency therefore arrive byte for byte. The Playground Decision tab speaks the same System One shape. We did not build a normalised `/v1/decisions` contract with per-backend adapters: the one shape already covers every resolved backend, and adapters would make the router own a mapping nobody needs yet. Add it, as a second `decision_style`, when a backend that does not speak System One is actually served.

## See also

- [Decision models research](../research/decision-models.md): the candidates, their licences and the evidence for System One.
- [SPEC.md: Decision models](../../SPEC.md#decision-models): the contract as implemented.
- [Glossary](../../GLOSSARY.md): the terms these decisions use.

## Linked from

<!-- backlinks:start -->
- [Decision models](../research/decision-models.md)
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
