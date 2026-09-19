---
name: finish-worktree
description: Retire a Warden git worktree once its branch has landed — confirm nothing unmerged or uncommitted remains, rescue git-ignored operating files, remove the worktree and its row from the workspace AGENTS.md table, and note it in memory. Use this whenever the user asks to clean up, remove, delete or prune a worktree or "the stale worktrees", says a branch is done, or after merge-to-main when nothing else needs the checkout; also when the user asks which worktrees can go.
---

# Retire a finished worktree

The workspace `AGENTS.md` says to clean up a worktree after its work is
merged and never to discard uncommitted work while doing so. Removing a
worktree is quick; recovering work lost with it is not, so the checks
come first and each is a reason to stop and report rather than proceed.

Git writes need the Bash sandbox off on this Mac.

## Which worktrees are candidates

The table's rows marked **Merged** and its "Stale worktrees" list, cross-
checked against git:

```sh
git -C .local/warden-local-deployments worktree list
```

## Checks, per worktree

Run all of them; one failing check keeps the worktree.

```sh
W=.local/<name>
git -C $W status --porcelain | head                      # 1. must be empty (untracked files count)
git -C $W fetch -q origin
git -C $W log --oneline origin/main..HEAD | head          # 2. must be empty: every commit is on main
git -C $W stash list                                      # 3. stash entries are shared across worktrees; leave them alone
ls $W/deploy/k8s/gke/env $W/dist/gke/kubeconfig 2>/dev/null   # 4. git-ignored operating files
```

- Untracked or modified files: show them to the user; commit, move or
  get an explicit decision. Never `git clean` or `--force` here.
- Commits missing from main: the branch is not merged; run
  `merge-to-main` or leave the worktree and say why.
- The GKE `env` and `kubeconfig` (and anything else `.gitignore` lists
  that took effort to produce) do not survive removal. If this is the
  only worktree holding them, copy them to another live worktree first
  and update that worktree's table row to say it now holds them.
- A deployed release built from this worktree keeps working after
  removal (`~/.warden/releases/` holds unpacked copies); only the source
  goes.

## Remove

```sh
git -C .local/warden-local-deployments worktree remove .local/<name>
git -C .local/warden-local-deployments branch -d <branch>      # -d refuses an unmerged branch; do not use -D
git -C .local/warden-local-deployments worktree prune
```

Leave the branch on `origin`; it is the history of the work.

## Record it

- Delete the row from the workspace `AGENTS.md` table (or the entry in
  its "Stale worktrees" line).
- If a memory file for the work names the worktree path, note that it is
  gone and where the git-ignored files went.

Report what was removed, what was kept and why, in a few lines.
