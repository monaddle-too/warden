# Workspace panel: shared repositories on a chat that has not run

Status: started 2026-09-18 on branch `fix/workspace-repositories-fresh-chat`
from origin/main 144d4f4.

## Objective

Sharing a GitHub repository with a workspace shows up in the workspace
panel at once. Today, on a chat that has not sent its first message, the
panel keeps saying no repository is shared until the first turn runs (or
some other action changes the chat); the share itself succeeded.

## What exists

- `chats/environments.go` `Engine.Environments` builds `GET environments`.
  It asks the policy service for the workspace's shared repositories
  (`github_list`) only inside the `ranChat(chats) != nil` block, the block
  that also asks the runner for the sandbox's status, usage and pod.
  `ranChat` is a chat with a thread, entries or a run id — none of which a
  fresh chat has. The policy side keys `github_list` on `sandboxID`; the
  `chatID` only has to be a valid identifier.
- `web/src/components/RepositorySharing.tsx` `save()` posts
  `sharing/github_select` and closes the dialog; the panel learns of the
  change from `ChatShell.tsx`'s 5 s poll of `environments`, or sooner when
  something dispatches `warden-refresh-state`.

## Steps

1. List repositories for every live workspace, whether or not a chat has
   run: move the `github_list` call out of the `ranChat` block, using the
   workspace's first chat as the identifier.
2. `RepositorySharing.save()` dispatches `warden-refresh-state` so the
   panel updates as the dialog closes instead of on the next poll.
3. A unit test in `chats/sharing_test.go` (or `environments_test.go`) for a
   workspace whose only chat has not run: `Environments` returns the
   shared repositories.

## Key decisions

1. Fix on the server rather than in the panel: the panel already shows
   `ws.repositories`; the list was simply not being filled.

## Progress log

- 2026-09-18: opened; steps 1–3 done. `Environments` asks `github_list`
  for every live workspace (deleted ones excepted) with its first chat as
  the identifier; `save()` dispatches `warden-refresh-state`;
  `TestEnvironmentsListRepositoriesBeforeTheFirstTurn` in
  `chats/grants_test.go` (fails on main: the fresh chat's listing has no
  repositories). `go test ./internal/chats/` and `tsc --noEmit` pass. Not
  live-tested in a browser; not deployed.
