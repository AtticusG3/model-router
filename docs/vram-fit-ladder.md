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

Every chat stanza uses the TurboQuant `llama-server` (`/opt/ai/bin/llama-server`).
Start at the highest cache quality, then step down. Do not cut context
until cache is at turbo4.

| Step | K cache | V cache | Notes |
|------|---------|---------|--------|
| 1 | f16 | f16 | Best quality, most VRAM |
| 2 | q8_0 | q8_0 | First compression step |
| 3 | turbo4 | turbo4 | Fleet default. Stock llama.cpp (if you must): q4_0 / q4_0 |

## Slots (`--parallel`)

`--parallel n` is llama.cpp's slot count. **`--ctx-size` is the total KV
across all slots**, not the per-slot window. Each slot's context is
`--ctx-size / n`. Two 256k slots means:

```
--ctx-size 524288 --parallel 2
```

`--ctx-size 262144 --parallel 2` is ~128k per slot. `--parallel 3` at
262144 is ~87k per slot. That is not 256k.

`--ctx-size` = (per-slot target) × `--parallel`. Never raise `--parallel`
without multiplying ctx. Never cut per-slot ctx to buy a slot.

1. Fit **one** 256k slot at turbo4, extras on:
   `--ctx-size 262144 --parallel 1`. If that fails, one 128k slot
   (`131072`, parallel 1). Never below 128k per slot (specialists are
   not this ladder).
2. **MoE only** (`qwen3.6-35b-a3b`, `ornith-1.5-35b-a3b-bigbang`): the slot goal is **two** full 256k
   slots (`--ctx-size 524288 --parallel 2`). Drop MTP, then mmproj, only
   if that unlocks those two 256k slots. Prefer one 256k slot with extras
   over two 128k slots. If two 256k slots fit, **stop** — do not probe a
   third. A third 256k slot (`--ctx-size 786432 --parallel 3`) is
   opportunistic only: take it only if it loads with extras still on and
   without cutting per-slot ctx. Do not drop ctx for a third slot.
3. **Dense** (agents-a1, ornith-1.0, qwen3.8, nomad): do not chase a second
   slot. One 256k (or one 128k if that is the floor) is the target. A
   second full-size slot is opportunistic only: same extras, same
   per-slot ctx, `--ctx-size` doubled. If it OOMs, keep parallel 1.
4. Re-measure `vram_mb` at the winning `--ctx-size` / `--parallel`.
   Admission reserves the whole process, including every slot's KV.

The router parses `--parallel` / `-np` into stanza `slots` (override with
`slots:`). Local occupancy below `slots` stays local; at cap, the request
spills to a peer with the same `model_id` that has a free slot. If no
peer can take it, the request queues on the local llama-server.

Specialists (intent / embed / rerank, 8k ctx) keep `--parallel 2`.
Image/sd.cpp has no llama slots; treat as 1.

## Drop order after min ctx + turbo4 + parallel 1 still OOM

You are already at 131072 + turbo4 + `--parallel 1`. Now drop add-ons,
then backtrack ctx upward if VRAM returns. Do not spend this VRAM on a
second slot by cutting below 128k.

1. Drop **MTP / dSpark / dFlash / speculative draft** (`--spec-type`,
   `--model-draft`). These buy speed, not capability.
2. Drop **mmproj** (vision). Prefer 256k text-only over 128k+vision if
   both fit; if dropping mmproj frees 256k text, take 256k text
   (backtrack), then retry slots (above).
3. **MoE only:** offload **expert** tensors to CPU/RAM with `--n-cpu-moe`.
   Dense attention/non-expert layers stay on the GPU. Do **not** put the
   non-MoE layers on CPU — that is the wrong direction (experts are the
   bulky part).
4. **Dense only:** look for RPC / tensor-split across GPUs or nodes
   (`llama-server --rpc`, or a future router split stanza). Do not fake
   this with CPU offload of dense layers.

After each drop, retry one 256k slot then one 128k slot at turbo4. Then
apply the MoE / dense slot rules above (multiply `--ctx-size` by n).

## Do not

- Cut per-slot ctx below 131072 to keep MTP, mmproj, or an extra slot.
- Raise `--parallel` without multiplying `--ctx-size` (that shrinks every slot).
- Chase a third slot, or a second slot on a dense model, by dropping ctx or extras.
- Quantize cache past turbo4/q4_0 in order to keep 256k; cut ctx first.
- Use Q4_K_S / IQ4_XS as a "fit trick" for a Q4_K_M-class dense model.
- Give two nodes different `model_id`s for the same finetune.
- Leave `vram_mb` at a parallel-1 measurement after raising `--parallel`.

## Worked walk (chat 27B-class)

1. Weights Q5-class, `--ctx-size 262144 --parallel 1`, KV f16, extras on.
2. KV q8_0, then turbo4.
3. **MoE:** try `--ctx-size 524288 --parallel 2` at turbo4. If that OOMs,
   drop MTP, then mmproj, only to unlock two 256k slots. If two 256k
   slots still OOM, keep one 256k slot. Do not go for a third.
4. **Dense:** keep `--ctx-size 262144 --parallel 1`. Optionally probe
   `--ctx-size 524288 --parallel 2` with extras still on; take it only
   if it loads. Do not drop extras or ctx to get it.
5. If one 256k slot never fits: `--ctx-size 131072 --parallel 1`. MoE
   may then try two 128k slots (`--ctx-size 262144 --parallel 2`). Dense
   stays at one 128k slot.
6. Still OOM at one 128k slot: MoE → `--n-cpu-moe`; dense → RPC/split.

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
| `ornith-1.0-35b-heretic` | Ornith 1.0 35B Heretic (APEX Quality on 32GB, Compact on 24GB/16GB) | buster, nugget, digger, gareth |
| `ornith-1.5-35b-a3b-bigbang` | Ornith 1.5 35B A3B BigBang (Q5 grafted MTP on 32GB, Q4 no-MTP on 24GB) | buster, nugget, digger |
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
flag. Do not apply this template to
intent (`qwen3-0.6b-instruct`, `qwen2.5-1.5b-instruct`), embeddings, or
sd.cpp `--llm` vision encoders.

## Measured `vram_mb` (2026-08-21 per-slot ctx rewalk)

`--ctx-size` is total KV. Winning row is per-slot ctx × `--parallel`.
nvidia-smi **used** at `/health`, `vram_mb` rounded up. Specialists were
not rewalked. Dense 2×256k probes that loaded are recorded as unused:
the ladder does not chase a second dense slot. Gareth MoE 2×256k loaded
at `/health` without a real KV bump (~+30 MiB); leftover VRAM cannot pay
a second 256k cache, so it stays one slot.

| Node | GPU | `model_id` | Winning ladder | used | `vram_mb` |
|---|---|---|---|---:|---:|
| buster | V100 32GB | `agents-a1` | 1×256k turbo4, MTP+mmproj | 26949 | 27500 |
| buster | V100 32GB | `qwen3.8-27b` | 1×256k turbo4 | 26451 | 27000 |
| buster | V100 32GB | `ornith-1.0-35b-heretic` | 1×256k turbo4, MTP+mmproj (Quality) | 26027 | 26600 |
| buster | V100 32GB | `qwen3.6-35b-a3b` | 2×256k turbo4, MTP+mmproj (Quality), `--ctx-size 524288 --parallel 2` | 29713 | 30300 |
| buster | V100 32GB | `ornith-1.5-35b-a3b-bigbang` | 2×256k turbo4, grafted MTP Q5_K_M, `--ctx-size 524288 --parallel 2` | 32099 | 32600 |
| buster | 4060 8GB | `qwen3-0.6b-instruct` | specialist | 1019 | 1500 |
| buster | 4060 8GB | `qwen3-embedding-4b` | specialist | 3419 | 3600 |
| buster | 4060 8GB | `jina-reranker-v3.5` | specialist | 1269 | 1500 |
| nugget | V100 32GB | `agents-a1` | 1×256k turbo4, MTP+mmproj | 26395 | 27000 |
| nugget | V100 32GB | `qwen3.8-27b` | 1×256k turbo4 | 26819 | 27400 |
| nugget | V100 32GB | `ornith-1.0-35b-heretic` | 1×256k turbo4, MTP+mmproj (Quality) | 26331 | 26900 |
| nugget | V100 32GB | `qwen3.6-35b-a3b` | 2×256k turbo4, MTP+mmproj (Quality), `--ctx-size 524288 --parallel 2` | 30081 | 30600 |
| nugget | V100 32GB | `ornith-1.5-35b-a3b-bigbang` | 2×256k turbo4, grafted MTP Q5_K_M, `--ctx-size 524288 --parallel 2` | 32161 | 32700 |
| nugget | V100 32GB | `qwen3-embedding-4b` | specialist | 4029 | 4200 |
| nugget | V100 32GB | `jina-reranker-v3.5` | specialist | 1881 | 2000 |
| digger | RTX PRO 4000 24GB | `ornith-1.0-35b-heretic` | 1×256k turbo4, MTP+mmproj (Compact) | 20689 | 21200 |
| digger | RTX PRO 4000 24GB | `qwen3.8-27b` | 1×128k turbo4 (256k lost) | 22753 | 23300 |
| digger | RTX PRO 4000 24GB | `ornith-1.5-35b-a3b-bigbang` | 1×256k turbo4, no MTP (Q4_K_M). 2×256k `--ctx-size 524288` died | 23788 | 23200 |
| digger | CPU | `qwen3-0.6b-instruct` | ngl 0 | 0 | 0 |
| gareth | 5060 Ti 16GB | `qwen3.6-35b-a3b` | 1×256k turbo4, no MTP, no mmproj (Compact) | 14732 | 15200 |
| gareth | 5060 Ti 16GB | `ornith-1.0-35b-heretic` | 1×256k turbo4, MTP+mmproj (Compact) | 13808 | 14300 |
| gareth | 5060 Ti 16GB | `qwen2.5-1.5b-instruct` | specialist | 1278 | 1500 |
| nomad | RTX A2000 8GB | `qwen3.5-9b` | 1×49k turbo4, mmproj (small-card exception) | 6879 | 7400 |

Image stanzas keep conservative `vram_mb` (offload-to-cpu / `--max-vram`); they were not part of the llama.cpp ladder.
