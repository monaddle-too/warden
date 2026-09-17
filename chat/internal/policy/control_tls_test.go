package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
	"warden/chat/internal/transport"
)

// Over tls:// the control endpoint admits the runner and the chat, refuses
// the edge and anyone outside the deployment CA, and answers the version
// handshake exactly as it does on the Unix socket.
func TestControlOverMutualTLSAdmitsRunnerAndChatOnly(t *testing.T) {
	f := newSbxFixture(t)
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	material := func(ca *transport.CA, id string) *transport.TLS {
		m, err := ca.Material(filepath.Join(dir, ca.Certificate.Subject.CommonName, id), id, []string{"127.0.0.1"}, time.Now(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	server, err := ListenControl("tls://127.0.0.1:0", material(ca, transport.Policy), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	address := "tls://" + server.Addr().String()
	want := handshake.Peer{Name: "warden-policy", Revision: release.Revision, Protocol: release.Protocol}
	for _, id := range []string{transport.Runner, transport.Chat} {
		peer, err := handshake.Policy(context.Background(), address, material(ca, id))
		if err != nil || peer != want {
			t.Fatalf("%s: %+v %v", id, peer, err)
		}
	}
	result, err := ControlRPC(address, material(ca, transport.Runner), map[string]any{"version": 1, "operation": "check", "context": f.value, "phase": "runtime"})
	if err != nil || !ready(result) {
		t.Fatalf("rpc over tls: %v %v", result, err)
	}
	other, err := transport.NewCA("other", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*transport.TLS{"edge": material(ca, transport.Edge), "runner from another CA": material(other, transport.Runner), "no material": nil} {
		if _, err := handshake.Policy(context.Background(), address, m); err == nil {
			t.Fatalf("%s admitted", name)
		}
		if _, err := ControlRPC(address, m, map[string]any{"version": 1, "operation": "version"}); err == nil {
			t.Fatalf("%s admitted by ControlRPC", name)
		}
	}
	// The Unix socket shape is untouched: no material, 0600, same answers.
	short, err := os.MkdirTemp("/tmp", "wctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	path := filepath.Join(short, "c.sock")
	unix, err := ListenControl("unix://"+path, nil, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	if peer, err := handshake.Policy(context.Background(), "unix://"+path, nil); err != nil || peer != want {
		t.Fatalf("%+v %v", peer, err)
	}
}
