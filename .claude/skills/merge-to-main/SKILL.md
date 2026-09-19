---
name: merge-to-main
description: Land the current feature branch on origin/main — merge main in if it moved, resolve the conflicts this repo tends to produce, run the full Go/web/chart verification, push, fast-forward main, and update the workspace table, plan document and memory. Use this whenever the user says merge to main, push to main, land it, ship it, "is this merged?", or asks to fast-forward main to a branch; also when they want the branch merged and then deployed.
---

# Merge a branch to main

The repository's `main` is only ever fast-forwarded: a branch is brought
up to date with `origin/main` by merging main *into the branch*, verified
there, pushed, and then `origin/main` is pushed to the branch's head. No
merge commit is ever made on main itself, and main is never force-pushed.
Sessions that skipped a step here have shipped a dropped CSS brace and a
feature-map row that named a file which no longer existed, so the
verification is not optional.

Git writes (commit, push, worktree operations) need the Bash sandbox off
on this Mac; reads work inside it.

## 0. Is there anything to merge?

"merge to main" is often asked when the branch already landed. Check
before doing anything:

```sh
git fetch -q origin
git log --oneline origin/main..HEAD | head        # commits main lacks
git log --oneline HEAD..origin/main | head        # commits the branch lacks
git status --short | wc -l                        # uncommitted work
```

Empty first list → already merged; say so with the sha and stop.
Uncommitted work → commit it (or ask) before merging; never merge a dirty
tree.

## 1. Bring main into the branch

If `HEAD..origin/main` is empty the branch fast-forwards; skip to step 2.
Otherwise:

```sh
git merge --no-commit origin/main
git diff --name-only --diff-filter=U
```

Conflict hotspots in this tree and how they have been resolved before:

- `chat/web/src/chat.css` — take the union of both sides, then run
  `pnpm --dir chat/web test -- chat.css` (the brace-balance test exists
  because a seam once dropped a closing brace).
- `docs/feature-map.md` — union the rows, then check that every path in
  the rows you touched exists (`AGENTS.md` makes the map part of done).
- `chat/web/src/components/{ChatShell,WorkspacePanel}.tsx` and the panel
  types — usually both sides added props/state; keep both.
- Go call sites of a constructor one side extended (`sandbox.Create`,
  `RuntimeSpec`) — port main's new callers to the branch's signature,
  then `go vet ./...` finds the ones you missed.

Commit the merge with a message that names what main brought and how
each conflict was resolved; the next merger reads it.

## 2. Verify on the merged tree

```sh
cd chat && GOPROXY=off GOFLAGS=-mod=mod gofmt -l . && go vet ./... && go test ./... ; cd ..
pnpm --dir chat/web install --frozen-lockfile && pnpm --dir chat/web build && pnpm --dir chat/web test
deploy/helm/warden/test.sh          # only when deploy/helm changed; --update rewrites goldens, review the diff
```

A failure here is a bug on the merged tree, not a reason to skip; fix it
on the branch and commit.

## 3. Push and fast-forward main

```sh
git config user.email                              # must be the repo-local GitHub noreply address, never a personal one
git push origin HEAD
git merge-base --is-ancestor origin/main HEAD && git push origin HEAD:main
git fetch -q origin && git rev-parse --short origin/main HEAD   # the two must match
```

If the ancestor check fails, main moved while you verified: go back to
step 1. A push to main triggers `.github/workflows/guest-image.yml` on the
self-hosted Mac runner when `deploy/guest/**` changed; mention it when
that is the case.

Optionally bring the mainline checkout along so its table row is not
stale: `git -C ../warden-local-deployments merge --ff-only origin/main`
(its branch `public` tracks `origin/main`; skip if it has local changes).

## 4. Record the landing

- Workspace `AGENTS.md` (the checkouts table one level above the repo):
  change the row's role to `**Merged to main <date>** (<sha>). …` and keep
  any note about files the worktree still holds (GKE `env`, kubeconfig).
- The plan document `docs/<feature>-plan.md`: progress line with the
  merge sha and what remains.
- The memory file for the work, if one exists: merged sha and date.

## 5. Offer the next step

Merging does not deploy anything. Ask, or if the user already said so,
continue with the right deployment skill: `deploy-local` for this Mac,
`gke-deploy` for the cloud cluster, the OVH procedure for production.
Then `finish-worktree` removes the worktree once nothing else needs it.
