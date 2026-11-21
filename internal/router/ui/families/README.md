# Model family icons

> **Last reviewed:** 2026-11-22

Bundled into the binary with the UI (see `internal/router/ui.go`); the UI never
fetches an icon over the network. The file name is the family id from
`internal/router/family.go`.

| File | Family (maker) | Source | Licence |
|------|----------------|--------|---------|
| `qwen.svg` | Qwen (Alibaba) | Simple Icons 16.38.0, `qwen` path, colour `#6950EF` | CC0-1.0 |
| `mistral.svg` | Mistral (Mistral AI) | Simple Icons 16.38.0, `mistralai` path, colour `#FA520F` | CC0-1.0 |
| `llama.svg` | Llama (Meta) | Simple Icons 16.38.0, `meta` path, colour `#0467DF` | CC0-1.0 |
| `gemma.svg` | Gemma (Google) | Simple Icons 16.38.0, `google` path, colour `#4285F4` | CC0-1.0 |
| `deepseek.svg` | DeepSeek (DeepSeek) | Simple Icons 16.38.0, `deepseek` path, colour `#5786FE` | CC0-1.0 |
| `flux.svg` | FLUX (Black Forest Labs) | Lettermark drawn for this repo | CC0-1.0 |
| `phi.svg` | Phi (Microsoft) | Lettermark drawn for this repo | CC0-1.0 |
| `ideogram.svg` | Ideogram (Ideogram) | Lettermark drawn for this repo | CC0-1.0 |
| `krea.svg` | Krea (Krea) | Lettermark drawn for this repo | CC0-1.0 |
| `fibo.svg` | FIBO (Bria) | Lettermark drawn for this repo | CC0-1.0 |
| `whisper.svg` | Whisper (OpenAI) | Lettermark drawn for this repo | CC0-1.0 |
| `hermes.svg` | Hermes (Nous Research) | Lettermark drawn for this repo | CC0-1.0 |

Simple Icons (https://simpleicons.org) releases its icon data under CC0-1.0.
The marks are still their owners' trademarks; they are shown here only to
identify each model's maker. Simple Icons' `flux` icon is a different project,
so FLUX uses a lettermark. Every colour has at least 3:1 contrast against the
dashboard panel (`#141d29`).

To add a family: add a row to the table in `family.go`, drop `<id>.svg` here
(24x24 viewBox, `aria-hidden="true"`, no `<title>`), and add it to this list.
