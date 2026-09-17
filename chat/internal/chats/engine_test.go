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
	"strings"
	"sync"
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
	inputs      [][]any // the input items of every turn/start and turn/steer
}

func (f *fakeWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.fail {
		return sandbox.Response{}, errors.New("unverified sandbox")
	}
	if r.Operation == "stop" && f.conn != nil {
		f.conn.Close()
	}
	id := r.SandboxID
	if f.steal {
		id = "other"
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
				result = map[string]any{"thread": map[string]any{"id": "thread-one", "turns": []any{}}}
			case "turn/start":
				f.mu.Lock()
				f.turns++
				f.inputs = append(f.inputs, agent.Array(frame.Params["input"]))
				f.mu.Unlock()
				result = map[string]any{"turn": map[string]any{"id": "turn-one", "status": "inProgress"}}
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
func setup(t *testing.T) (*Engine, *fakeWorker, context.CancelFunc) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorker{}
	e := NewEngine(s, w)
	e.ResidentProviders = []string{} // these tests exercise one run per message; resident_test.go covers sessions
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
	id, err := e.Create("Test", "", "")
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
	id, _ := e.Create("Failure", "", "")
	_ = e.Message(id, "Run", cv.ID())
	e.run(context.Background(), id)
	c := s.Snapshot().chat(id)
	if c.Status != "failed" || c.Conversation.Entries[0].Delivery != "failed" || len(w.methods) > 0 {
		t.Fatalf("not fail closed: %+v", c)
	}
	_ = s.update(func(st *State) error {
		c := st.chat(id)
		c.Status = "running"
		c.Conversation.Entries[0].Delivery = "failed"
		c.Conversation.Entries[0].Detail = "Delivery unconfirmed"
		return nil
	})
	root := filepath.Dir(s.path)
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c = s.Snapshot().chat(id)
	if c.Status != "interrupted" || c.Conversation.Entries[0].Delivery != "failed" {
		t.Fatal("replayed unconfirmed delivery")
	}
	info, _ := os.Stat(s.path)
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
	id, _ := e.Create("Steering", "", "")
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
	id, _ := e.Create("Approval", "", "")
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
	id, _ := e.Create("status", "", "")
	w.steal = true
	if _, err := e.Runtime(context.Background(), id, "status"); err == nil {
		t.Fatal("wrong environment returned")
	}
}
func TestEnvironmentSharingAndArchive(t *testing.T) {
	e, _, _ := setup(t)
	id, _ := e.Create("One", "", "github://owner/repo")
	one := e.Store.Snapshot().chat(id)
	shared, err := e.Create("Two", one.SandboxID, "")
	if err != nil {
		t.Fatal(err)
	}
	two := e.Store.Snapshot().chat(shared)
	if two.SandboxID != one.SandboxID || two.Repository != one.Repository {
		t.Fatal("sharing lost binding")
	}
	if _, err = e.Create("Other", "unknown", ""); err == nil {
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
	id, _ := e.Create("One", "", "github://owner/repo")
	one := e.Store.Snapshot().chat(id)
	shared, _ := e.Create("Two", one.SandboxID, "")
	other, _ := e.Create("Alone", "", "")
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
	if err = e.StopEnvironment(context.Background(), one.SandboxID); err == nil {
		t.Fatal("stopped an environment under a running chat")
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
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
	if _, err = e.Create("Three", one.SandboxID, ""); err == nil {
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
	for _, value := range []struct {
		url   string
		valid bool
	}{{"http://127.0.0.1:32100/", true}, {"https://evil.example/", false}, {"http://localhost:32100/", false}, {"http://user@127.0.0.1:32100/", false}, {"javascript:alert(1)", false}} {
		err := validateAttachment(sandbox.PreviewAttachment{State: "available", URL: value.url})
		if (err == nil) != value.valid {
			t.Errorf("URL validation failed for %q", value.url)
		}
	}
}

// Model the real worker's asynchronous cleanup, which rejects stop until cancel
// has been acknowledged and the active reservation has gone away.
type orderedWorker struct {
	fakeWorker
	cancelled bool
	stops     int
}

func (w *orderedWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	if r.Operation == "cancel" {
		w.cancelled = true
		return sandbox.Response{}, nil
	}
	if r.Operation == "stop" {
		w.stops++
		if !w.cancelled {
			return sandbox.Response{}, errors.New("cancel must precede stop")
		}
		if w.stops == 1 {
			return sandbox.Response{}, errors.New("sandbox has an active run")
		}
	}
	return w.fakeWorker.Call(ctx, r)
}
func TestStopCancelsBeforeStoppingAndWaitsForCleanup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &orderedWorker{}
	e := NewEngine(s, w)
	id, _ := e.Create("Stop", "", "")
	_ = s.update(func(st *State) error { c := st.chat(id); c.Status = "running"; c.RunID = cv.ID(); return nil })
	if err = e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !w.cancelled || w.stops != 2 || s.Snapshot().chat(id).Status != "interrupted" {
		t.Fatal("stop did not wait for cancelled run cleanup")
	}
}

func TestConversationAgentSelectionPersistsAndLocksProvider(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &fakeWorker{})
	id, err := e.Create("Claude", "", "", "claude", "sonnet")
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
	if _, err = e.Create("bad", "", "", "unknown", ""); err == nil {
		t.Fatal("accepted unknown provider")
	}
}

func TestArchiveEnvironmentStopsAndArchivesAllChats(t *testing.T) {
	e, w, _ := setup(t)
	id, _ := e.Create("One", "", "")
	one := e.Store.Snapshot().chat(id)
	two, _ := e.Create("Two", one.SandboxID, "")
	if err := e.Message(id, "Hello", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "running" })
	if err := e.ArchiveEnvironment(context.Background(), one.SandboxID); err == nil {
		t.Fatal("archived a workspace with a running chat")
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	if err := e.ArchiveEnvironment(context.Background(), one.SandboxID); err != nil {
		t.Fatal(err)
	}
	st := e.Store.Snapshot()
	if !st.chat(id).Archived || !st.chat(two).Archived || st.deleted(one.SandboxID) {
		t.Fatal("archive must archive every chat without deleting the workspace")
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
	e, _, _ := setup(t)
	now := time.Unix(1000, 0)
	e.Now = func() time.Time { return now }
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	id, _ := e.Create("shared", "", "")
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
	now = time.Unix(1007, 0)
	if len(e.View().chat(id).Typing) != 1 {
		t.Fatal("indicator lapsed early")
	}
	now = time.Unix(1008, 0)
	if len(e.View().chat(id).Typing) != 0 {
		t.Fatal("indicator outlived its ttl")
	}
}
