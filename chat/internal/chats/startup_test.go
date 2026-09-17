package chats

import (
	"context"
	"testing"
	"time"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

func startupOf(e *Engine, id string) Startup {
	for _, c := range e.View().Chats {
		if c.ID == id && c.Startup != nil {
			return *c.Startup
		}
	}
	return Startup{}
}

// A chat's start reports its stages: the engine's own around prepare, the
// runner's while prepare runs, and nothing once the turn is confirmed.
func TestStartupStagesFollowTheRunner(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Test", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	w.mu.Lock()
	w.prepareGate = gate
	w.progress = &sandbox.Progress{Stage: sandbox.StageCreating, Detail: "waiting for a node: 0/1 nodes are available", Since: time.Now()}
	w.mu.Unlock()
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return startupOf(e, id).Stage == stagePreparing })
	// The follower polls once a second and copies the runner's report.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && startupOf(e, id).Stage != sandbox.StageCreating {
		time.Sleep(20 * time.Millisecond)
	}
	if s := startupOf(e, id); s.Stage != sandbox.StageCreating || s.Detail != "waiting for a node: 0/1 nodes are available" || s.Since == 0 {
		t.Fatalf("runner stage not copied: %+v", s)
	}
	if e.View().chat(id).Status != "running" {
		t.Fatal("chat should be running while it starts")
	}
	close(gate)
	// After prepare the engine launches and connects, then clears the report
	// once the turn is confirmed.
	until(t, func() bool {
		c := e.View().chat(id)
		return c.Startup == nil && len(c.Conversation.Entries) > 0 && c.Conversation.Entries[0].Delivery == "sent"
	})
	w.mu.Lock()
	var ops []string
	for _, r := range w.requests {
		ops = append(ops, r.Operation)
	}
	w.mu.Unlock()
	seen := map[string]bool{}
	for _, op := range ops {
		seen[op] = true
	}
	if !seen["progress"] || !seen["prepare"] || !seen["stream"] {
		t.Fatalf("operations: %v", ops)
	}
	// A busy runner is a waiting stage, with the runner's reason.
	if d := busyDetail(sandbox.ErrBusy); d != "waiting for the runner: sandbox worker is busy" {
		t.Fatalf("busyDetail = %q", d)
	}
}

// A second chat on a busy workspace is queued with the reason, and moves
// on once the workspace is free.
func TestQueuedStartupNamesTheBlocker(t *testing.T) {
	e, w, _ := setup(t)
	first, err := e.Create("First", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	w.mu.Lock()
	w.prepareGate = gate
	w.mu.Unlock()
	if err = e.Message(first, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return startupOf(e, first).Stage == stagePreparing })
	sandboxID := e.Store.Snapshot().chat(first).SandboxID
	second, err := e.Create("Second", sandboxID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Message(second, "Me too", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		s := startupOf(e, second)
		return s.Stage == stageQueued && s.Detail == "another chat on this workspace is running"
	})
	// Stopping the first chat lets the second start: its report moves on
	// from queued.
	if err = e.Stop(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	close(gate)
	until(t, func() bool {
		s := startupOf(e, second)
		return s.Stage != "" && s.Stage != stageQueued
	})
}
