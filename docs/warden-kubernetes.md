# Warden on Kubernetes

This is the operator guide for the third shape of Warden: the same `warden`
binary as the Mac install (`docs/warden-local-install.md`) and the OVH
Compose install (`deploy/chat/README.md`), installed on a Kubernetes cluster
by the Helm chart in `deploy/helm/warden`. The design, the decisions and the
step-by-step record are in `docs/warden-kubernetes-plan.md`; this document
says what to run and what you get.

What is verified: the chart renders and lints, its four golden renders are
under `deploy/helm/warden/testdata`, every object it produces was accepted
by the dev cluster's API server, a real install into a scratch namespace
issued the four mutual-TLS Secrets through the bootstrap Job, and the
admission policy rejected each forbidden pod shape (plan, step 6). The
guest base image runs under both tiers and the NetworkPolicy behaviour was
proven with test-owned probes (plan, "Spike results"). The services
themselves do not yet run in the `kubernetes` kind: the runtime driver, the
policy service's inspector, the shared gateway, the trust publisher and the
Secret credential store are plan steps 2 and 4, and the first end-to-end
chat is step 7. Every statement below that depends on them is marked "(to
be verified in step N)". Until step 7 lands, an install from this chart
brings up the objects and the pods, and the pods refuse the `kubernetes`
section of `warden.json`.

## What this shape is, and is not

- **No SBX.** There is no sandboxd, no SBX daemon and no `sbx` executable in
  any pod. A sandbox is a pod in a dedicated namespace under a Kubernetes
  RuntimeClass, and the `runtime.kind: kubernetes` setting in `warden.json`
  (rendered by the chart) selects the Kubernetes driver in the runner and
  the Kubernetes inspector in the policy service. The Mac and OVH shapes
  keep SBX unchanged.
- **Two isolation tiers, chosen at install.** `runtime.tier` is `kata`
  (a microVM with its own kernel per sandbox) or `gvisor` (a userspace
  kernel per sandbox). There is deliberately no plain-container tier; see
  "Tiers".
- **Four services over mutual TLS.** The policy service, the runner, the
  chat and the edge are four Deployments (`strategy: Recreate`, one replica
  each), each with its own ServiceAccount, state PersistentVolumeClaim and
  NetworkPolicy. They speak the same line-JSON protocol as the other shapes
  over `tls://` instead of Unix sockets; the client certificate is the
  service identity, so the edge does not read the chat's `endpoint.json`
  and the owner bearer of the sbx shapes is not used between services.
- **One gateway, identity by credential, egress by label.** The policy
  service runs one gateway listener behind the `warden-gateway` Service,
  not one loopback port per sandbox. A sandbox is told a proxy URL carrying
  its binding credential; the gateway authenticates that credential. Egress
  is granted by setting the label `warden.monaddle.com/egress=gateway` on
  the sandbox pod and revoked by removing it: one static NetworkPolicy lets
  labelled pods reach the gateway port and nothing else, and the namespace
  default-deny covers everything else.
- **State on PVCs, logins in Secrets.** Each service has a PVC; each
  sandbox has a workspace PVC mounted at `/home/agent`, which is the only
  path that survives a stop. Provider logins (Codex, Claude, GitHub) are
  Kubernetes Secrets the operator creates before installing; the chart
  never contains a credential and only the policy ServiceAccount may read
  or write them.
- **Hardening by admission, proven by the verifier.** The sandbox namespace
  has Pod Security Admission at `baseline`, a ValidatingAdmissionPolicy
  that refuses any pod not in the exact shape the runner creates, a
  ResourceQuota and a LimitRange. The policy service reads these objects
  back as cluster facts and runs two canary pods to prove NetworkPolicy is
  enforced; a cluster where it is not is refused, not worked around.
- **Not in this shape:** Docker inside guests (the `shell-docker` template
  is an SBX feature), more than one runner or multi-node placement of
  sandboxes beyond what the scheduler does with one runner, a `warden
  install` path (Helm is the installer), migrating an SBX installation's
  sandboxes into a cluster, Windows containers.

## Support matrix

| Requirement | What is needed | Notes |
|---|---|---|
| Kubernetes | 1.30 or later (`Chart.yaml` sets `kubeVersion: ">=1.30.0-0"`; the `-0` admits vendor-suffixed versions such as GKE's `v1.35.7-gke.1222000`) | ValidatingAdmissionPolicy is GA from 1.30; the sandbox namespace's hardening depends on it. |
| CNI | One that enforces NetworkPolicy, ingress and egress | The spike proved k3s's embedded controller; Cilium, Calico and GKE Dataplane V2 enforce; plain flannel does not. The policy service's canaries refuse a cluster where the policies are not enforced (the canary proof runs at every policy start and passed on the dev cluster). **Anti-spoofing caveat:** the spike showed a pod cannot bind another pod's address, which is not a proof that the CNI drops spoofed source addresses. The gateway treats the binding credential as the authority and the source pod as a second check for that reason; choose a CNI with source-address filtering (Cilium and Calico document it) where the difference matters. |
| RuntimeClass, `gvisor` tier | A RuntimeClass whose handler is the `runsc` shim, on nodes with `allow-suid = "true"` in the runsc configuration | GKE Sandbox provides RuntimeClass `gvisor` and sets allow-suid; a self-managed node needs the shim registered in containerd and `/etc/containerd/runsc.toml` with `allow-suid = "true"` (see "Development"), or the guest's passwordless `sudo` does not work. |
| RuntimeClass, `kata` tier | A RuntimeClass from kata-deploy (`kata-qemu` by default; `runtime.runtimeClassName` names another such as `kata-clh`) on nodes with `/dev/kvm` | Bare metal, VMs with nested virtualization, or cloud nodes that expose KVM. Kata nodes are usually a pool: set `sandboxes.nodeSelector` and `sandboxes.tolerations`. |
| StorageClass | ReadWriteOnce volumes; `dataSource` PVC cloning for chat forks | Cloning is a CSI feature (GKE `pd.csi.storage.gke.io`, Longhorn, Ceph RBD and others); without it the runner copies the workspace with tar through exec and reports which it used. k3s's `local-path` does not clone, and the copy fallback is what the dev cluster exercises (the clone path is covered by the driver's tests against a fake API server). |
| cert-manager | Optional | With it, the four service certificates are `Certificate` objects renewed automatically; without it a bootstrap Job issues them once. Public previews need cert-manager (or another issuer) for the wildcard preview certificate. |
| Ingress controller | Public previews only | Any controller; the chart renders one Ingress with the app host and `*.<hostSuffix>`. Loopback previews need no Ingress. |
| Helm | 3 | `helm.sh/resource-policy: keep` and hooks are used. |
| Images | `ghcr.io/monaddle-too/warden` (the server) and `ghcr.io/monaddle-too/warden-guest-base` (the guest), or locally built images by digest | The guest image is pinned by its platform manifest digest, never the index digest (`guestImage.digest` is required). |

## Install

### 1. The release namespace and the provider Secrets

Everything except the sandboxes lives in the release namespace (`warden`
below). The provider logins are Secrets you create there first; each holds
the same JSON the sbx shapes keep in `providers.<p>.authFile`, under a key
named like that file (`auth.json` for Codex, `claude.json` for Claude,
`github.json` for GitHub). The default names are the ones `values.secrets`
lists; change both together.

```sh
kubectl create namespace warden
```

**Codex** (`warden-codex-login`): the `auth.json` that `codex login
--device-auth` writes. On any machine with a Codex CLI:

```sh
mkdir -m 700 -p "$TMPDIR/codex-login"
CODEX_HOME="$TMPDIR/codex-login" codex login --device-auth
kubectl -n warden create secret generic warden-codex-login --from-file=auth.json="$TMPDIR/codex-login/auth.json"
rm -rf "$TMPDIR/codex-login"
```

Codex refreshes its tokens on the host in the sbx shapes; in this shape
the Secret store can write a refreshed value back through the API, but no
loader does so yet, so replace the Secret yourself when the login expires
(see "Provider Secret rotation").

**Claude** (`warden-claude-login`): the file `warden login claude` writes
from a `claude setup-token` value, `{"claudeAiOauth":{"accessToken":…,
"expiresAt":…}}`. On a machine with a Mac-shape install the file is
`~/.warden/provider/claude.json`; `scripts/warden-claude-token FILE` writes
the same file without an install:

```sh
scripts/warden-claude-token "$TMPDIR/claude.json"
kubectl -n warden create secret generic warden-claude-login --from-file=claude.json="$TMPDIR/claude.json"
rm -f "$TMPDIR/claude.json"
```

**GitHub** (`warden-github-login`): in user-token mode the file `warden
login github` writes (`{"token":"gho_…","login":"…","scopes":[…],
"obtained":…}`), for example `~/.warden/provider/github.json` from a Mac
install:

```sh
kubectl -n warden create secret generic warden-github-login --from-file=github.json="$HOME/.warden/provider/github.json"
```

In GitHub App mode (`providers.github.appID` set) the same Secret holds the
App broker's credential instead of a user token, under the keys
`broker.json` and `app-private-key.pem`; the App-mode broker on Kubernetes
is not wired yet.

A provider you do not use is turned off with `providers.<p>.enabled:
false`; its Secret is then not required and not granted.

### 2. Values

The chart refuses to render without `guestImage.digest`. Start from the
smallest file for your shape and add what the values reference below
describes.

Owner sign-in, loopback previews (one person, a port-forward, no domain):

```yaml
# values.yaml
guestImage:
  digest: sha256:<the platform manifest digest of the guest base image>
runtime:
  tier: gvisor          # or kata
tls:
  bootstrap: true       # no cert-manager
```

Google sign-in, public previews (a domain, an Ingress controller,
cert-manager):

```yaml
# values.yaml
guestImage:
  digest: sha256:<the platform manifest digest of the guest base image>
runtime:
  tier: gvisor
auth:
  mode: google
  publicURL: https://warden.example.com
  google:
    signInClientID: <client id>.apps.googleusercontent.com
    owners: [you@example.com]
previews:
  mode: public
  hostSuffix: preview.example.com
  ingress:
    className: nginx
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt-dns   # DNS-01: the preview host is a wildcard
tls:
  certManager:
    enabled: true
networkPolicy:
  edgeIngressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
```

The chart checks the combinations: `previews.mode: public` requires
`auth.mode: google`, a dotted `previews.hostSuffix` and an `https://`
`auth.publicURL`; `auth.mode: owner` requires an `http://` URL (default
`http://127.0.0.1:<edge.port>`); `tls.bootstrap` and
`tls.certManager.enabled` are exclusive. With neither TLS option set, the
four Secrets `warden-{policy,runner,chat,edge}-tls` (each with `ca.crt`,
`tls.crt`, `tls.key` from one CA, CN equal to the identity) must exist in
the release namespace before the pods can start.

### 3. `helm install`

Every Warden release `v<X>` publishes the chart to
`oci://ghcr.io/monaddle-too/charts/warden` as chart version `<X>` (the tag
without its `v`, so `v0.1.0-alpha.9` is `--version 0.1.0-alpha.9`;
`appVersion` is the tag). The release workflow packages it after the
server image, so its values default to that release's image: `image.tag`
is the tag and `image.digest` is the image's index digest, and one chart
version means one binary. The GitHub release page also carries the
`warden-<X>.tgz` and its sum in `SHA256SUMS`.

```sh
helm install warden oci://ghcr.io/monaddle-too/charts/warden --version <chart version> -n warden --create-namespace -f values.yaml
```

`helm show values oci://ghcr.io/monaddle-too/charts/warden --version <chart
version>` prints that release's defaults. From a checkout instead, the
chart has placeholder versions (`0.0.0-dev`) and no image default, so set
`image.tag` (or `image.digest`) to a published image in your values:

```sh
helm install warden deploy/helm/warden -n warden --create-namespace -f values.yaml --set image.tag=v<X>
```

A chart built locally by `scripts/release.sh --chart` (when Actions
minutes are out) sets the versions and `image.tag` but no digest, because
that script builds no image; push `ghcr.io/monaddle-too/warden:<tag>` from
`deploy/chat/Dockerfile` before installing it. To run an image of your own
from the published chart, set `image.digest=""` together with
`image.repository` and `image.tag`, because a set digest wins over the
tag.

One release per namespace: the Services, ServiceAccounts and TLS Secrets
have fixed names (`warden-policy`, `warden-runner`, `warden-chat`,
`warden-edge`, `warden-gateway`) because the certificates and the
addresses in `warden.json` name them. What the install creates:

1. First, with `tls.bootstrap: true`, the pre-install Job
   `warden-tls-bootstrap` runs `warden tls bootstrap` from the server image
   and stores the CA and the four certificates as the Secrets
   `warden-<identity>-tls`. It does nothing when all four exist and fails
   when only some do (they must share a CA).
2. The sandbox namespace (`sandboxNamespace.name`, default
   `warden-sandboxes`) with its PSA labels, the empty trust ConfigMap
   `warden-guest-trust`, the three sandbox NetworkPolicies (default deny;
   gateway egress by label; runner ingress), the ResourceQuota and
   LimitRange, the runner's and the policy service's Roles there, and the
   cluster-scoped ValidatingAdmissionPolicy and binding named
   `warden-<sandbox namespace>-sandbox-pods`.
3. In the release namespace: the ConfigMap `warden-config` holding
   `warden.json`, four ServiceAccounts, the policy service's credentials
   Role (the named provider Secrets), four PVCs
   (`warden-{policy,runner,app,edge}-state`), four Deployments, the
   Services, the core NetworkPolicies (default deny plus one per service)
   and, in public mode, the Ingress `warden-edge`. With cert-manager, the
   Issuers and Certificates.

`helm install` prints the NOTES with the exact commands for your values.
Then:

```sh
kubectl -n warden get pods
kubectl -n warden-sandboxes get pods,pvc
```

The chart's `kubectl` images: only the bootstrap Job pulls one
(`tls.bootstrapJob.kubectlImage`, `docker.io/alpine/k8s:1.34.1`), because
the server image has neither an HTTP client nor kubectl.

### 4. First login

**Owner mode.** Forward the edge port to your machine and keep the forward
running while you use Warden:

```sh
kubectl -n warden port-forward svc/warden-edge 18781:18781
```

In this shape the chat service writes no `endpoint.json` the edge could
read (they share no volume; the edge reaches the chat with its certificate
alone), so the edge mints the owner sign-in capability itself at every
start, as the chat does on the other shapes: it stores it in
`/var/lib/warden/edge/endpoint.json` (mode 0600, the chat's file shape,
`{"url":"http://127.0.0.1:18781","token":"<64 hex>"}`) and logs one line
with the launch URL. Read that line:

```sh
kubectl -n warden logs deploy/warden-edge | grep -m1 'launch URL'
```

It looks like `Warden launch URL (owner capability; rotates at every edge
start; kept in /var/lib/warden/edge/endpoint.json): http://127.0.0.1:18781/?launch=<time>#session=<capability>`,
the same URL `warden open --print` builds on the Mac. If the log has been
rotated away, the state file still holds the capability
(`kubectl -n warden exec deploy/warden-edge -- cat /var/lib/warden/edge/endpoint.json`;
the launch URL is `<url>/#session=<token>`).

Open the URL. The web app stores the capability, drops it from the address
bar and sends it as a bearer; the edge identifies you as the owner by it and
mints the cookie session that preview navigations need (only for browser
requests, which carry fetch metadata: curl and the TUI get no session, and
the cookie name carries the forwarded port, so a local `warden start` on the
same address keeps its own). Previews are
`http://<binding-id>.localhost:18781/…` through the same forward, with the
same ticket, per-request binding check and revocation model as the other
shapes. The capability rotates when the edge restarts (every session made
from the old one ends with it, as on the Mac when the chat restarts); read
the log again after an upgrade. Anyone who can read the edge pod's log or
its PVC can sign in as the owner, which is the same trust the Mac places in
the state directory.

**Google mode.** Open `auth.publicURL` and sign in with a Google account
listed in `auth.google.owners`; any other account is refused, as on OVH.
Previews are `https://<binding-id>.<hostSuffix>/…` through the Ingress:

```sh
kubectl -n warden get ingress warden-edge
```

Google Docs is connected from the browser (Admin console or a chat's
Documents panel), as in the other shapes; the token is stored by the
policy service in its PVC.

### 5. Upgrades

To the next release's chart (which brings its image):

```sh
helm upgrade warden oci://ghcr.io/monaddle-too/charts/warden --version <chart version> -n warden -f values.yaml
```

(or `helm upgrade warden deploy/helm/warden -n warden -f values.yaml
--set image.tag=v<X>` from a checkout).

- Each Deployment uses `Recreate`: the old pod stops before the new one
  starts, so each service is down for the restart. An edge restart signs
  viewers out (chat and agent work stay server-side); a runner restart
  reconciles the sandbox pods and PVCs it finds by label (every registered
  sandbox is stopped, spares are removed and recreated, unregistered claims
  are kept and logged; seen on every runner restart on the dev cluster).
- The pods carry a checksum of the rendered `warden.json`, so a values
  change that alters it rolls the pods; a change that does not (for
  example `resources`) rolls only what Kubernetes needs to.
- The PVCs are not touched. The bootstrap Job runs again as a pre-upgrade
  hook and does nothing while the four TLS Secrets exist.
- The trust ConfigMap in the sandbox namespace is created empty by the
  chart and written by the policy service; Helm's three-way merge leaves
  its data alone on upgrade.
- Guest image bumps are a values change (`guestImage.digest`); new
  sandboxes use the new digest, existing pods keep theirs until they are
  stopped, and the verifier checks each pod's `imageID` against the pinned
  digest of its own generation (the check is exercised by the policy
  package's tests; a live image bump has not been run yet).

### 6. Uninstall

```sh
helm uninstall warden -n warden
```

What is kept, by default:

- The four service PVCs `warden-{policy,runner,app,edge}-state`
  (`storage.keepOnUninstall: true`; they hold the chats, the sandbox
  registry, the gateway CA and the sign-in ledger). Delete them by hand to
  start over.
- The sandbox namespace (`sandboxNamespace.keep: true`) with the workspace
  PVCs and any sandbox pods still in it; the policies, quota, Roles and the
  trust ConfigMap inside it are release objects and are removed.
  `kubectl delete namespace warden-sandboxes` removes the rest
  deliberately.
- The TLS Secrets from the bootstrap Job (they are not release objects),
  the Secrets cert-manager wrote, and the provider Secrets you created.
- The release namespace itself (Helm does not delete namespaces).

Removed: the Deployments, Services, ConfigMap, NetworkPolicies, RBAC, the
ValidatingAdmissionPolicy and binding, the RuntimeClass if the chart
created it, the Ingress and the cert-manager objects.

## Values reference

Every top-level key of `deploy/helm/warden/values.yaml`, with the defaults
from that file. The file itself documents each value and is the reference
when the two differ.

| Key | Purpose | Default |
|---|---|---|
| `image` | The server image: `repository`, `tag` (empty selects the chart's `appVersion`), `digest` (`sha256:<64 hex>`; when set the image is pulled by digest and `tag` is ignored; pin it in production), `pullPolicy`. | `ghcr.io/monaddle-too/warden`, `""`, `""`, `IfNotPresent` |
| `imagePullSecrets` | Names of existing pull Secrets in the release namespace, applied to the four Deployments and the bootstrap Job. | `[]` |
| `guestImage` | The guest base image sandboxes run: `repository` and `digest`. The digest is required and must be a platform manifest digest; the runner pins it and the policy service checks each pod's `imageID` against it. | `ghcr.io/monaddle-too/warden-guest-base`, `""` |
| `runtime` | `tier` (`kata` or `gvisor`); `runtimeClassName` (empty selects the tier default, `gvisor` or `kata-qemu`; the admission policy refuses any other class); `handlers.{gvisor,kata}` (used only when the chart creates the RuntimeClass); `createRuntimeClasses` (off: clusters usually own theirs); `overhead.{memoryMi,cpuMillis}` (the RuntimeClass pod overhead the sandbox quota must allow, typically 160Mi and 250m on Kata; written into the RuntimeClass when the chart creates it). | `gvisor`, `""`, `runsc`/`kata-qemu`, `false`, `0`/`0` |
| `sandboxNamespace` | `name` of the sandbox namespace, whether the chart creates it, and whether it is kept on uninstall. | `warden-sandboxes`, `true`, `true` |
| `sandboxes` | Sandbox sizing, mirrored into `warden.json` and into the namespace quota and LimitRange: `memoryMB` (512–16384; request and limit of every sandbox container), `cpuMillis`, `maxRunning`, `warmSpares`, `stopAfterIdleMinutes`, `keepStopped` (stopped workspaces kept), `extraPods` (quota headroom for the two canaries), `extraPVCs` (headroom for a fork clone in flight), `nodeSelector` and `tolerations` for sandbox pods (rendered into `warden.json` only when set). | `1536`, `1000`, `2`, `1`, `15`, `32`, `2`, `2`, `{}`, `[]` |
| `egress` | `restricted` (the policy template's destination list) or `open` (any public HTTP/HTTPS host); enforced at the gateway, same NetworkPolicies either way. | `restricted` |
| `auth` | `mode` (`owner` or `google`); `publicURL` (the URL browsers open: `http://127.0.0.1:<edge.port>` by default in owner mode, the Ingress URL in Google mode); `google.signInClientID`, `google.owners`, `google.demoDomains`. | `owner`, `""`, `""`, `[]`, `[]` |
| `previews` | `mode` (`loopback` or `public`); `hostSuffix` (public only); `ingress.enabled`, `ingress.className`, `ingress.annotations`, `ingress.host` (empty derives the app host from `auth.publicURL`), `ingress.tls.enabled`, `ingress.tls.secretName` (the certificate for the app host and `*.<hostSuffix>`). | `loopback`, `""`, `true`, `""`, `{}`, `""`, `true`, `warden-edge-public-tls` |
| `edge` | `port` the edge listens on and the `warden-edge` Service exposes; `service.type` and `service.annotations`. | `18781`, `ClusterIP`, `{}` |
| `gateway` | `port` of the policy service's shared gateway behind the `warden-gateway` Service. | `7000` |
| `services` | Ports of the mutual-TLS control listeners: `policy.port`, `runner.port`, `chat.port` (rendered as `services.*` in `warden.json`); `runner.previewPort`, the runner's shared preview server the chat dials as `https://warden-runner:<port>/<publication ID>` (rendered as `services.runner.previews`). | `7443`, `7444`, `7445`, `7446` |
| `storage` | `className` for every PVC (empty is the cluster default; forks clone when it supports `dataSource`); sizes of the service PVCs `policy`, `runner`, `app`, `edge`; `workspaceGi` per sandbox; `keepOnUninstall`. | `""`, `5Gi`, `5Gi`, `10Gi`, `1Gi`, `20`, `true` |
| `secrets` | Names of the existing provider login Secrets: `codex`, `claude`, `github`. | `warden-codex-login`, `warden-claude-login`, `warden-github-login` |
| `providers` | `codex.enabled`, `claude.enabled`, `google.enabled` and `google.docsClient` (`builtin` or a client file path inside the policy pod), `github.enabled`, `github.appID` (0 is user-token mode; otherwise `appSlug` and `installationOwner` are rendered too). A disabled provider renders as JSON `null`. | all `true`, `builtin`, `0`, `""`, `""` |
| `tls` | `certManager.enabled`, `certManager.issuerRef` (an existing CA issuer; empty makes the chart create a self-signed Issuer, a CA Certificate and a CA Issuer), `certManager.duration`, `renewBefore`, `caDuration`, `caRenewBefore`; `bootstrap` (the pre-install/pre-upgrade Job); `bootstrapJob.kubectlImage`, `kubectlPullPolicy`, `days` (the CA lasts ten times as long). | `false`, `{}`, `8760h`, `720h`, `87600h`, `8760h`, `false`, `docker.io/alpine/k8s:1.34.1`, `IfNotPresent`, `365` |
| `podSecurityContext` | UID, GID, `fsGroup` and `fsGroupChangePolicy` of the four service pods (the image has no dedicated user). | `1000`, `1000`, `1000`, `OnRootMismatch` |
| `resources` | Requests and limits per service container: `policy`, `runner`, `chat`, `edge`. | policy and runner 250m/256Mi, limit 1Gi; chat 100m/128Mi, limit 512Mi; edge 100m/64Mi, limit 256Mi |
| `nodeSelector`, `tolerations`, `affinity` | Scheduling of the four service pods (not the sandboxes). | `{}`, `[]`, `{}` |
| `networkPolicy` | `enabled` (off only for debugging: the policy service refuses an unenforced cluster anyway); `dns.namespace`, `dns.podSelector` and `dns.cidr` (how the pods reach cluster DNS: the resolver pods, plus a CIDR for a node-local cache such as GKE's NodeLocal DNSCache at `169.254.20.10/32`, which is on by default on Autopilot); `apiServer.cidr` and `apiServer.ports` (kube-apiserver is not a pod; restrict the CIDR where known); `providerEgress.cidr` and `.ports` (the policy pod's egress to provider hosts); `edgeIngressFrom` (empty admits every source; otherwise NetworkPolicyPeer objects such as the Ingress controller's namespace); `edgeEgress` (Google's signing keys in Google mode). | `true`, `kube-system`/`k8s-app: kube-dns`, `0.0.0.0/0`/`[443, 6443]`, `0.0.0.0/0`/`[443]`, `[]`, `0.0.0.0/0`/`[443]` |
| `rbac` | `policyClusterFacts`: a read-only ClusterRole for the policy service on the cluster-scoped hardening it verifies (the sandbox Namespace, RuntimeClasses, ValidatingAdmissionPolicies and bindings). | `true` |
| `extraEnv` | Extra environment variables per service container: `policy`, `runner`, `chat`, `edge`. | `[]` each |

What the chart renders into `warden.json` from these (plan, appendix A):
`runtime.kind: kubernetes`; `services.*` as `tls://0.0.0.0:<port>` listeners
and `tls://warden-<svc>:<port>` addresses, plus `services.runner.previews`
(the runner's preview server on `services.runner.previewPort`, the same
two URL shapes); `tls.*` under `/etc/warden/tls`;
`kubernetes.{namespace,tier,runtimeClass,guestImage,guestImageDigest,
storageClass,workspaceSizeGi,gatewayService,gatewayPort,trustConfigMap,
nodeSelector,tolerations}`; `sandboxes.*` with `egress`;
`previews.{mode,hostSuffix,edgeListen}`; `auth.*`; and `providers.<p>.secret`
in place of `authFile`. `paths.state` is `/var/lib/warden`, so each service's
state is at the same container path as in the Compose file.

## Tiers

Both tiers keep the controls that do not depend on the kernel: the
namespace default-deny and the label-gated gateway egress, the gateway's
per-request decisions and credential injection, the admission policy, the
verifier and the preview model. What differs is the boundary under the
guest's root, and what the agents' own inner sandboxes can do. The
measurements are from the plan's step 0 spike (Lima on an M4 Mac, k3s
v1.36.4, gVisor release-20260914.0 on the systrap platform, Kata 4.2.0 with
QEMU).

**gVisor** (`runtime.tier: gvisor`). Each sandbox runs on gVisor's
userspace kernel; the boundary is the sentry's syscall surface, not
hardware. It runs wherever the `runsc` shim can be installed, including
managed clusters (GKE Sandbox is exactly this), and a sandbox starts in
about 0.3 s from a cached image, so warm spares are cheap.

- `allow-suid = "true"` is required in the node's runsc configuration:
  runsc ignores SUID bits by default and the guest's passwordless `sudo`
  stays uid 1000 without it. GKE Sandbox sets it; a self-managed node may
  not.
- Codex runs **without its inner sandbox** on this tier
  (`sandbox_mode = "danger-full-access"`): its Linux sandbox needs
  bubblewrap with `--unshare-net`, which fails under gVisor (netlink
  `RTM_NEWADDR` unimplemented), and Landlock returns `ENOSYS`. The gVisor
  boundary, the NetworkPolicy and the gateway are the controls. Claude
  Code's controls are unchanged (Warden's MCP permission prompt,
  `--permission-mode default`; it does not use bubblewrap in this launch).
- Plain bubblewrap with user, pid, ipc and mount namespaces works, so a
  session can still use it for its own purposes; nested Docker does not.
- A directory-mounted ConfigMap refreshes inside a running gVisor pod in
  about 48 s (the kubelet's sync period), which bounds how long a CA
  rotation takes to reach a running guest.

**Kata Containers** (`runtime.tier: kata`). Each sandbox is a microVM with
its own guest kernel, the closest match to the SBX guarantee.

- Needs `/dev/kvm` on the sandbox nodes: bare metal, VMs with nested
  virtualization, or cloud nodes that expose it.
- The agents' inner sandboxes work as on any Linux: `codex sandbox` runs
  with bubblewrap and Landlock (ABI 7), so on this tier Codex keeps its full
  inner sandbox. A hand-run `bwrap --proc` fails on the masked container
  `/proc`; lifting that needs `procMount: Unmasked` with
  `hostUsers: false`, which the runner does not set.
- Pod start from a cached image is about 33 s when the guest boots
  normally. Under nested virtualization on the Apple Silicon dev VM about
  half the boots stalled for roughly 17 minutes before the agent started,
  independent of load; the cause was not found. The plan therefore verifies
  the Kata tier's timing and its end-to-end runs on a host with real KVM,
  and treats the dev VM's Kata as functional only. Size `warmSpares` for
  the boot time you measure.
- Kata's default `cpu_features = "pmu=off"` is refused by QEMU under nested
  virtualization on Apple Silicon (the VM's KVM exposes no PMU); the dev
  loop clears it with a drop-in. Real KVM hosts do not need this.
- Set `runtime.overhead` to the RuntimeClass's pod overhead (typically
  160Mi and 250m for `kata-qemu`) so the sandbox quota allows it, and a
  ConfigMap refresh reaches a running Kata pod in about 3 s.

**Why there is no `runc` tier.** A plain container shares the node kernel
with everything else on it, and the whole point of the RuntimeClass is that
root in the guest is a product feature. The gVisor shim installs in one
step (the dev VM does it in a provisioning script), so a `runc` mode would
buy nothing and would leak into support. The admission policy refuses a
sandbox pod without the configured RuntimeClass, and the verifier refuses a
RuntimeClass whose handler is outside the tier's allowlist (the allowlist
per tier lives in the policy service; that it accepts the handler names
GKE Sandbox, kata-deploy and a self-managed `runsc` use is `runsc` and
`gvisor` for the gVisor tier and `kata`, `kata-qemu`, `kata-clh`,
`kata-fc` and `kata-qemu-runtime-rs` for Kata; `runtime.handlers` in the
chart overrides it).

## Operations

```sh
kubectl -n warden get pods
kubectl -n warden logs --tail 50 deploy/warden-policy
kubectl -n warden logs --tail 50 deploy/warden-runner
kubectl -n warden logs --tail 50 deploy/warden-chat
kubectl -n warden logs --tail 50 deploy/warden-edge
kubectl -n warden-sandboxes get pods,pvc -L warden.monaddle.com/sandbox,warden.monaddle.com/egress
```

Each service logs to stdout; there are no log files on the PVCs. As in the
other shapes, every service answers `--version` with `<name> <revision>
protocol=<n>`, the chat logs the three revisions it handshaked with at
start, and it refuses to run beside a runner or policy service on another
protocol number.

**The cluster facts and the canary proof.** At start, on any watch event on
the sandbox namespace's NetworkPolicies, Namespace, RuntimeClass or
admission objects, and hourly, the policy service establishes the cluster
fact "this cluster enforces what the chart installed": the namespace's PSA
label and role label, the three static NetworkPolicies, the
ValidatingAdmissionPolicy and its binding, the RuntimeClass and its
handler, and two short-lived canary pods in the sandbox namespace, one
without the egress label that must fail to reach a cluster address, the
API server, cluster DNS and an external address, and one with the label
that must reach the gateway and nothing else. A failed fact refuses every
sandbox (chats report enforcement unavailable, as "unsupported SBX
version" does today); nothing is worked around and no sandbox gets egress
until the fact passes again (the suite's `unlabelled-pod` and
`cluster-addresses` rows and the policy restart cover this). Per-sandbox
verification is control-plane facts only: the pod's spec and labels, the
policies that select it, its `imageID`, the PVC UID and the per-generation
pod UID pin. Nothing runs inside a guest on the verifier's behalf.

**Provider Secret rotation.** Replace the Secret's `auth.json`; the policy
service watches the named Secrets and picks up the new value without a
restart (verified against the dev cluster):

```sh
kubectl -n warden create secret generic warden-claude-login --from-file=claude.json="$TMPDIR/claude.json" --dry-run=client -o yaml | kubectl apply -f -
```

Until a Secret exists or while its token is rejected, chats on that
provider fail with the same "Refresh the … sign-in" message as the other
shapes. The Admin console's Disconnect for GitHub and Google works as
before; for GitHub it empties the stored token in the Secret rather than
deleting a file (not exercised live yet), and the token stays valid at
GitHub until you revoke it there.

**Gateway CA rotation.** The gateway CA is in the policy PVC at
`/var/lib/warden/policy/gateway-ca/`. The policy service publishes the
guest trust bundle (the base image's system CAs plus the CA in force) into
the ConfigMap `warden-guest-trust` in the sandbox namespace at start and
after a rotation; the runner mounts that ConfigMap at `/opt/warden/trust`
in every sandbox pod, which is where the image's system bundle symlink and
every client environment variable point, so the kubelet's in-place refresh
delivers a rotated CA to running guests without exec, root or restart
(about 48 s on gVisor, 3 s on Kata; the publisher and the mount run on
the dev cluster, every guest there trusts the published bundle). The
service rotates the CA itself at start when it is older than the
configured maximum age (`sbx.inspectionCertMaxAgeDays` in the sbx shapes,
`kubernetes.gatewayCAMaxAgeDays` here). To rotate on demand, stop
the policy service, run the subcommand against its PVC, and start it again;
the previous CA directory is kept as `gateway-ca.retired-<timestamp>`:

```sh
kubectl -n warden scale deploy/warden-policy --replicas=0
kubectl -n warden wait --for=delete pod -l warden.monaddle.com/component=policy --timeout=60s
kubectl -n warden apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: warden-rotate-ca
spec:
  restartPolicy: Never
  securityContext: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  containers:
    - name: rotate
      image: ghcr.io/monaddle-too/warden:<tag>
      args: [warden, policy, rotate-gateway-ca, --state, /var/lib/warden/policy]
      volumeMounts: [{name: state, mountPath: /var/lib/warden/policy}]
  volumes:
    - name: state
      persistentVolumeClaim: {claimName: warden-policy-state}
EOF
kubectl -n warden wait --for=jsonpath='{.status.phase}'=Succeeded pod/warden-rotate-ca --timeout=120s
kubectl -n warden delete pod warden-rotate-ca
kubectl -n warden scale deploy/warden-policy --replicas=1
```

Bindings established before the rotation are re-verified when the service
is back; guests that fetched the new bundle keep working, and a guest that
replaced its own trust file (running `update-ca-certificates` as root
inside the sandbox) is only affected until its next pod.

**TLS rotation between the services.** The transport reads each service's
certificate and key at every handshake, and the CA when a listener starts,
so:

- With cert-manager, leaf renewals (`tls.certManager.renewBefore`) need no
  action. When the CA itself is renewed, restart the four Deployments so
  the listeners load the new CA:

  ```sh
  kubectl -n warden rollout restart deploy/warden-policy deploy/warden-runner deploy/warden-chat deploy/warden-edge
  ```

- With the bootstrap Job, the four Secrets share a CA that lasts ten times
  `tls.bootstrapJob.days`. To re-issue, delete all four and run
  `helm upgrade`; the pre-upgrade hook issues a new CA and certificates,
  and the upgrade rolls the pods:

  ```sh
  kubectl -n warden delete secret warden-policy-tls warden-runner-tls warden-chat-tls warden-edge-tls
  helm upgrade warden oci://ghcr.io/monaddle-too/charts/warden --version <chart version> -n warden -f values.yaml
  ```

  Deleting only some of them makes the Job fail on purpose.

**Backups.** Back up the PVCs with the services stopped
(`kubectl -n warden scale deploy --all --replicas=0`) using whatever
snapshot or copy mechanism your StorageClass has. What each holds:

| PVC | Namespace | Content |
|---|---|---|
| `warden-policy-state` | release | `bindings.json`, `runtime-identities.json`, `sandboxes/<digest>/`, `sharing.sqlite`, `google.sqlite` (the Google Docs connection), `egress.json` (the Admin console's network-access choice), `gateway-ca/` (the gateway CA key) |
| `warden-runner-state` | release | `managed-v2.json` and the sandbox registry |
| `warden-app-state` | release | `chats.json` (every chat and transcript) |
| `warden-edge-state` | release | `logins.json` (the Google sign-in ledger); in owner mode `endpoint.json` (the launch capability; regenerated at start) |
| one per sandbox, labelled `warden.monaddle.com/sandbox=<runtime name>` | sandbox | `/home/agent` of that sandbox (the suite's stop/resume row proves it persists) |

The provider logins are Secrets, not PVC content; export them with
`kubectl get secret -o yaml` or keep the files you created them from. The
TLS Secrets can always be re-issued. Do not restore a policy PVC without
its runner and app PVCs from the same moment: the runtime identity pins in
the policy state must agree with the runner's registry.

**Sizing the sandbox namespace.** The ResourceQuota allows
`maxRunning + warmSpares + extraPods` pods, that many times
`memoryMB + overhead.memoryMi` of memory and `cpuMillis +
overhead.cpuMillis` of CPU, `maxRunning + warmSpares + keepStopped +
extraPVCs` workspace PVCs of `workspaceGi` each, and no Services or
Secrets; the LimitRange makes every sandbox container exactly `memoryMB`
and `cpuMillis`. Change these in values, not on the objects.

## The persisted set

A sandbox's persisted set is exactly `/home/agent`, the agent's home and
working directory, on its workspace PVC. Everything else in the guest is
image state: package installs (`sudo apt-get install …`), files under `/opt`
or `/usr`, `/tmp` (an emptyDir) and the trust bundle all come back fresh
with the next pod. "Stop" deletes the pod and keeps the PVC; "resume"
creates a new pod on the same PVC (a new generation, with a new pod UID the
verifier pins afresh, while the PVC UID is the stable identity); "remove"
deletes the PVC; `sandboxes.keepStopped` bounds how many stopped workspaces
are retained. Chat forks clone the PVC where the StorageClass supports it
and copy it otherwise. The base image's entrypoint takes ownership of an
empty, root-owned volume and seeds it from `/etc/skel` on first start.

This is narrower than SBX, where the whole root filesystem persists, and it
is a decision, not a footnote: the agent's system prompt states that
package installs outside the home directory do not survive a stop, so an
agent that needs a tool across sessions installs it under its home (a
`venv`, a user-local npm prefix) or reinstalls it. Anything an agent leaves
under `/home/agent` (including shell startup files) does persist and is
re-executed on the next session, which is the same as on SBX.

## Threat-model delta against SBX

The bar is the adversarial matrix in `docs/sbx-integration-plan.md`; the
adversary has root in the guest. Compared with the SBX shapes:

**The same.**

- Gateway-only egress. The sandbox can reach exactly one destination, the
  gateway, and only while its egress label is present; the gateway decides
  per request, injects credentials only for approved operations, and
  refuses everything else. Removing the proxy environment, IP literals,
  other ports, IPv6, DNS to anything, raw TCP and `CONNECT` to non-HTTP
  fail the same way.
- Brokered credentials. The guest holds only placeholders
  (`WARDEN_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`); the real logins are read
  only by the policy service and swapped in on the way to the provider.
- The verifier and the trust model: provisional trust once the gateway is
  up and egress is granted, background full verification, synchronous
  re-verification after a failure, a failed check revoking leases and
  removing egress. The per-sandbox facts are control-plane facts instead of
  `sbx inspect` output.
- The edge, the preview session model and the preview isolation audit.

**Weaker.**

- On the gVisor tier the boundary is a userspace kernel, not a microVM.
  Root in the guest is root of the sentry's view of the world; a sentry
  escape is a node compromise. Codex also runs without its inner sandbox
  there. The Kata tier restores the microVM boundary (and the inner
  sandbox) at the cost of KVM and slower starts.
- Cluster admin can read everything. Anyone who can read Secrets in the
  release namespace has the provider logins; anyone who can read the PVCs
  (or is root on a node) has the gateway CA key, the chats and every
  workspace. On the Mac and OVH the equivalent is root on one host; a
  cluster usually has more principals with that reach, and the node's
  kubelet is one of them.
- In-cluster lateral reach is bounded by NetworkPolicy, not by the absence
  of a route. An SBX guest had one gateway and no other network; a sandbox
  pod is on the pod network with the API server, cluster DNS, every other
  pod, the node and the cloud metadata address one hop away, and only the
  default-deny policy keeps them unreachable. If the CNI stops enforcing
  (misconfiguration, an upgrade), the canary fact fails and sandboxes are
  refused; it does not fail open, but the enforcement point has moved from
  the host into the CNI.
- Source-address spoofing depends on the CNI. The spike could not prove the
  CNI drops spoofed frames, so the gateway's identity is the binding
  credential and the source-pod check is defence in depth only. The
  credential is minted per binding and delivered only to that pod's launch
  environment; a guest presenting another binding's credential, or
  forging another pod's address (CNI-dependent), is the row to keep
  testing. The policy service follows the sandbox pods and the shared
  gateway honours a credential only from the address of the one live pod
  running that binding's sandbox (403 otherwise, no challenge); the
  `other-binding-credential` row of the suite proves it on gVisor. What
  the check cannot cover is a CNI that lets a pod forge another pod's
  address, which is why the credential stays the authority.
- The runner may reach every TCP port of every sandbox pod (one static
  ingress policy), where SBX published one port per approval. Only the
  runner's preview proxy is admitted, and it dials only published ports.
  The chat reaches that proxy over mutual TLS on the runner's preview
  port (`https://warden-runner:7446/<publication ID>/…`, the chat's
  certificate presented, the runner admitting only `warden-chat`) where
  the single-host shapes use a loopback listener per publication.

**Stronger.**

- Services authenticate to each other. Every control connection is mutual
  TLS with a per-service certificate and the peer identity checked at the
  handshake; the chat accepts principal headers only from the edge's
  certificate. On OVH the three services share a state volume, the host
  network and Unix sockets, and the edge reads a capability file.
- The edge shares nothing with the policy service: no volume, no loopback,
  no ServiceAccount token. The internet-facing component and the component
  holding the provider logins and the gateway CA key are separate pods with
  a NetworkPolicy between them that admits exactly edge to chat.
- Least privilege on the API: the runner can touch pods and PVCs in the
  sandbox namespace only; the policy service can label and inspect them,
  read the policies, write the trust ConfigMap and the named provider
  Secrets, and nothing else; the chat and the edge have no token at all;
  sandbox pods have no ServiceAccount token (`automountServiceAccountToken:
  false`, enforced by admission).
- The admission policy makes the sandbox shape a property of the namespace,
  not of the runner's good behaviour: a pod without the RuntimeClass, with a
  host namespace, privileged, with a hostPath or projected volume, or
  without the Warden label is refused whoever creates it.
- Image state does not persist. A persistent implant outside `/home/agent`
  does not survive a stop, and every generation starts from the pinned
  image digest the verifier checks.

**Adversarial rows the Kubernetes suite runs** (plan, work item 8;
`chat/tests/k8s`, Go with `-tags k8s` against `WARDEN_K8S_KUBECONFIG`,
run by `scripts/k8s-dev.sh test` — see "Development"). Verified on the
gVisor tier of the dev VM (the "Development" section lists each row and
its result); the Kata tier runs the same suite where KVM is real. The
test, not the verifier, execs into a sandbox pod and runs, against
controlled destinations with unique markers:

| Matrix row (`docs/sbx-integration-plan.md`) | Kubernetes rows |
|---|---|
| Approved dependency GET and AI HTTPS | A Codex turn and a Claude turn through the gateway; a published preview through the runner proxy and the edge |
| Unset proxy env, custom socket client | Direct egress with the proxy variables unset; `no_proxy=*` |
| Forge headers, use another gateway port, reuse a decision | Wrong or missing proxy credentials; another binding's credentials (must be refused and attributed to neither) |
| IPv4 literal; alternative ports | IP literal to the gateway's port and to external addresses; other ports on the gateway address |
| IPv6 | IPv6 literals and routes from the pod |
| DNS on 53, DoH/DoT | Cluster DNS on 53; external 53; DoH to a public resolver |
| SSH, opaque TCP, non-HTTP bytes | `CONNECT` to a non-HTTP destination; raw TCP through the gateway port |
| Drop or change the proxy CA | The trust bundle mount replaced or removed inside the guest: TLS fails, no direct fallback |
| Host gateway/control/app ports, LAN | The API server, cluster DNS, the metadata address, the node, another sandbox's preview port, the runner's and the chat's control ports |
| Guest root modifies routes/firewall | A pod without the egress label reaches nothing; routes and `iptables` changes inside the guest change nothing outside it |
| Restart worker, gateway, daemon with sessions | Restart of the policy Deployment with live bindings: the gateway address is stable (a Service IP), leases are re-established, no binding widens |

Each row records the tier it ran under. Rows about SBX daemon settings, the
`shell-docker` template's Docker and host loopback aliases do not apply
here; rows about the edge and previews are shared with the other shapes.

## Development

The dev loop is a Lima VM running k3s with both tiers on an Apple Silicon
Mac (plan, decision 15 and appendix B). Everything is in
`deploy/k8s/dev/lima.yaml`, `deploy/k8s/dev/values.yaml` and
`scripts/k8s-dev.sh`; the step 0 probes are in `deploy/k8s/dev/spike/`.

```sh
brew install lima kubectl helm
scripts/k8s-dev.sh up                 # create or start the VM; prints the KUBECONFIG export
export KUBECONFIG="$(scripts/k8s-dev.sh kubeconfig)"
kubectl get runtimeclasses            # gvisor, and kata-qemu when /dev/kvm exists in the VM
```

`up` creates the VM from `lima.yaml` (Ubuntu 24.04, `vmType: vz`,
`nestedVirtualization: true`, 8 CPUs, 16 GiB, 100 GiB, the workspace
mounted read-only at the same path), whose provisioning installs the
pinned gVisor release (`runsc`, the shim and the `gvisor-bin/` sidecars, on
the systrap platform), k3s without Traefik and with the embedded network
policy controller, and buildkitd plus nerdctl on k3s's containerd. It then
applies the `gvisor` RuntimeClass and, when the VM has `/dev/kvm`, installs
kata-deploy 4.2.0 with the QEMU shim only. The two node-side fixes found in
step 0 are part of it:

- `/etc/containerd/runsc.toml` sets `allow-suid = "true"` (with the systrap
  platform and shim logging under `/var/log/runsc`), so the guest's
  passwordless `sudo` works under gVisor.
- `/opt/kata/share/defaults/kata-containers/runtimes/qemu/config.d/90-warden-nested-virt.toml`
  sets `cpu_features = ""`, because nested virtualization on Apple Silicon
  exposes no PMU and QEMU rejects Kata's default `pmu=off`.

Build the images straight into k3s's containerd, so nothing is pushed or
pulled:

```sh
scripts/k8s-dev.sh build-images
```

This builds the web UI with pnpm and the linux/arm64 binary on the Mac
(`GOPROXY=off`, as `scripts/release.sh` does), then inside the VM runs
`deploy/guest/build-base.sh --k3s` for `warden-guest-base:<rev>` and
`nerdctl build -f deploy/chat/Dockerfile` for `warden:<rev>`, both also
tagged `dev`. buildkitd runs with the containerd worker in the `k8s.io`
namespace, which is where the kubelet looks, and the dev values set
`pullPolicy: Never`. `build-base.sh` prints the image's manifest digest;
put it in `deploy/k8s/dev/values.yaml` as `guestImage.digest` after a
rebuild, because the pinned digest is what the runner and the verifier
use, not the tag.

Deploy with the dev values (gVisor tier, owner sign-in, loopback previews,
the `local-path` StorageClass, small PVCs, TLS from the bootstrap Job):

```sh
scripts/k8s-dev.sh deploy               # helm upgrade --install warden … -n warden --create-namespace -f deploy/k8s/dev/values.yaml
kubectl -n warden logs deploy/warden-edge | grep 'launch URL'   # the owner capability, minted at every edge start
```

Only the bootstrap Job's kubectl image is pulled from Docker Hub. The dev
values make the edge a `LoadBalancer` Service on port 28781: k3s's service
load balancer binds it on the node and Lima forwards node ports to the
Mac's loopback, so the app is at `http://127.0.0.1:28781` and `*.localhost`
previews need no port-forward. 28781, not 18781, because a local
`warden start` on the same Mac already holds 18781 and Lima would silently
lose the race for it. (With a ClusterIP edge, `kubectl -n warden
port-forward svc/warden-edge 28781:28781` does the same.)

Other subcommands: `scripts/k8s-dev.sh shell [cmd]` for a shell in the VM,
`test` for the end-to-end suite below, `down` to stop the VM with state
kept, `delete` to remove it. The VM is disposable; everything that matters
is in the repository and the values.

### The end-to-end suite

`chat/tests/k8s` (Go, build tag `k8s`; plan, work item 8) drives the
deployed release the way the browser and `warden chat` do, then attacks it
from inside its own sandbox pods. It installs nothing: the precondition is
`scripts/k8s-dev.sh up`, `build-images` and `deploy`, with the edge
reachable on the Mac's loopback as above.

```sh
scripts/k8s-dev.sh test                                    # the whole suite, about ten minutes on the dev VM
scripts/k8s-dev.sh test -run TestKubernetes/Adversarial    # one part; subtests create what they need on first use
cd chat && WARDEN_K8S_KUBECONFIG="$(../scripts/k8s-dev.sh kubeconfig)" GOPROXY=off GOFLAGS=-mod=mod \
  go test -tags k8s ./tests/k8s/ -run TestKubernetes -v -count=1      # the same by hand
```

`WARDEN_K8S_KUBECONFIG` is required (without it the package skips, so
`go test ./...` never needs a cluster); `WARDEN_K8S_EDGE_URL` (default
`http://127.0.0.1:28781`), `WARDEN_K8S_NAMESPACE` (`warden`) and
`WARDEN_K8S_SANDBOX_NAMESPACE` (`warden-sandboxes`) point it at another
release. The suite reads the release's `warden-config` ConfigMap for the
tier, RuntimeClass, image digest and gateway, reads the owner capability
from the edge pod's endpoint file over `pods/exec`, and talks to the chat
API through the edge with that bearer. It creates its own chats (titled
`k8s suite <provider> <time>`, one workspace per provider) and deletes
their workspaces and its own pods when it ends, whatever the outcome; it
waits for `sandboxes.maxRunning` capacity rather than stopping anyone
else's workspace (stopping its own idle one to make room), and it needs
two resident sandboxes of its own for the cross-sandbox rows. A published
preview keeps a workspace resident, so another person's previewed
workspace holds a slot until it is revoked.

What it asserts, in order (`-run TestKubernetes/<name>` runs one):

- `CodexTurn`, `ClaudeTurn`: a turn on a fresh chat of each provider runs
  `id -u; uname -r` in the guest and answers with uid 1000 and the tier's
  kernel (`gvisor` in the release name on the gVisor tier); the pod's
  `runtimeClassName` is the configured one and its `imageID` is the pinned
  digest.
- `Preview`: the Codex agent serves a page with a random marker from the
  workspace on `0.0.0.0:8080` and calls `preview_attach`; the suite
  approves the `warden/ports/bind` request as the owner, fetches
  `http://<binding>.localhost:28781/` through the edge's sign-in redirects
  with the owner's cookie session (the page came through the runner's
  mTLS preview listener) and checks the marker; an anonymous request gets
  the sign-in redirect, not the page.
- `StopResume`: after a turn wrote a marker file, `environments/{id}/stop`
  removes the pod (same name on resume, new UID, the claim kept) and the
  next turn reads the file back.
- `Adversarial/*`: the rows of the table in "Threat-model delta", from a
  test-owned exec in the Codex chat's pod as the guest account, each
  recorded with the RuntimeClass it ran under and each with its expected
  refusal asserted: direct egress with no proxy variables, `no_proxy=*`
  and IP destinations; missing and wrong proxy credentials (407 with
  `Proxy-Authenticate`) and a foreign bearer on the provider route (401);
  IP literals through the gateway and other ports on an allowed host
  (403); IPv6 (no global address, literals fail, the gateway refuses
  them); DNS on 53 to the cluster resolver and public resolvers, DoT, the
  system resolver and DoH through the gateway (403); `CONNECT` to a
  non-443 port and a plain-HTTP tunnel (403), a disallowed method on an
  allowed host (403) and SSH bytes on the gateway port (400, not
  forwarded); the trust bundle without the gateway CA (curl exit 60, no
  fallback) and `--cacert /dev/null`; the API server, cluster DNS, the
  metadata address, the node's API, kubelet and SSH ports and the
  release's control and preview ports (all unreachable); another
  binding's credential presented from this pod (its owner's pod is the
  positive control); another sandbox's listener by pod IP both ways; a
  pod without the egress label (created by the suite under the
  RuntimeClass with the guest image: it reaches neither the gateway nor
  anything else, and a pod without the RuntimeClass is refused by
  admission). A positive control (an approved dependency GET through the
  gateway) runs first, so a row cannot pass because the lease was absent.
- `PolicyRestart`: with the Codex chat's binding live, the policy
  Deployment is restarted (the other suite workspace is stopped first so
  the new pod's canary proof fits the namespace quota). The gateway
  Service IP is unchanged across the roll and no binding widens. The live
  run is fail-closed by design — while the policy service is down the
  runner's periodic lease renewal fails, so it ends the run and stops the
  sandbox rather than let a guest keep egress it can no longer verify — so
  the sandbox does not survive the restart with its binding intact. The row
  proves the binding re-establishes cleanly on the next turn (the sandbox
  resumes, re-registers and gets a fresh credential at the same gateway
  address) and that the pre-restart credential no longer works.

The `other-binding-credential` row first found that the shared gateway
authenticated by the credential alone; decision 4's source-pod check has
been added since (the inspector's pod watch feeds it) and the row passes:
a valid credential presented from another sandbox's pod is refused with
403 and no upstream connection.

The suite prints one line per row at the end (`row <name> PASS|FAIL under
tier …`) and names any expected row that did not run.

Chart checks, no cluster needed:

```sh
deploy/helm/warden/test.sh              # helm lint, then helm template against each testdata/values-*.yaml and diff with the golden
deploy/helm/warden/test.sh --update     # rewrite the goldens; review the diff before committing
go -C chat test ./internal/config -run Helm   # the rendered warden.json's shape (skips without helm)
```

The step 0 probes (`deploy/k8s/dev/spike/spike.sh`, with `KUBECONFIG` set)
apply hand-written namespaces, policies, a stand-in gateway and sandbox
pods per tier, and print the egress and ingress table, the label flip, the
address-binding check, the ConfigMap refresh time and the pod start time
per tier. They are the evidence behind "Spike results" in the plan and are
not used by Warden itself.

### A real cluster: GKE Autopilot with public previews

The dev VM cannot exercise public previews (a public address, a domain,
a browser certificate, Google sign-in). `deploy/k8s/gke/` and
`scripts/k8s-gke.sh` run the chart on a GKE Autopilot cluster for that,
as a test cluster that costs little while nothing runs: Autopilot bills
pod requests only (the four service pods on Spot capacity are about $8 a
month, the GKE free tier covers one cluster's management fee), GKE Sandbox
is the gVisor tier with nothing to install on nodes, Dataplane V2 enforces
the NetworkPolicies, the ingress controller's load balancer is about $18 a
month while it exists, and a sandbox costs about five cents an hour while
it runs. `sandboxes.warmSpares` is 0 in `deploy/k8s/gke/values.yaml` for
that reason (a warm spare is billed around the clock), so the first
sandbox after an idle period waits for a GKE Sandbox node.

The domain is one delegated zone: the app is `https://<domain>/` and
previews are `https://<binding-id>.<domain>/`, so `auth.publicURL` and
`previews.hostSuffix` are the same name, one wildcard certificate covers
both, and the parent domain gets NS records once. cert-manager solves
Let's Encrypt's DNS-01 challenge in Cloud DNS as a Google service account
through Workload Identity; the issuers are
`deploy/k8s/gke/cluster-issuer.yaml` (staging first, then production).

What the operator supplies, in `deploy/k8s/gke/env` (copy `env.example`;
the file is git-ignored):

1. A Google Cloud project with billing, `gcloud` signed in to it
   (`brew install --cask google-cloud-sdk`, `gcloud auth login`,
   `gcloud components install gke-gcloud-auth-plugin`).
2. A hostname under a domain you control, `WARDEN_GKE_DOMAIN`; after `up`
   you add the NS records `dns` prints at the parent domain's DNS host
   (one NS record per name server, host = the delegated label, for
   example `cloud.warden` for `cloud.warden.monaddle.com`; Squarespace
   asks for a fresh Google sign-in before it lets you edit DNS, and the
   delegation was visible at Google's resolver within a minute).
3. A Google sign-in web client: in the project's APIs & Services →
   Credentials, an OAuth client ID of type Web application with
   `https://<domain>` as an authorized JavaScript origin (the built-in
   Desktop client is for Docs, not sign-in); its ID is
   `WARDEN_GKE_CLIENT_ID`.
4. The provider logins: `secrets` copies the three Secrets from the dev
   cluster; otherwise create them as in "The release namespace and the
   provider Secrets".

Then:

```sh
scripts/k8s-gke.sh up             # cluster, zone, registry, ingress-nginx, cert-manager, issuers (about ten minutes)
scripts/k8s-gke.sh dns            # the NS records to add at the parent; A records for <domain> and *.<domain>
scripts/k8s-gke.sh build-images   # linux/amd64 images, built in the dev VM under emulation, pushed to Artifact Registry
scripts/k8s-gke.sh secrets        # provider Secrets from the dev cluster
scripts/k8s-gke.sh deploy         # helm upgrade --install with the values, the domain, the client and the owner
scripts/k8s-gke.sh status         # pods, certificate, ingress, delegation
export KUBECONFIG="$(scripts/k8s-gke.sh kubeconfig)"   # dist/gke/kubeconfig, not ~/.kube/config
```

`build-images` exists because GKE's nodes are x86 and the dev images are
arm64: the Mac cross-compiles the binary and the VM builds both images
for `linux/amd64` under QEMU user emulation (the binfmt image's QEMU,
registered on first use; Ubuntu's own qemu-user-static segfaults in the
runtimes' installer), about a minute for the server image and two for the
guest base image, pushes them to the project's Artifact Registry with the
signed-in account's access token and records the guest image's registry
manifest digest in `dist/gke/` for `deploy`. The images cannot be
smoke-tested under emulation (Bun aborts there); the cluster is their
first run. The published multi-architecture images from `release.yml` do
the same job once a tag exists (`image.repository` and
`guestImage.digest` for that platform in the values).

Open `https://<domain>/` and sign in as `WARDEN_GKE_OWNER` once `status`
shows the certificate ready (with `WARDEN_GKE_ACME=staging` the browser
warns about the untrusted issuer; switch to `production` and `deploy`
again when the delegation is proven). The end-to-end suite runs against
this cluster too, with the public address instead of the loopback edge:

```sh
WARDEN_K8S_KUBECONFIG="$(scripts/k8s-gke.sh kubeconfig)" WARDEN_K8S_EDGE_URL="https://<domain>" \
  GOPROXY=off GOFLAGS=-mod=mod go -C chat test -tags k8s ./tests/k8s/ -run TestKubernetes -v -count=1
```

`park` scales everything to zero (the disks and the load balancer keep
costing), `unpark` brings it back, `down` deletes the cluster with its
disks, and `delete` also removes the zone, the registry and the service
account. Two things to know before the first run: the policy service's
canary proof has a three-minute budget, and on a cold Autopilot cluster
the two canary pods first wait for a GKE Sandbox node (one to two
minutes), so the first start after `up` or `unpark` is the slow one; and
GKE's `gvisor` RuntimeClass (handler `gvisor`) is in the tier's allowlist,
so `runtime.runtimeClassName` stays empty.
