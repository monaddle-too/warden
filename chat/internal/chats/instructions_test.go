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

// The blocks: the header, then one per person (quoted name), the creator
// first, then senders in order of first appearance, people without text
// left out; owed says whether a message's sender has text the session was
// not given, or had theirs changed or removed since.
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
	// Nobody with text: nothing appended, not even the header.
	if text, _ = sessionInstructions(&State{}, c); text != "" {
		t.Fatalf("%q", text)
	}
	// Bob has no text and was given none: nothing owed. Ada's was given.
	// A change to Ada's, or its removal, is owed; so is a late joiner's.
	if owed(&st, cv.Entry{Sender: &bob}, delivered) || owed(&st, cv.Entry{Sender: &ada}, delivered) || owed(&st, cv.Entry{}, delivered) {
		t.Fatal("owed with nothing changed")
	}
	st.Instructions["u1"].Text = "Be brief."
	if !owed(&st, cv.Entry{Sender: &ada}, delivered) {
		t.Fatal("changed text not owed")
	}
	delete(st.Instructions, "u1")
	if !owed(&st, cv.Entry{Sender: &ada}, delivered) {
		t.Fatal("removed text not owed")
	}
	if !owed(&st, cv.Entry{Sender: &cv.Actor{PrincipalID: "u3"}}, delivered) {
		t.Fatal("late joiner not owed")
	}
	// Without a name or email the stored name serves; without either, a
	// participant.
	st.Instructions["u2"] = &Instructions{Text: "x", Name: "Bob"}
	if b := instructionsBlock(cv.Actor{PrincipalID: "u2"}, st.Instructions["u2"]); b != "From \"Bob\":\nx" {
		t.Fatalf("%q", b)
	}
	if b := instructionsBlock(cv.Actor{PrincipalID: "u9"}, &Instructions{Text: "y"}); b != "From \"a participant\":\ny" {
		t.Fatalf("%q", b)
	}
}

// A session's launch carries the participants' blocks (the stream request
// for Claude's system prompt, developerInstructions for Codex); a late
// joiner's message into the live session, or one after a person changed
// their text, relaunches the session (a second stream request with the
// current blocks, the message itself untouched); a sender whose text the
// session has is answered on the same session.
func TestInstructionsReachTheLaunchAndRelaunchForLateJoiners(t *testing.T) {
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
	streams := func() []sandbox.Request {
		w.mu.Lock()
		defer w.mu.Unlock()
		var out []sandbox.Request
		for _, r := range w.requests {
			if r.Operation == "stream" {
				out = append(out, r)
			}
		}
		return out
	}
	input := func(i int) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		text, _ := agent.Map(w.inputs[i][0])["text"].(string)
		return text
	}
	if err = e.MessageFrom(id, "Hello", cv.ID(), owner); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 1 })
	w.mu.Lock()
	developer := w.developer
	w.mu.Unlock()
	if s := streams(); len(s) != 1 || s[0].Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in haiku form." {
		t.Fatalf("stream instructions %+v", s)
	}
	if !strings.HasPrefix(developer, "You are an agent in a Warden-managed sandbox.") || !strings.HasSuffix(developer, "\n\nFrom \"the owner\":\nAnswer in haiku form.") {
		t.Fatalf("developerInstructions %q", developer)
	}
	if input(0) != "Hello" {
		t.Fatalf("first message: %q", input(0))
	}
	completeTurn(t, e, w, id)
	// The owner again: the same session answers.
	sendAndDeliver(t, e, id, "Still me")
	until(t, func() bool { return w.turnCount() == 2 })
	if len(streams()) != 1 {
		t.Fatal("relaunched for a sender the session knows")
	}
	completeTurn(t, e, w, id)
	// Ada joins the live session: it is relaunched with both blocks and
	// her message is sent as written.
	if err = e.MessageFrom(id, "Hi from Ada", cv.ID(), ada); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if s := streams(); len(s) != 2 || s[1].Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in haiku form.\n\nFrom \"Ada\":\nBe terse." {
		t.Fatalf("relaunch: %+v", s)
	}
	if input(2) != "Hi from Ada" {
		t.Fatalf("late joiner's message: %q", input(2))
	}
	completeTurn(t, e, w, id)
	if err = e.MessageFrom(id, "Again", cv.ID(), ada); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 4 })
	if len(streams()) != 2 {
		t.Fatal("relaunched twice for the same text")
	}
	completeTurn(t, e, w, id)
	// The owner changes theirs: relaunched with the new text.
	if err = e.SetInstructions(owner, "Answer in limericks."); err != nil {
		t.Fatal(err)
	}
	if err = e.MessageFrom(id, "Once more", cv.ID(), owner); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 5 })
	if s := streams(); len(s) != 3 || s[2].Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in limericks.\n\nFrom \"Ada\":\nBe terse." {
		t.Fatalf("changed text: %+v", s)
	}
	if input(4) != "Once more" {
		t.Fatalf("%q", input(4))
	}
	completeTurn(t, e, w, id)
	// Removing theirs relaunches too, without their block.
	if err = e.SetInstructions(ada, ""); err != nil {
		t.Fatal(err)
	}
	if err = e.MessageFrom(id, "Gone", cv.ID(), ada); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 6 })
	if s := streams(); len(s) != 4 || s[3].Instructions != instructionsHeader+"\n\nFrom \"the owner\":\nAnswer in limericks." {
		t.Fatalf("removed text: %+v", s)
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
