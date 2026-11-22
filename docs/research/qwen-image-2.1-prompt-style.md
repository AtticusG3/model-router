[Docs home](../README.md) › Research › Qwen-Image 2.1 prompt style

# Qwen-Image 2.1 prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #208. Feeds the
`qwen-image-2.1` prompt profile (`internal/router/imageprompt_qwenimage21.go`).

This page records how Qwen-Image 2.1 wants to be prompted, with most weight on
text rendering, and whether the tracked `qwen-image-edit-2511` stanza can
share the profile. Every claim links to its source. A claim that could not be
confirmed from the vendor's own pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files. The base model card at `Qwen/Qwen-Image-2.1` on
Hugging Face did not come up in any search, so the facts below lean on the
vendor's GitHub repo, the vendor's prompt-enhancer model card and the
diffusers docs `(verify)` against the base card.

Sources, primary first:

- The vendor's GitHub repo ([repo][repo]), its prompt-rewrite README
  ([pe readme][pereadme]) and its text-to-image rewriter system prompt
  ([t2i system prompt][pesys]), also shipped with the enhancer model
  ([pe card][pecard], [system_prompt.txt][pesyshf]).
- The diffusers pipeline docs for Qwen-Image 2.1 ([diffusers][diffusers]),
  the vLLM-Omni recipe ([vllm][vllm]) and the DiffSynth-Studio page
  ([diffsynth][diffsynth]).
- The repackaged weights that name the vendor repos ([comfy][comfy]) and the
  GGUF repack the tracked stanza loads ([gguf][gguf]).
- The original Qwen-Image card, rewriter and blog, for the family's text
  conventions ([v1 card][v1card], [v1 prompt utils][v1utils],
  [v1 blog][v1blog]).
- Third-party reviews and guides, used only where they agree with the vendor
  pages ([review][review], [pixeldojo][pixeldojo]).

## 1. What the model is

Qwen-Image 2.1 is the vendor's unified text-to-image and image editing model,
released in September 2026 with a Turbo (8-step, CFG 1) checkpoint beside it
([comfy][comfy], [diffsynth][diffsynth]). It is a 7B diffusion transformer
with a Qwen3-VL text encoder ([review][review]) `(verify)`. Alongside it the
vendor released two prompt-rewriter models, PE-T2I for text-to-image and
PE-I2I for editing ([repo][repo], [pe card][pecard]).

The tracked `qwen-image-2.1` stanza runs the base (not Turbo) Q8_0 GGUF in
sd-server with a Qwen3-VL 8B text encoder, 20 steps and `--cfg-scale 6.0`.
GGUF repacks for sd-server exist ([gguf][gguf]); which one the stanza loads is
not recorded `(verify)`.

## 2. Findings

### Prose versus tags

Prose. The vendor's rewriter "turns a short, rough image request into a long
English prompt that reads like a description of the finished picture"
([pe readme][pereadme]). No vendor page uses tag lists. The enhancer turns
tags and short prompts into one paragraph.

### Structure and length

Long. The vendor README says "For best results, we recommend using the
official prompt rewriting models to expand short prompts into detailed,
high-quality descriptions" ([repo][repo]). The rewriter system prompt asks
for ([pesys][pesys], [pe card][pecard]):

- one long English paragraph of about twenty sentences and four to five
  hundred words, and "a thin brief never buys a thin description";
- "Observe, don't instruct": present tense, third person, declarative, with
  no "you", "create", "make sure" or "the AI should", and observer hedges
  such as "appears to be";
- a fixed opening sentence: `The image is a <vertical / wide / square / tall>
  <style> <photograph · poster · illustration · ...> of <subject>, <the
  background and its palette>.`;
- every piece of legible text in reading order, then the regions of a divided
  frame (poster, page, interface, busy wide scene) walked in order;
- physical consistency: shadows away from the light, matching reflections,
  consistent scale;
- no aspect ratio, resolution or pixel count in the description.

The rewriter answers with JSON, `{"rewritten_prompt": ..., "wh_ratio":
"16:9"}`, after a think block ([pe card][pecard]). The router cannot change
the canvas a client asked for, so the profile asks for the prose alone and
gives the enhancer the canvas so it can pick vertical, wide or square.

Short prompts still work: the diffusers example is "A capybara wearing a
wizard hat, oil painting" ([diffusers][diffusers]).

### Quality tags and weighting

Not used. The rewriter forbids quality boosters such as "masterpiece", "8K",
"highly detailed" and "award-winning" ([pesys][pesys]). The original
Qwen-Image card appended ", Ultra HD, 4K, cinematic composition."
([v1 card][v1card]); 2.1's own rewriter drops that, so the profile follows
2.1. No weighting syntax was found.

### Negative prompts

Supported only with CFG on. The vendor's default is 40 steps with no guidance
(`true_cfg_scale = 1.0`), and a negative prompt only takes effect with
`true_cfg_scale > 1` ([diffusers][diffusers], [vllm][vllm]). DiffSynth
defaults the negative prompt to a single space ([diffsynth][diffsynth]). The
Turbo checkpoint runs at CFG 1 and ignores it ([pixeldojo][pixeldojo]).

The tracked stanza runs sd-server at `--cfg-scale 6.0`, so a client's
negative prompt is used there `(verify)` how sd-server maps that to true CFG.
The profile never writes or edits `negative_prompt`; a client's value reaches
sd-server untouched.

### Text rendering

This is the family's headline skill. The original model put text to draw in
double quotes in the prompt (a chalkboard reading "Qwen Coffee 😊 $2 per
cup") ([v1 card][v1card]), and its edit rewriter wraps all text in English
double quotes and keeps its language and capitalisation
([v1 prompt utils][v1utils]). The 2.1 rewriter lists every piece of legible
text in reading order and keeps it in its original script while the rest of
the prompt is English ([pesys][pesys]). The vendor's benchmark scores text
accuracy, layout, font and cross-lingual text separately
([review][review]); the family has rendered Chinese and English text since
the first release ([v1 blog][v1blog]). Whether the 2.1 announcement itself
claims bilingual text is disputed between reviews `(verify)`.

The profile tells the enhancer to copy each requested word character for
character inside English double quotes, with its place, typeface, size and
colour, and never to add text that was not asked for. The reply check
rejects a rewrite that drops a quoted span of the original prompt, whether it
was quoted with `"…"`, `“…”` or `「…」`, so a translated or altered sign falls
back to the prompt verbatim.

### Bilingual requests

The rewriter takes a request "in any language" and always writes English,
except for the text drawn in the image ([pe readme][pereadme],
[pesys][pesys]). The profile does the same.

### Transparent output

2.1 can generate RGBA images when the prompt starts with a fixed trigger:
"This is an RGBA image with transparency. The image has alpha channel and the
background is transparent." ([repo][repo]). The profile tells the enhancer to
open its reply with both sentences word for word (or the first alone, if
that is all the request has), and the reply check rejects a reply that does
not. Whether
sd-server returns the alpha channel is untested `(verify)`.

### txt2img versus img2img and edit

Only txt2img. The vendor's edit rewriter is a separate model that reads the
source images and writes a precise edit instruction; the two rewriters'
prompts "are not interchangeable" ([pe readme][pereadme]). The router's
enhancer is text only and cannot see the image, and the shared caption step
already skips img2img.

### Fallback

The verbatim prompt. The model reads plain text and short prompts work
([diffusers][diffusers]). The ideogram4 JSON template would be read as
literal text.

### Enhancer model

The vendor's own rewriter is a 9B fine-tune ([repo][repo]). No tracked stanza
serves it, and its JSON-after-think answer does not fit the shared reply
check. The profile uses the shared enhancer stanza (`qwen38-flash-next`) with
the rewriter's rules as a system prompt. Serving the vendor rewriter instead
is a possible follow-up, not part of #208.

## 3. Variants

| Stanza | What it is | Profile |
|---|---|---|
| `qwen-image-2.1` | Qwen-Image 2.1 base, txt2img and edit | `qwen-image-2.1` |
| `qwen-image-edit-2511` | Older edit-only model, Qwen2.5-VL text encoder | none |

**qwen-image-edit-2511.** A separate, older model from the edit line. Its
prompts are edit instructions written against the input image, and the
vendor's rewriter for it reads the image too ([edit 2511 space][edit2511],
[v1 prompt utils][v1utils]). The caption step only runs on txt2img, so there
is nothing for this profile to do there. It stays unenhanced.

## 4. Golden example

Request (1664 by 928 canvas):

```text
a night market noodle stall with a sign that says "夜市面馆" and "NIGHT NOODLES"
```

Enhanced prompt:

```text
The image is a wide photograph of a narrow night-market food stall, its glowing signboard set against a dark street washed in red and amber light. Above the stall hangs a long rectangular lightbox sign whose upper line reads "夜市面馆" in large bold white brush-style characters, and whose lower line reads "NIGHT NOODLES" in smaller condensed red capitals. Under the sign, a cook in a white apron and dark cap stands behind a steel counter, lifting a tangle of noodles from a steaming pot with long wooden chopsticks. Steam rises past the hanging bulbs and catches their warm light. Stacked ceramic bowls, a bottle of chilli oil and a row of soy sauce jars line the counter in the foreground. To the left, red paper lanterns sway on a wire, and to the right a second stall fades into soft shadow. The wet asphalt in front of the stall reflects the sign and the lanterns as long smeared streaks of colour. The light comes from the sign and the bulbs above the counter, so the cook's face is lit from above and the shadows of the bowls fall toward the street. The scene appears to be shot at eye level with a 35mm lens, with a shallow depth of field that keeps the cook and the sign sharp. The palette is deep red, amber and warm white against near-black, and the style is a candid documentary street photograph.
```

The golden reply is shorter than the vendor's four to five hundred words to
keep the fixture readable. This pair is the fixture in
`internal/router/imageprompt_qwenimage21_test.go`, which also covers the
verbatim fallback (enhancer down, HTTP 500, dropped quoted text, a JSON reply
in the vendor's shape), `enhance_prompt: false` and img2img.

## 5. What the profile does

- System prompt: the vendor rewriter's rules as one English paragraph of
  about 300 to 500 words, opening with the fixed sentence; observational
  voice; text in reading order, quoted exactly and in its own script; the
  RGBA sentence kept; no ratio, quality boosters or negative prompt; prose
  only, no JSON.
- User message: the canvas size and the raw request (shared).
- Reply check: the Z-Image check (non-empty, not a JSON object, every
  double-quoted span of the original kept word for word).
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Enabled on the one tracked `qwen-image-2.1` stanza named in #208.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[repo]: https://github.com/QwenLM/Qwen-Image-2.1
[pereadme]: https://github.com/QwenLM/Qwen-Image-2.1/blob/main/prompt_rewrite/README.md
[pesys]: https://github.com/QwenLM/Qwen-Image-2.1/blob/main/prompt_rewrite/prompts/system_prompt_t2i.txt
[pecard]: https://huggingface.co/Qwen/Qwen-Image-2.1-PE-T2I
[pesyshf]: https://huggingface.co/Qwen/Qwen-Image-2.1-PE-T2I/blob/main/system_prompt.txt
[diffusers]: https://huggingface.co/docs/diffusers/en/api/pipelines/qwenimage21
[vllm]: https://github.com/vllm-project/vllm-omni/blob/main/recipes/Qwen/Qwen-Image-2.1.md
[diffsynth]: https://diffsynth-studio-doc.readthedocs.io/en/latest/Model_Details/Qwen-Image-2.1.html
[comfy]: https://huggingface.co/Comfy-Org/Qwen-Image-2.1
[gguf]: https://huggingface.co/leejet/Qwen-Image-2.1-GGUF
[v1card]: https://huggingface.co/Qwen/Qwen-Image
[v1utils]: https://github.com/QwenLM/Qwen-Image/blob/main/src/examples/tools/prompt_utils.py
[v1blog]: https://qwenlm.github.io/blog/qwen-image/
[edit2511]: https://huggingface.co/spaces/Qwen/Qwen-Image-Edit-2511/blob/main/app.py
[review]: https://www.eesel.ai/blog/qwen-image-2-1-review
[pixeldojo]: https://pixeldojo.ai/guides/qwen-image-2-1-prompting-guide

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Z-Image prompt style](z-image-prompt-style.md): the profile whose reply check this one shares.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
