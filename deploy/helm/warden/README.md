# warden (Helm chart)

Warden on Kubernetes: the policy broker, runner, chat and edge as four
Deployments over mutual TLS, agent sandboxes as pods under a RuntimeClass
(Kata Containers or gVisor) in a hardened namespace, egress through the
policy service's gateway only. The operator guide is
[docs/warden-kubernetes.md](../../../docs/warden-kubernetes.md); the design
is [docs/warden-kubernetes-plan.md](../../../docs/warden-kubernetes-plan.md).

## Install

Create the provider login Secrets in the release namespace first (the chart
never contains a credential; see the guide for the exact `kubectl create
secret` shapes), then:

```sh
helm install warden deploy/helm/warden -n warden --create-namespace \
  --set guestImage.digest=sha256:<platform manifest digest> \
  --set runtime.tier=gvisor \
  --set tls.bootstrap=true
```

`guestImage.digest` is required; the chart refuses to render without it.
The NOTES printed after the install give the port-forward, first-login and
Secret commands for the values you chose. Requires Kubernetes 1.30 or
later, a CNI that enforces NetworkPolicy, and a RuntimeClass for the
selected tier (`runtime.createRuntimeClasses` creates one when the cluster
does not own it).

## Values

Every value is documented in [values.yaml](values.yaml), and the guide's
"Values reference" lists each top-level key with its default. The main
switches: `runtime.tier` (`gvisor` or `kata`), `auth.mode` (`owner` or
`google`), `previews.mode` (`loopback` or `public`), `tls.bootstrap` or
`tls.certManager.enabled`, `storage.className`, and `sandboxes.*` for
sizing. One release per namespace: the Services, ServiceAccounts and TLS
Secrets have fixed names because the certificates and `warden.json` use
them.

## Dev values

[deploy/k8s/dev/values.yaml](../../k8s/dev/values.yaml) is the shape of the
Lima dev cluster (`scripts/k8s-dev.sh`): images built into k3s's containerd
and never pulled, gVisor tier, owner sign-in, loopback previews, the
`local-path` StorageClass, small PVCs, TLS from the bootstrap Job.

```sh
scripts/k8s-dev.sh deploy    # helm upgrade --install warden deploy/helm/warden -n warden --create-namespace -f deploy/k8s/dev/values.yaml
```

## Tests

```sh
deploy/helm/warden/test.sh             # helm lint, then helm template per testdata/values-*.yaml diffed against <case>.golden.yaml
deploy/helm/warden/test.sh --update    # rewrite the goldens; review the diff before committing
```

Needs `helm` on `PATH`, no cluster. The four cases cover both tiers with
both auth and preview modes (`gvisor-loopback`, `gvisor-public`,
`kata-loopback`, `kata-public`). `go -C chat test ./internal/config -run
Helm` checks the rendered `warden.json`'s shape (skips without helm).

## Layout

| Template | Objects |
|---|---|
| `sandbox-namespace.yaml` | The sandbox Namespace (PSA baseline) and the empty trust ConfigMap `warden-guest-trust` the policy service writes |
| `sandbox-networkpolicies.yaml` | Default deny; gateway egress by the `warden.monaddle.com/egress=gateway` label; runner ingress |
| `sandbox-admission.yaml` | The ValidatingAdmissionPolicy and binding that fix the sandbox pod shape |
| `sandbox-quota.yaml` | ResourceQuota and LimitRange from `sandboxes.*` |
| `rbac.yaml` | Four ServiceAccounts; the runner's and the policy service's Roles in the sandbox namespace; the policy service's credentials Role; the optional cluster-facts ClusterRole |
| `configmap.yaml` | `warden-config` with the rendered `warden.json` (`_helpers.tpl`, `warden.config`) |
| `pvcs.yaml` | `warden-{policy,runner,app,edge}-state` |
| `deployments.yaml` | The four Deployments (`Recreate`, non-root, read-only root filesystem) |
| `services.yaml` | `warden-policy`, `warden-gateway`, `warden-runner`, `warden-chat`, `warden-edge` |
| `networkpolicies.yaml` | Core namespace default deny and one policy per service |
| `tls-bootstrap.yaml` | The pre-install/pre-upgrade Job issuing the four mTLS Secrets without cert-manager |
| `tls-certmanager.yaml` | The Issuers and Certificates with cert-manager |
| `ingress.yaml` | The edge Ingress in public preview mode |
| `runtimeclass.yaml` | The optional RuntimeClass of the selected tier |
| `NOTES.txt` | Post-install instructions |
