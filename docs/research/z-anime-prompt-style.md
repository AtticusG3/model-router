[Docs home](../README.md) › Research › Z-Anime prompt style

# Z-Anime prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #206. Feeds the `z-anime`
prompt profile (`internal/router/imageprompt_zanime.go`).

This page records how Z-Anime wants to be prompted, and how that differs from
its parent, Z-Image ([Z-Image prompt style](z-image-prompt-style.md)). Every
claim links to its source. A claim that could not be confirmed from the
fine-tune's own pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files.

Sources, primary first:

- The Z-Anime model card on Hugging Face ([card][card], [README][readme]).
- The author's ComfyUI workflow in the same repo ([workflow][workflow]).
- The CivitAI listing for the same model, through an archive mirror
  ([CivArchive][civ]); it carries the per-variant settings table.
- Hosted-API notes, used only where they agree with the card: Replicate
  ([readme][replicate]) and deAPI ([guide][deapi-guide], [model page][deapi]).
- A third-party local test write-up ([lilting][lilting]).

## 1. What the model is

Z-Anime is a full fine-tune of Z-Image base (the 6B single-stream diffusion
transformer), not a LoRA merge, shipped as Base, Distill-8-Step and
Distill-4-Step, in BF16, FP8, GGUF and all-in-one files ([card][card],
[README][readme]). The repo ships two text encoders: the stock Z-Image one
and "Engineer V4", a drop-in fine-tune of it ([README][readme]). Two of the
tracked stanzas run the 8-step distill with Engineer V4; the tracked GGUF
stanza runs the 8-step distill with a different community encoder.

## 2. Findings

### Tags versus prose

Natural language, not tags. The ticket expected booru-style tag lists; the
card says the opposite. Its prompting guide reads "Natural language works
best, not tag lists", with a full sentence as the good example and a short
comma list ("anime girl, silver hair, shrine maiden, ...") as the bad one
([card][card]). The hosted notes repeat it: write natural sentences, not
comma-separated tags ([replicate][replicate], [deapi][deapi]).

A local tester got usable images from a tag-like prompt, put down to the LLM
text encoder reading tags as plain words, but still found prose more stable
([lilting][lilting]). So a tag list is accepted but is the weaker input. That
makes tag-to-prose conversion the main job of the enhancer for this family.

### Structure and length

- Put the most important details first; early tokens weigh more
  ([replicate][replicate]). The card's own guide says the same `(verify)`
  ([card][card]).
- Longer, descriptive prompts beat short ones ([replicate][replicate]). The
  text encoder truncates at about 512 tokens ([replicate][replicate]), the
  same limit as Z-Image.
- The card's example templates name the character, then lighting, eyes, hair
  and line work, and end with a short quality phrase written as words:
  "clean linework, professional anime illustration quality" ([card][card]).
  The workflow's default prompt follows that shape ([workflow][workflow]).
- Illustration vocabulary ("cel shading", "expressive eyes with reflections")
  helps more than lens or camera terms ([deapi-guide][deapi-guide]).

### Quality and rating tags

Not expected. No source asks for `masterpiece`, `best quality`, `score_*` or
rating tags. Those belong to the author's separate Illustrious-based model,
AniSee, whose card starts prompts with "masterpiece, best quality, score_7"
([AniSee][anisee]). Z-Anime's examples close with a quality phrase in prose
instead ([card][card]).

### Negative prompts

It depends on the variant ([CivArchive][civ], [workflow][workflow]):

- **Base** (28 to 50 steps, CFG 3 to 5): full effect, strongly recommended.
- **Distill-8-Step and Distill-4-Step** (CFG 1.0, at most about 1.5): limited
  effect; the workflow says it can be left empty and zeroes it out.

Every tracked stanza runs the 8-step distill at CFG 1.0, so a negative prompt
does almost nothing there. The profile never writes or edits
`negative_prompt`; a client's value reaches sd-server untouched.

### Text rendering

The card makes no text-rendering claim for Z-Anime `(verify)`. The bilingual
text claim belongs to the Z-Image base
([Z-Image prompt style](z-image-prompt-style.md)), and the author's photoreal
all-in-one card is the one that shows a quoted-sign example prompt
([AIO][aio]). Hosted notes warn that fine text can render
imperfectly ([replicate][replicate]). No special syntax was found. The
profile keeps the parent's rule (exact text in English double quotes, kept
short) because the text encoder is the same, and the shared reply check
rejects a rewrite that drops a quoted span.

### txt2img versus img2img

Only txt2img. No source documents img2img or edit prompting for Z-Anime, and
an expanded description could fight the init image. img2img on
`z-anime-turbo` also crashes sd-server today (#203); this profile does not
touch that path and does not claim it works.

### Fallback

The verbatim prompt. Z-Anime reads plain text, so a short prompt or a tag
list is still a valid prompt, just a weaker one. The ideogram4 JSON template
would be read as literal text.

## 3. Compared with Z-Image

| Point | Z-Image | Z-Anime |
|---|---|---|
| Prompt form | Prose | Prose; tags accepted but weaker |
| Order | Subject, setting, framing, light, colour, style | Most important details first, character before scene |
| Closing style phrase | No quality tags | One short quality phrase in prose, like the card's examples |
| Vocabulary | Concrete scene and camera terms | Illustration terms: line work, shading, eyes, hair |
| Negative prompt (tracked stanzas) | Base model, CFG 4: works | 8-step distill, CFG 1: little effect |
| Text rendering | Claimed, quoted text | Not claimed `(verify)`, quoted text kept |
| Fallback | Prompt verbatim | Prompt verbatim |

So the Z-Image system prompt is not reused. The two share the user message,
the reply check and the fallback.

## 4. Golden example

Request (tag list, 768 by 1024 canvas):

```text
1girl, silver hair, side ponytail, blue scrunchie, school uniform, rooftop, sunset, masterpiece
```

Enhanced prompt:

```text
A young anime girl with long silver hair tied in a side ponytail with a blue scrunchie stands on a quiet school rooftop, smiling softly. She wears a white shirt, a red necktie and a navy pleated skirt that moves in the breeze. A chain-link fence and a pale evening sky fill the background, with the girl framed from the knees up in the centre of a tall canvas. Warm sunset rim light catches her hair. Clean line work, professional anime illustration with rich colour.
```

This pair is the fixture in `internal/router/imageprompt_zanime_test.go`.

## 5. What the profile does

- System prompt: turn tags into sentences and drop quality, score and rating
  tags; 60 to 160 words; character first, then setting, framing for the
  canvas, lighting and colour; illustration vocabulary; end with one short
  style sentence; exact quoted text; no negative prompt; prompt only.
- User message: the canvas size and the raw request (shared).
- Reply check: the Z-Image check (non-empty, not a JSON object, every
  double-quoted span of the original kept word for word).
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Enabled on the three tracked `z-anime-turbo` stanzas named in #206.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[card]: https://huggingface.co/SeeSee21/Z-Anime
[readme]: https://huggingface.co/SeeSee21/Z-Anime/blob/main/README.md
[workflow]: https://huggingface.co/SeeSee21/Z-Anime/blob/main/workflows/Z-Anime-Workflow-v1.json
[civ]: https://civarchive.com/models/2483351?modelVersionId=2848912
[replicate]: https://replicate.com/lucataco/z-anime/readme
[deapi-guide]: https://deapi.ai/blog/z-anime-prompting-guide-settings-resolutions-and-example-prompts
[deapi]: https://deapi.ai/models/z-anime
[lilting]: https://lilting.ch/en/articles/z-anime-z-image-local-comfyui
[anisee]: https://huggingface.co/SeeSee21/AniSee
[aio]: https://huggingface.co/SeeSee21/Z-Image-Turbo-AIO

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Z-Image prompt style](z-image-prompt-style.md): the parent family's profile.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
