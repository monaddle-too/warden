// Package kube is the policy service's Kubernetes enforcement
// (docs/warden-kubernetes-plan.md, work item 5): the SandboxInspector of
// the kubernetes runtime kind, the guest trust-bundle publisher and the
// Secret-backed credential store.
//
// Inspector proves the cluster-wide invariants of decisions 11 and 12 (the
// hardened sandbox Namespace, the three static NetworkPolicies, the
// admission policy and its binding, the RuntimeClass of the tier, and the
// canary proof that NetworkPolicy is enforced) and, per binding, the
// control-plane facts of decisions 2, 3 and 4: the pod's spec and labels,
// the image digest, the workspace volume identity with the pod pinned per
// generation, and the egress label with only the static policies selecting
// the pod. It grants and denies egress by setting and removing that label.
// Nothing runs inside a sandbox on the verifier's behalf.
//
// TrustPublisher assembles the guest trust bundle (the policy container's
// system CAs plus the gateway CA, decision 9) and updates the ConfigMap the
// chart created in the sandbox namespace; it never creates it.
//
// SecretCredentials is the CredentialStore of decision 8 over the provider
// login Secrets in the core namespace, one key per file of the sbx shapes.
package kube
