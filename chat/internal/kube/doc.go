// Package kube is Warden's Kubernetes API client: the small surface the
// Kubernetes runtime driver, the policy inspector, the shared gateway's pod
// watch, the trust-bundle publisher and the Secret credential store need,
// on the standard library alone (docs/warden-kubernetes-plan.md, decision
// 6).
//
// Config comes from the pod's service account (InClusterConfig) or a
// kubeconfig file (LoadKubeconfig). Client has the verbs Get, List, Create,
// Update, Delete, Patch (JSON merge patch) and Apply (server-side apply)
// over a Resource and typed structs that hold only the fields Warden reads
// and writes (Pod, PersistentVolumeClaim, Secret, ConfigMap, NetworkPolicy,
// Namespace, RuntimeClass, ValidatingAdmissionPolicy and its binding), or
// the untyped Object. Errors from the server are *StatusError, tested with
// IsNotFound, IsConflict, IsAlreadyExists, IsForbidden and IsGone.
//
// Watch streams one watch; ListWatch keeps a continuous, self-healing view
// of a collection. Exec runs a command over the remote command WebSocket
// protocol with stdin, stdout, stderr and an exit code; Logs streams a
// container's log.
package kube
