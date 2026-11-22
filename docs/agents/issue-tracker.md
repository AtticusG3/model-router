[Docs home](../README.md) › Agent setup › Issue tracker: Gitea

# Issue tracker: Gitea

> **Type:** Reference · **Audience:** agents and maintainers · **Last reviewed:** 2026-11-22

Issues and specs for this repo live as Gitea issues in `kevyn/model-router` on
`git.kevynwatkins.com`. Issue and PR numbers (#185, #222) refer to that repo.
`AtticusG3/model-router` on GitHub is a mirror: Gitea main is canonical and
GitHub only fast-forwards. Do not open issues or PRs on the mirror, and do not
merge an outside contribution there.

`gt` is the Gitea CLI and the only CLI that sees these issues. `gh` reaches the
GitHub mirror only.

## Commands

Create an issue with the API. Labels go in by **id**, not name:

```bash
GT=$(git config --get credential.https://git.kevynwatkins.com.password)
B=https://git.kevynwatkins.com/api/v1
curl -s -H "Authorization: token $GT" "$B/repos/kevyn/model-router/labels" | jq -r '.[] | "\(.id)\t\(.name)"'
curl -s -X POST -H "Authorization: token $GT" -H 'Content-Type: application/json' \
  "$B/repos/kevyn/model-router/issues" \
  -d '{"title":"...","body":"...","labels":[785]}'      # 785 = docs
```

Read, list, comment, close and drive PRs with `gt`, naming the repo every time:

| Task | Command |
|---|---|
| Read an issue | `gt issue view <n> --repo kevyn/model-router` |
| List open | `gt issue list --repo kevyn/model-router --state open --limit 100` |
| Comment | `gt issue comment <n> --repo kevyn/model-router --body "..."` |
| Close | `gt issue close <n> --repo kevyn/model-router` |
| Read a PR | `gt pr view <n> --repo kevyn/model-router` |
| Open a PR | `gt pr create --repo kevyn/model-router --base master --head <branch> --title "..." --body "..."` |
| Merge a PR | `gt pr merge <n> --repo kevyn/model-router --method merge --yes` |

## Pitfalls

- A `GITEA_TOKEN` in the environment overrides the stored `gt` login and makes
  every call fail (`1 of 2 credentials cannot authenticate`). Run
  `env -u GITEA_TOKEN gt ...`.
- A PR body containing `Closes #N` closes N when the PR merges. Do not close it
  again; the "already closed" warning means the merge did it.
- Gitea refuses to close an issue that still has an open `blocked by` dependency
  (HTTP 412). Close the blocker first; `gt` has no command to remove the link.
- Issues and PRs share one number space, so `#222` may be either. Try
  `gt pr view 222`, then `gt issue view 222`.
- Labels may contain `/` (`severity/p1`); URL-quote them in API paths.

## When a skill says "publish to the issue tracker"

Create a Gitea issue as above. Text that names hosts, clients or credentials
stays on Gitea and is never copied to the GitHub mirror.

## When a skill says "fetch the relevant ticket"

`gt issue view <n> --repo kevyn/model-router`.

## See also

- [Docs index](../README.md): every docs page, by reader.
- [Domain docs](domain.md): how agents read GLOSSARY and ADRs.

## Linked from

<!-- backlinks:start -->
- [model-router docs](../README.md)
<!-- backlinks:end -->
