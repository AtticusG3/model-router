[Docs home](../README.md) › Decisions › A failed load tries the next router peer

# A failed load tries the next router peer

> **Type:** Explanation · **Audience:** maintainers · **Last reviewed:** 2026-11-22

Picking one router peer and then returning 503 sticks the client on a node that just entered start cooldown. A failed load walks the other router peers that list the model, skipping any peer remembered until its `Retry-After`, and stops when one accepts or none are left. The walk covers a direct peer target, the last hop of a local spill, and a slot spill. It stops once response bytes have started. If none accept, a slot spill queues on the local model. A request with no local model returns 503 whose body is the last failure and whose `Retry-After` is the smallest window any refusal sent, or absent if none sent one. Pools are unchanged. A `kind: openai` peer stays a single hop. A node that is itself in start cooldown answers 503 and does not bounce the request onward. The origin owns the walk.

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Glossary](../../GLOSSARY.md): the terms these decisions use.

## Linked from

<!-- backlinks:start -->
- [model-router docs](../README.md)
<!-- backlinks:end -->
