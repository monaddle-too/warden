# Workspaces that survive a redeploy and stay up 30 minutes

Status: started 2026-09-18 on branch `feat/workspace-keepalive` from
origin/main be6d608.

## Objective

Chats on the GKE deployment were regularly interrupted and then waited
minutes for their workspace to come back. Two causes, both in the runner:

1. **Every runner restart stops every workspace.** `initializeManaged`
   stops each registered sandbox and the Kubernetes driver's `Reconcile`
   deletes every pod under its labels (a documented decision of the
   Kubernetes plan: "no sandbox pod is legitimately running when the
   worker starts"). A deploy restarts the runner (`Recreate`), so each
   deploy killed the running workspace pods; on Autopilot the gVisor node
   then scaled to zero and the next message waited for a new node (the
   cluster's events on 2026-09-18 16:07: `Killing … guest` at the second
   the new runner pod started, then `TriggeredScaleUp 0->1` and a
   `FailedAttachVolume` retry when the chat resumed it four minutes
   later).
2. **The idle window is counted from the wrong moment.** The engine
   releases an idle agent session 10 minutes after the last turn; the
   runner's sweep counts `sandboxes.stopAfterIdleMinutes` (15) from the
   moment that stream ends, since `finishManagedRun` sets `LastActivity`.
   So a workspace stops about 25 minutes after its last turn, and the
   setting's documented meaning ("no chat activity") is not what it does.

After this work: a runner restart on Kubernetes adopts the workspace
pods that are still running (their identity pinned per generation by the
policy service, which persists the pins) instead of deleting them, so a
redeploy costs a chat only the turn in flight and the next message
resumes in seconds; and a workspace stays up for 30 minutes (the new
default of `stopAfterIdleMinutes`) after the last chat activity — a
turn's end, a message, a person's command, a preview, a review — with the
session release not counting.

## What exists

- `chat/internal/sandbox/managed.go`: `initializeManaged` (startup:
  stops every non-stopped registered sandbox, ends active grants,
  removes spares, then `Reconciler.Reconcile(registered names)`),
  `SweepIdle` (stops a running sandbox `IdleTimeout` after
  `LastActivity` unless a run is active or a preview is live),
  `finishManagedRun` (stream end: `LastActivity = now`, stop on explicit
  cancel / error), `startLocked` (a running sandbox keeps its
  `Generation`; `Gate.Register` + `Gate.Check("runtime")` on the same
  generation).
- `chat/internal/sandbox/runtime.go`: `Reconciler` interface
  (`Reconcile(ctx, registered []string) error`), `NoResidency`.
- `chat/internal/sandbox/kube/driver.go`: the in-memory `runtime` view
  (claim UID, pod UID, pod IP, generation, workspace, publications),
  `ensureRunning` (adopts an existing labelled pod, `known` by UID),
  `Reconcile` (deletes every pod, keeps registered claims).
- `chat/internal/policy/kube/inspector.go`: identity pins
  (`kube-runtime-identities.json`: volume UID + pod UID per generation)
  persisted in the policy state; a re-check of a known generation with
  the same pod UID passes.
- `chat/internal/chats/engine.go`: `ResidentIdle` (10 min session idle),
  `awaitMessage`, `settleTurn`, `run`'s end; the engine calls runner ops
  `bind-chat`, `prepare`, `stream`, `checkpoint` (before every user
  turn), `cancel`, `stop`.
- Config: `sandboxes.stopAfterIdleMinutes` (`config.go` default 15,
  `runnersvc` `--idle-timeout`, chart `values.yaml`, goldens, docs).

## Steps

1. Runner: `Reconciler.Reconcile` reports which registered runtimes are
   still resident; the Kubernetes driver keeps a pod whose sandbox is
   registered and whose generation annotation matches, rebuilding its
   view of it (claim UID, pod UID and IP, workspace), and deletes the
   rest as before. `initializeManaged` keeps those sandboxes `running`
   with `LastActivity = now` (a full idle window after the restart) and
   no active run; the SBX path is unchanged (its guests die with the
   worker).
2. Runner: a new op `activity` sets `LastActivity` to the time the
   engine reports (never backwards); `finishManagedRun` no longer bumps
   it on a clean stream end.
3. Engine: report activity at every turn's end and when a run ends for
   any reason other than the idle release (the last turn's end is what
   it reports, so a release for another chat or a Stop does not extend
   the window).
4. Default `stopAfterIdleMinutes` 30 in config, runner flag, chart values,
   goldens and docs; the Kubernetes doc's upgrade note and the
   feature map updated.
5. Live: deploy to GKE, run a chat, redeploy, confirm the pod survives
   and the next message resumes without a node wait; confirm the sweep
   at 30 minutes.

## Key decisions

1. Adopt on Kubernetes only. SBX guests stop with their keep-alive
   session, so "stop everything at startup" remains right there; the
   Reconciler already marks the drivers whose guests outlive the worker.
2. Activity is reported by the engine, not inferred by the runner: the
   runner cannot tell an idle release from a turn's end (both close the
   stream), and parsing the agent protocol in the runner would couple it
   to the adapters.
3. The default moves from 15 to 30 minutes with the semantic change:
   under the old counting a workspace lived ~25 minutes after its last
   turn, so 15 minutes from the last turn would have been a cut.

## Progress log

- 2026-09-18: diagnosis on the GKE cluster (events and runner log);
  worktree and plan opened.
