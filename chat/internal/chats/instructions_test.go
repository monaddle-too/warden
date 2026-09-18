package chats

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

func getAs(t *testing.T, h *HTTP, path, principal string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "http://localhost:18780/api/"+path, nil)
	r.Header.Set("Authorization", "Bearer owner-secret")
	if principal != "" {
		r.Header.Set("X-Warden-Principal", principal)
		r.Header.Set("X-Warden-Name", "Ada")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// me/instructions is each person's own text: the owner's and a signed-in
// user's are separate, a blank save removes it, the size is bounded, and
// the state clients stream never carries anyone's.
func TestInstructionsRoutesArePerPerson(t *testing.T) {
	e, _, _ := setup(t)
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	var got Instructions
	rec := getAs(t, h, "me/instructions", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Text != "" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec = postAs(t, h, "me/instructions", map[string]any{"text": "Answer in haiku form.\r\n\n"}, ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Text != "Answer in haiku form." || got.UpdatedAt == 0 || got.Name != "the owner" {
		t.Fatalf("%s", rec.Body.String())
	}
	if rec = postAs(t, h, "me/instructions", map[string]any{"text": "Be terse."}, "user-1"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = getAs(t, h, "me/instructions", "user-1")
	if json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Text != "Be terse." || got.Name != "Ada" {
		t.Fatalf("%s", rec.Body.String())
	}
	rec = getAs(t, h, "me/instructions", "")
	if json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Text != "Answer in haiku form." {
		t.Fatalf("owner's changed: %s", rec.Body.String())
	}
	if rec = postAs(t, h, "me/instructions", map[string]any{"text": strings.Repeat("x", MaxInstructions+1)}, ""); rec.Code != 409 {
		t.Fatalf("over-long accepted: %d", rec.Code)
	}
	if rec = postAs(t, h, "me/instructions", map[string]any{"text": "x", "extra": 1}, ""); rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if rec = postAs(t, h, "me/instructions", map[string]any{"text": "  \n"}, "user-1"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.Store.Snapshot().Instructions["user-1"]; ok {
		t.Fatal("blank save kept the text")
	}
	if rec = getAs(t, h, "state", "user-1"); strings.Contains(rec.Body.String(), "haiku") || strings.Contains(rec.Body.String(), "instructions") {
		t.Fatalf("state leaks instructions: %s", rec.Body.String())
	}
	if e.View().Instructions != nil {
		t.Fatal("view carries instructions")
	}
}

// The blocks: named after the person (quoted), the creator first, then
// senders in order of first appearance, people without text left out; a
// message prefix only for text the session was not given.
func TestInstructionBlocks(t *testing.T) {
	owner := cv.Actor{PrincipalID: "owner"}
	ada := cv.Actor{PrincipalID: "u1", Name: "Ada", Email: "ada@example.com"}
	bob := cv.Actor{PrincipalID: "u2", Email: "bob@example.com"}
	st := State{Instructions: map[string]*Instructions{
		"owner": {Text: "Answer in haiku form."},
		"u1":    {Text: "Be terse.\n"},
		"u3":    {Text: "unused"},
	}}
	c := &Chat{Creator: &owner, Conversation: cv.Conversation{Entries: []cv.Entry{
		{Role: "user", Sender: &bob}, {Role: "user", Sender: &ada}, {Role: "assistant"}, {Role: "user", Sender: &owner}, {Role: "user", Sender: &ada},
	}}}
	text, delivered := sessionInstructions(&st, c)
	want := instructionsHeader + "\n\nFrom \"the owner\":\nAnswer in haiku form.\n\nFrom \"Ada\":\nBe terse."
	if text != want {
		t.Fatalf("%q", text)
	}
	if len(delivered) != 2 || delivered["owner"] != "Answer in haiku form." || delivered["u1"] != "Be terse.\n" {
		t.Fatalf("%+v", delivered)
	}
	// Bob has no text: no prefix. Ada's was delivered: no prefix. A change
	// to Ada's: one prefix, then none.
	if p := messageInstructions(&st, cv.Entry{Sender: &bob}, delivered); p != "" {
		t.Fatalf("bob: %q", p)
	}
	if p := messageInstructions(&st, cv.Entry{Sender: &ada}, delivered); p != "" {
		t.Fatalf("ada again: %q", p)
	}
	st.Instructions["u1"].Text = "Be brief."
	if p := messageInstructions(&st, cv.Entry{Sender: &ada}, delivered); !strings.HasPrefix(p, "[Warden: the standing instructions of the sender") || !strings.HasSuffix(p, "\nFrom \"Ada\":\nBe brief.]") {
		t.Fatalf("ada changed: %q", p)
	}
	if p := messageInstructions(&st, cv.Entry{Sender: &ada}, delivered); p != "" {
		t.Fatalf("ada delivered twice: %q", p)
	}
	// A name is quoted; without name or email the stored name serves.
	st.Instructions["u2"] = &Instructions{Text: "x", Name: "Bob"}
	if p := messageInstructions(&st, cv.Entry{Sender: &cv.Actor{PrincipalID: "u2"}}, map[string]string{}); !strings.HasSuffix(p, "\nFrom \"Bob\":\nx]") {
		t.Fatalf("%q", p)
	}
	items := withInstructions([]any{map[string]any{"type": "text", "text": "hello", "text_elements": []any{}}, map[string]any{"type": "localImage"}}, "[x]")
	if len(items) != 2 || agent.Map(items[0])["text"] != "[x]\n\nhello" || agent.Map(items[0])["type"] != "text" || agent.Map(items[1])["type"] != "localImage" {
		t.Fatalf("%+v", items)
	}
	if got := withInstructions(items, ""); len(got) != 2 || agent.Map(got[0])["text"] != "[x]\n\nhello" {
		t.Fatalf("%+v", got)
	}
}

// A session's launch carries the participants' blocks (the stream request
// for Claude's system prompt, developerInstructions for Codex); a late
// joiner's first message into the live session carries their block as a
// prefix, their next one does not; the creator is recorded.
func TestInstructionsReachTheLaunchAndLateJoiners(t *testing.T) {
	e, w := residentSetup(t)
	owner := cv.Actor{PrincipalID: "owner"}
	ada := cv.Actor{PrincipalID: "u1", Name: "Ada"}
	if err := e.SetInstructions(owner, "Answer in haiku form."); err != nil {
		t.Fatal(err)
	}
	if err := e.SetInstructions(ada, "Be terse."); err != nil {
		t.Fatal(err)
	}
	id, err := e.CreateFrom(owner, "Test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.Creator == nil || c.Creator.PrincipalID != "owner" {
		t.Fatalf("creator %+v", c.Creator)
	}
	if err = e.MessageFrom(id, "Hello", cv.ID(), owner); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 1 })
	w.mu.Lock()
	var stream sandbox.Request
	for _, r := range w.requests {
		if r.Operation == "stream" {
			stream = r
		}
	}
	first := agent.Map(w.inputs[0][0])["text"]
	developer := w.developer
	w.mu.Unlock()
	if stream.Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in haiku form." {
		t.Fatalf("stream instructions %q", stream.Instructions)
	}
	if !strings.HasPrefix(developer, "You are an agent in a Warden-managed sandbox.") || !strings.HasSuffix(developer, "\n\nFrom \"the owner\":\nAnswer in haiku form.") {
		t.Fatalf("developerInstructions %q", developer)
	}
	if first != "Hello" {
		t.Fatalf("first message prefixed: %q", first)
	}
	completeTurn(t, e, w, id)
	// Ada joins the live session: her block once, as a prefix.
	if err = e.MessageFrom(id, "Hi from Ada", cv.ID(), ada); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 2 })
	w.mu.Lock()
	second, _ := agent.Map(w.inputs[1][0])["text"].(string)
	w.mu.Unlock()
	if !strings.HasPrefix(second, "[Warden: the standing instructions of the sender") || !strings.HasSuffix(second, "\nFrom \"Ada\":\nBe terse.]\n\nHi from Ada") {
		t.Fatalf("late joiner: %q", second)
	}
	completeTurn(t, e, w, id)
	if err = e.MessageFrom(id, "Again", cv.ID(), ada); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	w.mu.Lock()
	third := agent.Map(w.inputs[2][0])["text"]
	w.mu.Unlock()
	if third != "Again" {
		t.Fatalf("delivered twice: %q", third)
	}
	completeTurn(t, e, w, id)
	// The owner changes theirs: the next message carries the new text.
	if err = e.SetInstructions(owner, "Answer in limericks."); err != nil {
		t.Fatal(err)
	}
	if err = e.MessageFrom(id, "Once more", cv.ID(), owner); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 4 })
	w.mu.Lock()
	fourth, _ := agent.Map(w.inputs[3][0])["text"].(string)
	w.mu.Unlock()
	if !strings.HasSuffix(fourth, "\nFrom \"the owner\":\nAnswer in limericks.]\n\nOnce more") {
		t.Fatalf("changed text: %q", fourth)
	}
	completeTurn(t, e, w, id)
	// A fresh launch carries everyone's current text in order.
	e.releaseChat(t.Context(), id)
	until(t, func() bool { return !e.sessionAlive(id) })
	if err = e.MessageFrom(id, "Back", cv.ID(), owner); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 5 })
	w.mu.Lock()
	for _, r := range w.requests {
		if r.Operation == "stream" {
			stream = r
		}
	}
	fifth := agent.Map(w.inputs[4][0])["text"]
	w.mu.Unlock()
	if stream.Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in limericks.\n\nFrom \"Ada\":\nBe terse." {
		t.Fatalf("relaunch instructions %q", stream.Instructions)
	}
	if fifth != "Back" {
		t.Fatalf("relaunch prefixed: %q", fifth)
	}
	completeTurn(t, e, w, id)
}

// The memory routes: the listing comes from the runner with the chat's
// reported auto-memory directory and the read hint; a write validates
// scope and path, reaches the runner with the person's principal and the
// text, and leaves a notice naming the person and the file.
func TestMemoryRoutesListAndWriteWithAttribution(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Mem", "", "", nil, "claude")
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	err = e.Store.update(func(st *State) error {
		st.chat(id).Session = &Session{Model: "m", AutoMemory: "/home/agent/.claude/projects/-home-agent-workspace/memory"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.memory = &sandbox.MemoryListing{Root: "/home/agent/workspace", AutoDir: "/home/agent/.claude/projects/-home-agent-workspace/memory", Exists: true, Files: []sandbox.MemoryFile{{Scope: "workspace", Path: "CLAUDE.md", Size: 4, Text: "# hi"}, {Scope: "auto", Path: "MEMORY.md", Size: 3, Text: "abc"}}}
	before := len(w.requests)
	w.mu.Unlock()
	rec := getAs(t, h, "chats/"+id+"/memory", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var view MemoryView
	if err = json.Unmarshal(rec.Body.Bytes(), &view); err != nil || len(view.Files) != 2 || view.Files[1].Scope != "auto" || view.Read || !strings.Contains(view.Hint, "Claude reads these only") || view.AutoDir == "" || !view.Exists {
		t.Fatalf("%s", rec.Body.String())
	}
	w.mu.Lock()
	req := w.requests[before]
	w.mu.Unlock()
	if req.Operation != "memory-list" || req.Path != "/home/agent/.claude/projects/-home-agent-workspace/memory" || req.ChatID != id {
		t.Fatalf("%+v", req)
	}
	if rec = getAs(t, h, "chats/missing/memory", ""); rec.Code != 409 {
		t.Fatalf("missing chat: %d", rec.Code)
	}
	for _, bad := range []map[string]any{{"scope": "workspace", "path": "README.md", "text": "x"}, {"scope": "auto", "path": "../x.md", "text": "x"}, {"scope": "", "path": "CLAUDE.md", "text": "x"}, {"scope": "workspace", "path": "CLAUDE.md", "text": "a\x00b"}} {
		if rec = postAs(t, h, "chats/"+id+"/memory/write", bad, ""); rec.Code != 409 {
			t.Fatalf("%+v accepted: %d", bad, rec.Code)
		}
	}
	w.mu.Lock()
	before = len(w.requests)
	w.mu.Unlock()
	rec = postAs(t, h, "chats/"+id+"/memory/write", map[string]any{"scope": "workspace", "path": ".claude/rules/style.md", "text": "Use tabs.\r\n"}, "user-1")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	req = w.requests[before]
	w.mu.Unlock()
	if req.Operation != "memory-write" || req.Scope != "workspace" || req.Directory != ".claude/rules/style.md" || string(req.Bytes) != "Use tabs.\n" || req.PrincipalID != "user-1" || req.Path != "/home/agent/.claude/projects/-home-agent-workspace/memory" {
		t.Fatalf("%+v", req)
	}
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 1 {
		t.Fatalf("entries: %d", len(c.Conversation.Entries))
	}
	v := c.Conversation.Entries[0]
	if v.Role != "notice" || v.Text != "Ada edited .claude/rules/style.md" || v.Sender == nil || v.Sender.PrincipalID != "user-1" || v.Delivery != "" {
		t.Fatalf("%+v", v)
	}
	if rec = postAs(t, h, "chats/"+id+"/memory/write", map[string]any{"scope": "auto", "path": "MEMORY.md", "text": ""}, ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	c = e.Store.Snapshot().chat(id)
	if v = c.Conversation.Entries[1]; v.Text != "The owner edited auto-memory MEMORY.md" || v.Sender == nil || v.Sender.PrincipalID != "owner" {
		t.Fatalf("%+v", v)
	}
	if c.Status == "queued" || c.Status == "running" {
		t.Fatalf("a write queued a turn: %s", c.Status)
	}
	// A Codex chat's hint says it reads AGENTS.md.
	cid, _ := e.Create("Codex", "", "", nil, "codex")
	rec = getAs(t, h, "chats/"+cid+"/memory", "")
	if err = json.Unmarshal(rec.Body.Bytes(), &view); err != nil || !view.Read || !strings.Contains(view.Hint, "AGENTS.md") {
		t.Fatalf("%s", rec.Body.String())
	}
}
