package chats

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// sizeWorker answers health with limits, status with the sandbox's size
// (zero until a resize, as for a sandbox the runner has not created) and
// records resizes.
type sizeWorker struct {
	fakeWorker
	mu      sync.Mutex
	limits  *sandbox.ResourceLimits
	current sandbox.Resources
	ops     []string
	resized []sandbox.Resources
	// inPlaceRefused answers the first resize as a live runner whose
	// cluster could not apply it under the run (GKE Sandbox's gVisor); a
	// resize after a stop succeeds, as the runner's fallback does.
	inPlaceRefused bool
}

func (w *sizeWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ops = append(w.ops, r.Operation)
	switch r.Operation {
	case "health":
		return sandbox.Response{Limits: w.limits}, nil
	case "resize":
		if w.inPlaceRefused && !slices.Contains(w.ops, "stop") {
			return sandbox.Response{}, fmt.Errorf("%w: not implemented", sandbox.ErrResizeRestart)
		}
		w.resized = append(w.resized, *r.Resources)
		w.current = *r.Resources
	}
	return sandbox.Response{Version: 2, Directory: "/home/agent/workspace", Sandbox: &sandbox.SandboxInfo{ID: r.SandboxID, ProjectID: r.ProjectID, State: "running", Resources: w.current}}, nil
}
func (w *sizeWorker) snapshot() ([]string, []sandbox.Resources) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ops...), append([]sandbox.Resources(nil), w.resized...)
}

var sbxLimits = sandbox.ResourceLimits{Default: sandbox.Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: sandbox.Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000, Restart: true}

func sizeEngine(t *testing.T, limits sandbox.ResourceLimits) (*Engine, *sizeWorker) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	l := limits
	w := &sizeWorker{limits: &l}
	return NewEngine(s, w), w
}

func TestCreateTakesAWorkspaceSizeWithinTheLimits(t *testing.T) {
	e, w := sizeEngine(t, sbxLimits)
	id, err := e.Create("Big", "", "", &sandbox.Resources{MemoryMB: 4096})
	if err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Resources == nil || *c.Resources != (sandbox.Resources{CPUMilli: 1000, MemoryMB: 4096}) {
		t.Fatalf("size on the chat: %+v", c.Resources)
	}
	if view := e.View(); view.Sandboxes == nil || view.Sandboxes.Max.MemoryMB != 8192 {
		t.Fatalf("view carries no limits: %+v", view.Sandboxes)
	}
	if _, err = e.Create("Shared", c.SandboxID, "", &sandbox.Resources{MemoryMB: 2048}); err == nil || !strings.Contains(err.Error(), "already has its size") {
		t.Fatalf("size on a shared workspace: %v", err)
	}
	shared, err := e.Create("Shared", c.SandboxID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sc := e.Store.Snapshot().chat(shared); sc.Resources == nil || sc.Resources.MemoryMB != 4096 {
		t.Fatal("a chat joining the workspace does not carry its size")
	}
	for _, bad := range []sandbox.Resources{{MemoryMB: 16384}, {CPUMilli: 500}, {MemoryMB: 1000}} {
		if _, err = e.Create("Bad", "", "", &bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	plain, err := e.Create("Plain", "", "", &sandbox.Resources{})
	if err != nil || e.Store.Snapshot().chat(plain).Resources != nil {
		t.Fatal("an empty size is not the default", err)
	}
	w.mu.Lock()
	w.limits = nil
	w.mu.Unlock()
	e.limits = nil
	if _, err = e.Create("NoRunner", "", "", &sandbox.Resources{MemoryMB: 2048}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("size accepted without limits: %v", err)
	}
}

func TestResizeEnvironmentByTheOwner(t *testing.T) {
	e, w := sizeEngine(t, sbxLimits)
	id, err := e.Create("One", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	ctx := context.Background()
	// Never run: only the chats change, the runner is not asked.
	if err = e.ResizeEnvironment(ctx, c.SandboxID, &sandbox.Resources{CPUMilli: 2000}); err != nil {
		t.Fatal(err)
	}
	ops, resized := w.snapshot()
	if len(resized) != 0 || strings.Contains(strings.Join(ops, " "), "resize") {
		t.Fatal("runner resized a sandbox that was never created")
	}
	if got := e.Store.Snapshot().chat(id).Resources; got == nil || *got != (sandbox.Resources{CPUMilli: 2000, MemoryMB: 1536}) {
		t.Fatalf("size on the chat: %+v", got)
	}
	// Run once; a restarting resize stops the running chat itself (the
	// owner asked for the change), then resizes.
	e.Store.update(func(st *State) error { ch := st.chat(id); ch.Status = "running"; ch.RunID = "run"; return nil })
	if err = e.ResizeEnvironment(ctx, c.SandboxID, &sandbox.Resources{MemoryMB: 4096}); err != nil {
		t.Fatalf("resize under a running chat: %v", err)
	}
	if status := e.Store.Snapshot().chat(id).Status; status == "running" || status == "stopping" {
		t.Fatalf("chat still %s after the resize", status)
	}
	ops, resized = w.snapshot()
	joined := strings.Join(ops, " ")
	if len(resized) != 1 || resized[0] != (sandbox.Resources{CPUMilli: 2000, MemoryMB: 4096}) || strings.Index(joined, "stop") > strings.LastIndex(joined, "resize") {
		t.Fatalf("runner ops %v resized %+v", ops, resized)
	}
	if err = e.ResizeEnvironment(ctx, c.SandboxID, &sandbox.Resources{MemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	_, resized = w.snapshot()
	if len(resized) != 2 || resized[1] != (sandbox.Resources{CPUMilli: 2000, MemoryMB: 1024}) {
		t.Fatalf("runner resize: %+v", resized)
	}
	if err = e.ResizeEnvironment(ctx, c.SandboxID, &sandbox.Resources{MemoryMB: 65536}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over the ceiling: %v", err)
	}
	if err = e.ResizeEnvironment(ctx, "nope", &sandbox.Resources{MemoryMB: 1024}); err == nil {
		t.Fatal("unknown workspace resized")
	}
}

// The agent's request: validated against the runner's current size and
// limits, growth only, answered at once when already satisfied; approval
// resizes live where the platform allows, or restarts the chat on SBX.
func TestResourceGrantLiveAndRestarting(t *testing.T) {
	live := sbxLimits
	live.Restart, live.CPUStepMilli = false, 250
	e, w := sizeEngine(t, live)
	id, _ := e.Create("Agent", "", "", nil)
	e.Store.update(func(st *State) error { ch := st.chat(id); ch.Status = "running"; ch.RunID = "run"; return nil })
	c := e.Store.Snapshot().chat(id)
	request := func(args map[string]any) (int, error) {
		t.Helper()
		err := e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_resources", "arguments": args}})
		return len(e.Store.Snapshot().chat(c.ID).Approvals), err
	}
	for name, args := range map[string]map[string]any{"nothing": {"reason": "r"}, "no reason": {"cpus": 2}, "shrink": {"memory_mb": 512, "reason": "r"}, "over": {"memory_mb": 65536, "reason": "r"}, "off step": {"cpus": 0.3, "reason": "r"}} {
		if n, err := request(args); err == nil || n != 0 {
			t.Fatalf("%s: err %v, approvals %d", name, err, n)
		}
	}
	if n, err := request(map[string]any{"cpus": 1, "memory_mb": 1536, "reason": "r"}); err != nil || n != 0 {
		t.Fatalf("already satisfied: err %v, approvals %d", err, n)
	}
	if n, err := request(map[string]any{"cpus": 1.5, "reason": "the build is killed"}); err != nil || n != 1 {
		t.Fatalf("request: err %v, approvals %d", err, n)
	}
	a := e.Store.Snapshot().chat(c.ID).Approvals[0]
	if a.Method != methodResources || a.Params["cpu_milli"] != float64(1500) || a.Params["memory_mb"] != float64(1536) || a.Params["current_cpu_milli"] != float64(1000) || a.Params["restart"] != false {
		t.Fatalf("approval params: %v", a.Params)
	}
	if v := e.resolveGrant(c, a, false, cv.Actor{PrincipalID: "owner"}).(map[string]any); v["success"] != false {
		t.Fatal("decline succeeded")
	}
	v := e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "owner"}).(map[string]any)
	text := agent.String(agent.Map(agent.Array(v["contentItems"])[0])["text"])
	if v["success"] != true || !strings.Contains(text, "in effect now") {
		t.Fatalf("live allow: %v", v)
	}
	_, resized := w.snapshot()
	if len(resized) != 1 || resized[0] != (sandbox.Resources{CPUMilli: 1500, MemoryMB: 1536}) {
		t.Fatalf("runner resize: %+v", resized)
	}
	if got := e.Store.Snapshot().chat(id).Resources; got == nil || got.CPUMilli != 1500 {
		t.Fatalf("chat size after the grant: %+v", got)
	}

	// SBX: the answer says the sandbox restarts; then the chat is stopped,
	// resized and resumed by Warden's note.
	e2, w2 := sizeEngine(t, sbxLimits)
	id2, _ := e2.Create("Agent", "", "", nil)
	e2.Store.update(func(st *State) error { ch := st.chat(id2); ch.Status = "running"; ch.RunID = "run"; return nil })
	c2 := e2.Store.Snapshot().chat(id2)
	if err := e2.requestGrant(c2, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_resources", "arguments": map[string]any{"memory_mb": 4096, "reason": "tests need it"}}}); err != nil {
		t.Fatal(err)
	}
	a2 := e2.Store.Snapshot().chat(c2.ID).Approvals[0]
	if a2.Params["restart"] != true {
		t.Fatalf("approval params: %v", a2.Params)
	}
	v = e2.resolveGrant(c2, a2, true, cv.Actor{PrincipalID: "owner"}).(map[string]any)
	if text = agent.String(agent.Map(agent.Array(v["contentItems"])[0])["text"]); v["success"] != true || !strings.Contains(text, "restarts now with 1 CPU · 4 GiB") {
		t.Fatalf("restart allow: %v", v)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ch := e2.Store.Snapshot().chat(id2)
		entries := ch.Conversation.Entries
		if n := len(entries); n > 0 && entries[n-1].Sender != nil && entries[n-1].Sender.PrincipalID == "warden" {
			if ch.Status != "queued" || entries[n-1].Delivery != "queued" || !strings.Contains(entries[n-1].Text, "restarted with 1 CPU · 4 GiB") {
				t.Fatalf("resume after restart: status %s entry %+v", ch.Status, entries[n-1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat not resumed: %+v error %q", ch.Status, ch.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ops, resized := w2.snapshot()
	joined := strings.Join(ops, " ")
	if len(resized) != 1 || resized[0].MemoryMB != 4096 || strings.Index(joined, "stop") > strings.Index(joined, "resize") {
		t.Fatalf("runner ops %v resized %v", ops, resized)
	}
	if got := e2.Store.Snapshot().chat(id2).Resources; got == nil || got.MemoryMB != 4096 {
		t.Fatalf("chat size after the restart: %+v", got)
	}

	// A live platform whose cluster refuses the in-place resize under the
	// run (the runner answers ErrResizeRestart): the grant takes the
	// restarting path, stop then resize then Warden's note.
	e3, w3 := sizeEngine(t, live)
	w3.inPlaceRefused = true
	id3, _ := e3.Create("Agent", "", "", nil)
	e3.Store.update(func(st *State) error { ch := st.chat(id3); ch.Status = "running"; ch.RunID = "run"; return nil })
	c3 := e3.Store.Snapshot().chat(id3)
	if err := e3.requestGrant(c3, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_resources", "arguments": map[string]any{"memory_mb": 4096, "reason": "tests need it"}}}); err != nil {
		t.Fatal(err)
	}
	a3 := e3.Store.Snapshot().chat(c3.ID).Approvals[0]
	v = e3.resolveGrant(c3, a3, true, cv.Actor{PrincipalID: "owner"}).(map[string]any)
	if text = agent.String(agent.Map(agent.Array(v["contentItems"])[0])["text"]); v["success"] != true || !strings.Contains(text, "restarts now with 1 CPU · 4 GiB") {
		t.Fatalf("refused-in-place allow: %v", v)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		ch := e3.Store.Snapshot().chat(id3)
		entries := ch.Conversation.Entries
		if n := len(entries); n > 0 && entries[n-1].Sender != nil && entries[n-1].Sender.PrincipalID == "warden" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat not resumed after the refused in-place resize: %+v error %q", ch.Status, ch.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ops, resized = w3.snapshot()
	joined = strings.Join(ops, " ")
	if len(resized) != 1 || resized[0].MemoryMB != 4096 || strings.Index(joined, "stop") > strings.LastIndex(joined, "resize") {
		t.Fatalf("runner ops %v resized %v", ops, resized)
	}
}
