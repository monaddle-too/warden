package policy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"warden/chat/internal/bugreport"
	"warden/chat/internal/release"
	"warden/chat/internal/transport"
)

var contextKeys = []string{"projectID", "sandboxID", "runtimeName", "generation", "chatID", "runID", "principalID"}

// IdentityKeys are the immutable binding identity fields.
var IdentityKeys = []string{"projectID", "sandboxID", "runtimeName", "generation", "principalID"}

var identifierShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// imageDigestShape is the optional context key imageDigest: the image a
// sandbox the runner derived from a snapshot of a verified guest runs (a
// workspace copy, a regeneration at a new size), which the SBX
// inspector's image pin accepts for that binding on the runner's word
// (the runner is trusted infrastructure with the daemon in hand; the pin
// guards the daemon's state, not the runner).
var imageDigestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	MaxControlMessage = 12 * 1024 * 1024
	LeaseSeconds      = 120
)

// ProviderRoutes are the guest-facing provider API paths.
var ProviderRoutes = stringSet("/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens")

// NetworkProof is a trusted in-process verifier result; it is never decoded
// from worker messages.
type NetworkProof struct {
	Binding      string
	Phase        string
	PolicyDigest string
	ExpiresAt    float64
	EvidenceID   string
	GatewayPort  int
}

// Verifier proves that a sandbox's network is enforced for a phase.
type Verifier interface {
	Verify(identity map[string]string, phase string) (*NetworkProof, error)
	LastFailure() string
}

// UnsupportedVerifier never proves anything: installing the broker alone
// deliberately cannot enable sandbox execution.
type UnsupportedVerifier struct{}

func (UnsupportedVerifier) Verify(map[string]string, string) (*NetworkProof, error) {
	return nil, nil
}
func (UnsupportedVerifier) LastFailure() string { return "" }

// Lease is the active run holding a binding's capabilities.
type Lease struct {
	Provider  string
	RunID     string
	ChatID    string
	ExpiresAt float64
}

// Binding is one registered sandbox with its engine and gateway state.
type Binding struct {
	Identity       map[string]string
	Engine         *Engine
	Capability     string
	Lease          *Lease
	Ended          map[string]bool
	Decisions      map[string]map[string]any
	ProviderSecret string
	// GatewayPort is the binding's loopback gateway port (gateway-port.json)
	// on the sbx shapes, or the shared listener's port; a proof names it.
	GatewayPort int
	// Endpoint is what the registry's Gateway bound for this binding, which
	// Begin advertises; unset until Bind ran in this process.
	Endpoint GatewayEndpoint
	// LastBegin (when Begun) keeps the binding warm for background
	// re-verification after its lease ends.
	LastBegin float64
	Begun     bool
	// ImageDigest is the snapshot image the runner says this sandbox runs
	// (context key imageDigest; "" on the pinned guest image), handed to
	// the inspector with the identity so its image pin accepts it.
	ImageDigest string
}

// endpoint is the gateway endpoint Begin advertises: the one the Gateway
// bound, else the loopback endpoint of the recorded port (a registry
// without a Gateway, as the sbx control tests run it).
func (b *Binding) endpoint() GatewayEndpoint {
	if b.Endpoint.Port != 0 {
		return b.Endpoint
	}
	return LoopbackEndpoint(b.GatewayPort)
}

// RegistryOptions configures the registry.
type RegistryOptions struct {
	Operations      *Operations
	PolicyTemplate  string
	GitHubAppConfig string // WARDEN_GITHUB_APP_BROKER path, may be empty
	GitHubAuthFile  string // --github-auth-file user token path, may be empty; exclusive with GitHubAppConfig
	EgressMode      string // "" (the template decides), "restricted" or "public"; see EngineOptions.EgressMode
	Clock           Clock  // monotonic clock for leases/proofs
	Verifier        Verifier
	ProviderSource  ProviderSource
	ClaudeSource    ProviderSource
	DocumentAPI     *DocumentAPI
	Networks        []*net.IPNet
	// CA is the host-wide gateway CA; when nil it is loaded or created under
	// <state>/gateway-ca.
	CA *GatewayCA
}

// Registry is the private SBX control and credential broker.
type Registry struct {
	State          string
	Verifier       Verifier
	ProviderSource ProviderSource
	ClaudeSource   ProviderSource
	DocumentAPI    *DocumentAPI
	Sharing        *Sharing
	// Gateways runs the bindings' gateways (LoopbackGateways on the sbx
	// shapes, SharedGateway behind one listener); nil in tests that bind
	// a port by hand.
	Gateways Gateway
	Clock    Clock
	// CA signs every gateway's leaf certificates; its public certificate is
	// what guests install.
	CA            *GatewayCA
	Bindings      map[string]*Binding
	StorageFailed bool
	Retired       map[string][]string
	options       RegistryOptions
	// egressOverrides holds the sandboxes with an egress mode of their own
	// (the workspace's choice, by sandbox ID), which wins over the
	// install's mode; persisted as egressOverridesFile.
	egressOverrides map[string]string
	mu              sync.Mutex
	// hostAudit is the install-wide audit log for the jailbreak's host
	// events (<state>/audit/events.jsonl, its own hash chain; opened on
	// the first event), beside the sandbox's own chain when it is bound.
	hostAudit *Audit
}

// HostEvents are the jailbreak's audit event types (chats/host.go), the
// only ones the sharing host_event operation records.
var HostEvents = map[string]bool{"host.exec": true, "host.file": true, "host.expose": true, "workspace.jailbreak": true}

// EmitHostEvent records one host event (docs/host-dogfood-plan.md) in the
// install-wide audit chain and, when the sandbox is bound, in that
// sandbox's own chain, so the record of what an agent did on the host
// survives beside the gateway's.
func (r *Registry) EmitHostEvent(sandbox, event string, fields map[string]any) error {
	if !HostEvents[event] {
		return errors.New("unknown host event")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hostAudit == nil {
		audit, err := NewAudit(filepath.Join(r.State, "audit", "events.jsonl"), NewRedactor())
		if err != nil {
			return err
		}
		r.hostAudit = audit
	}
	if _, err := r.hostAudit.EmitSeverity(event, "warning", fields); err != nil {
		return err
	}
	if b := r.Bindings[sandbox]; b != nil && b.Engine != nil && b.Engine.Audit != nil {
		if _, err := b.Engine.Audit.EmitSeverity(event, "warning", fields); err != nil {
			return err
		}
	}
	return nil
}

// egressFile persists an egress mode chosen from the console under the
// registry state; when present it overrides the configured mode at start.
const egressFile = "egress.json"

// EgressMode reports the current egress mode ("restricted" or "public") and
// where it came from: "console" when set through SetEgressMode and
// persisted, else "config".
func (r *Registry) EgressMode() (mode, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mode = r.options.EgressMode
	if mode == "" {
		mode = "restricted"
	}
	source = "config"
	if _, err := os.Stat(filepath.Join(r.State, egressFile)); err == nil {
		source = "console"
	}
	return mode, source
}

// SetEgressMode applies an egress mode to every live sandbox engine and to
// the ones created later, and persists it so the choice survives restarts
// (it then overrides sandboxes.egress in warden.json until cleared with
// ClearEgressMode). A sandbox with a mode of its own (SetSandboxEgress)
// keeps it.
func (r *Registry) SetEgressMode(mode string) error {
	if mode != "restricted" && mode != "public" {
		return errors.New("egress mode must be restricted or public")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for sandbox, b := range r.Bindings {
		if b.Engine != nil && r.egressOverrides[sandbox] == "" {
			if err := b.Engine.SetEgressMode(mode); err != nil {
				return err
			}
		}
	}
	r.options.EgressMode = mode
	tmp := filepath.Join(r.State, egressFile+".tmp")
	if err := os.WriteFile(tmp, []byte(Dumps(map[string]any{"mode": mode})+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.State, egressFile))
}

// AllowHost applies an owner-approved temporary egress grant to the sandbox
// with this ID (its current binding's engine).
func (r *Registry) AllowHost(sandbox, host string, until float64) error {
	r.mu.Lock()
	b := r.Bindings[sandbox]
	r.mu.Unlock()
	if b == nil || b.Engine == nil {
		return errors.New("sandbox is not registered with the policy service")
	}
	return b.Engine.AllowHost(host, until)
}

// LoadEgressMode reads a mode persisted by SetEgressMode, or "" when none.
func LoadEgressMode(state string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(state, egressFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	parsed, err := StrictJSON(raw)
	if err != nil {
		return "", err
	}
	m, _ := parsed.(map[string]any)
	mode, _ := m["mode"].(string)
	if mode != "restricted" && mode != "public" {
		return "", errors.New(egressFile + ": mode must be restricted or public")
	}
	return mode, nil
}

// egressOverridesFile persists the sandboxes' own egress modes under the
// registry state: {"<sandboxID>": "restricted" | "public"}.
const egressOverridesFile = "egress-overrides.json"

func loadEgressOverrides(state string) (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(state, egressOverridesFile))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	parsed, err := StrictJSON(raw)
	if err != nil {
		return nil, err
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New(egressOverridesFile + ": invalid")
	}
	for sandbox, value := range m {
		mode, _ := value.(string)
		if !validIdentifier(sandbox) || (mode != "restricted" && mode != "public") {
			return nil, errors.New(egressOverridesFile + ": mode must be restricted or public")
		}
		out[sandbox] = mode
	}
	return out, nil
}

func (r *Registry) saveEgressOverridesLocked() error {
	body := map[string]any{}
	for sandbox, mode := range r.egressOverrides {
		body[sandbox] = mode
	}
	tmp := filepath.Join(r.State, egressOverridesFile+".tmp")
	if err := os.WriteFile(tmp, []byte(Dumps(body)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.State, egressOverridesFile))
}

// effectiveEgressLocked is the mode a sandbox's engine gets: its own when
// it has one, else the install's (options.EgressMode; "" leaves the
// template's, which the engine reads as restricted).
func (r *Registry) effectiveEgressLocked(sandbox string) string {
	if mode := r.egressOverrides[sandbox]; mode != "" {
		return mode
	}
	return r.options.EgressMode
}

// SandboxEgress reports a sandbox's own egress mode ("" when it follows
// the install) and the mode in effect for it.
func (r *Registry) SandboxEgress(sandbox string) (own, effective string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	own = r.egressOverrides[sandbox]
	effective = r.effectiveEgressLocked(sandbox)
	if effective == "" {
		effective = "restricted"
	}
	return own, effective
}

// SetSandboxEgress gives one sandbox an egress mode of its own ("restricted"
// or "public"), or with "" returns it to the install's mode. The sandbox
// need not be registered yet: the choice is keyed by sandbox ID, applied
// to its live engine now and to every engine created for it later, and
// persisted so it survives the sandbox's regeneration and a restart.
func (r *Registry) SetSandboxEgress(sandbox, mode string) error {
	if !validIdentifier(sandbox) {
		return errors.New("invalid sandbox")
	}
	if mode != "" && mode != "restricted" && mode != "public" {
		return errors.New("egress mode must be restricted, public or empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if mode == "" {
		delete(r.egressOverrides, sandbox)
	} else {
		r.egressOverrides[sandbox] = mode
	}
	if b := r.Bindings[sandbox]; b != nil && b.Engine != nil {
		effective := r.effectiveEgressLocked(sandbox)
		if effective == "" {
			effective = "restricted"
		}
		if err := b.Engine.SetEgressMode(effective); err != nil {
			return err
		}
	}
	return r.saveEgressOverridesLocked()
}

// EgressOverrides counts the sandboxes with a mode of their own.
func (r *Registry) EgressOverrides() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.egressOverrides)
}

// ValidateContext checks a worker-supplied run context.
func ValidateContext(value any) (map[string]string, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid context")
	}
	expected := len(contextKeys)
	if _, hasProvider := m["provider"]; hasProvider {
		expected++
	}
	if _, hasDigest := m["imageDigest"]; hasDigest {
		expected++
	}
	if len(m) != expected {
		return nil, errors.New("invalid context")
	}
	for _, key := range contextKeys {
		if _, ok := m[key]; !ok {
			return nil, errors.New("invalid context")
		}
	}
	out := map[string]string{}
	for k, v := range m {
		s, ok := v.(string)
		if k == "imageDigest" {
			if !ok || !imageDigestShape.MatchString(s) {
				return nil, errors.New("invalid image digest")
			}
			out[k] = s
			continue
		}
		if !ok || !identifierShape.MatchString(s) {
			return nil, errors.New("invalid context identifier")
		}
		out[k] = s
	}
	if provider, ok := out["provider"]; ok && provider != "codex" && provider != "claude" {
		return nil, errors.New("invalid provider")
	}
	return out, nil
}

func identityOf(value map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range IdentityKeys {
		out[key] = value[key]
	}
	return out
}

func sameIdentity(a, b map[string]string) bool {
	for _, key := range IdentityKeys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}

// BindingDigest names a binding's private state directory.
func BindingDigest(value map[string]string) string {
	return sha256Hex([]byte(Dumps(identityOf(value))))
}

func providerOf(value map[string]string) string {
	if p, ok := value["provider"]; ok {
		return p
	}
	return "codex"
}

// NewRegistry opens the durable binding manifest and each engine.
func NewRegistry(state string, options RegistryOptions) (*Registry, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, err
	}
	ca := options.CA
	if ca == nil {
		var err error
		if ca, err = LoadOrCreateGatewayCA(filepath.Join(state, HostCADir)); err != nil {
			return nil, err
		}
	}
	r := &Registry{State: state, Verifier: options.Verifier, ProviderSource: options.ProviderSource, ClaudeSource: options.ClaudeSource,
		DocumentAPI: options.DocumentAPI, Clock: options.Clock, Bindings: map[string]*Binding{}, Retired: map[string][]string{}, options: options}
	r.CA = ca
	if r.Verifier == nil {
		r.Verifier = UnsupportedVerifier{}
	}
	if r.Clock == nil {
		r.Clock = monotonicClock
	}
	overrides, err := loadEgressOverrides(state)
	if err != nil {
		return nil, err
	}
	r.egressOverrides = overrides
	manifest := filepath.Join(state, "bindings.json")
	saved := map[string]any{"active": []any{}, "retired": map[string]any{}}
	if raw, err := os.ReadFile(manifest); err == nil {
		parsed, err := StrictJSON(raw)
		if err != nil {
			return nil, err
		}
		saved, _ = parsed.(map[string]any)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if saved == nil || !sameKeys(saved, "active", "retired") {
		return nil, errors.New("invalid binding manifest")
	}
	retired, _ := saved["retired"].(map[string]any)
	for sandbox, list := range retired {
		items, _ := list.([]any)
		for _, item := range items {
			generation, _ := item.(string)
			r.Retired[sandbox] = append(r.Retired[sandbox], generation)
		}
	}
	active, _ := saved["active"].([]any)
	for _, item := range active {
		entry, _ := item.(map[string]any)
		if entry == nil || !sameKeys(entry, IdentityKeys...) {
			r.closeAll()
			return nil, errors.New("invalid durable binding")
		}
		identity := map[string]string{}
		for _, key := range IdentityKeys {
			identity[key], _ = entry[key].(string)
		}
		for _, generation := range r.Retired[identity["sandboxID"]] {
			if generation == identity["generation"] {
				r.closeAll()
				return nil, errors.New("retired durable binding")
			}
		}
		if _, err := r.load(identity); err != nil {
			r.closeAll()
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) closeAll() {
	for _, b := range r.Bindings {
		b.Engine.Close()
	}
	r.Bindings = map[string]*Binding{}
}

func (r *Registry) save() error {
	if err := r.saveManifest(); err != nil {
		// A failed durable transition cannot be repaired by a successful
		// in-memory idempotent retry. Require restart/reconciliation.
		r.StorageFailed = true
		return err
	}
	return nil
}

// SaveManifest is replaceable in tests to simulate storage failure.
var saveManifestHook func(r *Registry) error

func (r *Registry) saveManifest() error {
	if saveManifestHook != nil {
		return saveManifestHook(r)
	}
	active := []any{}
	for _, b := range r.Bindings {
		active = append(active, b.Identity)
	}
	sortBindings(active)
	retired := map[string]any{}
	for sandbox, generations := range r.Retired {
		list := make([]any, len(generations))
		for i, g := range generations {
			list[i] = g
		}
		retired[sandbox] = list
	}
	return atomicWrite(filepath.Join(r.State, "bindings.json"), filepath.Join(r.State, "bindings.tmp"), []byte(Dumps(map[string]any{"active": active, "retired": retired})+"\n"))
}

func sortBindings(active []any) {
	for i := 1; i < len(active); i++ {
		for j := i; j > 0 && Dumps(active[j]) < Dumps(active[j-1]); j-- {
			active[j], active[j-1] = active[j-1], active[j]
		}
	}
}

// atomicWrite writes privately, fsyncs, renames and fsyncs the directory.
func atomicWrite(path, temporary string, data []byte) error {
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err = os.Chmod(temporary, 0o600); err != nil {
		file.Close()
		return err
	}
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r *Registry) load(identity map[string]string) (*Binding, error) {
	sandbox := identity["sandboxID"]
	if _, exists := r.Bindings[sandbox]; exists {
		return nil, errors.New("duplicate sandbox identity")
	}
	for _, other := range r.Bindings {
		if other.Identity["runtimeName"] == identity["runtimeName"] {
			return nil, errors.New("runtime already registered")
		}
	}
	directory := filepath.Join(r.State, "sandboxes", BindingDigest(identity))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	engineOptions := EngineOptions{Operations: r.options.Operations, PolicyTemplate: r.options.PolicyTemplate, EgressMode: r.effectiveEgressLocked(sandbox)}
	engine, err := NewEngine(directory, engineOptions)
	if err != nil {
		return nil, err
	}
	switch {
	case r.options.GitHubAppConfig != "" && r.options.GitHubAuthFile != "":
		engine.Close()
		return nil, errors.New("GitHub App broker and user token file are mutually exclusive")
	case r.options.GitHubAppConfig != "":
		app, err := LoadGitHubAppConfig(r.options.GitHubAppConfig, engine.Redactor, engine.Now)
		if err != nil {
			engine.Close()
			return nil, err
		}
		engine.GitHubApp = app
	case r.options.GitHubAuthFile != "":
		engine.GitHubApp = NewGitHubUserCredentials(r.options.GitHubAuthFile, engine.Redactor, engine.Now)
	}
	// Repository-less chats must not inherit account-wide GitHub authority.
	if _, ok := engine.Policy["allowed_repositories"]; !ok {
		policy := engine.PolicyCopy()
		policy["allowed_repositories"] = []any{}
		if err = engine.SavePolicy(policy); err != nil {
			engine.Close()
			return nil, err
		}
	}
	port := 0
	if raw, err := os.ReadFile(filepath.Join(directory, "gateway-port.json")); err == nil {
		parsed, err := StrictJSON(raw)
		value, ok := asInt(parsed)
		if err != nil || !ok || value < 1024 || value > 65535 {
			engine.Close()
			return nil, errors.New("invalid durable gateway port")
		}
		if _, isNumber := parsed.(json.Number); !isNumber {
			engine.Close()
			return nil, errors.New("invalid durable gateway port")
		}
		port = int(value)
	}
	b := &Binding{Identity: identityOf(identity), Engine: engine, Capability: tokenURLSafe(32), Ended: map[string]bool{}, Decisions: map[string]map[string]any{}, GatewayPort: port}
	r.Bindings[sandbox] = b
	return b, nil
}

// Register creates or replaces a sandbox binding.
func (r *Registry) Register(value any) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sandbox := ctx["sandboxID"]
	if previous, exists := r.Bindings[sandbox]; exists {
		if sameIdentity(ctx, previous.Identity) {
			if digest := ctx["imageDigest"]; digest != "" && digest != previous.ImageDigest {
				// A binding that failed its check without the digest is
				// distrusted and verified again with it on the next check.
				previous.ImageDigest = digest
			}
			return map[string]any{"ok": true, "ready": false}, nil
		}
		for _, key := range IdentityKeys {
			if key != "generation" && ctx[key] != previous.Identity[key] {
				return nil, errors.New("immutable sandbox identity changed")
			}
		}
		if r.lease(previous) != nil {
			return nil, errors.New("cannot replace an active generation")
		}
		retired := r.Retired[sandbox]
		if len(retired) >= 10000 {
			return nil, errors.New("generation cannot be reused")
		}
		for _, generation := range retired {
			if generation == ctx["generation"] {
				return nil, errors.New("generation cannot be reused")
			}
		}
		r.revoke(previous)
		r.Retired[sandbox] = append(retired, previous.Identity["generation"])
		delete(r.Bindings, sandbox)
		replacement, err := r.load(identityOf(ctx))
		if err != nil {
			return nil, err
		}
		if err = replacement.Engine.SavePolicy(previous.Engine.PolicyCopy()); err != nil {
			return nil, err
		}
		replacement.ProviderSecret = previous.ProviderSecret
		replacement.ImageDigest = ctx["imageDigest"]
		if previous.GatewayPort != 0 {
			if err = r.setGatewayPort(replacement, previous.GatewayPort); err != nil {
				return nil, err
			}
		}
		replacement.Engine.Redactor.Register(previous.ProviderSecret)
		if err = r.save(); err != nil {
			return nil, err
		}
		previous.Engine.Close()
		return map[string]any{"ok": true, "ready": false}, nil
	}
	if len(r.Bindings) >= 64 {
		return nil, errors.New("sandbox registry full")
	}
	for _, other := range r.Bindings {
		if other.Identity["runtimeName"] == ctx["runtimeName"] {
			return nil, errors.New("runtime already registered")
		}
	}
	created, err := r.load(identityOf(ctx))
	if err != nil {
		return nil, err
	}
	created.ImageDigest = ctx["imageDigest"]
	if err = r.save(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "ready": false}, nil
}

func (r *Registry) binding(ctx map[string]string) (*Binding, error) {
	b := r.Bindings[ctx["sandboxID"]]
	if b == nil || !sameIdentity(b.Identity, ctx) {
		return nil, errors.New("binding mismatch")
	}
	if digest := ctx["imageDigest"]; digest != "" && b.ImageDigest == "" {
		// A binding reloaded from the manifest learns its image again
		// from the first request that names it.
		b.ImageDigest = digest
	}
	return b, nil
}

func (r *Registry) proof(b *Binding, phase string) *NetworkProof {
	if phase != "create" && phase != "runtime" {
		r.revoke(b)
		return nil
	}
	identity := map[string]string{}
	for k, v := range b.Identity {
		identity[k] = v
	}
	if b.ImageDigest != "" {
		identity["imageDigest"] = b.ImageDigest
	}
	proof, err := r.Verifier.Verify(identity, phase)
	if err != nil {
		proof = nil
	}
	digest := b.Engine.PolicyDigest()
	now := r.Clock()
	if r.StorageFailed || proof == nil || proof.Binding != BindingDigest(b.Identity) || proof.Phase != phase || proof.PolicyDigest != digest ||
		!(now < proof.ExpiresAt && proof.ExpiresAt <= now+ProofSeconds) || proof.EvidenceID == "" || proof.GatewayPort < 1024 || proof.GatewayPort > 65535 ||
		proof.GatewayPort != b.GatewayPort || !b.Engine.NetworkEnabled() {
		r.revoke(b)
		return nil
	}
	return proof
}

func (r *Registry) revoke(b *Binding) {
	lease := b.Lease
	if lease != nil {
		b.Ended[lease.RunID] = true
	}
	b.Lease = nil
	b.Decisions = map[string]map[string]any{}
	b.Engine.ClearNetworkDecisions()
	if lease != nil {
		_ = b.Engine.RevokeAll()
	}
}

func (r *Registry) lease(b *Binding) *Lease {
	if b.Lease != nil && r.Clock() >= b.Lease.ExpiresAt {
		r.revoke(b)
	}
	return b.Lease
}

// Check reports readiness for a phase without activating anything.
func (r *Registry) Check(value any, phase string) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	if phase != "create" && phase != "runtime" {
		return nil, errors.New("invalid readiness phase")
	}
	proof := r.proof(b, phase)
	result := map[string]any{"ok": true, "ready": proof != nil, "reason": "verified", "detail": nil}
	if proof == nil {
		result["reason"] = "enforcement_unavailable"
		if failure := r.Verifier.LastFailure(); failure != "" {
			result["detail"] = failure
		}
	}
	return result, nil
}

func (r *Registry) caCertificate(_ *Binding) (string, bool) {
	if r.CA == nil || len(r.CA.CertPEM) == 0 {
		return "", false
	}
	return string(r.CA.CertPEM), true
}

// Begin activates or renews a run's lease and returns guest configuration.
func (r *Registry) Begin(value any, renew bool) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	proof := r.proof(b, "runtime")
	if proof == nil {
		return map[string]any{"ok": true, "ready": false, "reason": "enforcement_unavailable"}, nil
	}
	provider := providerOf(ctx)
	source := r.ProviderSource
	if provider == "claude" {
		source = r.ClaudeSource
	}
	secret := ""
	if provider == "codex" {
		secret = b.ProviderSecret
	}
	if secret == "" && (source == nil || !source.Available()) {
		r.revoke(b)
		return map[string]any{"ok": true, "ready": false, "reason": "provider_credential_unavailable"}, nil
	}
	lease := r.lease(b)
	if lease != nil && (lease.RunID != ctx["runID"] || lease.ChatID != ctx["chatID"] || lease.Provider != provider) {
		return map[string]any{"ok": false, "ready": false, "reason": "sandbox_busy"}, nil
	}
	if renew && lease == nil {
		return map[string]any{"ok": false, "ready": false, "reason": "lease_inactive"}, nil
	}
	if b.Ended[ctx["runID"]] || len(b.Ended) >= 10000 {
		return map[string]any{"ok": false, "ready": false, "reason": "run_not_reusable"}, nil
	}
	// Durable audit precedes capability activation/extension.
	event := "sbx.run.started"
	if renew {
		event = "sbx.run.renewed"
	}
	if _, err = b.Engine.Audit.Emit(event, map[string]any{"sandbox_id": ctx["sandboxID"], "project_id": ctx["projectID"], "run_id": ctx["runID"], "chat_id": ctx["chatID"]}); err != nil {
		return nil, err
	}
	b.Lease = &Lease{Provider: provider, RunID: ctx["runID"], ChatID: ctx["chatID"], ExpiresAt: r.Clock() + LeaseSeconds}
	b.LastBegin, b.Begun = r.Clock(), true
	// The proof names the gateway port it was issued for; the endpoint's
	// host and credential come from the Gateway that bound it.
	endpoint := b.endpoint()
	endpoint.Port = proof.GatewayPort
	gateway := endpoint.BaseURL()
	result := map[string]any{"ok": true, "ready": true, "leaseSeconds": LeaseSeconds, "apiKeyPlaceholder": endpoint.Placeholder(), "provider": provider, "proxyURL": endpoint.ProxyURL()}
	if provider == "claude" {
		result["providerBaseURL"] = gateway + "/anthropic"
	} else {
		result["providerBaseURL"] = gateway + "/openai/v1"
	}
	if ca, ok := r.caCertificate(b); ok {
		result["caCertificate"] = ca
	}
	if r.DocumentAPI != nil {
		result["documentBaseURL"] = gateway + DocumentPrefix
	}
	return result, nil
}

func itoa(n int) string {
	return json.Number(Dumps(n)).String()
}

// End releases the run's lease and revokes its decisions.
func (r *Registry) End(value any) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	lease := r.lease(b)
	if lease != nil && (lease.RunID != ctx["runID"] || lease.ChatID != ctx["chatID"]) {
		return map[string]any{"ok": false, "reason": "run_mismatch"}, nil
	}
	r.revoke(b)
	return map[string]any{"ok": true, "ready": false}, nil
}

// Gateway returns host launcher configuration; never sent to a guest.
func (r *Registry) Gateway(value any) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	var port any
	if b.GatewayPort != 0 {
		port = b.GatewayPort
	}
	return map[string]any{"ok": true, "bindingID": ctx["sandboxID"], "capability": b.Capability, "gatewayPort": port}, nil
}

// BindGateway records the loopback port of a binding's gateway.
func (r *Registry) BindGateway(value any, port any) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	n, ok := asInt(port)
	if _, isBool := port.(bool); !ok || isBool || n < 1024 || n > 65535 {
		return nil, errors.New("invalid gateway port")
	}
	if b.GatewayPort != 0 && b.GatewayPort != int(n) {
		return nil, errors.New("gateway binding immutable until restart")
	}
	for _, other := range r.Bindings {
		if other != b && other.GatewayPort == int(n) {
			return nil, errors.New("gateway port already registered")
		}
	}
	if err = r.setGatewayPort(b, int(n)); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "ready": false}, nil
}

func (r *Registry) setGatewayPort(b *Binding, port int) error {
	path := filepath.Join(b.Engine.State, "gateway-port.json")
	if err := atomicWrite(path, filepath.Join(b.Engine.State, "gateway-port.tmp"), []byte(Dumps(port))); err != nil {
		r.StorageFailed = true
		return err
	}
	b.GatewayPort = port
	return nil
}

// SetGatewayPort is used by the gateway pool while it holds the registry lock.
func (r *Registry) SetGatewayPort(b *Binding, port int) error { return r.setGatewayPort(b, port) }

// ConfigureProvider installs a per-sandbox provider secret.
func (r *Registry) ConfigureProvider(value any, secret any) (map[string]any, error) {
	ctx, err := ValidateContext(value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.binding(ctx)
	if err != nil {
		return nil, err
	}
	s, ok := secret.(string)
	if !ok || len(s) < 16 || len(s) > 4096 || strings.ContainsAny(s, " \t\n\r\v\f") {
		return nil, errors.New("invalid provider credential")
	}
	r.revoke(b)
	b.Engine.Redactor.Register(s)
	if _, err = b.Engine.Audit.Emit("credential.configured", map[string]any{"credential": map[string]any{"provider": "openai", "storage": "host_memory"}}); err != nil {
		return nil, err
	}
	b.ProviderSecret = s
	return map[string]any{"ok": true}, nil
}

func (r *Registry) providerRoute(b *Binding, path string) (string, string, error) {
	if b.Lease != nil && b.Lease.Provider == "claude" {
		if r.ClaudeSource == nil {
			return "", "", errors.New("Claude not configured")
		}
		return r.ClaudeSource.Route(path)
	}
	if b.ProviderSecret != "" {
		if path != "/v1/responses" && path != "/v1/chat/completions" {
			return "", "", errors.New("unsupported provider route")
		}
		return "api.openai.com", path, nil
	}
	if r.ProviderSource != nil {
		return r.ProviderSource.Route(path)
	}
	return "", "", errors.New("provider unavailable")
}

func (r *Registry) authenticate(sandbox string, capability any) (*Binding, error) {
	b := r.Bindings[sandbox]
	cap, ok := capability.(string)
	if b == nil || !ok || subtle.ConstantTimeCompare([]byte(b.Capability), []byte(cap)) != 1 {
		return nil, errors.New("gateway authentication failed")
	}
	return b, nil
}

// DocumentRequest brokers one guest document API call.
func (r *Registry) DocumentRequest(sandbox string, capability any, message map[string]any) (map[string]any, error) {
	r.mu.Lock()
	b, err := r.authenticate(sandbox, capability)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if r.DocumentAPI == nil || r.proof(b, "runtime") == nil || r.lease(b) == nil {
		r.mu.Unlock()
		return map[string]any{"allow": false, "status": 503}, nil
	}
	if !sameKeys(message, "action", "method", "path", "body") {
		r.mu.Unlock()
		return map[string]any{"allow": false, "status": 403}, nil
	}
	encoded, _ := message["body"].(string)
	method, _ := message["method"].(string)
	path, _ := message["path"].(string)
	if len(encoded) > (DocumentMaxBody+2)/3*4 {
		r.mu.Unlock()
		return map[string]any{"allow": false, "status": 403}, nil
	}
	body, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		r.mu.Unlock()
		return map[string]any{"allow": false, "status": 403}, nil
	}
	lease := b.Lease
	trusted := map[string]any{}
	for k, v := range b.Identity {
		trusted[k] = v
	}
	trusted["runID"] = lease.RunID
	trusted["chatID"] = lease.ChatID
	upstreamPath, headers, err := r.DocumentAPI.Prepare(method, path, body, trusted)
	if err != nil {
		r.mu.Unlock()
		return map[string]any{"allow": false, "status": 403}, nil
	}
	if _, err = b.Engine.Audit.Emit("sbx.document.authorized", map[string]any{"sandbox_id": sandbox, "project_id": trusted["projectID"], "run_id": trusted["runID"], "chat_id": trusted["chatID"],
		"request": map[string]any{"method": method, "path": strings.SplitN(upstreamPath, "?", 2)[0]}}); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()
	// Let End/revocation proceed while the local app responds. The app also
	// checks current run assignment before a write, and delivery rechecks it.
	status, data, dispatchErr := r.DocumentAPI.Dispatch(method, upstreamPath, body, headers)
	if dispatchErr != nil {
		return map[string]any{"allow": false, "status": 502}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Bindings[sandbox] != b || r.proof(b, "runtime") == nil {
		return map[string]any{"allow": false, "status": 503}, nil
	}
	current := r.lease(b)
	if current == nil || current.RunID != lease.RunID || current.ChatID != lease.ChatID {
		return map[string]any{"allow": false, "status": 503}, nil
	}
	if _, err = b.Engine.Audit.Emit("sbx.document.completed", map[string]any{"sandbox_id": sandbox, "project_id": trusted["projectID"], "run_id": trusted["runID"], "status": status}); err != nil {
		return nil, err
	}
	return map[string]any{"allow": true, "status": status, "body": base64.StdEncoding.EncodeToString(data)}, nil
}

var gatewayActions = stringSet("authorize", "egress", "active", "event", "egress.finish", "unpublish", "provider", "providerRoute", "destination")
var providerRouteTargets = map[[2]string]bool{
	{"api.openai.com", "/v1/responses"}:                true,
	{"api.openai.com", "/v1/chat/completions"}:         true,
	{"chatgpt.com", "/backend-api/codex/responses"}:    true,
	{"api.anthropic.com", "/v1/messages"}:              true,
	{"api.anthropic.com", "/v1/messages/count_tokens"}: true,
}

// Proxy handles one gateway control message for a binding.
func (r *Registry) Proxy(sandbox string, capability any, message any) (map[string]any, error) {
	msg, isMap := message.(map[string]any)
	if isMap && msg["action"] == "document" {
		return r.DocumentRequest(sandbox, capability, msg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.authenticate(sandbox, capability)
	if err != nil {
		return nil, err
	}
	if !isMap {
		return nil, errors.New("invalid gateway message")
	}
	action, _ := msg["action"].(string)
	if !gatewayActions[action] {
		return nil, errors.New("unsupported gateway action")
	}
	if r.proof(b, "runtime") == nil || r.lease(b) == nil {
		return map[string]any{"allow": false, "active": false, "status": 503, "reason": "active verified run required"}, nil
	}
	engine := b.Engine
	sharing := r.Sharing
	lease := b.Lease
	request, _ := msg["request"].(map[string]any)
	requestHost, _ := request["host"].(string)
	decisionID, _ := msg["decision_id"].(string)
	if sharing != nil && action == "authorize" && (requestHost == "github.com" || requestHost == "api.github.com") {
		grant, authorization, ok, err := sharing.GitHubGrant(lease.ChatID, sandbox, engine, request)
		if err != nil {
			return map[string]any{"allow": false, "status": 403, "reason": "GitHub repository sharing unavailable"}, nil
		}
		if ok {
			if r.lease(b) == nil {
				return map[string]any{"allow": false, "status": 403, "reason": "run expired during credential acquisition"}, nil
			}
			decision := "grepo-" + randomHex(16)
			b.Decisions[decision] = map[string]any{"grant": grant, "chat": lease.ChatID, "run": lease.RunID, "expires": sharing.Clock() + 300}
			engine.Redactor.Register(strings.TrimPrefix(authorization, "Bearer "))
			if _, err = engine.Audit.Emit("sbx.github.authorized", map[string]any{"sandbox_id": sandbox, "chat_id": lease.ChatID, "run_id": lease.RunID, "grant_id": grant}); err != nil {
				return nil, err
			}
			return map[string]any{"allow": true, "authorization": authorization, "decision_id": decision, "request_id": decision, "remaining_seconds": 300, "expires_at": sharing.Clock() + 300}, nil
		}
	}
	if sharing != nil && action == "active" && strings.HasPrefix(decisionID, "grepo-") {
		record := b.Decisions[decisionID]
		active := false
		if record != nil {
			expires, _ := asNumber(record["expires"])
			grant, _ := record["grant"].(string)
			active = engine.NetworkEnabled() && expires > sharing.Clock() && record["run"] == lease.RunID && record["chat"] == lease.ChatID && sharing.GitHubActive(grant, lease.ChatID, sandbox)
		}
		return map[string]any{"active": active}, nil
	}
	if sharing != nil && action == "authorize" && (requestHost == "docs.googleapis.com" || requestHost == "sheets.googleapis.com") {
		denied := map[string]any{"allow": false, "status": 403, "reason": "Google document access requires an active sharing grant"}
		if !engine.NetworkEnabled() {
			return denied, nil
		}
		grant, authorization, err := sharing.Authorize(lease.ChatID, sandbox, request)
		if err != nil {
			denied["reason"] = err.Error()
			return denied, nil
		}
		grantID, _ := grant["request_id"].(string)
		if r.lease(b) == nil || !sharing.Active(grantID, lease.ChatID, sandbox) {
			return denied, nil
		}
		decision := "gdoc-" + randomHex(16)
		b.Decisions[decision] = map[string]any{"grant": grantID, "chat": lease.ChatID, "run": lease.RunID}
		engine.Redactor.Register(strings.TrimPrefix(authorization, "Bearer "))
		if _, err = engine.Audit.Emit("sbx.google.authorized", map[string]any{"sandbox_id": sandbox, "chat_id": lease.ChatID, "run_id": lease.RunID, "grant_id": grantID}); err != nil {
			return nil, err
		}
		expires, _ := asNumber(grant["expires_at"])
		remaining := expires - sharing.Clock()
		if remaining < 0 {
			remaining = 0
		}
		result := map[string]any{"allow": true, "authorization": authorization, "decision_id": decision, "request_id": decision, "expires_at": expires, "remaining_seconds": remaining}
		return result, nil
	}
	if sharing != nil && action == "unpublish" {
		// The gateway reports a Docs image edit finished; the published
		// images go away whatever the upstream answered.
		var tokens []string
		items, _ := msg["publications"].([]any)
		for _, t := range items {
			if token, ok := t.(string); ok {
				tokens = append(tokens, token)
			}
		}
		sharing.Images.Unpublish(tokens)
		return map[string]any{"released": true}, nil
	}
	if sharing != nil && action == "active" && strings.HasPrefix(decisionID, "gdoc-") {
		record := b.Decisions[decisionID]
		active := false
		if record != nil {
			grant, _ := record["grant"].(string)
			active = engine.NetworkEnabled() && record["run"] == lease.RunID && record["chat"] == lease.ChatID && sharing.Active(grant, lease.ChatID, sandbox)
		}
		return map[string]any{"active": active}, nil
	}
	switch action {
	case "providerRoute":
		path, _ := msg["path"].(string)
		host, target, err := r.providerRoute(b, path)
		if err != nil {
			return map[string]any{"allow": false, "reason": "unsupported provider route"}, nil
		}
		return map[string]any{"allow": true, "host": host, "path": target}, nil
	case "destination":
		// This happens before any gateway DNS lookup. Restrict even
		// CONNECT hostnames so rejected names cannot become DNS egress.
		method, _ := request["method"].(string)
		scheme, _ := request["scheme"].(string)
		allow := requestHost == "github.com" || requestHost == "api.github.com" || requestHost == "api.figma.com" || requestHost == "docs.googleapis.com" || requestHost == "sheets.googleapis.com" || EgressPermits(engine.egressPolicyCopy(), requestHost, method, scheme, false) || engine.HostAllowed(requestHost)
		if allow {
			if _, err := engine.Audit.Emit("dns.query", map[string]any{"hostname": requestHost, "reason": "SBX destination checked before resolution"}); err != nil {
				return nil, err
			}
		}
		return map[string]any{"allow": allow}, nil
	case "provider":
		record := b.Decisions[decisionID]
		routes := map[[2]string]bool{}
		for path := range ProviderRoutes {
			if host, target, err := r.providerRoute(b, path); err == nil {
				routes[[2]string{host, target}] = true
			}
		}
		recordHost, _ := record["host"].(string)
		recordPath, _ := record["path"].(string)
		if record == nil || !jsonEqual(record, request) || !engine.Active(decisionID) || record["scheme"] != "https" || record["method"] != "POST" || !routes[[2]string{recordHost, recordPath}] {
			return map[string]any{"allow": false, "status": 403, "reason": "provider credential denied"}, nil
		}
		var headers map[string]string
		if lease.Provider == "claude" {
			if r.ClaudeSource == nil {
				return nil, errors.New("invalid credential headers")
			}
			headers, err = r.ClaudeSource.Headers()
		} else if b.ProviderSecret != "" {
			headers = map[string]string{"Authorization": "Bearer " + b.ProviderSecret}
		} else if r.ProviderSource != nil {
			headers, err = r.ProviderSource.Headers()
		} else {
			err = errors.New("provider unavailable")
		}
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			if (key != "Authorization" && key != "ChatGPT-Account-ID") || strings.ContainsAny(value, "\r\n") {
				return nil, errors.New("invalid credential headers")
			}
		}
		if !strings.HasPrefix(headers["Authorization"], "Bearer ") {
			return nil, errors.New("invalid credential headers")
		}
		for _, value := range headers {
			engine.Redactor.Register(value)
			engine.Redactor.Register(strings.TrimPrefix(value, "Bearer "))
		}
		if _, err = engine.Audit.Emit("sbx.provider.authorized", map[string]any{"sandbox_id": sandbox, "run_id": lease.RunID, "decision_id": decisionID, "hostname": recordHost}); err != nil {
			return nil, err
		}
		headersAny := map[string]any{}
		for k, v := range headers {
			headersAny[k] = v
		}
		return map[string]any{"allow": true, "headers": headersAny, "authorization": headers["Authorization"]}, nil
	case "egress":
		if request == nil {
			return map[string]any{"allow": false, "status": 403, "reason": "opaque TLS unsupported"}, nil
		}
		if tls, _ := request["tls"].(bool); tls {
			return map[string]any{"allow": false, "status": 403, "reason": "opaque TLS unsupported"}, nil
		}
		result, err := engineInternal(engine, msg)
		if err != nil {
			return nil, err
		}
		if allow, _ := result["allow"].(bool); allow {
			id, _ := result["decision_id"].(string)
			copied := map[string]any{}
			for k, v := range request {
				copied[k] = v
			}
			b.Decisions[id] = copied
		}
		return result, nil
	case "egress.finish":
		delete(b.Decisions, decisionID)
	}
	return engineInternal(engine, msg)
}

func (e *Engine) egressPolicyCopy() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.egressPolicy()
}

var proxyEventTypes = stringSet("http.response", "http.response.started", "http.request.external", "network.denied", "request.interrupted", "proxy.error", "dns.query", "proxy.started", "tls.passthrough")
var proxyEventFields = stringSet("request_id", "decision_id", "status", "reason", "request", "response", "destination", "hostname", "latency_ms")

// engineInternal is the proxy-facing engine action set.
func engineInternal(engine *Engine, message map[string]any) (map[string]any, error) {
	action, _ := message["action"].(string)
	switch action {
	case "authorize":
		request, _ := message["request"].(map[string]any)
		if request == nil {
			return nil, errors.New("invalid request")
		}
		return engine.Authorize(request)
	case "egress":
		request, _ := message["request"].(map[string]any)
		if request == nil {
			return nil, errors.New("invalid request")
		}
		return engine.AuthorizeEgress(request)
	case "egress.finish":
		id, _ := message["decision_id"].(string)
		engine.FinishEgress(id)
		return map[string]any{"released": true}, nil
	case "active":
		id, _ := message["decision_id"].(string)
		return map[string]any{"active": engine.Active(id)}, nil
	case "event":
		eventType, _ := message["event_type"].(string)
		if !proxyEventTypes[eventType] {
			return nil, errors.New("invalid proxy event type")
		}
		fields := map[string]any{}
		if raw, present := message["fields"]; present {
			var ok bool
			if fields, ok = raw.(map[string]any); !ok {
				return nil, errors.New("invalid proxy event fields")
			}
		}
		for key := range fields {
			if !proxyEventFields[key] {
				return nil, errors.New("invalid proxy event fields")
			}
		}
		if _, err := engine.Audit.Emit(eventType, fields); err != nil {
			return nil, err
		}
		return map[string]any{"recorded": true}, nil
	}
	return nil, errors.New("unknown action")
}

// Dispatch routes one control protocol message.
func (r *Registry) Dispatch(message any) (map[string]any, error) {
	r.mu.Lock()
	failed := r.StorageFailed
	r.mu.Unlock()
	if failed {
		return nil, errors.New("durable registry unavailable")
	}
	msg, ok := message.(map[string]any)
	if !ok {
		return nil, errors.New("unsupported protocol")
	}
	version, ok := asInt(msg["version"])
	if _, isBool := msg["version"].(bool); !ok || isBool || version != 1 {
		return nil, errors.New("unsupported protocol")
	}
	operation, _ := msg["operation"].(string)
	if operation == "version" {
		// The startup handshake (chat/internal/handshake): this service's
		// build revision and the release-wide protocol number.
		if !sameKeys(msg, "version", "operation") {
			return nil, errors.New("unexpected protocol fields")
		}
		return map[string]any{"version": 1, "ok": true, "protocol": release.Protocol, "revision": release.Revision}, nil
	}
	if operation == "sharing" {
		// Sharing is driven by the chat service on the owner's behalf and
		// its refusals are written for people ("Refresh the GitHub sign-in
		// before using repositories", "repository is not shared with this
		// workspace"), so unlike sandbox-facing operations the message is
		// returned; the caller shows it to the owner and the agent.
		if !sameKeys(msg, "version", "operation", "action", "data") || r.Sharing == nil {
			return map[string]any{"version": 1, "ok": false, "error": "sharing unavailable"}, nil
		}
		action, _ := msg["action"].(string)
		data, _ := msg["data"].(map[string]any)
		result, err := r.Sharing.Dispatch(action, data)
		if err != nil {
			return map[string]any{"version": 1, "ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"version": 1, "ok": true, "result": result}, nil
	}
	fields := []string{"version", "operation", "context"}
	switch operation {
	case "check":
		fields = append(fields, "phase")
	case "configureProvider":
		fields = append(fields, "secret")
	case "bindGateway":
		fields = append(fields, "port")
	case "proxy":
		fields = []string{"version", "operation", "bindingID", "capability", "message"}
	}
	if !sameKeys(msg, fields...) {
		return nil, errors.New("unexpected protocol fields")
	}
	var result map[string]any
	var err error
	switch operation {
	case "proxy":
		bindingID, _ := msg["bindingID"].(string)
		result, err = r.Proxy(bindingID, msg["capability"], msg["message"])
	case "register":
		result, err = r.Register(msg["context"])
	case "check":
		phase, _ := msg["phase"].(string)
		result, err = r.Check(msg["context"], phase)
	case "begin", "renew":
		result, err = r.Begin(msg["context"], operation == "renew")
	case "end":
		result, err = r.End(msg["context"])
	case "gateway":
		result, err = r.Gateway(msg["context"])
	case "configureProvider":
		result, err = r.ConfigureProvider(msg["context"], msg["secret"])
	case "bindGateway":
		result, err = r.BindGateway(msg["context"], msg["port"])
	default:
		return nil, errors.New("unknown operation")
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"version": 1}
	for k, v := range result {
		out[k] = v
	}
	return out, nil
}

// Refresher is implemented by verifiers with a background proof loop.
type Refresher interface{ StopRefresher() }

// Close stops the verifier's refresher and gateways, then releases every engine.
func (r *Registry) Close() {
	// Stop the refresher before taking the lock: it may be waiting for it.
	if refresher, ok := r.Verifier.(Refresher); ok {
		refresher.StopRefresher() // also waits for background verifications
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Gateways != nil {
		r.Gateways.Close()
	}
	if r.hostAudit != nil {
		r.hostAudit.Close()
		r.hostAudit = nil
	}
	for _, b := range r.Bindings {
		b.ProviderSecret = ""
		b.Engine.Close()
	}
	r.Bindings = map[string]*Binding{}
}

// ControlServer serves the private line-delimited JSON protocol on the
// policy service's listener: a Unix socket (0600) on the sbx shapes, a
// mutual-TLS port admitting warden-runner and warden-chat on Kubernetes.
type ControlServer struct {
	registry *Registry
	listener net.Listener
	slots    chan struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
	once     sync.Once
}

// ControlPeers are the identities the control listener admits over tls://.
var ControlPeers = []string{transport.Runner, transport.Chat}

// ListenControl binds address (unix://<path>, bound privately at 0600, or
// tls://<host>:<port> with the service's material) and starts serving.
func ListenControl(address string, material *transport.TLS, registry *Registry) (*ControlServer, error) {
	listener, err := transport.Listen(address, transport.ListenOptions{Mode: 0o600, TLS: material, Peers: ControlPeers})
	if err != nil {
		return nil, err
	}
	s := &ControlServer{registry: registry, listener: listener, slots: make(chan struct{}, 64), closed: make(chan struct{})}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *ControlServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		select {
		case s.slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			defer bugreport.Recover("policy control connection")
			s.handle(conn)
		}()
	}
}

var controlRejected = map[string]any{"version": 1, "ok": false, "ready": false, "allow": false, "reason": "control_request_rejected"}

func (s *ControlServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(bufio.NewReaderSize(conn, 65536), MaxControlMessage)
	var response map[string]any
	if err == nil {
		parsed, parseErr := StrictJSON(line)
		if parseErr == nil {
			// Long-running operations (verifier attestation, upstream document
			// calls) may exceed the read deadline; the write gets its own.
			_ = conn.SetDeadline(time.Time{})
			response, err = s.registry.Dispatch(parsed)
		} else {
			err = parseErr
		}
	}
	if err != nil || response == nil {
		response = controlRejected
	}
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = conn.Write([]byte(Dumps(response) + "\n"))
}

// readLine reads one newline-terminated frame of at most limit bytes.
func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit+1 {
			return nil, errors.New("invalid frame")
		}
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
	if len(line) > limit || line[len(line)-1] != '\n' {
		return nil, errors.New("invalid frame")
	}
	return line[:len(line)-1], nil
}

// Addr is the bound address (a tls:// listener's ephemeral port, in tests).
func (s *ControlServer) Addr() net.Addr { return s.listener.Addr() }

// Close stops accepting and waits for in-flight handlers.
func (s *ControlServer) Close() {
	s.once.Do(func() {
		close(s.closed)
		s.listener.Close()
	})
	s.wg.Wait()
}

// ControlRPC performs one request against a control endpoint (a unix:// or
// tls:// URL; material is the caller's for tls://).
func ControlRPC(address string, material *transport.TLS, message map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, address, transport.DialOptions{TLS: material})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err = conn.Write([]byte(Dumps(message) + "\n")); err != nil {
		return nil, err
	}
	line, err := readLine(bufio.NewReaderSize(conn, 65536), MaxControlMessage)
	if err != nil {
		return nil, errors.New("invalid control response")
	}
	parsed, err := StrictJSON(line)
	if err != nil {
		return nil, errors.New("invalid control response")
	}
	value, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("control request rejected")
	}
	version, _ := asInt(value["version"])
	if okValue, present := value["ok"]; version != 1 || (present && okValue == false) {
		return nil, errors.New("control request rejected")
	}
	return value, nil
}
