package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// The health answer, with the size offer the chat service shows in its
// views, does not wait for the worker's mutex: a prepare or a stop holds
// it for as long as its subprocess runs.
func TestHealthAnswersWhileTheMutexIsHeld(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wsw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	w := NewWorker(dir, "/not-called", "template")
	w.MemoryMB = 2048
	socket := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Serve(ctx, l) }()
	defer func() { cancel(); l.Close(); <-done }()
	if _, err = handshake.Runner(ctx, "unix://"+socket, nil); err != nil {
		t.Fatal(err) // the registry is loaded once the handshake answers
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	res, err := (&Client{Address: "unix://" + socket}).Call(callCtx, Request{Operation: "health"})
	if err != nil || res.Limits == nil || res.Limits.Default.MemoryMB != 2048 {
		t.Fatalf("health under the held mutex: %+v %v", res.Limits, err)
	}
}
