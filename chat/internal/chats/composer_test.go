package chats

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/sandbox"
)

func postAs(t *testing.T, h *HTTP, path string, body map[string]any, principal string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "http://localhost:18780/api/"+path, strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer owner-secret")
	if principal != "" {
		r.Header.Set("X-Warden-Principal", principal)
		r.Header.Set("X-Warden-Name", "Ada")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// "!cmd" in the composer: the exec route runs the command through the
// runner's exec op with the person's principal, records a command card
// attributed to them (running, then the output and exit status), and
// never queues anything for the agent.
func TestExecRouteRunsAPersonsCommandAsACard(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Shell", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	if rec := postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "   "}, ""); rec.Code != 409 {
		t.Fatalf("empty command: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postAs(t, h, "chats/missing/exec", map[string]any{"text": "ls"}, ""); rec.Code != 409 {
		t.Fatalf("missing chat: %d", rec.Code)
	}
	w.mu.Lock()
	w.exec = &sandbox.ExecResult{Output: "total 0\ndrwxr-xr-x 2 agent agent 40 .\n", ExitCode: 0}
	before := len(w.requests)
	w.mu.Unlock()
	rec := postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "ls -la\n"}, "user-1")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var result ExecResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.ExitCode != 0 || !strings.HasPrefix(result.Output, "total 0") || len(result.ID) != 32 {
		t.Fatalf("%s", rec.Body.String())
	}
	w.mu.Lock()
	req := w.requests[before]
	w.mu.Unlock()
	if req.Operation != "exec" || req.Command != "ls -la" || req.PrincipalID != "user-1" || req.ChatID != id {
		t.Fatalf("%+v", req)
	}
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 1 {
		t.Fatalf("entries: %d", len(c.Conversation.Entries))
	}
	v := c.Conversation.Entries[0]
	if v.ID != result.ID || v.Role != "activity" || v.Text != "ls -la" || v.Tool == nil || v.Tool.Kind != "command" || v.Tool.Name != "shell" || v.Tool.Status != "completed" || !strings.HasPrefix(v.Detail, "total 0") || v.Sender == nil || v.Sender.PrincipalID != "user-1" || v.Sender.Name != "Ada" || v.Delivery != "" || v.TurnID != nil || v.EndedAt == 0 || v.IsStreaming {
		t.Fatalf("%+v", v)
	}
	// Nothing was queued for the agent: the chat stays idle.
	if c.Status == "queued" || c.Status == "running" {
		t.Fatalf("status %s", c.Status)
	}
	// A non-zero exit and a timeout show as the card's status.
	w.mu.Lock()
	w.exec = &sandbox.ExecResult{Output: "fatal: not a git repository\n", ExitCode: 128}
	w.mu.Unlock()
	rec = postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "git status"}, "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	w.exec = &sandbox.ExecResult{Output: "tick\n", ExitCode: -1, TimedOut: true}
	w.mu.Unlock()
	rec = postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "sleep 100"}, "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	entries := e.Store.Snapshot().chat(id).Conversation.Entries
	if entries[1].Tool.Status != "exit 128" || entries[1].Sender.PrincipalID != "owner" || entries[2].Tool.Status != "timed out" || entries[2].Detail != "tick\n" {
		t.Fatalf("%+v %+v", entries[1].Tool, entries[2].Tool)
	}
	// A runner refusal (a stopped sandbox) is the card's failure and the
	// route's error.
	w.mu.Lock()
	w.fail = true
	w.mu.Unlock()
	rec = postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "ls"}, "")
	w.mu.Lock()
	w.fail = false
	w.mu.Unlock()
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "unverified sandbox") {
		t.Fatalf("failure: %d %s", rec.Code, rec.Body.String())
	}
	entries = e.Store.Snapshot().chat(id).Conversation.Entries
	if last := entries[len(entries)-1]; last.Tool.Status != "failed" || !strings.Contains(last.Detail, "unverified sandbox") {
		t.Fatalf("%+v %q", last.Tool, last.Detail)
	}
	// An archived chat refuses.
	if err := e.Edit(id, "Shell", true); err != nil {
		t.Fatal(err)
	}
	if rec := postAs(t, h, "chats/"+id+"/exec", map[string]any{"text": "ls"}, ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "archived") {
		t.Fatalf("archived: %d %s", rec.Code, rec.Body.String())
	}
}

// "#note" in the composer: the memory route appends the note to the
// workspace's CLAUDE.md as a bullet through the runner and leaves a system
// line with the hint about when the agent reads it.
func TestMemoryRouteAppendsToClaudeMD(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Notes", "", "", nil, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	if rec := postAs(t, h, "chats/"+id+"/memory", map[string]any{"text": " "}, ""); rec.Code != 409 {
		t.Fatalf("empty note: %d", rec.Code)
	}
	w.mu.Lock()
	before := len(w.requests)
	w.mu.Unlock()
	rec := postAs(t, h, "chats/"+id+"/memory", map[string]any{"text": "use tabs\nnot spaces"}, "user-1")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	req := w.requests[before]
	w.mu.Unlock()
	if req.Operation != "memory-append" || string(req.Bytes) != "- use tabs\n  not spaces" || req.PrincipalID != "user-1" || req.ChatID != id {
		t.Fatalf("%+v %q", req, req.Bytes)
	}
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 1 {
		t.Fatalf("entries: %d", len(c.Conversation.Entries))
	}
	v := c.Conversation.Entries[0]
	if v.Role != "system" || !strings.HasPrefix(v.Text, "Added to CLAUDE.md: “use tabs”. The agent reads it only once the workspace's settings are loaded") || v.Sender == nil || v.Sender.PrincipalID != "user-1" {
		t.Fatalf("%+v", v)
	}
	if c.Status == "queued" || c.Status == "running" {
		t.Fatalf("status %s", c.Status)
	}
	// A runner refusal leaves no line.
	w.mu.Lock()
	w.fail = true
	w.mu.Unlock()
	rec = postAs(t, h, "chats/"+id+"/memory", map[string]any{"text": "x"}, "")
	w.mu.Lock()
	w.fail = false
	w.mu.Unlock()
	if rec.Code != 409 || len(e.Store.Snapshot().chat(id).Conversation.Entries) != 1 {
		t.Fatalf("failure: %d %s", rec.Code, rec.Body.String())
	}
	if hint := memoryHint("codex"); !strings.Contains(hint, "AGENTS.md") {
		t.Fatalf("%q", hint)
	}
}
