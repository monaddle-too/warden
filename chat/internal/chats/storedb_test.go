package chats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cv "warden/chat/internal/conversation"
)

// The first start on the database imports the whole-state JSON file of
// the install before it, row by row, and leaves the file renamed as a
// backup; the next start loads the rows, and what was imported comes
// back whole: chats in order with their entries, turns, approvals and
// permission history, the ports, the deleted sandboxes, each person's
// instructions, the workspaces' rules and the model catalog.
func TestOpenImportsTheLegacyFileOnce(t *testing.T) {
	root := t.TempDir()
	turn := "turn-1"
	legacy := State{Version: 1, Chats: []*Chat{
		{ID: "a", Provider: "claude", Model: "opus", Title: "First", SandboxID: "sb-1", Status: "idle", Mode: ModeAsk,
			Rules: []Rule{{ID: "r1", Kind: "allow", Pattern: "Bash(git *)"}},
			Conversation: cv.Conversation{ThreadID: cv.Ptr("thread-1"), Entries: []cv.Entry{
				{ID: "m1", Role: "user", Text: "hello", Delivery: "sent", CreatedAt: 10, TurnID: &turn},
				{ID: "m2", Role: "assistant", Text: "hi", CreatedAt: 11, TurnID: &turn, Tool: &cv.Tool{Kind: "bash", Input: map[string]any{"command": "ls"}}},
			}, Turns: []cv.Turn{{ID: turn, StartedAt: 10, EndedAt: 11, Usage: &cv.Usage{Input: 3, Output: 4}}}},
			Approvals:   []Approval{{ID: "ap1", RunID: "run-1", Method: "item/tool/requestUserInput", Params: map[string]any{"q": "?"}, State: "answered"}},
			Permissions: []PermissionEvent{{ID: "pe1", At: 12, Tool: "Bash", Summary: "ls", Decision: "allow", How: "auto"}},
			Reviews:     []Review{{ID: "rv1", Kind: "pull_request", Status: "pending", Title: "Fix", RequestedAt: 13}}},
		{ID: "b", Provider: "codex", Model: "gpt", Title: "Second", Status: "running", Archived: true,
			Conversation: cv.Conversation{Entries: []cv.Entry{{ID: "m3", Role: "user", Text: "go", Delivery: "sending", CreatedAt: 20}}}},
	},
		Ports:            []PortBinding{{ID: "p1", ChatID: "a", SandboxID: "sb-1", Port: 3000, Title: "app", URL: "http://x", State: "active"}},
		DeletedSandboxes: []string{"sb-old"},
		Instructions:     map[string]*Instructions{"owner": {Text: "be brief", UpdatedAt: 5, Name: "Dana"}},
		Environments:     map[string]*EnvironmentRecord{"sb-1": {Rules: []Rule{{ID: "r2", Kind: "deny", Pattern: "Bash(rm *)"}}}},
		Catalog:          map[string]*Catalog{"claude": {At: 7, Models: []ModelInfo{{Value: "opus", Label: "Opus"}}}},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, legacyFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, legacyFile)); !os.IsNotExist(err) {
		t.Fatal("the legacy file was not moved aside")
	}
	if _, err = os.Stat(filepath.Join(root, legacyFile+".migrated")); err != nil {
		t.Fatal("no backup of the legacy file")
	}
	// The restart fix-ups applied to what was imported.
	got := s.Snapshot()
	if got.Chats[1].Status != "interrupted" || got.Chats[1].Conversation.Entries[0].Delivery != "failed" {
		t.Fatalf("restart fix-ups skipped: %+v", got.Chats[1])
	}
	s.Close()
	// A start with the database present loads it and ignores a JSON file.
	if err = os.WriteFile(filepath.Join(root, legacyFile), []byte(`{"version":1,"chats":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got = s.Snapshot()
	if len(got.Chats) != 2 || got.Chats[0].ID != "a" || got.Chats[1].ID != "b" {
		t.Fatalf("chats: %+v", got.Chats)
	}
	a := got.Chats[0]
	if a.Title != "First" || a.Mode != ModeAsk || len(a.Rules) != 1 || *a.Conversation.ThreadID != "thread-1" {
		t.Fatalf("chat record: %+v", a)
	}
	if len(a.Conversation.Entries) != 2 || a.Conversation.Entries[1].Tool.Input["command"] != "ls" || *a.Conversation.Entries[0].TurnID != turn {
		t.Fatalf("entries: %+v", a.Conversation.Entries)
	}
	if len(a.Conversation.Turns) != 1 || a.Conversation.Turns[0].Usage.Output != 4 {
		t.Fatalf("turns: %+v", a.Conversation.Turns)
	}
	if len(a.Approvals) != 1 || a.Approvals[0].Params["q"] != "?" || len(a.Permissions) != 1 || a.Permissions[0].Summary != "ls" || len(a.Reviews) != 1 || a.Reviews[0].Title != "Fix" {
		t.Fatalf("lists: %+v %+v %+v", a.Approvals, a.Permissions, a.Reviews)
	}
	if len(got.Ports) != 1 || got.Ports[0].URL != "http://x" || len(got.DeletedSandboxes) != 1 || got.DeletedSandboxes[0] != "sb-old" {
		t.Fatalf("ports / deleted: %+v %+v", got.Ports, got.DeletedSandboxes)
	}
	if got.Instructions["owner"].Text != "be brief" || got.Instructions["owner"].Name != "Dana" || len(got.Environments["sb-1"].Rules) != 1 || got.Catalog["claude"].Models[0].Value != "opus" || got.Catalog["claude"].At != 7 {
		t.Fatalf("maps: %+v %+v %+v", got.Instructions, got.Environments, got.Catalog)
	}
	// The imported chat's entries are queryable rows, not one blob.
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE chat_id = 'a' AND role = 'assistant'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("entry rows: %d %v", n, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM chats WHERE archived = 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("archived column: %d %v", n, err)
	}
}

// A mutation writes the rows it changed and no others: a streamed token
// on one entry is that entry's row; a new entry is its row; a rewind is
// the rows past the new length; a title is the chat row; a change to
// another chat leaves this one's rows alone.
func TestWritesAreTheChangedRowsOnly(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.update(func(st *State) error {
		for _, id := range []string{"a", "b"} {
			st.Chats = append(st.Chats, &Chat{ID: id, Title: id, Conversation: cv.Conversation{Entries: []cv.Entry{
				{ID: id + "1", Role: "user", Text: "one"}, {ID: id + "2", Role: "assistant", Text: "two"}, {ID: id + "3", Role: "assistant", Text: "three"},
			}}, Approvals: []Approval{}})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// plan is the statements a chat's plan holds (each list that changed
	// adds one no-op statement carrying its shadow update).
	plan := func(id string) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, ch := range s.planChat(id) {
			if ch.exec != nil {
				n++
			}
		}
		return n
	}
	s.mu.Lock()
	a := s.state.chat("a")
	a.Conversation.Entries[2].Text += " more"
	s.mu.Unlock()
	// The changed entry, plus the no-op statement that carries the list's
	// shadow update.
	if got := plan("a"); got != 2 {
		t.Fatalf("a streamed token planned %d statements, want the entry row and its bookkeeping", got)
	}
	if got := plan("b"); got != 0 {
		t.Fatalf("the other chat planned %d statements", got)
	}
	if err = s.updateChat("a", func(c *Chat) error { c.Title = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	a.Conversation.Entries = a.Conversation.Entries[:1]
	s.mu.Unlock()
	if got := plan("a"); got != 2 {
		t.Fatalf("a rewind planned %d statements, want one delete past the new length and its bookkeeping", got)
	}
	if err = s.updateChat("a", func(*Chat) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE chat_id = 'a'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("entries after the rewind: %d %v", n, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE chat_id = 'b'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("the other chat's entries: %d %v", n, err)
	}
	var title string
	if err = s.db.QueryRow(`SELECT title FROM chats WHERE id = 'a'`).Scan(&title); err != nil || title != "renamed" {
		t.Fatalf("title column: %q %v", title, err)
	}
	// Deleting a chat removes its rows.
	if err = s.update(func(st *State) error { st.Chats = st.Chats[1:]; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE chat_id = 'a'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("entries of a deleted chat: %d %v", n, err)
	}
	if err = s.db.QueryRow(`SELECT position FROM chats WHERE id = 'b'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("position after the deletion: %d %v", n, err)
	}
}
