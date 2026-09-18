package chats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
)

func bugCapturer(t *testing.T, enabled bool) (*bugreport.Capturer, string) {
	t.Helper()
	state := t.TempDir()
	cfg := config.Defaults(state)
	cfg.Reporting.Enabled = enabled
	c := bugreport.New(cfg, "", bugreport.ComponentChat, "owner-secret-capability")
	c.Logf = func(string, ...any) {}
	return c, state
}

// "/bug text": the route drafts a user report with the person's text as
// the description and the chat's ids as the context (never a message),
// plus the chat log's tail, redacted; off, it answers the notice and
// writes nothing.
func TestBugRouteDraftsAUserReportWithIdsOnly(t *testing.T) {
	e, _, _ := setup(t)
	id, err := e.Create("Bugs", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Message(id, "the secret message text", strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	// A nil capturer (a service without one) reports nothing.
	rec := postAs(t, h, "chats/"+id+"/bug", map[string]any{"text": "it broke"}, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), bugreport.OffNotice) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	off, offState := bugCapturer(t, false)
	e.Bugs = off
	rec = postAs(t, h, "chats/"+id+"/bug", map[string]any{"text": "it broke"}, "")
	var result BugResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Drafted || result.Notice != bugreport.OffNotice {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(bugreport.PendingDir(offState)); err == nil {
		t.Fatal("drafted while off")
	}
	on, state := bugCapturer(t, true)
	e.Bugs = on
	os.WriteFile(filepath.Join(state, "warden-chat.log"), []byte("chat started\nowner-secret-capability seen by someone@example.com\n"), 0o600)
	if rec = postAs(t, h, "chats/"+id+"/bug", map[string]any{"text": "  "}, ""); rec.Code != 409 {
		t.Fatalf("empty text: %d %s", rec.Code, rec.Body.String())
	}
	if rec = postAs(t, h, "chats/missing/bug", map[string]any{"text": "x"}, ""); rec.Code != 409 {
		t.Fatalf("missing chat: %d", rec.Code)
	}
	rec = postAs(t, h, "chats/"+id+"/bug", map[string]any{"text": "the spinner never stops\nfor someone@example.com"}, "user-1")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || !result.Drafted || !bugreport.ValidID(result.ID) || result.Notice != bugreport.DraftedNotice {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	drafts, _ := bugreport.Pending(state)
	if len(drafts) != 1 || drafts[0].Report.ID != result.ID {
		t.Fatalf("%+v", drafts)
	}
	r := drafts[0].Report
	c := e.Store.Snapshot().chat(id)
	if r.Kind != bugreport.KindUser || r.Trigger != bugreport.TriggerUser || r.Component != bugreport.ComponentChat || r.Summary != "the spinner never stops" || r.Description != "the spinner never stops\nfor <email>" {
		t.Fatalf("%+v", r)
	}
	if r.Context == nil || r.Context.ChatID != id || r.Context.Provider != c.Provider || r.Context.RunID != c.RunID {
		t.Fatalf("%+v", r.Context)
	}
	if len(r.Logs) != 1 || r.Logs[0].Name != "warden-chat.log" || r.Logs[0].Lines[1] != "<secret> seen by <email>" {
		t.Fatalf("%+v", r.Logs)
	}
	// No chat content anywhere in the draft.
	raw, _ := os.ReadFile(drafts[0].Path)
	if strings.Contains(string(raw), "secret message text") || strings.Contains(string(raw), "Bugs") {
		t.Fatalf("chat content in the draft:\n%s", raw)
	}
	// The transcript is untouched: /bug is not a message.
	if n := len(c.Conversation.Entries); n != 1 {
		t.Fatalf("%d entries", n)
	}
}

// "/test bugreporting": the route raises the exception on a goroutine of
// the service, the recovery drafts it as a test report with the stack, and
// the answer names the draft; off, nothing is raised or written.
func TestBugTestRouteRaisesAndDraftsTheTestException(t *testing.T) {
	e, _, _ := setup(t)
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	off, offState := bugCapturer(t, false)
	e.Bugs = off
	rec := postAs(t, h, "bug-test", map[string]any{}, "")
	var result BugResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Drafted || result.Notice != bugreport.OffNotice {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(bugreport.PendingDir(offState)); err == nil {
		t.Fatal("drafted while off")
	}
	on, state := bugCapturer(t, true)
	e.Bugs = on
	rec = postAs(t, h, "bug-test", map[string]any{}, "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || !result.Drafted || result.Notice != bugreport.DraftedNotice {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	drafts, _ := bugreport.Pending(state)
	if len(drafts) != 1 || drafts[0].Report.ID != result.ID {
		t.Fatalf("%+v", drafts)
	}
	r := drafts[0].Report
	if r.Kind != bugreport.KindError || r.Trigger != bugreport.TriggerTest || r.Error == nil || r.Error.Message != TestException || r.Error.Operation != "POST /api/bug-test" || !strings.Contains(r.Error.Stack, "chats.(*Engine).BugTest") {
		t.Fatalf("%+v", r)
	}
	// The service is still up: a second call drafts a second report.
	rec = postAs(t, h, "bug-test", map[string]any{}, "")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if drafts, _ = bugreport.Pending(state); len(drafts) != 2 {
		t.Fatalf("%d drafts", len(drafts))
	}
	// GET is not it.
	if rec = getAs(t, h, "bug-test", ""); rec.Code != 404 {
		t.Fatalf("GET: %d", rec.Code)
	}
}
