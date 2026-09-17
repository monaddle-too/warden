# Startup stages and Kubernetes cluster visibility

Branch `feat/k8s-startup-visibility` (worktree `.local/warden-k8s-visibility`),
from `plan/warden-kubernetes` with `feat/workspace-sandbox-stats` (be91f42,
the workspace panel's Resources section) cherry-picked underneath. Started
2026-09-17.

## Objective

1. **Startup stages.** A chat whose message is waiting for its agent said
   "Message queued" and then "Agent is running" for the whole wait. On
   Kubernetes that wait can be minutes (a node being provisioned, an image
   pulled, a Kata VM booted), so the chat now says which stage the start is
   at, with the runtime's own detail ("scheduling: 0/3 nodes are available
   …", "pulling the guest image"), on every shape.
2. **Cluster visibility.** The owner can see, without kubectl, what the
   cluster is doing: each workspace names its pod (node, phase, tier,
   requests and limits, live CPU and memory beside the guest-reported
   Resources), and the admin console has a Cluster section with the nodes,
   the sandbox pods, the Warden service pods, and a log viewer for any of
   them.

## Design

### Stages

`sandbox.Progress{Stage, Detail, Since}` is one startup report. Stages, in
the order a cold start passes through them:

| Stage | Set by | Meaning |
|---|---|---|
| `queued` | chat engine | the message waits for a run slot or for another chat on the same workspace |
| `binding` | chat engine | registering the chat with the runner |
| `waiting` | runner | a capacity or policy gate before the sandbox is touched |
| `creating` | runner (detail from the driver) | a new sandbox: volume, pod, scheduling, image pull, container start, manifest check; on SBX the VM |
| `resuming` | runner (detail from the driver) | a stopped sandbox's pod (or VM) coming back |
| `attesting` | runner | the policy service checks the runtime's networking |
| `probing` | runner | reading the guest report |
| `installing` | runner | copying the Codex bundle or the Claude executable into the guest |
| `cloning` | runner | fetching the workspace repository |
| `launching` | chat engine | the agent process starting in the guest |
| `connecting` | chat engine | the agent thread starting or resuming |

The report is not persisted anywhere: a restart clears it with the run.

- **Runner.** `Worker.progress` (its own mutex, never `w.mu`, which
  `prepare` holds for its whole duration) keyed by sandbox ID; `prepare`
  sets the stage as it goes and clears it when it returns. The new control-
  lane op `progress` reads it; its identity check is against the control
  bindings, so it never waits behind a creation. Drivers report sub-stage
  detail through the context (`sandbox.WithProgress`, `sandbox.Report`): the
  Kubernetes driver from `awaitRunning`'s pod state (trust bundle, claim,
  pod created, scheduling condition, container waiting reason, address,
  manifest, workspace copy), the SBX driver around `sbx create` and the
  boot.
- **Prepare timeout.** `Worker.PrepareTimeout` bounds `prepare` (2 minutes
  as before on the sbx shapes; 10 minutes on Kubernetes, set by the runner
  service, since a node can take that long to join). The chat's runner
  client gives `prepare` a 15-minute connection deadline instead of 5.
- **Chat engine.** `Engine.startup` (chat ID → `Startup{Stage, Detail,
  Since}`) is filled by `run` and merged into `Chat.Startup` by `View`, the
  way typing indicators are, so the SSE stream carries it. While `prepare`
  is in flight a goroutine polls the runner's `progress` op every second and
  copies the runner's stage over the engine's `preparing` placeholder.
- **Web / TUI.** The composer's status line shows the stage label and the
  detail while `chat.startup` is set; the sidebar entry and the workspace
  panel's chat list use the stage label in place of "running"; the queued
  entry's "Queued" tag becomes "Starting…" once a stage is known. The TUI
  prints the same line under the transcript.

### Cluster visibility

- **Source.** The runner (it drives the pods and already has the client and
  a Role in the sandbox namespace). `sandbox.ClusterInspector` is an
  optional interface a driver may satisfy; `sandbox/kube/cluster.go`
  implements it: `Pod(ctx, runtimeName)` for one sandbox and
  `Cluster(ctx)` for the whole picture, both cached for 3 s. The SBX driver
  does not, and the chat service answers `{"available": false}`.
- **Runner ops** (control lane, no `w.mu` beyond the binding check):
  `pod` (per sandbox, needs the chat/sandbox binding), `cluster.status`,
  `cluster.logs` (owner-level, like `stats`; the runner admits only the
  chat service).
- **Chat API.** `GET environments` → `runtime.pod` (`sandbox.PodInfo`);
  `GET cluster` → `sandbox.ClusterStatus`; `GET cluster/logs?namespace=
  &pod=&container=&tail=&previous=` → `{lines: []string, truncated}`.
  The edge lists `api/cluster` under `ownerOnly`, so a demo sign-in cannot
  read pod logs.
- **What is shown.** Nodes: name, ready, roles, kubelet version, runtime,
  allocatable CPU/memory, usage (metrics.k8s.io) and the sandbox pods on
  them. Sandbox pods: pod name, the workspace it belongs to (annotation
  `warden.monaddle.com/sandbox-id` → the environment's name), spare or not,
  phase / waiting reason, node, tier (RuntimeClass), age, restarts,
  requests/limits, usage. Service pods (the four Deployments, by the
  `warden.monaddle.com/component` label in the runner's own namespace):
  component, pod, phase, ready, node, restarts, age, usage. Logs: the last
  N lines (default 200, at most 2000; 1 MiB cap), container choice,
  previous instance, timestamps; the viewer refreshes on demand or every 3 s.
- **RBAC** (chart `templates/rbac.yaml`). Runner Role in the sandbox
  namespace: `pods.metrics.k8s.io` get/list. New Role `warden-runner-view`
  in the release namespace: pods get/list, pods/log get. New ClusterRole
  (value `rbac.runnerClusterView`, default true): nodes get/list,
  `nodes.metrics.k8s.io` get/list. Missing metrics-server or a refused
  metrics read leaves the usage columns empty rather than failing the page.
- **Quantities.** `kube.ParseQuantity` handles the resource strings the
  API returns (`109m`, `1086Mi`, `2`, `1.5Gi`, `3e3`); CPU is reported in
  millicores, memory in bytes.

## Steps

1. [x] Worktree, cherry-pick of the Resources section, plan.
2. [ ] `sandbox`: `Progress`, context reporter, worker progress map, `progress` op, stages in `prepare`, `PrepareTimeout`.
3. [ ] Kubernetes driver: detail reports; SBX driver: create/boot reports.
4. [ ] `kube` client: nodes, metrics resources and types, `ParseQuantity`, `LogOptions`.
5. [ ] `sandbox/kube/cluster.go`: `PodInfo`, `ClusterStatus`, logs; runner ops.
6. [ ] Chat engine: startup map, `View`, progress polling; runner client deadline; `cluster` routes; edge `ownerOnly`.
7. [ ] Web: status line, sidebar, workspace panel pod section, admin console Cluster section and log viewer; TUI line.
8. [ ] Chart RBAC, values, goldens.
9. [ ] Docs: feature map, operator guide.
10. [ ] Live check on the Lima cluster: cold start stages, resume stages, cluster page, logs.

## Progress

- 2026-09-17: step 1 done; steps 2–10 in progress.

## Decisions

- The runner's progress store is separate from the managed registry so a
  reader never queues behind `prepare`'s lock; the cost is a second identity
  check (against the control bindings).
- Sub-stage detail flows through the context rather than a new driver
  method, so the driver interface and its fakes are unchanged.
- The cluster data comes from the runner, not the policy service: the
  runner already owns the pods, and the policy service's client is scoped
  to enforcement.
- No log streaming (`follow`): the viewer polls the tail; a streaming
  endpoint through the edge and chat would need its own connection
  management for little gain on an admin page.
- Service pod logs are readable by the owner. They can contain the edge's
  launch URL, which the owner already holds.

## Remaining

- Kata tier not exercised live in this session (the dev VM boots Kata
  bimodally); the stage reports are driver-level and tier-independent.
- The TUI shows the stage line but has no cluster view.
