package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

func sampleResources() Resources {
	return Resources{
		Documents:    []ResourceDocument{{ID: "1AbC", Title: "Budget 2026", Kind: "spreadsheet", URL: "https://docs.google.com/spreadsheets/d/1AbC/edit", Access: "write"}, {ID: "2DeF", Title: "Notes", Kind: "document", URL: "https://docs.google.com/document/d/2DeF/edit", Access: "read"}},
		Repositories: []ResourceRepository{{Name: "monaddle-too/warden", URL: "https://github.com/monaddle-too/warden", CloneURL: "https://github.com/monaddle-too/warden.git", Access: []string{"contents", "issues"}}},
		Previews:     []ResourcePreview{{ID: "b1", Title: "Dev server", Port: 3000, URL: "https://b1.preview.example.com/"}, {ID: "b2", Title: "", Port: 8080, URL: "http://127.0.0.1:18781/ports/b2/proxy/"}},
	}
}

// Tokens: quoted when the name has spaces; expansion matches names case-
// insensitively with the sentence's punctuation kept, quoted names as
// they are, ids as well; what is not shared stays as typed.
func TestMentionTokensAndExpansion(t *testing.T) {
	if got := MentionToken("doc", "Budget 2026"); got != `@doc:"Budget 2026"` {
		t.Fatal(got)
	}
	if got := MentionToken("repo", "monaddle-too/warden"); got != "@repo:monaddle-too/warden" {
		t.Fatal(got)
	}
	if got := MentionToken("preview", ` Say "hi" `); got != `@preview:"Say hi"` {
		t.Fatal(got)
	}
	if got := MentionToken("preview", ""); got != `@preview:""` {
		t.Fatal(got)
	}
	r := sampleResources()
	cases := []struct {
		in, want string
		n        int
	}{
		{`Look at @repo:monaddle-too/warden, then @doc:"Budget 2026" and open @preview:"Dev server".`,
			`Look at the shared repository monaddle-too/warden (clone URL https://github.com/monaddle-too/warden.git, read access: contents, issues), then the shared Google spreadsheet "Budget 2026" (document_id 1AbC, write access, https://docs.google.com/spreadsheets/d/1AbC/edit) and open the preview "Dev server" (https://b1.preview.example.com/, port 3000).`, 3},
		{`@REPO:Monaddle-Too/Warden! and @doc:notes?`, `the shared repository monaddle-too/warden (clone URL https://github.com/monaddle-too/warden.git, read access: contents, issues)! and the shared Google document "Notes" (document_id 2DeF, read access, https://docs.google.com/document/d/2DeF/edit)?`, 2},
		{`@doc:1AbC by id and @preview:b2 by id`, `the shared Google spreadsheet "Budget 2026" (document_id 1AbC, write access, https://docs.google.com/spreadsheets/d/1AbC/edit) by id and the preview "port 8080" (http://127.0.0.1:18781/ports/b2/proxy/, port 8080) by id`, 2},
		{`@repo:someone/else and @doc:"No such" and @preview: stay; so does mail@doc:x`, `@repo:someone/else and @doc:"No such" and @preview: stay; so does mail@doc:x`, 0},
		{"plain text", "plain text", 0},
	}
	for _, c := range cases {
		got, n := ExpandMentions(c.in, r)
		if got != c.want || n != c.n {
			t.Errorf("%q\n got %q (%d)\nwant %q (%d)", c.in, got, n, c.want, c.n)
		}
	}
	if HasMentions("mail@doc:x") {
		// A bare "@doc:" inside a word is still a token by the pattern;
		// it expands to nothing since nothing is named "x".
		if got, n := ExpandMentions("mail@doc:x", r); got != "mail@doc:x" || n != 0 {
			t.Fatal(got, n)
		}
	}
	if HasMentions("no tokens here") || !HasMentions("see @preview:b2") {
		t.Fatal("HasMentions")
	}
}

// The resources route lists the workspace's documents and repositories
// from the policy service and its approved previews from the state; a
// message with tokens reaches the agent expanded while the transcript
// keeps it as typed.
func TestResourcesRouteAndExpandedInput(t *testing.T) {
	e, w := residentSetup(t)
	sharing, socket := newFakeSharing(t)
	e.PolicyAddress = "unix://" + socket
	id, err := e.Create("Mentions", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sharing.results["list"] = map[string]any{"grants": []any{map[string]any{"access": "read", "expires_at": 1.7e9, "documents": []any{map[string]any{"id": "2DeF", "title": "Notes", "kind": "document", "url": "https://docs.google.com/document/d/2DeF/edit"}}}}}
	sharing.results["github_list"] = map[string]any{"repositories": []any{map[string]any{"full_name": "monaddle-too/warden", "url": "https://github.com/monaddle-too/warden", "clone_url": "https://github.com/monaddle-too/warden.git", "access": []any{"contents"}}}}
	c := e.Store.Snapshot().chat(id)
	_ = e.Store.update(func(st *State) error {
		st.Ports = append(st.Ports, PortBinding{ID: "b1", ChatID: id, SandboxID: c.SandboxID, Port: 3000, Title: "Dev server", URL: "https://b1.preview.example.com/", State: "approved"}, PortBinding{ID: "b9", ChatID: id, SandboxID: "other", Port: 1, Title: "Elsewhere", URL: "https://x/", State: "approved"}, PortBinding{ID: "b8", ChatID: id, SandboxID: c.SandboxID, Port: 2, Title: "Gone", URL: "https://y/", State: "revoked"})
		return nil
	})
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780", WebDir: t.TempDir()}
	req := httptest.NewRequest("GET", "http://localhost:18780/api/chats/"+id+"/resources", nil)
	req.Header.Set("Authorization", "Bearer owner-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var got Resources
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Documents) != 1 || got.Documents[0].Title != "Notes" || got.Documents[0].Expires != 1.7e9 || len(got.Repositories) != 1 || got.Repositories[0].CloneURL != "https://github.com/monaddle-too/warden.git" || len(got.Previews) != 1 || got.Previews[0].ID != "b1" {
		t.Fatalf("%+v", got)
	}
	for i := 0; i < 2; i++ {
		data := agent.Map(sharing.op(i)["data"])
		if data["chatID"] != id || data["sandboxID"] != c.SandboxID {
			t.Fatalf("op %d %v", i, sharing.op(i))
		}
	}
	// The message: the agent's input carries the expansion, the entry the
	// token; a message without tokens asks the policy service nothing.
	text := `Clone @repo:monaddle-too/warden and read @doc:Notes; ignore @doc:"Not shared".`
	mid := cv.ID()
	if err := e.Message(id, text, mid); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.inputs) > 0 })
	w.mu.Lock()
	inputs := append([][]any(nil), w.inputs...)
	w.mu.Unlock()
	if len(inputs) == 0 {
		t.Fatal("no turn input")
	}
	sent := agent.String(agent.Map(inputs[len(inputs)-1][0])["text"])
	if !strings.Contains(sent, "the shared repository monaddle-too/warden (clone URL https://github.com/monaddle-too/warden.git, read access: contents)") || !strings.Contains(sent, `the shared Google document "Notes" (document_id 2DeF, read access, https://docs.google.com/document/d/2DeF/edit);`) || !strings.Contains(sent, `ignore @doc:"Not shared".`) {
		t.Fatalf("agent input: %q", sent)
	}
	var entry *cv.Entry
	for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
		if v.ID == mid {
			entry = &v
		}
	}
	if entry == nil || entry.Text != text {
		t.Fatalf("transcript entry %+v", entry)
	}
	completeTurn(t, e, w, id)
	sharing.mu.Lock()
	ops := len(sharing.ops)
	sharing.mu.Unlock()
	sendAndDeliver(t, e, id, "no mentions")
	until(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.inputs) > len(inputs) })
	sharing.mu.Lock()
	after := len(sharing.ops)
	sharing.mu.Unlock()
	if after != ops {
		t.Fatal("a message without tokens asked the policy service", after-ops)
	}
	// Without sharing configured the previews are still listed.
	e.PolicyAddress = ""
	resources, err := e.Resources(context.Background(), id)
	if err != nil || len(resources.Previews) != 1 || len(resources.Documents) != 0 || len(resources.Repositories) != 0 {
		t.Fatalf("%+v %v", resources, err)
	}
}
