# Warden on Kubernetes

Status: plan, drafted September 16, 2026, awaiting owner decisions (marked
"Owner:" below). Branch `plan/warden-kubernetes`, started from origin/main
a45aeaf (v0.1.0-alpha.8). This document records the inventory, decisions,
work, the development environment and progress.

## Objective

A Helm chart installs Warden on a Kubernetes cluster and gives the same
milestone flow as the Mac and OVH installations (chat → Warden-managed agent
sandbox → approved preview → Google Docs and GitHub through the policy
broker) with **no SBX anywhere in the deployment**. The sandbox is a pod
under a Kubernetes RuntimeClass (Kata Containers microVM or gVisor), the
gateway-only egress rule is a NetworkPolicy, and everything else is the
existing single `warden` binary with a second runtime driver.

The Mac local install (SBX) and the OVH Compose install (SBX) keep working
unchanged throughout; the Kubernetes deployment is a third shape of the same
binary, selected by configuration.

Development happens on the owner's Mac (Apple Silicon M4, 48 GiB, macOS 15.6,
nested virtualization available) in a Linux VM running a single-node cluster,
with the same scripts usable against any other cluster through `KUBECONFIG`.

## What is coupled to SBX and to a single host today

Inventory taken from a45aeaf.

- **The runtime seam exists but is incomplete.** `RuntimeDriver` in
  `chat/internal/sandbox/runtime.go` (Create, KeepAlive, Exec, Copy,
  InstallCA, Stream, Stop, Remove, Publish, Unpublish, Mappings) is the only
  runtime interface, implemented by `sbxRuntime` and faked in tests. Only the
  managed (protocol 2) path uses it. The legacy `ws-*` paths in
  `sandbox/worker.go`, `sandbox/fetch.go` and `sandbox/openai.go` shell out
  to `sbx` directly (`ls`, `create`, `exec`, `cp`, `stop`, `secret`).
- **The policy side has no runtime seam at all.** `SbxCliVerifier`
  (`chat/internal/policy/verifier.go`) is built on `CLIRunner`, a function
  that runs the pinned `sbx` executable. Host facts (`sbx version`,
  `settings get`, `mcp ls`, `policy ls`, `policy check network`), per-sandbox
  facts (`sbx ls --json`, `sbx inspect --json`: agent, image digest, kits,
  mounts, workspace) and the sandbox UUID pin in `runtime-identities.json`
  are all SBX vocabulary.
- **The gateway-only rule is an SBX per-sandbox policy.** The runner creates
  every sandbox with `--deny-network '**'`; `establishSandboxRule` adds
  `sbx policy allow network --sandbox N localhost:<gatewayPort>` and then
  removes the deny; positive and negative probes go through
  `sbx policy check network`. On failure the verifier re-applies the deny.
- **The guest reaches the gateway through `host.docker.internal`.**
  `Registry.Begin` hard-codes `http://host.docker.internal:<port>`; the
  gateway pool binds `127.0.0.1:0` inside the policy process; the health
  probe is an HMAC over a nonce on that loopback port.
- **Everything is loopback and single-host.** `chat.listen`, the edge
  listener in loopback mode, gateway ports, preview host ports and the
  runner's preview proxy ports are all `127.0.0.1` and validated as such
  (`config.go`, `chatsvc/main.go`, `gatewaypool.go`, `preview.go`,
  `ports.go` rejects any mapping whose host IP is not `127.0.0.1`). The edge
  validates a loopback chat upstream and reads `endpoint.json` from the app
  state directory. `sbx ports --publish` publishes into the runner's own
  loopback.
- **Three services share one state root and one SBX daemon.** Chat, runner
  and policy talk over `runner/worker.sock` and `policy/sbx-control.sock`
  (line JSON), all three mount `<state>/sbx` read-write, and OVH runs them
  with `network_mode: host`.
- **Runtimes are copied from host paths, unless the guest image has them.**
  The runner copies the Codex bundle and the Claude executable into a guest
  on first use; the published guest image (`deploy/guest/Dockerfile`)
  already ships both plus the gateway CA and a manifest at
  `/opt/warden/guest-manifest.json`, which makes the copy a no-op. That
  image is `FROM docker/sandbox-templates:shell-docker`, the Docker Sandboxes
  template (Docker-in-sandbox, SBX agent conventions).
- **Warm spares and residency depend on SBX auto-stop.** `KeepAlive` holds an
  `sbx exec … cat` session because SBX stops a VM after its last exec ends.
  Stop keeps the whole guest root filesystem; guest `/tmp` persists across
  stop/start; chat forks use `sbx create --clone`.
- **The installer is SBX-centric.** `warden install|doctor|uninstall`
  (`chat/cmd/warden/sbx.go`, `install.go`, `doctor.go`) create the private
  SBX namespace, log in, load templates and start the daemon.
- **Dependencies are deliberately few.** `chat/go.mod` has four direct
  requirements; the module cache on the owner's Mac has no `k8s.io/*` and no
  `golang.org/x/net` (see the toolchain note in the memory file
  `go-toolchain-sandbox`).
- **Nothing in the repository mentions Kubernetes.** The SBX integration
  plan (`docs/sbx-integration-plan.md`) considered and rejected "an external
  host/appliance restriction over all SBX outbound traffic" for cost; its
  adversarial validation matrix is the acceptance bar any replacement
  runtime must meet.

## Operating modes

The chart has to cover these axes. Each is a configuration value, not a
build variant.

| Axis | Values | Notes |
|---|---|---|
| Runtime | `sbx` (Mac, OVH), `kubernetes` (this plan) | `runtime.kind` in `warden.json`; selects the driver, the verifier and which config sections are required. |
| Isolation tier | `kata` (microVM), `gvisor` (userspace kernel), `runc` (dev only) | The RuntimeClass name the runner sets and the verifier pins. `runc` is refused unless `kubernetes.insecureRuntime: true` and the chart prints a warning; never with `auth.mode: google`. |
| Auth | `local` (owner bearer capability), `google` (edge with Google ID tokens and allowlist) | Existing modes, unchanged. |
| Previews | `loopback` (`*.localhost` through a forwarded port), `public` (wildcard suffix through an Ingress) | Existing modes; loopback now means "reached through a port-forward". |
| Egress | `restricted`, `open` | Existing `sandboxes.egress`; unchanged semantics, enforced at the gateway. |
| Topology | `single` (one core pod, one node) | The only topology in this plan; see decision 5 for the split that is deliberately deferred. |

Isolation tiers in terms of what they promise:

- **Kata.** Each sandbox is a microVM with its own kernel, the closest match
  to the SBX guarantee. Needs nodes with `/dev/kvm`: bare metal, VMs with
  nested virtualization (the Mac dev VM, the OVH server), or cloud nodes
  that expose it. Real kernel, so the agents' own inner sandboxes (Codex's
  bubblewrap/Landlock/seccomp, Claude's bubblewrap) work as on any Linux.
- **gVisor.** Each sandbox runs on a userspace kernel; the boundary is the
  sentry's syscall surface, not hardware. Works everywhere a shim can be
  installed, including managed clusters (GKE Sandbox is exactly this). No
  nested Docker; the agents' inner sandboxes may not work (spike, decision
  13). Weaker than a microVM but a real boundary and the cheapest to run.
- **runc.** A plain container. Root in the guest is root in a container on
  the node. Development only, to iterate on the driver without Kata or
  gVisor; the verifier reports the tier in every proof and the UI shows it.

## Decisions

1. **Kubernetes replaces SBX entirely in this deployment; SBX stays for the
   other two.** No sandboxd, no SBX daemon and no SBX executable in any pod.
   `runtime.kind` selects the implementation; `sbx.*` is only required and
   only validated when the kind is `sbx`. The Mac and OVH shapes are not
   touched except where a change is needed to remove a loopback assumption,
   and each such change is made with the old behaviour as the default for
   those shapes.
2. **Isolation is a RuntimeClass, pinned and proven, not assumed.** The
   runner sets `runtimeClassName` from `kubernetes.runtimeClass`; the
   verifier reads the pod back and refuses any other class, any pod it did
   not create (label + owner annotation with the binding digest), host
   networking, host PID/IPC, privileged containers, hostPath or projected
   token volumes, extra containers and extra mounts. These checks replace
   the `sbx inspect` facts (agent, kits, mounts, workspace, image digest).
   The image digest check stays: the pod's `imageID` must be the pinned
   guest digest.
3. **The gateway-only rule is a NetworkPolicy owned by the policy service.**
   The sandbox namespace carries a default-deny policy for ingress and
   egress that the chart installs and the verifier requires. The runner
   creates pods under that default (the equivalent of `--deny-network
   '**'`). When a binding is established, the policy service creates one
   NetworkPolicy selecting that pod (by the binding-digest label) that
   allows egress only to the policy pod on the binding's gateway port. No
   DNS egress: `Begin` hands out the gateway as `http://<policy pod
   IP>:<port>`, so the guest never needs to resolve a name. Publishing a
   preview adds an ingress rule from the core pod to the published port
   only. On any verification failure the policy service deletes the
   binding's NetworkPolicy, which returns the pod to default-deny (the
   equivalent of re-applying `deny '**'`).
4. **Enforcement is proven by probes, not by reading policy objects
   alone.** NetworkPolicy is only enforced when the CNI implements it, so a
   cluster fact ("NetworkPolicy is enforced here") is established at policy
   start and every `RefreshSeconds`: a canary pod in the sandbox namespace
   must fail to reach a cluster address and an external address under the
   default-deny, and a second canary with a gateway allow must reach the
   gateway. Per-sandbox proofs keep the current shape: positive to the
   binding's gateway, negative to `example.com:443`, `1.1.1.1:443`, the
   cluster DNS service, the API server service IP, the node's address and
   the metadata address, executed inside the sandbox with a tiny static
   probe helper shipped in the guest image (no `curl` dependency). The
   existing trust model (provisional trust once the gateway is up and the
   rule exists, background full verification, synchronous re-verification
   after a failure) is unchanged.
5. **Topology: one core pod, four containers.** Policy, runner, chat and
   edge run as containers of one StatefulSet pod (replicas 1) sharing an
   `emptyDir` for the two Unix sockets and `endpoint.json`, with PVCs for
   the state directories. The line-JSON socket protocol, the startup
   handshake, `endpoint.json` and the loopback edge→chat upstream all keep
   working unchanged inside the pod's network namespace. The only network
   crossings that leave the pod are guest→gateway, core→guest (exec,
   preview proxy) and ingress→edge. Splitting the runner out (the mTLS
   runner listener in `runnersvc` and `sandbox/pool.go` already exists for
   that) is the successor plan for horizontal scale; nothing here makes it
   harder. Owner: confirm the single-pod topology for the first release.
6. **Kubernetes API access through a small in-tree client, not
   client-go.** `chat/internal/kube`: in-cluster configuration (service
   account token, CA, `KUBERNETES_SERVICE_HOST`), typed JSON for the handful
   of resources used (Pod, PersistentVolumeClaim, NetworkPolicy, Namespace
   read, RuntimeClass read, ValidatingAdmissionPolicy read), list/watch,
   `pods/exec` over WebSocket (`v4.channel.k8s.io`, RFC 6455 client written
   against the standard library), `pods/log`. Rationale: the repository is
   standard-library by design, client-go would add roughly a hundred
   modules to a tree that is built from an offline cache, and the surface
   used is about ten verbs. The client gets its own tests against an
   `httptest` API server. Owner: confirm; the alternative is client-go with
   the module cache populated once outside the sandbox.
7. **Guest image split: a plain base image, and the SBX variant layered on
   top.** `deploy/guest/Dockerfile.base` builds `warden-guest-base` from
   Ubuntu 24.04 with the `agent` user, `sudo`, the pinned Codex bundle, the
   Claude executable, the gateway CA, the probe helper, and the manifest.
   `deploy/guest/Dockerfile` becomes the Docker Sandboxes template plus the
   same layers (or copies from the base image), so SBX installs are
   unchanged. Kubernetes sandboxes use the base image only; the Kubernetes
   driver refuses an image without the manifest, so the runner's copy
   fallback is never exercised there.
8. **State: PVCs for services, one PVC per sandbox for the workspace.** The
   core pod mounts PVCs at `/var/lib/warden/{policy,runner,app,provider,
   edge}` (so every existing path default is valid inside the pod). Provider
   logins are seeded from Kubernetes Secrets into the `provider` PVC by an
   init container on first start and then owned by the policy container
   (Codex refreshes its own `auth.json`). A sandbox's workspace directory
   and the agent home are a per-sandbox PVC labelled with the sandbox ID.
   "Stop" deletes the pod and keeps the PVC; "resume" creates a pod on the
   same PVC; "remove" deletes the PVC; `keepStopped` bounds retained PVCs
   as it bounds stopped sandboxes today. What persists differs from SBX
   (whole root filesystem there, the workspace volume here) and is
   documented. Chat forks use a CSI volume clone when the StorageClass
   supports `dataSource`, otherwise a tar copy through exec from the source
   pod; the driver reports which it used.
9. **Previews use pod IPs; no host ports.** `Publish` records the guest
   port and creates the ingress NetworkPolicy rule (decision 3); the
   runner's preview proxy dials `<pod IP>:<guest port>` instead of a
   published loopback port; `Mappings` reports the recorded publications.
   The availability audit, the generation check, the header stripping and
   the chat's `/api/ports/<id>/proxy` path are unchanged. Public previews
   reach the edge through an Ingress (or Gateway API HTTPRoute) with a
   wildcard host; TLS is cert-manager with a DNS-01 wildcard certificate,
   with the existing `/_tls/allow` ask endpoint kept for deployments that
   terminate TLS in Caddy. Loopback previews work as on the Mac: the edge
   port is forwarded to the operator's `127.0.0.1` (Lima does this
   automatically for the dev VM; `kubectl port-forward` otherwise), so
   `*.localhost` URLs are unchanged.
10. **The sandbox namespace is hardened by admission, and the verifier
    checks that the hardening is present.** Its own Namespace with Pod
    Security Admission `baseline` enforced (the guest needs `sudo`, so
    `restricted` does not fit; the isolation boundary is the RuntimeClass),
    a ValidatingAdmissionPolicy that requires the configured
    `runtimeClassName`, `automountServiceAccountToken: false`, the
    Warden-owned label, no host namespaces, no privileged containers, no
    hostPath, and only the allowed volume types; a ResourceQuota and a
    LimitRange sized from `sandboxes.maxRunning` and `sandboxes.memoryMB`.
    RBAC: the runner's service account may create/get/list/watch/delete
    pods and PVCs and create `pods/exec` in the sandbox namespace only; the
    policy service account may manage NetworkPolicies and read pods there.
    Neither can touch the core namespace's Secrets. The verifier's cluster
    fact includes "the namespace's default-deny policy, PSA label and
    admission policy exist and select this namespace".
11. **Configuration.** `runtime.kind` and a `kubernetes` section are added
    to `warden.json` (appendix). The chart renders `warden.json` into a
    ConfigMap; a Go test renders the chart (`helm template`) with the
    example values and loads the result through `config.Load`, in the same
    way the OVH example file is asserted today.
12. **Development environment: a Lima VM with k3s.** Lima with the `vz`
    backend and `nestedVirtualization: true` gives the VM `/dev/kvm` on this
    Mac, so both tiers are testable locally: gVisor through the `runsc`
    shim, Kata through `kata-deploy`. k3s brings containerd, a local-path
    StorageClass and a NetworkPolicy controller. Cilium is adopted only if
    the k3s controller proves insufficient for the proofs in decision 4.
    Images are built inside the VM and imported into k3s's containerd, so
    the loop needs no registry and no GitHub Actions minutes. If Kata under
    nested virtualization turns out to be unreliable on Apple Silicon, the
    Kata tier is verified on the OVH server (which has KVM) and the Mac loop
    runs gVisor; the plan does not depend on Kata working in the dev VM.
13. **The agents' inner sandboxes under gVisor are a spike, not an
    assumption.** Codex's Linux sandbox uses bubblewrap, seccomp and
    Landlock; Claude Code's uses bubblewrap. gVisor implements user
    namespaces but may not implement Landlock. Step 0 runs both agents in a
    gVisor pod and records what works. Per tier the runner then passes the
    agent's sandbox flags accordingly (full inner sandbox on Kata; on gVisor
    whatever the spike shows), and the tier documentation says which
    layers are in force.
14. **No Docker inside Kubernetes guests in this plan.** The
    `shell-docker` template's Docker is an SBX feature. Kata could run a
    privileged-in-VM Docker later; recorded as out of scope.
15. **Same binary, same release process.** The chart is published as an OCI
    artefact (`oci://ghcr.io/monaddle-too/charts/warden`) by
    `release.yml`, and by `scripts/release.sh --chart` when Actions minutes
    are unavailable; the chart's `appVersion` is the release tag and the
    image is `ghcr.io/monaddle-too/warden:<tag>` built by the existing
    Dockerfile. The chart, the base guest image and the binary are versioned
    together.

## Work

### 1. Finish the runtime seam

Files: `chat/internal/sandbox/{runtime.go,worker.go,fetch.go,openai.go,
managed.go,preview.go,ports.go}`, `chat/internal/policy/{verifier.go,
registry.go,gatewaypool.go}`, `chat/internal/config`, the two service
`config.go` files.

- Retire or wrap the legacy `ws-*` code paths that call `sbx` directly so
  every sandbox operation in the runner goes through `RuntimeDriver`. If a
  legacy path is dead in protocol 2, delete it with its tests; if it is
  live, move the SBX calls into `sbxRuntime`.
- Extend `RuntimeDriver` with what Kubernetes needs and SBX can answer
  trivially: `Address(ctx, name) (string, error)` (where the core reaches
  the sandbox: `127.0.0.1` for SBX, the pod IP for Kubernetes), and make
  `Publish`/`Mappings` speak in terms of `(address, port)` rather than a
  loopback host port. `KeepAlive` stays in the interface; the Kubernetes
  driver returns a no-op closer.
- Introduce the policy-side seam. Replace `CLIRunner` as the verifier's
  dependency with a `SandboxInspector` interface: `HostFacts(ctx)`,
  `Facts(ctx, identity)` (returns a runtime-neutral `RuntimeFacts`:
  identity pin, image digest, isolation tier, structural violations),
  `EstablishGatewayRule(ctx, identity, gateway)`, `Deny(ctx, identity)`,
  `Probe(ctx, identity, target) (allowed bool, kind string)`. Move the SBX
  code into `SbxInspector` (behaviour-preserving; the existing verifier
  tests keep passing with the fake CLI behind the new interface).
- Make the gateway host and bind address driver-provided: `GatewayPool`
  takes a bind address and `Begin` takes the advertised host from the
  inspector (`host.docker.internal` for SBX, the pod IP for Kubernetes).
- Config: `runtime.kind` (default `sbx`), `kubernetes.*` (appendix);
  validation requires `sbx.*` only for kind `sbx` and `kubernetes.*` only
  for kind `kubernetes`. Loopback validations become mode-aware: the
  Kubernetes kind allows the chat listener and gateway bind on the pod's
  addresses and the edge upstream on loopback inside the pod.
- Tests: fakes for the new interfaces; the SBX path has no behaviour change
  (race suite, vet, the compose/example equality tests).

### 2. Minimal Kubernetes client

Package `chat/internal/kube`.

- `Config` from the in-cluster environment, or from a kubeconfig path for
  tests and `warden doctor --kubeconfig`.
- REST: get/list/create/delete/patch for the resources in decision 6, with
  typed structs holding only the fields Warden reads and writes; server-side
  apply for the NetworkPolicies so re-establishing a rule is idempotent.
- Watch: chunked JSON stream with resourceVersion resume, used by the runner
  to follow pod phase and by the policy service for its canary pods.
- Exec: WebSocket client (`v4.channel.k8s.io`), channels 0–3 mapped to
  stdin/stdout/stderr/error-status, used for `Exec`, `Stream`, `Copy` (tar
  over stdin/stdout) and the probes. Deadline and cancellation semantics
  matching what `sbx exec` gave the runner today (context cancel closes the
  socket; the `Stream` reader sees EOF).
- Tests against an `httptest` server that speaks enough of the API to cover
  each verb, the watch resume and the exec channel framing.

### 3. Kubernetes runtime driver (runner)

Package `chat/internal/sandbox/kube` (or `kuberuntime`), implementing
`RuntimeDriver`.

- **Pod spec builder** from `RuntimeSpec` and config: guest image by digest,
  `runtimeClassName`, resources from `sandboxes.memoryMB` and one CPU,
  `automountServiceAccountToken: false`, the workspace PVC mounted at the
  working directory and the agent home, `securityContext` (run as `agent`,
  no privilege escalation beyond what `sudo` needs, no added capabilities,
  seccomp RuntimeDefault), labels `warden.monaddle.com/sandbox=<runtime
  name>` and `…/binding=<digest>`, and an annotation with the generation.
  The spec is a pure function with a golden test.
- **Create**: create the PVC (or clone it from the source for forks), create
  the pod, wait for `Running` through watch, record the pod UID as the
  runtime identity (the verifier pins it exactly as it pins the SBX UUID).
  Idempotent on an existing pod with the same labels (the `sbx ls --quiet`
  check today).
- **Exec/Stream/Copy/InstallCA**: over the client's exec. `InstallCA` is a
  no-op when the manifest's CA fingerprint matches (the image ships the CA)
  and otherwise installs it exactly as today.
- **Stop/Remove**: delete the pod (grace 10 s) keeping the PVC; delete the
  PVC. **Startup reconciliation**: list pods and PVCs by label instead of
  `sbx ls --json`; spares are pods labelled `…/spare=true`.
- **Warm spares** keep their mechanism; adoption is a label patch, no
  rename. Measure pod start time per tier and revisit the default of one
  spare.
- **Previews**: `Publish` records the mapping and asks the policy service
  (through the existing control socket, a new `publish` operation) to add
  the ingress rule; the preview proxy dials the pod IP. `Mappings` returns
  the recorded mappings after checking the pod is still the same UID.
- Tests: driver against the fake API server; `managed_test.go` runs its
  scenarios against both drivers.

### 4. Kubernetes enforcement (policy)

Package `chat/internal/policy/kube`, implementing `SandboxInspector`.

- **HostFacts** (cluster facts): the sandbox Namespace exists with the PSA
  label, the default-deny NetworkPolicy and the ValidatingAdmissionPolicy
  binding; the RuntimeClass exists with the expected handler; the canary
  proof of decision 4 (two short-lived pods, run at start and on refresh,
  cached like the host proof today).
- **Facts** for a binding: the pod by label, structural checks (decision
  2), `imageID` digest, UID pin, and the NetworkPolicy set that selects it
  (must be exactly the binding's policy, or none).
- **EstablishGatewayRule**: apply the binding NetworkPolicy (egress to the
  policy pod on the port; ingress from the core pod for published ports);
  read back; require exactly that policy. **Deny**: delete it.
- **Probe**: exec the probe helper inside the pod against the target; the
  helper reports connect success, refusal or timeout, and the verifier
  classifies timeouts under default-deny as the implicit denial (the
  `deny_kind == implicit` check today).
- Gateways bind on the pod IP (all interfaces inside the core pod would
  also expose them to the cluster; bind the pod IP and rely on the sandbox
  namespace's default-deny plus a core-namespace ingress policy that admits
  only sandbox pods to the gateway port range). `Begin` advertises
  `http://<pod IP>:<port>`.
- Tests: inspector against the fake API server; the verifier's existing
  scenario tests (provisional trust, background failure, refresh) run
  against both inspectors.

### 5. Guest images

- `deploy/guest/Dockerfile.base`: Ubuntu 24.04, `agent` user with `sudo`,
  the pinned Codex bundle and Claude executable (same versions, SHAs and
  paths as today), the gateway CA, the static probe helper (Go, built in
  the same workflow), the manifest. Multi-architecture.
- `deploy/guest/Dockerfile`: the Docker Sandboxes template plus the same
  layers, for SBX. A test asserts both Dockerfiles pin the same versions.
- `.github/workflows/guest-image.yml` publishes both;
  `scripts/build-guest-image-in-sbx.sh` learns the base variant; a script
  builds the base image inside the dev VM and imports it into k3s.
- The manifest gains `tier` hints if the spike (decision 13) needs
  different agent flags per tier.

### 6. Networking, ingress and the edge

- Remove the loopback-only assumptions where the Kubernetes kind needs it
  (work item 1), with the SBX kinds keeping loopback defaults.
- Edge in the core pod: upstream `http://127.0.0.1:<chat.listen>` inside
  the pod, `endpoint.json` on the shared `emptyDir`; the edge container is
  the only one with a Service in front of it. Owner sign-in, the allowlist
  and the admin console are unchanged.
- Chart ingress: an Ingress (default) or an HTTPRoute (optional) for the
  app host and the wildcard preview host; TLS through cert-manager
  (`Certificate` with a DNS-01 issuer reference from values) or through an
  external terminator using `/_tls/allow`.
- Loopback previews: a NodePort or port-forward to the edge; the
  `previews.hostSuffix` stays `localhost`.

### 7. Helm chart

`deploy/helm/warden/`.

- `values.yaml` sections: `image` (repository, tag, digest), `guestImage`
  (repository, digest), `runtime` (`tier: kata|gvisor|runc`,
  `runtimeClassName` override, `insecureRuntime`), `sandboxes` (mirrors
  `warden.json`), `auth` (`mode`, Google client, owner allowlist),
  `previews` (`mode`, `hostSuffix`, ingress and TLS), `egress`, `storage`
  (class, sizes for each PVC and for workspaces), `secrets` (names of
  existing Secrets for Codex, Claude, GitHub; the chart never contains a
  credential), `resources`, `nodeSelector`/`tolerations` (Kata nodes are
  usually a pool).
- Templates: two Namespaces (core, sandboxes) or one namespace plus a
  sandbox namespace value; ServiceAccounts, Roles, RoleBindings; the
  StatefulSet with four containers and the init container; PVCs; the
  ConfigMap with `warden.json` and `policy.template.json`; the edge Service
  and Ingress; the sandbox namespace's default-deny NetworkPolicy, PSA
  labels, ValidatingAdmissionPolicy and binding, ResourceQuota, LimitRange;
  the core-namespace ingress policy for the gateway port range; optional
  RuntimeClass objects (off by default; clusters usually own them);
  `NOTES.txt` with the port-forward and first-login instructions.
- Tests: `helm lint`; `helm template` golden files for the three tiers and
  the two auth modes under `deploy/helm/warden/testdata`; the Go test of
  decision 11 loading the rendered `warden.json`; a `kubeconform` run in
  the workflow when minutes allow.

### 8. Development environment and end-to-end tests

- `deploy/k8s/dev/lima.yaml`: Ubuntu 24.04, `vmType: vz`,
  `nestedVirtualization: true`, 8 CPUs, 16 GiB, 100 GiB disk, the repository
  mounted, provisioning that installs k3s (Traefik disabled), the gVisor
  shim and RuntimeClass, `kata-deploy` and its RuntimeClasses, and
  `nerdctl` for image builds. `scripts/k8s-dev.sh` wraps `limactl` (`up`,
  `kubeconfig`, `build-images`, `deploy`, `down`) and prints the
  `KUBECONFIG` export. Appendix B has the recipe.
- `tests/k8s/`: an end-to-end suite (Go, `-tags k8s`, needs `KUBECONFIG`)
  that installs the chart with dev values, waits for readiness, creates a
  chat through the API, runs a Codex and a Claude turn, publishes a
  preview, and then executes the networking rows of the adversarial matrix
  in `docs/sbx-integration-plan.md` against a sandbox pod: direct egress,
  unset proxy variables, IP literal, IPv6, DNS on 53/DoH, `CONNECT` to
  non-HTTP, dropping the CA, another gateway port, the API server, the
  metadata address, another sandbox's preview port, and restart of the
  policy container with live bindings. Each row records the tier it ran
  under; the suite is run for gVisor on the Mac and for Kata where KVM is
  available.

### 9. Documentation and release

- `docs/warden-kubernetes.md`: install with the chart, values reference,
  tiers and what each guarantees, the support matrix (cluster version,
  CNI with NetworkPolicy, RuntimeClass availability, StorageClass with
  clone support), operations (upgrades, CA rotation, provider logins,
  backups of the PVCs), and the threat-model delta against SBX (workspace
  persistence, gVisor boundary, cluster-admin trust, in-cluster lateral
  reach).
- `docs/architecture.md` and the README gain the third shape.
- `release.yml` and `scripts/release.sh` publish the chart; the guest
  image workflow publishes the base image.

## Steps and parallel tracks

- [ ] 0 Spike, in the dev VM, before any driver code: bring up Lima + k3s
      with gVisor and Kata RuntimeClasses; hand-write a sandbox pod from the
      current guest image (it runs as-is under a RuntimeClass, minus the
      Docker daemon), a default-deny NetworkPolicy and a gateway allow rule
      to a stand-in proxy pod; prove with probes that the k3s NetworkPolicy
      controller enforces both directions under gVisor and Kata; run Codex
      and Claude Code inside the gVisor pod and record what the inner
      sandboxes do (decision 13); measure pod start times per tier; note
      whether Kata boots under nested virtualization on this Mac. Output:
      a "Spike results" section here with the pinned versions and the
      decisions this changes.
- [ ] 1 Runtime seam and configuration (work item 1). SBX behaviour
      unchanged; race suite, vet, frontend build green.
- [ ] 2 Minimal Kubernetes client (work item 2), no dependency on step 1.
- [ ] 3 Kubernetes driver and inspector (work items 3 and 4), after 1 and 2.
- [ ] 4 Guest base image (work item 5), from day one.
- [ ] 5 Chart, networking, edge (work items 6 and 7); templates and the
      static hardening objects can start after step 0, the StatefulSet
      waits for step 3.
- [ ] 6 First milestone on the Mac: chat → gVisor pod → loopback preview
      through the Lima port forward, Codex and Claude, Google Docs and
      GitHub through the gateway.
- [ ] 7 End-to-end suite and the adversarial networking rows on gVisor
      (work item 8); then the same on Kata (dev VM if step 0 says it
      works, otherwise on the OVH server).
- [ ] 8 Public-preview mode with an Ingress and cert-manager on a real
      cluster (Owner: which cluster; the OVH server with k3s alongside the
      Compose install is the cheapest, a managed cluster with gVisor nodes
      the most representative).
- [ ] 9 Documentation and chart release (work item 9).

Tracks after step 0. Track A: steps 1 then 3, the critical path. Track B:
step 2, independent. Track C: step 4, independent. Track D: step 5's static
parts. Steps 6 to 9 in order. Step 6 is the first user-visible milestone.

## Out of scope

Splitting the runner into its own Deployment and multi-node scheduling of
sandboxes (successor plan; the mTLS listener is the hook); Docker inside
guests; Windows containers; clusters without NetworkPolicy enforcement
(refused by the verifier, not worked around); a `warden install` path for
Kubernetes (Helm is the installer; `warden doctor --kubeconfig` may run the
cluster facts from outside); migrating an SBX installation's sandboxes into
a cluster; Panta; provider breadth beyond what the other shapes have.

## Appendix A: `warden.json` additions

```json
{
  "version": 1,
  "runtime": { "kind": "kubernetes" },
  "kubernetes": {
    "namespace": "warden-sandboxes",
    "runtimeClass": "gvisor",
    "tier": "gvisor",
    "insecureRuntime": false,
    "guestImage": "ghcr.io/monaddle-too/warden-guest-base",
    "guestImageDigest": "sha256:…",
    "storageClass": "",
    "workspaceSizeGi": 20,
    "gatewayPortRange": "30000-30999",
    "probeTimeoutSeconds": 5
  }
}
```

| Field | Default | Consumers |
|---|---|---|
| `runtime.kind` | `sbx` | all services: driver, inspector, validation of `sbx.*` vs `kubernetes.*`, loopback rules |
| `kubernetes.namespace` | required | runner (pods, PVCs), policy (NetworkPolicies, facts) |
| `kubernetes.runtimeClass` | required | runner (pod spec), policy (pinned in facts) |
| `kubernetes.tier` | derived from the RuntimeClass handler when omitted | policy proofs, UI, docs |
| `kubernetes.insecureRuntime` | `false` | required `true` for `runc`; refused with `auth.mode: google` |
| `kubernetes.guestImage` / `guestImageDigest` | required | runner (pod image), policy (`imageID` check); replaces `sbx.guestImage*` for this kind |
| `kubernetes.storageClass` | cluster default | runner (workspace PVCs) |
| `kubernetes.workspaceSizeGi` | 20 | runner |
| `kubernetes.gatewayPortRange` | `30000-30999` | policy (gateway bind ports; matches the core-namespace ingress policy the chart installs) |
| `kubernetes.probeTimeoutSeconds` | 5 | policy (implicit-deny classification) |

`sandboxes.*`, `previews.*`, `auth.*`, `providers.*` and `chat.*` are
unchanged. `paths.*` keep their defaults, which the chart mounts at the same
container paths as the Compose file does.

## Appendix B: development environment recipe

To be pinned in step 0; this is the shape.

```bash
brew install lima kubectl helm
```

```bash
limactl create --name warden-k8s deploy/k8s/dev/lima.yaml
```

`lima.yaml` (essentials): `vmType: vz`, `nestedVirtualization: true`,
`cpus: 8`, `memory: 16GiB`, `disk: 100GiB`, `mounts` for the repository,
`provision` scripts that (1) install k3s with `--disable traefik` and the
NetworkPolicy controller left on, (2) install `runsc` and
`containerd-shim-runsc-v1` and register the `runsc` runtime in k3s's
containerd template, then a `RuntimeClass` named `gvisor`, (3) apply
`kata-deploy` with its k3s overlay and the `kata-qemu` RuntimeClass, (4)
install `nerdctl`; `portForwards` for the k3s API (6443) and the edge port
so `*.localhost` previews reach the Mac's loopback.

```bash
scripts/k8s-dev.sh kubeconfig > "$TMPDIR/warden-k8s.kubeconfig" && export KUBECONFIG="$TMPDIR/warden-k8s.kubeconfig"
```

```bash
scripts/k8s-dev.sh build-images
```

```bash
helm upgrade --install warden deploy/helm/warden -f deploy/k8s/dev/values.yaml
```

Sizing: with 16 GiB for the VM, two resident sandboxes at 1536 MiB plus one
spare fit beside the core pod and the cluster components under either tier;
Kata adds the guest kernel's memory per sandbox. The VM is disposable
(`limactl delete warden-k8s`); all state that matters lives in the
repository and in the chart values.

## Progress

- 2026-09-16: plan drafted from the a45aeaf inventory; no code yet. Open
  owner decisions: 5 (single-pod topology), 6 (in-tree client vs
  client-go), step 8 (which real cluster).
