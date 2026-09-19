---
name: gke-deploy
description: Build, deploy, inspect or roll back Warden on the GKE Autopilot test cluster (the public "cloud" preview deployment) with scripts/k8s-gke.sh. Use this whenever the user says deploy/redeploy/push to GKE, to the cloud cluster, to Kubernetes, to Autopilot, or to the cloud.warden host; when they ask what is running on GKE, why a GKE pod or sandbox is failing, to read GKE logs, or to park/unpark/tear down the cluster. Also use it after a merge to main when the user wants the merged tree live on the cluster.
---

# Deploy Warden to GKE

`scripts/k8s-gke.sh` owns the whole GKE lifecycle. This skill is the
operating knowledge around it that sessions otherwise re-derive from
memory files and by reading the script: where the settings live, what the
environment needs, how long steps take, and how to tell whether a deploy
worked. Read `docs/warden-kubernetes.md` ("A real cluster: GKE Autopilot
with public previews") for the design; the script's header lists every
subcommand.

## Which checkout to run from

The script needs two git-ignored files that exist only in checkouts that
have deployed before:

- `deploy/k8s/gke/env` — project, region, cluster, domain, owner, sign-in
  client ID (template: `deploy/k8s/gke/env.example`).
- `dist/gke/kubeconfig` — written by `up`, plus `dist/gke/image-rev` and
  `dist/gke/guest-image-digest` written by `build-images`.

Find them with `ls ../*/deploy/k8s/gke/env ../*/dist/gke/kubeconfig` from
any worktree. To deploy from a checkout that lacks them, copy both files
in (`env` names the owner, so never commit it; `dist/` is ignored). The
workspace `AGENTS.md` table says which worktrees currently hold them.

The tree you deploy is the checkout you run from: `build-images` tags
images with `git describe --always --dirty`, so uncommitted changes show
up as `-dirty` in the pod image tag. Deploying a merged main means
checking out or merging that commit first.

## Environment

`gcloud`, `kubectl`, `helm` and `limactl` must be on PATH, and the GKE
auth plugin lives beside gcloud without being linked:

```sh
export PATH="$PATH:/opt/homebrew/share/google-cloud-sdk/bin"
export KUBECONFIG="$(scripts/k8s-gke.sh kubeconfig)"
```

`load_env` inside the script does the same for its own subcommands; the
exports are for the ad-hoc `kubectl` and `gcloud` calls you make around
them. Run these commands with the sandbox off: they need the network, the
Lima socket and the Go toolchain, all of which the Bash sandbox breaks.

## The deploy sequence

1. **Check the cluster is up**: `scripts/k8s-gke.sh status` prints nodes,
   pods, the certificate, the ingress and the DNS delegation. A parked
   cluster (everything scaled to zero) needs `unpark` first; a deleted one
   needs `up` (~15 min, creates APIs, cluster, DNS zone, registry,
   ingress-nginx, cert-manager).
2. **Build and push images** — only when code changed since
   `dist/gke/image-rev`:
   ```sh
   scripts/k8s-gke.sh build-images > "$TMPDIR/gke-build.log" 2>&1; echo "exit $?" >> "$TMPDIR/gke-build.log"
   ```
   This needs the Lima dev VM `warden-k8s` running (`scripts/k8s-dev.sh up`)
   because the linux/amd64 images are built there under QEMU emulation. It
   takes 10–20 minutes; run it in the background with a log file and
   check `tail` of the log, not the terminal. It ends with the pushed tags
   and the guest image digest.
3. **Deploy**: `scripts/k8s-gke.sh deploy` runs `helm upgrade --install`
   with `deploy/k8s/gke/values.yaml` and the recorded image revision. Extra
   arguments pass through to helm (`--set sandboxes.warmSpares=1`).
4. **Wait for the rollout** rather than trusting helm's exit code:
   ```sh
   kubectl -n warden rollout status deploy/warden-chat --timeout=240s
   kubectl -n warden rollout status deploy/warden-runner --timeout=180s
   kubectl -n warden get pods -o wide
   ```
   Autopilot schedules new nodes for changed resource requests, so a
   rollout can sit in `Pending` for 2–4 minutes; that is not a failure.
5. **Verify**: the domain in `env` answers `200` at `/` and `401` at
   `/api/state` signed out; the owner signs in with Google and starts a
   chat. `kubectl -n warden-sandboxes get pods` shows a sandbox pod per
   live workspace.

## Reading state and logs

```sh
kubectl -n warden get pods -o wide
kubectl -n warden get events --sort-by=.lastTimestamp | grep -viE 'autopilot-default'
kubectl -n warden logs deploy/warden-policy | grep -iE 'canary|cluster|enforc|verif|refus|error|warn'
kubectl -n warden logs deploy/warden-runner --tail=100
kubectl -n warden get configmap warden-config -o jsonpath='{.data.warden\.json}' | python3 -m json.tool
kubectl -n warden-sandboxes get pods,resourcequota,limitrange
kubectl get nodes -L cloud.google.com/gke-spot,cloud.google.com/gke-nodepool
```

Pipe through `cut -c1-200` when reading in a terminal transcript; these
outputs are wide.

## Traps this cluster has already taught us

Each of these cost a session; the fix is in the tree, but the symptom
recurs when a setting is reverted:

- No DNS inside pods → the edge reports "Warden host is offline". Autopilot
  runs NodeLocal DNSCache at 169.254.20.10; the chart's
  `networkPolicy.dns.cidr` must allow it.
- cert-manager never issues → it needs
  `global.leaderElection.namespace=cert-manager` (Autopilot forbids leases
  in kube-system) and the Cloud DNS issuer needs `hostedZoneName`, else it
  backs off 30 minutes after finding the parent zone.
- `Pending` sandbox pods with a quota event → `SSD_TOTAL_GB` in the
  project's quotas (node boot disks count; it was raised 500 → 2000).
- Spot capacity is opt-in and can be absent; pods stay `Pending` with no
  node event.
- Workspace resize does nothing on GKE Sandbox (gVisor cannot resize in
  place); the sandbox restarts at the new size instead.
- `kubectl` says the auth plugin is missing → PATH lacks the SDK `bin`
  above.
- The first reply in a fresh chat is slow because the model is; check
  runner logs before suspecting the cluster.

## Cost and shutdown

The cluster costs roughly $25–45 a month while up. `park` scales
everything to zero (disks and the load balancer remain, a few dollars),
`down` deletes the cluster and its disks (zone, images and delegation
stay so `up` recreates it), `delete` removes everything. Ask before
`down` or `delete`; `park` is safe to suggest when the user says they are
done for the day.

## Afterwards

Record the deployed revision where the work is tracked: the plan document
for the feature, or the workspace `AGENTS.md` row for the worktree, so
the next session knows what is live without querying the cluster.
