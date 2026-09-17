package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warden/chat/internal/transport"
)

func TestTLSBootstrapWritesCAAndServiceMaterial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	code, out := runCLI("", false, "tls", "bootstrap", "--out", dir, "--sans", "127.0.0.1,warden-policy.warden.svc")
	if code != 0 {
		t.Fatalf("bootstrap (%d):\n%s", code, out)
	}
	for _, id := range []string{transport.Policy, transport.Runner, transport.Chat, transport.Edge} {
		if !strings.Contains(out, id+": "+filepath.Join(dir, id)) {
			t.Fatalf("missing %s in output:\n%s", id, out)
		}
		for _, f := range []string{"ca.crt", "tls.crt", "tls.key"} {
			if _, err := os.Stat(filepath.Join(dir, id, f)); err != nil {
				t.Fatal(err)
			}
		}
	}
	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("CA key", info, err)
	}
	// The material works with the transport: the policy identity admits
	// the runner, and a re-run refuses to overwrite the CA.
	policy := &transport.TLS{CAFile: filepath.Join(dir, transport.Policy, "ca.crt"), CertFile: filepath.Join(dir, transport.Policy, "tls.crt"), KeyFile: filepath.Join(dir, transport.Policy, "tls.key")}
	if _, err := transport.ServerConfig(policy, []string{transport.Runner}); err != nil {
		t.Fatal(err)
	}
	if code, out = runCLI("", false, "tls", "bootstrap", "--out", dir); code == 0 || !strings.Contains(out, "already holds a CA") {
		t.Fatalf("re-run (%d):\n%s", code, out)
	}
	if code, _ = runCLI("", false, "tls", "bootstrap"); code != 2 {
		t.Fatal("missing --out accepted", code)
	}
	if code, _ = runCLI("", false, "tls", "rotate"); code != 2 {
		t.Fatal("unknown tls subcommand accepted", code)
	}
	if code, out = runCLI("", false, "tls", "bootstrap", "--out", filepath.Join(t.TempDir(), "one"), "--names", "warden-policy"); code != 0 || strings.Contains(out, transport.Edge) {
		t.Fatalf("--names (%d):\n%s", code, out)
	}
}
