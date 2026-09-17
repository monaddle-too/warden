package policy

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostWideGatewayCASharedByEveryGateway(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 1000}
	verifier := &fixtureVerifier{enabled: true}
	registry := newTestRegistry(t, filepath.Join(dir, "state"), clock, verifier)
	verifier.registry = registry
	t.Cleanup(func() { registry.Close() })
	if _, err := os.Stat(filepath.Join(dir, "state", HostCADir, caKeyFile)); err != nil {
		t.Fatalf("host CA not created under the state directory: %v", err)
	}
	var seen []*GatewayCA
	pool := NewLoopbackGateways(registry, nil)
	pool.Factory = func(cfg GatewayConfig) (*BindingGateway, error) {
		seen = append(seen, cfg.CA)
		return NewBindingGateway(cfg)
	}
	registry.Gateways = pool
	certificates := map[string]bool{}
	for _, sandbox := range []string{"s1", "s2"} {
		value := runContext(map[string]any{"sandboxID": sandbox, "runtimeName": "sbx-" + sandbox})
		if _, err := registry.Register(value); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.ConfigureProvider(value, "synthetic-provider-fixture-secret"); err != nil {
			t.Fatal(err)
		}
		if err := pool.Ensure(registry.Bindings[sandbox]); err != nil {
			t.Fatal(err)
		}
		result, err := registry.Begin(value, false)
		if err != nil || !ready(result) {
			t.Fatalf("begin %s: %v %v", sandbox, result, err)
		}
		certificates[result["caCertificate"].(string)] = true
	}
	if len(certificates) != 1 || !certificates[string(registry.CA.CertPEM)] {
		t.Fatalf("guests must receive the one host CA, got %d distinct certificates", len(certificates))
	}
	if len(seen) != 2 || seen[0] == seen[1] || !bytes.Equal(seen[0].CertPEM, seen[1].CertPEM) {
		t.Fatal("each gateway must derive its own leaf cache from the shared CA")
	}
	leaf, err := seen[0].Certificate("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: registry.CA.Pool()}); err != nil {
		t.Fatalf("leaf must chain to the host CA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(registry.Bindings["s1"].Engine.State, "gateway", "ca")); err == nil {
		t.Fatal("per-binding CA directory should no longer be created")
	}
}

func TestGatewayCARotationOnDemandAndByAge(t *testing.T) {
	state := t.TempDir()
	now := time.Now()
	first, rotated, err := PrepareGatewayCA(state, DefaultGatewayCAMaxAge, now)
	if err != nil || rotated {
		t.Fatalf("initial create: %v rotated=%v", err, rotated)
	}
	same, rotated, err := PrepareGatewayCA(state, DefaultGatewayCAMaxAge, now.Add(24*time.Hour))
	if err != nil || rotated || same.Fingerprint() != first.Fingerprint() {
		t.Fatal("a young CA must be kept")
	}
	aged, rotated, err := PrepareGatewayCA(state, DefaultGatewayCAMaxAge, now.Add(366*24*time.Hour))
	if err != nil || !rotated || aged.Fingerprint() == first.Fingerprint() {
		t.Fatalf("a CA older than the maximum age must rotate: %v rotated=%v", err, rotated)
	}
	kept, _, err := PrepareGatewayCA(state, 0, now.Add(10*365*24*time.Hour))
	if err != nil || kept.Fingerprint() != aged.Fingerprint() {
		t.Fatal("max age 0 must disable rotation")
	}
	demanded, err := RotateGatewayCA(state, now)
	if err != nil || demanded.Fingerprint() == aged.Fingerprint() {
		t.Fatalf("on-demand rotation: %v", err)
	}
	reloaded, err := LoadOrCreateGatewayCA(filepath.Join(state, HostCADir))
	if err != nil || reloaded.Fingerprint() != demanded.Fingerprint() {
		t.Fatal("rotated CA must be the one loaded afterwards")
	}
	retired, _ := filepath.Glob(filepath.Join(state, HostCADir+".retired-*"))
	if len(retired) != 2 {
		t.Fatalf("retired CA directories must be kept, got %v", retired)
	}
	for _, dir := range retired {
		if _, err := os.Stat(filepath.Join(dir, caKeyFile)); err != nil {
			t.Fatalf("retired CA lost its key file: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, HostCADir+".next")); err == nil {
		t.Fatal("staging directory left behind")
	}
}
