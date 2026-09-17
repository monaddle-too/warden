package kube

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	api "warden/chat/internal/kube"
	"warden/chat/internal/policy"
)

// testClock is the registry's monotonic clock under test control.
type testClock struct {
	mu  sync.Mutex
	now float64
}

func (c *testClock) read() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(seconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += seconds
}

// sharedGatewayStub stands in for the SharedGateway: one advertised
// address, identity by credential, always healthy.
type sharedGatewayStub struct{}

func (sharedGatewayStub) Bind(b *policy.Binding) (policy.GatewayEndpoint, error) {
	b.GatewayPort = 7000
	b.Endpoint = policy.GatewayEndpoint{Host: testGateway, Port: 7000, BindingID: b.Identity["sandboxID"], Capability: b.Capability}
	return b.Endpoint, nil
}
func (sharedGatewayStub) Healthy(b *policy.Binding) bool { return b.GatewayPort == 7000 }
func (sharedGatewayStub) Close()                         {}

type scenario struct {
	t         *testing.T
	f         *fakeAPI
	clock     *testClock
	registry  *policy.Registry
	inspector *Inspector
	verifier  *policy.RuntimeVerifier
	value     map[string]any
}

// newScenario wires a registry, the verifier and this package's inspector
// over a compliant cluster holding one running sandbox pod, as the policy
// service does for the kubernetes kind.
func newScenario(t *testing.T) *scenario {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := policy.LoadOperations(filepath.Join(root, "vendor", "github-operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, f: cluster(t), clock: &testClock{now: 1000}}
	seedSandbox(s.f, "wc-one")
	s.registry, err = policy.NewRegistry(t.TempDir(), policy.RegistryOptions{Operations: operations, PolicyTemplate: filepath.Join(root, "config", "policy.template.json"), Clock: s.clock.read})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.registry.Close)
	s.registry.Gateways = sharedGatewayStub{}
	s.inspector = newInspector(t, s.f, nil)
	s.verifier = policy.NewRuntimeVerifier(s.registry, s.inspector)
	s.registry.Verifier = s.verifier
	t.Cleanup(s.verifier.WaitIdle)
	s.value = map[string]any{"projectID": "p1", "sandboxID": "s1", "runtimeName": "wc-one", "generation": "1", "chatID": "c1", "runID": "r1", "principalID": "owner"}
	if _, err := s.registry.Register(s.value); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *scenario) ready(phase string) bool {
	s.t.Helper()
	result, err := s.registry.Check(s.value, phase)
	if err != nil {
		s.t.Fatal(err)
	}
	r, _ := result["ready"].(bool)
	return r
}

func (s *scenario) labelled() bool {
	var pod api.Pod
	if !s.f.object(api.Pods, testNamespace, "wc-one", &pod) {
		return false
	}
	return pod.Metadata.Labels[LabelEgress] == EgressGateway
}

// The trust model over the Kubernetes inspector: the chat path sets the
// egress label and trusts provisionally; the background verification
// proves the cluster (canaries) and the pod; a fresh proof is reused; a
// violation denies egress (the label goes) and distrusts the binding until
// a synchronous verification passes; a cluster failure denies every warm
// binding.
func TestVerifierScenarioOverTheKubernetesInspector(t *testing.T) {
	s := newScenario(t)
	if !s.ready("runtime") {
		t.Fatalf("provisional trust refused: %s", s.verifier.LastFailure())
	}
	if !s.labelled() {
		t.Fatal("chat path did not grant egress by label")
	}
	if s.f.count("POST", "/pods") != 0 {
		t.Fatal("chat path ran the canaries")
	}
	s.verifier.WaitIdle()
	if s.verifier.LastFailure() != "" || s.f.count("POST", "/pods") != 2 {
		t.Fatalf("background verification: failure=%q canaries=%d", s.verifier.LastFailure(), s.f.count("POST", "/pods"))
	}
	if !s.ready("runtime") || s.f.count("POST", "/pods") != 2 {
		t.Fatal("fresh proof re-proved the cluster")
	}
	// Begin advertises the shared endpoint with the binding credential.
	if _, err := s.registry.ConfigureProvider(s.value, "synthetic-secret-for-tests-only"); err != nil {
		t.Fatal(err)
	}
	result, err := s.registry.Begin(s.value, false)
	if err != nil || result["ready"] != true {
		t.Fatalf("begin: %v %v", result, err)
	}
	capability := s.registry.Bindings["s1"].Capability
	if result["proxyURL"] != "http://s1:"+capability+"@"+testGateway+":7000" || result["apiKeyPlaceholder"] != "s1."+capability {
		t.Fatalf("advertised: %v", result)
	}
	// The pod stops satisfying decision 2: the next verification fails,
	// denies egress and distrusts the binding.
	var pod api.Pod
	s.f.object(api.Pods, testNamespace, "wc-one", &pod)
	pod.Spec.Containers[0].SecurityContext = &api.SecurityContext{Privileged: api.Bool(true)}
	s.f.put(api.Pods, testNamespace, &pod)
	s.clock.advance(policy.ProofSeconds + 1)
	s.ready("runtime") // provisional again until the background check fails
	s.verifier.WaitIdle()
	if s.ready("runtime") || !strings.Contains(s.verifier.LastFailure(), "privileged") || s.labelled() {
		t.Fatalf("violation accepted: failure=%q labelled=%v", s.verifier.LastFailure(), s.labelled())
	}
	// Distrusted: the synchronous verification fails again, still denied.
	if s.ready("runtime") || s.labelled() {
		t.Fatal("distrusted binding trusted provisionally")
	}
	// Fixed: the synchronous verification passes and egress is granted.
	s.f.object(api.Pods, testNamespace, "wc-one", &pod)
	pod.Spec.Containers[0].SecurityContext = nil
	s.f.put(api.Pods, testNamespace, &pod)
	if !s.ready("runtime") || !s.labelled() {
		t.Fatalf("recovered pod refused: %s", s.verifier.LastFailure())
	}
	// The cluster stops enforcing: the refresh denies the warm binding.
	scriptCanaries(s.f, map[string]string{"gateway": "closed", "apiserver": "closed", "dns": "closed", "external": "open"}, enforcedVerdicts(CanaryGateway))
	s.inspector.Invalidate()
	if s.verifier.RefreshOnce() != 0 || !strings.Contains(s.verifier.LastFailure(), "NetworkPolicy is not enforced") || s.labelled() {
		t.Fatalf("cluster failure: failure=%q labelled=%v", s.verifier.LastFailure(), s.labelled())
	}
	if s.ready("runtime") {
		t.Fatal("ready on a failing cluster")
	}
}

// A pod that is not there fails the runtime phase; the create phase needs
// no pod.
func TestVerifierScenarioAbsentPod(t *testing.T) {
	s := newScenario(t)
	s.f.remove(api.Pods, testNamespace, "wc-one")
	if !s.ready("create") {
		t.Fatalf("create not ready: %s", s.verifier.LastFailure())
	}
	s.verifier.WaitIdle()
	if !s.ready("create") || s.verifier.LastFailure() != "" {
		t.Fatalf("create verification failed: %s", s.verifier.LastFailure())
	}
	if s.ready("runtime") {
		t.Fatal("runtime ready without a pod")
	}
	if s.verifier.LastFailure() != "runtime absent" {
		t.Fatalf("failure: %q", s.verifier.LastFailure())
	}
}

// A resumed sandbox (a new pod, a new generation, the same workspace
// claim) verifies as a new generation; a replaced claim never does.
func TestVerifierScenarioResumeAndReplacedVolume(t *testing.T) {
	s := newScenario(t)
	if !s.ready("runtime") {
		t.Fatal(s.verifier.LastFailure())
	}
	s.verifier.WaitIdle()
	if s.verifier.LastFailure() != "" {
		t.Fatal(s.verifier.LastFailure())
	}
	// Stop: the pod goes; resume: a new pod under generation 2.
	s.f.remove(api.Pods, testNamespace, "wc-one")
	pod := sandboxPod("wc-one", "wc-one-home")
	pod.Metadata.Annotations[AnnotationGeneration] = "2"
	s.f.seed(api.Pods, testNamespace, pod)
	s.value["generation"] = "2"
	if _, err := s.registry.Register(s.value); err != nil {
		t.Fatal(err)
	}
	if !s.ready("runtime") {
		t.Fatal(s.verifier.LastFailure())
	}
	s.verifier.WaitIdle()
	if s.verifier.LastFailure() != "" || !s.labelled() {
		t.Fatalf("resume: %q", s.verifier.LastFailure())
	}
	// The claim is replaced under the name: fatal for the next generation.
	s.f.remove(api.Pods, testNamespace, "wc-one")
	s.f.remove(api.PersistentVolumeClaims, testNamespace, "wc-one-home")
	seedSandbox(s.f, "wc-one")
	s.value["generation"] = "3"
	if _, err := s.registry.Register(s.value); err != nil {
		t.Fatal(err)
	}
	s.ready("runtime")
	s.verifier.WaitIdle()
	if s.ready("runtime") || !strings.Contains(s.verifier.LastFailure(), "workspace volume replaced") {
		t.Fatalf("replaced volume: %q", s.verifier.LastFailure())
	}
}
