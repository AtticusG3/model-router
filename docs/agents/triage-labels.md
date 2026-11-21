[Docs home](../README.md) › Agent setup › Triage Labels

# Triage Labels

> **Type:** Reference · **Audience:** agents and maintainers · **Last reviewed:** 2026-11-22

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker.

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

Edit the "Label in our tracker" column to match whatever vocabulary you actually use.

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Domain docs](domain.md): how agents read GLOSSARY and ADRs.

## Linked from

<!-- backlinks:start -->
- [model-router docs](../README.md)
<!-- backlinks:end -->
