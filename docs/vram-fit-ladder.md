# VRAM fit ladder

How to write a stanza for a given weight file so the fleet shares one
`model_id` per model (different quants of the same weight family are the
same id). Apply this on the target GPU; do not guess from another node.

The mesh lists and routes by `model_id`. Pools are not catalog names.
Legacy client ids (`coding-pool`, `daily-driver`, `embed-pool`, …) are
**aliases on the local stanza**, not a second identity.

## Identity

- Same architecture + finetune + size = same `model_id` even if the GGUF
  quant differs (`Q4_K_M` vs `Q5_K_M` vs `UD-Q5_K_XL`).
- A different finetune is a different model (`qwopus-3.6-27b-coder` vs
  `-coder-compat` vs `-fusion`; heretic vs base; Compact vs Quality).
- One stanza per `model_id` per node. If a node has two quants of the
  same model, keep the one this ladder would pick; do not register both.

## Hard floor (weights)

- Do not serve weight quants below **Q4_K_M** (no Q4_K_S, Q4_0, IQ4_XS,
  IQ3, Q3, Q2, Q2_K).
- Preferred weight quant: **Q5_K_M** (or Unsloth `UD-Q5_K_XL` as the
  Q5-class equivalent). If VRAM remains after the full ctx/cache target,
  Q6_K / Q8_0 is allowed. If Q5-class cannot fit even at the bottom of
  this ladder, step the **weights** down to Q4_K_M. Never below.
- Exceptions (native low-bit architectures, not "we squeezed Qwen"):
  ternary / 1.58-bit / NVFP4 models whose published file is that format.

## Context target

- Preferred context: **262144** (256k).
- Minimum context: **131072** (128k). (128k is `2^17 = 131072`, not 131074.)
- Specialists (intent, embed, rerank) may use a smaller ctx; they are not
  this ladder.

## Cache (KV) quality steps

Start at the highest cache quality that the build supports, then step
down. Do not cut context until cache is at the last step.

| Step | K cache | V cache | Notes |
|------|---------|---------|--------|
| 1 | f16 | f16 | Best quality, most VRAM |
| 2 | q8_0 | q8_0 | First compression step |
| 3 | turbo4 | turbo4 | TurboQuant builds only. Stock llama.cpp: use q4_0 / q4_0 instead |

`--parallel` defaults to 1 before any ctx cut (`parallel 2` doubles KV).

## Drop order after min ctx + last-step cache still OOM

You are already at 131072 + turbo4/q4_0. Now drop add-ons, then backtrack
ctx upward if VRAM returns.

1. Drop **MTP / dSpark / dFlash / speculative draft** (`--spec-type`,
   `--model-draft`). These buy speed, not capability.
2. Drop **mmproj** (vision). Prefer keeping 128k text over 128k+vision if
   that is what fits; if dropping mmproj frees a 256k text config, take
   256k text (backtrack).
3. **MoE only:** offload **expert** tensors to CPU/RAM with `--n-cpu-moe`.
   Dense attention/non-expert layers stay on the GPU. Do **not** put the
   non-MoE layers on CPU — that is the wrong direction (experts are the
   bulky part).
4. **Dense only:** look for RPC / tensor-split across GPUs or nodes
   (`llama-server --rpc`, or a future router split stanza). Do not fake
   this with CPU offload of dense layers.

After each drop, retry ctx 262144 then 131072 at the last cache step.

## Do not

- Cut ctx below 131072 to keep MTP or mmproj.
- Quantize cache past turbo4/q4_0 in order to keep 256k; cut ctx first.
- Use Q4_K_S / IQ4_XS as a "fit trick" for a Q4_K_M-class dense model.
- Give two nodes different `model_id`s for the same finetune.

## Worked walk (chat 27B-class on a 16 GB card)

1. Weights Q5_K_M, ctx 262144, KV f16, parallel 1, mmproj+MTP on.
2. KV q8_0, then turbo4 (or q4_0).
3. Ctx 131072 at turbo4/q4_0.
4. Drop MTP. If 262144 now fits, take it.
5. Drop mmproj. Prefer 262144 text-only over 131072+vision if both fit.
6. Still OOM: MoE → `--n-cpu-moe`; dense → RPC/split.

## Fleet `model_id` catalog

Legacy pool/proxy names are aliases only.

| `model_id` | Family (quants may differ) | Nodes |
|---|---|---|
| `agents-a1` | Agents-A1 Uncensored MTP Quality | buster |
| `qwen3-0.6b-instruct` | Qwen3 0.6B Instruct | buster, digger |
| `qwen2.5-1.5b-instruct` | Qwen2.5 1.5B Instruct | gareth |
| `qwen3-embedding-4b` | Qwen3 Embedding 4B | buster |
| `jina-reranker-v3.5` | Jina Reranker v3.5 | buster |
| `qwen3.8-27b` | Qwen3.8 27B | buster |
| `qwopus-3.6-27b-coder` | Qwopus3.6 27B Coder MTP | digger, gareth |
| `qwopus-3.6-27b-coder-compat` | Qwopus3.6 27B Coder Compat | buster |
| `qwopus-3.6-27b-fusion` | Qwopus3.6 27B Fusion | digger |
| `qwable-3.6-35b` | Qwable 3.6 35B | digger, gareth |
| `qwable-3.6-27b` | Qwable 3.6 27B | digger |
| `ornith-1.0-35b-heretic-quality` | Ornith 1.0 35B Heretic Quality | buster |
| `ornith-1.0-35b-heretic-compact` | Ornith 1.0 35B Heretic Compact | digger |
| `ornith-1.0-35b-compact` | Ornith 1.0 35B Compact | digger, gareth |
| `gemma-4-12b` | Gemma 4 12B it QAT | buster |
| `gemma-4-26b-a4b` | Gemma 4 26B A4B | gareth |
| `qwen3.5-9b` | Qwen 3.5 9B Abliterated | nomad |
| `qwen3.6-35b-a3b` | Qwen 3.6 35B A3B | gareth |
| `laguna-s-2.1` | Laguna-S-2.1 | gareth |
| `krea-2-turbo` | Krea 2 Turbo | buster, digger |
| `sdxl-lightning` | Juggernaut XI Lightning | digger, gareth |
| `sdxl-juggernaut-xi` | Juggernaut XI | digger, gareth |
| `z-anime` | Z-Anime | digger, gareth |
| `z-image` | Z-Image | digger, gareth |
| `z-image-juggernaut-fast` | Juggernaut Z Fast | digger, gareth |
| `flux2-klein` | Flux.2 Klein 9B | digger, gareth |

Image-only or single-node ids keep a descriptive name (`ltx-2.3-video`,
`ternary-bonsai-27b`, …) and must not be prefixed with a hostname or
`-waldron`.
