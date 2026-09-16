package policy

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/release"
)

type cliFixture struct {
	mu            sync.Mutex
	commands      [][]string
	runtime       bool
	uuid          string
	mcp           []any
	ssh           bool
	image         string
	network       []map[string]any
	globalNetwork []map[string]any
}

func newCliFixture() *cliFixture {
	f := &cliFixture{runtime: true, uuid: "fixture-runtime-uuid", mcp: []any{}, image: SBXShellDigest}
	f.network = []map[string]any{f.rule("deny", "**")}
	return f
}

func (f *cliFixture) rule(decision, resource string) map[string]any {
	return map[string]any{"resource_type": "network", "status": "active", "layer": "local", "scope": "sandbox:sbx-one", "sandbox_id": "sbx-one",
		"origin": "scoped", "decision": decision, "resources": []any{resource}}
}

func (f *cliFixture) list() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.commands...)
}

func (f *cliFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = nil
}

func (f *cliFixture) call(args []string, denied bool) (string, error) {
	f.mu.Lock()
	f.commands = append(f.commands, args)
	f.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case joined == "version":
		return "sbx version: v0.42.1 fixture-build", nil
	case strings.HasPrefix(joined, "settings get --json "):
		var value any = "direct"
		if strings.HasPrefix(args[3], "ssh.") {
			value = f.ssh
		}
		return string(mustJSON(map[string]any{"key": args[3], "value": value})), nil
	case strings.HasPrefix(joined, "mcp ls"):
		return string(mustJSON(map[string]any{"gateway": map[string]any{"local": true}, "servers": f.mcp})), nil
	case strings.HasPrefix(joined, "policy ls"):
		rules := []any{}
		for _, r := range f.globalNetwork {
			rules = append(rules, r)
		}
		if strings.Contains(joined, "sbx-one") {
			for _, r := range f.network {
				rules = append(rules, r)
			}
		}
		return string(mustJSON(map[string]any{"rules": rules})), nil
	case strings.HasPrefix(joined, "policy check network"):
		allow := strings.Contains(joined, "--sandbox") && args[len(args)-1] == "localhost:19443"
		result := map[string]any{"allowed": allow, "governance": map[string]any{"active": false}}
		if !allow {
			result["deny_kind"] = "implicit"
		}
		return string(mustJSON(result)), nil
	case joined == "ls --json":
		sandboxes := []any{}
		if f.runtime {
			sandboxes = append(sandboxes, map[string]any{"name": "sbx-one", "id": f.uuid, "agent": "shell", "status": "stopped"})
		}
		return string(mustJSON(map[string]any{"sandboxes": sandboxes})), nil
	case args[0] == "inspect":
		return string(mustJSON(map[string]any{"daemon_version": "v0.42.1", "agent": "shell", "image_digest": f.image, "kits": []any{}})), nil
	case strings.HasPrefix(joined, "policy allow network"):
		f.network = append(f.network, f.rule("allow", args[len(args)-1]))
		return "{}", nil
	case strings.HasPrefix(joined, "policy rm network"):
		kept := f.network[:0]
		for _, r := range f.network {
			if !jsonEqual(r["resources"], []any{args[len(args)-1]}) {
				kept = append(kept, r)
			}
		}
		f.network = kept
		return "{}", nil
	case strings.HasPrefix(joined, "policy deny network"):
		f.network = append(f.network, f.rule("deny", args[len(args)-1]))
		return "{}", nil
	}
	return "", errors.New("unexpected command " + joined)
}

type verifierFixture struct {
	t        *testing.T
	clock    *testClock
	registry *Registry
	value    map[string]any
	cli      *cliFixture
	verifier *SbxCliVerifier
}

func newVerifierFixture(t *testing.T) *verifierFixture {
	f := &verifierFixture{t: t, clock: &testClock{now: 1000, mono: 1000}}
	f.registry = newTestRegistry(t, t.TempDir(), f.clock, nil)
	t.Cleanup(func() { f.registry.Close() })
	f.value = runContext(nil)
	if _, err := f.registry.Register(f.value); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.BindGateway(f.value, 19443); err != nil {
		t.Fatal(err)
	}
	f.cli = newCliFixture()
	verifier, err := NewSbxCliVerifier(f.registry, "/trusted/sbx", true, f.cli.call)
	if err != nil {
		t.Fatal(err)
	}
	f.verifier = verifier
	f.registry.Verifier = verifier
	previous := GatewayHealthy
	GatewayHealthy = func(*Binding) bool { return true }
	t.Cleanup(func() { GatewayHealthy = previous })
	// Cleanups run last-in first-out: wait for background verifications
	// before the hook above is restored.
	t.Cleanup(func() { f.verifier.WaitIdle() })
	return f
}

func (f *verifierFixture) ready(phase string) bool {
	f.t.Helper()
	result, err := f.registry.Check(f.value, phase)
	if err != nil {
		f.t.Fatal(err)
	}
	return ready(result)
}

func (f *verifierFixture) mutations() []string {
	var out []string
	for _, args := range f.cli.list() {
		if len(args) >= 2 && args[0] == "policy" && (args[1] == "allow" || args[1] == "rm") {
			out = append(out, strings.Join(args, " "))
		}
	}
	return out
}

func TestCreateProvesGlobalDenyWithoutExecutingGuestOrChangingPolicy(t *testing.T) {
	f := newVerifierFixture(t)
	f.cli.runtime = false
	if !f.ready("create") {
		t.Fatal("create not ready")
	}
	f.verifier.WaitIdle()
	for _, args := range f.cli.list() {
		if args[0] == "create" || args[0] == "exec" || args[0] == "run" {
			t.Fatal("guest executed")
		}
	}
	if len(f.mutations()) != 0 {
		t.Fatal("policy mutated during create")
	}
}

func TestScopedTransitionAddsAllowBeforeRemovingBootstrapDeny(t *testing.T) {
	f := newVerifierFixture(t)
	if !f.ready("runtime") {
		t.Fatalf("runtime not ready: %s", f.verifier.LastFailure())
	}
	f.verifier.WaitIdle()
	mutations := f.mutations()
	if len(mutations) != 2 || mutations[0] != "policy allow network --sandbox sbx-one localhost:19443" || mutations[1] != "policy rm network --sandbox sbx-one --resource **" {
		t.Fatalf("mutations: %v", mutations)
	}
}

func TestLinuxImmutableDefaultDenySentinel(t *testing.T) {
	f := newVerifierFixture(t)
	sentinel := map[string]any{}
	for k, v := range linuxDefaultDeny {
		sentinel[k] = v
	}
	f.cli.globalNetwork = []map[string]any{sentinel}
	if !f.ready("runtime") {
		t.Fatalf("sentinel rejected: %s", f.verifier.LastFailure())
	}
	f.verifier.WaitIdle()
	if !f.ready("runtime") {
		t.Fatalf("sentinel rejected by verification: %s", f.verifier.LastFailure())
	}
	f.clock.advance(ProofSeconds + 1)
	allowed := map[string]any{}
	for k, v := range sentinel {
		allowed[k] = v
	}
	allowed["decision"] = "allow"
	f.cli.globalNetwork = []map[string]any{allowed}
	f.ready("runtime") // provisional: the binding was trusted so far
	f.verifier.WaitIdle()
	if f.ready("runtime") {
		t.Fatal("global allow accepted")
	}
}

func TestUnmanagedBootstrapStaysClosed(t *testing.T) {
	f := newVerifierFixture(t)
	f.verifier.ManageNetwork = false
	if f.ready("runtime") {
		t.Fatal("unmanaged bootstrap deny proved readiness")
	}
}

func TestRuntimeUUIDReplacementRejectedAfterCacheExpires(t *testing.T) {
	f := newVerifierFixture(t)
	if !f.ready("runtime") {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	f.clock.advance(ProofSeconds + 1)
	f.cli.uuid = "different-runtime"
	f.ready("runtime") // provisional until the background verification fails
	f.verifier.WaitIdle()
	if f.ready("runtime") {
		t.Fatal("replaced runtime accepted")
	}
	last := strings.Join(f.cli.list()[len(f.cli.list())-1], " ")
	if last != "policy deny network --sandbox sbx-one **" {
		t.Fatalf("last command %s", last)
	}
}

func TestUnexpectedPermissionsMCPOrForwardingRejected(t *testing.T) {
	f := newVerifierFixture(t)
	cases := []struct {
		name  string
		apply func()
		reset func()
	}{
		{"mcp", func() { f.cli.mcp = []any{map[string]any{"name": "host-shell"}} }, func() { f.cli.mcp = []any{} }},
		{"ssh", func() { f.cli.ssh = true }, func() { f.cli.ssh = false }},
		{"image", func() { f.cli.image = "sha256:wrong" }, func() { f.cli.image = SBXShellDigest }},
		{"network", func() { f.cli.network = []map[string]any{f.cli.rule("allow", "**")} }, func() { f.cli.network = []map[string]any{f.cli.rule("deny", "**")} }},
		{"global", func() {
			rule := f.cli.rule("allow", "**")
			rule["scope"] = "global"
			f.cli.globalNetwork = []map[string]any{rule}
		}, func() { f.cli.globalNetwork = nil }},
	}
	for _, c := range cases {
		c.apply()
		f.ready("runtime") // provisional at most until the background verification
		f.verifier.WaitIdle()
		if f.ready("runtime") {
			t.Fatalf("%s accepted", c.name)
		}
		c.reset()
	}
}

func TestReusesAttestationUntilProofExpiry(t *testing.T) {
	f := newVerifierFixture(t)
	if !f.ready("runtime") {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	count := len(f.cli.list())
	f.clock.advance(ProofSeconds - 1)
	if !f.ready("runtime") || len(f.cli.list()) != count {
		t.Fatal("attestation not reused")
	}
	f.clock.advance(2)
	if !f.ready("runtime") {
		t.Fatal("expired proof not reprovisioned")
	}
	f.verifier.WaitIdle()
	if len(f.cli.list()) <= count {
		t.Fatal("attestation not refreshed in the background")
	}
}

func (f *verifierFixture) hostCalls() int {
	n := 0
	for _, args := range f.cli.list() {
		if args[0] == "version" {
			n++
		}
	}
	return n
}

func (f *verifierFixture) sandboxCalls() int {
	n := 0
	for _, args := range f.cli.list() {
		joined := strings.Join(args, " ")
		if args[0] == "inspect" || joined == "ls --json" || strings.HasPrefix(joined, "policy ls sbx-one") || strings.Contains(joined, "--sandbox") {
			n++
		}
	}
	return n
}

func TestHostChecksAreSharedAcrossPhasesAndBindings(t *testing.T) {
	f := newVerifierFixture(t)
	f.cli.runtime = false
	if !f.ready("create") {
		t.Fatal("create not ready")
	}
	f.verifier.WaitIdle()
	f.cli.runtime = true
	if !f.ready("runtime") {
		t.Fatal("runtime not ready")
	}
	f.verifier.WaitIdle()
	if f.hostCalls() != 1 {
		t.Fatalf("host verified %d times across create and runtime phases", f.hostCalls())
	}
	other := runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"})
	if _, err := f.registry.Register(other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.BindGateway(other, 19444); err != nil {
		t.Fatal(err)
	}
	result, err := f.registry.Check(other, "create")
	f.verifier.WaitIdle()
	if err != nil || !ready(result) || f.hostCalls() != 1 {
		t.Fatalf("second binding re-verified the host: %v %v host=%d", result, err, f.hostCalls())
	}
}

func TestColdBeginAfterWarmWindowRunsOnlySandboxChecks(t *testing.T) {
	f := newRefresherFixture(t)
	if !f.begin(false) {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	if _, err := f.registry.End(f.value); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(WarmSeconds + 1)
	if f.verifier.RefreshOnce() != 0 {
		t.Fatal("cold binding refreshed")
	}
	f.cli.reset()
	f.value = runContext(map[string]any{"runID": "r2"}) // an ended run is never reused
	if !f.begin(false) {
		t.Fatal("cold begin failed")
	}
	f.verifier.WaitIdle()
	if f.hostCalls() != 0 || f.sandboxCalls() == 0 {
		t.Fatalf("cold begin: host calls %d sandbox calls %d commands=%v lastFailure=%q", f.hostCalls(), f.sandboxCalls(), f.cli.list(), f.verifier.LastFailure())
	}
}

func TestLiveGatewayHealthIsRequiredEvenWithCachedHostProof(t *testing.T) {
	f := newVerifierFixture(t)
	if !f.ready("runtime") {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	GatewayHealthy = func(*Binding) bool { return false }
	if f.ready("runtime") {
		t.Fatal("dead gateway accepted")
	}
}

// Refresher tests (ported from RefresherTests).

func newRefresherFixture(t *testing.T) *verifierFixture {
	f := newVerifierFixture(t)
	if _, err := f.registry.ConfigureProvider(f.value, "synthetic-secret-for-tests-only"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *verifierFixture) cliCalls() [][]string {
	var out [][]string
	for _, args := range f.cli.list() {
		if !(len(args) >= 2 && args[0] == "policy" && args[1] == "deny") {
			out = append(out, args)
		}
	}
	return out
}

func (f *verifierFixture) begin(renew bool) bool {
	f.t.Helper()
	result, err := f.registry.Begin(f.value, renew)
	if err != nil {
		f.t.Fatal(err)
	}
	return ready(result)
}

func TestWarmBindingIsReverifiedInBackgroundAndChatPathUsesCache(t *testing.T) {
	f := newRefresherFixture(t)
	if !f.ready("runtime") || !f.begin(false) {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	// The runner renews the lease every 20 s; renewals inside the proof
	// lifetime must not run the CLI.
	f.clock.advance(100)
	if !f.begin(true) {
		t.Fatal("renew failed")
	}
	f.clock.advance(RefreshSeconds - 100) // t=120: refresher cadence; the first proof is still valid
	f.cli.reset()
	if f.verifier.RefreshOnce() != 1 {
		t.Fatal("warm binding not refreshed")
	}
	if len(f.cliCalls()) == 0 {
		t.Fatal("background refresh must inspect the host")
	}
	f.cli.reset()
	f.clock.advance(90) // t=210, inside the lease renewed at t=100
	if !f.begin(true) {
		t.Fatal("renew failed")
	}
	f.clock.advance(ProofSeconds + 1 - RefreshSeconds - 90) // t=301: past the first proof; only the refreshed one is valid
	if !f.begin(true) || !f.ready("runtime") {
		t.Fatal("refreshed proof rejected")
	}
	f.verifier.WaitIdle()
	if len(f.cliCalls()) != 0 {
		t.Fatalf("chat path ran the CLI while the proof was fresh: %v", f.cliCalls())
	}
}

func TestColdBindingIsNotRefreshed(t *testing.T) {
	f := newRefresherFixture(t)
	// The host is always refreshed; a cold binding's sandbox is not.
	if f.verifier.RefreshOnce() != 0 || f.sandboxCalls() != 0 || f.hostCalls() != 1 {
		t.Fatalf("cold binding refreshed: host=%d sandbox=%d", f.hostCalls(), f.sandboxCalls())
	}
	if !f.ready("runtime") {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	f.clock.advance(WarmSeconds + 100) // warm window elapsed without a begin
	f.cli.reset()
	if f.verifier.RefreshOnce() != 0 || f.sandboxCalls() != 0 {
		t.Fatal("cold binding refreshed after warm window")
	}
}

func TestStaleProofReprovisionsAndVerifiesInBackground(t *testing.T) {
	f := newRefresherFixture(t)
	if !f.begin(false) {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	release := make(chan struct{})
	inner := f.cli.call
	f.verifier.Runner = func(args []string, denied bool) (string, error) {
		if strings.Join(args, " ") == "ls --json" {
			<-release // the background inspection blocks here; the chat path must not
		}
		return inner(args, denied)
	}
	for _, step := range []float64{100, 100, ProofSeconds + 1 - 200} {
		f.clock.advance(step) // renewals keep the lease alive; the proof lapses at ProofSeconds
		done := make(chan bool, 1)
		go func() { done <- f.begin(true) }()
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("renew failed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("renew waited for the inspection")
		}
	}
	f.cli.reset()
	close(release)
	f.verifier.WaitIdle()
	if len(f.cliCalls()) == 0 {
		t.Fatal("stale proof was not re-verified in the background")
	}
}

func TestProvisionalBeginDoesNotWaitForInspection(t *testing.T) {
	f := newRefresherFixture(t)
	release := make(chan struct{})
	inner := f.cli.call
	f.verifier.Runner = func(args []string, denied bool) (string, error) {
		if args[0] == "inspect" {
			<-release
		}
		return inner(args, denied)
	}
	done := make(chan bool, 1)
	go func() { done <- f.begin(false) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("provisional begin denied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("begin waited for the inspection")
	}
	if f.registry.Bindings["s1"].Lease == nil {
		t.Fatal("lease not active on provisional proof")
	}
	close(release)
	f.verifier.WaitIdle()
	if !f.ready("runtime") || f.sandboxCalls() == 0 {
		t.Fatal("background verification did not run or failed")
	}
}

func TestDistrustedBindingIsVerifiedSynchronously(t *testing.T) {
	f := newRefresherFixture(t)
	f.cli.mcp = []any{map[string]any{"name": "rogue"}}
	f.begin(false) // provisional
	f.verifier.WaitIdle()
	if f.ready("runtime") {
		t.Fatal("rogue host accepted")
	}
	f.cli.mcp = []any{}
	f.cli.reset()
	f.value = runContext(map[string]any{"runID": "r2"})
	if !f.begin(false) {
		t.Fatal("repaired host rejected")
	}
	if f.hostCalls() != 1 || f.sandboxCalls() == 0 {
		t.Fatalf("distrusted binding not verified synchronously: host=%d sandbox=%d", f.hostCalls(), f.sandboxCalls())
	}
	f.verifier.WaitIdle()
	f.clock.advance(ProofSeconds + 1)
	f.cli.reset()
	f.begin(true)
	if len(f.cli.list()) != 0 {
		t.Fatal("trust not restored after a passing verification")
	}
}

func TestFailedBackgroundVerificationRevokesLikeASynchronousOne(t *testing.T) {
	f := newRefresherFixture(t)
	if !f.begin(false) {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	f.cli.mcp = []any{map[string]any{"name": "rogue"}}
	if f.verifier.RefreshOnce() != 0 {
		t.Fatal("rogue MCP accepted")
	}
	if !strings.Contains(f.verifier.LastFailure(), "MCP") {
		t.Fatalf("last failure %q", f.verifier.LastFailure())
	}
	if f.ready("runtime") {
		t.Fatal("ready after failed background verification")
	}
	found := false
	for _, args := range f.cli.list() {
		if strings.Join(args, " ") == "policy deny network --sandbox sbx-one **" {
			found = true
		}
	}
	if !found {
		t.Fatal("deny rule not applied")
	}
}

func TestUnhealthyGatewayBypassesFreshCache(t *testing.T) {
	f := newRefresherFixture(t)
	if !f.begin(false) {
		t.Fatal("not ready")
	}
	f.verifier.WaitIdle()
	f.cli.reset()
	GatewayHealthy = func(*Binding) bool { return false }
	if f.ready("runtime") {
		t.Fatal("dead gateway accepted with fresh cache")
	}
}

func TestRefresherStartsAndStopsWithRegistryClose(t *testing.T) {
	f := newRefresherFixture(t)
	f.verifier.StartRefresher()
	f.verifier.StartRefresher()
	f.registry.Close()
	if f.verifier.stop != nil {
		t.Fatal("refresher still running after close")
	}
}

func TestVerifierAcceptsStockTemplateAndPinnedGuestImage(t *testing.T) {
	v := &SbxCliVerifier{ShellDigest: "sha256:" + strings.Repeat("a", 64)}
	if !v.allowedImage(SBXShellDigest) || !v.allowedImage(v.ShellDigest) {
		t.Fatal("stock template and pinned guest image must both be allowed")
	}
	if v.allowedImage("sha256:"+strings.Repeat("b", 64)) || v.allowedImage(nil) || v.allowedImage(42) {
		t.Fatal("other images must be refused")
	}
	if !ValidImageDigest(SBXShellDigest) || ValidImageDigest("sha256:short") || ValidImageDigest(strings.Repeat("a", 71)) {
		t.Fatal("digest validation")
	}
}

func TestVerifierStockDigestsPerArchitecture(t *testing.T) {
	amd64 := &SbxCliVerifier{ShellDigest: SBXShellDigest, StockDigests: release.StockTemplateDigests(release.AMD64)}
	arm64 := &SbxCliVerifier{ShellDigest: SBXShellDigest, StockDigests: release.StockTemplateDigests(release.ARM64)}
	index := release.StockTemplateDigest
	amdManifest, armManifest := release.StockTemplatePlatformDigests[release.AMD64], release.StockTemplatePlatformDigests[release.ARM64]
	if !ValidImageDigest(index) || !ValidImageDigest(amdManifest) || !ValidImageDigest(armManifest) || amdManifest == armManifest || amdManifest == index || armManifest == index {
		t.Fatal("stock digests must be three distinct sha256 digests")
	}
	if got := release.StockTemplateDigests(release.AMD64); len(got) != 1 || got[0] != index {
		t.Fatalf("amd64 stock set must be exactly the index digest, got %v", got)
	}
	if got := release.StockTemplateDigests("riscv64"); len(got) != 1 || got[0] != index {
		t.Fatalf("unknown architecture must get the index digest only, got %v", got)
	}
	if !amd64.allowedImage(index) || amd64.allowedImage(amdManifest) || amd64.allowedImage(armManifest) {
		t.Fatal("amd64 must accept the index digest only, as before")
	}
	if !arm64.allowedImage(index) || !arm64.allowedImage(armManifest) || arm64.allowedImage(amdManifest) {
		t.Fatal("arm64 must accept the index digest and the arm64 manifest digest")
	}
	if v, err := NewSbxCliVerifier(&Registry{State: t.TempDir(), Bindings: map[string]*Binding{}}, "sbx", false, func([]string, bool) (string, error) { return "", nil }); err != nil {
		t.Fatal(err)
	} else if len(v.StockDigests) == 0 || v.StockDigests[0] != index {
		t.Fatalf("new verifier must start from the host architecture's stock set, got %v", v.StockDigests)
	}
}
