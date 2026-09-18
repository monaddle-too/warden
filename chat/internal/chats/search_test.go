package chats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"warden/chat/internal/conversation"
)

// The search reaches every chat's title and transcript, archived chats
// and a subagent's nested entries and the person's own commands
// included, in the palette's order (search.ts): title hits first, then
// entries newest chat and newest entry first; case and the kind of
// whitespace do not matter; the limit caps the hits and counts the rest.
func TestSearchAcrossChats(t *testing.T) {
	e, _, _ := setup(t)
	entry := func(id, role, text, detail string, at float64) conversation.Entry {
		return conversation.Entry{ID: id, Role: role, Text: text, Detail: detail, CreatedAt: at}
	}
	err := e.Store.update(func(st *State) error {
		old := &Chat{ID: "old", Title: "Fix the build", Provider: "claude", Status: "idle"}
		old.Conversation.Entries = []conversation.Entry{
			entry("o1", "user", "Why does the\tBUILD fail?", "", 1),
			entry("o2", "activity", "make", "make: *** No rule to make target build", 5),
		}
		recent := &Chat{ID: "new", Title: "Deploy notes", Provider: "claude", Status: "idle"}
		recent.Conversation.Entries = []conversation.Entry{
			entry("n1", "user", "Write the deploy notes", "", 8),
			entry("n2", "assistant", "Notes: the Build is green", "", 10),
			entry("agent", "activity", "Agent: look (Explore)", "It is in lex.go.", 11),
			{ID: "child", Role: "activity", Text: "Grep \"tokenizer\" in .", Detail: "lex.go:12", ParentID: "agent", CreatedAt: 12, Tool: &conversation.Tool{Kind: "search", Status: "completed"}},
			{ID: "mine", Role: "activity", Text: "git status", Detail: "?? scratch.txt", Sender: &conversation.Actor{PrincipalID: "owner"}, CreatedAt: 13, Tool: &conversation.Tool{Kind: "command", Status: "completed"}},
		}
		gone := &Chat{ID: "gone", Title: "Build archive", Provider: "codex", Status: "idle", Archived: true}
		gone.Conversation.Entries = []conversation.Entry{entry("g1", "assistant", "archived build", "", 20)}
		st.Chats = append(st.Chats, old, recent, gone)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := func(r SearchResult) []string {
		var out []string
		for _, h := range r.Hits {
			if h.Field == "title" {
				out = append(out, "chat:"+h.ChatID)
			} else {
				out = append(out, h.EntryID+":"+h.Field)
			}
		}
		return out
	}
	r := e.Search("build", 0)
	if got := strings.Join(keys(r), " "); got != "chat:old chat:gone n2:text o2:detail o1:text g1:text" || r.More != 0 {
		t.Fatalf("order: %s (more %d)", got, r.More)
	}
	if h := r.Hits[0]; h.Snippet.Match != "build" || h.Snippet.Before != "Fix the " || h.Title != "Fix the build" {
		t.Fatalf("title hit: %+v", h)
	}
	if h := r.Hits[2]; h.Role != "assistant" || h.CreatedAt != 10 || h.Snippet.Match != "Build" || h.Snippet.After != " is green" || h.Snippet.Before != "Notes: the " {
		t.Fatalf("entry hit: %+v", h)
	}
	if h := r.Hits[4]; h.Snippet.Match != "BUILD" || h.Snippet.Before != "Why does the " {
		t.Fatalf("whitespace folded: %+v", h)
	}
	if h := r.Hits[5]; !h.Archived || h.Provider != "codex" {
		t.Fatalf("archived hit: %+v", h)
	}
	// A subagent's entry names its card; the person's command is theirs.
	r = e.Search("TOKENIZER", 0)
	if len(r.Hits) != 1 || r.Hits[0].EntryID != "child" || r.Hits[0].ParentID != "agent" || r.Hits[0].Field != "text" {
		t.Fatalf("nested: %+v", r.Hits)
	}
	r = e.Search("scratch", 0)
	if len(r.Hits) != 1 || r.Hits[0].EntryID != "mine" || r.Hits[0].Sender == nil || r.Hits[0].Field != "detail" {
		t.Fatalf("person's command: %+v", r.Hits)
	}
	// A message's detail (a delivery error) is not searched; a step's is.
	if r = e.Search("No rule", 0); len(r.Hits) != 1 || r.Hits[0].Field != "detail" {
		t.Fatalf("step output: %+v", r.Hits)
	}
	// Blank queries match nothing; the limit caps and counts.
	if r = e.Search(" \n", 0); len(r.Hits) != 0 || r.More != 0 {
		t.Fatalf("blank: %+v", r)
	}
	if r = e.Search("build", 2); len(r.Hits) != 2 || r.More != 4 {
		t.Fatalf("limit: %d hits, %d more", len(r.Hits), r.More)
	}
	if r = e.Search("build", 10000); len(r.Hits) != 6 {
		t.Fatalf("over the maximum: %d", len(r.Hits))
	}
	// The route.
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	req := httptest.NewRequest("GET", h.Origin+"/api/chats/search?q=build&limit=3", nil)
	req.Header.Set("Authorization", "Bearer private")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("route: %d %s", w.Code, w.Body.String())
	}
	var res SearchResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 || res.More != 3 || res.Hits[0].Field != "title" || res.Hits[2].EntryID != "n2" {
		t.Fatalf("route result: %+v", res)
	}
	req = httptest.NewRequest("GET", h.Origin+"/api/chats/search?q=", nil)
	req.Header.Set("Authorization", "Bearer private")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"hits":[],"more":0}` {
		t.Fatalf("empty query: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("GET", h.Origin+"/api/chats/search?q=build", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("unauthenticated search: %d", w.Code)
	}
}

func TestSearchSnippets(t *testing.T) {
	text := []rune("The quick brown fox\njumps over the lazy dog and keeps on running far away")
	at := indexRunes(foldRunes(string(text)), []rune("lazy"))
	if got := snippetAt(text, at, at+4, 12); got != (Snippet{Before: "…over the ", Match: "lazy", After: " dog and…"}) {
		t.Fatalf("%+v", got)
	}
	if got := snippetAt([]rune("lazy dog"), 0, 4, 40); got != (Snippet{Match: "lazy", After: " dog"}) {
		t.Fatalf("%+v", got)
	}
	if got := snippetAt([]rune("a\n\nlazy"), 3, 7, 40); got.Before != "a " {
		t.Fatalf("%+v", got)
	}
	if indexRunes([]rune("abc"), []rune("abcd")) != -1 || indexRunes([]rune("abc"), nil) != -1 || indexRunes([]rune("xabc"), []rune("abc")) != 1 {
		t.Fatal("indexRunes")
	}
	if string(foldQuery("  Café\tAU ")) != "café au" {
		t.Fatalf("foldQuery: %q", string(foldQuery("  Café\tAU ")))
	}
}
