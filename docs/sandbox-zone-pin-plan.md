# Sandbox zone pin and the scheduler's wording

Status: started 2026-09-19 on branch `feat/sandbox-zone-pin` from
origin/main bfceb60.

## Objective

Two things the owner saw resuming a workspace on GKE:

1. `Resuming the sandbox · waiting for a node: 0/5 nodes are available:
   2 Insufficient cpu, 2 Insufficient memory, 3 node(s) didn't match
   Pod's node affinity/selector · 24 s`. Accurate, but it reads like a
   capacity outage. What happened: the workspace's disk is a zonal
   persistent disk in `us-central1-c`; the gVisor node that last held it
   was scaled away after the idle stop; the two gVisor nodes still up were
   in zones `a` and `b` (and, on Autopilot's container-optimized platform,
   their slack is held by a `system-node-critical` balloon pod that
   nothing preempts, hence "Insufficient cpu"); the three `pool-1` nodes
   have no gVisor runtime. Autopilot added a zone-c node in ~45 s and the
   pod ran. Every resume after a long idle pays that node boot, because
   a regional cluster drains nodes per zone while disks stay put.
2. Nothing in the deployment keeps sandboxes, spares and disks in one
   zone, so the warm spare (its own disk, its own zone) never helps a
   resume, and a resume rarely finds a node already up in the right zone.

The change: the GKE values pin sandbox pods to one zone
(`sandboxes.nodeSelector: topology.kubernetes.io/zone`), so every new
disk lands there and a resume can use a node that is already up (GKE
regrows it in seconds instead of booting one); the runner drops the zone
key for a workspace whose claim is already bound (its disk pins the zone
on its own, and a pod that names another zone could never schedule); and
the startup detail says the scheduler's verdict in the owner's words.

## What exists

- `deploy/helm/warden/values.yaml` `sandboxes.nodeSelector` /
  `tolerations` → `_helpers.tpl` renders them into `warden.json`
  `kubernetes.nodeSelector` → `runnersvc/main.go` → `kube.Options` →
  `spec.go` `PodSpec` (`copyLabels(o.NodeSelector)`). Also the chart's
  own RuntimeClass `scheduling.nodeSelector` (off on GKE:
  `runtime.createRuntimeClasses` false).
- `deploy/k8s/gke/values.yaml`: no `sandboxes.nodeSelector`; GKE's
  RuntimeClass admission adds `sandbox.gke.io/runtime: gvisor` itself.
- `driver.go` `ensureRunning` → `ensureClaim` (the PVC, `Spec.VolumeName`
  set once bound; `volumeBindingMode: WaitForFirstConsumer` on GKE's
  `standard-rwo`) → `ensurePod` → `awaitRunning` → `StartupDetail(pod)`:
  the `PodScheduled` condition's first sentence verbatim after
  "waiting for a node: ".
- `feat/startup-detail-events` (unmerged, `.local/warden-startup-events`)
  reads Kubernetes events and appends the latest hint ("a node is being
  added") after this detail; it keeps the scheduler text as is, so its
  `startupDetailWithEvents` test will need this wording when it merges.

## Steps

1. `deploy/k8s/gke/values.yaml`: `sandboxes.nodeSelector` with the zone,
   with the reasoning in a comment.
2. `spec.go`: `PodSpec` takes whether the claim is bound; a bound claim
   drops the zone keys (`topology.kubernetes.io/zone`,
   `topology.gke.io/zone`, `failure-domain.beta.kubernetes.io/zone`) from
   the selector. `ensurePod` passes `claim.Spec.VolumeName != ""`.
   Unit test + goldens.
3. `driver.go` `StartupDetail`: parse the scheduler's `0/N nodes are
   available: …` into "none of the N nodes can take the sandbox (2 out of
   CPU or memory, 3 not sandbox nodes)"; unknown reasons pass through.
   Tests.
4. `docs/warden-kubernetes.md` (startup stages paragraph, GKE section),
   `docs/feature-map.md` if a row changes; this plan.
5. Verify (Go, web, chart goldens), merge to main, deploy to GKE, watch a
   resume.

## Key decisions

1. The pin is a values-file choice for the GKE recipe, not a chart or
   runner default: a single-zone cluster or a Kata pool has no such
   problem, and the owner picks the zone.
2. A bound claim wins over the pin (step 2) rather than migrating the
   two existing disks: disks in other zones keep resuming exactly as
   today; only new ones benefit. No disk is moved or deleted.
3. The zone is `us-central1-c`: it is where the most recently used
   workspace's disk already is, and the zone Autopilot chose for the last
   two nodes.

## Progress log

- 2026-09-19: worktree opened, plan written.
