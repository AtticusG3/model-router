# VRAM fit ladder

How to write a stanza for a given weight file so the fleet shares one
`model_id` per model (different quants of the same weight family are the
same id). Apply this on the target GPU; do not guess from another node.

The mesh lists and routes by `model_id`. Pools are not catalog names.
Legacy client ids (`coding-pool`, `daily-driver`, `embed-pool`, …) are
**aliases on the local stanza**, not a second identity.

## Identity

- Same architecture + finetune + size = same `model_id` even if the GGUF
  quant differs (`Q4_K_M` vs `Q5_K_M` vs `UD-Q5_K_XL`; APEX-I **Compact**
  vs **Quality**). Compact and Quality are APEX quant tiers, not finetunes.
- A different finetune is a different model (`qwopus-3.6-27b-coder` vs
  `-coder-compat` vs `-fusion`; heretic vs base).
- One stanza per `model_id` per node. If a node has two quants of the
  same model, keep the one this ladder would pick; do not register both.

## Hard floor (weights)

- Do not serve weight quants below **Q4_K_M** (no Q4_K_S, Q4_0, IQ4_XS,
  IQ3, Q3, Q2, Q2_K).
- Preferred weight quant: **Q5_K_M** (or Unsloth `UD-Q5_K_XL`, or APEX-I
  **Quality**, as the Q5-class equivalent). If VRAM remains after the
  full ctx/cache target, Q6_K / Q8_0 is allowed. If Q5-class cannot fit
  even at the bottom of this ladder, step the **weights** down to
  Q4_K_M (or APEX-I **Compact**). Never below.
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
| `agents-a1` | Agents-A1 Uncensored MTP Quality | buster, nugget |
| `qwen3-0.6b-instruct` | Qwen3 0.6B Instruct (rag-proxy intent) | buster, digger, nugget |
| `qwen2.5-1.5b-instruct` | Qwen2.5 1.5B Instruct (rag-proxy intent) | gareth |
| `qwen3-embedding-4b` | Qwen3 Embedding 4B (rag-proxy) | buster, nugget |
| `jina-reranker-v3.5` | Jina Reranker v3.5 (rag-proxy) | buster, nugget |
| `qwen3.8-27b` | Qwen3.8 27B UD-Q5_K_XL | buster, nugget, digger |
| `ornith-1.0-35b-heretic` | Ornith 1.0 35B Heretic (APEX Quality on 32GB, Compact on 24GB) | buster, nugget, digger |
| `ornith-1.0-35b` | Ornith 1.0 35B (APEX Compact; 16GB pick) | gareth |
| `qwen3.5-9b` | Qwen 3.5 9B Abliterated | nomad |
| `qwen3.6-35b-a3b` | Qwen 3.6 35B A3B heretic (APEX Quality on 32GB, Compact on 16GB) | buster, nugget, gareth |
| `krea-2-turbo` | Krea 2 Turbo | buster, nugget, digger, gareth |
| `z-anime-turbo` | Z-Anime Turbo (alias `z-anime`) | buster, nugget, digger, gareth |
| `ideogram4` | Ideogram 4 | buster, nugget, digger, gareth |

Image-only extras on digger/gareth (`sdxl-lightning`, `z-image`, `flux2-klein`, …) keep their local ids. Do not prefix with a hostname or `-waldron` as the identity.

## Qwen chat template (3.5 / 3.6 / 3.8)

Those chat stanzas (`qwen3.5-9b`, `qwen3.6-35b-a3b`, `qwen3.8-27b`) use
[froggeric/Qwen-Fixed-Chat-Templates](https://huggingface.co/froggeric/Qwen-Fixed-Chat-Templates)
v22.2. Copy `configs/qwen-fixed-chat-template.jinja` to
`/opt/ai/config/qwen-fixed-chat-template.jinja` on every node that hosts
them, then pass:

```
--jinja --chat-template-file /opt/ai/config/qwen-fixed-chat-template.jinja
--reasoning-format deepseek
```

Add `--reasoning-preserve` only when that llama-server build lists the
flag (buster stock, nugget TurboQuant). Do not apply this template to
intent (`qwen3-0.6b-instruct`, `qwen2.5-1.5b-instruct`), embeddings, or
sd.cpp `--llm` vision encoders.

## Measured `vram_mb` (2026-08-20)

nvidia-smi **used** at `/health`, then `vram_mb` rounded up for admission headroom.

| Node | GPU | `model_id` | Winning ladder | used | `vram_mb` |
|---|---|---|---|---:|---:|
| buster | V100 32GB | `agents-a1` | 256k KV f16, MTP+mmproj | 28437 | 29000 |
| buster | V100 32GB | `qwen3.8-27b` | 256k KV q8_0 (f16 lost) | 28703 | 29200 |
| buster | V100 32GB | `ornith-1.0-35b-heretic` | 256k KV f16, MTP+mmproj (Quality) | 28635 | 29200 |
| buster | V100 32GB | `qwen3.6-35b-a3b` | 256k KV f16, MTP+mmproj (Quality) | 28965 | 29500 |
| buster | 4060 8GB | `qwen3-0.6b-instruct` | specialist | 1019 | 1500 |
| buster | 4060 8GB | `qwen3-embedding-4b` | specialist | 3419 | 3600 |
| buster | 4060 8GB | `jina-reranker-v3.5` | specialist | 1269 | 1500 |
| nugget | V100 32GB | `agents-a1` | 256k KV f16, MTP+mmproj | 29003 | 29500 |
| nugget | V100 32GB | `qwen3.8-27b` | 256k KV q8_0 (f16 lost) | 29059 | 29500 |
| nugget | V100 32GB | `ornith-1.0-35b-heretic` | 256k KV f16, MTP+mmproj (Quality) | 29241 | 29700 |
| nugget | V100 32GB | `qwen3.6-35b-a3b` | 256k KV f16, MTP+mmproj (Quality) | 29333 | 29800 |
| nugget | V100 32GB | `qwen3-embedding-4b` | specialist | 4029 | 4200 |
| nugget | V100 32GB | `jina-reranker-v3.5` | specialist | 1881 | 2000 |
| digger | RTX PRO 4000 24GB | `ornith-1.0-35b-heretic` | 256k KV f16, MTP+mmproj (Compact) | 23299 | 23500 |
| digger | RTX PRO 4000 24GB | `qwen3.8-27b` | 128k KV q4_0 (256k lost) | 22118 | 22500 |
| digger | CPU | `qwen3-0.6b-instruct` | ngl 0 | 0 | 0 |
| gareth | 5060 Ti 16GB | `qwen3.6-35b-a3b` | 128k KV turbo4, no MTP, no mmproj (Compact) | 14764 | 15200 |
| gareth | 5060 Ti 16GB | `ornith-1.0-35b` | 256k KV f16, MTP+mmproj (Compact) | 14660 | 15100 |
| gareth | 5060 Ti 16GB | `qwen2.5-1.5b-instruct` | specialist | 1278 | 1500 |

Image stanzas keep conservative `vram_mb` (offload-to-cpu / `--max-vram`); they were not part of the llama.cpp ladder.
