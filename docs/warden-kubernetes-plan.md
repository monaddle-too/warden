# Warden on Kubernetes

Status: plan, drafted September 16, 2026 and revised the same day after a
review for single-host assumptions carried into the design. Awaiting owner
decisions (marked "Owner:" below). Branch `plan/warden-kubernetes`, started
from origin/main a45aeaf (v0.1.0-alpha.8). This document records the
inventory, decisions, work, the development environment and progress.

## Objective

A Helm chart installs Warden on a Kubernetes cluster and gives the same
milestone flow as the Mac and OVH installations (chat → Warden-managed agent
sandbox → approved preview → Google Docs and GitHub through the policy
broker) with **no SBX anywhere in the deployment**. The sandbox is a pod
under a Kubernetes RuntimeClass (Kata Containers microVM or gVisor), the
gateway-only egress rule is a NetworkPolicy, services are separate
workloads that authenticate to each other, and everything else is the
existing single `warden` binary with a second runtime driver.

The Mac local install (SBX) and the OVH Compose install (SBX) keep working
unchanged throughout; the Kubernetes deployment is a third shape of the same
binary, selected by configuration.

Development happens on the owner's Mac (Apple Silicon M4, 48 GiB, macOS 15.6,
nested virtualization available) in a Linux VM running a single-node cluster,
with the same scripts usable against any other cluster through `KUBECONFIG`.

## Design rule

Every place where the current code relies on being one process tree on one
host is replaced by the Kubernetes primitive for the same need, not carried
over. The specific rule applied in the review: if something exists because
services shared a filesystem or a loopback interface (Unix sockets as
authentication, a token on disk, a port as an identity, exec as a file
delivery mechanism), it is not an invariant, and the plan must say what
replaces it. What is kept is kept for a reason stated next to it.

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
  that runs the pinned `sbx` executable. Host facts, per-sandbox facts
  (`sbx inspect --json`: agent, image digest, kits, mounts, workspace) and
  the sandbox UUID pin in `runtime-identities.json` are SBX vocabulary.
- **A loopback port is the sandbox's identity at the gateway.** The gateway
  pool opens one MITM listener per binding on `127.0.0.1:0`, persists the
  port in `gateway-port.json`, and the guest is told
  `http://host.docker.internal:<port>` by `Registry.Begin`. The gateway-only
  rule is an SBX per-sandbox policy allowing exactly `localhost:<port>`
  after a `deny '**'`; on failure the verifier re-applies the deny. The
  health probe is an HMAC over a nonce on that port. Nothing the guest
  presents proves which binding it is; reaching the port is the proof.
- **Authentication between services is the filesystem.** Chat, runner and
  policy talk line JSON over `runner/worker.sock` (0660) and
  `policy/sbx-control.sock` (0600); the edge reads the owner bearer
  capability from `app/endpoint.json` and validates a loopback chat
  upstream. All three services mount `<state>/sbx` read-write, OVH runs
  them with `network_mode: host`, and the edge is a separate systemd unit
  with `ProtectSystem=strict`, which is the only service boundary in the
  deployment. The runner already has an unused mutual-TLS TCP listener and
  client (`runnersvc/main.go`, `sandbox/pool.go`).
- **Files are delivered to guests by exec.** The runner copies the Codex
  bundle and the Claude executable on first use and installs the gateway CA
  with `sbx cp` plus `update-ca-certificates` as root. The published guest
  image (`deploy/guest/Dockerfile`) already ships both runtimes at
  `/tmp/warden-runtime` and `/tmp/warden-claude`, the CA, and a manifest at
  `/opt/warden/guest-manifest.json`, which makes the copy a no-op. That
  image is `FROM docker/sandbox-templates:shell-docker`, the Docker
  Sandboxes template.
- **Provider logins are files the policy process rewrites.**
  `providers.codex.authFile` is refreshed by Codex; the Claude and GitHub
  files are written by operators and by `warden login`.
- **Warm spares and residency depend on SBX auto-stop.** `KeepAlive` holds
  an `sbx exec … cat` session because SBX stops a VM after its last exec
  ends. Stop keeps the whole guest root filesystem; guest `/tmp` persists
  across stop/start; chat forks use `sbx create --clone`. The runtime
  identity pin assumes a sandbox keeps its UUID across stop and start.
- **Previews publish host ports.** `sbx ports --publish 127.0.0.1:…` puts
  the guest port on the runner's loopback; the runner's availability proxy
  dials it; `parsePortMappings` rejects any host IP but `127.0.0.1`.
- **The installer is SBX-centric.** `warden install|doctor|uninstall` create
  the private SBX namespace, log in, load templates and start the daemon.
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
| Runtime | `sbx` (Mac, OVH), `kubernetes` (this plan) | `runtime.kind` in `warden.json`; selects the driver, the inspector, the credential store and which config sections are required. |
| Isolation tier | `kata` (microVM), `gvisor` (userspace kernel) | Explicit `kubernetes.tier`; the verifier checks the RuntimeClass handler against the tier's allowlist. There is no plain-container tier: the gVisor shim installs in one step in the dev VM, so a `runc` mode would buy nothing and would leak into support. |
| Auth | `owner` (owner sign-in without a Google client), `google` (edge with Google ID tokens and allowlist) | Existing modes, unchanged in meaning (the code and the chart call the first one `owner`). |
| Previews | `loopback` (`*.localhost` through a forwarded port), `public` (wildcard suffix through an Ingress) | Existing modes; loopback now means "reached through a port-forward". |
| Egress | `restricted`, `open` | Existing `sandboxes.egress`; unchanged semantics, enforced at the gateway. |
| Transport | `unix` (sbx shapes), `tls` (Kubernetes) | Address scheme per service; same protocol either way (decision 5). |

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
  14). Weaker than a microVM but a real boundary and the cheapest to run.

## Decisions

1. **Kubernetes replaces SBX entirely in this deployment; SBX stays for the
   other two.** No sandboxd, no SBX daemon and no SBX executable in any pod.
   `runtime.kind` selects the implementation; `sbx.*` is only required and
   only validated when the kind is `sbx`. The Mac and OVH shapes keep their
   behaviour; where a shared component changes (transport, credential
   store, gateway addressing), the sbx shapes get the file and loopback
   implementations of the new interface with today's values as defaults.

2. **Isolation is a RuntimeClass, pinned and proven, not assumed.** The
   runner sets `runtimeClassName` from `kubernetes.runtimeClass`; the
   verifier reads the pod back and refuses any other class, a handler
   outside the tier's allowlist, any pod it did not create (Warden labels
   and an owner annotation with the binding digest), host networking, host
   PID/IPC, privileged containers, hostPath or projected token volumes,
   extra containers and extra mounts. These checks replace the
   `sbx inspect` facts. The image check stays: the pod's `imageID` must be
   the pinned guest digest.

3. **Runtime identity is the workspace volume, and the pod is a generation
   of it.** The stable identity of a sandbox is its workspace PVC UID,
   pinned once as the SBX UUID is today. Each pod created on that PVC is
   pinned per generation (pod UID recorded with the generation number). A
   different PVC UID for the same sandbox is fatal, as a changed UUID is
   today; a new pod UID on resume is expected and starts a new generation
   proof. A literal port of "a changed UUID is fatal" would refuse every
   resume, since pods are not preserved across stop.

4. **One gateway, identity by credential, egress by label.** The policy
   service runs one gateway listener behind a ClusterIP Service
   (`warden-gateway`), not one port per binding. A binding is identified by
   the standard proxy mechanism: `Begin` returns
   `http://<bindingID>:<capability>@<service IP>:<port>` and the gateway
   authenticates `Proxy-Authorization` against the binding's capability
   (the same value the health HMAC already uses), dispatching to that
   binding's engine, decisions and provider secret. The source pod, resolved
   from a watch on the sandbox namespace, is a second check; it is
   defence in depth because IP anti-spoofing is a CNI property, while the
   credential is the authority. The guest receives an IP, so no DNS egress
   is needed and the address survives policy restarts. `gateway-port.json`
   and the per-binding listeners go away in this kind; the SBX kind keeps
   its per-binding loopback listeners behind the same gateway interface.
   Step 0 confirms that Codex and Claude Code honour credentials in the
   proxy URL; if one does not, that client's runs use a per-binding header
   set by the runner's launch environment instead, never port identity.

   Egress control is one static NetworkPolicy in the sandbox namespace: it
   selects pods labelled `warden.monaddle.com/egress=gateway` and allows
   egress only to the policy pod on the gateway port. The namespace's
   default-deny policy covers everything else. The policy service grants
   egress by setting that label when the binding is established and denies
   by removing it (the equivalents of today's `allow localhost:<port>` and
   `deny '**'`). No per-binding policy objects are created or deleted, and
   the verifier's per-sandbox fact is "the label is present and only the
   two static policies select this pod".

5. **Four workloads, one protocol, mutual TLS.** Policy, runner, chat and
   edge are separate Deployments (`strategy: Recreate`, one replica each,
   because their state PVCs are ReadWriteOnce), each with its own PVC,
   ServiceAccount and NetworkPolicy. The line-JSON protocol and the startup
   handshake are unchanged; only the transport is abstracted: every
   listener and every client address is a URL, `unix://<path>` for the sbx
   shapes and `tls://<host>:<port>` for Kubernetes, with mutual TLS from a
   chart-provisioned CA (cert-manager `Certificate`s where the cluster has
   it, otherwise a bootstrap Job writing a Secret). The runner's existing
   mTLS listener and `pool.go` client are the template for chat and policy.
   Service identity is the client certificate: the edge's certificate is
   its authority to forward principal headers to chat, so `endpoint.json`
   and the owner bearer are not used in this kind (they remain the sbx
   shapes' mechanism). The internet-facing edge therefore shares no
   namespace, volume or loopback with the container that holds provider
   credentials and the gateway CA key, which preserves the one service
   boundary OVH has today rather than weakening it. NetworkPolicies between
   services admit exactly chat→runner, chat→policy, runner→policy,
   edge→chat, sandbox→policy on the gateway port, and runner→sandbox for
   the preview proxy; exec goes through the API server, not the pod
   network.

   The transport abstraction lands first, with the sbx shapes still on Unix
   sockets and no behaviour change, so the runtime change and the topology
   change are not debugged together. Owner: confirm four Deployments and
   mTLS from the start. Proceeding with this under the execution loop
   started 2026-09-16; the owner may override before step 6.

6. **Kubernetes API access through a small in-tree client, not
   client-go.** `chat/internal/kube`: in-cluster configuration, typed JSON
   for the resources used (Pod, PersistentVolumeClaim, Secret, ConfigMap,
   NetworkPolicy, Namespace, RuntimeClass, ValidatingAdmissionPolicy and
   its binding), list/watch, `pods/exec` over WebSocket
   (`v4.channel.k8s.io`, RFC 6455 client on the standard library),
   `pods/log`, server-side apply for objects Warden owns. Rationale: the
   repository is standard-library by design, client-go would add roughly a
   hundred modules to a tree built from an offline cache, and the surface
   used is small. The client gets its own tests against an `httptest` API
   server. Owner: confirm; the alternative is client-go with the module
   cache populated once outside the sandbox. Proceeding with the in-tree
   client under the execution loop started 2026-09-16.

7. **Guest image split: a plain base image, and the SBX variant layered on
   top.** `deploy/guest/Dockerfile.base` builds `warden-guest-base` from
   Ubuntu 24.04 with the `agent` user, `sudo`, the pinned Codex bundle and
   Claude executable under `/opt/warden/runtime` and `/opt/warden/claude`,
   the system trust bundle symlinked to `/opt/warden/trust/ca-certificates.crt`
   (decision 9), and the manifest, which now names the runtime paths so the
   runner stops assuming `/tmp`. `deploy/guest/Dockerfile` becomes the
   Docker Sandboxes template plus the same layers, keeping the `/tmp` paths
   the SBX runner expects until the manifest-driven paths land there too.
   Kubernetes sandboxes use the base image only; the Kubernetes driver
   refuses an image without a manifest, so the runner's copy fallback is
   never exercised there.

8. **State: a PVC per service, a PVC per sandbox, Secrets for
   credentials.** Each Deployment mounts its own PVC (`policy`, `runner`,
   `app`, `edge`); there is no `provider` volume. Provider logins are
   Kubernetes Secrets accessed through a credential store interface
   (`CredentialStore`: `Load`, `Store`, `Watch`) with a file implementation
   for the sbx shapes and a Secret implementation here, so Codex's token
   refresh writes back through the API and a rotated Secret is picked up
   without a restart. Only the policy ServiceAccount may read or write
   those Secrets. A sandbox's persisted set is exactly the agent home
   (`/home/agent`, which contains the working directory) on a per-sandbox
   PVC labelled with the sandbox ID; everything else is image state. This
   is narrower than SBX, where the whole root filesystem persists, and it
   is a decision, not a footnote: the agent's system prompt states that
   package installs outside the home directory do not survive a stop, and
   the docs say the same. "Stop" deletes the pod and keeps the PVC; "resume"
   creates a pod on the same PVC (new generation, decision 3); "remove"
   deletes the PVC; `keepStopped` bounds retained PVCs. Chat forks use a
   CSI volume clone when the StorageClass supports `dataSource`, otherwise
   a tar copy through exec from the source pod; the driver reports which
   it used.

9. **Trust is a mounted bundle, never an exec.** The policy service
   publishes the guest trust bundle (the base image's system CAs plus the
   current gateway CA) as a ConfigMap; the runner mounts it as a directory
   at `/opt/warden/trust`, which the base image's system bundle symlink
   points into. Kubelet refreshes directory-mounted ConfigMaps in place, so
   a CA rotation reaches running guests without root, exec or restart;
   step 0 confirms the refresh under both tiers. `InstallCA` is not
   implemented by the Kubernetes driver; the interface method moves to the
   SBX driver's own preparation path (work item 2). `Copy` remains a
   primitive for git bundles and the fork fallback. The ConfigMap starts
   empty (chart-created), so "the trust bundle is published" is part of
   the runner's readiness: the runner creates no sandbox pod until the
   ConfigMap holds a non-empty `ca-certificates.crt`, and the policy
   service publishes it before it serves its control listener.

10. **Previews use pod IPs; no host ports and no per-publication
    objects.** `Publish` records the guest port and generation; the
    runner's availability proxy dials `<pod IP>:<guest port>`; `Mappings`
    returns the recorded publications after checking the pod UID. One
    static NetworkPolicy admits ingress from the runner pod to sandbox pods
    on all TCP ports; the runner's proxy is the only source and dials only
    published ports, so per-port rules add nothing. The runner's proxy is
    kept because the runner is the authority on sandbox liveness,
    generation and the isolation audit, not because of host ports. Public
    previews reach the edge through an Ingress (or Gateway API HTTPRoute)
    with a wildcard host; TLS is cert-manager with a DNS-01 wildcard
    certificate, with the existing `/_tls/allow` ask endpoint kept for
    deployments that terminate TLS in Caddy. Loopback previews work as on
    the Mac: the edge port is forwarded to the operator's `127.0.0.1`
    (`kubectl port-forward` to the edge Service, or in the dev VM an edge
    Service of type LoadBalancer, which k3s's ServiceLB binds on the node
    and Lima then forwards to the Mac's loopback), so `*.localhost` URLs
    are unchanged.

11. **The sandbox namespace is hardened by admission, and the verifier
    checks that the hardening is present.** Its own Namespace with Pod
    Security Admission `baseline` enforced (root in the guest is a product
    feature and the RuntimeClass is the boundary, so `restricted` does not
    fit), a ValidatingAdmissionPolicy that requires the configured
    `runtimeClassName`, `automountServiceAccountToken: false`, the
    Warden-owned label, no host namespaces, no privileged containers, no
    hostPath, and only the allowed volume types (the workspace PVC, the
    trust ConfigMap, an emptyDir `/tmp`); a ResourceQuota and a LimitRange
    sized from `sandboxes.maxRunning` and `sandboxes.memoryMB`. RBAC: the
    runner's ServiceAccount may create/get/list/watch/delete pods and PVCs,
    create `pods/exec` and read `pods/log` in the sandbox namespace only;
    the policy ServiceAccount may get/list/watch/patch pods (labels),
    create/delete pods and read `pods/log` there (the canaries of decision
    12), read NetworkPolicies there, update/patch the trust ConfigMap
    there, and read/write the provider Secrets in the core namespace.
    The trust ConfigMap lives in the sandbox namespace because pods can
    only mount ConfigMaps of their own namespace; the chart creates it
    empty and the policy service only updates it. Neither ServiceAccount
    can touch the TLS Secrets beyond its own.

12. **Enforcement is proven by canaries and control-plane facts.** A
    cluster fact ("NetworkPolicy is enforced here") is established by two
    short-lived Warden-owned canary pods in the sandbox namespace: one
    under default-deny must fail to reach a cluster address, the API
    server, the cluster DNS service and an external address; one with the
    egress label must reach the gateway and nothing else. The fact is
    refreshed at policy start, on any watch event on the namespace's
    NetworkPolicy, Namespace, RuntimeClass or admission objects, and on a
    long interval (hourly), not every two minutes: the API can be watched,
    so polling copies a cadence SBX needed and Kubernetes does not.
    Per-sandbox verification is control-plane facts only (decisions 2, 3
    and 4): the pod spec, its labels, the policies that select it, the
    image digest and the identity pins. Nothing runs inside an untrusted
    guest on the verifier's behalf, and the guest image ships no probe
    helper. The trust model (provisional trust once the gateway is up and
    the label is set, background full verification, synchronous
    re-verification after a failure) is unchanged.

13. **Configuration.** `runtime.kind`, a `kubernetes` section, per-service
    `listen`/address URLs, a `tls` section and per-provider Secret names are
    added to `warden.json` (appendix A). The chart renders `warden.json`
    into a ConfigMap; a Go test renders the chart with the example values
    and loads the result through `config.Load`, as the OVH example file is
    asserted today.

14. **The agents' inner sandboxes under gVisor are a spike, not an
    assumption.** Codex's Linux sandbox uses bubblewrap, seccomp and
    Landlock; Claude Code's uses bubblewrap. gVisor implements user
    namespaces but may not implement Landlock. Step 0 runs both agents in a
    gVisor pod and records what works. Per tier the runner then passes the
    agent's sandbox flags accordingly (full inner sandbox on Kata; on gVisor
    whatever the spike shows), and the tier documentation says which
    layers are in force.

15. **Development environment: a Lima VM with k3s.** Lima with the `vz`
    backend and `nestedVirtualization: true` gives the VM `/dev/kvm` on this
    Mac, so both tiers are testable locally: gVisor through the `runsc`
    shim, Kata through `kata-deploy`. k3s brings containerd, a local-path
    StorageClass and a NetworkPolicy controller. Cilium is adopted only if
    the k3s controller fails the canary proofs or lacks source-IP
    anti-spoofing. Images are built inside the VM and imported into k3s's
    containerd, so the loop needs no registry and no GitHub Actions
    minutes. If Kata under nested virtualization is unreliable on Apple
    Silicon, the Kata tier is verified on the OVH server (which has KVM)
    and the Mac loop runs gVisor; the plan does not depend on Kata working
    in the dev VM.

16. **No Docker inside Kubernetes guests in this plan.** The
    `shell-docker` template's Docker is an SBX feature. Kata could run a
    privileged-in-VM Docker later; recorded as out of scope.

17. **Same binary, same release process.** The chart is published as an OCI
    artefact (`oci://ghcr.io/monaddle-too/charts/warden`) by
    `release.yml`, and by `scripts/release.sh --chart` when Actions minutes
    are unavailable; the chart's `appVersion` is the release tag and the
    image is `ghcr.io/monaddle-too/warden:<tag>` built by the existing
    Dockerfile. The chart, the base guest image and the binary are versioned
    together.

## Work

### 1. Transport abstraction and mutual TLS

Files: `chat/internal/services/{policysvc,runnersvc,chatsvc,edgesvc}`,
`chat/internal/sandbox/{client.go,enforcement.go,pool.go}`,
`chat/internal/handshake`, `chat/internal/edge`, `chat/internal/config`.

- A `transport` package: `Listen(url)` and `Dial(url)` for `unix://` and
  `tls://`, the latter with a CA, certificate and key from config and peer
  verification that records the client identity (certificate subject) on
  the connection. Line-JSON servers and clients take the abstraction; the
  runner's existing mTLS listener and `pool.go` client are folded into it.
- Chat and policy gain `tls://` listeners; the edge's upstream is a URL
  and, for `tls://`, the edge authenticates with its certificate and does
  not read `endpoint.json`. Chat accepts principal headers only from a
  peer whose certificate identity is the edge.
- Config: `services.{policy,runner,chat}.listen`, `services.*.address`
  (what clients dial), `tls.{caFile,certFile,keyFile}`. Defaults for the
  sbx shapes are today's socket paths; validation requires `tls` only when
  any address is `tls://`.
- Behaviour-preserving for the sbx shapes: the race suite, vet, the
  compose/example equality tests and a live Mac run pass before anything
  else lands.

### 2. Finish the runtime seam

Files: `chat/internal/sandbox/{runtime.go,worker.go,fetch.go,openai.go,
managed.go,preview.go,ports.go}`, `chat/internal/policy/{verifier.go,
registry.go,gatewaypool.go,ca.go}`, `chat/internal/config`.

- Retire or wrap the legacy `ws-*` code paths that call `sbx` directly so
  every sandbox operation in the runner goes through `RuntimeDriver`. If a
  legacy path is dead in protocol 2, delete it with its tests; if it is
  live, move the SBX calls into `sbxRuntime`.
- Reshape `RuntimeDriver`: remove `KeepAlive` and `InstallCA` from the
  interface (residency and CA delivery are SBX preparation details owned by
  `sbxRuntime`, which keeps the exec session and the `update-ca-certificates`
  step internally); add `Address(ctx, name)`; make `Publish`/`Mappings`
  speak `(address, port)`; read runtime paths from the guest manifest.
- Policy-side seam: replace `CLIRunner` with `SandboxInspector`
  (`ClusterFacts`, `Facts(identity)` returning runtime-neutral
  `RuntimeFacts`, `GrantEgress(identity)`, `DenyEgress(identity)`) and move
  the SBX code into `SbxInspector`, behaviour-preserving.
- Gateway seam: `Gateway` interface with `Bind(binding)` returning the
  advertised URL for `Begin`; `LoopbackGateways` is today's per-binding
  pool, `SharedGateway` is decision 4's single listener with
  `Proxy-Authorization` dispatch (usable by the sbx shapes later, but not
  switched there in this plan).
- Credential store seam (decision 8): `CredentialStore` with the file
  implementation; the policy service's provider loaders use it.
- Config: `runtime.kind` (default `sbx`), `kubernetes.*`, per-provider
  `secret` (appendix A); validation by kind.

### 3. Minimal Kubernetes client

Package `chat/internal/kube`.

- `Config` from the in-cluster environment, or from a kubeconfig path for
  tests and `warden doctor --kubeconfig`.
- REST: get/list/create/delete/patch/server-side apply for the resources
  in decision 6, with typed structs holding only the fields Warden reads
  and writes.
- Watch: chunked JSON stream with resourceVersion resume, used by the
  runner (pod phase), by the policy service (sandbox pods for source-IP
  resolution and label facts; hardening objects for the cluster fact) and
  by the Secret credential store.
- Exec: WebSocket client (`v4.channel.k8s.io`), channels 0–3 mapped to
  stdin/stdout/stderr/error-status, used for `Exec`, `Stream` and `Copy`
  (tar over stdin/stdout). Context cancellation closes the socket and the
  `Stream` reader sees EOF, matching what `sbx exec` gave the runner.
- Tests against an `httptest` server that speaks enough of the API to cover
  each verb, the watch resume and the exec channel framing.

### 4. Kubernetes runtime driver (runner)

Package `chat/internal/sandbox/kube`, implementing `RuntimeDriver`.

- **Pod spec builder** from `RuntimeSpec` and config: guest image by digest,
  `runtimeClassName`, resources from `sandboxes.memoryMB` and one CPU,
  `automountServiceAccountToken: false`, the workspace PVC at `/home/agent`,
  the trust ConfigMap at `/opt/warden/trust`, an emptyDir `/tmp`,
  `securityContext` (run as `agent`, privilege escalation allowed only for
  `sudo`, no added capabilities, seccomp RuntimeDefault), labels
  `warden.monaddle.com/sandbox=<runtime name>`, `…/binding=<digest>`,
  `…/spare` when applicable, and an annotation with the generation. The
  spec is a pure function with a golden test.
- **Create**: create the PVC (or clone it from the source for forks), record
  its UID as the runtime identity, create the pod, wait for `Running`
  through watch, record the pod UID with the generation. Idempotent on an
  existing pod with the same labels.
- **Exec/Stream/Copy** over the client's exec. No CA step.
- **Stop/Remove**: delete the pod (grace 10 s) keeping the PVC; delete the
  PVC. **Startup reconciliation**: list pods and PVCs by label.
- **Warm spares** keep their mechanism; adoption is a label patch. Measure
  pod start per tier and set the default spare count from it.
- **Previews**: record the mapping; the proxy dials the pod IP; `Mappings`
  checks the pod UID.
- Tests: driver against the fake API server; `managed_test.go` scenarios
  run against both drivers.

### 5. Kubernetes enforcement (policy)

Packages `chat/internal/policy/kube` (inspector, credential store, trust
publisher) and the `SharedGateway`.

- **ClusterFacts**: the sandbox Namespace with the PSA label, the two static
  NetworkPolicies (default-deny, gateway egress by label) and the ingress
  policy for the runner, the ValidatingAdmissionPolicy and binding, the
  RuntimeClass with a handler in the tier's allowlist, and the canary proof
  (decision 12) with its refresh rules.
- **Facts** for a binding: the pod by label, structural checks (decision
  2), `imageID`, PVC UID and pod-per-generation pins (decision 3), the
  egress label, and "only the static policies select this pod".
- **GrantEgress/DenyEgress**: patch the label; read back.
- **SharedGateway**: one listener on the pod IP behind the `warden-gateway`
  Service; `Proxy-Authorization` lookup to the binding; source-pod
  cross-check from the pod watch; health endpoint per binding on the shared
  port with the existing HMAC; `Begin` advertises the credentialed URL.
- **Trust publisher**: assemble the bundle at start and after rotation,
  server-side apply the ConfigMap.
- **Secret credential store**: `Load`/`Store`/`Watch` on the named Secrets.
- Tests: inspector, gateway and store against the fake API server; the
  verifier's scenario tests run against both inspectors.

### 6. Guest images

- `deploy/guest/Dockerfile.base`: Ubuntu 24.04, `agent` user with `sudo`,
  the pinned Codex bundle and Claude executable under `/opt/warden` (same
  versions and SHAs), the trust symlink, the manifest with runtime paths
  and the CA fingerprint the image was built with. Multi-architecture.
- `deploy/guest/Dockerfile`: the Docker Sandboxes template plus the same
  layers, for SBX. A test asserts both Dockerfiles pin the same versions.
- `.github/workflows/guest-image.yml` publishes both;
  `scripts/build-guest-image-in-sbx.sh` learns the base variant; a script
  builds the base image inside the dev VM and imports it into k3s.

### 7. Helm chart

`deploy/helm/warden/`.

- `values.yaml` sections: `image` (repository, tag, digest), `guestImage`
  (repository, digest), `runtime` (`tier: kata|gvisor`, `runtimeClassName`
  override), `sandboxes` (mirrors `warden.json`), `auth` (`mode`, Google
  client, owner allowlist), `previews` (`mode`, `hostSuffix`, ingress and
  TLS), `egress`, `storage` (class, sizes per service PVC and per
  workspace), `secrets` (names of existing Secrets for Codex, Claude,
  GitHub; the chart never contains a credential), `tls` (`certManager`
  issuer reference or bootstrap Job), `resources`,
  `nodeSelector`/`tolerations` (Kata nodes are usually a pool).
- Templates: core and sandbox Namespaces; four ServiceAccounts, Roles and
  RoleBindings; four Deployments with `Recreate`; four PVCs; the ConfigMap
  with `warden.json` and `policy.template.json`; Services for chat, runner,
  policy control, the gateway and the edge; the edge Ingress; inter-service
  NetworkPolicies (decision 5); the sandbox namespace's default-deny,
  gateway-egress-by-label and runner-ingress NetworkPolicies, PSA labels,
  ValidatingAdmissionPolicy and binding, ResourceQuota, LimitRange; TLS
  `Certificate`s or the bootstrap Job; optional RuntimeClass objects (off
  by default; clusters usually own them); `NOTES.txt` with the port-forward
  and first-login instructions.
- Tests: `helm lint`; `helm template` golden files for the two tiers and
  the two auth modes under `deploy/helm/warden/testdata`; the Go test of
  decision 13; a `kubeconform` run in the workflow when minutes allow.

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
  in `docs/sbx-integration-plan.md` against a sandbox pod from a
  test-owned exec (the test, not the verifier, runs inside the guest):
  direct egress, unset proxy variables, wrong or missing proxy credentials,
  another binding's credentials, IP literal, IPv6, DNS on 53/DoH, `CONNECT`
  to non-HTTP, dropping the trust bundle, the API server, the metadata
  address, another sandbox's preview port, a pod without the egress label,
  and restart of the policy Deployment with live bindings (address stable,
  leases re-established). Each row records the tier it ran under; the suite
  runs for gVisor on the Mac and for Kata where KVM is available.

### 9. Documentation and release

- `docs/warden-kubernetes.md`: install with the chart, values reference,
  tiers and what each guarantees, the support matrix (cluster version, CNI
  with NetworkPolicy enforcement and anti-spoofing, RuntimeClass
  availability, StorageClass with clone support), operations (upgrades, CA
  rotation, provider Secrets, TLS rotation, backups of the PVCs), the
  persisted set, and the threat-model delta against SBX (gVisor boundary,
  cluster-admin trust, in-cluster lateral reach).
- `docs/architecture.md` and the README gain the third shape.
- `release.yml` and `scripts/release.sh` publish the chart; the guest
  image workflow publishes the base image.

## Spike results (step 0, 2026-09-16/17)

Environment: Lima 2.2.0 (`vz`, `nestedVirtualization: true`, 8 CPUs, 16 GiB)
on the owner's M4 Mac; Ubuntu 24.04.4; k3s v1.36.4+k3s1 (containerd
2.3.4, flannel, the embedded network policy controller, local-path
storage); gVisor release-20260914.0 (`runsc` with the `gvisor-bin/`
sidecars, systrap platform); Kata Containers 4.2.0 via the kata-deploy Helm
chart (`k8sDistribution=k3s`, qemu shim only, `defaultShim.<arch>=qemu`);
buildkit 0.33.0 and nerdctl 2.3.5 on k3s's containerd. All of it is in
`deploy/k8s/dev/lima.yaml` and `scripts/k8s-dev.sh`; the probes are in
`deploy/k8s/dev/spike/`.

Two host-side fixes were needed and are now part of the dev loop:

- **runsc ignores SUID bits by default**, so the guest's passwordless
  `sudo` stayed uid 1000. `allow-suid = "true"` in `/etc/containerd/runsc.toml`
  restores it. The chart's support matrix must require this on gVisor nodes
  (GKE Sandbox sets it; self-managed nodes may not).
- **Kata's default `cpu_features = "pmu=off"` is refused by QEMU under
  nested virtualization** on Apple Silicon (the VM's KVM exposes no PMU).
  A drop-in in `runtimes/qemu/config.d/` clears it.

Results against the step 0 questions:

| Question | gVisor | Kata |
|---|---|---|
| NetworkPolicy enforced both ways (default deny, egress to the gateway only by label, ingress from the runner only) | all 8 egress rows and 3 ingress rows pass | same |
| Grant/deny by label on a running pod | effective within 3 s, no restart | same |
| A pod cannot use another pod's IP | blocked (the probe could not bind the address; not a proof that the CNI filters spoofed frames) | same caveat |
| `HTTPS_PROXY` credentials honoured | curl and Python send `Proxy-Authorization: Basic` preemptively; libcurl-based git sends none on `CONNECT` until the proxy answers 407 with `Proxy-Authenticate: Basic`, then retries | same (client behaviour, not tier) |
| Directory-mounted ConfigMap refreshes in a running pod | yes, ~48 s (kubelet sync) | yes, ~3 s |
| Base guest image runs as-is | yes: `agent` uid 1000, Codex 0.154.0, Claude 2.1.272, manifest, trust symlink; `sudo` after `allow-suid` | yes, including `sudo` |
| Agents' inner sandboxes | bubblewrap with user, pid, ipc and mount namespaces works; `--unshare-net` fails (netlink `RTM_NEWADDR` unimplemented), which is what `codex sandbox` uses; Landlock `ENOSYS` | `codex sandbox` works (bubblewrap + Landlock ABI 7); a hand-run `bwrap --proc` fails on the masked container `/proc`, which `procMount: Unmasked` would lift but needs `hostUsers: false` |
| Pod start, image cached, create → Ready | 0.3 s | 33 s when the guest boots normally; about half the boots on this Mac stall for ~17 min before the agent starts (seen with 0, 1 and 2 other Kata VMs running, so not load) |

Decisions this changes:

- **Decision 14 (agents under gVisor).** On the gVisor tier Codex runs
  without its inner sandbox (`sandbox_mode = "danger-full-access"`); the
  gVisor boundary, the NetworkPolicy and the gateway are the controls, and
  the tier documentation says so. Claude Code's controls are unchanged on
  both tiers (Warden's MCP permission prompt, `--permission-mode default`;
  it does not use bubblewrap in this launch). On Kata Codex keeps its full
  inner sandbox.
- **Decision 15 (dev loop).** The Mac loop runs gVisor; Kata is functional
  here but its boot time is bimodal under nested virtualization, so the
  Kata tier's end-to-end and adversarial runs move to a host with real KVM
  (the OVH server), as the decision already allowed. The dev VM keeps Kata
  installed for functional checks.
- **Decision 4 (gateway identity), refinement.** Provider traffic does not
  go through the proxy: the runner points `OPENAI_BASE_URL`-style settings
  and `ANTHROPIC_BASE_URL` straight at the gateway with a bearer placeholder
  (`WARDEN_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`). The binding credential
  therefore rides in that placeholder on provider routes and in
  `Proxy-Authorization` on proxied routes; `Begin` mints both and the
  gateway must answer unauthenticated `CONNECT` with 407 and
  `Proxy-Authenticate: Basic`, or libcurl clients never send it.
- **Decision 2 / work item 4.** The pod spec must leave
  `allowPrivilegeEscalation` unset (PSA baseline allows it) or `sudo`
  cannot work. `capabilities.drop: [ALL]` is NOT fine (found in step 4: it
  empties the bounding set and setuid `sudo` fails to change to the root
  gid); the driver drops AUDIT_WRITE, FSETID, MKNOD, NET_RAW, SETFCAP,
  SETPCAP and SYS_CHROOT and adds nothing.
- **Runtime facts.** `imageID` for a locally built image is the manifest
  digest `build-base.sh` prints, so the pin works without a registry.

## Steps and parallel tracks

- [x] 0 Spike, in the dev VM, before any driver code (done 2026-09-17; see
      "Spike results"): bring up Lima + k3s
      with gVisor and Kata RuntimeClasses; hand-write a sandbox pod from the
      current guest image, the default-deny policy, the label-gated gateway
      egress policy and a stand-in proxy pod; prove with test-owned probes
      that the k3s NetworkPolicy controller enforces both directions under
      gVisor and Kata and that a pod cannot use another pod's IP; confirm
      Codex and Claude Code honour credentials in `HTTPS_PROXY`; confirm a
      directory-mounted ConfigMap refreshes inside a gVisor and a Kata
      pod; run Codex and Claude Code inside the gVisor pod and record what
      the inner sandboxes do (decision 14); measure pod start times per
      tier; note whether Kata boots under nested virtualization on this
      Mac. Output: a "Spike results" section here with the pinned versions
      and the decisions this changes.
- [x] 1 Transport abstraction and mutual TLS (work item 1); sbx shapes on
      Unix sockets with no behaviour change; race suite, vet, frontend
      build, live Mac run green. Done 2026-09-16 (track A, merged 496a8cf):
      `chat/internal/transport`, `warden tls bootstrap`, `services.*` and
      `tls.*` config; the merged binary passes `warden doctor` against the
      owner's existing install.
- [x] 2 Runtime, inspector, gateway and credential-store seams plus
      configuration (work item 2); sbx behaviour unchanged. Done 2026-09-17
      (track A, merged eb44e63): `RuntimeDriver` reshaped around `Prepare`,
      `Address` and manifest paths; `SandboxInspector` + `SbxInspector`;
      `Gateway` with `LoopbackGateways` and the new `SharedGateway`
      (credentialed dispatch, 407 challenge); `CredentialStore` with the
      file store; `runtime.kind`, `kubernetes.*`, `providers.*.secret`,
      validation by kind; gateway mode derived from the kind. Race suite,
      vet and the read-only `warden doctor` against the owner's install
      green.
- [x] 3 Minimal Kubernetes client (work item 3), no dependency on 1 or 2.
      Done 2026-09-17 (track B, merged 625b91b): `chat/internal/kube` with
      in-cluster and kubeconfig config, typed REST, watch/ListWatch, exec
      over WebSocket, logs; fake API server tests.
- [x] 4 Kubernetes driver, inspector, shared gateway, trust publisher and
      Secret store (work items 4 and 5), after 1, 2 and 3. Done 2026-09-17
      in three tracks: the runner driver `chat/internal/sandbox/kube`
      (merged 268c42b; live create 5–6.5 s, stop, resume with the workspace
      kept, fork by copy, reconcile on the dev cluster), the policy side
      `chat/internal/policy/kube` (merged 9e5ab10; cluster facts, canary
      proof passing live in 6 s under gVisor, trust publisher, Secret store,
      `policysvc` wiring with `--kubeconfig`), and the edge minting its own
      owner capability over a `tls://` upstream (merged 7297883).
- [x] 5 Guest base image (work item 6), from day one. Done 2026-09-17
      (track C, merged 2414686): `Dockerfile.base` built for real in the dev
      VM (`warden-guest-base:dev`, manifest digest
      `sha256:cd77d0ff2af115f3cb700414c7c54840c2c1c87c3322ea0a98c17880ef07b63e`
      for linux/arm64) and exercised under both tiers.
- [x] 6 Chart (work item 7); the static hardening objects, RBAC and
      NetworkPolicies after step 0, the Deployments after step 4. Chart
      landed 2026-09-17; the rendered `warden.json` now loads through
      `config.Load` for kind `kubernetes` (`helm_test.go`), the runner Role
      reads the trust ConfigMap, the Secret keys are named like the sbx
      files, and the edge behaviour the chart assumed is implemented. Chart
      landed 2026-09-17 (track C, merged 6f7d88e): all objects render, lint
      is clean, four goldens under `deploy/helm/warden/testdata`, 46/46
      objects accepted by the dev API server, a real install into a scratch
      namespace issued the four mTLS Secrets through the bootstrap Job and
      the admission policy rejected each forbidden pod shape. Remaining for
      this step: reconcile the rendered `warden.json` with the kind
      `kubernetes` schema once step 4 lands (the services currently refuse
      the `kubernetes` field, as expected), and the two Go-side decisions
      the chart assumes: the guest trust ConfigMap lives in the sandbox
      namespace and is chart-created (the policy service updates it, never
      creates it), and in owner auth mode the edge mints its own sign-in
      capability into `/var/lib/warden/edge/endpoint.json` and logs the
      launch URL, because chat writes no `endpoint.json` over `tls://`.
- [ ] 7 First milestone on the Mac: chat → gVisor pod → loopback preview
      through the Lima port forward, Codex and Claude, Google Docs and
      GitHub through the gateway.
- [ ] 8 End-to-end suite and the adversarial networking rows on gVisor
      (work item 8); then the same on Kata (dev VM if step 0 says it
      works, otherwise on the OVH server).
- [ ] 9 Public-preview mode with an Ingress and cert-manager on a real
      cluster (Owner: which cluster; the OVH server with k3s alongside the
      Compose install is the cheapest, a managed cluster with gVisor nodes
      the most representative).
- [ ] 10 Documentation and chart release (work item 9).

Tracks after step 0. Track A: steps 1, 2 then 4, the critical path. Track
B: step 3, independent. Track C: step 5, independent. Track D: step 6's
static parts. Steps 7 to 10 in order. Step 7 is the first user-visible
milestone.

## Out of scope

Multi-node scheduling of sandboxes and more than one runner (the
Deployments and mTLS are the hook; the runner's sandbox registry is still
one file); Docker inside guests; Windows containers; clusters without
NetworkPolicy enforcement (refused by the verifier, not worked around); a
`warden install` path for Kubernetes (Helm is the installer; `warden doctor
--kubeconfig` may run the cluster facts from outside); migrating an SBX
installation's sandboxes into a cluster; switching the sbx shapes to the
shared gateway; Panta; provider breadth beyond what the other shapes have.

## Appendix A: `warden.json` additions

```json
{
  "version": 1,
  "runtime": { "kind": "kubernetes" },
  "services": {
    "policy": { "listen": "tls://0.0.0.0:7443", "address": "tls://warden-policy:7443" },
    "runner": { "listen": "tls://0.0.0.0:7444", "address": "tls://warden-runner:7444" },
    "chat":   { "listen": "tls://0.0.0.0:7445", "address": "tls://warden-chat:7445" }
  },
  "tls": {
    "caFile": "/etc/warden/tls/ca.crt",
    "certFile": "/etc/warden/tls/tls.crt",
    "keyFile": "/etc/warden/tls/tls.key"
  },
  "kubernetes": {
    "namespace": "warden-sandboxes",
    "tier": "gvisor",
    "runtimeClass": "gvisor",
    "guestImage": "ghcr.io/monaddle-too/warden-guest-base",
    "guestImageDigest": "sha256:…",
    "storageClass": "",
    "workspaceSizeGi": 20,
    "gatewayService": "warden-gateway",
    "gatewayPort": 7000,
    "trustConfigMap": "warden-guest-trust",
    "gatewayCAMaxAgeDays": 365,
    "nodeSelector": {},
    "tolerations": []
  },
  "providers": {
    "codex":  { "secret": "warden-codex-login" },
    "claude": { "secret": "warden-claude-login" },
    "github": { "secret": "warden-github-login" }
  }
}
```

| Field | Default | Consumers |
|---|---|---|
| `runtime.kind` | `sbx` | all services: driver, inspector, gateway, credential store, validation of `sbx.*` vs `kubernetes.*` |
| `services.<svc>.listen` / `address` | today's `unix://` socket paths | the service's listener; its clients |
| `tls.*` | none; required when any address is `tls://` | all services |
| `kubernetes.namespace` | required | runner (pods, PVCs), policy (labels, facts, canaries) |
| `kubernetes.tier` | required | policy (handler allowlist, proofs), UI, docs |
| `kubernetes.runtimeClass` | required | runner (pod spec), policy (pinned in facts) |
| `kubernetes.guestImage` / `guestImageDigest` | required | runner (pod image), policy (`imageID` check); replaces `sbx.guestImage*` for this kind |
| `kubernetes.storageClass` | cluster default | runner (workspace PVCs) |
| `kubernetes.workspaceSizeGi` | 20 | runner |
| `kubernetes.gatewayService` / `gatewayPort` | `warden-gateway` / 7000 | policy (advertised address in `Begin`; the shared gateway listens on the port) |
| `kubernetes.gatewayCAMaxAgeDays` | 365 | policy (CA rotation; replaces `sbx.inspectionCertMaxAgeDays` for this kind) |
| `kubernetes.nodeSelector` / `tolerations` | none | runner (sandbox pod placement; Kata nodes are usually a pool) |
| `kubernetes.trustConfigMap` | `warden-guest-trust` | policy (publisher), runner (pod volume) |
| `providers.<p>.secret` | none; `authFile` for the sbx shapes | policy (Secret credential store). Key layout mirrors the files of the sbx shapes: codex `auth.json`; claude `claude.json`; github `github.json` (user-token mode) or `broker.json` plus `app-private-key.pem` (App mode). |

`sandboxes.*`, `previews.*`, `auth.*` and `chat.*` are unchanged. `paths.*`
keep their defaults, which the chart mounts at the same container paths as
the Compose file does. `sbx.*` is ignored for this kind.

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

Sizing: with 16 GiB for the VM, two resident sandboxes at 1024 MiB (the dev values; 1536 MiB elsewhere) plus one
spare fit beside the four service pods and the cluster components under
either tier; Kata adds the guest kernel's memory per sandbox. The VM is
disposable (`limactl delete warden-k8s`); all state that matters lives in
the repository and in the chart values.

## Progress

- 2026-09-16: plan drafted from the a45aeaf inventory; no code yet.
- 2026-09-17 (later): step 2 merged (eb44e63), the chart (6f7d88e) and
  the Kubernetes docs (12b489e) merged; plan reconciled with both. Step 4
  runs as three parallel tracks: the runner driver (`k8s/track-a`,
  `chat/internal/sandbox/kube`), the policy side (`k8s/track-d`,
  `chat/internal/policy/kube`: inspector, canaries, trust publisher,
  Secret store, shared-gateway wiring) and the edge's owner capability
  over a `tls://` upstream (`k8s/track-c`).
- 2026-09-17: steps 0, 1, 3 and 5 done and merged (see the step list and
  "Spike results"). The dev VM `warden-k8s` runs k3s with both tiers;
  `warden-guest-base:dev` and `warden:dev` (server image, built from the
  cross-compiled linux/arm64 binary and the pnpm web build) are in the
  node's containerd; `scripts/k8s-dev.sh build-images` reproduces both.
  In progress on track branches: step 2 (seams, `k8s/track-a`) and the
  chart's static parts (`k8s/track-c`). Kata boot-time stall under nested
  virtualization left unexplained after a console-capture attempt; the
  Kata tier's timing verification is deferred to real KVM.
- 2026-09-16 (execution): the owner started an agent loop to run the plan
  to completion. Decisions 5 and 6 proceed with their recommendations;
  step 9's real cluster stays the owner's call, so that step is exercised
  in the dev VM (Ingress controller plus a self-signed issuer) and the
  real-cluster run is recorded as remaining. Track branches from 5ef87fd:
  `k8s/track-a` (step 1, then 2 and 4) in `.local/wk8s-track-a`,
  `k8s/track-b` (step 3) in `.local/wk8s-track-b`, `k8s/track-c` (step 5)
  in `.local/wk8s-track-c`; step 0 runs in this worktree.
- 2026-09-16 (revision): reviewed for single-host assumptions carried
  over. Replaced: the single core pod with four Deployments over mutual
  TLS (decision 5); per-binding gateway ports with one credentialed
  gateway and label-gated egress (decision 4); in-guest verifier probes
  with canaries and control-plane facts (decision 12); CA install by exec
  with a mounted trust bundle (decision 9); Secret-seeded provider files
  with a credential store (decision 8); the pod-UID identity pin with a
  PVC identity and per-generation pod pins (decision 3); the derived tier
  with an explicit tier and handler allowlist; the two-minute canary
  cadence with watch-driven refresh; the `runc` tier removed; the
  persisted set decided as the agent home. Open owner decisions: 5 (four
  Deployments and mTLS from the start), 6 (in-tree client vs client-go),
  step 9 (which real cluster).
