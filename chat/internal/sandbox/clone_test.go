package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cloneRequest is a clone of the fixture's sandbox into a new one for a
// new chat.
func cloneRequest(r Request) Request {
	return Request{Version: 2, Operation: "clone", ProjectID: r.ProjectID, ChatID: "chat-copy", SandboxID: "sandbox-copy", Source: r.SandboxID, PrincipalID: r.PrincipalID}
}

func calls(d *testRuntime) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func after(list []string, marker string) []string {
	for i, c := range list {
		if c == marker {
			return list[i+1:]
		}
	}
	return nil
}

// A copy on the SBX shape: the resident source is stopped for the
// snapshot, the copy is created from it at the source's size, stopped,
// and the source's residency comes back; the copy then prepares like any
// stopped sandbox, without a second creation.
func TestCloneStopsAndRestoresTheSourceOnSBX(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000, Restart: true}
	prepareFixture(t, w, r)
	w.mu.Lock()
	src := w.managed.Sandboxes[r.SandboxID]
	src.Active = nil // the run ended; the session's residency stays
	src.Resources = Resources{CPUMilli: 2000, MemoryMB: 3072}
	src.Checkpoints = []Checkpoint{{ID: "m1", Commit: "abc"}}
	w.mu.Unlock()
	d.mu.Lock()
	d.calls = append(d.calls, "---")
	d.mu.Unlock()
	res, err := w.dispatch(context.Background(), cloneRequest(r))
	if err != nil {
		t.Fatal(err)
	}
	if res.Sandbox == nil || res.Sandbox.ID != "sandbox-copy" || res.Sandbox.State != "stopped" || res.Sandbox.Resources.String() != "2 CPUs · 3 GiB" {
		t.Fatalf("copy status %+v", res.Sandbox)
	}
	got := after(calls(d), "---")
	w.mu.Lock()
	copyName := w.managed.Sandboxes["sandbox-copy"].RuntimeName
	source := w.managed.Sandboxes["sandbox-copy"].Source
	checkpoints := len(w.managed.Sandboxes["sandbox-copy"].Checkpoints)
	bound := w.managed.Chats["chat-copy"]
	srcState := src.State
	w.mu.Unlock()
	want := []string{"release-residency", "stop:" + src.RuntimeName, "create:" + copyName, "size:" + copyName + ":2 CPUs · 3 GiB", "source:" + copyName + ":" + src.RuntimeName, "hold-residency:" + src.RuntimeName, "prepare:" + src.RuntimeName + ":2 CPUs · 3 GiB", "stop:" + copyName}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("driver calls\n got %v\nwant %v", got, want)
	}
	if source != src.RuntimeName || checkpoints != 1 || bound == nil || bound.SandboxID != "sandbox-copy" || srcState != "running" {
		t.Fatalf("copy record: source %q checkpoints %d binding %+v source state %s", source, checkpoints, bound, srcState)
	}
	// The copy's first run: bound again (a no-op), prepared as a stopped
	// sandbox, never created again.
	next := cloneRequest(r)
	next.Operation, next.RunID, next.Source = "bind-chat", "run-copy", ""
	if _, err := w.dispatch(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.calls = append(d.calls, "---2")
	d.mu.Unlock()
	next.Operation = "prepare"
	next.Provider = "codex"
	if _, err := w.dispatch(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	for _, c := range after(calls(d), "---2") {
		if strings.HasPrefix(c, "create:") {
			t.Fatal("the copy was created again at its prepare", c)
		}
	}
	w.mu.Lock()
	state := w.managed.Sandboxes["sandbox-copy"].State
	w.mu.Unlock()
	if state != "running" {
		t.Fatal("copy not running after its prepare", state)
	}
}

// A copy on a pod driver: a stopped source is made resident for the
// copy (the fallback copies from its pod) and stopped again after.
func TestCloneStartsAndStopsAStoppedSourceOnPods(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000}
	prepareFixture(t, w, r)
	w.mu.Lock()
	src := w.managed.Sandboxes[r.SandboxID]
	src.Active = nil
	if err := w.stopLocked(context.Background(), src); err != nil {
		w.mu.Unlock()
		t.Fatal(err)
	}
	w.mu.Unlock()
	d.mu.Lock()
	d.calls = append(d.calls, "---")
	d.mu.Unlock()
	if _, err := w.dispatch(context.Background(), cloneRequest(r)); err != nil {
		t.Fatal(err)
	}
	got := after(calls(d), "---")
	w.mu.Lock()
	copyName := w.managed.Sandboxes["sandbox-copy"].RuntimeName
	srcState := src.State
	w.mu.Unlock()
	want := []string{"hold-residency:" + src.RuntimeName, "prepare:" + src.RuntimeName + ":1 CPU · 1536 MiB", "create:" + copyName, "size:" + copyName + ":1 CPU · 1536 MiB", "source:" + copyName + ":" + src.RuntimeName, "release-residency", "stop:" + src.RuntimeName, "stop:" + copyName}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("driver calls\n got %v\nwant %v", got, want)
	}
	if srcState != "stopped" {
		t.Fatal("source left", srcState)
	}
}

func TestCloneRefusals(t *testing.T) {
	w, d, _, r := managedFixture(t)
	// Not created yet.
	if _, err := w.dispatch(context.Background(), cloneRequest(r)); err == nil || !strings.Contains(err.Error(), "no sandbox to copy") {
		t.Fatal("uncreated source copied", err)
	}
	prepareFixture(t, w, r)
	// An active run.
	if _, err := w.dispatch(context.Background(), cloneRequest(r)); !errors.Is(err, ErrBusy) {
		t.Fatal("running source copied", err)
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	// Another principal, an unknown source, itself, a taken name.
	for _, bad := range []func(*Request){
		func(q *Request) { q.PrincipalID = "someone-else" },
		func(q *Request) { q.Source = "no-such-sandbox" },
		func(q *Request) { q.SandboxID = q.Source },
		func(q *Request) { q.ChatID = r.ChatID },
		func(q *Request) { q.Source = "" },
	} {
		q := cloneRequest(r)
		bad(&q)
		if _, err := w.dispatch(context.Background(), q); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	for _, c := range calls(d) {
		if strings.HasPrefix(c, "create:sandbox") || strings.Contains(c, "source:") {
			t.Fatal("a refusal touched the driver", c)
		}
	}
	// A driver failure leaves nothing registered and removes the name.
	d.createBlock = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.dispatch(ctx, cloneRequest(r)); err == nil {
		t.Fatal("failed creation accepted")
	}
	w.mu.Lock()
	registered := w.managed.Sandboxes["sandbox-copy"] != nil || w.managed.Chats["chat-copy"] != nil
	w.mu.Unlock()
	if registered {
		t.Fatal("a failed copy stayed registered")
	}
	removed := false
	for _, c := range calls(d) {
		if strings.HasPrefix(c, "remove:") {
			removed = true
		}
	}
	if !removed {
		t.Fatal("a failed copy's runtime was not removed")
	}
}
