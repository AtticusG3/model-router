## Agent skills

### Issue tracker

Issues and specs live as Gitea issues in `kevyn/model-router`. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

## Verification

- Go: `make check`.
- sd-server shim: `python3 -m unittest discover -s deploy/tests -v`. Standard library only; no GPU or backend needed.
- Docs: `python3 <skills>/kevyn-docs-standard/scripts/docs_lint.py . --skip .cursor --skip .agents --skip .claude` must report 0 errors. Repo terms it should not flag go in `.docs-lint-allow`.

## Documentation rule (mandatory)

Docs are part of done. Follow the `kevyn-docs-standard` skill. The docs map is `docs/README.md`.

1. Any change to behaviour, config, CLI, API, deploy steps or user-visible output updates the matching docs **in the same PR**.
2. Update `Last reviewed:` on every page you touch.
3. Add a CHANGELOG line under `## [Unreleased]` when users or operators will notice.
4. New pages: add breadcrumbs, `See also`, a `Linked from` block, and list them in `docs/README.md`.
5. Run the docs lint with `--fix-backlinks` and fix every error before pushing.
6. If no docs change is needed, say "no docs impact" and why in the PR description.
