package policy

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
)

type sbxFixture struct {
	t        *testing.T
	dir      string
	clock    *testClock
	verifier *fixtureVerifier
	registry *Registry
	value    map[string]any
	gateway  map[string]any
}

func newSbxFixture(t *testing.T) *sbxFixture {
	f := &sbxFixture{t: t, dir: t.TempDir(), clock: &testClock{now: 1000, mono: 1000}, verifier: &fixtureVerifier{enabled: true}}
	f.registry = newTestRegistry(t, f.dir, f.clock, f.verifier)
	f.verifier.registry = f.registry
	t.Cleanup(func() { f.registry.Close() })
	f.value = runContext(nil)
	f.must(f.registry.Register(f.value))
	f.must(f.registry.BindGateway(f.value, 19443))
	f.must(f.registry.ConfigureProvider(f.value, "synthetic-secret-for-tests-only"))
	f.gateway = f.must(f.registry.Gateway(f.value))
	return f
}

func (f *sbxFixture) must(result map[string]any, err error) map[string]any {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *sbxFixture) proxy(message map[string]any) (map[string]any, error) {
	return f.registry.Proxy(f.gateway["bindingID"].(string), f.gateway["capability"], message)
}

func (f *sbxFixture) egress(changes map[string]any) (map[string]any, map[string]any) {
	f.t.Helper()
	request := map[string]any{"host": "api.openai.com", "method": "POST", "scheme": "https", "path": "/v1/responses"}
	for k, v := range changes {
		request[k] = v
	}
	result, err := f.proxy(map[string]any{"action": "egress", "request": request})
	if err != nil {
		f.t.Fatal(err)
	}
	return request, result
}

// reopen restarts the registry the way production does: with no verifier
// attached until the service wires one up again.
func (f *sbxFixture) reopen() {
	f.t.Helper()
	f.registry.Close()
	f.registry = newTestRegistry(f.t, f.dir, f.clock, nil)
	f.verifier.registry = f.registry
}

func ready(result map[string]any) bool {
	r, _ := result["ready"].(bool)
	return r
}

func TestDefaultVerifierNeverAcceptsRegistrationOrStaticClaim(t *testing.T) {
	f := newSbxFixture(t)
	f.registry.Verifier = UnsupportedVerifier{}
	if ready(f.must(f.registry.Check(f.value, "create"))) || ready(f.must(f.registry.Check(f.value, "runtime"))) || ready(f.must(f.registry.Begin(f.value, false))) {
		t.Fatal("unsupported verifier proved readiness")
	}
	if _, err := f.registry.Dispatch(map[string]any{"version": 1, "operation": "check", "phase": "runtime", "context": f.value, "ready": true}); err == nil {
		t.Fatal("static ready claim accepted")
	}
}

func TestBindingsImmutableAcrossProjectRuntimeGenerationPrincipal(t *testing.T) {
	f := newSbxFixture(t)
	for _, field := range []string{"projectID", "runtimeName", "principalID"} {
		if _, err := f.registry.Register(runContext(map[string]any{field: "changed"})); err == nil {
			t.Fatalf("%s change accepted", field)
		}
	}
	if _, err := f.registry.Register(runContext(map[string]any{"sandboxID": "another"})); err == nil {
		t.Fatal("duplicate runtime accepted")
	}
	if r := f.must(f.registry.Register(f.value)); r["ok"] != true {
		t.Fatal("idempotent register")
	}
}

func TestGenerationReplacementRequiresEndedLeaseAndRejectsReplay(t *testing.T) {
	f := newSbxFixture(t)
	updated := runContext(map[string]any{"generation": "2", "runID": "r2"})
	f.must(f.registry.Begin(f.value, false))
	if _, err := f.registry.Register(updated); err == nil {
		t.Fatal("active generation replaced")
	}
	f.must(f.registry.End(f.value))
	if r := f.must(f.registry.Register(updated)); r["ok"] != true {
		t.Fatal("replacement failed")
	}
	if f.must(f.registry.Gateway(updated))["capability"] == f.gateway["capability"] {
		t.Fatal("capability reused")
	}
	identity, _ := ValidateContext(f.value)
	f.verifier.overrides = map[string]any{"binding": BindingDigest(identity)}
	if ready(f.must(f.registry.Check(updated, "runtime"))) {
		t.Fatal("old binding proof accepted")
	}
	if _, err := f.registry.Register(f.value); err == nil {
		t.Fatal("retired generation replayed")
	}
	f.reopen()
	if _, err := f.registry.Register(f.value); err == nil {
		t.Fatal("retired generation replayed after restart")
	}
}

func TestIdentitySurvivesRestartButCredentialsAndCapabilitiesDoNot(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	f.reopen()
	if ready(f.must(f.registry.Check(f.value, "runtime"))) {
		t.Fatal("ready after restart without proof")
	}
	b := f.registry.Bindings["s1"]
	if b.ProviderSecret != "" || b.Lease != nil || b.GatewayPort != 19443 {
		t.Fatalf("binding state: %+v", b)
	}
	if f.must(f.registry.Gateway(f.value))["capability"] == f.gateway["capability"] {
		t.Fatal("capability survived restart")
	}
	if _, err := f.proxy(map[string]any{"action": "egress", "request": map[string]any{}}); err == nil {
		t.Fatal("old capability accepted")
	}
}

func TestWrongOrStaleProofCannotStart(t *testing.T) {
	f := newSbxFixture(t)
	for _, override := range []map[string]any{{"binding": "wrong"}, {"phase": "create"}, {"policy_digest": "wrong"}, {"expires_at": f.clock.monotonic()},
		{"expires_at": f.clock.monotonic() + ProofSeconds + 60}, {"gateway_port": 19444}, {"evidence_id": ""}} {
		f.verifier.overrides = override
		if ready(f.must(f.registry.Begin(f.value, false))) {
			t.Fatalf("proof %v accepted", override)
		}
	}
}

func TestContextAndProtocolRejectUnknownFields(t *testing.T) {
	f := newSbxFixture(t)
	for _, message := range []map[string]any{
		{"version": true, "operation": "end", "context": f.value},
		{"version": 1, "operation": "end", "context": runContext(map[string]any{"authority": "admin"})},
		{"version": 1, "operation": "end", "context": runContext(map[string]any{"sandboxID": "../s1"})},
	} {
		if _, err := f.registry.Dispatch(message); err == nil {
			t.Fatalf("%v accepted", message)
		}
	}
}

func TestProviderRequiresVerifiedLeaseAndReturnsOnlyPlaceholderToWorker(t *testing.T) {
	f := newSbxFixture(t)
	if _, denied := f.egress(nil); denied["allow"] == true {
		t.Fatal("egress allowed without lease")
	}
	result := f.must(f.registry.Begin(f.value, false))
	if !ready(result) || result["apiKeyPlaceholder"] != "warden-proxy-managed" || result["providerBaseURL"] != "http://host.docker.internal:19443/openai/v1" || strings.Contains(Dumps(result), "synthetic-secret") {
		t.Fatalf("begin: %v", result)
	}
	request, decision := f.egress(nil)
	provider, err := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": request})
	if err != nil || provider["authorization"] != "Bearer synthetic-secret-for-tests-only" {
		t.Fatalf("provider: %v %v", provider, err)
	}
}

func TestProviderExactRouteAndRecordBinding(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	request, decision := f.egress(nil)
	for _, changes := range []map[string]any{{"host": "example.com"}, {"path": "/v1/files"}, {"path": "/v1/responses?redirect=evil"}, {"scheme": "http"}, {"method": "GET"}} {
		altered := map[string]any{}
		for k, v := range request {
			altered[k] = v
		}
		for k, v := range changes {
			altered[k] = v
		}
		r, _ := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": altered})
		if r["allow"] == true {
			t.Fatalf("%v allowed", changes)
		}
	}
	for _, path := range []string{"/v1/files", "/v1/responses?x=y"} {
		req, result := f.egress(map[string]any{"path": path})
		r, _ := f.proxy(map[string]any{"action": "provider", "decision_id": result["decision_id"], "request": req})
		if r["allow"] == true {
			t.Fatalf("%s allowed", path)
		}
	}
}

func TestGatewayCapabilityCannotCrossSandbox(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Register(runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"})))
	if _, err := f.registry.Proxy("s2", f.gateway["capability"], map[string]any{"action": "egress", "request": map[string]any{}}); err == nil {
		t.Fatal("capability crossed sandbox")
	}
	if _, err := f.proxy(map[string]any{"action": "ready", "firewall": "enforced"}); err == nil {
		t.Fatal("ready action accepted")
	}
	if _, err := f.proxy(map[string]any{"action": "approve", "request_id": "forged"}); err == nil {
		t.Fatal("approve action accepted")
	}
}

func TestEndAndExpiryRevokeDecisionsAndInflightActive(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	request, decision := f.egress(nil)
	f.must(f.registry.End(f.value))
	active, _ := f.proxy(map[string]any{"action": "active", "decision_id": decision["decision_id"]})
	if active["active"] == true {
		t.Fatal("decision active after end")
	}
	provider, _ := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": request})
	if provider["allow"] == true {
		t.Fatal("provider after end")
	}
	if ready(f.must(f.registry.Begin(f.value, false))) {
		t.Fatal("ended run reused")
	}
	next := runContext(map[string]any{"runID": "r2"})
	if !ready(f.must(f.registry.Begin(next, false))) {
		t.Fatal("next run denied")
	}
	f.clock.advance(121)
	if _, denied := f.egress(nil); denied["allow"] == true {
		t.Fatal("expired lease allowed egress")
	}
	if ready(f.must(f.registry.Begin(next, true))) {
		t.Fatal("renew after expiry")
	}
}

func TestOtherChatCannotEndActiveRunAndRenewalIsBounded(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	other := runContext(map[string]any{"chatID": "c2", "runID": "r2"})
	if ready(f.must(f.registry.Begin(other, false))) {
		t.Fatal("other chat began")
	}
	if r := f.must(f.registry.End(other)); r["ok"] != false {
		t.Fatal("other chat ended run")
	}
	f.clock.advance(30)
	if !ready(f.must(f.registry.Begin(f.value, true))) {
		t.Fatal("renew failed")
	}
	if f.registry.Bindings["s1"].Lease.ExpiresAt != f.clock.monotonic()+120 {
		t.Fatal("renewal not bounded")
	}
}

func TestLossOfReadinessRevokesLeaseAndOldDecisions(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	request, decision := f.egress(nil)
	f.verifier.enabled = false
	if ready(f.must(f.registry.Check(f.value, "runtime"))) {
		t.Fatal("ready without verifier")
	}
	f.verifier.enabled = true
	provider, _ := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": request})
	if provider["allow"] == true {
		t.Fatal("old decision survived readiness loss")
	}
}

func TestAuditFailureNeverActivatesOrReleasesProviderCredential(t *testing.T) {
	f := newSbxFixture(t)
	engine := f.registry.Bindings["s1"].Engine
	engine.Audit.failWith = errors.New("ENOSPC")
	if _, err := f.registry.Begin(f.value, false); err == nil {
		t.Fatal("begin succeeded without audit")
	}
	if f.registry.Bindings["s1"].Lease != nil {
		t.Fatal("lease activated")
	}
	engine.Audit.failWith = nil
	f.must(f.registry.Begin(f.value, false))
	request, decision := f.egress(nil)
	engine.Audit.failWith = errors.New("ENOSPC")
	if _, err := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": request}); err == nil {
		t.Fatal("provider released without audit")
	}
}

func TestManifestFailurePoisoningPreventsInMemoryRetryReadiness(t *testing.T) {
	f := newSbxFixture(t)
	saveManifestHook = func(*Registry) error { return errors.New("ENOSPC") }
	_, err := f.registry.Register(runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"}))
	saveManifestHook = nil
	if err == nil {
		t.Fatal("register succeeded")
	}
	if ready(f.must(f.registry.Check(f.value, "runtime"))) {
		t.Fatal("ready after storage failure")
	}
	if _, err := f.registry.Dispatch(map[string]any{"version": 1, "operation": "check", "context": f.value, "phase": "runtime"}); err == nil {
		t.Fatal("dispatch after storage failure")
	}
}

func TestCredentialsAbsentFromDurableStateAndOpaqueTLSDenied(t *testing.T) {
	f := newSbxFixture(t)
	f.must(f.registry.Begin(f.value, false))
	r, _ := f.proxy(map[string]any{"action": "egress", "request": map[string]any{"host": "swcdn.apple.com", "method": "GET", "scheme": "https", "tls": true}})
	if r["allow"] == true {
		t.Fatal("opaque TLS allowed")
	}
	if hits := filesContain(t, f.dir, []byte("synthetic-secret-for-tests-only")); len(hits) > 0 {
		t.Fatalf("secret persisted: %v", hits)
	}
}

func TestPrivateProtocolRoundtripDuplicateJSONAndGenericErrors(t *testing.T) {
	f := newSbxFixture(t)
	dir, err := os.MkdirTemp("/tmp", "wsbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "control.sock")
	server, err := ListenControl("unix://"+path, nil, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	result, err := ControlRPC("unix://"+path, nil, map[string]any{"version": 1, "operation": "check", "context": f.value, "phase": "runtime"})
	if err != nil || !ready(result) {
		t.Fatalf("rpc: %v %v", result, err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte(`{"version":1,"version":1,"secret":"never-echo"}` + "\n"))
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	conn.Close()
	if strings.Contains(string(buf[:n]), "never-echo") || !strings.Contains(string(buf[:n]), `"ready":false`) {
		t.Fatalf("response: %s", buf[:n])
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", info.Mode().Perm())
	}
	if _, err := ControlRPC("unix://"+path, nil, map[string]any{"version": 1, "operation": "sharing", "action": "state", "data": map[string]any{}}); err == nil {
		t.Fatal("sharing without service accepted")
	}
	// Sharing refusals travel back with their wording (the chat service
	// shows them to the owner and the agent); other failures stay generic.
	response, err := f.registry.Dispatch(map[string]any{"version": 1, "operation": "sharing", "action": "state", "data": map[string]any{}})
	if err != nil || response["ok"] != false || response["error"] != "sharing unavailable" {
		t.Fatalf("sharing without service: %v %v", response, err)
	}
	f.registry.Sharing = &Sharing{}
	response, err = f.registry.Dispatch(map[string]any{"version": 1, "operation": "sharing", "action": "github_list", "data": map[string]any{"chatID": "c1", "sandboxID": "s1"}})
	if err != nil || response["ok"] != false || response["error"] != "GitHub is not connected" {
		t.Fatalf("sharing refusal: %v %v", response, err)
	}
	f.registry.Sharing = nil
	if _, err = f.registry.Dispatch(map[string]any{"version": 1, "operation": "check", "context": "nope", "phase": "runtime"}); err == nil {
		t.Fatal("bad check accepted")
	}
}

func TestClaudeLeaseRoutesOnlyClaude(t *testing.T) {
	f := newSbxFixture(t)
	claude := fakeSource{available: true, routes: map[string][2]string{"/v1/messages": {"api.anthropic.com", "/v1/messages"}, "/v1/messages/count_tokens": {"api.anthropic.com", "/v1/messages/count_tokens"}}, headers: map[string]string{"Authorization": "Bearer synthetic-claude-host-secret"}}
	f.registry.ClaudeSource = claude
	value := runContext(map[string]any{"runID": "claude-run", "provider": "claude"})
	result := f.must(f.registry.Begin(value, false))
	if !ready(result) || result["providerBaseURL"] != "http://host.docker.internal:19443/anthropic" || result["provider"] != "claude" {
		t.Fatalf("claude begin: %v", result)
	}
	route, _ := f.proxy(map[string]any{"action": "providerRoute", "path": "/v1/messages"})
	if route["allow"] != true || route["host"] != "api.anthropic.com" {
		t.Fatalf("route: %v", route)
	}
	route, _ = f.proxy(map[string]any{"action": "providerRoute", "path": "/v1/responses"})
	if route["allow"] == true {
		t.Fatal("codex route under claude lease")
	}
	request, decision := f.egress(map[string]any{"host": "api.anthropic.com", "path": "/v1/messages"})
	provider, _ := f.proxy(map[string]any{"action": "provider", "decision_id": decision["decision_id"], "request": request})
	if provider["allow"] != true || provider["authorization"] != "Bearer synthetic-claude-host-secret" {
		t.Fatalf("provider: %v", provider)
	}
	f.registry.ClaudeSource = fakeSource{available: false}
	if ready(f.must(f.registry.Begin(value, true))) {
		t.Fatal("renewed with unavailable credential")
	}
	if f.registry.Bindings["s1"].Lease != nil {
		t.Fatal("lease survived credential loss")
	}
}

type fakeSource struct {
	available bool
	routes    map[string][2]string
	headers   map[string]string
}

func (s fakeSource) Available() bool { return s.available }
func (s fakeSource) Route(path string) (string, string, error) {
	route, ok := s.routes[path]
	if !ok {
		return "", "", errors.New("unsupported")
	}
	return route[0], route[1], nil
}
func (s fakeSource) Headers() (map[string]string, error) { return s.headers, nil }

// The control protocol's "version" operation answers the startup handshake
// with this service's revision and the release protocol; it takes no
// context and rejects extra fields like every other operation.
func TestVersionOperationAnswersTheHandshake(t *testing.T) {
	f := newSbxFixture(t)
	result, err := f.registry.Dispatch(map[string]any{"version": 1, "operation": "version"})
	if err != nil || result["ok"] != true || result["protocol"] != release.Protocol || result["revision"] != release.Revision {
		t.Fatalf("%v %v", result, err)
	}
	if _, err = f.registry.Dispatch(map[string]any{"version": 1, "operation": "version", "context": f.value}); err == nil {
		t.Fatal("extra fields accepted")
	}
	dir, err := os.MkdirTemp("/tmp", "wsbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "control.sock")
	server, err := ListenControl("unix://"+path, nil, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peer, err := handshake.Policy(context.Background(), "unix://"+path, nil)
	if err != nil || peer != (handshake.Peer{Name: "warden-policy", Revision: release.Revision, Protocol: release.Protocol}) {
		t.Fatalf("%+v %v", peer, err)
	}
}

// The console's egress switch applies to live engines and later ones, is
// persisted so a restart keeps it, and is readable with its source.
func TestRegistryEgressSwitchAppliesEverywhereAndPersists(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 1000}
	registry := newTestRegistry(t, dir, clock, &fixtureVerifier{enabled: true})
	if _, err := registry.Register(runContext(nil)); err != nil {
		t.Fatal(err)
	}
	mode := func(e *Engine) string {
		egress, _ := e.PolicyCopy()["egress"].(map[string]any)
		m, _ := egress["mode"].(string)
		return m
	}
	first := registry.Bindings["s1"].Engine
	if m, src := registry.EgressMode(); m != "restricted" || src != "config" || mode(first) != "restricted" {
		t.Fatalf("initial: %s %s %s", m, src, mode(first))
	}
	if err := registry.SetEgressMode("public"); err != nil {
		t.Fatal(err)
	}
	if m, src := registry.EgressMode(); m != "public" || src != "console" || mode(first) != "public" {
		t.Fatalf("after set: %s %s %s", m, src, mode(first))
	}
	if _, err := registry.Register(runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"})); err != nil {
		t.Fatal(err)
	}
	if mode(registry.Bindings["s2"].Engine) != "public" {
		t.Fatal("later engine not open")
	}
	if saved, err := LoadEgressMode(dir); err != nil || saved != "public" {
		t.Fatalf("persisted: %q %v", saved, err)
	}
	if err := registry.SetEgressMode("everything"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := registry.SetEgressMode("restricted"); err != nil || mode(first) != "restricted" || mode(registry.Bindings["s2"].Engine) != "restricted" {
		t.Fatalf("back to restricted: %v", err)
	}
}

// A sandbox's own egress mode is keyed by sandbox ID: set before the
// sandbox registers it shapes the engine created for it, set on a live
// sandbox it changes the engine at once, the install-wide switch leaves it
// alone, it survives a registry restart, and clearing it returns the
// sandbox to the install's mode.
func TestRegistrySandboxEgressOverridesTheInstall(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 1000}
	registry := newTestRegistry(t, dir, clock, &fixtureVerifier{enabled: true})
	mode := func(e *Engine) string {
		egress, _ := e.PolicyCopy()["egress"].(map[string]any)
		m, _ := egress["mode"].(string)
		return m
	}
	// Chosen at creation, before the sandbox exists.
	if err := registry.SetSandboxEgress("s1", "public"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(runContext(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"})); err != nil {
		t.Fatal(err)
	}
	first, second := registry.Bindings["s1"].Engine, registry.Bindings["s2"].Engine
	if mode(first) != "public" || mode(second) != "restricted" {
		t.Fatalf("after register: %s %s", mode(first), mode(second))
	}
	if own, effective := registry.SandboxEgress("s1"); own != "public" || effective != "public" {
		t.Fatalf("s1: %s %s", own, effective)
	}
	if own, effective := registry.SandboxEgress("s2"); own != "" || effective != "restricted" {
		t.Fatalf("s2: %s %s", own, effective)
	}
	if registry.EgressOverrides() != 1 {
		t.Fatalf("overrides: %d", registry.EgressOverrides())
	}
	// The install-wide switch skips the sandbox with its own mode.
	if err := registry.SetSandboxEgress("s2", "restricted"); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetEgressMode("public"); err != nil {
		t.Fatal(err)
	}
	if mode(second) != "restricted" {
		t.Fatal("install-wide switch changed a sandbox with its own mode")
	}
	if own, effective := registry.SandboxEgress("s2"); own != "restricted" || effective != "restricted" {
		t.Fatalf("s2 after install open: %s %s", own, effective)
	}
	// A live change applies at once; clearing follows the install (now open).
	if err := registry.SetSandboxEgress("s2", ""); err != nil || mode(second) != "public" {
		t.Fatalf("s2 cleared: %v %s", err, mode(second))
	}
	if err := registry.SetSandboxEgress("s1", "restricted"); err != nil || mode(first) != "restricted" {
		t.Fatalf("s1 restricted live: %v %s", err, mode(first))
	}
	for _, bad := range []struct{ sandbox, mode string }{{"", "public"}, {"s1", "open"}, {strings.Repeat("x", 129), "public"}} {
		if err := registry.SetSandboxEgress(bad.sandbox, bad.mode); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	// Persisted: a restarted registry recreates s1's engine restricted
	// while the install is open, and s2's open.
	registry.Close()
	options := RegistryOptions{Operations: testOperations(t), PolicyTemplate: templatePath(t), Verifier: &fixtureVerifier{enabled: true}, Clock: clock.monotonic, EgressMode: "public"}
	restarted, err := NewRegistry(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if mode(restarted.Bindings["s1"].Engine) != "restricted" || mode(restarted.Bindings["s2"].Engine) != "public" {
		t.Fatalf("after restart: %s %s", mode(restarted.Bindings["s1"].Engine), mode(restarted.Bindings["s2"].Engine))
	}
	if restarted.EgressOverrides() != 1 {
		t.Fatalf("overrides after restart: %d", restarted.EgressOverrides())
	}
	if err := os.WriteFile(filepath.Join(dir, egressOverridesFile), []byte(`{"s1":"open"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if _, err := NewRegistry(dir, options); err == nil {
		t.Fatal("invalid overrides file accepted")
	}
}

// The runner's declared snapshot image (a workspace copy, a regeneration)
// reaches the verifier with the binding's identity: on the registration
// that names it, on a re-registration of the same generation, on a new
// generation, and again after a restart from the first request that
// names it; a malformed digest is refused; the digest is not part of the
// binding identity.
func TestRegistryPassesTheDeclaredImageDigestToTheVerifier(t *testing.T) {
	f := newSbxFixture(t)
	var seen []string
	f.verifier.hook = func(identity map[string]string, phase string) { seen = append(seen, identity["imageDigest"]) }
	digest := "sha256:" + strings.Repeat("c", 64)
	if _, err := f.registry.Register(runContext(map[string]any{"imageDigest": "sha256:short"})); err == nil {
		t.Fatal("malformed digest accepted")
	}
	if ready(f.must(f.registry.Check(f.value, "create"))); len(seen) != 1 || seen[0] != "" {
		t.Fatalf("no digest declared yet: %v", seen)
	}
	declared := runContext(map[string]any{"imageDigest": digest})
	f.must(f.registry.Register(declared))
	f.must(f.registry.Check(declared, "create"))
	if seen[len(seen)-1] != digest {
		t.Fatalf("re-registration did not pass the digest: %v", seen)
	}
	identity, _ := ValidateContext(declared)
	plain, _ := ValidateContext(f.value)
	if BindingDigest(identity) != BindingDigest(plain) {
		t.Fatal("the digest changed the binding identity")
	}
	// A new generation declared with the digest keeps it.
	f.must(f.registry.Begin(declared, false))
	f.must(f.registry.End(declared))
	next := runContext(map[string]any{"generation": "2", "runID": "r2", "imageDigest": digest})
	f.must(f.registry.Register(next))
	f.must(f.registry.Check(next, "create"))
	if seen[len(seen)-1] != digest {
		t.Fatalf("new generation lost the digest: %v", seen)
	}
	// After a restart the manifest has identities only; the first request
	// naming the digest restores it, one without it verifies without.
	f.reopen()
	f.registry.Verifier = f.verifier
	f.must(f.registry.Check(runContext(map[string]any{"generation": "2", "runID": "r2"}), "create"))
	if seen[len(seen)-1] != "" {
		t.Fatalf("restart kept a digest the manifest does not hold: %v", seen)
	}
	f.must(f.registry.Check(next, "create"))
	if seen[len(seen)-1] != digest {
		t.Fatalf("digest not restored by the request naming it: %v", seen)
	}
}

// A host event lands in the install-wide audit chain under <state>/audit
// and, once the sandbox is bound, in that sandbox's own chain too, as a
// warning; an unknown event is refused.
func TestRegistryRecordsHostEventsInBothChains(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 1000}
	registry := newTestRegistry(t, dir, clock, &fixtureVerifier{enabled: true})
	if err := registry.EmitHostEvent("s1", "host.reboot", nil); err == nil {
		t.Fatal("unknown event accepted")
	}
	if err := registry.EmitHostEvent("s1", "workspace.jailbreak", map[string]any{"sandbox_id": "s1", "chat_id": "c1", "on": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(runContext(nil)); err != nil {
		t.Fatal(err)
	}
	if err := registry.EmitHostEvent("s1", "host.exec", map[string]any{"sandbox_id": "s1", "chat_id": "c1", "command": "uname -a", "exit": 0}); err != nil {
		t.Fatal(err)
	}
	install, err := os.ReadFile(filepath.Join(dir, "audit", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(install)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"event_type": "workspace.jailbreak"`) && !strings.Contains(lines[0], `"event_type":"workspace.jailbreak"`) || !strings.Contains(lines[1], "host.exec") || !strings.Contains(lines[1], "uname -a") || !strings.Contains(lines[1], `"warning"`) {
		t.Fatalf("install chain:\n%s", install)
	}
	b := registry.Bindings["s1"]
	own, err := os.ReadFile(b.Engine.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(own), "host.exec") || strings.Contains(string(own), "workspace.jailbreak") {
		t.Fatalf("sandbox chain:\n%s", own)
	}
	registry.Close()
}
