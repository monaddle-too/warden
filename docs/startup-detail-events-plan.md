# Startup detail in full, and Kubernetes events

Status: started 2026-09-17 on branch `feat/startup-detail-events` from
origin/main 21615a0.

## Objective

Two gaps the owner hit watching a chat start on GKE:

1. The status line under the composer cuts the startup detail off
   ("Resuming the sandbox · waiting for a node: 0/2 nodes are available:
   2 node…"); the rest is only a tooltip. The line should show the whole
   detail, and open the workspace panel where the start is explained.
2. Nothing reads Kubernetes **events**, so the one thing `kubectl describe
   pod` answers — is a node being added (`TriggeredScaleUp`), or can none
   be (`NotTriggerScaleUp`); is the image pulling; did a volume fail to
   mount — is invisible. Events belong per pod (workspace panel while
   starting and after, cluster page) and cluster-wide (a "Recent events"
   list on the admin console's Cluster section). While a pod waits, the
   startup detail itself should say what the latest event says.

## What exists

- Stages: `sandbox/progress.go` (`Report`), `chats/startup.go`
  (`Startup{stage, detail, since}`), `web/src/stages.ts` (`startupLine`),
  rendered in `Conversation.tsx`'s composer footer as a single ellipsised
  span (`conversation.css` `.composer-footer > span > span`) with the
  detail as `title`. The detail on Kubernetes is
  `sandbox/kube/driver.go` `StartupDetail(pod)`, reported from
  `awaitRunning`'s pod watch — which only fires on pod updates, so an
  unscheduled pod's detail never changes while it waits.
- Cluster view: `sandbox.ClusterInspector` (`sandbox/cluster.go`:
  `Pod`, `Cluster`, `Logs`), `sandbox/kube/cluster.go` (3 s cache),
  runner ops `pod` / `cluster.status` / `cluster.logs`, chat routes `GET
  environments` → `pod`, `GET cluster`, `GET cluster/logs`; web
  `WorkspacePanel.tsx` `Pod` facts, `ClusterView.tsx` tables + log viewer.
- RBAC: chart `templates/rbac.yaml`: Role `warden-runner` (sandbox
  namespace), Role `warden-runner-view` (release namespace), ClusterRole
  `<prefix>-runner-view` (nodes). No `events` verbs anywhere.
- The client (`chat/internal/kube`) has no `Event` type; the fake API
  server in `apiserver_test.go` supports nested field selectors.

## Steps

1. `kube.Events` resource and `kube.Event` type (involvedObject, reason,
   message, type, count, first/last/eventTime, source, reportingComponent).
2. `sandbox.Event` (owner-facing: at, type, reason, message, count,
   object kind/name/namespace, source) and `EventHint(reason, message)`
   in `sandbox/kube` — the owner's words for the reasons that matter
   (`TriggeredScaleUp`, `NotTriggerScaleUp`, `FailedScheduling`,
   `Pulling`/`Pulled`, `FailedMount`/`FailedAttachVolume`, `Scheduled`,
   `BackOff`, `Unhealthy`, `Evicted`, `OOMKilling`…), a Warning's
   `reason: message` otherwise.
3. `PodInfo.Events` (newest first, at most 20): `Pod()` lists by
   `involvedObject.uid`; `Cluster()` lists each namespace once and
   matches by uid. `ClusterStatus.Events` (+ `EventsError`): the newest
   100 across the sandbox and service namespaces.
4. RBAC: `events` get/list in `warden-runner` and `warden-runner-view`;
   chart goldens.
5. Driver: `awaitRunning` keeps the last pod and, on a 10 s ticker while
   the pod is not running, lists its events and reports
   `StartupDetail(pod)` + ` · ` + the latest event's hint when it adds
   something (`waiting for a node: … · a node is being added`).
6. Web: the status line wraps (no ellipsis while starting) and is a
   button that opens the workspace panel (`Conversation` gets
   `onOpenWorkspace`); the panel shows a **Starting** section (stage,
   full detail, elapsed) while the chat starts, and the Pod section lists
   the pod's events; `ClusterView` gains **Recent events** with a per-pod
   filter from an Events button on each pod row.
7. `docs/feature-map.md` rows for startup stage and cluster visibility;
   this plan's progress log.
8. Verify: Go unit tests (`kube`, `sandbox/kube`, `chats`), web tests,
   helm goldens; then deploy to GKE (`gke-deploy`) and watch a cold
   start's status line and panel.

## Key decisions

1. Events are read by the runner (it holds the client and the Roles),
   like the rest of the cluster view; the chat only relays.
2. Node events (namespace `default`) are not read: they would need a
   Role in a namespace the chart does not own; the pod's own
   scale-up/scheduling events carry what the owner needs.
3. The startup detail carries the latest event's hint rather than the
   chat merging events into the status: one source of the sentence.

## Progress log

- 2026-09-17: worktree opened, plan written.
