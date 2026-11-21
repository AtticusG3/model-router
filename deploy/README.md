# sd-router-shim

Wrapper between model-router and sd-server, installed as the stanza command so
the router sees one healthy endpoint. This copy is the source of truth; the
installed binary is replaced by copying this file.

A stanza that sets `image_pipeline` runs the same checks inside the router and
needs no shim; this shim stays supported for nodes that still wrap sd-server
with it. See `SPEC.md` for the option and its presets.

Backend runtime layout (environments, torch tracks, weights, ports, probe):
see [`backends.md`](backends.md).

Usage:

```
sd-router-shim [--prompt-enhance] [--retry-blocked] {port} -- sd-server [args...]
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
was sent.

| Setting | Default | Meaning |
|---|---|---|
| `SHIM_ENHANCER_URL` | `http://127.0.0.1:18080/v1` | chat-completions base URL |
| `SHIM_ENHANCER_MODEL` | first running model that is not an embedding or reranking model | chat model id |
| `SHIM_ENHANCER_TIMEOUT` | `20` | seconds before the template fallback |
| `SHIM_BLOCKED_RETRIES` | `1` | retries, only with `--retry-blocked` |

If the enhancer times out, returns malformed JSON, or returns a caption
without an element that has an in-range bbox, the shim sends a deterministic
template instead: one centred element whose description is the original
prompt. sd-server never receives a broken caption.

The system prompt asks for `style_description` only when the request asks for
a style, medium, camera, lighting or aesthetic; otherwise the shim removes it.

## Eviction

The shim keeps the image model loaded. model-router has no header or query
parameter that asks a request not to cold-load, so the shim checks
`GET {SHIM_ENHANCER_URL}/models` first. When the chat model is not already
running, it skips the call and uses the template. It therefore never starts a
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
