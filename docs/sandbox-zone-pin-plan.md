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
- 2026-09-19: steps 1–4 done. `Placement` drops `ZoneKeys` for a bound
  claim (`VolumeName` set or phase `Bound`); the fake API now binds a
  claim when its first pod is created, like a WaitForFirstConsumer class,
  and `TestPrepareRecreatesThePodAfterAStop` checks the first pod carries
  the pin and the resumed one does not. `SchedulerVerdict` covers the
  Autopilot resume, the one-node dev cluster, PV zone affinity, taints,
  cordons, an empty cluster, unbound claims; unknown tallies verbatim.
  `go vet`, `go test ./...` green (one `chats` flake on the first run,
  clean on the rerun). `helm template` with the GKE values renders the
  zone into `warden.json`.
- 2026-09-19: merged to main e7233aa (fast-forward).
- 2026-09-19: deployed to GKE with `feat/startup-detail-events` (main
  daf0bce, image `v0.1.0-alpha.13-82-gdaf0bce`). The zone renders into
  `warden.json`; the first pod created after the deploy (the warm spare)
  carried `topology.kubernetes.io/zone: us-central1-c`, refused the
  zone-b gVisor node ("not for sandboxes"), triggered a zone-c scale-up
  and ran there 118 s later with its disk in us-central1-c. The two
  pre-pin workspace disks (zones a and c) are untouched and resume as
  before through `Placement`. Step 5 done.
- 2026-09-19, probed whether the pin also ends "a node per pod": no. A
  1-CPU gVisor probe in zone c landed on the spare's 5-minute-old node
  at once (balloon still 0); the balloon then grew to 3990m / 16.5 GB
  (~1.3 CPU headroom left); a 2-CPU probe got `Insufficient cpu` on it
  and `TriggeredScaleUp` two seconds later, Running on a new zone-c node
  at 90 s, the old node's balloon unchanged. The balloon is one-way, so
  the pin's win is the zone (spare, disks and nodes together) and the
  headroom stage; an arrival after a node has settled still boots one.
  Docs and the values comment corrected (they claimed GKE regrows the
  node).
- 2026-09-19, the `Scale-Out` compute class tried (owner's call, on the
  expectation of conventional bin-packing nodes): two 1-CPU gVisor probes
  with `cloud.google.com/compute-class: Scale-Out` in zone c were admitted
  (GKE Sandbox works there) but Autopilot's node auto-provisioner sized
  the pool to the pending pod — `t2d-standard-2`, 1930m allocatable — so
  the second probe got `Insufficient cpu` on the first's node and its own
  node ~65 s later. Same node-per-pod outcome at +26% per pod; not
  applied. Left, if the boot ever matters: a custom ComputeClass with a
  fixed larger machine type (`n2d-standard-8`), which needs its own gVisor
  admission test; otherwise the warm spare is the answer for fresh chats
  and resumes keep the ~1–2 min cold start.
- 2026-09-19, capacity provisioning probed (Google's documented Autopilot
  pattern): a gVisor pause pod shaped like a sandbox (1 CPU / 1.5 GiB,
  zone c) under a PriorityClass of value -10 / `preemptionPolicy: Never`
  scheduled onto the spare's node in 5 s (GKE shrank the balloon for
  it), and 100 s later a normal-priority sandbox-shaped pod preempted it
  and was Running on that node in **3 s** — no node boot. This is the
  fix if the cold start ever matters: a chart-level
  `sandboxes.capacitySpares: N` (PriorityClass + a Deployment of
  placeholder pods with the sandbox's runtime class, size, node
  selector and tolerations, in a sibling namespace so the sandbox
  namespace's admission and quota are untouched); each slot bills like a
  spare (~$38/mo at 1 CPU / 1.5 GiB) and, unlike the warm spare, serves
  resumes and a second chat. Not built; owner's call.
