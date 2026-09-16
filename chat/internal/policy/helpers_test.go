package policy

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var (
	catalogOnce sync.Once
	catalog     *Operations
	catalogErr  error
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testOperations(t *testing.T) *Operations {
	t.Helper()
	catalogOnce.Do(func() {
		root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
		catalog, catalogErr = LoadOperations(filepath.Join(root, "vendor", "github-operations.json"))
	})
	if catalogErr != nil {
		t.Fatal(catalogErr)
	}
	return catalog
}

func templatePath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "config", "policy.template.json")
}

type testClock struct {
	mu   sync.Mutex
	now  float64
	mono float64
}

func (c *testClock) wall() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) monotonic() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *testClock) advance(seconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += seconds
	c.mono += seconds
}

func newTestEngine(t *testing.T, dir string, clock *testClock) *Engine {
	t.Helper()
	options := EngineOptions{Operations: testOperations(t), PolicyTemplate: templatePath(t)}
	if clock != nil {
		options.Clock = clock.wall
		options.Monotonic = clock.monotonic
	}
	engine, err := NewEngine(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	return engine
}

func runContext(changes map[string]any) map[string]any {
	ctx := map[string]any{"projectID": "p1", "sandboxID": "s1", "runtimeName": "sbx-one", "generation": "1", "chatID": "c1", "runID": "r1", "principalID": "owner"}
	for k, v := range changes {
		ctx[k] = v
	}
	return ctx
}

// fixtureVerifier issues synthetic proofs in tests only; production has no
// enable API.
type fixtureVerifier struct {
	registry  *Registry
	enabled   bool
	overrides map[string]any
	failure   string
	hook      func(identity map[string]string, phase string)
}

func (v *fixtureVerifier) LastFailure() string { return v.failure }

func (v *fixtureVerifier) Verify(identity map[string]string, phase string) (*NetworkProof, error) {
	if v.hook != nil {
		v.hook(identity, phase)
	}
	if !v.enabled {
		return nil, nil
	}
	actual := v.registry.Bindings[identity["sandboxID"]]
	proof := &NetworkProof{Binding: BindingDigest(identity), Phase: phase, PolicyDigest: actual.Engine.PolicyDigest(), ExpiresAt: v.registry.Clock() + 15, EvidenceID: "synthetic-fixture", GatewayPort: actual.GatewayPort}
	for key, value := range v.overrides {
		switch key {
		case "binding":
			proof.Binding = value.(string)
		case "phase":
			proof.Phase = value.(string)
		case "policy_digest":
			proof.PolicyDigest = value.(string)
		case "expires_at":
			proof.ExpiresAt = value.(float64)
		case "gateway_port":
			proof.GatewayPort = value.(int)
		case "evidence_id":
			proof.EvidenceID = value.(string)
		}
	}
	return proof, nil
}

func newTestRegistry(t *testing.T, dir string, clock *testClock, verifier Verifier) *Registry {
	t.Helper()
	options := RegistryOptions{Operations: testOperations(t), PolicyTemplate: templatePath(t), Verifier: verifier}
	if clock != nil {
		options.Clock = clock.monotonic
	}
	registry, err := NewRegistry(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func filesContain(t *testing.T, root string, needle []byte) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil && len(needle) > 0 && containsBytes(data, needle) {
			hits = append(hits, path)
		}
		return nil
	})
	return hits
}

func containsBytes(data, needle []byte) bool {
	return len(needle) > 0 && (len(data) >= len(needle)) && indexBytes(data, needle) >= 0
}

func indexBytes(data, needle []byte) int {
outer:
	for i := 0; i+len(needle) <= len(data); i++ {
		for j := range needle {
			if data[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
