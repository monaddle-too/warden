package policy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/release"
)

// SBXShellDigest is the stock template pin (chat/internal/release). The sbx
// version is not pinned: hostChecks only requires that the executable
// answers `sbx version` like sbx does.
const (
	// SBXShellDigest is the stock shell template's OCI index digest, which is
	// what sbx inspect reports for a sandbox the daemon pulled it for.
	SBXShellDigest = release.StockTemplateDigest

	ProofSeconds   = 300  // proof lifetime: a refreshed proof outlives two refresh cycles
	RefreshSeconds = 120  // background re-verification cadence (host checks and warm bindings)
	WarmSeconds    = 1200 // a binding stays warm this long after its last begin (longer than the runner's idle stop)
)

// CLIRunner executes the pinned SBX executable and returns stdout.
type CLIRunner func(args []string, denied bool) (string, error)

// GatewayHealthy probes a binding's gateway with a fresh HMAC challenge.
// It is a variable so tests can substitute it. Callers pass a snapshot of
// the binding's port, capability and identity.
var GatewayHealthy = func(b *Binding) bool {
	if b.GatewayPort == 0 {
		return false
	}
	nonce := randomHex(32)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}}
	res, err := client.Get("http://127.0.0.1:" + itoa(b.GatewayPort) + "/__warden_sbx_health/" + nonce)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4097))
	if err != nil || res.StatusCode != 200 {
		return false
	}
	parsed, err := StrictJSON(data)
	if err != nil {
		return false
	}
	value, _ := parsed.(map[string]any)
	mac := hmac.New(sha256.New, []byte(b.Capability))
	mac.Write([]byte(nonce))
	expected := hex.EncodeToString(mac.Sum(nil))
	got, _ := value["mac"].(string)
	return value["bindingID"] == b.Identity["sandboxID"] && got != "" && hmac.Equal([]byte(got), []byte(expected))
}

// SbxCliVerifier is the narrow sbx host verifier. It checks actual
// daemon policy, identity and gateway liveness; no guest report or worker
// flag contributes evidence. The chat path never waits for an inspection: a
// binding without a fresh proof gets a provisional one once its gateway is
// up and its gateway-only network rule is in place (that rule is
// configuration the broker owns, not evidence), and the full host and
// sandbox verification runs in the background, then every RefreshSeconds
// while the binding is warm. A failed verification clears every proof and
// re-applies the deny rule, and the failed binding gets no provisional
// trust again until a full verification passes.
type SbxCliVerifier struct {
	registry      *Registry
	Executable    string
	ManageNetwork bool
	Runner        CLIRunner
	// ShellDigest is the pinned Warden guest image digest. A verified runtime
	// may run it or the stock mountless shell template it is built from, so
	// sandboxes created before the image was installed keep working.
	ShellDigest string
	// StockDigests are the digests the stock template may report on this
	// host architecture (release.StockTemplateDigests): the index digest, and
	// on arm64 also the arm64 image manifest inside it. Nil means the index
	// digest only.
	StockDigests []string
	pinsPath     string
	pins         map[string]string // guarded by verifyMu
	// verifyMu serialises CLI inspection; the registry lock is not held
	// while a background inspection runs, so chats are not blocked by it.
	verifyMu    sync.Mutex
	cacheMu     sync.Mutex // guards cache and lastFailure
	cache       map[[3]string]*NetworkProof
	hostExpires float64 // guarded by cacheMu; host-wide checks valid until then
	lastFailure string
	pending     map[string]bool // bindings with a background verification in flight (cacheMu)
	configured  map[string]bool // bindings whose gateway-only rule was established (cacheMu)
	distrusted  map[string]bool // bindings that failed verification; no provisional trust (cacheMu)
	background  sync.WaitGroup
	stop        chan struct{}
	done        chan struct{}
	refresherMu sync.Mutex
}

func NewSbxCliVerifier(registry *Registry, executable string, manageNetwork bool, runner CLIRunner) (*SbxCliVerifier, error) {
	v := &SbxCliVerifier{registry: registry, Executable: executable, ManageNetwork: manageNetwork, Runner: runner, ShellDigest: SBXShellDigest,
		StockDigests: release.StockTemplateDigests(runtime.GOARCH),
		pinsPath:     filepath.Join(registry.State, "runtime-identities.json"), pins: map[string]string{}, cache: map[[3]string]*NetworkProof{},
		pending: map[string]bool{}, distrusted: map[string]bool{}, configured: map[string]bool{}}
	if v.Runner == nil {
		v.Runner = v.run
	}
	if raw, err := os.ReadFile(v.pinsPath); err == nil {
		parsed, err := StrictJSON(raw)
		if err != nil {
			return nil, err
		}
		pins, _ := parsed.(map[string]any)
		for name, value := range pins {
			id, _ := value.(string)
			v.pins[name] = id
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return v, nil
}

func (v *SbxCliVerifier) LastFailure() string {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	return v.lastFailure
}

// StartRefresher keeps proofs for warm bindings fresh off the chat path.
func (v *SbxCliVerifier) StartRefresher() {
	v.refresherMu.Lock()
	defer v.refresherMu.Unlock()
	if v.stop != nil {
		return
	}
	v.stop = make(chan struct{})
	v.done = make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		ticker := time.NewTicker(RefreshSeconds * time.Second)
		defer ticker.Stop()
		// Verify the host immediately so the first chat finds a warm proof.
		_ = v.RefreshOnce()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = v.RefreshOnce()
			}
		}
	}(v.stop, v.done)
}

// WaitIdle waits for in-flight background verifications (tests and close).
func (v *SbxCliVerifier) WaitIdle() { v.background.Wait() }

// StopRefresher stops the background loop and waits for it.
func (v *SbxCliVerifier) StopRefresher() {
	v.refresherMu.Lock()
	stop, done := v.stop, v.done
	v.stop, v.done = nil, nil
	v.refresherMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	v.background.Wait()
}

func (v *SbxCliVerifier) warm(b *Binding) bool {
	now := v.registry.Clock()
	if b.Lease != nil && now < b.Lease.ExpiresAt {
		return true
	}
	return b.Begun && now < b.LastBegin+WarmSeconds
}

// RefreshOnce re-verifies the host and every warm binding and returns how
// many bindings passed.
func (v *SbxCliVerifier) RefreshOnce() int {
	v.registry.mu.Lock()
	var targets []map[string]string
	for _, b := range v.registry.Bindings {
		if v.warm(b) {
			identity := map[string]string{}
			for k, val := range b.Identity {
				identity[k] = val
			}
			targets = append(targets, identity)
		}
	}
	v.registry.mu.Unlock()
	v.verifyMu.Lock()
	hostErr := v.hostChecks(true)
	v.verifyMu.Unlock()
	if hostErr != nil {
		// A host that no longer satisfies the policy invalidates every
		// proof; warm sandboxes get the deny rule like a per-sandbox failure.
		v.verifyMu.Lock()
		for _, identity := range targets {
			v.failed(identity, hostErr)
		}
		if len(targets) == 0 {
			v.failed(map[string]string{}, hostErr)
		}
		v.verifyMu.Unlock()
		return 0
	}
	refreshed := 0
	for _, identity := range targets {
		v.verifyMu.Lock()
		_, err := v.verify(identity, "runtime", true, false)
		if err == nil {
			v.setFailure("")
			refreshed++
		} else {
			v.failed(identity, err)
		}
		v.verifyMu.Unlock()
	}
	return refreshed
}

func (v *SbxCliVerifier) setFailure(text string) {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	v.lastFailure = text
}

// withRegistry runs fn with the registry lock held, taking it unless the
// caller already holds it (the synchronous verification path).
func (v *SbxCliVerifier) withRegistry(locked bool, fn func()) {
	if !locked {
		v.registry.mu.Lock()
		defer v.registry.mu.Unlock()
	}
	fn()
}

// snapshot captures what verification needs from the registry: the binding,
// its gateway (started if necessary) and the policy digest.
func (v *SbxCliVerifier) snapshot(identity map[string]string, phase string, locked bool) (probe *Binding, digest string, err error) {
	v.withRegistry(locked, func() {
		b := v.registry.Bindings[identity["sandboxID"]]
		if b == nil || !sameIdentity(b.Identity, identity) {
			err = errors.New("unknown binding")
			return
		}
		if v.registry.GatewayPool != nil {
			if err = v.registry.GatewayPool.Ensure(b); err != nil {
				return
			}
		}
		probe = &Binding{Identity: b.Identity, Capability: b.Capability, GatewayPort: b.GatewayPort}
		digest = b.Engine.PolicyDigest()
	})
	return probe, digest, err
}

// fresh returns a still-valid cached proof once the gateway answered a live
// probe; no CLI runs on this path.
func (v *SbxCliVerifier) fresh(identity map[string]string, phase string, locked bool) *NetworkProof {
	var probe *Binding
	var digest string
	v.withRegistry(locked, func() {
		b := v.registry.Bindings[identity["sandboxID"]]
		if b == nil || !sameIdentity(b.Identity, identity) {
			return
		}
		probe = &Binding{Identity: b.Identity, Capability: b.Capability, GatewayPort: b.GatewayPort}
		digest = b.Engine.PolicyDigest()
	})
	if probe == nil {
		return nil
	}
	v.cacheMu.Lock()
	cached := v.cache[[3]string{BindingDigest(identity), phase, digest}]
	v.cacheMu.Unlock()
	if cached != nil && v.registry.Clock() < cached.ExpiresAt && GatewayHealthy(probe) {
		return cached
	}
	return nil
}

func (v *SbxCliVerifier) run(args []string, denied bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.Executable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, limit: 1024*1024 + 1}
	cmd.Stderr = &limitedWriter{w: &stderr, limit: 4096}
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !(denied && errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
			return "", errors.New("SBX inspection failed")
		}
	}
	if stdout.Len() > 1024*1024 {
		return "", errors.New("SBX inspection failed")
	}
	return stdout.String(), nil
}

func (v *SbxCliVerifier) json(args []string, denied bool) (map[string]any, error) {
	out, err := v.Runner(args, denied)
	if err != nil {
		return nil, err
	}
	parsed, err := StrictJSON([]byte(out))
	if err != nil {
		return nil, err
	}
	value, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("unrecognized policy schema")
	}
	return value, nil
}

var linuxDefaultDeny = map[string]any{
	"id": "default-deny-all", "name": "default-deny-all", "policy_name": "default-deny-all", "scope": "global",
	"applies_to": "all", "resource_type": "network", "decision": "deny", "resources": []any{"**"}, "origin": "local",
	"layer": "local", "status": "active", "editable": false,
}

func (v *SbxCliVerifier) rules(name string) ([]map[string]any, error) {
	args := []string{"policy", "ls"}
	if name != "" {
		args = append(args, name)
	}
	args = append(args, "--json", "--include-inactive")
	result, err := v.json(args, false)
	if err != nil {
		return nil, err
	}
	list, ok := result["rules"].([]any)
	if !sameKeys(result, "rules") || !ok {
		return nil, errors.New("unrecognized policy schema")
	}
	var relevant []map[string]any
	for _, item := range list {
		rule, _ := item.(map[string]any)
		kind, _ := rule["resource_type"].(string)
		if kind == "filesystem:read" || kind == "filesystem:write" {
			continue
		}
		if kind != "network" || rule["status"] != "active" || rule["layer"] != "local" {
			return nil, errors.New("unknown or governed network policy")
		}
		// Linux SBX 0.42.1 materializes its immutable implicit-deny sentinel
		// in policy ls. It grants nothing; check still proves implicit denial
		// and absence of governance independently.
		if jsonEqual(rule, linuxDefaultDeny) {
			continue
		}
		if name == "" && rule["scope"] != "global" {
			continue
		}
		relevant = append(relevant, rule)
	}
	return relevant, nil
}

func (v *SbxCliVerifier) check(name, target string, allowed bool) error {
	args := []string{"policy", "check", "network", "--json"}
	if name != "" {
		args = append(args, "--sandbox", name)
	}
	args = append(args, target)
	result, err := v.json(args, !allowed)
	if err != nil {
		return err
	}
	governance, _ := result["governance"].(map[string]any)
	if result["allowed"] != allowed || !sameKeys(governance, "active") || governance["active"] != false {
		return errors.New("policy result or governance mismatch")
	}
	if !allowed && result["deny_kind"] != "implicit" {
		return errors.New("default-deny policy required")
	}
	return nil
}

func (v *SbxCliVerifier) pin(name string, id any) error {
	uuid, ok := id.(string)
	if !ok || uuid == "" {
		return errors.New("missing runtime UUID")
	}
	if existing, ok := v.pins[name]; ok {
		if existing != uuid {
			return errors.New("runtime UUID changed")
		}
		return nil
	}
	updated := map[string]any{}
	for k, val := range v.pins {
		updated[k] = val
	}
	updated[name] = uuid
	if err := atomicWrite(v.pinsPath, v.pinsPath+".tmp", []byte(Dumps(updated))); err != nil {
		v.registry.StorageFailed = true
		return err
	}
	v.pins[name] = uuid
	return nil
}

// Verify is called with the registry lock held. A fresh proof with a healthy
// gateway needs no CLI inspection. Without one, a trusted binding gets a
// provisional proof and a background verification; a binding that failed
// before is verified synchronously.
func (v *SbxCliVerifier) Verify(identity map[string]string, phase string) (*NetworkProof, error) {
	if cached := v.fresh(identity, phase, true); cached != nil {
		return cached, nil
	}
	key := BindingDigest(identity)
	v.cacheMu.Lock()
	distrusted := v.distrusted[key]
	v.cacheMu.Unlock()
	if distrusted {
		v.verifyMu.Lock()
		defer v.verifyMu.Unlock()
		if cached := v.fresh(identity, phase, true); cached != nil {
			return cached, nil
		}
		proof, err := v.verify(identity, phase, false, true)
		if err != nil {
			v.failed(identity, err)
			return nil, err
		}
		v.setFailure("")
		return proof, nil
	}
	proof, err := v.provision(identity, phase)
	if err != nil {
		v.verifyMu.Lock()
		v.failed(identity, err)
		v.verifyMu.Unlock()
		return nil, err
	}
	v.startBackground(identity, phase)
	return proof, nil
}

// provision configures what the broker owns (a running gateway and the
// sandbox's gateway-only rule) and issues a provisional proof. The sandbox
// was just created with the bootstrap deny rule by the trusted runner, so it
// is assumed to be otherwise configured correctly until verified.
func (v *SbxCliVerifier) provision(identity map[string]string, phase string) (*NetworkProof, error) {
	probe, digest, err := v.snapshot(identity, phase, true)
	if err != nil {
		return nil, err
	}
	if !GatewayHealthy(probe) {
		return nil, errors.New("gateway unavailable")
	}
	key := BindingDigest(identity)
	v.cacheMu.Lock()
	configured := v.configured[key]
	v.cacheMu.Unlock()
	if phase == "runtime" && !configured {
		v.verifyMu.Lock()
		err = v.establishSandboxRule(identity["runtimeName"], probe.GatewayPort)
		v.verifyMu.Unlock()
		if err != nil {
			return nil, err
		}
		v.cacheMu.Lock()
		v.configured[key] = true
		v.cacheMu.Unlock()
	}
	proof := &NetworkProof{Binding: key, Phase: phase, PolicyDigest: digest, ExpiresAt: v.registry.Clock() + ProofSeconds, EvidenceID: "sbx-provisional-pending-verification", GatewayPort: probe.GatewayPort}
	v.cacheMu.Lock()
	v.cache[[3]string{proof.Binding, phase, digest}] = proof
	v.cacheMu.Unlock()
	return proof, nil
}

// startBackground verifies a provisionally trusted binding without blocking
// the caller. At most one verification per binding is in flight.
func (v *SbxCliVerifier) startBackground(identity map[string]string, phase string) {
	key := BindingDigest(identity)
	v.cacheMu.Lock()
	if v.pending[key] {
		v.cacheMu.Unlock()
		return
	}
	v.pending[key] = true
	v.cacheMu.Unlock()
	v.background.Add(1)
	go func() {
		defer v.background.Done()
		v.verifyMu.Lock()
		_, err := v.verify(identity, phase, true, false)
		if err != nil {
			v.failed(identity, err)
		} else {
			v.setFailure("")
		}
		v.verifyMu.Unlock()
		v.cacheMu.Lock()
		delete(v.pending, key)
		v.cacheMu.Unlock()
	}()
}

// establishSandboxRule moves a sandbox from its bootstrap deny rule to the
// single gateway-only allow rule, or confirms that rule is already the only
// one. It runs with verifyMu held.
func (v *SbxCliVerifier) establishSandboxRule(name string, port int) error {
	resource := "localhost:" + itoa(port)
	rules, err := v.rules(name)
	if err != nil {
		return err
	}
	var bootstrap, allowed []map[string]any
	for _, rule := range rules {
		if rule["scope"] != "sandbox:"+name || rule["sandbox_id"] != name || rule["origin"] != "scoped" {
			return errors.New("policy outside owned sandbox")
		}
		switch {
		case rule["decision"] == "deny" && jsonEqual(rule["resources"], []any{"**"}):
			bootstrap = append(bootstrap, rule)
		case rule["decision"] == "allow" && jsonEqual(rule["resources"], []any{resource}):
			allowed = append(allowed, rule)
		default:
			return errors.New("unexpected sandbox permission")
		}
	}
	if v.ManageNetwork && len(bootstrap) > 0 {
		if len(allowed) == 0 {
			if _, err = v.Runner([]string{"policy", "allow", "network", "--sandbox", name, resource}, false); err != nil {
				return err
			}
		}
		// Deny remains active while the sole gateway exception is added.
		if _, err = v.Runner([]string{"policy", "rm", "network", "--sandbox", name, "--resource", "**"}, false); err != nil {
			return err
		}
		rules, err = v.rules(name)
		if err != nil {
			return err
		}
		if len(rules) != 1 || rules[0]["decision"] != "allow" || !jsonEqual(rules[0]["resources"], []any{resource}) {
			return errors.New("gateway policy transition failed")
		}
	} else if len(bootstrap) > 0 || len(allowed) != 1 {
		return errors.New("gateway-only policy not established")
	}
	return nil
}

// failed records the failure, clears every proof and re-applies the deny
// rule. It runs with verifyMu held.
func (v *SbxCliVerifier) failed(identity map[string]string, err error) {
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	v.cacheMu.Lock()
	v.lastFailure = text
	v.cache = map[[3]string]*NetworkProof{}
	v.hostExpires = 0
	if identity["sandboxID"] != "" {
		v.distrusted[BindingDigest(identity)] = true
		delete(v.configured, BindingDigest(identity))
	}
	v.cacheMu.Unlock()
	if _, pinned := v.pins[identity["runtimeName"]]; v.ManageNetwork && pinned {
		// Worker must also stop on failed readiness; no success is reported.
		_, _ = v.Runner([]string{"policy", "deny", "network", "--sandbox", identity["runtimeName"], "**"}, false)
	}
}

// hostChecks verifies the host-wide invariants (pinned daemon version, safe
// settings, no MCP servers, no global network permissions, implicit denial)
// and caches the result for ProofSeconds. It runs with verifyMu held.
func (v *SbxCliVerifier) hostChecks(force bool) error {
	v.cacheMu.Lock()
	fresh := v.registry.Clock() < v.hostExpires
	v.cacheMu.Unlock()
	if fresh && !force {
		return nil
	}
	version, err := v.Runner([]string{"version"}, false)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(version), "sbx version: ") {
		return errors.New("not an sbx executable: `sbx version` did not answer")
	}
	for _, setting := range []struct {
		key      string
		required any
	}{{"ssh.agentForwardingEnabled", false}, {"proxy.sandbox", "direct"}} {
		result, err := v.json([]string{"settings", "get", "--json", setting.key}, false)
		if err != nil {
			return err
		}
		if result["key"] != setting.key || result["value"] != setting.required {
			return errors.New("unsafe SBX setting")
		}
	}
	mcp, err := v.json([]string{"mcp", "ls", "--json"}, false)
	if err != nil {
		return err
	}
	servers, ok := mcp["servers"].([]any)
	gateway, _ := mcp["gateway"].(map[string]any)
	if !ok || len(servers) != 0 || gateway["local"] != true {
		return errors.New("host MCP gateway must have no servers")
	}
	global, err := v.rules("")
	if err != nil {
		return err
	}
	if len(global) > 0 {
		return errors.New("global network permissions unsupported")
	}
	if err = v.check("", "example.com:443", false); err != nil {
		return err
	}
	v.cacheMu.Lock()
	v.hostExpires = v.registry.Clock() + ProofSeconds
	v.cacheMu.Unlock()
	return nil
}

// verify inspects the sandbox (and the host when its proof lapsed). It runs
// with verifyMu held; locked says whether the caller also holds the
// registry lock.
func (v *SbxCliVerifier) verify(identity map[string]string, phase string, force, locked bool) (*NetworkProof, error) {
	probe, digest, err := v.snapshot(identity, phase, locked)
	if err != nil {
		return nil, err
	}
	if !GatewayHealthy(probe) {
		return nil, errors.New("gateway unavailable")
	}
	key := [3]string{BindingDigest(identity), phase, digest}
	// Recheck host policy frequently without spawning a CLI per audit event.
	if !force {
		v.cacheMu.Lock()
		cached := v.cache[key]
		v.cacheMu.Unlock()
		if cached != nil && v.registry.Clock() < cached.ExpiresAt {
			return cached, nil
		}
	}
	if err = v.hostChecks(false); err != nil {
		return nil, err
	}
	inventory, err := v.json([]string{"ls", "--json"}, false)
	if err != nil {
		return nil, err
	}
	rows, ok := inventory["sandboxes"].([]any)
	if !ok {
		return nil, errors.New("invalid runtime inventory")
	}
	var matching []map[string]any
	for _, item := range rows {
		row, _ := item.(map[string]any)
		if row["name"] == identity["runtimeName"] {
			matching = append(matching, row)
		}
	}
	if len(matching) > 1 {
		return nil, errors.New("ambiguous runtime identity")
	}
	if phase == "runtime" && len(matching) == 0 {
		return nil, errors.New("runtime absent")
	}
	if len(matching) == 1 {
		row := matching[0]
		if row["agent"] != "shell" || (row["status"] != "running" && row["status"] != "stopped") {
			return nil, errors.New("unsupported runtime")
		}
		details, err := v.json([]string{"inspect", identity["runtimeName"], "--json"}, false)
		if err != nil {
			return nil, err
		}
		kits, _ := details["kits"].([]any)
		daemonVersion, _ := details["daemon_version"].(string)
		if daemonVersion == "" || details["agent"] != "shell" || !v.allowedImage(details["image_digest"]) || kits == nil || len(kits) != 0 {
			return nil, errors.New("unsupported runtime profile")
		}
		for _, k := range []string{"workspace", "workspaces", "mounts", "static_mcp"} {
			if truthy(details[k]) {
				return nil, errors.New("unsupported runtime profile")
			}
		}
		if err = v.pin(identity["runtimeName"], row["id"]); err != nil {
			return nil, err
		}
	}
	if phase == "runtime" {
		name := identity["runtimeName"]
		resource := "localhost:" + itoa(probe.GatewayPort)
		if err = v.establishSandboxRule(name, probe.GatewayPort); err != nil {
			return nil, err
		}
		if err = v.check(name, resource, true); err != nil {
			return nil, err
		}
		for _, target := range []string{"example.com:443", "api.openai.com:443", "1.1.1.1:443", "localhost:18765"} {
			if err = v.check(name, target, false); err != nil {
				return nil, err
			}
		}
	}
	proof := &NetworkProof{Binding: BindingDigest(identity), Phase: phase, PolicyDigest: digest, ExpiresAt: v.registry.Clock() + ProofSeconds, EvidenceID: "sbx-cli-and-gateway", GatewayPort: probe.GatewayPort}
	v.cacheMu.Lock()
	v.cache[key] = proof
	delete(v.distrusted, key[0])
	if phase == "runtime" {
		v.configured[key[0]] = true
	}
	v.cacheMu.Unlock()
	return proof, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if n, ok := asNumber(v); ok {
		return n != 0
	}
	return true
}

// ValidImageDigest reports whether value is a sha256 OCI digest.
func ValidImageDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, c := range value[len("sha256:"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// allowedImage accepts the stock shell template, under any digest it may
// report on this architecture, and the pinned guest image.
func (v *SbxCliVerifier) allowedImage(digest any) bool {
	value, ok := digest.(string)
	if !ok {
		return false
	}
	if value == SBXShellDigest || value == v.ShellDigest {
		return true
	}
	for _, stock := range v.StockDigests {
		if value == stock {
			return true
		}
	}
	return false
}
