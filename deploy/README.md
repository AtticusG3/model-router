# sd-router-shim

> **Last reviewed:** 2026-11-22

Wrapper between model-router and sd-server, installed as the stanza command so
the router sees one healthy endpoint. This copy is the source of truth; the
installed binary is replaced by copying this file.

A stanza that sets `image_pipeline` parses and validates the same checks in the
router. The router runs the img2img payload checks itself, warms sd-server's
LoRA cache and retries one `invalid lora path` reply, and applies the
blocked-card retry when the stanza sets `retry_blocked`; caption enhancement
still ships in the shim, so keep the shim on a stanza that needs it. This shim
stays supported for nodes that still wrap sd-server with it. See `SPEC.md` for
the option and its presets.

Backend runtime layout (environments, torch tracks, weights, ports, probe):
see [`backends.md`](backends.md).

Usage:

```
sd-router-shim [--prompt-enhance] [--enhancer-timeout SECONDS] [--retry-blocked] {port} -- sd-server [args...]
```

The shim starts sd-server on a free loopback port, answers `GET /health`,
warms the LoRA cache, validates img2img payloads, and retries once when
sd-server rejects a LoRA path.

## Prompt enhancement

`--prompt-enhance` (or a truthy `SHIM_PROMPT_ENHANCE`) rewrites a plain
`/sdapi/v1/txt2img` prompt into a structured JSON caption with one bbox per
element. Enable it only on the stanza whose model expects those captions;
other stanzas are untouched.

A request is forwarded unchanged when the prompt is already a JSON object,
when the body sets `"enhance_prompt": false`, or when it is an img2img call.
The response carries `X-SD-Shim-Prompt-Original` and
`X-SD-Shim-Prompt-Enhanced` (percent-encoded) so the activity log shows what
was sent, and `X-SD-Shim-Prompt-Source: enhancer` or `template` to say which
path built it. None of these headers is set when the body went through
untouched.

| Setting | Default | Meaning |
|---|---|---|
| `SHIM_ENHANCER_URL` | `http://127.0.0.1:18080/v1` | chat-completions base URL |
| `SHIM_ENHANCER_MODEL` | first running model that is not an embedding or reranking model | chat model id; must be listed and running |
| `SHIM_ENHANCER_TIMEOUT` | `20` | seconds before the template fallback |
| `--enhancer-timeout SECONDS` | unset | same as `SHIM_ENHANCER_TIMEOUT`, and wins over it |
| `SHIM_BLOCKED_RETRIES` | `1` | retries, only with `--retry-blocked` |

Both timeout settings are per stanza: set the env var in the stanza's `env:`
list, or put the flag in the stanza command so it shows next to the other
shim flags. The timeout is one wall-clock budget for the catalogue lookup and
the chat call together, so a slow or trickled reply cannot hold the image
request past it.

Any enhancer failure only drops the rewrite: a timeout, a model that is not
running, an unreadable catalogue, a connection or HTTP error, a reply that is
not JSON or has no `choices`, a caption without an element that has an
in-range bbox, a malformed `SHIM_ENHANCER_URL`, or any other exception. The
shim then sends a deterministic template: the original prompt as the
high-level description and as one centred element. sd-server never receives a
broken caption, and stderr gets one line with the reason.

Bboxes follow the image model's upstream caption spec (the field table in
`docs/prompting.md` of the model's repo):
`[y_min, x_min, y_max, x_max]` on a 0-1000 grid, origin top-left. The
enhancer is asked for `[x_min, y_min, x_max, y_max]`, which chat models get
right more often, and the shim swaps each validated bbox to y-first before
forwarding. It also writes each element's keys in the spec's order (`obj`:
`type, bbox, desc, color_palette`; `text`: `type, bbox, text, desc,
color_palette`), drops keys the spec does not list, and drops elements without
a valid bbox. A prompt the client already sent as a JSON caption is forwarded
as is, so it must already be y-first.

The system prompt asks for `style_description` only when the request asks for
a style, medium, camera, lighting or aesthetic; otherwise the shim removes it.

## Eviction

The shim keeps the image model loaded. model-router has no header or query
parameter that asks a request not to cold-load, so the shim checks
`GET {SHIM_ENHANCER_URL}/models` first. When the chat model is not already
running, is missing from the catalogue, or the catalogue cannot be read, it
skips the call and uses the template. It therefore never starts a
load that could evict the image model, and it never waits on a slot while the
image model holds a card. If a request later lands on a stopped model it falls
back to the template as well. Coupling the check and the chat call in the
router would close the remaining race, but needs a router-side flag that does
not exist today.

## Blocked-card retry

`--retry-blocked` resamples up to `SHIM_BLOCKED_RETRIES` times when the
rendered first image is the near-uniform grey card. Detection samples up to
4096 pixels and requires a low luminance spread, a mid-grey mean and a low
channel difference. Every retry gets a new seed and a stderr line. This is off
by default.

## Tests

```
python3 -m unittest discover -s deploy/tests -v
```

The tests use a loopback fake enhancer and need no GPU or backend.
