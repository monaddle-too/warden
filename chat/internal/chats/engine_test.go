package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

type fakeWorker struct {
	mu          sync.Mutex
	requests    []sandbox.Request
	methods     []string
	conn        net.Conn
	writeMu     sync.Mutex
	fail        bool
	steal       bool
	rejectSteer bool
	turns       int
	inputs      [][]any  // the input items of every turn/start and turn/steer
	turnID      string   // the ID of the next turn/start's turn; "turn-one" when empty
	paths       []string // what a "paths" completion answers
	// exec is what an "exec" answers (nil: a plain success with no output).
	exec *sandbox.ExecResult
	// memory is what a "memory-list" answers (nil: an empty listing).
	memory *sandbox.MemoryListing
	// developer is the developerInstructions of the last thread/start;
	// tools the names of its dynamicTools.
	developer string
	tools     []string
	// host is what a "host.status" answers.
	host *sandbox.HostStatus
	// prepareGate, when set, holds prepare until it is closed; progress is
	// what the progress operation answers meanwhile (startup_test.go).
	prepareGate chan struct{}
	progress    *sandbox.Progress
	// threadGate, when set, holds the thread/start reply until it is
	// closed: the agent is live but has no turn yet.
	threadGate chan struct{}
	// turnGate, when set, holds the turn/start reply until it is closed:
	// the message is handed over but the turn is not confirmed.
	turnGate chan struct{}
	// ignoreInterrupt answers turn/interrupt without ending the turn, as an
	// agent that hangs would.
	ignoreInterrupt bool
	// checkpoints are the records the checkpoint op made (rewind_test.go);
	// rewindAnswer is what conversation/rewind answers (nil: rewound).
	checkpoints  []sandbox.Checkpoint
	rewindAnswer map[string]any
}

func (f *fakeWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Operation == "health" {
		// The size offer (Engine.refreshLimits) is no chat's request and
		// comes whenever the refresher runs; tests index the requests.
		return sandbox.Response{}, nil
	}
	f.requests = append(f.requests, r)
	if f.fail {
		return sandbox.Response{}, errors.New("unverified sandbox")
	}
	if r.Operation == "progress" {
		return sandbox.Response{Version: 2, Progress: f.progress}, nil
	}
	if r.Operation == "prepare" && f.prepareGate != nil {
		gate := f.prepareGate
		f.mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
		}
		f.mu.Lock()
	}
	if r.Operation == "stop" && f.conn != nil {
		f.conn.Close()
	}
	id := r.SandboxID
	if f.steal {
		id = "other"
	}
	if r.Operation == "paths" {
		return sandbox.Response{Version: 2, Paths: f.paths}, nil
	}
	switch r.Operation {
	case "host.exec":
		// A host command (host.go): what exec answers, with its size.
		result := f.exec
		if result == nil {
			result = &sandbox.ExecResult{Output: "hello-from-host\n", ExitCode: 0, Bytes: 16, DurationMS: 5}
		}
		return sandbox.Response{Version: 2, Exec: result}, nil
	case "host.put", "host.get":
		return sandbox.Response{Version: 2, Directory: r.Directory, Output: r.Path, Size: 12}, nil
	case "host.status":
		return sandbox.Response{Version: 2, Host: f.host}, nil
	case "host.expose":
		a := sandbox.PreviewAttachment{ID: "host-attachment", ChatID: r.ChatID, SandboxID: r.SandboxID, Port: r.Port, Path: r.Path, Title: r.Title, URL: "http://127.0.0.1:34567" + r.Path, State: "available", Upstream: sandbox.UpstreamHost}
		return sandbox.Response{Version: 2, Attachment: &a}, nil
	case "checkpoint":
		cp := sandbox.Checkpoint{ID: r.CallID, ChatID: r.ChatID, Commit: "commit-" + r.CallID[:4], Tree: "tree-" + r.CallID[:4], Store: "repository", Changed: true}
		f.checkpoints = append(f.checkpoints, cp)
		return sandbox.Response{Version: 2, Checkpoint: &cp}, nil
	case "checkpoints":
		return sandbox.Response{Version: 2, Checkpoints: append([]sandbox.Checkpoint(nil), f.checkpoints...)}, nil
	case "restore", "diff":
		for _, cp := range f.checkpoints {
			if cp.ID == r.CallID {
				if r.Operation == "restore" {
					restore := &sandbox.WorkspaceRestore{Checkpoint: cp, Restored: []string{"a.txt"}, Removed: []string{"b.txt"}}
					if r.Before != "" {
						// The workspace as it was, recorded under Before
						// (sandbox/checkpoint.go) for an undo.
						before := sandbox.Checkpoint{ID: r.Before, ChatID: r.ChatID, Commit: "commit-" + r.Before[:4], Tree: "tree-" + r.Before[:4], Store: "repository", Changed: true}
						f.checkpoints = append(f.checkpoints, before)
						restore.Before = &before
					}
					return sandbox.Response{Version: 2, Restore: restore}, nil
				}
				return sandbox.Response{Version: 2, Changes: &sandbox.WorkspaceChanges{Base: cp.ID, Files: []sandbox.ReviewFile{{Path: "a.txt", Added: 1}}, Diff: "diff --git a/a.txt b/a.txt\n--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1 @@\n+hello\n"}}, nil
			}
		}
		return sandbox.Response{}, errors.New("no checkpoint was recorded at this message")
	}
	if r.Operation == "exec" {
		result := f.exec
		if result == nil {
			result = &sandbox.ExecResult{}
		}
		return sandbox.Response{Version: 2, Exec: result}, nil
	}
	if r.Operation == "memory-append" {
		return sandbox.Response{Version: 2, Directory: "CLAUDE.md"}, nil
	}
	if r.Operation == "memory-list" {
		listing := f.memory
		if listing == nil {
			listing = &sandbox.MemoryListing{Root: "/home/agent/workspace", Files: []sandbox.MemoryFile{}}
		}
		return sandbox.Response{Version: 2, Memory: listing}, nil
	}
	if r.Operation == "memory-write" {
		return sandbox.Response{Version: 2, Directory: r.Directory}, nil
	}
	return sandbox.Response{Version: 2, Directory: "/home/agent/workspace", Sandbox: &sandbox.SandboxInfo{ID: id, ProjectID: r.ProjectID}}, nil
}
func (f *fakeWorker) send(v any) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		_ = json.NewEncoder(c).Encode(v)
	}
}
func (f *fakeWorker) Open(ctx context.Context, r sandbox.Request) (io.ReadWriteCloser, sandbox.Response, error) {
	client, server := net.Pipe()
	f.mu.Lock()
	f.conn = server
	f.requests = append(f.requests, r)
	f.mu.Unlock()
	go func() {
		defer server.Close()
		scan := bufio.NewScanner(server)
		for scan.Scan() {
			var frame agent.Frame
			if json.Unmarshal(scan.Bytes(), &frame) != nil {
				return
			}
			if frame.Method == "" {
				continue
			}
			f.mu.Lock()
			f.methods = append(f.methods, frame.Method)
			f.mu.Unlock()
			if len(frame.ID) == 0 {
				continue
			}
			var result any = map[string]any{}
			switch frame.Method {
			case "thread/start", "thread/resume":
				f.mu.Lock()
				gate := f.threadGate
				f.developer = agent.String(frame.Params["developerInstructions"])
				f.tools = nil
				for _, tool := range agent.Array(frame.Params["dynamicTools"]) {
					f.tools = append(f.tools, agent.String(agent.Map(tool)["name"]))
				}
				f.mu.Unlock()
				if gate != nil {
					<-gate
				}
				result = map[string]any{"thread": map[string]any{"id": "thread-one", "turns": []any{}}}
			case "turn/interrupt":
				// Codex answers at once and ends the turn as interrupted.
				f.send(map[string]any{"id": frame.ID, "result": map[string]any{}})
				f.mu.Lock()
				ignore := f.ignoreInterrupt
				f.mu.Unlock()
				if !ignore {
					f.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": frame.Params["turnId"], "status": "interrupted"}}})
				}
				continue
			case "turn/start":
				f.mu.Lock()
				f.turns++
				f.inputs = append(f.inputs, agent.Array(frame.Params["input"]))
				turnID := f.turnID
				gate := f.turnGate
				f.mu.Unlock()
				if gate != nil {
					<-gate
				}
				if turnID == "" {
					turnID = "turn-one"
				}
				result = map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}
			case "conversation/rewind":
				f.mu.Lock()
				answer := f.rewindAnswer
				f.mu.Unlock()
				if answer == nil {
					answer = map[string]any{"rewound": true}
				}
				result = answer
			case "turn/steer":
				f.mu.Lock()
				f.inputs = append(f.inputs, agent.Array(frame.Params["input"]))
				f.mu.Unlock()
				if f.rejectSteer {
					f.send(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32000, "message": "turn completed before steering"}})
					continue
				}
				result = map[string]any{"turnId": "turn-one"}
			}
			f.send(map[string]any{"id": frame.ID, "result": result})
		}
	}()
	return client, sandbox.Response{Version: 2}, nil
}

// setup opens a store and serves an engine on it; configure runs before
// Serve starts, the place for fields Serve reads (PolicyAddress, Bugs).
func setup(t *testing.T, configure ...func(*Engine)) (*Engine, *fakeWorker, context.CancelFunc) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorker{}
	e := NewEngine(s, w)
	e.ResidentProviders = []string{} // these tests exercise one run per message; resident_test.go covers sessions
	for _, f := range configure {
		f(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go e.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Error("engine did not stop")
		}
		s.Close()
	})
	return e, w, cancel
}
func until(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
func TestRunStreamingSteeringResume(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	message := cv.ID()
	if err = e.Message(id, "Hello", message); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		c := e.Store.Snapshot().chat(id)
		return len(c.Conversation.Entries) > 0 && c.Conversation.Entries[0].Delivery == "sent"
	})
	// Retried HTTP requests don't enqueue duplicate messages.
	if err = e.Message(id, "Hello", message); err != nil {
		t.Fatal(err)
	}
	w.send(agent.Frame{Method: "item/agentMessage/delta", Params: map[string]any{"itemId": "answer", "turnId": "turn-one", "delta": "Hello from SBX"}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Entries) == 2 })
	steering := cv.ID()
	if err = e.Message(id, "Also test steering", steering); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
			if v.ID == steering {
				return v.Delivery == "sent"
			}
		}
		return false
	})
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 3 || c.Conversation.Entries[1].IsStreaming {
		t.Fatalf("bad transcript: %+v", c)
	}
	if err = e.Message(id, "Resume", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, m := range w.methods {
			if m == "thread/resume" {
				return true
			}
		}
		return false
	})
	if err = e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "interrupted" })
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.requests {
		if r.ProjectID != "warden-local" || r.PrincipalID != "owner" || r.ChatID != id || r.SandboxID == "" {
			t.Fatalf("missing trusted binding: %+v", r)
		}
	}
}
func TestFailClosedAndRecovery(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorker{fail: true}
	e := NewEngine(s, w)
	id, _ := e.Create("Failure", "", "", nil)
	_ = e.Message(id, "Run", cv.ID())
	e.run(context.Background(), id)
	c := s.Snapshot().chat(id)
	if c.Status != "failed" || c.Conversation.Entries[0].Delivery != "failed" || len(w.methods) > 0 {
		t.Fatalf("not fail closed: %+v", c)
	}
	// A restart catches a message handed over but not yet confirmed: it is
	// failed as unconfirmed, never replayed.
	_ = s.update(func(st *State) error {
		c := st.chat(id)
		c.Status = "running"
		c.Conversation.Entries[0].Delivery = "sending"
		c.Conversation.Entries[0].Detail = ""
		return nil
	})
	root := s.root
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c = s.Snapshot().chat(id)
	if c.Status != "interrupted" || c.Conversation.Entries[0].Delivery != "failed" || c.Conversation.Entries[0].Detail != "Interrupted before confirmed delivery" {
		t.Fatalf("replayed unconfirmed delivery: %+v", c.Conversation.Entries[0])
	}
	info, _ := os.Stat(filepath.Join(s.root, dbFile))
	if info.Mode().Perm() != 0600 {
		t.Fatal("transcript is not private")
	}
	if _, err = Open(root); err == nil {
		t.Fatal("second writer accepted")
	}
}
func TestUnconfirmedSteeringNeverReplayed(t *testing.T) {
	e, w, _ := setup(t)
	w.rejectSteer = true
	id, _ := e.Create("Steering", "", "", nil)
	_ = e.Message(id, "Hello", cv.ID())
	until(t, func() bool { return e.Store.Snapshot().chat(id).Conversation.Entries[0].Delivery == "sent" })
	mid := cv.ID()
	_ = e.Message(id, "Late instruction", mid)
	until(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, m := range w.methods {
			if m == "turn/steer" {
				return true
			}
		}
		return false
	})
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	c := e.Store.Snapshot().chat(id)
	if c.Conversation.Entries[1].Delivery != "failed" {
		t.Fatal("uncertain delivery replayed")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.turns != 1 {
		t.Fatal("unconfirmed steering started a new turn")
	}
}
func TestApprovalsBoundToActiveRun(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("Approval", "", "", nil)
	_ = e.Message(id, "Hello", cv.ID())
	until(t, func() bool { return e.Store.Snapshot().chat(id).Conversation.Entries[0].Delivery == "sent" })
	w.send(agent.Frame{ID: json.RawMessage(`42`), Method: "item/commandExecution/requestApproval", Params: map[string]any{"command": "ls"}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Approvals) == 1 })
	aid := e.Store.Snapshot().chat(id).Approvals[0].ID
	if err := e.Resolve("other", aid, true, nil); err == nil {
		t.Fatal("cross-chat approval accepted")
	}
	if err := e.Resolve(id, aid, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(id, aid, true, nil); err == nil {
		t.Fatal("approval replayed")
	}
}
func TestHTTPAuthenticationAndBindings(t *testing.T) {
	e, w, _ := setup(t)
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	for _, tc := range []struct {
		host, origin, token string
		code                int
	}{{"evil.test", "", "private", 403}, {h.Host, "https://evil.test", "private", 403}, {h.Host, "", "", 401}, {h.Host, "", "private", 200}} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/api/state", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("status %d, want %d", w.Code, tc.code)
		}
	}
	r := httptest.NewRequest("POST", h.Origin+"/api/chats", strings.NewReader(`{"title":"test","principalID":"attacker"}`))
	r.Header.Set("Authorization", "Bearer private")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != http.StatusBadRequest {
		t.Fatal("untrusted principal accepted")
	}
	id, _ := e.Create("status", "", "", nil)
	w.steal = true
	if _, err := e.Runtime(context.Background(), id, "status"); err == nil {
		t.Fatal("wrong environment returned")
	}
}
func TestEnvironmentSharingAndArchive(t *testing.T) {
	e, _, _ := setup(t)
	id, _ := e.Create("One", "", "github://owner/repo", nil)
	one := e.Store.Snapshot().chat(id)
	shared, err := e.Create("Two", one.SandboxID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	two := e.Store.Snapshot().chat(shared)
	if two.SandboxID != one.SandboxID || two.Repository != one.Repository {
		t.Fatal("sharing lost binding")
	}
	if _, err = e.Create("Other", "unknown", "", nil); err == nil {
		t.Fatal("unknown sandbox accepted")
	}
	if err = e.Edit(id, "Renamed", true); err != nil {
		t.Fatal(err)
	}
	if err = e.Message(id, "should reject", cv.ID()); err == nil {
		t.Fatal("archived chat accepted a run")
	}
}

func TestEnvironmentsListStopAndDelete(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("One", "", "github://owner/repo", nil)
	one := e.Store.Snapshot().chat(id)
	shared, _ := e.Create("Two", one.SandboxID, "", nil)
	other, _ := e.Create("Alone", "", "", nil)
	envs, err := e.Environments(context.Background())
	if err != nil || len(envs) != 2 {
		t.Fatal(envs, err)
	}
	if envs[0].ID != one.SandboxID || envs[0].Name != "One" || len(envs[0].Chats) != 2 || envs[0].Chats[1].ID != shared || envs[0].Repository != "github://owner/repo" || envs[0].Runtime != nil {
		t.Fatalf("bad environment: %+v", envs[0])
	}
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "running" })
	if err = e.DeleteEnvironment(context.Background(), one.SandboxID); err == nil {
		t.Fatal("deleted an environment with a running chat")
	}
	// Stopping the workspace ends its running chat first (the owner asked
	// for the workspace to stop), then the sandbox.
	if err = e.StopEnvironment(context.Background(), one.SandboxID); err != nil {
		t.Fatalf("stop under a running chat: %v", err)
	}
	if status := e.Store.Snapshot().chat(id).Status; status == "running" || status == "stopping" {
		t.Fatalf("chat still %s after the workspace stop", status)
	}
	envs, _ = e.Environments(context.Background())
	if envs[0].Runtime == nil || envs[0].Runtime.ID != one.SandboxID {
		t.Fatalf("runtime not reported once a chat has run: %+v", envs[0])
	}
	if err = e.StopEnvironment(context.Background(), one.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err = e.DeleteEnvironment(context.Background(), one.SandboxID); err != nil {
		t.Fatal(err)
	}
	ops := map[string]int{}
	w.mu.Lock()
	for _, r := range w.requests {
		if r.SandboxID == one.SandboxID {
			ops[r.Operation]++
		}
	}
	w.mu.Unlock()
	if ops["stop"] == 0 || ops["remove"] != 1 {
		t.Fatalf("worker did not stop then remove the sandbox: %v", ops)
	}
	st := e.Store.Snapshot()
	if !st.chat(id).Archived || !st.chat(shared).Archived || st.chat(other).Archived || !st.deleted(one.SandboxID) {
		t.Fatal("deletion did not archive exactly the environment's chats")
	}
	if err = e.Message(id, "again", cv.ID()); err == nil {
		t.Fatal("deleted environment accepted a message")
	}
	if err = e.Edit(id, "One", false); err == nil {
		t.Fatal("chat on a deleted environment was restored")
	}
	if _, err = e.Create("Three", one.SandboxID, "", nil); err == nil {
		t.Fatal("new chat joined a deleted environment")
	}
	envs, _ = e.Environments(context.Background())
	alone := e.Store.Snapshot().chat(other).SandboxID
	if len(envs) != 2 || envs[0].ID != alone || envs[0].Deleted || envs[1].ID != one.SandboxID || !envs[1].Deleted {
		t.Fatalf("deleted environment should list last: %+v", envs)
	}
	if err = e.DeleteEnvironment(context.Background(), "missing"); err == nil {
		t.Fatal("unknown environment deleted")
	}
}

func TestPreviewURLsAndIdentity(t *testing.T) {
	var e Engine
	for _, value := range []struct {
		url   string
		valid bool
	}{{"http://127.0.0.1:32100/", true}, {"https://evil.example/", false}, {"http://localhost:32100/", false}, {"http://user@127.0.0.1:32100/", false}, {"javascript:alert(1)", false}, {"https://warden-runner:7446/abc/", false}} {
		err := e.validateAttachment(sandbox.PreviewAttachment{State: "available", URL: value.url})
		if (err == nil) != value.valid {
			t.Errorf("URL validation failed for %q", value.url)
		}
	}
	// With the runner's shared preview server configured, exactly that
	// https origin is accepted beside the loopback listeners.
	e.RunnerPreviewHost = "warden-runner:7446"
	for _, value := range []struct {
		url   string
		valid bool
	}{{"https://warden-runner:7446/abc/", true}, {"https://warden-runner:7446/abc", true}, {"http://127.0.0.1:32100/", true}, {"https://warden-runner:7447/abc/", false}, {"https://warden-runner/abc/", false}, {"http://warden-runner:7446/abc/", false}, {"https://user@warden-runner:7446/abc/", false}, {"https://warden-runner:7446/abc/?x=1", false}, {"https://evil.example:7446/abc/", false}} {
		err := e.validateAttachment(sandbox.PreviewAttachment{State: "available", URL: value.url})
		if (err == nil) != value.valid {
			t.Errorf("URL validation failed for %q with the shared server", value.url)
		}
	}
}

// Model the real worker's asynchronous cleanup, which rejects stop until cancel
// has been acknowledged and the active reservation has gone away.
type orderedWorker struct {
	fakeWorker
	orderMu   sync.Mutex
	cancelled bool
	stops     int
}

func (w *orderedWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	w.orderMu.Lock()
	if r.Operation == "cancel" {
		w.cancelled = true
		w.orderMu.Unlock()
		return sandbox.Response{}, nil
	}
	if r.Operation == "stop" {
		w.stops++
		if !w.cancelled {
			w.orderMu.Unlock()
			return sandbox.Response{}, errors.New("cancel must precede stop")
		}
		if w.stops == 1 {
			w.orderMu.Unlock()
			return sandbox.Response{}, errors.New("sandbox has an active run")
		}
	}
	w.orderMu.Unlock()
	return w.fakeWorker.Call(ctx, r)
}
func (w *orderedWorker) outcome() (bool, int) {
	w.orderMu.Lock()
	defer w.orderMu.Unlock()
	return w.cancelled, w.stops
}

// A run whose agent is not up yet (the sandbox still preparing) has nothing
// to interrupt: Stop falls back to cancelling the run, tombstoning it on
// the runner before the stop, and waits out the runner's cleanup.
func TestStopCancelsBeforeStoppingAndWaitsForCleanup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &orderedWorker{}
	w.prepareGate = make(chan struct{})
	e := NewEngine(s, w)
	e.ResidentProviders = []string{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Serve(ctx)
	id, _ := e.Create("Stop", "", "", nil)
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.count("prepare") == 1 })
	if err = e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	cancelled, stops := w.outcome()
	if !cancelled || stops != 2 || s.Snapshot().chat(id).Status != "interrupted" {
		t.Fatalf("stop did not wait for cancelled run cleanup: cancelled %v, stops %d, status %s", cancelled, stops, s.Snapshot().chat(id).Status)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	cancel()
	<-e.done
}

// Stop on a turn in flight interrupts it through the agent's protocol: the
// chat is handed back as interrupted with what streamed so far, the run
// ends cleanly (no cancel tombstone, so the runner keeps the sandbox) and
// the next message runs as usual.
func TestStopInterruptsTurnWithoutStoppingSandbox(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("Interrupt", "", "", nil)
	sendAndDeliver(t, e, id, "think hard")
	w.send(agent.Frame{Method: "item/agentMessage/delta", Params: map[string]any{"itemId": "answer", "turnId": "turn-one", "delta": "Half an"}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Entries) == 2 })
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Status != "interrupted" || c.Error != "" {
		t.Fatalf("after stop: %s %q", c.Status, c.Error)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	w.mu.Lock()
	methods := append([]string(nil), w.methods...)
	w.mu.Unlock()
	if !slices.Contains(methods, "turn/interrupt") {
		t.Fatalf("turn not interrupted: %v", methods)
	}
	if w.count("cancel") != 0 || w.count("stop") != 0 {
		t.Fatalf("stop touched the sandbox: %d cancels, %d stops", w.count("cancel"), w.count("stop"))
	}
	c = e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 2 || c.Conversation.Entries[1].Text != "Half an" || c.Conversation.Entries[1].IsStreaming {
		t.Fatalf("interrupted transcript: %+v", c.Conversation.Entries)
	}
	sendAndDeliver(t, e, id, "carry on")
	until(t, func() bool { return w.turnCount() == 2 })
}

// An agent that answers the interrupt but never ends the turn is cancelled
// after the grace, as before: the run is tombstoned and the sandbox stopped.
func TestStopFallsBackToCancelWhenAgentIgnoresInterrupt(t *testing.T) {
	e, w, _ := setup(t)
	w.ignoreInterrupt = true
	grace := interruptGrace
	interruptGrace = 300 * time.Millisecond
	t.Cleanup(func() { interruptGrace = grace })
	id, _ := e.Create("Hung", "", "", nil)
	sendAndDeliver(t, e, id, "hang")
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.Status != "interrupted" {
		t.Fatalf("after stop: %s %q", c.Status, c.Error)
	}
	if w.count("cancel") == 0 || w.count("stop") == 0 {
		t.Fatalf("the hung run was not cancelled: %d cancels, %d stops", w.count("cancel"), w.count("stop"))
	}
}

// Stop while the agent is up but has no turn yet (its session starting)
// ends the run cleanly rather than tombstoning it: nothing runs in the
// guest that a cancel would need to kill.
func TestStopBeforeFirstTurnEndsRunWithoutCancel(t *testing.T) {
	e, w, _ := setup(t)
	gate := make(chan struct{})
	w.threadGate = gate
	id, _ := e.Create("Starting", "", "", nil)
	if err := e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return slices.Contains(w.methods, "thread/start")
	})
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	close(gate)
	c := e.Store.Snapshot().chat(id)
	if c.Status != "interrupted" || c.Error != "" {
		t.Fatalf("after stop: %s %q", c.Status, c.Error)
	}
	if w.count("cancel") != 0 || w.count("stop") != 0 {
		t.Fatalf("stop touched the sandbox: %d cancels, %d stops", w.count("cancel"), w.count("stop"))
	}
}

// A chat waiting for its run (queued, nothing started) is just taken off
// the queue, its message held for later; the sandbox is not touched.
func TestStopQueuedChatTouchesNothing(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &fakeWorker{}
	e := NewEngine(s, w)
	id, _ := e.Create("Queued", "", "", nil)
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err = e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	c := s.Snapshot().chat(id)
	if c.Status != "interrupted" || c.Conversation.Entries[0].Delivery != "queued" || len(w.requests) != 0 {
		t.Fatalf("after stop: %s, delivery %s, runner calls %d", c.Status, c.Conversation.Entries[0].Delivery, len(w.requests))
	}
}

func TestConversationAgentSelectionPersistsAndLocksProvider(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &fakeWorker{})
	id, err := e.Create("Claude", "", "", nil, "claude", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Message(id, "hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureAgent(id, "codex", ""); err == nil {
		t.Fatal("changed active provider")
	}
	_ = s.update(func(st *State) error { st.chat(id).Status = "idle"; return nil })
	if err = e.ConfigureAgent(id, "codex", ""); err == nil {
		t.Fatal("changed provider with history")
	}
	if err = e.ConfigureAgent(id, "claude", "opus"); err != nil {
		t.Fatal(err)
	}
	r := request(s.Snapshot().chat(id), "prepare")
	if r.Provider != "claude" || r.Model != "opus" {
		t.Fatal(r)
	}
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Snapshot().chat(id)
	if c.Provider != "claude" || c.Model != "opus" {
		t.Fatal(c)
	}
	if _, err = e.Create("bad", "", "", nil, "unknown", ""); err == nil {
		t.Fatal("accepted unknown provider")
	}
}

func TestArchiveEnvironmentStopsAndArchivesAllChats(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("One", "", "", nil)
	one := e.Store.Snapshot().chat(id)
	two, _ := e.Create("Two", one.SandboxID, "", nil)
	if err := e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "running" })
	// Archiving stops the running chat itself, as stopping the workspace does.
	if err := e.ArchiveEnvironment(context.Background(), one.SandboxID); err != nil {
		t.Fatalf("archive under a running chat: %v", err)
	}
	st := e.Store.Snapshot()
	if !st.chat(id).Archived || !st.chat(two).Archived || st.deleted(one.SandboxID) || st.chat(id).Status == "running" {
		t.Fatalf("archive must stop and archive every chat without deleting the workspace: %s", st.chat(id).Status)
	}
	stopped := false
	w.mu.Lock()
	for _, r := range w.requests {
		if r.SandboxID == one.SandboxID && r.Operation == "stop" {
			stopped = true
		}
	}
	w.mu.Unlock()
	if !stopped {
		t.Fatal("archive did not stop the sandbox")
	}
	envs, _ := e.Environments(context.Background())
	if len(envs) != 1 || !envs[0].Archived || envs[0].Deleted {
		t.Fatalf("archived workspace not reported: %+v", envs)
	}
	// Restoring a chat brings the workspace back; nothing was deleted.
	if err := e.Edit(two, "Two", false); err != nil {
		t.Fatal(err)
	}
	envs, _ = e.Environments(context.Background())
	if envs[0].Archived {
		t.Fatal("workspace stayed archived after a chat was restored")
	}
	if err := e.ArchiveEnvironment(context.Background(), "missing"); err == nil {
		t.Fatal("unknown workspace archived")
	}
}

// Messages are attributed to the person the edge identified (headers the
// edge sets and clients cannot), falling back to the owner; typing
// indicators show for TypingTTL after the last reported keystroke and
// vanish when that person sends.
func TestAttributionAndTypingIndicators(t *testing.T) {
	var now atomic.Int64 // the test's clock, read by the engine's run goroutines too
	now.Store(1000)
	e, _, _ := setup(t, func(e *Engine) { e.Now = func() time.Time { return time.Unix(now.Load(), 0) } })
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	id, _ := e.Create("shared", "", "", nil)
	call := func(path, body string, identity map[string]string) int {
		t.Helper()
		r := httptest.NewRequest("POST", h.Origin+"/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer private")
		for k, v := range identity {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	alice := map[string]string{"X-Warden-Principal": "sub-alice", "X-Warden-Email": "alice@example.com", "X-Warden-Name": "Alice Example"}
	bob := map[string]string{"X-Warden-Principal": "sub-bob", "X-Warden-Email": "bob@example.com"}
	if code := call("chats/"+id+"/typing", "{}", alice); code != 200 {
		t.Fatalf("typing: %d", code)
	}
	if code := call("chats/"+id+"/typing", "{}", bob); code != 200 {
		t.Fatalf("typing: %d", code)
	}
	if code := call("chats/unknown/typing", "{}", alice); code != 409 {
		t.Fatalf("typing in unknown chat: %d", code)
	}
	typing := e.View().chat(id).Typing
	if len(typing) != 2 || typing[0].Name != "Alice Example" || typing[0].PrincipalID != "sub-alice" || typing[1].Name != "bob@example.com" || typing[0].Until != 1008 {
		t.Fatalf("typing: %+v", typing)
	}
	if e.Store.Snapshot().chat(id).Typing != nil {
		t.Fatal("typing leaked into the stored state")
	}
	// Alice sends: her indicator goes, her message carries her identity.
	if code := call("chats/"+id+"/message", `{"text":"hello","id":"`+strings.Repeat("a", 32)+`"}`, alice); code != 200 {
		t.Fatalf("message: %d", code)
	}
	view := e.View().chat(id)
	if len(view.Typing) != 1 || view.Typing[0].PrincipalID != "sub-bob" {
		t.Fatalf("typing after send: %+v", view.Typing)
	}
	entry := view.Conversation.Entries[0]
	if entry.Sender == nil || entry.Sender.PrincipalID != "sub-alice" || entry.Sender.Email != "alice@example.com" || entry.Sender.Name != "Alice Example" {
		t.Fatalf("sender: %+v", entry.Sender)
	}
	// Without the edge's headers the owner is the sender.
	if code := call("chats/"+id+"/message", `{"text":"local","id":"`+strings.Repeat("b", 32)+`"}`, nil); code != 200 {
		t.Fatal("owner message refused")
	}
	if s := e.View().chat(id).Conversation.Entries[1].Sender; s == nil || s.PrincipalID != "owner" || s.Name != "" {
		t.Fatalf("owner sender: %+v", s)
	}
	// Bob's indicator lapses eight seconds after his last keystroke.
	now.Store(1007)
	if len(e.View().chat(id).Typing) != 1 {
		t.Fatal("indicator lapsed early")
	}
	now.Store(1008)
	if len(e.View().chat(id).Typing) != 0 {
		t.Fatal("indicator outlived its ttl")
	}
}

// A turn's record: begun when the agent accepts the message, its usage the
// growth of the process's running total (a repeated report of the same
// total counts nothing), ended when the turn completes; a stopped run ends
// the turn it cut short.
func TestTurnTimingAndTokenUsage(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("Usage", "", "", nil)
	before := float64(time.Now().UnixMilli()) / 1000
	_ = e.Message(id, "Count", cv.ID())
	until(t, func() bool { return e.Store.Snapshot().chat(id).Conversation.Entries[0].Delivery == "sent" })
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Turns) != 1 || c.Conversation.Turns[0].ID != "turn-one" || c.Conversation.Turns[0].StartedAt < before || c.Conversation.Turns[0].EndedAt != 0 || c.Conversation.Turns[0].Usage != nil {
		t.Fatalf("turn not begun: %+v", c.Conversation.Turns)
	}
	report := func(turn string, input, cached, output, total float64) {
		breakdown := map[string]any{"inputTokens": input, "cachedInputTokens": cached, "outputTokens": output, "reasoningOutputTokens": 0, "totalTokens": total}
		w.send(agent.Frame{Method: "thread/tokenUsage/updated", Params: map[string]any{"threadId": "thread-one", "turnId": turn, "tokenUsage": map[string]any{"last": breakdown, "total": breakdown}}})
	}
	report("turn-one", 1000, 600, 200, 1200)
	usage := func() *cv.Usage {
		turns := e.Store.Snapshot().chat(id).Conversation.Turns
		if len(turns) == 0 {
			return nil
		}
		return turns[0].Usage
	}
	until(t, func() bool { u := usage(); return u != nil && u.Total == 1200 })
	report("turn-one", 2500, 1500, 700, 3200)
	until(t, func() bool { u := usage(); return u != nil && u.Total == 3200 })
	if u := usage(); u.Input != 2500 || u.Cached != 1500 || u.Output != 700 {
		t.Fatalf("usage %+v", u)
	}
	// The agent's context report is kept on the conversation as it stands.
	w.send(agent.Frame{Method: "thread/context/updated", Params: map[string]any{"threadId": "thread-one", "turnId": "turn-one", "context": map[string]any{"used": 42787.0, "window": 200000.0, "model": "claude-sonnet-5"}}})
	until(t, func() bool { c := e.Store.Snapshot().chat(id).Conversation.Context; return c != nil && c.Used == 42787 })
	if c := e.Store.Snapshot().chat(id).Conversation.Context; c.Window != 200000 || c.Model != "claude-sonnet-5" {
		t.Fatalf("context %+v", c)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	c = e.Store.Snapshot().chat(id)
	first := c.Conversation.Turns[0]
	if first.EndedAt < first.StartedAt || first.Usage.Total != 3200 {
		t.Fatalf("turn not ended: %+v", first)
	}
	// The next turn is a new process here (one run per message), whose
	// total starts again: its usage is that total, not a difference.
	w.mu.Lock()
	w.turnID = "turn-two"
	w.mu.Unlock()
	_ = e.Message(id, "Again", cv.ID())
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Turns) == 2 })
	report("turn-two", 800, 100, 100, 900)
	until(t, func() bool {
		turns := e.Store.Snapshot().chat(id).Conversation.Turns
		return turns[1].Usage != nil && turns[1].Usage.Total == 900
	})
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "interrupted" })
	turns := e.Store.Snapshot().chat(id).Conversation.Turns
	if turns[0].EndedAt != first.EndedAt || turns[1].EndedAt < turns[1].StartedAt || turns[1].Usage.Input != 800 {
		t.Fatalf("stopped run left the turn wrong: %+v", turns)
	}
}

// The agent's thread/started names the session's slash commands and
// settings (Claude's system/init); they are on the chat, in GET state as
// chat.commands and chat.session, survive a restart, and follow the
// next thread/started. A thread/started without them (Codex) leaves them
// alone. A compaction item (item 8) is a compaction entry in the
// transcript, its running state included.
func TestSessionCommandsInStateAndCompactionNote(t *testing.T) {
	e, w, _ := setup(t)
	// The fake worker speaks the Codex protocol; the frames below are what
	// the Claude adapter emits into it.
	id, _ := e.Create("Commands", "", "", nil)
	_ = e.Message(id, "/compact", cv.ID())
	until(t, func() bool { return e.Store.Snapshot().chat(id).Conversation.Entries[0].Delivery == "sent" })
	if c := e.Store.Snapshot().chat(id); len(c.Commands) != 0 || c.Session != nil {
		t.Fatalf("commands before the session reported any: %+v", c)
	}
	w.send(agent.Frame{Method: "thread/started", Params: map[string]any{"thread": map[string]any{"id": "thread-one",
		"commands": []any{map[string]any{"name": "compact"}, map[string]any{"name": "probe-cmd", "description": "From the workspace"}, map[string]any{"name": ""}},
		"model":    "claude-opus-5[1m]", "permissionMode": "default", "outputStyle": "default"}}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Commands) == 2 })
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	r := httptest.NewRequest("GET", "http://"+h.Host+"/api/state", nil)
	r.Header.Set("Authorization", "Bearer private")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	var state struct {
		Chats []struct {
			Commands []Command `json:"commands"`
			Session  *Session  `json:"session"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &state); err != nil || len(state.Chats) != 1 {
		t.Fatal(err, out.Body.String())
	}
	got := state.Chats[0]
	if len(got.Commands) != 2 || got.Commands[0] != (Command{Name: "compact"}) || got.Commands[1] != (Command{Name: "probe-cmd", Description: "From the workspace"}) {
		t.Fatalf("commands %+v", got.Commands)
	}
	if got.Session == nil || *got.Session != (Session{Model: "claude-opus-5[1m]", PermissionMode: "default", OutputStyle: "default"}) {
		t.Fatalf("session %+v", got.Session)
	}
	w.send(agent.Frame{Method: "item/started", Params: map[string]any{"turnId": "turn-one", "item": map[string]any{"id": "k1", "type": "compaction", "status": "running"}}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Entries) == 2 })
	if note := e.Store.Snapshot().chat(id).Conversation.Entries[1]; note.Role != "compaction" || note.Text != "Compacting context…" || !note.IsStreaming {
		t.Fatalf("running compaction %+v", note)
	}
	w.send(agent.Frame{Method: "item/completed", Params: map[string]any{"turnId": "turn-one", "item": map[string]any{"id": "k1", "type": "compaction", "status": "completed", "trigger": "manual", "preTokens": 27230.0, "postTokens": 1850.0, "summary": "This session is being continued…"}}})
	until(t, func() bool { return !e.Store.Snapshot().chat(id).Conversation.Entries[1].IsStreaming })
	if note := e.Store.Snapshot().chat(id).Conversation.Entries[1]; note.Role != "compaction" || note.Text != "Context compacted" || note.Detail != "This session is being continued…" || note.Compaction == nil || note.Compaction.Trigger != "manual" || note.Compaction.PreTokens != 27230 || note.Compaction.PostTokens != 1850 {
		t.Fatalf("note %+v %+v", note, note.Compaction)
	}
	// Codex's thread/started, and a later Claude init with fewer commands.
	w.send(agent.Frame{Method: "thread/started", Params: map[string]any{"thread": map[string]any{"id": "thread-one"}}})
	w.send(agent.Frame{Method: "item/completed", Params: map[string]any{"turnId": "turn-one", "item": map[string]any{"id": "k2", "type": "compaction", "status": "completed", "trigger": "auto"}}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Entries) == 3 })
	if c := e.Store.Snapshot().chat(id); len(c.Commands) != 2 || c.Session == nil || c.Conversation.Entries[2].Role != "compaction" || c.Conversation.Entries[2].Compaction.Trigger != "auto" {
		t.Fatalf("unchanged by a bare thread/started: %+v", c)
	}
	w.send(agent.Frame{Method: "thread/started", Params: map[string]any{"thread": map[string]any{"id": "thread-one", "commands": []any{map[string]any{"name": "init"}}}}})
	until(t, func() bool {
		c := e.Store.Snapshot().chat(id)
		return len(c.Commands) == 1 && c.Commands[0].Name == "init"
	})
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	if c := e.Store.Snapshot().chat(id); c.Session == nil || len(c.Commands) != 1 {
		t.Fatalf("session lost with the turn: %+v", c)
	}
	// The commands belong to the provider: choosing the other one drops them
	// (only possible before the chat has history).
	fresh, _ := e.Create("Fresh", "", "", nil, "claude", "")
	_ = e.Store.update(func(st *State) error {
		st.chat(fresh).sessionStarted(map[string]any{"commands": []any{map[string]any{"name": "compact"}}, "model": "m"})
		return nil
	})
	if err := e.ConfigureAgent(fresh, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(fresh); len(c.Commands) != 0 || c.Session != nil {
		t.Fatalf("commands survived the provider change: %+v", c)
	}
}

// A message handed to the agent reads "sending" until the turn confirms
// it, and "sent" after; it is never marked failed on the way (that mark
// used to flash under every first message while the agent started).
func TestHandOverIsSendingUntilConfirmed(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	w.mu.Lock()
	w.turnGate = gate
	w.mu.Unlock()
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return startupOf(e, id).Stage == stageSending })
	if v := e.View().chat(id).Conversation.Entries[0]; v.Delivery != "sending" || v.Detail != "" {
		t.Fatalf("in flight: %+v", v)
	}
	close(gate)
	until(t, func() bool { return e.View().chat(id).Conversation.Entries[0].Delivery == "sent" })
	if v := e.View().chat(id).Conversation.Entries[0]; v.Detail != "" || v.TurnID == nil {
		t.Fatalf("confirmed: %+v", v)
	}
}

// A run that ends while a message is in flight leaves it failed as
// unconfirmed: the agent may or may not have read it.
func TestHandOverUnconfirmedWhenTheRunEnds(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	defer close(gate)
	w.mu.Lock()
	w.turnGate = gate
	w.mu.Unlock()
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return startupOf(e, id).Stage == stageSending })
	if err = e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.View().chat(id).Conversation.Entries[0].Delivery == "failed" })
	if v := e.View().chat(id).Conversation.Entries[0]; v.Detail != deliveryUnconfirmed {
		t.Fatalf("unconfirmed: %+v", v)
	}
}

// The service's own shutdown cuts a run short without tombstoning it on
// the runner: a cancel would make the runner stop the sandbox, and a
// redeploy would take every live workspace down with it. The runner sees
// a plain disconnect and keeps the sandbox for the run that resumes.
func TestShutdownEndsRunWithoutCancellingSandbox(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &fakeWorker{}
	w.prepareGate = make(chan struct{})
	e := NewEngine(s, w)
	e.ResidentProviders = []string{}
	ctx, cancel := context.WithCancel(context.Background())
	go e.Serve(ctx)
	id, _ := e.Create("Shutdown", "", "", nil)
	if err = e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.count("prepare") == 1 })
	cancel()
	select {
	case <-e.done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not stop")
	}
	if w.count("cancel") != 0 || w.count("stop") != 0 {
		t.Fatalf("shutdown touched the sandbox: %d cancels, %d stops", w.count("cancel"), w.count("stop"))
	}
}
