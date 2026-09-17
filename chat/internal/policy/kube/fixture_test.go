package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

const (
	testNamespace = "warden-sandboxes"
	testCore      = "warden"
	testClass     = "gvisor"
	testDigest    = "sha256:cd77d0ff2af115f3cb700414c7c54840c2c1c87c3322ea0a98c17880ef07b63e"
	testGateway   = "10.43.0.7"
)

// cluster seeds the fake with the chart's objects for a compliant sandbox
// namespace and scripts canaries that report what enforced policies
// would.
func cluster(t *testing.T) *fakeAPI {
	t.Helper()
	f := newFakeAPI(t)
	f.seed(api.Namespaces, "", &api.Namespace{Metadata: api.ObjectMeta{Name: testNamespace, Labels: map[string]string{
		PSAEnforceLabel: PSABaseline, "pod-security.kubernetes.io/warn": "restricted", LabelRole: RoleSandboxes, NamespaceNameLabel: testNamespace,
	}}, Status: api.NamespaceStatus{Phase: "Active"}})
	for _, np := range staticPolicies() {
		f.seed(api.NetworkPolicies, testNamespace, np)
	}
	policy, binding := admission()
	f.seed(api.ValidatingAdmissionPolicies, "", policy)
	f.seed(api.ValidatingAdmissionPolicyBindings, "", binding)
	f.seed(api.RuntimeClasses, "", &api.RuntimeClass{Metadata: api.ObjectMeta{Name: testClass}, Handler: "runsc"})
	f.seed(api.ConfigMaps, testNamespace, &api.ConfigMap{Metadata: api.ObjectMeta{Name: "warden-guest-trust"}})
	scriptCanaries(f, enforcedVerdicts(CanaryDeny), enforcedVerdicts(CanaryGateway))
	return f
}

// enforcedVerdicts are what a canary reports under enforced policies.
func enforcedVerdicts(role string) map[string]string {
	v := map[string]string{"gateway": "closed", "apiserver": "closed", "dns": "closed", "external": "closed"}
	if role == CanaryGateway {
		v["gateway"] = "open"
	}
	return v
}

// scriptCanaries makes every created canary pod end at once with the log
// its role is given.
func scriptCanaries(f *fakeAPI, deny, gateway map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCreate = func(res apiPath, obj map[string]any) {
		if res.resource != "pods" {
			return
		}
		meta := obj["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		role, _ := labels[LabelCanary].(string)
		var verdicts map[string]string
		switch role {
		case CanaryDeny:
			verdicts = deny
		case CanaryGateway:
			verdicts = gateway
		default:
			return
		}
		obj["status"] = map[string]any{"phase": "Succeeded"}
		var b strings.Builder
		for _, probe := range canaryProbes {
			if v, ok := verdicts[probe]; ok {
				b.WriteString("warden-canary " + probe + " " + v + "\n")
			}
		}
		if verdicts["done"] != "no" {
			b.WriteString("warden-canary done\n")
		}
		f.logs[res.objectPath()] = b.String()
	}
}

func staticPolicies() []*api.NetworkPolicy {
	tcp := "TCP"
	port := api.FromInt(7000)
	corePeer := func(component string) api.NetworkPolicyPeer {
		return api.NetworkPolicyPeer{
			NamespaceSelector: &api.LabelSelector{MatchLabels: map[string]string{NamespaceNameLabel: testCore}},
			PodSelector:       &api.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "warden", "app.kubernetes.io/instance": "warden", LabelComponent: component}},
		}
	}
	return []*api.NetworkPolicy{
		{Metadata: api.ObjectMeta{Name: PolicyDefaultDeny}, Spec: api.NetworkPolicySpec{PolicyTypes: []string{"Ingress", "Egress"}}},
		{Metadata: api.ObjectMeta{Name: PolicyGatewayEgress}, Spec: api.NetworkPolicySpec{
			PodSelector: api.LabelSelector{MatchLabels: map[string]string{LabelEgress: EgressGateway}},
			PolicyTypes: []string{"Egress"},
			Egress:      []api.NetworkPolicyEgressRule{{To: []api.NetworkPolicyPeer{corePeer(ComponentPolicy)}, Ports: []api.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}}},
		}},
		{Metadata: api.ObjectMeta{Name: PolicyRunnerIngress}, Spec: api.NetworkPolicySpec{
			PolicyTypes: []string{"Ingress"},
			Ingress:     []api.NetworkPolicyIngressRule{{From: []api.NetworkPolicyPeer{corePeer(ComponentRunner)}, Ports: []api.NetworkPolicyPort{{Protocol: &tcp}}}},
		}},
	}
}

func admission() (*api.ValidatingAdmissionPolicy, *api.ValidatingAdmissionPolicyBinding) {
	fail := "Fail"
	name := "warden-" + testNamespace + "-sandbox-pods"
	policy := &api.ValidatingAdmissionPolicy{Metadata: api.ObjectMeta{Name: name}, Spec: api.ValidatingAdmissionPolicySpec{
		FailurePolicy: &fail,
		MatchConstraints: &api.MatchResources{ResourceRules: []api.NamedRuleWithOperations{{
			APIGroups: []string{""}, APIVersions: []string{"v1"}, Operations: []string{"CREATE", "UPDATE"}, Resources: []string{"pods", "pods/ephemeralcontainers"},
		}}},
		Validations: []api.Validation{
			{Expression: "object.spec.?runtimeClassName.orValue('') == '" + testClass + "'"},
			{Expression: "has(object.spec.automountServiceAccountToken) && object.spec.automountServiceAccountToken == false"},
			{Expression: "has(object.metadata.labels) && '" + LabelSandbox + "' in object.metadata.labels && object.metadata.labels['" + LabelSandbox + "'] != ''"},
			{Expression: "!object.spec.?hostNetwork.orValue(false) && !object.spec.?hostPID.orValue(false) && !object.spec.?hostIPC.orValue(false)"},
			{Expression: "variables.containers.all(c, !c.?securityContext.?privileged.orValue(false))"},
			{Expression: "object.spec.?volumes.orValue([]).all(v, has(v.persistentVolumeClaim) || has(v.configMap) || has(v.emptyDir))"},
		},
	}}
	binding := &api.ValidatingAdmissionPolicyBinding{Metadata: api.ObjectMeta{Name: name}, Spec: api.ValidatingAdmissionPolicyBindingSpec{
		PolicyName: name, ValidationActions: []string{"Deny"},
		MatchResources: &api.MatchResources{NamespaceSelector: &api.LabelSelector{MatchLabels: map[string]string{NamespaceNameLabel: testNamespace}}},
	}}
	return policy, binding
}

// sandboxPod is a compliant sandbox pod as the runner creates it, running
// the pinned image on the workspace claim.
func sandboxPod(name, claim string) *api.Pod {
	return &api.Pod{
		Metadata: api.ObjectMeta{Name: name, Labels: map[string]string{LabelSandbox: name}, Annotations: map[string]string{AnnotationGeneration: "1"}},
		Spec: api.PodSpec{
			RuntimeClassName:             api.String(testClass),
			AutomountServiceAccountToken: api.Bool(false),
			Containers:                   []api.Container{{Name: "guest", Image: "warden-guest-base@" + testDigest}},
			Volumes: []api.Volume{
				{Name: "home", PersistentVolumeClaim: &api.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
				{Name: "trust", ConfigMap: &api.ConfigMapVolumeSource{Name: "warden-guest-trust"}},
				{Name: "tmp", EmptyDir: &api.EmptyDirVolumeSource{}},
			},
		},
		Status: api.PodStatus{Phase: "Running", PodIP: "10.42.0.9", ContainerStatuses: []api.ContainerStatus{{Name: "guest", ImageID: testDigest, Ready: true}}},
	}
}

func seedSandbox(f *fakeAPI, name string) {
	f.seed(api.PersistentVolumeClaims, testNamespace, &api.PersistentVolumeClaim{Metadata: api.ObjectMeta{Name: name + "-home"}})
	f.seed(api.Pods, testNamespace, sandboxPod(name, name+"-home"))
}

func newInspector(t *testing.T, f *fakeAPI, mutate func(*Options)) *Inspector {
	t.Helper()
	o := Options{
		Client: f.client(), Namespace: testNamespace, CoreNamespace: testCore, Tier: TierGVisor, RuntimeClass: testClass,
		GuestImageDigest: testDigest, GatewayPort: 7000, State: t.TempDir(),
		Canary: CanaryOptions{GatewayHost: testGateway, APIServer: "10.43.0.1:443", PollInterval: 10 * time.Millisecond, Timeout: 10 * time.Second},
	}
	if mutate != nil {
		mutate(&o)
	}
	i, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func identity(name string) map[string]string {
	return map[string]string{"projectID": "p1", "sandboxID": "s1", "runtimeName": name, "generation": "1", "principalID": "owner"}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}
