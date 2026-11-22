[Docs home](../README.md) › Research › FLUX.2 Klein prompt style

# FLUX.2 Klein prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #209. Feeds the
`flux2-klein` prompt profile (`internal/router/imageprompt_flux2_klein.go`).

This page records how FLUX.2 [klein] wants to be prompted. Every claim links
to its source. A claim that could not be confirmed from Black Forest Labs'
own pages is marked `(verify)`. Advice for FLUX.1 and FLUX.3 is not used as
FLUX.2 fact; section 6 notes where FLUX.3 Klein differs.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files. The dedicated Klein prompting guide
([klein guide][kleinguide]) did not appear in any extract; its rules are
taken from the vendor pages that summarise it and are marked `(verify)` where
only a secondary source quotes them.

Sources, primary first:

- The vendor's model cards on Hugging Face ([9B card][card9b],
  [4B card][card4b]) and the FLUX.2 overview ([overview][overview]).
- Vendor prompt guidance: the Klein help article ([help][help]), the prompt
  reference ([reference][reference]), "Building a Good Prompt"
  ([building][building]), the FLUX.2 [pro] and [max] guide ([flux2 guide][flux2guide])
  and the vendor's agent skills repo ([skills][skills]).
- Secondary guides, used only where they agree with the vendor pages
  ([fal][fal], [deAPI][deapi], [guide summary][yaah], [fuser][fuser]).

## 1. What the model is

FLUX.2 [klein] 9B is a 9 billion parameter rectified flow transformer for
text-to-image and multi-reference editing ([9B card][card9b]). The distilled
checkpoints pair it with an 8B language-model text embedder and are step-distilled to
4 steps ([9B card][card9b]); the vendor's example runs them at
`guidance_scale=1.0`, 4 steps ([4B card][card4b]). A separate base checkpoint
is not distilled ([9B card][card9b]).

Both tracked `flux2-klein` stanzas run 4 steps at `--cfg-scale 1.0`, so they
behave as the distilled model. One stanza loads a community 9B
checkpoint and both load a community uncensored text encoder; no card was
found saying either changes the prompt style `(verify)`.

## 2. Findings

### Prose versus tags

Prose. The vendor tells Klein users to "describe scenes like a novelist, not
keyword lists" ([help][help]), and the prompt reference repeats it for Klein
([reference][reference]). So the enhancer turns tags and short prompts into
descriptive sentences.

### No prompt upsampling

FLUX.2 [klein] has no prompt upsampling: "write detailed, descriptive prompts
for best results" ([overview][overview], [help][help]). The prompt reference
says that for Klein "what you write is exactly what the model receives"
([reference][reference]). This is the case for an enhancer: a short request
reaches the model as it is.

### Structure and length

- Subject + action + style + context, most important first; "FLUX.2 pays more
  attention to what comes first" ([flux2 guide][flux2guide]).
- For Klein: subject, then setting, details, lighting and atmosphere
  ([guide summary][yaah]) `(verify)`. Lighting, described by source, quality,
  direction and colour temperature, has the highest single impact on quality
  ([reference][reference], [skills][skills]).
- The prompt reference suggests ending with "Style: [style]. Mood: [mood]."
  for consistent looks ([reference][reference]) `(verify)`.
- Length bands: 10 to 30 words for quick concepts, 30 to 80 for most scenes,
  80 to 300+ for complex scenes. "More words do not automatically mean better
  results" ([building][building]). A secondary guide finds Klein strongest at
  40 to 120 words ([deapi][deapi]). The profile aims for 40 to 120 words.
- English prompts give the most precise results, since most training data is
  English ([reference][reference]). The profile writes the prompt in English
  and keeps quoted text in its own language.

### Weighting and quality tags

Not used. Klein has no weight syntax and responds to plain-language emphasis
([fal][fal]) `(verify)`. No vendor page asks for quality tags.

### Negative prompts

Not supported. "FLUX.2 does not support negative prompts. Focus on describing
what you want, not what you don't want" ([flux2 guide][flux2guide]). On the
distilled Klein checkpoints guidance is fixed at 1.0 ([4B card][card4b]), so
CFG and its negative branch are off; secondary guides agree negatives are
ignored ([deapi][deapi]). The profile never writes or edits
`negative_prompt`; a client's value reaches sd-server untouched. The
enhancer is told to phrase exclusions positively ("sharp focus" rather than
"no blur") ([fuser][fuser]).

### Text rendering and colour

Supported, with quotes: use "quoted text" for typography ([skills][skills]),
with placement, font and colour ([flux2 guide][flux2guide]). Klein supports
exact colour matching with hex codes ([help][help]); a hex code should be tied
to a named object, for example "the background is hex #1a1a2e"
([flux2 guide][flux2guide]). The profile keeps quoted text and hex codes
exactly, and the shared reply check rejects a rewrite that drops a quoted
span. No other special syntax was found. The JSON structured prompting in the
FLUX.2 guide ([flux2 guide][flux2guide]) is not used: Klein is told to read
prose ([help][help]), and the reply check rejects JSON.

### txt2img versus img2img and edit

Only txt2img. Klein edits take up to 4 reference images ([reference][reference]),
and edit prompts describe only the change, naming each image's role ("subject
from image 1, style from image 2") ([flux2 guide][flux2guide],
[guide summary][yaah]). Re-describing the reference fights it
([guide summary][yaah]) `(verify)`. The shared caption step already skips
img2img, and both tracked stanzas advertise `img2img`, so edits stay as
written.

### Fallback

The verbatim prompt. Klein reads plain text, so a short prompt is still a
valid prompt ([help][help]). The ideogram4 JSON template would be read as
literal text.

## 3. Golden example

Request (1024 by 1024 canvas):

```text
a lighthouse on a rocky coast with a banner that says "HARBOUR DAYS"
```

Enhanced prompt:

```text
A white stone lighthouse stands on a rocky headland above a choppy grey-green sea, waves breaking white against the dark rocks below. A long canvas banner strung between two poles at the base of the tower reads "HARBOUR DAYS" in bold navy capitals on a cream background. The lighthouse sits slightly left of centre in a square frame, with the rugged coastline running away to the right and a few gulls in the sky. Low late-afternoon sun from the left rakes across the stone and casts long warm shadows over the rocks. Style: crisp documentary photograph. Mood: bright and festive.
```

This pair is the fixture in `internal/router/imageprompt_flux2_klein_test.go`,
which also covers the verbatim fallback, `enhance_prompt: false` and img2img.

## 4. What the profile does

- System prompt: one paragraph of English prose, 40 to 120 words; subject and
  action first, then setting, framing for the canvas, lighting (source,
  direction, quality, colour temperature), palette, and a closing
  "Style: ... Mood: ..." sentence; exact quoted text with placement; hex
  codes kept and tied to their object; no weights, quality tags or negative
  prompt; prompt only.
- User message: the canvas size and the raw request (shared).
- Reply check: the Z-Image check (non-empty, not a JSON object, every
  double-quoted span of the original kept word for word).
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Enabled on the two tracked `flux2-klein` stanzas named in #209.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

## 5. Stanzas

| Stanza | Tracked copies | Profile |
|---|---|---|
| `flux2-klein` | 2 | `flux2-klein` |
| `flux3-klein` | 1 | none (#210) |

## 6. FLUX.3 Klein is not FLUX.2 Klein

FLUX.3 Klein is a later ticket (#210) and this profile is not meant for it.
The tracked `flux3-klein` stanza loads a different text encoder (a 4B
vision-language model) and samples at 16 steps rather than 4, so its prompt
rules have to be researched from its own card. Nothing on this page is
claimed for FLUX.3.

[kleinguide]: https://docs.bfl.ai/guides/prompting_guide_flux2_klein
[card9b]: https://huggingface.co/black-forest-labs/FLUX.2-klein-9B
[card4b]: https://huggingface.co/black-forest-labs/FLUX.2-klein-4B
[overview]: https://docs.bfl.ml/flux_2/flux2_overview
[help]: https://help.bfl.ai/articles/7592221790-how-do-i-generate-quickly-with-flux-2-klein
[reference]: https://docs.bfl.ai/guides/prompting_unified_reference
[building]: https://docs.bfl.ai/guides/prompting_unified_building
[flux2guide]: https://docs.bfl.ml/guides/prompting_guide_flux2
[skills]: https://github.com/black-forest-labs/skills
[fal]: https://fal.ai/learn/devs/flux-2-klein-prompt-guide
[deapi]: https://deapi.ai/blog/prompting-flux-2-klein-what-works-what-doesnt-and-why
[yaah]: https://github.com/elboaf/YAAH/issues/437
[fuser]: https://fuser.studio/articles/flux-2-prompt-guide

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Z-Image prompt style](z-image-prompt-style.md): the profile whose reply check this one shares.
- [Krea 2 prompt style](krea2-prompt-style.md): another prose profile built the same way.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
