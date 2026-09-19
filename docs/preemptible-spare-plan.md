# The warm spare as capacity: preemptible spare pods

Status: started 2026-09-19 on branch `feat/preemptible-spare` from
origin/main 7de26d6.

## Objective

On GKE Autopilot a sandbox pod that does not fit an existing node waits
~1–2 minutes for a node to boot (`docs/sandbox-zone-pin-plan.md`: the
platform's balloon pod is one-way, the Scale-Out class right-sizes nodes
to the pod). The warm spare makes a fresh chat instant but serves nothing
else: a resume, a fork copy or a second chat still boots a node. The
same probe log shows that a pod with a negative priority is preempted by
a normal one in 3 s. So the spare pod becomes that placeholder: it
carries a low PriorityClass, a sandbox pod that finds no room preempts it
and lands in its slot in seconds, and the runner replaces the spare in
the background (where the node boot is felt by nobody). One paid slot
(~$38/month at the default size) now serves whichever arrives first.

## What exists

- Spares: `sandbox/managed.go` `maintainSpares` (creates `wc-spare-<id>`
  with `RuntimeSpec.Spare`, prepares it, takes the guest report, records
  `managed.Spares[name]`), `takeSpareLocked` (adoption by the next new
  chat), and the runner's restart path (spares re-listed from the
  driver's reconcile).
- Pods: `sandbox/kube/spec.go` `PodSpec` (labels the spare with
  `LabelSpare`; no priority), `kube.PodSpec` type, `Options` from
  `config.Kubernetes` via `runnersvc/main.go`.
- Chart: `sandboxes.warmSpares`, `_helpers.tpl` renders `warden.json`
  `kubernetes.{…}`; cluster-scoped objects already made by the chart:
  ClusterRole (`rbac.yaml`), RuntimeClass (`runtimeclass.yaml`, opt-in).
  The sandbox namespace's ValidatingAdmissionPolicy
  (`sandbox-admission.yaml`) constrains runtime class, service account
  token, host namespaces and volume sources — not priority.

## Steps

1. Chart: a `PriorityClass` `<release>-spare` (value -10,
   `preemptionPolicy: Never`, `globalDefault: false`) rendered when
   `sandboxes.warmSpares > 0` and `sandboxes.spare.preemptible` (default
   true); its name into `warden.json` `kubernetes.sparePriorityClass`.
   Goldens.
2. Config/runner: `config.Kubernetes.SparePriorityClass` →
   `kube.Options.SparePriorityClass` → `PodSpec` sets
   `priorityClassName` on spare pods only. Golden `pod-spare.golden.json`.
3. Worker: a spare whose pod was preempted must be replaced, not handed
   out. Detect it (the driver's view of the pod, or a failed adoption)
   and drop it from `managed.Spares`, removing its claim, so
   `maintainSpares` makes a new one.
4. Docs: `docs/warden-kubernetes.md` (the GKE paragraph: what the spare
   now covers), `docs/feature-map.md`; this plan.
5. Verify: unit tests, chart goldens; deploy to GKE and watch a resume
   preempt the spare (Running in seconds) and the spare come back.

## Key decisions

1. The spare is the placeholder; no separate capacity pod (same price,
   worse fresh-chat latency, see the zone-pin plan's log of 2026-09-19).
2. `preemptionPolicy: Never` on the spare: it never evicts anything
   itself; a normal-priority sandbox pod (priority 0) may evict it.

## Progress log

- 2026-09-19: worktree opened, plan written.
