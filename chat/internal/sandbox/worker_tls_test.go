package sandbox

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

// Over tls:// the runner admits only the chat: its handshake and worker
// protocol answer a warden-chat certificate from the deployment CA and
// refuse the policy service, the edge and another CA.
func TestRunnerOverMutualTLSAdmitsChatOnly(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wrt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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
	w := NewWorker(filepath.Join(dir, "root"), "/not-called", "template")
	w.Revision = "built-here"
	l, err := transport.Listen("tls://127.0.0.1:0", transport.ListenOptions{TLS: material(ca, transport.Runner), Peers: []string{transport.Chat}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Serve(ctx, l) }()
	defer func() { cancel(); l.Close(); <-done }()
	address := "tls://" + l.Addr().String()
	chat := material(ca, transport.Chat)
	peer, err := handshake.Runner(ctx, address, chat)
	if err != nil || peer != (handshake.Peer{Name: "warden-runner", Revision: "built-here", Protocol: release.Protocol}) {
		t.Fatalf("%+v %v", peer, err)
	}
	client := &Client{Address: address, TLS: chat}
	if res, err := client.Call(ctx, Request{Operation: "health"}); err != nil || res.Revision != "built-here" {
		t.Fatalf("%+v %v", res, err)
	}
	other, err := transport.NewCA("other", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*transport.TLS{"policy": material(ca, transport.Policy), "edge": material(ca, transport.Edge), "chat from another CA": material(other, transport.Chat), "no material": nil} {
		if _, err := handshake.Runner(ctx, address, m); err == nil {
			t.Fatalf("%s admitted by the handshake", name)
		}
		if _, err := (&Client{Address: address, TLS: m}).Call(ctx, Request{Operation: "health"}); err == nil {
			t.Fatalf("%s admitted by the worker protocol", name)
		}
	}
}
