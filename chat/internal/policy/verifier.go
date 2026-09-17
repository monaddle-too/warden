package policy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	ProofSeconds   = 300  // proof lifetime: a refreshed proof outlives two refresh cycles
	RefreshSeconds = 120  // background re-verification cadence (cluster checks and warm bindings)
	WarmSeconds    = 1200 // a binding stays warm this long after its last begin (longer than the runner's idle stop)
)

// GatewayHealthy probes a loopback binding gateway with a fresh HMAC
// challenge. It is a variable so tests can substitute it. Callers pass a
// snapshot of the binding's port, capability and identity. A Gateway
// implementation with another listener shape answers Healthy itself.
var GatewayHealthy = func(b *Binding) bool {
	if b.GatewayPort == 0 {
		return false
	}
	return gatewayAnswers("http://127.0.0.1:"+itoa(b.GatewayPort), nil, b.Identity["sandboxID"], b.Capability)
}

// gatewayAnswers sends the health challenge to origin, with the request
// headers a dispatching gateway needs, and checks the HMAC over the nonce
// keyed by the binding capability.
func gatewayAnswers(origin string, headers map[string]string, bindingID, capability string) bool {
	nonce := randomHex(32)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}}
	req, err := http.NewRequest(http.MethodGet, origin+"/__warden_sbx_health/"+nonce, nil)
	if err != nil {
		return false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
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
	mac := hmac.New(sha256.New, []byte(capability))
	mac.Write([]byte(nonce))
	expected := hex.EncodeToString(mac.Sum(nil))
	got, _ := value["mac"].(string)
	return value["bindingID"] == bindingID && got != "" && hmac.Equal([]byte(got), []byte(expected))
}

// RuntimeVerifier proves that a binding's sandbox is enforced, through a
// SandboxInspector for the facts and the registry's Gateway for the
// gateway. The chat path never waits for an inspection: a binding without
// a fresh proof gets a provisional one once its gateway is up and its
// egress is granted to that gateway alone (a grant is configuration the
// broker owns, not evidence), and the full cluster and runtime
// verification runs in the background, then every RefreshSeconds while the
// binding is warm. A failed verification clears every proof and denies the
// runtime's egress again, and the failed binding gets no provisional trust
// until a full verification passes.
type RuntimeVerifier struct {
	registry  *Registry
	Inspector SandboxInspector
	// verifyMu serialises inspection; the registry lock is not held while
	// a background inspection runs, so chats are not blocked by it.
	verifyMu       sync.Mutex
	cacheMu        sync.Mutex // guards cache and lastFailure
	cache          map[[3]string]*NetworkProof
	clusterExpires float64 // guarded by cacheMu; cluster-wide checks valid until then
	lastFailure    string
	pending        map[string]bool // bindings with a background verification in flight (cacheMu)
	configured     map[string]bool // bindings whose egress grant was established (cacheMu)
	distrusted     map[string]bool // bindings that failed verification; no provisional trust (cacheMu)
	background     sync.WaitGroup
	stop           chan struct{}
	done           chan struct{}
	refresherMu    sync.Mutex
}

// SbxCliVerifier is the RuntimeVerifier of the sbx shapes; the name is kept
// for its callers.
type SbxCliVerifier = RuntimeVerifier

// NewRuntimeVerifier verifies the registry's bindings through inspector.
func NewRuntimeVerifier(registry *Registry, inspector SandboxInspector) *RuntimeVerifier {
	return &RuntimeVerifier{registry: registry, Inspector: inspector, cache: map[[3]string]*NetworkProof{},
		pending: map[string]bool{}, distrusted: map[string]bool{}, configured: map[string]bool{}}
}

// NewSbxCliVerifier builds the sbx shapes' verifier: a RuntimeVerifier over
// an SbxInspector on the pinned executable, with the runtime identity pins
// kept in the registry state. The inspector is reachable as
// Inspector.(*SbxInspector).
func NewSbxCliVerifier(registry *Registry, executable string, manageNetwork bool, runner CLIRunner) (*RuntimeVerifier, error) {
	inspector, err := NewSbxInspector(registry.State, executable, manageNetwork, runner)
	if err != nil {
		return nil, err
	}
	inspector.StorageFailed = func() { registry.StorageFailed = true }
	return NewRuntimeVerifier(registry, inspector), nil
}

func (v *RuntimeVerifier) LastFailure() string {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	return v.lastFailure
}

// StartRefresher keeps proofs for warm bindings fresh off the chat path.
func (v *RuntimeVerifier) StartRefresher() {
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
		// Verify the cluster immediately so the first chat finds a warm proof.
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
func (v *RuntimeVerifier) WaitIdle() { v.background.Wait() }

// StopRefresher stops the background loop and waits for it.
func (v *RuntimeVerifier) StopRefresher() {
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

func (v *RuntimeVerifier) warm(b *Binding) bool {
	now := v.registry.Clock()
	if b.Lease != nil && now < b.Lease.ExpiresAt {
		return true
	}
	return b.Begun && now < b.LastBegin+WarmSeconds
}

// RefreshOnce re-verifies the cluster and every warm binding and returns
// how many bindings passed.
func (v *RuntimeVerifier) RefreshOnce() int {
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
	clusterErr := v.clusterChecks(true)
	v.verifyMu.Unlock()
	if clusterErr != nil {
		// A cluster that no longer satisfies the policy invalidates every
		// proof; warm sandboxes get their egress denied like a per-sandbox
		// failure.
		v.verifyMu.Lock()
		for _, identity := range targets {
			v.failed(identity, clusterErr)
		}
		if len(targets) == 0 {
			v.failed(map[string]string{}, clusterErr)
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

func (v *RuntimeVerifier) setFailure(text string) {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	v.lastFailure = text
}

// withRegistry runs fn with the registry lock held, taking it unless the
// caller already holds it (the synchronous verification path).
func (v *RuntimeVerifier) withRegistry(locked bool, fn func()) {
	if !locked {
		v.registry.mu.Lock()
		defer v.registry.mu.Unlock()
	}
	fn()
}

// healthy probes the binding's gateway: through the registry's Gateway
// when one runs the gateways, else the loopback probe (tests that bind a
// port without a gateway substitute GatewayHealthy).
func (v *RuntimeVerifier) healthy(probe *Binding) bool {
	if v.registry.Gateways != nil {
		return v.registry.Gateways.Healthy(probe)
	}
	return GatewayHealthy(probe)
}

// snapshot captures what verification needs from the registry: the binding,
// its gateway endpoint (bound if necessary) and the policy digest.
func (v *RuntimeVerifier) snapshot(identity map[string]string, phase string, locked bool) (probe *Binding, digest string, err error) {
	v.withRegistry(locked, func() {
		b := v.registry.Bindings[identity["sandboxID"]]
		if b == nil || !sameIdentity(b.Identity, identity) {
			err = errors.New("unknown binding")
			return
		}
		if v.registry.Gateways != nil {
			var endpoint GatewayEndpoint
			if endpoint, err = v.registry.Gateways.Bind(b); err != nil {
				return
			}
			b.Endpoint = endpoint
		}
		probe = &Binding{Identity: b.Identity, Capability: b.Capability, GatewayPort: b.GatewayPort, Endpoint: b.endpoint()}
		digest = b.Engine.PolicyDigest()
	})
	return probe, digest, err
}

// fresh returns a still-valid cached proof once the gateway answered a live
// probe; no inspection runs on this path.
func (v *RuntimeVerifier) fresh(identity map[string]string, phase string, locked bool) *NetworkProof {
	var probe *Binding
	var digest string
	v.withRegistry(locked, func() {
		b := v.registry.Bindings[identity["sandboxID"]]
		if b == nil || !sameIdentity(b.Identity, identity) {
			return
		}
		probe = &Binding{Identity: b.Identity, Capability: b.Capability, GatewayPort: b.GatewayPort, Endpoint: b.endpoint()}
		digest = b.Engine.PolicyDigest()
	})
	if probe == nil {
		return nil
	}
	v.cacheMu.Lock()
	cached := v.cache[[3]string{BindingDigest(identity), phase, digest}]
	v.cacheMu.Unlock()
	if cached != nil && v.registry.Clock() < cached.ExpiresAt && v.healthy(probe) {
		return cached
	}
	return nil
}

// Verify is called with the registry lock held. A fresh proof with a healthy
// gateway needs no inspection. Without one, a trusted binding gets a
// provisional proof and a background verification; a binding that failed
// before is verified synchronously.
func (v *RuntimeVerifier) Verify(identity map[string]string, phase string) (*NetworkProof, error) {
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
// sandbox's egress grant to it) and issues a provisional proof. The sandbox
// was just created in its bootstrap deny state by the trusted runner, so it
// is assumed to be otherwise configured correctly until verified.
func (v *RuntimeVerifier) provision(identity map[string]string, phase string) (*NetworkProof, error) {
	probe, digest, err := v.snapshot(identity, phase, true)
	if err != nil {
		return nil, err
	}
	if !v.healthy(probe) {
		return nil, errors.New("gateway unavailable")
	}
	key := BindingDigest(identity)
	v.cacheMu.Lock()
	configured := v.configured[key]
	v.cacheMu.Unlock()
	if phase == "runtime" && !configured {
		v.verifyMu.Lock()
		err = v.Inspector.GrantEgress(context.Background(), identity, probe.Endpoint)
		v.verifyMu.Unlock()
		if err != nil {
			return nil, err
		}
		v.cacheMu.Lock()
		v.configured[key] = true
		v.cacheMu.Unlock()
	}
	proof := &NetworkProof{Binding: key, Phase: phase, PolicyDigest: digest, ExpiresAt: v.registry.Clock() + ProofSeconds, EvidenceID: "provisional-pending-verification", GatewayPort: probe.GatewayPort}
	v.cacheMu.Lock()
	v.cache[[3]string{proof.Binding, phase, digest}] = proof
	v.cacheMu.Unlock()
	return proof, nil
}

// startBackground verifies a provisionally trusted binding without blocking
// the caller. At most one verification per binding is in flight.
func (v *RuntimeVerifier) startBackground(identity map[string]string, phase string) {
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

// failed records the failure, clears every proof and denies the runtime's
// egress again. It runs with verifyMu held.
func (v *RuntimeVerifier) failed(identity map[string]string, err error) {
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	v.cacheMu.Lock()
	v.lastFailure = text
	v.cache = map[[3]string]*NetworkProof{}
	v.clusterExpires = 0
	if identity["sandboxID"] != "" {
		v.distrusted[BindingDigest(identity)] = true
		delete(v.configured, BindingDigest(identity))
	}
	v.cacheMu.Unlock()
	if identity["runtimeName"] != "" {
		_ = v.Inspector.DenyEgress(context.Background(), identity)
	}
}

// clusterChecks verifies the cluster-wide invariants through the inspector
// and caches a pass for ProofSeconds. It runs with verifyMu held.
func (v *RuntimeVerifier) clusterChecks(force bool) error {
	v.cacheMu.Lock()
	fresh := v.registry.Clock() < v.clusterExpires
	v.cacheMu.Unlock()
	if fresh && !force {
		return nil
	}
	if err := v.Inspector.ClusterFacts(context.Background()); err != nil {
		return err
	}
	v.cacheMu.Lock()
	v.clusterExpires = v.registry.Clock() + ProofSeconds
	v.cacheMu.Unlock()
	return nil
}

// verify inspects the sandbox (and the cluster when its proof lapsed). It
// runs with verifyMu held; locked says whether the caller also holds the
// registry lock.
func (v *RuntimeVerifier) verify(identity map[string]string, phase string, force, locked bool) (*NetworkProof, error) {
	probe, digest, err := v.snapshot(identity, phase, locked)
	if err != nil {
		return nil, err
	}
	if !v.healthy(probe) {
		return nil, errors.New("gateway unavailable")
	}
	key := [3]string{BindingDigest(identity), phase, digest}
	// Recheck cluster policy frequently without an inspection per audit event.
	if !force {
		v.cacheMu.Lock()
		cached := v.cache[key]
		v.cacheMu.Unlock()
		if cached != nil && v.registry.Clock() < cached.ExpiresAt {
			return cached, nil
		}
	}
	if err = v.clusterChecks(false); err != nil {
		return nil, err
	}
	ctx := context.Background()
	if phase == "runtime" {
		if err = v.Inspector.GrantEgress(ctx, identity, probe.Endpoint); err != nil {
			return nil, err
		}
	}
	facts, err := v.Inspector.Facts(ctx, identity)
	if err != nil {
		return nil, err
	}
	if len(facts.Violations) > 0 {
		return nil, errors.New(facts.Violations[0])
	}
	if phase == "runtime" {
		if !facts.Present {
			return nil, errors.New("runtime absent")
		}
		if facts.Egress != EgressGateway {
			return nil, errors.New("gateway-only policy not established")
		}
	}
	tier := facts.Tier
	if tier == "" {
		tier = "runtime"
	}
	proof := &NetworkProof{Binding: BindingDigest(identity), Phase: phase, PolicyDigest: digest, ExpiresAt: v.registry.Clock() + ProofSeconds, EvidenceID: tier + "-inspected-and-gateway", GatewayPort: probe.GatewayPort}
	v.cacheMu.Lock()
	v.cache[key] = proof
	delete(v.distrusted, key[0])
	if phase == "runtime" {
		v.configured[key[0]] = true
	}
	v.cacheMu.Unlock()
	return proof, nil
}
