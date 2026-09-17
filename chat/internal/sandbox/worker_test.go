package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
)

// The startup handshake asks a running worker for its identity with the
// "health" operation; the answer must carry the release protocol number and
// the revision the worker was built with.
func TestWorkerAnswersTheVersionHandshake(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wsw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	w := NewWorker(dir, "/not-called", "template")
	w.Revision = "built-here"
	socket := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Serve(ctx, l) }()
	defer func() { cancel(); l.Close(); <-done }()
	peer, err := handshake.Runner(ctx, "unix://"+socket, nil)
	if err != nil || peer.Protocol != release.Protocol || peer.Revision != "built-here" || peer.Name != "warden-runner" {
		t.Fatalf("%+v %v", peer, err)
	}
	if _, err = handshake.Compare(handshake.Peer{Name: "warden-chat", Revision: "built-here", Protocol: release.Protocol}, peer); err != nil {
		t.Fatal(err)
	}
}
