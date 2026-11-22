[Docs home](../README.md) › Research › Fibo 2.0 prompt style

# Fibo 2.0 prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #211. Feeds the `fibo-2.0`
prompt profile (`internal/router/imageprompt_fibo_2.0.go`).

This page records how Fibo 2.0 wants to be prompted, the structured caption
schema it reads, and which requests on the `fibo-2.0-edit-turbo` stanza the
enhancer should touch. Every claim links to its source. A claim that could not
be confirmed from the vendor's own pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files. Field lists in particular were read from extracts, so
the sub-keys under each object are marked `(verify)` where only a community
page or an older Bria repo showed them.

Sources, primary first:

- Bria's model cards on Hugging Face for the variant we serve
  ([edit-turbo card][turbo]) and for the base model ([dev card][dev]), and the
  ModelScope mirror of the Edit Turbo card ([ModelScope][ms]).
- Bria's VGL schema page ([VGL][vgl]) and the `bria-vgl` package that turns a
  short prompt or an edit instruction into a caption ([bria-vgl][pypi]).
- The diffusers pipeline docs for Fibo 2.0 ([diffusers][diffusers]) and the
  diffusers LoRA training notes ([dreambooth][dreambooth]).
- Bria's earlier FIBO and FIBO Edit repos, which publish the same caption
  layout ([FIBO][fibo1], [Fibo-Edit][fiboedit]).
- The stable-diffusion.cpp pull requests that add Fibo 2.0 to sd-server
  ([sd.cpp #2180][sdcpp], [sd.cpp #2216][sdtext]).
- Community pages, used only where they agree with the vendor pages
  ([Forge Neo][forge], [FastSD CPU][fastsd], [T2I card][t2i], [SDNQ card][sdnq],
  [PS5SD #21][ps5sd]).

## 1. What the model is

Fibo 2.0 is an 8B flow-matching image model from Bria that works in the latent
space of the FLUX.2 VAE. Prompts are encoded by a 4B vision-language model,
and context images for edits go through both that encoder and the VAE
([turbo][turbo]). It ships as Dev (generation and editing, 50 steps, CFG on)
and Edit Turbo (distilled, 4 steps, no CFG) ([dev][dev], [turbo][turbo]).

The tracked stanza runs **Edit Turbo**. Its card says Turbo is for editing
only, and that text-to-image should use Dev ([turbo][turbo]). A community
card reports that Edit Turbo still draws a VGL caption with no input image,
in 4 steps ([t2i][t2i]) `(verify)`. Weights are CC BY-NC 4.0
([turbo][turbo]).

## 2. Findings

### Structured JSON, not prose

Fibo 2.0 is trained on structured JSON captions and "will not work well with
freeform text" ([dev][dev]). Bria calls the format VGL (Visual GenAI
Language) ([vgl][vgl]). Its own tool for turning a short prompt into a caption
is `bria-vgl`: `create(Generate(prompt=...))` for generation and
`modify(EditImages(prompt=..., images=[...]))` for edits ([pypi][pypi],
[turbo][turbo]).

Plain text still runs. The diffusers pipeline accepts a plain string prompt
([diffusers][diffusers]), and community front ends say plain sentences work,
with the caption as the better input ([forge][forge], [fastsd][fastsd]).

### Serialisation

Before encoding, the caption is cleaned the way the training captions were:
empty values and the `aesthetic_score` and `preference_score` keys are
dropped, and the JSON is written compactly,
`json.dumps(caption, separators=(",", ":"), ensure_ascii=False)`
([dev][dev], [diffusers][diffusers]).

### Typical length

The original FIBO captions run to about 1,000 words ([FIBO][fibo1]). No page
found gives a target length for a Fibo 2.0 caption `(verify)`. The profile
lets the enhancer fill the schema rather than aim for a word count.

### Tags versus prose

Neither. Field values are short natural-language phrases inside the JSON
([vgl][vgl], [fiboedit][fiboedit]). No vendor page uses booru tags or
`(word:1.3)` weighting.

### Negative prompt

Ignored on the variant we serve. Edit Turbo runs at `guidance_scale=1.0`, so
the negative branch never runs and "negative_prompt has no effect"
([sdnq][sdnq], [fastsd][fastsd]). The tracked stanzas run sd-server at
`--cfg-scale 1.0`. Dev uses a structured negative caption at
`guidance_scale=5.0` ([dev][dev]). The profile never writes or edits
`negative_prompt`.

### Text rendering

Text goes in `text_render`, an optional array of entries each holding the
exact `text` and its placement and look ([vgl][vgl], [diffusers][diffusers]).
A minimal caption can be just `short_description` plus `text_render`
([diffusers][diffusers]). Fibo 2.0 renders text better when the encoder also
sees each text drawn on its own; sd-server can do this, off by default, with a
FreeType build and fonts ([diffusers][diffusers], [sd.cpp #2216][sdtext]).
A third-party guide says to leave `text_render` empty unless the user gives
exact text `(verify)`. The profile asks for one entry per piece of requested
text, copied exactly, and none otherwise. A double-quoted span is not always
text to draw (it may be a name or a style reference), so the reply check
rejects a caption only when the span appears in none of its string values.

### txt2img versus img2img and edit

**Only txt2img.** An edit caption is the caption of the *result*, written by a
vision model that sees the source image, plus an `edit_instruction`
([turbo][turbo], [diffusers][diffusers], [pypi][pypi]). The router's enhancer
is a text-only chat model and is never shown the image, so it cannot write
that caption. sd-server already wraps a plain edit prompt itself: its Fibo 2.0
conditioner builds `{"structured_caption": ..., "edit_instruction": ...}`
from the instruction ([sd.cpp #2180][sdcpp]). The shared caption step already
skips img2img, so edits (and inpaint, which is img2img with a mask) reach
sd-server unchanged.

So on the tracked Edit Turbo stanza the profile only acts on the Generate
path, which the playground offers for every image model. Enhancing there is
low risk: a VGL caption is what the model was trained on, and the community
txt2img card feeds Edit Turbo exactly that ([t2i][t2i]) `(verify)`.

### Already-structured prompts

A prompt that is already a JSON object passes through untouched, as for
ideogram4. A client that sends its own VGL caption, or a
`{"edit_instruction": ...}` object, keeps it.

### Fallback

The verbatim prompt. Fibo 2.0 reads plain text, if not as well as a caption
([diffusers][diffusers], [forge][forge]), so a short prompt is still valid.
Bria publishes no minimal template that would beat a plain prompt, so the
profile does not invent one, and the ideogram4 template would be the wrong
schema. Source header: `X-SD-Shim-Prompt-Source: original`.

## 3. The VGL caption schema

Top-level keys, in the order the vendor's empty caption lists them
([dev][dev], [diffusers][diffusers]). `text_render` is not in that empty
caption; its place after `style_medium` follows the earlier FIBO schema
([FIBO][fibo1]) `(verify)`. Required versus optional follows Bria's VGL page
([vgl][vgl]).

| Key | Type | Required | Meaning |
|---|---|---|---|
| `short_description` | string | yes | The semantic anchor: what the image shows ([vgl][vgl]) |
| `objects` | array of object | no `(verify)` | One entry per subject ([fiboedit][fiboedit]) |
| `background_setting` | string | yes | Environment, location and scene ([vgl][vgl]) |
| `lighting` | object | yes | `conditions`, `direction`, `shadows` ([vgl][vgl]) |
| `aesthetics` | object | yes | `composition`, `color_scheme`, `mood_atmosphere` ([vgl][vgl], [diffusers][diffusers]) |
| `photographic_characteristics` | object | no `(verify)` | `depth_of_field`, `focus`, `camera_angle`, `lens_focal_length` ([FIBO][fibo1]) |
| `style_medium` | string | yes | photography, digital illustration, 3D render, vector art, watercolour, oil painting, pencil sketch, anime or pixel art ([vgl][vgl]) |
| `text_render` | array of object | no | Text to draw: `text` plus placement and look ([vgl][vgl]) |
| `context` | string | no | Narrative or situational framing ([vgl][vgl]) |
| `artistic_style` | string | no | Stylistic references; at most three words in the FIBO Edit format ([vgl][vgl], [fiboedit][fiboedit]) |

`objects` entries ([fiboedit][fiboedit]) `(verify)` for Fibo 2.0:
`description`, `location`, `relationship`, `relative_size` (small, medium or
large), `shape_and_color`, `texture`, `appearance_details`, `orientation`,
`number_of_objects`, and for people `pose`, `expression`, `clothing`,
`action`, `gender`, `skin_tone_and_texture`.

`text_render` entries ([diffusers][diffusers]) `(verify)`: `text`,
`location`, `size`, `color`, `font`, `appearance_details`.

Edit prompts add `edit_instruction`, with the caption of the result either as
the other keys or as a `structured_caption` string or object
([diffusers][diffusers], [dreambooth][dreambooth]). The profile never writes
edit captions (see above).

Bria's VGL page says a later VGL version moves colour fields to
`primary_color` and `secondary_color` RGB arrays ([vgl][vgl]) `(verify)`. The
profile follows the current string form.

## 4. Golden example

Request (1344 by 768 canvas):

```text
a bakery at dawn with a sign that says "FRESH BREAD"
```

Caption sent to sd-server (compact on one line; wrapped here to read):

```json
{"short_description":"A small bakery storefront at dawn with a painted sign above its front window.",
 "objects":[{"description":"a small bakery storefront with a wide front window full of loaves and pastries",
  "location":"center","relative_size":"large within frame",
  "shape_and_color":"rectangular shopfront painted dark green with cream trim",
  "texture":"weathered painted wood and clean glass","appearance_details":"warm yellow light glows from inside",
  "relationship":"the sign hangs above the window","orientation":"facing the viewer"}],
 "background_setting":"a quiet cobbled street at dawn with terrace houses on either side",
 "lighting":{"conditions":"early morning light mixed with warm light from the shop",
  "direction":"from inside the shop and from the left","shadows":"long soft shadows across the cobbles"},
 "aesthetics":{"composition":"centred, eye level, symmetrical",
  "color_scheme":"dark green, cream and warm yellow against pale blue","mood_atmosphere":"calm and welcoming"},
 "photographic_characteristics":{"depth_of_field":"deep","focus":"sharp on the storefront and sign",
  "camera_angle":"eye level","lens_focal_length":"standard"},
 "style_medium":"photography",
 "text_render":[{"text":"FRESH BREAD","location":"top center, above the window","size":"large",
  "color":"cream","font":"hand-painted serif","appearance_details":"painted on a dark green board"}],
 "context":"a shopfront photograph for a local bakery","artistic_style":"warm documentary photography"}
```

The enhancer's reply also carried `aesthetic_score` and `preference_score`;
the check dropped them. This pair is the fixture in
`internal/router/imageprompt_fibo_2.0_test.go`, which also covers the
verbatim fallback (enhancer down, prose reply, an ideogram4-shaped reply, a
dropped quoted span), `enhance_prompt: false`, a prompt that is already a VGL
caption, and an img2img edit.

## 5. What the profile does

- System prompt: the schema above with the keys in order, the required keys,
  one `objects` entry per subject, layout for the canvas, one `style_medium`,
  exact `text_render` text and none that was not asked for, no score keys and
  no negative prompt, JSON only.
- User message: the canvas size and the raw request (shared).
- Reply check: a JSON object (a markdown fence is stripped) with a non-empty
  `short_description`, `background_setting` and `style_medium`, `lighting` and
  `aesthetics` objects, `objects` entries with a `description`, `text_render`
  entries with `text`, and every double-quoted span of the request kept word
  for word in some string value. Keys outside the schema, empty top-level values and the
  two score keys are dropped, and the caption is written compactly without
  HTML escaping.
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Runs on txt2img only. img2img and inpaint edits are left to sd-server's own
  edit wrapping.
- Enabled on the two tracked `fibo-2.0-edit-turbo` stanzas named in #211, with
  the same enhancer model and 45 second budget as the other families.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[turbo]: https://huggingface.co/briaai/Fibo-2.0-edit-turbo
[dev]: https://huggingface.co/briaai/Fibo-2.0-dev
[ms]: https://www.modelscope.ai/models/briaai/Fibo-2.0-edit-turbo
[vgl]: https://docs.bria.ai/vgl
[pypi]: https://pypi.org/project/bria-vgl/0.8.0/
[diffusers]: https://huggingface.co/docs/diffusers/main/api/pipelines/bria_fibo2
[dreambooth]: https://github.com/huggingface/diffusers/blob/main/examples/dreambooth/README_bria_fibo2.md
[fibo1]: https://github.com/Bria-AI/FIBO
[fiboedit]: https://github.com/Bria-AI/Fibo-Edit
[sdcpp]: https://github.com/leejet/stable-diffusion.cpp/pull/2180
[sdtext]: https://github.com/leejet/stable-diffusion.cpp/pull/2216
[forge]: https://github.com/aoleg/fibo2-forge-neo
[fastsd]: https://github.com/rupeshs/fastsdcpu
[t2i]: https://huggingface.co/lvladikov/FIBO2-Edit-Turbo-T2I
[sdnq]: https://huggingface.co/OzzyGT/fibo_2_0_edit_turbo_sdnq_dynamic_4bit
[ps5sd]: https://github.com/Darkatek7/PS5SD/issues/21

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Krea 2 prompt style](krea2-prompt-style.md): a prose family, for contrast with this structured one.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
