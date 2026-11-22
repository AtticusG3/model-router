[Docs home](../README.md) › Research › Z-Image prompt style

# Z-Image prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #205. Feeds the `z-image`
prompt profile (`internal/router/imageprompt_zimage.go`).

This page records how the Z-Image family wants to be prompted, so the router's
caption step can rewrite a short txt2img prompt into that style. Every claim
links to its source. A claim that could not be confirmed from the vendor's own
pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files. The vendor's prompt enhancer template (`pe.py`) is
written in Chinese; its rules are taken from search extracts of the file and
from a third-party English translation, so its exact wording is `(verify)`.

Sources:

- Z-Image-Turbo model card on Hugging Face ([card][zit-card]) and ModelScope
  ([ModelScope][zit-ms]).
- Z-Image base model card ([card][zb-card], and an older revision with a
  settings table: [revision][zb-rev]).
- The vendor's GitHub repo ([repo][gh]) and launch blog ([blog][blog]).
- The vendor's demo Space: the prompt enhancer template ([pe.py][pe]) and the
  app that calls it ([app.py][app]).
- The "PROMPTING GUIDE" discussion on the Turbo model card ([guide][guide]);
  who wrote it is `(verify)`.
- The diffusers pipeline docs ([diffusers][diffusers]).

## 1. Findings

### Structure and length

Z-Image is a 6B single-stream diffusion transformer with an LLM text encoder
([card][zit-card]). The Turbo card's prompting guide says it works best with
long, detailed prompts, and suggests writing the idea by hand and then letting
an LLM expand it ([guide][guide]). The vendor ships that expansion step as the
"Prompt Enhancer", which the card credits with letting the model "tap into
underlying world knowledge" ([card][zit-card], [repo][gh]).

The enhancer template works in a fixed order: lock in the core elements of the
request, decide whether the request needs reasoning (for example a worked
answer drawn in the image), add aesthetic detail (composition, lighting,
colour, materials), then handle any text ([pe.py][pe], translation:
[zenn][pe-en]) `(verify)`. It forbids meta tags such as "8K" and
"Masterpiece" and asks for the final prompt only ([pe-en][pe-en]) `(verify)`.

The text encoder reads at most 512 tokens by default; longer prompts are
truncated ([diffusers][diffusers]). Third-party guides put the sweet spot at
100 to 300 tokens `(verify)`. The profile asks for 80 to 200 words, which sits
well inside the 512-token cap.

### Tags versus prose

Natural-language prose. The enhancer template produces flowing descriptive
text, not keyword lists ([pe.py][pe]), and the Juggernaut Z guide for the same
base warns that comma-separated SDXL-style tags "mostly run into errors and
broken concepts" ([Civitai][jugg-civitai]). No official source documents tag
or weighting syntax such as `(word:1.2)`; the encoder reads it as plain text
`(verify)`.

### Negative prompts

It depends on the variant:

- **Turbo** is distilled to run without classifier-free guidance; the card
  says guidance should be 0 ([card][zit-card]), so a negative prompt has no
  effect ([guide][guide]).
- **Base** keeps full CFG and responds to negative prompts; the card lists
  them as "strongly recommended" with guidance 3 to 5 ([revision][zb-rev],
  [card][zb-card]).

The tracked `z-image` stanzas run the base model at CFG 4. The profile never
writes or edits `negative_prompt`: a client's negative prompt reaches
sd-server untouched, and the enhancer is told not to write one.

### Text rendering

The card claims bilingual text rendering in English and Chinese
([card][zit-card]). The enhancer template requires every piece of on-image
text to be copied word for word inside English double quotes, and for
posters, menus and UI it asks for the font and layout too ([pe.py][pe]). The
demo's example prompt follows that pattern ([app.py][app]). That is the only
special syntax found.

The profile keeps this rule, and its reply check rejects a rewrite that drops
any double-quoted span of the original prompt, so requested text is never
lost.

### Language

The enhancer template is written in Chinese ([pe.py][pe]). The guide thread
reports that an English rewrite gave better results ([guide][guide])
`(verify)`; the cards give no preference beyond bilingual support. The profile
writes in the language of the request and keeps quoted text as given.

### txt2img versus edit

Only txt2img. The dedicated edit model, Z-Image-Edit, is announced but still
listed "to be released" in the vendor's model table ([repo][gh],
[blog][blog]). Plain img2img through the base or Turbo weights takes an
ordinary prompt ([diffusers][diffusers]), but an expanded description could
fight the init image, so img2img is left alone, as ideogram4 does. Revisit
when Z-Image-Edit ships.

### Fallback

The verbatim prompt. Z-Image reads plain prose, so a short prompt is still a
valid prompt; it just carries less detail. No structured template is needed,
and the ideogram4 JSON template would be read as literal text.

## 2. Variants in the tracked configs

| Stanza | What it is | Profile | Why |
|---|---|---|---|
| `z-image` | Z-Image base | on | The base family. |
| `z-image-juggernaut-fast` | Juggernaut Z Fast, a fine-tune of Z-Image base ([HF][jugg-hf]) | on | Its guide asks for the same natural-language scene description, with exact text quoted ([guide][jugg-guide], [Civitai][jugg-civitai], [API docs][jugg-fast]). |
| `z-image-redcraft` | RedCraft, a Z-Image-Turbo checkpoint ([listing][redcraft]) | off | No prompt guide from its author was found, and its model file sits outside the Z-Image folder in the stanza `(verify)`. Enable once its prompt style is confirmed. |
| `z-anime-turbo` | Z-Anime, a fine-tune of Z-Image | off | Separate ticket (#206). |

The local stanzas load community fine-tunes of the base text encoder rather
than the stock one. They are expected to read the same prose `(verify)`.

### Notes for Z-Anime (#206, not implemented here)

The Z-Anime card describes a full anime fine-tune of Z-Image base, in base,
8-step and 4-step distilled variants. It recommends descriptive sentences over
tag lists, says the base variant has full negative prompt support, and ships
an alternative text encoder ([card][zanime]). Its sample prompts end with
style phrases such as "professional anime illustration, fine line work"
([card][zanime]). A mirror's sample uses tag style instead `(verify)`.

## 3. What the profile does

- System prompt: one detailed paragraph of prose in the order subject,
  setting, framing for the canvas, lighting, colours and materials, style;
  exact quoted text with font and placement; no quality tags; no negative
  prompt; prompt only.
- User message: the canvas size and the raw request (shared with ideogram4).
- Reply check: non-empty after stripping a code fence, not a JSON object, and
  every double-quoted span of the original prompt kept word for word.
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[zit-card]: https://huggingface.co/Tongyi-MAI/Z-Image-Turbo
[zit-ms]: https://www.modelscope.ai/models/Tongyi-MAI/Z-Image-Turbo
[zb-card]: https://huggingface.co/Tongyi-MAI/Z-Image
[zb-rev]: https://huggingface.co/Tongyi-MAI/Z-Image/blob/7bd56ea0a65f6c4470ba2ffc727571f34ead6114/README.md
[gh]: https://github.com/Tongyi-MAI/Z-Image
[blog]: https://tongyi-mai.github.io/Z-Image-blog/
[pe]: https://huggingface.co/spaces/Tongyi-MAI/Z-Image-Turbo/blob/main/pe.py
[pe-en]: https://zenn.dev/kun432/scraps/c15de86463c670
[app]: https://huggingface.co/spaces/Tongyi-MAI/Z-Image-Turbo/blob/main/app.py
[guide]: https://huggingface.co/Tongyi-MAI/Z-Image-Turbo/discussions/8
[diffusers]: https://huggingface.co/docs/diffusers/api/pipelines/z_image
[jugg-hf]: https://huggingface.co/RunDiffusion/Juggernaut-Z-Image
[jugg-guide]: https://www.rundiffusion.com/juggernaut-z-prompt-guide
[jugg-civitai]: https://civitai.com/models/2600510/juggernaut-z
[jugg-fast]: https://runware.ai/docs/models/rundiffusion-juggernaut-z-fast
[redcraft]: https://civarchive.com/tensorart/models/938483338719543111/versions/938483338719543111
[zanime]: https://huggingface.co/SeeSee21/Z-Anime

## See also

- [Docs index](../README.md): every docs page, by reader.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [FLUX.2 Klein prompt style](flux2-klein-prompt-style.md)
- [FLUX.3 Klein prompt style](flux3-klein-prompt-style.md)
- [Krea 2 prompt style](krea2-prompt-style.md)
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
- [Z-Anime prompt style](z-anime-prompt-style.md)
<!-- backlinks:end -->
