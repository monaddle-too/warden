package kube

// Labels and annotations on sandbox pods and the namespace, shared with the
// runner's pod spec (work item 4) and the chart.
const (
	// LabelSandbox names the runtime on its pod (the binding's runtimeName);
	// the admission policy requires it on every pod of the namespace.
	LabelSandbox = "warden.monaddle.com/sandbox"
	// LabelBinding carries the binding digest the pod was created for
	// (policy.BindingDigest). Checked when present.
	LabelBinding = "warden.monaddle.com/binding"
	// LabelEgress is the egress switch of decision 4: the static
	// gateway-egress NetworkPolicy selects LabelEgress=EgressGateway.
	LabelEgress = "warden.monaddle.com/egress"
	// EgressGateway is the only value LabelEgress may hold.
	EgressGateway = "gateway"
	// LabelCanary marks the two canary pods of decision 12 with their role
	// (CanaryDeny or CanaryGateway); a canary is never a sandbox.
	LabelCanary = "warden.monaddle.com/canary"
	// LabelRole is the sandbox namespace's role label the chart sets.
	LabelRole = "warden.monaddle.com/role"
	// RoleSandboxes is LabelRole's value on the sandbox namespace.
	RoleSandboxes = "sandboxes"
	// LabelComponent is the chart's component label; the policy pod carries
	// ComponentPolicy and the gateway-egress policy selects it.
	LabelComponent  = "warden.monaddle.com/component"
	ComponentPolicy = "policy"
	ComponentRunner = "runner"
	// AnnotationGeneration is the generation the runner created the pod for
	// (identity["generation"]). Checked when present.
	AnnotationGeneration = "warden.monaddle.com/generation"

	// PSAEnforceLabel is Pod Security Admission's enforce label on the
	// namespace; PSABaseline is the level the chart sets (decision 11).
	PSAEnforceLabel = "pod-security.kubernetes.io/enforce"
	PSABaseline     = "baseline"
	// NamespaceNameLabel is the label every namespace carries with its
	// name, which the static policies' namespace selectors use.
	NamespaceNameLabel = "kubernetes.io/metadata.name"
)

// The static NetworkPolicies of the sandbox namespace, by name, as the
// chart creates them (deploy/helm/warden/templates/sandbox-networkpolicies.yaml).
const (
	PolicyDefaultDeny   = "default-deny"
	PolicyGatewayEgress = "gateway-egress"
	PolicyRunnerIngress = "runner-ingress"
)

// StaticPolicies are the only NetworkPolicies that may select a sandbox pod.
var StaticPolicies = map[string]bool{PolicyDefaultDeny: true, PolicyGatewayEgress: true, PolicyRunnerIngress: true}

// Tiers and their RuntimeClass handler allow-lists (decision 2).
const (
	TierGVisor = "gvisor"
	TierKata   = "kata"
)

// DefaultHandlers is the handler allow-list per tier.
var DefaultHandlers = map[string][]string{
	TierGVisor: {"runsc", "gvisor"},
	TierKata:   {"kata", "kata-qemu", "kata-clh", "kata-fc", "kata-qemu-runtime-rs"},
}
