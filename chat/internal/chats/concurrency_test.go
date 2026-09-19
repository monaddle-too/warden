package chats

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

type multiChatWorker struct {
	mu      sync.Mutex
	workers map[string]*fakeWorker
}

func (m *multiChatWorker) worker(id string) *fakeWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[id] == nil {
		m.workers[id] = &fakeWorker{}
	}
	return m.workers[id]
}
func (m *multiChatWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	return m.worker(r.ChatID).Call(ctx, r)
}
func (m *multiChatWorker) Open(ctx context.Context, r sandbox.Request) (io.ReadWriteCloser, sandbox.Response, error) {
	return m.worker(r.ChatID).Open(ctx, r)
}
func TestTwoChatsRunConcurrentlyAndSharedSandboxWaits(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &multiChatWorker{workers: map[string]*fakeWorker{}}
	e := NewEngine(s, w)
	a, _ := e.Create("A", "", "", nil)
	b, _ := e.Create("B", "", "", nil)
	shared := s.Snapshot().chat(a).SandboxID
	c, _ := e.Create("C", shared, "", nil)
	for _, id := range []string{a, b, c} {
		if err = e.Message(id, "hello", cv.ID()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go e.Serve(ctx)
	defer func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Error("shutdown hung")
		}
		s.Close()
	}()
	sent := func(id string) bool { return s.Snapshot().chat(id).Conversation.Entries[0].Delivery == "sent" }
	until(t, func() bool { return sent(a) && sent(b) })
	if s.Snapshot().chat(c).Status != "queued" {
		t.Fatal("shared sandbox ran concurrently")
	}
	// Stopping A must not stop B, and frees A's sandbox for C.
	if err = e.Stop(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return sent(c) })
	if s.Snapshot().chat(b).Status != "running" {
		t.Fatal("other sandbox stopped")
	}
	w.worker(b).send(agent.Frame{Method: "item/agentMessage/delta", Params: map[string]any{"itemId": "b-answer", "turnId": "turn-one", "delta": "B alive"}})
	until(t, func() bool { return len(s.Snapshot().chat(b).Conversation.Entries) == 2 })
	if len(s.Snapshot().chat(c).Conversation.Entries) != 1 {
		t.Fatal("cross-chat output")
	}
}
