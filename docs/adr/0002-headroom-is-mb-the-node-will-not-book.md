[Docs home](../README.md) › Decisions › Headroom is MB the node will not book

# Headroom is MB the node will not book

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Admission was willing to book the last free megabyte, and peers selected candidates from that same raw free figure. `headroom_mb` is one integer on the node, subtracted once from each GPU before a `vram_mb` check, default 0. The same subtraction comes off every free figure the node publishes, including free-if-evicted. It is not a per cent of the card and not a haircut on `vram_mb`. A per cent would mean a different spare on a 4060 and a V100. Publishing raw `nvidia-smi` free would send this node loads it is required to refuse.

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Glossary](../../GLOSSARY.md): the terms these decisions use.

## Linked from

<!-- backlinks:start -->
- [model-router docs](../README.md)
<!-- backlinks:end -->
