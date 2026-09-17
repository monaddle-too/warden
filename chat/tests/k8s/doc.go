// Package k8s is the end-to-end suite of the Kubernetes shape
// (docs/warden-kubernetes-plan.md, work item 8): it drives an installed
// release through the edge's HTTP API exactly as the browser and `warden
// chat` do, and runs the adversarial networking rows of
// docs/sbx-integration-plan.md from a test-owned exec inside sandbox pods.
//
// The suite is behind the k8s build tag and skips unless
// WARDEN_K8S_KUBECONFIG names a kubeconfig, so the ordinary unit run never
// needs a cluster:
//
//	scripts/k8s-dev.sh test                # the dev VM: sets the kubeconfig and runs the suite
//	cd chat && WARDEN_K8S_KUBECONFIG=… GOPROXY=off GOFLAGS=-mod=mod go test -tags k8s ./tests/k8s/ -run . -v -count=1
//
// Environment: WARDEN_K8S_KUBECONFIG (required), WARDEN_K8S_EDGE_URL
// (default http://127.0.0.1:28781, the edge as the operator's browser
// reaches it), WARDEN_K8S_NAMESPACE (default warden) and
// WARDEN_K8S_SANDBOX_NAMESPACE (default warden-sandboxes). The release must
// already be installed and ready (scripts/k8s-dev.sh up, build-images and
// deploy); the suite installs nothing, reads the owner capability from the
// edge pod's endpoint file over pods/exec, creates its own chats (titled
// "k8s suite …") and deletes everything it created, even on failure. It
// never touches other namespaces or other people's chats.
package k8s
