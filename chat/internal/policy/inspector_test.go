package policy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeInspector is a SandboxInspector with scripted facts, recording the
// egress switches the verifier drives.
type fakeInspector struct {
	mu         sync.Mutex
	calls      []string
	clusterErr error
	facts      RuntimeFacts
	factsErr   error
	grantErr   error
	granted    map[string]GatewayEndpoint
}

func newFakeInspector() *fakeInspector {
	return &fakeInspector{facts: RuntimeFacts{Present: true, Identity: "vol-1", ImageDigest: "sha256:" + strings.Repeat("a", 64), Tier: "gvisor"}, granted: map[string]GatewayEndpoint{}}
}

func (f *fakeInspector) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}
func (f *fakeInspector) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}
func (f *fakeInspector) ClusterFacts(context.Context) error {
	f.record("cluster")
	return f.clusterErr
}
func (f *fakeInspector) Facts(_ context.Context, identity map[string]string) (RuntimeFacts, error) {
	f.record("facts:" + identity["runtimeName"])
	f.mu.Lock()
	defer f.mu.Unlock()
	facts := f.facts
	if _, ok := f.granted[identity["runtimeName"]]; ok {
		facts.Egress = EgressGateway
	} else {
		facts.Egress = EgressDenied
	}
	return facts, f.factsErr
}
func (f *fakeInspector) GrantEgress(_ context.Context, identity map[string]string, gateway GatewayEndpoint) error {
	f.record("grant:" + identity["runtimeName"] + ":" + gateway.ProxyURL())
	if f.grantErr != nil {
		return f.grantErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted[identity["runtimeName"]] = gateway
	return nil
}
func (f *fakeInspector) DenyEgress(_ context.Context, identity map[string]string) error {
	f.record("deny:" + identity["runtimeName"])
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.granted, identity["runtimeName"])
	return nil
}

type inspectorFixture struct {
	t         *testing.T
	clock     *testClock
	registry  *Registry
	value     map[string]any
	inspector *fakeInspector
	verifier  *RuntimeVerifier
}

func newInspectorFixture(t *testing.T) *inspectorFixture {
	f := &inspectorFixture{t: t, clock: &testClock{now: 1000, mono: 1000}, inspector: newFakeInspector()}
	f.registry = newTestRegistry(t, t.TempDir(), f.clock, nil)
	t.Cleanup(func() { f.registry.Close() })
	f.value = runContext(nil)
	if _, err := f.registry.Register(f.value); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.BindGateway(f.value, 19443); err != nil {
		t.Fatal(err)
	}
	f.verifier = NewRuntimeVerifier(f.registry, f.inspector)
	f.registry.Verifier = f.verifier
	previous := GatewayHealthy
	GatewayHealthy = func(*Binding) bool { return true }
	t.Cleanup(func() { GatewayHealthy = previous })
	t.Cleanup(func() { f.verifier.WaitIdle() })
	return f
}

func (f *inspectorFixture) ready(phase string) bool {
	f.t.Helper()
	result, err := f.registry.Check(f.value, phase)
	if err != nil {
		f.t.Fatal(err)
	}
	return ready(result)
}

func (f *inspectorFixture) count(prefix string) int {
	n := 0
	for _, c := range f.inspector.list() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// The trust model runs unchanged over any inspector: the chat path grants
// egress and trusts provisionally, the background verification proves the
// cluster and the runtime, a failure denies egress and distrusts the
// binding until a synchronous verification passes.
func TestVerifierTrustModelThroughInspector(t *testing.T) {
	f := newInspectorFixture(t)
	if !f.ready("runtime") {
		t.Fatalf("provisional trust refused: %s", f.verifier.LastFailure())
	}
	if f.count("grant:sbx-one:http://host.docker.internal:19443") != 1 || f.count("facts:") != 0 {
		t.Fatalf("chat path must grant egress and not inspect: %v", f.inspector.list())
	}
	f.verifier.WaitIdle()
	if f.count("cluster") != 1 || f.count("facts:sbx-one") != 1 {
		t.Fatalf("background verification did not run: %v", f.inspector.list())
	}
	if !f.ready("runtime") || f.count("facts:sbx-one") != 1 {
		t.Fatal("fresh proof re-inspected")
	}
	// The runtime stops satisfying the facts: the next background
	// verification denies egress and the binding loses provisional trust.
	f.clock.advance(ProofSeconds + 1)
	f.inspector.mu.Lock()
	f.inspector.facts.Violations = []string{"pod runs an unpinned RuntimeClass"}
	f.inspector.mu.Unlock()
	f.ready("runtime") // provisional until the background verification fails
	f.verifier.WaitIdle()
	if f.ready("runtime") || !strings.Contains(f.verifier.LastFailure(), "RuntimeClass") || f.count("deny:sbx-one") == 0 {
		t.Fatalf("violation accepted: failure=%q calls=%v", f.verifier.LastFailure(), f.inspector.list())
	}
	// Distrusted: verified synchronously, denied again on failure.
	denies := f.count("deny:")
	if f.ready("runtime") || f.count("deny:") != denies+1 {
		t.Fatal("distrusted binding was not re-verified synchronously and re-denied")
	}
	f.inspector.mu.Lock()
	f.inspector.facts.Violations = nil
	f.inspector.mu.Unlock()
	if !f.ready("runtime") {
		t.Fatalf("recovered runtime refused: %s", f.verifier.LastFailure())
	}
	// A cluster failure invalidates every proof and denies warm bindings.
	if _, err := f.registry.ConfigureProvider(f.value, "synthetic-secret-for-tests-only"); err != nil {
		t.Fatal(err)
	}
	if result, err := f.registry.Begin(f.value, false); err != nil || !ready(result) {
		t.Fatalf("begin: %v %v", result, err)
	}
	f.inspector.mu.Lock()
	f.inspector.clusterErr = errors.New("NetworkPolicy not enforced")
	f.inspector.mu.Unlock()
	denies = f.count("deny:")
	if f.verifier.RefreshOnce() != 0 || f.count("deny:sbx-one") != denies+1 || !strings.Contains(f.verifier.LastFailure(), "NetworkPolicy") {
		t.Fatalf("cluster failure did not deny the warm binding: %v %q", f.inspector.list(), f.verifier.LastFailure())
	}
}

// A runtime phase proof needs the runtime present and reaching exactly its
// gateway; the create phase needs neither.
func TestVerifierRequiresPresenceAndGatewayEgressForRuntimePhase(t *testing.T) {
	f := newInspectorFixture(t)
	f.inspector.mu.Lock()
	f.inspector.facts.Present = false
	f.inspector.mu.Unlock()
	if !f.ready("create") {
		t.Fatal("create not ready")
	}
	f.verifier.WaitIdle()
	if f.count("grant:") != 0 || !f.ready("create") {
		t.Fatal("create phase granted egress or failed")
	}
	f.ready("runtime")
	f.verifier.WaitIdle()
	if f.ready("runtime") || f.verifier.LastFailure() != "runtime absent" {
		t.Fatalf("absent runtime accepted: %q", f.verifier.LastFailure())
	}
}

// The SBX inspector reports its tier and probes egress against the
// endpoint it granted.
func TestSbxInspectorFactsAndEgress(t *testing.T) {
	cli := newCliFixture()
	inspector, err := NewSbxInspector(t.TempDir(), "/trusted/sbx", true, cli.call)
	if err != nil {
		t.Fatal(err)
	}
	identity := map[string]string{"sandboxID": "s1", "runtimeName": "sbx-one"}
	if err = inspector.ClusterFacts(context.Background()); err != nil {
		t.Fatal(err)
	}
	facts, err := inspector.Facts(context.Background(), identity)
	if err != nil || !facts.Present || facts.Tier != TierSBX || facts.Identity != "fixture-runtime-uuid" || facts.ImageDigest != SBXShellDigest || facts.Egress != EgressDenied || len(facts.Violations) != 0 {
		t.Fatalf("facts: %+v %v", facts, err)
	}
	if err = inspector.GrantEgress(context.Background(), identity, LoopbackEndpoint(19443)); err != nil {
		t.Fatal(err)
	}
	facts, err = inspector.Facts(context.Background(), identity)
	if err != nil || facts.Egress != EgressGateway {
		t.Fatalf("granted facts: %+v %v", facts, err)
	}
	if err = inspector.DenyEgress(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	last := strings.Join(cli.list()[len(cli.list())-1], " ")
	if last != "policy deny network --sandbox sbx-one **" {
		t.Fatalf("deny not applied: %s", last)
	}
	cli.uuid = "replaced"
	if _, err = inspector.Facts(context.Background(), identity); err == nil || err.Error() != "runtime UUID changed" {
		t.Fatalf("pin not enforced: %v", err)
	}
	if _, err = inspector.Facts(context.Background(), map[string]string{"runtimeName": "absent"}); err != nil {
		t.Fatal(err)
	}
	// An unpinned runtime's policy is never touched by a deny.
	before := len(cli.list())
	if err = inspector.DenyEgress(context.Background(), map[string]string{"runtimeName": "never-pinned"}); err != nil || len(cli.list()) != before {
		t.Fatal("deny touched an unpinned sandbox")
	}
}

func TestUnsupportedVerifierStaysClosed(t *testing.T) {
	clock := &testClock{now: 1000, mono: 1000}
	registry := newTestRegistry(t, t.TempDir(), clock, nil)
	t.Cleanup(func() { registry.Close() })
	value := runContext(nil)
	if _, err := registry.Register(value); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BindGateway(value, 19443); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"create", "runtime"} {
		result, err := registry.Check(value, phase)
		if err != nil || ready(result) {
			t.Fatalf("%s: %v %v", phase, result, err)
		}
	}
}
