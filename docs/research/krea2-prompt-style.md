[Docs home](../README.md) › Research › Krea 2 prompt style

# Krea 2 prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #207. Feeds the `krea2`
prompt profile (`internal/router/imageprompt_krea2.go`).

This page records how Krea 2 Turbo wants to be prompted, and whether the
character creator and Ostris edit variants can share that profile. Every
claim links to its source. A claim that could not be confirmed from the
vendor's own pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files.

Sources, primary first:

- The vendor's model card on Hugging Face ([card][card]) and the official
  inference repo ([repo][repo]), including its prompting guide
  ([prompting][prompting]) and its prompt-expansion system prompt
  ([expansion][expansion]).
- The diffusers pipeline docs for Krea 2 ([diffusers][diffusers]) and a GGUF
  repack card that states the sampling settings ([byteshape][byteshape]).
- Variant pages: the QuadView character sheet LoRA ([CharacterSheet][charsheet]),
  the Identity Edit LoRA ([identity][identity]), the reference-image blog post
  on Hugging Face ([ref blog][refblog]) and the Ostris edit pipeline
  ([ostris][ostris]).
- Community guides, used only where they agree with the vendor pages
  ([gist][gist], [tested on Turbo][siren], [booru to prose][booru],
  [workflow template fix][tplfix]).

## 1. What the model is

Krea 2 is a text-to-image diffusion transformer from Krea, released as Raw
(undistilled, uses CFG) and Turbo (post-trained and distilled to 8 steps)
([card][card], [repo][repo]). The card lists 13B parameters; some repack
cards say 12B `(verify)` ([byteshape][byteshape]). The text encoder is a
4B vision-language model, not CLIP or T5, and the reference code reads at
most 512 prompt tokens, about 375 English words ([booru][booru]). Every
tracked `krea-2-turbo` stanza runs the Turbo checkpoint at 8 steps.

## 2. Findings

### Prose versus tags

Prose. The vendor guide says "We recommend users to use natural language
prompts" ([prompting][prompting]). The expansion system prompt asks for one
paragraph with no bullets, JSON or markdown ([expansion][expansion]). A tag
list still works, but in a side-by-side test the tag version turned a
described candid moment into a posed portrait, and a flat tag list cannot say
which subject owns which attribute ([siren][siren], [booru][booru]). So the
enhancer turns tags and short prompts into one paragraph.

### Structure and length

- "Long detailed prompts yield best results, but the model is capable of
  generating high quality images with minimal prompt engineering"
  ([prompting][prompting]).
- The official sample prompts are 30 to 150 words of continuous description
  ([gist][gist]) `(verify)`. Past 512 tokens the end is cut off
  ([booru][booru]).
- The expansion rules, which the profile follows ([expansion][expansion]):
  keep every original subject, action, colour and spatial relationship; add
  no objects, props, characters or animals the user does not imply; group each
  subject with its own attributes and actions; choose style, medium, framing
  and lighting; do not invent highly specific clothing, colours or materials;
  and if the prompt is already detailed, lightly polish it rather than expand
  it. The exact numbering of the middle rules was not visible in the extracts
  `(verify)`.
- Community guides order a prompt subject and action first, then setting,
  framing, light, palette and medium ([siren][siren], [gist][gist]).

### Quality tags and weighting

Not used. No vendor page asks for quality tags. Old `(word:1.3)` weighting
does not carry over to this text encoder and breaks the image past about 1.2
([booru][booru]) `(verify)`. The profile asks for neither.

### Negative prompts

Ignored on Turbo in the vendor setup. The card and diffusers docs run Turbo
with `guidance_scale=0.0`, which turns CFG off, so the negative branch is
never evaluated ([diffusers][diffusers], [byteshape][byteshape]). The vendor
prompting guide does not mention negative prompts ([prompting][prompting]).

The tracked stanzas differ: some run sd-server at `--cfg-scale 2.0` (CFG on
in sd-server's convention), others at `1.0` (off). Either way the profile
never writes or edits `negative_prompt`; a client's value reaches sd-server
untouched.

### Text rendering

Supported, with quotes. "For text rendering, we recommend putting quotes
around the words to be rendered" ([prompting][prompting]), and the expansion
rules say to give the exact text and wrap it in quotes ([expansion][expansion]).
A Turbo test spelled a short quoted sign correctly on three seeds when the
prompt also said where it sat and what it was painted on ([siren][siren]).
No other special syntax was found.

The model draws text the prompt mentions, so an expander must not add any:
a shared workflow template had its expander paint its own instructions into
images until it was patched ([tplfix][tplfix]). The profile tells the
enhancer never to add text, labels or instructions that were not asked for,
and the shared reply check rejects a rewrite that drops a quoted span.

### txt2img versus img2img and edit

Only txt2img. Edit prompts for Krea 2 are short instructions applied to a
reference image ("add sunglasses and a hat to the capybara"), not scene
descriptions ([ref blog][refblog], [identity][identity]). An expanded scene
paragraph would fight the reference. The shared caption step already skips
img2img.

### Fallback

The verbatim prompt. Krea 2 reads plain text, so a short prompt is still a
valid prompt ([prompting][prompting]). The ideogram4 JSON template would be
read as literal text.

## 3. Variants

| Stanza | What it is | Profile |
|---|---|---|
| `krea-2-turbo` | Krea 2 Turbo, plain txt2img | `krea2` |
| `krea-character-creator` | Turbo with QuadView and Identity Edit LoRAs | none |
| `krea2-ostris-edit` | Ostris reference-image edit pipeline | none |
| Krea 2 fine-tunes in the tracked configs | Community fine-tunes with a community text encoder | none `(verify)` |

**Character creator.** QuadView is an edit LoRA that turns a reference image
into a character sheet, with one fixed trigger prompt that works best used
exactly as written ([CharacterSheet][charsheet]). Identity Edit needs at least
one reference image and takes a plain edit instruction ([identity][identity]).
A rewrite would break the trigger and the instruction, so this stanza stays
unenhanced.

**Ostris edit.** A custom pipeline that conditions on one or two reference
images; without one it is a plain Krea 2 sampler ([ostris][ostris]). The
tracked stanza is a separate edit server matched on the body's `model` field,
used for edits with an Identity Edit LoRA. Its prompts are edit
instructions, so it gets no `image_pipeline` and stays unenhanced.

**Fine-tunes.** The tracked configs also carry Krea 2 community fine-tunes
(RedCraft, DarkBeast 2 and 3, Kreamagine) that load a different community
text encoder. One fine-tune's listing shows prose example prompts
([DarkBeast][darkbeast]), but no card found says whether they keep the base
prompt style or need trigger words. They are left off until someone confirms
it `(verify)`.

## 4. Golden example

Request (1344 by 768 canvas):

```text
a bakery at dawn with a sign that says "FRESH BREAD"
```

Enhanced prompt:

```text
A small corner bakery at dawn, seen from across a quiet cobbled street. A wooden sign above the door reads "FRESH BREAD" in cream painted letters on a dark green board. Warm yellow light spills from the wide front window onto the wet stones, and loaves and pastries are stacked on shelves behind the glass. The shop fills the centre of a wide frame, with the empty street in the foreground and a pale blue sky above the rooftops. Soft early morning light, muted warm palette, natural documentary photograph.
```

This pair is the fixture in `internal/router/imageprompt_krea2_test.go`,
which also covers the verbatim fallback, `enhance_prompt: false` and img2img.

## 5. What the profile does

- System prompt: the expansion rules above as one paragraph of prose, 50 to
  150 words and never over 250 (inside the 512-token limit); subject and
  action first with attributes beside each subject; one style and medium;
  exact quoted text with placement; no added text, quality tags or negative
  prompt; prompt only.
- User message: the canvas size and the raw request (shared).
- Reply check: the Z-Image check (non-empty, not a JSON object, every
  double-quoted span of the original kept word for word).
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Enabled on the three tracked `krea-2-turbo` stanzas named in #207.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[card]: https://huggingface.co/krea/Krea-2-Turbo
[repo]: https://github.com/krea-ai/krea-2
[prompting]: https://github.com/krea-ai/krea-2/blob/main/docs/prompting.md
[expansion]: https://github.com/krea-ai/krea-2/blob/main/docs/expansion.txt
[diffusers]: https://huggingface.co/docs/diffusers/api/pipelines/krea2
[byteshape]: https://huggingface.co/byteshape/Krea-2-Turbo-GGUF
[charsheet]: https://huggingface.co/Alissonerdx/CharacterSheet
[identity]: https://huggingface.co/conradlocke/krea2-identity-edit
[refblog]: https://huggingface.co/blog/OzzyGT/using-krea-with-reference-images
[ostris]: https://huggingface.co/ostris/Krea2OstrisEdit
[gist]: https://gist.github.com/cicalooo/6d26f1d4d54f6d05e87e6fde94b703b7
[siren]: https://prompts.sirenrender.com/krea-2/prompting-guide/
[booru]: https://civitai.com/articles/37453/tags-to-natural-language-how-booru-prompt-gallery-writes-prompts-for-flux2-krea-2-and-moss-image
[tplfix]: https://github.com/Comfy-Org/workflow_templates/pull/1313
[darkbeast]: https://www.sogni.ai/models/dark-beast-krea-2

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Z-Image prompt style](z-image-prompt-style.md): the profile whose reply check this one shares.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Fibo 2.0 prompt style](fibo-2.0-prompt-style.md)
- [FLUX.2 Klein prompt style](flux2-klein-prompt-style.md)
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
