---
name: new-worktree
description: Start a piece of Warden work the way this workspace expects — a new git worktree under .local/ on a dedicated branch from origin/main, a row in the workspace AGENTS.md checkouts table, and a plan document stub in docs/. Use this whenever the user asks to start, begin, kick off, spike, or experiment on a feature or fix, says "new branch" or "new worktree", or gives a task that is not a one-line change to an existing branch; use it before writing any code for such a task, even when the user does not mention branches.
---

# Open a worktree for new work

Every checkout of the Warden repository is a git worktree of one shared
`.git` (the private history repo), listed in the workspace `AGENTS.md`
table one level above the repo. Work that starts on an existing worktree's
branch gets entangled with it, and a worktree that is not in the table is
invisible to the next session, so the three steps below always go
together.

Git writes need the Bash sandbox off on this Mac.

## 1. Create the worktree from origin/main

From the mainline checkout (the table's **Mainline** row, today
`.local/warden-local-deployments`):

```sh
git fetch -q origin
git worktree add -b <branch> ../<name> origin/main
```

- `<name>` is `warden-<topic>` (`warden-install-preflight`), so the
  path reads `.local/warden-<topic>`. Panta worktrees are
  `panta-<topic>`.
- `<branch>` prefixes in use: `feat/` for features, `exp/` for
  experiments that may not ship, `plan/` for a multi-track plan whose
  tracks branch from it, `k8s/`, `wld-` for tracks of that plan. Match
  the neighbours.
- Branch from `origin/main`, not from the mainline checkout's local
  branch (it lags). Branch from another feature branch only when the user
  says the work builds on it, and say so in the table row.
- Some git-ignored files are needed to operate but are not in main: the
  GKE `deploy/k8s/gke/env` and `dist/gke/kubeconfig`. Copy them from a
  worktree that has them (the table says which) only when the work
  needs the cluster.

## 2. Add the row to the workspace AGENTS.md

The table is under "Active checkouts" in
`/Users/danielporter/Documents/warden-workspace/AGENTS.md`, one line per
worktree:

```
| `.local/warden-<topic>` | `feat/<topic>` | One sentence on the work; plan `docs/<topic>-plan.md` (<date>). |
```

Keep the role a sentence, with the date opened; the row is updated to
`**Merged to main <date>**` by `merge-to-main` and removed by
`finish-worktree`.

## 3. Start the plan document

`docs/<topic>-plan.md` in the new worktree, kept current throughout the
work so another session can resume from it. The shape the existing plans
share:

```markdown
# <Title>

Status: started <date> on branch `<branch>` from origin/main <sha>.

## Objective
What changes for the owner or the operator, in a paragraph.

## What exists
The relevant code as it is today (files, types, routes), from reading the
tree and docs/feature-map.md, not from memory.

## Steps
Numbered, each independently verifiable.

## Key decisions
Numbered, with the reason; add as they are made.

## Progress log
Dated entries: done, verified how, what remains.
```

Commit the stub as the branch's first commit. Then read the repo's
`AGENTS.md` and `docs/feature-map.md` before touching code — the map says
which files own the feature and must be updated in the same change.

## Related

`merge-to-main` lands the branch; `finish-worktree` removes the worktree
afterwards.
