package policy

import "context"

// Egress states a SandboxInspector reports for a runtime.
const (
	// EgressDenied: the runtime can reach nothing (the bootstrap state).
	EgressDenied = "denied"
	// EgressGateway: the runtime can reach exactly its gateway endpoint.
	EgressGateway = "gateway"
	// EgressOther: any other state; verification fails on it.
	EgressOther = "other"
)

// RuntimeFacts is what an inspector proved about one runtime, in vocabulary
// no runtime owns: the SBX inspector fills it from `sbx ls/inspect/policy`,
// a Kubernetes inspector from the pod, its labels and the policies that
// select it (docs/warden-kubernetes-plan.md, decisions 2, 3, 4 and 12).
type RuntimeFacts struct {
	// Present reports that the runtime exists; the create phase is proved
	// before it does.
	Present bool
	// Identity is the runtime's stable identity as the inspector pins it
	// (the SBX sandbox UUID; the workspace volume UID later). A changed
	// identity is a violation the inspector reports itself.
	Identity string
	// ImageDigest is the image the runtime runs, already checked against
	// the inspector's allow-list.
	ImageDigest string
	// Tier names the isolation boundary: "sbx", "kata" or "gvisor".
	Tier string
	// Violations are structural findings (an unexpected image, mount, kit,
	// host namespace, ...); any one fails verification with its text.
	Violations []string
	// Egress is the runtime's network state: EgressDenied, EgressGateway
	// (reachable: exactly the endpoint the inspector granted) or EgressOther.
	Egress string
}

// SandboxInspector is the policy service's view of a sandbox runtime. The
// verifier owns the trust model (provisional trust, background and
// synchronous verification, proof caching); the inspector owns the facts
// and the egress switch of one runtime kind. Every method may run a CLI or
// an API call and must not be called with the registry lock held.
type SandboxInspector interface {
	// ClusterFacts proves the runtime-wide invariants (the SBX host's
	// daemon, settings and global policy; a Kubernetes cluster's admission
	// and NetworkPolicy enforcement). The verifier caches a pass.
	ClusterFacts(ctx context.Context) error
	// Facts inspects one runtime named by the binding identity
	// (runtimeName, sandboxID, ...). Egress is probed against the endpoint
	// GrantEgress established for it, so Facts follows GrantEgress in the
	// runtime phase.
	Facts(ctx context.Context, identity map[string]string) (RuntimeFacts, error)
	// GrantEgress moves the runtime from its bootstrap deny to reaching
	// exactly the gateway endpoint, or confirms that state.
	GrantEgress(ctx context.Context, identity map[string]string, gateway GatewayEndpoint) error
	// DenyEgress returns the runtime to its bootstrap deny after a failed
	// verification. Errors are not reported: the runner also stops on a
	// failed readiness, and no success is ever derived from a deny.
	DenyEgress(ctx context.Context, identity map[string]string) error
}
