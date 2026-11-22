[Docs home](../README.md) › Research › FLUX.3 Klein prompt style

# FLUX.3 Klein prompt style

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Date of reading: 2026-11-22. Part of #204, issue #210. Feeds the
`flux3-klein` prompt profile (`internal/router/imageprompt_flux3_klein.go`).

This page records how FLUX.3 Image Klein wants to be prompted, and how that
differs from FLUX.2 Klein. Every claim links to its source. A claim that
could not be confirmed from the vendor's own pages is marked `(verify)`.

## 0. Method

Page fetches were not available in this run (web search only). Every fact
comes from search-engine extracts of the cited pages, read on 2026-11-22, not
from opening the files. Only FLUX.3 sources are used for FLUX.3 claims.

Sources, primary first:

- The vendor's model card on Hugging Face ([card][card]), the vendor docs page
  for Klein ([docs][docs]) and the reference inference repo ([repo][repo]).
- The vendor's prompt upsampler card ([pup][pup]) and its published system
  prompts ([prompts][prompts]); a mirror repo that reproduces the prompt text
  ([mirror][mirror]) `(verify)`.
- The vendor's FLUX 3 Image prompting guides ([basics][basics],
  [reference][reference], [editing][editing]) and launch post ([blog][blog]).
- An API host's Klein guides, used only where they agree with the vendor
  pages ([runware][runware], [runware base][runwarebase]).
- For the FLUX.2 Klein comparison only: the vendor help page
  ([FLUX.2 help][f2help]) and a FLUX.2 Klein prompting guide ([FLUX.2
  guide][f2guide]).

## 1. What the model is

FLUX.3 Image Klein is a 5.6B open-weights image transformer from Black Forest
Labs. One model does text-to-image, editing and composition from up to 10
reference images, with a 4B vision-language model as the text encoder
([card][card], [blog][blog]). The distilled checkpoint runs 16 steps with no
guidance; Klein Base is the undistilled 50-step model for fine-tuning
([card][card]). Both tracked `flux3-klein` stanzas run the distilled Q8_0
checkpoint at 16 steps and `--cfg-scale 1.0`, with the 4B encoder.

## 2. Findings

### The model expects an upsampled caption

This is the main finding. The vendor docs say: "Do not use Klein without the
prompt upsampler. The model is trained on the captions it writes; a prompt
sent as typed gives noticeably weaker results" ([docs][docs]). The card says
to "always run a prompt upsampler, ours or your own LLM: the model takes full
scene captions (framing, subject, background, lighting)" ([card][card]). The
reference CLI runs the upsampler by default; `--no-pup` sends the prompt as
typed, for captions written by another model ([repo][repo], [card][card]).

The vendor upsampler is a separate 9B model (18.8 GB). The card says any other
model can do the job with the published system prompts ([pup][pup]). That is
exactly what the caption step does, using the stanza's local enhancer, so the
profile follows the vendor's `t2i` system prompt.

### Prose versus tags

Prose. The `t2i` prompt asks for "ONE detailed, self-contained caption: the
exact text the image model receives", as one continuous prose block in the
generative voice, never "this image shows", "we can see" or "is visible"
([prompts][prompts]). Its output rules forbid Markdown, headings, JSON,
coordinates, ids and quotes around the caption ([mirror][mirror]). No source
recommends tag lists. The enhancer turns tags and short prompts into one
caption.

### Structure and length

- Typical length is "100 to 350 words, more for complex scenes"
  ([mirror][mirror]) `(verify)` against the vendor file.
- The card names the content: framing, subject, background and lighting
  ([card][card]). The FLUX 3 Image guide orders medium and style first, then
  the subjects and where they sit, then light, colour and background
  ([basics][basics]). A write-up of the upsampler prompts says the background
  gets about a sixth of the caption `(verify)`.
- Describe what is there rather than what is absent: "an empty promenade",
  not "no crowds" ([basics][basics], [runware][runware]).
- If the input is not clearly an image request, the upsampler captions a
  visualisation of its concept ([prompts][prompts]).

### Negative prompts

Ignored. The distilled checkpoint runs with no guidance ([card][card]), and
the FLUX 3 Image request has no `negative_prompt` field ([basics][basics]).
Only Klein Base takes a negative prompt ([runware base][runwarebase]). The
tracked stanzas run the distilled checkpoint at `--cfg-scale 1.0`, which is
CFG off in sd-server's convention. The profile never writes or edits
`negative_prompt`; a client's value reaches sd-server untouched.

### Text rendering

Supported, with quotes. The card lists "Text to render stays verbatim, in
quotes" for the upsampler ([pup][pup]); the `i2i` prompt says to transcribe
on-image text verbatim in its original language inside double quotes
([prompts][prompts]). The guides add: say where the text sits and how it
looks, keep it short, front-load it, and `\n` inside the quotes splits lines
([reference][reference]). Text the prompt does not spell out can come back as
invented lettering, so describe an unlabelled object as unlabelled
([runware][runware]). The card warns rendered text may still be inaccurate
([card][card]).

The profile asks for quoted text with placement and look, and no added text.
The shared reply check rejects a rewrite that drops a quoted span.

### Layout boxes

FLUX 3 Image takes optional bounding boxes at the end of the prompt string
([basics][basics]), and the upsampler has a separate `t2i_bbox` mode that
writes a caption plus a JSON element table ([prompts][prompts]). The profile
only covers plain `t2i`. Whether sd-server reads the box table is not known
`(verify)`; a client that writes its own boxes should send
`enhance_prompt: false`.

### txt2img versus img2img and edit

Only txt2img. The vendor's edit mode (`i2i`) has the upsampler look at the
reference images and fold the edit into a full description of them
([prompts][prompts], [docs][docs]). The local enhancer is text-only and never
sees the image, so it cannot write that caption. Edit prompts stay short
instructions that name what changes and what stays ([editing][editing]). The
shared caption step already skips img2img.

### Fallback

The verbatim prompt. The vendor says an unexpanded prompt gives weaker
results, not broken ones ([docs][docs]), and the CLI accepts a typed prompt
with `--no-pup` ([repo][repo]). The ideogram4 JSON template would be read as
literal text.

## 3. Compared with FLUX.2 Klein

Separate families, separate profiles. No vendor page says they share a prompt
format.

| | FLUX.3 Image Klein | FLUX.2 Klein |
|---|---|---|
| Prompt upsampling | Required by the vendor; the model is trained on upsampler captions ([docs][docs]) | "[klein] does not include prompt upsampling"; write detailed prompts yourself ([FLUX.2 help][f2help]) |
| Typical length | 100 to 350 words ([mirror][mirror]) `(verify)` | 40 to 120 words, up to 512 tokens ([FLUX.2 guide][f2guide]) `(verify)` |
| Profile guidance source | The vendor `t2i` upsampler prompt ([prompts][prompts]) | FLUX.2 prompting guides (the FLUX.2 Klein ticket) |

So `flux3-klein` writes a longer caption in the upsampler's voice and keeps
its own caption name. It should not be pointed at a FLUX.2 Klein stanza, or
the other way round.

## 4. Golden example

Request (1024 by 1024 canvas):

```text
a red fox asleep in a suitcase on a train, luggage tag says "PLATFORM 9"
```

Enhanced prompt:

```text
A square, eye-level photograph of a small red fox curled up asleep inside an open vintage brown leather suitcase, which rests on a worn green fabric seat of an old passenger train carriage. The fox lies nose to tail in the centre of the frame, its rust-orange fur soft and slightly ruffled, white chest fur showing and its black-tipped ears folded back, eyes closed. The suitcase lid stands open behind it, lined with faded cream cloth, and a paper luggage tag tied to the handle reads "PLATFORM 9" in neat black handwritten capitals. Beside the seat, a large window shows a blurred green countryside sliding past. Warm late-afternoon sunlight falls through the glass from the left, laying long soft shadows across the fox and the seat. Muted warm palette of rust, brown, cream and olive green. Natural documentary photograph with a shallow depth of field, the background gently out of focus.
```

This pair is the fixture in `internal/router/imageprompt_flux3_klein_test.go`,
which also covers the verbatim fallback, `enhance_prompt: false` and img2img.

## 5. What the profile does

- System prompt: the vendor `t2i` upsampler rules in our own words: one
  self-contained prose caption in the generative voice, 100 to 350 words;
  framing, subject, background, lighting, colours and style; describe what is
  there, not what is absent; quoted text verbatim with placement; no added
  text, quality tags or negative prompt; caption only.
- User message: the canvas size and the raw request (shared).
- Reply check: the Z-Image check (non-empty, not a JSON object, every
  double-quoted span of the original kept word for word).
- Fallback: the original prompt verbatim, `X-SD-Shim-Prompt-Source: original`.
- Enabled on the two tracked `flux3-klein` stanzas named in #210.

See [`SPEC.md`](../../SPEC.md) for the shared skip rules, budget and headers.

[card]: https://huggingface.co/black-forest-labs/flux-3-image-klein
[docs]: https://docs.bfl.ai/flux_3/flux3_image_klein
[repo]: https://github.com/black-forest-labs/flux-3-image-klein
[pup]: https://huggingface.co/black-forest-labs/flux-3-image-klein-pup
[prompts]: https://huggingface.co/black-forest-labs/flux-3-image-klein-pup/blob/main/system_prompts.json
[mirror]: https://huggingface.co/prithivMLmods/flux-3-image-klein-prompt-upsampler-abliterated
[basics]: https://docs.bfl.ai/guides/prompting_unified_basics
[reference]: https://docs.bfl.ml/guides/prompting_unified_reference
[editing]: https://docs.bfl.ml/guides/prompting_editing_single_reference
[blog]: https://bfl.ai/blog/flux-3-image-klein
[runware]: https://runware.ai/docs/models/bfl-flux-3-image-klein/guides/prompting
[runwarebase]: https://runware.ai/docs/models/bfl-flux-3-image-klein-base/guides/guidance-and-negative-prompts
[f2help]: https://help.bfl.ai/articles/7592221790-how-do-i-generate-quickly-with-flux-2-klein
[f2guide]: https://deapi.ai/blog/prompting-flux-2-klein-what-works-what-doesnt-and-why

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Z-Image prompt style](z-image-prompt-style.md): the profile whose reply check this one shares.
- [SPEC](../../SPEC.md): the `image_pipeline` caption step this profile plugs into.

## Linked from

<!-- backlinks:start -->
- [Model Router: SPEC](../../SPEC.md)
- [model-router docs](../README.md)
<!-- backlinks:end -->
