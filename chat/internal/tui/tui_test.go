package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeKeys(t *testing.T) {
	keys, rest := DecodeKeys([]byte("hi\r\x7f\x1b[A\x1b[3~\x1b[5~\x1b[Z\x03\x04\x1b"))
	kinds := []KeyKind{KeyRune, KeyRune, KeyEnter, KeyBackspace, KeyUp, KeyDelete, KeyPageUp, KeyShiftTab, KeyCtrlC, KeyCtrlD}
	if len(keys) != len(kinds) {
		t.Fatalf("got %d keys: %+v", len(keys), keys)
	}
	for i, k := range kinds {
		if keys[i].Kind != k {
			t.Fatalf("key %d: %v, want %v", i, keys[i].Kind, k)
		}
	}
	if string(rest) != "\x1b" {
		t.Fatalf("lone escape must wait for more input, rest %q", rest)
	}
	// Multi-byte runes arrive whole even when split across reads.
	keys, rest = DecodeKeys([]byte("é"[:1]))
	if len(keys) != 0 || len(rest) != 1 {
		t.Fatalf("partial rune: %+v %q", keys, rest)
	}
	keys, _ = DecodeKeys([]byte("é"))
	if len(keys) != 1 || keys[0].Rune != 'é' {
		t.Fatalf("rune: %+v", keys)
	}
}

func TestEditorHistoryAndCursor(t *testing.T) {
	var e Editor
	for _, r := range "hello" {
		e.Handle(Key{Kind: KeyRune, Rune: r})
	}
	e.Handle(Key{Kind: KeyLeft})
	e.Handle(Key{Kind: KeyLeft})
	e.Handle(Key{Kind: KeyRune, Rune: 'X'})
	if e.Text() != "helXlo" || e.Cursor() != 4 {
		t.Fatalf("%q %d", e.Text(), e.Cursor())
	}
	e.Handle(Key{Kind: KeyHome})
	e.Handle(Key{Kind: KeyDelete})
	e.Handle(Key{Kind: KeyEnd})
	e.Handle(Key{Kind: KeyBackspace})
	if e.Text() != "elXl" {
		t.Fatalf("%q", e.Text())
	}
	if got := e.Submit(); got != "elXl" || e.Text() != "" {
		t.Fatalf("submit %q left %q", got, e.Text())
	}
	e.Set("second")
	e.Submit()
	e.Handle(Key{Kind: KeyRune, Rune: 'd'})
	e.Handle(Key{Kind: KeyUp})
	if e.Text() != "second" {
		t.Fatalf("history up: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyUp})
	if e.Text() != "elXl" {
		t.Fatalf("history up twice: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyDown})
	e.Handle(Key{Kind: KeyDown})
	if e.Text() != "d" {
		t.Fatalf("draft restored: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyCtrlU})
	if e.Text() != "" {
		t.Fatal("ctrl-u")
	}
}

func sampleChat() *Chat {
	c := &Chat{ID: "chat1", Title: "Local preview test", Provider: "codex", Status: "running"}
	c.Conversation.Entries = []Entry{
		{ID: "u1", Role: "user", Text: "Create a counter page"},
		{ID: "a1", Role: "activity", Text: "python3 -m http.server 8000", Detail: "completed\nServing HTTP on 0.0.0.0 port 8000\nline two\n"},
		{ID: "m1", Role: "assistant", Text: "Done. \x1b[31mred\x1b[0m text and a very long word " + strings.Repeat("x", 60), IsStreaming: true},
	}
	c.Approvals = []Approval{{ID: "ap1", Method: "warden/ports/bind", State: "pending", Params: map[string]any{"port": 8000, "title": "Counter", "path": "/counter.html"}}}
	return c
}

func TestRenderTranscriptSanitizesAndWraps(t *testing.T) {
	// A reply that has not streamed a character yet shows its label with
	// the cursor (this used to index an empty slice).
	empty := &Chat{ID: "e", Provider: "claude", Status: "running", Conversation: Conversation{Entries: []Entry{{ID: "m0", Role: "assistant", IsStreaming: true}}}}
	if got := RenderTranscript(empty, 40, false); len(got) != 2 || !strings.Contains(plain(got[0]), "claude › ▍") {
		t.Fatalf("empty streaming reply: %q", got)
	}
	lines := RenderTranscript(sampleChat(), 40, false)
	joined := plain(strings.Join(lines, "\n"))
	if strings.Contains(strings.Join(lines, "\n"), "\x1b[31m") {
		t.Fatal("agent escape sequence leaked into the terminal")
	}
	if !strings.Contains(joined, "you › Create a counter page") || !strings.Contains(joined, "codex › Done.") {
		t.Fatalf("missing speakers:\n%s", joined)
	}
	if !strings.Contains(joined, "│ Serving HTTP") || !strings.Contains(joined, "│ line two") {
		t.Fatalf("command output tail missing:\n%s", joined)
	}
	for _, l := range lines {
		visible := clipLen(l)
		if visible > 40 {
			t.Fatalf("line wider than 40: %d %q", visible, l)
		}
	}
	if !strings.Contains(joined, "▍") {
		t.Fatal("streaming marker missing")
	}
}

// plain strips escape sequences so tests can match visible text.
func plain(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func clipLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			n++
		}
	}
	return n
}

func TestRenderApprovalsAndStatus(t *testing.T) {
	c := sampleChat()
	lines := RenderApprovals(c, 80)
	joined := plain(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "bind sandbox port 8000") || !strings.Contains(joined, "y = allow, n = decline") {
		t.Fatalf("port approval:\n%s", joined)
	}
	c.Approvals = append(c.Approvals, Approval{ID: "ap2", Method: "item/tool/requestUserInput", State: "pending", Params: map[string]any{"questions": []any{map[string]any{"id": "q1", "question": "Which colour?", "options": []any{map[string]any{"label": "red"}, map[string]any{"label": "blue"}}}}}})
	joined = plain(strings.Join(RenderApprovals(c, 80), "\n"))
	if !strings.Contains(joined, "Which colour?  [red | blue]") || !strings.Contains(joined, "answered after the one above") {
		t.Fatalf("question approval:\n%s", joined)
	}
	status := StatusLine(c, []Port{{ChatID: "chat1", State: "approved", Port: 8000}}, true, time.Unix(0, 0))
	if !strings.Contains(status, "Local preview test") || !strings.Contains(status, "running") || !strings.Contains(status, "previews:1") || !strings.Contains(plain(status), "2 approvals") {
		t.Fatalf("status: %q", status)
	}
}

// composed is the body compose lays out, without the final count.
func composed(app *App, width int) []string {
	body, _ := app.compose(width)
	return body
}

// paints splits what the app wrote into one string per draw (each starts
// by hiding the cursor).
func paints(out string) []string {
	parts := strings.Split(out, "\x1b[?25l")
	return parts[1:]
}

func TestFrameSplitsCommittedFromLiveTail(t *testing.T) {
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }}
	c := sampleChat()
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "chat1"
	app.live = true
	f := app.frame(80, 24)
	// The user message and the completed command are final; the streaming
	// reply and the pending approval are live.
	committed := plain(strings.Join(f.Commit, "\n"))
	live := plain(strings.Join(f.Lines, "\n"))
	if !strings.Contains(committed, "Create a counter page") || !strings.Contains(committed, "line two") || strings.Contains(committed, "Done.") {
		t.Fatalf("committed:\n%s", committed)
	}
	if !strings.Contains(live, "Done.") || !strings.Contains(live, "bind sandbox port 8000") || strings.Contains(live, "counter page") {
		t.Fatalf("live:\n%s", live)
	}
	if f.Redraw || len(f.PromptLines) != 1 || len(f.Status) == 0 {
		t.Fatalf("frame: redraw %v prompt %q status %q", f.Redraw, f.PromptLines, f.Status)
	}
	// Nothing is final without a chat: the listing is all tail.
	app.ChatID = "missing"
	f = app.frame(80, 24)
	joined := plain(strings.Join(f.Lines, "\n"))
	if len(f.Commit) != 0 || !strings.Contains(joined, "no chat selected") || !strings.Contains(joined, "Local preview test") {
		t.Fatalf("chat list when no chat selected:\n%s", joined)
	}
}

func TestEntryFinality(t *testing.T) {
	kids := map[string][]Entry{"task": {{ID: "k1", Role: "activity", Tool: &Tool{Kind: "command", Status: "running"}}}}
	running := &Chat{Status: "running"}
	idle := &Chat{Status: "idle"}
	cases := []struct {
		name  string
		e     Entry
		final bool
	}{
		{"user", Entry{Role: "user", Text: "hi", Delivery: "sent"}, true},
		{"queued", Entry{Role: "user", Text: "hi", Delivery: "queued"}, false},
		{"being handed over", Entry{Role: "user", Text: "hi", Delivery: "sending"}, false},
		{"not delivered", Entry{Role: "user", Text: "hi", Delivery: "failed"}, true},
		{"streaming", Entry{Role: "assistant", IsStreaming: true}, false},
		{"reply", Entry{Role: "assistant", Text: "ok"}, true},
		{"running tool", Entry{Role: "activity", Tool: &Tool{Kind: "command", Status: "running"}}, false},
		{"background running", Entry{Role: "activity", Tool: &Tool{Kind: "command", Status: "running", Background: true}}, false},
		{"done tool", Entry{Role: "activity", Tool: &Tool{Kind: "command", Status: "completed"}}, true},
		{"failed tool", Entry{Role: "activity", Tool: &Tool{Kind: "command", Status: "failed"}}, true},
		{"task with a running child", Entry{ID: "task", Role: "activity", Tool: &Tool{Kind: "task", Status: "completed"}}, false},
		{"task alone", Entry{ID: "other", Role: "activity", Tool: &Tool{Kind: "task", Status: "completed"}}, true},
		{"compacting", Entry{Role: "compaction", Compaction: &Compaction{Status: "running"}}, false},
		{"compacted", Entry{Role: "compaction", Compaction: &Compaction{Status: "completed"}}, true},
		{"aside running", Entry{Role: "aside", Aside: &Aside{Status: "running"}}, false},
		{"thinking done", Entry{Role: "thinking", EndedAt: 2, CreatedAt: 1}, true},
	}
	for _, tc := range cases {
		if got := entryFinal(running, tc.e, kids); got != tc.final {
			t.Errorf("%s: final %v, want %v", tc.name, got, tc.final)
		}
	}
	// "failed" is a real failure, final on an idle chat too; a message
	// being handed over is live until the agent confirms it.
	failed := Entry{ID: "u", Role: "user", Text: "hi", Delivery: "failed", Detail: "Delivery unconfirmed."}
	if !entryFinal(idle, failed, nil) {
		t.Fatal("a failed message on an idle chat is not final")
	}
	if entryFinal(idle, Entry{ID: "u", Role: "user", Text: "hi", Delivery: "sending"}, nil) {
		t.Fatal("a message being handed over is final")
	}
}

// TestPaintCommitsOnceAndRewritesTheTail feeds a sequence of chat states
// through draw and checks the byte stream: no alternate screen or mouse
// modes, a committed line written exactly once, the tail rewritten by
// moving up the rows it took and clearing below, and a structural change
// (a rewind) clearing the screen and reprinting once.
func TestPaintCommitsOnceAndRewritesTheTail(t *testing.T) {
	var out bytes.Buffer
	now := time.Unix(1_700_000_000, 0)
	app := &App{Now: func() time.Time { return now }, Output: &out, Size: func() (int, int) { return 80, 24 }}
	c := &Chat{ID: "c", Title: "Paint", Provider: "claude", Status: "running"}
	c.Conversation.Entries = []Entry{
		{ID: "u1", Role: "user", Text: "first question", Delivery: "sent"},
		{ID: "m1", Role: "assistant", Text: "streaming answer", IsStreaming: true},
	}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "c"
	app.live = true
	app.draw()
	first := out.String()
	for _, mode := range []string{"\x1b[?1049h", "\x1b[?1000h", "\x1b[?1006h", "\x1b[2J"} {
		if strings.Contains(first, mode) {
			t.Fatalf("first paint uses %q:\n%q", mode, first)
		}
	}
	if !strings.Contains(first, "first question") || !strings.Contains(first, "streaming answer") || !strings.HasSuffix(first, "\x1b[?25h") {
		t.Fatalf("first paint:\n%q", first)
	}
	if strings.Count(first, "\x1b[") > 0 && strings.Contains(first, "A\r") {
		t.Fatalf("first paint moved the cursor up:\n%q", first)
	}
	// The reply grows: the user message is not written again, the tail is
	// rewritten from its top (cursor up by the rows it took, clear below).
	out.Reset()
	c.Conversation.Entries[1].Text = "streaming answer, longer"
	app.draw()
	second := out.String()
	if strings.Contains(second, "first question") || !strings.Contains(second, "answer, longer") {
		t.Fatalf("second paint:\n%q", second)
	}
	up := app.screen.cursorLine
	if up <= 0 || !strings.HasPrefix(second, fmt.Sprintf("\x1b[?25l\x1b[%dA\r\x1b[J", up)) {
		t.Fatalf("second paint does not go up %d rows and clear:\n%q", up, second)
	}
	// The same state again writes nothing.
	out.Reset()
	app.draw()
	if out.Len() != 0 {
		t.Fatalf("an unchanged tail was written again:\n%q", out.String())
	}
	// The reply completes and a tool runs: the reply is committed (written
	// once more, for good), the tool card is live.
	out.Reset()
	c.Conversation.Entries[1].IsStreaming = false
	c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "t1", Role: "activity", Text: "go test ./...", Tool: &Tool{Kind: "command", Status: "running"}})
	app.draw()
	third := out.String()
	if strings.Count(third, "answer, longer") != 1 || !strings.Contains(third, "go test") || strings.Contains(third, "\x1b[2J") {
		t.Fatalf("third paint:\n%q", third)
	}
	if got := plain(strings.Join(app.screen.committed, "\n")); !strings.Contains(got, "answer, longer") || strings.Contains(got, "go test") {
		t.Fatalf("committed after the reply ended:\n%s", got)
	}
	// A line committed by the first paint was never written again (the
	// reply was rewritten while it streamed, then written once for good).
	if n := strings.Count(first+second+third, "first question"); n != 1 {
		t.Fatalf("a committed line was written %d times", n)
	}
	// A rewind takes the reply and the tool back: the committed prefix no
	// longer matches, so the screen is cleared and the transcript printed
	// again, once.
	out.Reset()
	c.Conversation.Entries = c.Conversation.Entries[:1]
	c.Status = "idle"
	app.draw()
	fourth := out.String()
	if strings.Count(fourth, "\x1b[2J\x1b[H") != 1 || strings.Count(fourth, "first question") != 1 || strings.Contains(fourth, "answer") {
		t.Fatalf("rewind paint:\n%q", fourth)
	}
	if strings.Contains(fourth, "\x1b[3J") {
		t.Fatal("the terminal's history was cleared")
	}
	// Ctrl+L clears and reprints too; the next paint is incremental again.
	out.Reset()
	app.handleKey(context.Background(), Key{Kind: KeyCtrlL})
	app.draw()
	if strings.Count(out.String(), "\x1b[2J") != 1 || app.redraw {
		t.Fatalf("ctrl-l:\n%q", out.String())
	}
	out.Reset()
	c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "u2", Role: "user", Text: "second question", Delivery: "sent"})
	app.draw()
	if strings.Contains(out.String(), "\x1b[2J") || strings.Count(out.String(), "second question") != 1 || strings.Contains(out.String(), "first question") {
		t.Fatalf("after ctrl-l:\n%q", out.String())
	}
	// Exit leaves the cursor on a fresh line below the tail, the cursor
	// shown, bracketed paste off, the title popped.
	out.Reset()
	app.finish()
	if !strings.HasSuffix(out.String(), "\r\n\x1b[?2004l\x1b[?25h\x1b[23;0t") {
		t.Fatalf("finish:\n%q", out.String())
	}
}

// TestPaintCommitsEarlyWhenTheTailOutgrowsTheScreen streams a long reply
// on a short terminal: the tail never exceeds the rows less one, the
// cursor never moves up more than that, and the reply's earlier
// paragraphs are written once although it is still streaming.
func TestPaintCommitsEarlyWhenTheTailOutgrowsTheScreen(t *testing.T) {
	var out bytes.Buffer
	rows := 12
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }, Output: &out, Size: func() (int, int) { return 60, rows }}
	c := &Chat{ID: "c", Title: "Long", Provider: "claude", Status: "running"}
	c.Conversation.Entries = []Entry{{ID: "u1", Role: "user", Text: "tell me a story", Delivery: "sent"}, {ID: "m1", Role: "assistant", IsStreaming: true}}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "c"
	app.live = true
	var text strings.Builder
	maxUp := 0
	var writes []string          // what each paint wrote
	committedAt := map[int]int{} // paragraph → the paint after which it was in the scrollback
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&text, "Paragraph %d of the story, with enough words to wrap at sixty columns once or twice.\n\n", i)
		c.Conversation.Entries[1].Text = text.String()
		out.Reset()
		app.draw()
		writes = append(writes, out.String())
		if app.screen.tail > rows-1 {
			t.Fatalf("paint %d: tail %d rows on a %d-row screen", i, app.screen.tail, rows)
		}
		maxUp = max(maxUp, app.screen.cursorLine)
		committed := plain(strings.Join(app.screen.committed, "\n"))
		for p := 0; p <= i; p++ {
			if _, done := committedAt[p]; !done && strings.Contains(committed, fmt.Sprintf("Paragraph %d of", p)) {
				committedAt[p] = i
			}
		}
	}
	if maxUp >= rows-1 {
		t.Fatalf("cursor moved up %d rows", maxUp)
	}
	all := strings.Join(writes, "")
	if strings.Contains(all, "\x1b[2J") {
		t.Fatalf("a streaming reply caused a reprint:\n%q", all)
	}
	// Once in the scrollback, a paragraph was never written again.
	for p := 0; p < 25; p++ {
		at, ok := committedAt[p]
		if !ok {
			t.Fatalf("paragraph %d was never committed early", p)
		}
		for i := at + 1; i < len(writes); i++ {
			if strings.Contains(writes[i], fmt.Sprintf("Paragraph %d of", p)) {
				t.Fatalf("paragraph %d (committed by paint %d) written again by paint %d", p, at, i)
			}
		}
	}
	if committedAt[0] > 8 {
		t.Fatalf("paragraph 0 was committed only at paint %d", committedAt[0])
	}
	// The reply ends: what was committed early stays, the rest is written
	// once, and nothing was cleared.
	out.Reset()
	c.Conversation.Entries[1].IsStreaming = false
	c.Status = "idle"
	app.draw()
	if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "Paragraph 0 of") || !strings.Contains(out.String(), "Paragraph 29 of") {
		t.Fatalf("end of the reply:\n%q", out.String())
	}
}

func TestMultiLineNoticesArePrintedOnce(t *testing.T) {
	var out bytes.Buffer
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }, Output: &out, Size: func() (int, int) { return 80, 24 }}
	c := &Chat{ID: "c", Title: "Notices", Provider: "claude", Status: "idle", Conversation: Conversation{Entries: []Entry{{ID: "u1", Role: "user", Text: "hi", Delivery: "sent"}}}}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "c"
	app.draw()
	out.Reset()
	app.setNotice("one line")
	app.draw()
	if !strings.Contains(plain(out.String()), "› one line") || len(app.prints) != 0 {
		t.Fatalf("one-line notice:\n%q", out.String())
	}
	app.setNotice("first\nsecond")
	if len(app.prints) != 1 {
		t.Fatal("a multi-line notice was not queued to print")
	}
	out.Reset()
	app.draw()
	printed := plain(out.String())
	if !strings.Contains(printed, "› first\r\n› second\r\n") || strings.Contains(printed, "one line") || len(app.prints) != 0 {
		t.Fatalf("printed notice:\n%q", printed)
	}
	out.Reset()
	c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "u2", Role: "user", Text: "again", Delivery: "sent"})
	app.draw()
	if strings.Contains(out.String(), "first") {
		t.Fatalf("the printed notice was written again:\n%q", out.String())
	}
}

// fakeServer is a minimal warden-chat: state, chats, messages, approvals,
// and an event stream that emits the current state whenever it changes.
type fakeServer struct {
	// tail is what an undo-rewind puts back (the rewind tests set it).
	tail    []Entry
	mu      sync.Mutex
	state   State
	calls   []string
	uploads []string // name:content, in upload order
	srv     *httptest.Server
	token   string
	notes   []string // "#" notes the memory route received
	// fastMode is whether the fake Warden allows fast mode.
	fastMode bool
	// instructions is the person's text (me/instructions); memory the
	// listing chats/{id}/memory answers; writes what memory/write received
	// as scope:path=text.
	instructions string
	memory       MemoryView
	writes       []string
	searches     []string // the query strings /search sent
	// rules is what environments|chats/{id}/rules answer (the workspace's
	// then the chats'); adds and removes edit it; events is the chat's
	// permission history.
	rules  RulesView
	events []PermissionEvent
}

func newFakeServer(t *testing.T, initial State) *fakeServer {
	f := &fakeServer{state: initial, token: "cap"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) client() *Client { return &Client{Base: f.srv.URL, Token: f.token} }

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "Warden sign-in required", 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+path)
	f.mu.Unlock()
	switch {
	case path == "state":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.state)
	case path == "events":
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		last := ""
		for i := 0; i < 50; i++ {
			f.mu.Lock()
			b, _ := json.Marshal(f.state)
			f.mu.Unlock()
			if string(b) != last {
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
				last = string(b)
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	case path == "chats" && r.Method == "POST":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		id := fmt.Sprintf("new%d", len(f.state.Chats)+1)
		f.state.Chats = append(f.state.Chats, &Chat{ID: id, Title: body["title"].(string), Provider: body["provider"].(string), Status: "idle"})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	case path == "chats/search" && r.Method == "GET":
		// A plain version of chats/search.go: titles then entries, newest
		// entry first, case-insensitive, one hit per entry.
		q := strings.ToLower(r.URL.Query().Get("q"))
		res := SearchResult{Hits: []SearchHit{}}
		f.mu.Lock()
		f.searches = append(f.searches, r.URL.RawQuery)
		for _, c := range f.state.Chats {
			if q != "" && strings.Contains(strings.ToLower(c.Title), q) {
				h := SearchHit{ChatID: c.ID, Title: c.Title, Archived: c.Archived, Provider: c.Provider, Field: "title"}
				h.Snippet.Match = q
				res.Hits = append(res.Hits, h)
			}
		}
		for _, c := range f.state.Chats {
			for i := len(c.Conversation.Entries) - 1; i >= 0 && q != ""; i-- {
				e := c.Conversation.Entries[i]
				field := ""
				if strings.Contains(strings.ToLower(e.Text), q) {
					field = "text"
				} else if e.Role == "activity" && strings.Contains(strings.ToLower(e.Detail), q) {
					field = "detail"
				}
				if field == "" {
					continue
				}
				h := SearchHit{ChatID: c.ID, Title: c.Title, Provider: c.Provider, EntryID: e.ID, Field: field, Role: e.Role, ParentID: e.ParentID, CreatedAt: e.CreatedAt, Sender: e.Sender}
				h.Snippet.Match = q
				res.Hits = append(res.Hits, h)
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(res)
	case strings.HasSuffix(path, "/paths") && r.Method == "GET":
		q := r.URL.Query().Get("q")
		var paths []string
		for _, p := range []string{"src/", "src/app.go", "src/components/", "README.md"} {
			if strings.HasPrefix(strings.ToLower(p), strings.ToLower(q)) {
				paths = append(paths, p)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"paths": paths})
	case strings.HasSuffix(path, "/attachments") && r.Method == "POST":
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "one file required", 400)
			return
		}
		data, _ := io.ReadAll(file)
		kind := "file"
		if strings.HasSuffix(header.Filename, ".png") {
			kind = "image"
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, header.Filename+":"+string(data))
		id := fmt.Sprintf("%032d", len(f.uploads))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(Attachment{ID: id, Name: header.Filename, Kind: kind, Size: int64(len(data)), Path: ".warden/attachments/" + id + ".bin"})
	case strings.Contains(path, "/rules/") && strings.HasSuffix(path, "/remove"):
		parts := strings.Split(path, "/")
		f.mu.Lock()
		drop := func(rules []Rule) []Rule {
			var out []Rule
			for _, r := range rules {
				if r.ID != parts[3] {
					out = append(out, r)
				}
			}
			return out
		}
		if parts[0] == "environments" {
			f.rules.Rules = drop(f.rules.Rules)
		} else {
			for i := range f.rules.Chats {
				f.rules.Chats[i].Rules = drop(f.rules.Chats[i].Rules)
			}
		}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/remove"):
		w.Write([]byte(`{"ok":true}`))
	case strings.Contains(path, "/queued/") && strings.HasSuffix(path, "/edit"):
		// A queued message's text replaced in place; a sent one refused.
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		parts := strings.Split(path, "/")
		f.mu.Lock()
		c := f.state.Chat(parts[1])
		for i := range c.Conversation.Entries {
			e := &c.Conversation.Entries[i]
			if e.ID != parts[3] {
				continue
			}
			if e.Delivery != "queued" {
				f.mu.Unlock()
				http.Error(w, `{"error":"that message was already sent to the agent"}`, 409)
				return
			}
			e.Text = body.Text
			f.mu.Unlock()
			json.NewEncoder(w).Encode(*e)
			return
		}
		f.mu.Unlock()
		http.Error(w, `{"error":"no such message in this chat"}`, 409)
	case strings.HasSuffix(path, "/edit"):
		var body struct {
			Title    string `json:"title"`
			Archived bool   `json:"archived"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/edit")
		f.mu.Lock()
		c := f.state.Chat(id)
		if body.Archived && c.Status == "running" {
			f.mu.Unlock()
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"stop the chat before archiving"}`))
			return
		}
		c.Title, c.Archived = body.Title, body.Archived
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasPrefix(path, "environments/") && strings.HasSuffix(path, "/delete"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "environments/"), "/delete")
		f.mu.Lock()
		for _, c := range f.state.Chats {
			if c.SandboxID == id {
				c.Archived = true
			}
		}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/message"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/message")
		f.mu.Lock()
		c := f.state.Chat(id)
		c.Status = "running"
		entry := Entry{ID: body["id"].(string), Role: "user", Text: body["text"].(string)}
		if ids, ok := body["attachments"].([]any); ok {
			for _, v := range ids {
				entry.Attachments = append(entry.Attachments, Attachment{ID: v.(string)})
			}
		}
		c.Conversation.Entries = append(c.Conversation.Entries, entry)
		f.mu.Unlock()
		// The "agent" answers shortly after, then the chat goes idle.
		go func() {
			time.Sleep(60 * time.Millisecond)
			f.mu.Lock()
			c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "reply-" + body["id"].(string), Role: "assistant", Text: "echo: " + body["text"].(string)})
			c.Status = "idle"
			f.mu.Unlock()
		}()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/rules") && r.Method == "GET":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.rules)
	case strings.HasSuffix(path, "/rules") && r.Method == "POST":
		var body struct {
			Kind, Pattern string
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body.Pattern, "(") && body.Pattern != "Read" {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"rule pattern: missing the tool name"}`))
			return
		}
		parts := strings.Split(path, "/")
		f.mu.Lock()
		rule := Rule{ID: fmt.Sprintf("r%d", len(f.rules.Rules)+1), Kind: body.Kind, Pattern: body.Pattern, Origin: "editor", By: &Actor{PrincipalID: "owner"}}
		if parts[0] == "environments" {
			f.rules.Rules = append(f.rules.Rules, rule)
		} else {
			for i := range f.rules.Chats {
				if f.rules.Chats[i].ID == parts[1] {
					f.rules.Chats[i].Rules = append(f.rules.Chats[i].Rules, rule)
				}
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(rule)
	case strings.HasSuffix(path, "/permissions") && r.Method == "GET":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"events": f.events})
	case strings.Contains(path, "/approvals/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		parts := strings.Split(path, "/")
		f.mu.Lock()
		c := f.state.Chat(parts[1])
		for i := range c.Approvals {
			if c.Approvals[i].ID == parts[3] {
				if body["allow"] == true {
					c.Approvals[i].State = "allowed"
				} else {
					c.Approvals[i].State = "declined"
				}
				c.Approvals[i].Params["answers"] = body["answers"]
				for _, k := range []string{"always", "scope", "message", "mode"} {
					if v, ok := body[k]; ok {
						c.Approvals[i].Params[k] = v
					}
				}
			}
		}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/stop"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/stop")
		f.mu.Lock()
		f.state.Chat(id).Status = "idle"
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/exec"):
		// A person's command: the card lands in the transcript with its
		// output, attributed to the owner, and the answer says how it ended.
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/exec")
		cmd := body["text"].(string)
		code := 0
		if strings.HasPrefix(cmd, "false") {
			code = 1
		}
		f.mu.Lock()
		c := f.state.Chat(id)
		entry := Entry{ID: "x" + cmd, Role: "activity", Text: cmd, Detail: "out of " + cmd + "\n", Tool: &Tool{Kind: "command", Name: "shell", Status: "completed"}}
		if code != 0 {
			entry.Tool.Status = "exit 1"
		}
		entry.Sender = &struct {
			PrincipalID string `json:"principalID"`
			Email       string `json:"email"`
			Name        string `json:"name"`
		}{PrincipalID: "owner"}
		c.Conversation.Entries = append(c.Conversation.Entries, entry)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": entry.ID, "exitCode": code, "output": entry.Detail})
	case path == "me/instructions":
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.instructions = strings.TrimSpace(body["text"].(string))
		}
		json.NewEncoder(w).Encode(Instructions{Text: f.instructions, Name: "the owner"})
	case strings.HasSuffix(path, "/memory") && r.Method == "GET":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.memory)
	case strings.HasSuffix(path, "/memory/write"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		scope, _ := body["scope"].(string)
		file, _ := body["path"].(string)
		text, _ := body["text"].(string)
		if file == "README.md" {
			http.Error(w, `{"error":"a workspace memory file is CLAUDE.md, CLAUDE.local.md, AGENTS.md, .claude/CLAUDE.md or a .md file under .claude/rules"}`, 409)
			return
		}
		f.mu.Lock()
		f.writes = append(f.writes, scope+":"+file+"="+text)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/memory"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/memory")
		f.mu.Lock()
		f.notes = append(f.notes, body["text"].(string))
		c := f.state.Chat(id)
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "note", Role: "system", Text: "Added to CLAUDE.md: “" + body["text"].(string) + "”."})
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/mode"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/mode")
		f.mu.Lock()
		c := f.state.Chat(id)
		if c.Provider != "claude" {
			f.mu.Unlock()
			http.Error(w, `{"error":"permission modes apply to Claude chats"}`, 409)
			return
		}
		c.Mode = body["mode"].(string)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/settings"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/settings")
		f.mu.Lock()
		c := f.state.Chat(id)
		if fast, ok := body["fast"].(bool); ok && fast && !f.fastMode {
			f.mu.Unlock()
			http.Error(w, `{"error":"fast mode is not enabled on this Warden (providers.claude.allowFastMode)"}`, 409)
			return
		}
		if v, ok := body["thinking"].(string); ok {
			c.Thinking = v
		}
		if v, ok := body["effort"].(string); ok {
			c.Effort = v
		}
		if v, ok := body["fast"].(bool); ok {
			c.Fast = v
		}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/agent"):
		// The chat's provider and model for the next turn.
		var body struct{ Provider, Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if c := f.state.Chat(strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/agent")); c != nil {
			c.Provider, c.Model = body.Provider, body.Model
		}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/revoke"):
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/checkpoints"):
		// Every user message but the first has a checkpoint.
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/checkpoints")
		f.mu.Lock()
		var list []Checkpoint
		for i, e := range f.state.Chat(id).Conversation.Entries {
			if e.Role == "user" && i > 0 {
				list = append(list, Checkpoint{ID: e.ID, ChatID: id, Commit: "c" + e.ID, Store: "repository", Changed: true})
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"checkpoints": list})
	case strings.HasSuffix(path, "/fork"):
		var body struct{ TurnID string }
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/fork")
		f.mu.Lock()
		c := f.state.Chat(id)
		if c.Status == "running" {
			f.mu.Unlock()
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"wait for the agent to finish, or stop it, before forking"}`))
			return
		}
		var kept []Entry
		for _, e := range c.Conversation.Entries {
			if e.ID == body.TurnID {
				break
			}
			kept = append(kept, e)
		}
		kept = append(kept, Entry{ID: "fork-marker", Role: "fork", Text: "Forked from “" + c.Title + "”", Detail: "the agent continues from a copy of its session", Fork: &Fork{ChatID: c.ID, Title: c.Title, MessageID: body.TurnID}})
		fork := &Chat{ID: c.ID + "-fork", Title: c.Title + " (fork)", Provider: c.Provider, SandboxID: c.SandboxID, Status: "idle", Conversation: Conversation{Entries: kept}}
		f.state.Chats = append(f.state.Chats, fork)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(ForkResult{ID: fork.ID, Title: fork.Title, Session: "forked"})
	case strings.HasSuffix(path, "/aside"):
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/aside")
		f.mu.Lock()
		c := f.state.Chat(id)
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "aside1", Role: "aside", Text: body.Text, Detail: "Forty-two.", Aside: &Aside{Status: "completed", CostUSD: 0.03, Input: 1200, Output: 8, DurationMS: 2600}})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(AsideResult{ID: "aside1", Text: "Forty-two.", CostUSD: 0.03})
	case strings.HasSuffix(path, "/style"):
		var body struct{ Style string }
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/style")
		f.mu.Lock()
		f.state.Chat(id).OutputStyle = body.Style
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/rewind"):
		var body struct{ TurnID, What string }
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/rewind")
		f.mu.Lock()
		c := f.state.Chat(id)
		if c.Status == "running" {
			f.mu.Unlock()
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"wait for the agent to finish, or stop it, before rewinding"}`))
			return
		}
		var kept []Entry
		for _, e := range c.Conversation.Entries {
			if e.ID == body.TurnID {
				break
			}
			kept = append(kept, e)
		}
		if body.What != "code" {
			c.Conversation.Entries = kept
		}
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "marker", Role: "rewind", Text: "Rewound to before “" + body.TurnID + "” (" + body.What + ")", Detail: "1 file restored, 0 files removed"})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(RewindResult{MessageID: body.TurnID, What: body.What, Restored: []string{"a.txt"}, Conversation: "rewound"})
	case strings.HasSuffix(path, "/withdraw"):
		// A queued message out of the queue, returned; a sent one refused.
		var body struct{ ID string }
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/withdraw")
		f.mu.Lock()
		c := f.state.Chat(id)
		for i, e := range c.Conversation.Entries {
			if e.ID != body.ID {
				continue
			}
			if e.Delivery != "queued" {
				f.mu.Unlock()
				http.Error(w, `{"error":"that message was already sent to the agent"}`, 409)
				return
			}
			c.Conversation.Entries = append(c.Conversation.Entries[:i:i], c.Conversation.Entries[i+1:]...)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(e)
			return
		}
		f.mu.Unlock()
		http.Error(w, `{"error":"no such message in this chat"}`, 409)
	case strings.HasSuffix(path, "/undo-rewind"):
		// The last rewind's removed entries back in place of the marker.
		var body struct {
			ID   string
			Code bool
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/undo-rewind")
		f.mu.Lock()
		c := f.state.Chat(id)
		if c.UndoRewind == "" || c.UndoRewind != body.ID {
			f.mu.Unlock()
			http.Error(w, `{"error":"this rewind can no longer be undone"}`, 409)
			return
		}
		var kept []Entry
		for _, e := range c.Conversation.Entries {
			if e.ID == body.ID {
				kept = append(kept, f.tail...)
				continue
			}
			kept = append(kept, e)
		}
		c.Conversation.Entries = append(kept, Entry{ID: "undone", Role: "system", Text: "Rewind undone"})
		c.UndoRewind = ""
		n := len(f.tail)
		f.tail = nil
		f.mu.Unlock()
		code := "kept"
		if body.Code {
			code = "restored"
		}
		json.NewEncoder(w).Encode(UndoResult{MessageID: "u2", What: "both", Entries: n, Session: "fresh", Code: code, Restored: []string{"a.txt"}})
	case strings.HasSuffix(path, "/send-queued"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/send-queued")
		f.mu.Lock()
		c := f.state.Chat(id)
		queued := false
		for _, e := range c.Conversation.Entries {
			queued = queued || e.Delivery == "queued"
		}
		if !queued {
			f.mu.Unlock()
			http.Error(w, `{"error":"nothing is queued"}`, 409)
			return
		}
		c.Status = "queued"
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(path, "/diff"):
		json.NewEncoder(w).Encode(WorkspaceChanges{Base: "u2", Files: []ChangedFile{{Path: "src/app.go", Added: 2, Removed: 1}, {Path: "logo.png", Binary: true}}, Diff: "diff --git a/src/app.go b/src/app.go\n--- a/src/app.go\n+++ b/src/app.go\n@@ -1,2 +1,3 @@\n-old\n+new\n+more\n context\n"})
	default:
		http.Error(w, "not found", 404)
	}
}

func TestFollowPrintsRepliesUntilIdle(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "c1", Title: "one", Provider: "codex", Status: "idle"}}})
	client := f.client()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Message(ctx, "c1", "hello there", "m1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	status, err := Follow(ctx, client, "c1", &out, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if status != "idle" || !strings.Contains(out.String(), "you: hello there") || !strings.Contains(out.String(), "codex: echo: hello there") {
		t.Fatalf("status %q output:\n%s", status, out.String())
	}
}

// Following one message (`warden chat send --wait`) prints that message's
// own turn and returns when it ends, however many messages are queued
// before or after it; the queue's other turns are not waited for.
func TestFollowOneMessageWaitsForItsOwnTurnOnly(t *testing.T) {
	t0, t1, t2 := "t0", "t1", "t2"
	chat := &Chat{ID: "c1", Title: "one", Provider: "codex", Status: "running", Conversation: Conversation{
		Entries: []Entry{
			{ID: "u0", Role: "user", Text: "earlier", Delivery: "sent", TurnID: &t0},
			{ID: "q1", Role: "user", Text: "mine", Delivery: "queued"},
			{ID: "q2", Role: "user", Text: "later", Delivery: "queued"},
		},
		Turns: []Turn{{ID: t0, StartedAt: 1}},
	}}
	f := newFakeServer(t, State{Chats: []*Chat{chat}})
	go func() {
		step := func(d time.Duration, fn func(c *Chat)) {
			time.Sleep(d)
			f.mu.Lock()
			fn(f.state.Chats[0])
			f.mu.Unlock()
		}
		// The earlier turn answers and ends; "mine" opens its turn (the
		// service moves it to the end), answers with a step and a reply,
		// then "later" opens a turn that never ends.
		step(60*time.Millisecond, func(c *Chat) {
			c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "a0", Role: "assistant", Text: "reply to earlier", TurnID: &t0})
			c.Conversation.Turns[0].EndedAt = 2
		})
		step(60*time.Millisecond, func(c *Chat) {
			c.Conversation.Entries = []Entry{c.Conversation.Entries[0], c.Conversation.Entries[3], c.Conversation.Entries[2], {ID: "q1", Role: "user", Text: "mine", Delivery: "sent", TurnID: &t1}}
			c.Conversation.Turns = append(c.Conversation.Turns, Turn{ID: t1, StartedAt: 3})
		})
		step(60*time.Millisecond, func(c *Chat) {
			c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "s1", Role: "activity", Text: "ls", Detail: "a.txt", TurnID: &t1}, Entry{ID: "a1", Role: "assistant", Text: "reply to mine", TurnID: &t1, IsStreaming: true})
		})
		step(60*time.Millisecond, func(c *Chat) {
			c.Conversation.Entries[len(c.Conversation.Entries)-1].IsStreaming = false
			c.Conversation.Turns[1].EndedAt = 4
			c.Conversation.Entries = []Entry{c.Conversation.Entries[0], c.Conversation.Entries[1], c.Conversation.Entries[3], c.Conversation.Entries[4], c.Conversation.Entries[5], {ID: "q2", Role: "user", Text: "later", Delivery: "sent", TurnID: &t2}}
			c.Conversation.Turns = append(c.Conversation.Turns, Turn{ID: t2, StartedAt: 5})
		})
		step(60*time.Millisecond, func(c *Chat) {
			c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "a2", Role: "assistant", Text: "reply to later", TurnID: &t2})
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var out bytes.Buffer
	status, err := Follow(ctx, f.client(), "c1", &out, map[string]bool{"u0": true}, "q1")
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"you: mine\n", "(queued: sends when the agent finishes)", "· ls", "codex: reply to mine"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"earlier", "later"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output carries another turn's entries (%q):\n%s", unwanted, got)
		}
	}
	if status != "idle" {
		t.Fatalf("status %q: the message's turn ended while the queue went on", status)
	}
	if s, _ := f.client().State(ctx); s.Chats[0].Status != "running" {
		t.Fatal("the fake chat should still be running the later turn")
	}
}

// A followed message held in a stopped chat's queue, or withdrawn, ends
// the wait with an error saying so; one queued behind others says how
// many are ahead.
func TestFollowOneMessageHeldOrWithdrawn(t *testing.T) {
	held := &Chat{ID: "c1", Provider: "codex", Status: "interrupted", Conversation: Conversation{Entries: []Entry{
		{ID: "q0", Role: "user", Text: "first", Delivery: "queued"},
		{ID: "q1", Role: "user", Text: "mine", Delivery: "queued"},
	}}}
	f := newFakeServer(t, State{Chats: []*Chat{held}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	status, err := Follow(ctx, f.client(), "c1", &out, nil, "q1")
	if err == nil || !strings.Contains(err.Error(), "held in the queue") || status != "interrupted" || !strings.Contains(out.String(), "(queued: 1 message(s) ahead)") {
		t.Fatalf("held: %q %v\n%s", status, err, out.String())
	}
	// The hand-over marks the message "sending" until its turn confirms
	// it: the wait goes on through it.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.state.Chats[0].Conversation.Entries = f.state.Chats[0].Conversation.Entries[1:]
	f.state.Chats[0].Conversation.Entries[0].Delivery = "sending"
	f.mu.Unlock()
	tid := "t9"
	go func() {
		time.Sleep(80 * time.Millisecond)
		f.mu.Lock()
		c := f.state.Chats[0]
		c.Conversation.Entries[0].Delivery, c.Conversation.Entries[0].TurnID = "sent", &tid
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "a9", Role: "assistant", Text: "confirmed reply", TurnID: &tid})
		c.Conversation.Turns = []Turn{{ID: tid, StartedAt: 1, EndedAt: 2}}
		c.Status = "idle"
		f.mu.Unlock()
	}()
	out.Reset()
	if status, err := Follow(ctx, f.client(), "c1", &out, nil, "q1"); err != nil || status != "idle" || !strings.Contains(out.String(), "confirmed reply") {
		t.Fatalf("sending then confirmed: %q %v\n%s", status, err, out.String())
	}
	// A message that failed for good, with the run over, ends the wait.
	f.mu.Lock()
	f.state.Chats[0].Status = "failed"
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "q3", Role: "user", Text: "mine", Delivery: "failed", Detail: "Not delivered"})
	f.mu.Unlock()
	if _, err := Follow(ctx, f.client(), "c1", &out, nil, "q3"); err == nil || !strings.Contains(err.Error(), "Not delivered") {
		t.Fatalf("failed for good: %v", err)
	}
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.state.Chats[0].Conversation.Entries = []Entry{{ID: "u0", Role: "user", Text: "earlier", Delivery: "sent"}, {ID: "q1", Role: "user", Text: "mine", Delivery: "queued"}}
	f.mu.Unlock()
	go func() {
		time.Sleep(80 * time.Millisecond)
		f.mu.Lock()
		f.state.Chats[0].Conversation.Entries = f.state.Chats[0].Conversation.Entries[:1]
		f.mu.Unlock()
	}()
	out.Reset()
	if _, err := Follow(ctx, f.client(), "c1", &out, nil, "q1"); err == nil || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("withdrawn: %v", err)
	}
	if _, err := Follow(ctx, f.client(), "c1", &out, nil, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown message: %v", err)
	}
}

func TestAppSubmitAnswersApprovalsAndSendsMessages(t *testing.T) {
	c := sampleChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s
	// "y" answers the pending port binding rather than being sent as a message.
	app.submit(ctx, "y")
	s, _ = app.Client.State(ctx)
	if s.Chats[0].Approvals[0].State != "allowed" {
		t.Fatalf("approval not allowed: %+v", s.Chats[0].Approvals)
	}
	app.state = s
	// A question approval takes typed text as the answer.
	f.mu.Lock()
	f.state.Chats[0].Approvals = append(f.state.Chats[0].Approvals, Approval{ID: "q", Method: "item/tool/requestUserInput", State: "pending", Params: map[string]any{"questions": []any{map[string]any{"id": "q1", "question": "Colour?"}}}})
	f.mu.Unlock()
	s, _ = app.Client.State(ctx)
	app.state = s
	app.submit(ctx, "blue")
	s, _ = app.Client.State(ctx)
	if got := s.Chats[0].Approvals[1]; got.State != "allowed" || fmt.Sprint(got.Params["answers"]) != "map[q1:[blue]]" {
		t.Fatalf("question answer: %+v", got)
	}
	app.state = s
	// With nothing pending, text is a message.
	app.submit(ctx, "now a message")
	time.Sleep(100 * time.Millisecond)
	s, _ = app.Client.State(ctx)
	entries := s.Chats[0].Conversation.Entries
	if entries[len(entries)-1].Text != "echo: now a message" {
		t.Fatalf("message not sent: %+v", entries[len(entries)-1])
	}
	// /new creates a chat and switches to it; /switch goes back.
	app.submit(ctx, "/new Second chat")
	if app.ChatID != "new2" || !strings.Contains(app.notice, "Second chat") {
		t.Fatalf("new: %q %q", app.ChatID, app.notice)
	}
	app.submit(ctx, "/switch 1")
	if app.ChatID != "chat1" {
		t.Fatalf("switch: %q", app.ChatID)
	}
	app.submit(ctx, "/bogus")
	if !strings.Contains(app.notice, "unknown command") {
		t.Fatalf("notice: %q", app.notice)
	}
	// A stale capability is reported, not retried forever.
	app.Client.Token = "wrong"
	app.submit(ctx, "hello")
	if !strings.Contains(app.notice, "sign-in required") || app.editor.Text() != "hello" {
		t.Fatalf("stale capability: notice %q editor %q", app.notice, app.editor.Text())
	}
}

func TestWatchReportsEachPendingApprovalOnce(t *testing.T) {
	c := sampleChat() // one pending port binding already
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		Watch(ctx, f.client(), func(ch *Chat, a Approval) {
			mu.Lock()
			got = append(got, ch.Title+": "+a.Summary())
			if len(got) == 2 {
				cancel()
			}
			mu.Unlock()
		})
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	f.mu.Lock()
	f.state.Chats[0].Approvals = append(f.state.Chats[0].Approvals, Approval{ID: "q", Method: "item/tool/requestUserInput", State: "pending", Params: map[string]any{"questions": []any{map[string]any{"id": "q1", "question": "Deploy?"}}}})
	f.mu.Unlock()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "Local preview test: bind sandbox port 8000 (Counter)" || got[1] != "Local preview test: question: Deploy?" {
		t.Fatalf("notifications: %v", got)
	}
}

func TestPopupURLAndSummaries(t *testing.T) {
	if got := PopupURL("http://127.0.0.1:18781/?launch=5#session=abc", "c1"); got != "http://127.0.0.1:18781/?launch=5&chat=c1#session=abc" {
		t.Fatal(got)
	}
	if got := PopupURL("http://127.0.0.1:18781/", "c1"); got != "http://127.0.0.1:18781/?chat=c1" {
		t.Fatal(got)
	}
	a := Approval{Method: "item/commandExecution/requestApproval", Params: map[string]any{"command": "rm -rf build"}}
	if a.Summary() != "run command: rm -rf build" {
		t.Fatal(a.Summary())
	}
	if (Approval{Method: "item/fileChange/requestApproval"}).Summary() != "change files" {
		t.Fatal("file change summary")
	}
}

func TestScrollKeysAreTheTerminals(t *testing.T) {
	// A stray mouse report (a terminal left in mouse mode) is decoded and
	// dropped rather than typed; the app asks for no mouse reports.
	keys, rest := DecodeKeys([]byte("\x1b[<64;10;5M\x1b[<65;10;5M\x1b[<0;3;4m\x10\x0e"))
	if len(rest) != 0 || len(keys) != 4 || keys[0].Kind != KeyWheelUp || keys[1].Kind != KeyWheelDown || keys[2].Kind != KeyCtrlP || keys[3].Kind != KeyCtrlN {
		t.Fatalf("keys %+v rest %q", keys, rest)
	}
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }, Output: io.Discard}
	c := sampleChat()
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "chat1"
	ctx := context.Background()
	app.editor.Set("draft")
	app.editor.Submit()
	for _, k := range []KeyKind{KeyWheelUp, KeyWheelDown, KeyPageUp, KeyPageDown} {
		app.handleKey(ctx, Key{Kind: k})
	}
	if app.editor.Text() != "" || app.notice != "" {
		t.Fatalf("scroll keys did something: %q %q", app.editor.Text(), app.notice)
	}
	// Up recalls history; Home and End move within the draft.
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.editor.Text() != "draft" {
		t.Fatalf("up: %q", app.editor.Text())
	}
	app.handleKey(ctx, Key{Kind: KeyHome})
	if app.editor.Cursor() != 0 {
		t.Fatalf("home: cursor %d", app.editor.Cursor())
	}
	app.handleKey(ctx, Key{Kind: KeyEnd})
	if app.editor.Cursor() != 5 {
		t.Fatalf("end: cursor %d", app.editor.Cursor())
	}
	if !strings.Contains(helpText, "terminal's") || strings.Contains(helpText, "PgUp") {
		t.Fatal("help still describes in-app scrolling")
	}
}

func TestMarkdownAndDiffRendering(t *testing.T) {
	text := "# Plan\n\nUse `git status` first, then **commit**.\n\n- one item\n- another *soft* item\n1. first\n2. second\n\n```sh\nls -la\n```\ntrailing"
	lines := renderMarkdown(text, 60, "codex › ", "        ")
	joined := plain(strings.Join(lines, "\n"))
	for _, want := range []string{"codex › Plan", "Use git status first, then commit.", "• one item", "• another soft item", "1. first", "2. second", "[sh]", "  ls -la", "trailing"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	styled := strings.Join(lines, "\n")
	if !strings.Contains(styled, cyan+"git status"+reset) || !strings.Contains(styled, bold+"commit"+reset) {
		t.Fatalf("inline styles missing:\n%s", styled)
	}
	if strings.Contains(joined, "```") {
		t.Fatal("fence markers leaked")
	}
	diff := "path/a.go\n--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\nsame\n"
	collapsed := renderDetail(diff, 60, false)
	expanded := renderDetail(diff, 60, true)
	if len(expanded) != 7 || len(collapsed) != 7 {
		t.Fatalf("collapsed %d expanded %d lines", len(collapsed), len(expanded))
	}
	long := strings.Repeat("line\n", 20)
	if got := renderDetail(long, 60, false); len(got) != 7 || !strings.Contains(plain(got[0]), "14 more lines (Tab to expand)") {
		t.Fatalf("collapsed long output: %d lines, first %q", len(got), plain(got[0]))
	}
	if got := renderDetail(long, 60, true); len(got) != 20 {
		t.Fatalf("expanded long output: %d lines", len(got))
	}
	joinedDiff := strings.Join(expanded, "\n")
	if !strings.Contains(joinedDiff, green+"    │ +new") || !strings.Contains(joinedDiff, red+"    │ -old") || !strings.Contains(joinedDiff, cyan+"    │ @@") {
		t.Fatalf("diff colours:\n%s", joinedDiff)
	}
}

func TestMultilineComposerAndPaste(t *testing.T) {
	keys, rest := DecodeKeys([]byte("a\x1b\rb\n\x1b[200~two\rlines\x1b[201~\t"))
	if len(rest) != 0 {
		t.Fatalf("rest %q", rest)
	}
	kinds := []KeyKind{KeyRune, KeyNewline, KeyRune, KeyNewline, KeyPaste, KeyTab}
	for i, k := range kinds {
		if keys[i].Kind != k {
			t.Fatalf("key %d %v want %v", i, keys[i].Kind, k)
		}
	}
	if keys[4].Text != "two\nlines" {
		t.Fatalf("paste text %q", keys[4].Text)
	}
	var e Editor
	for _, k := range keys[:5] {
		e.Handle(k)
	}
	lines, row, col := e.Lines()
	if strings.Join(lines, "|") != "a|b|two|lines" || row != 3 || col != 5 {
		t.Fatalf("lines %v row %d col %d", lines, row, col)
	}
	e.Handle(Key{Kind: KeyLeft})
	e.Handle(Key{Kind: KeyLeft})
	_, row, col = e.Lines()
	if row != 3 || col != 3 {
		t.Fatalf("cursor after left: %d %d", row, col)
	}
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }}
	app.editor = e
	f := app.frame(80, 24)
	if len(f.PromptLines) != 4 || f.CursorRow != 3 || f.CursorCol != 5 || !strings.HasPrefix(f.PromptLines[0], "› a") || !strings.HasPrefix(f.PromptLines[1], "  b") {
		t.Fatalf("prompt %q row %d col %d", f.PromptLines, f.CursorRow, f.CursorCol)
	}
}

// /find reaches what the rendered transcript hides: a subagent's entries
// under its collapsed card, a person's command output past the fold, and
// steps Ctrl+O hid, by expanding (Tab) or showing the steps first, then
// prints the matching lines as it does for any match.
func TestFindReachesNestedAndFoldedEntries(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Conversation.Entries[2].IsStreaming = false
	c.Conversation.Entries = append(c.Conversation.Entries,
		Entry{ID: "agent", Role: "activity", Text: "Agent: look around (Explore)", Detail: "It is in lex.go.", Tool: &Tool{Kind: "task", Name: "Agent", Status: "completed", Input: map[string]any{"subagent_type": "Explore"}}},
		Entry{ID: "grep", Role: "activity", Text: "Grep \"tokenizer\" in .", Detail: "lex.go:12: func tokenizer()\n", ParentID: "agent", Tool: &Tool{Kind: "search", Name: "Grep", Status: "completed", Query: "tokenizer"}},
		Entry{ID: "said", Role: "assistant", Text: "The tokenizer is in lex.go, under the lexer heading.", ParentID: "agent"},
		Entry{ID: "mine", Role: "activity", Text: "git status", Detail: "?? scratch-marker.txt\n" + strings.Repeat("modified: x.go\n", 20), Sender: &struct {
			PrincipalID string `json:"principalID"`
			Email       string `json:"email"`
			Name        string `json:"name"`
		}{PrincipalID: "owner"}, Tool: &Tool{Kind: "command", Name: "Bash", Status: "completed"}},
	)
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Size: func() (int, int) { return 80, 30 }}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s
	app.frame(80, 30)
	// The subagent's message, collapsed behind the card's count line.
	app.submit(ctx, "/find lexer heading")
	if !app.expanded || !strings.HasPrefix(app.notice, `1 line(s) contain "lexer heading" (output expanded)`) || !strings.Contains(app.notice, "Explore › The tokenizer is in lex.go") {
		t.Fatalf("nested find: expanded %v notice %q", app.expanded, app.notice)
	}
	// The person's command output past the fold (a command shows its last
	// lines; the marker is on its first).
	app.expanded = false
	app.submit(ctx, "/find scratch-marker")
	if !app.expanded || !strings.Contains(app.notice, "(output expanded)") || !strings.Contains(app.notice, "?? scratch-marker.txt") {
		t.Fatalf("folded find: expanded %v notice %q", app.expanded, app.notice)
	}
	// A step hidden by Ctrl+O.
	app.expanded, app.quiet = false, true
	app.submit(ctx, "/find func tokenizer")
	if app.quiet || !app.expanded || !strings.Contains(app.notice, "(steps shown, output expanded)") {
		t.Fatalf("quiet find: quiet %v expanded %v notice %q", app.quiet, app.expanded, app.notice)
	}
	// Still nothing for a term the chat does not have; nothing expands.
	app.expanded = false
	app.submit(ctx, "/find zzzz-not-there")
	if app.expanded || app.notice != `"zzzz-not-there" is not in the transcript` {
		t.Fatalf("missing: expanded %v notice %q", app.expanded, app.notice)
	}
}

// /search lists the hits of every chat as a numbered menu (Enter or
// /search N opens one); a jump opens the chat and prints the entry's
// lines — its card's for a subagent's entry, expanding the transcript.
func TestSearchAcrossChatsListsAndJumps(t *testing.T) {
	first := sampleChat()
	first.Status, first.Conversation.Entries[2].IsStreaming = "idle", false
	second := &Chat{ID: "chat2", Title: "Second chat", Provider: "claude", Status: "idle"}
	second.Conversation.Entries = []Entry{
		{ID: "u2", Role: "user", Text: "Find the tokenizer", CreatedAt: 1, Delivery: "sent"},
		{ID: "agent", Role: "activity", Text: "Agent: look (Explore)", Detail: "lex.go", CreatedAt: 2, Tool: &Tool{Kind: "task", Name: "Agent", Status: "completed", Input: map[string]any{"subagent_type": "Explore"}}},
		{ID: "child", Role: "assistant", Text: "The tokenizer-marker is in lex.go.", ParentID: "agent", CreatedAt: 3},
		{ID: "tail2", Role: "assistant", Text: "later", CreatedAt: 4},
	}
	f := newFakeServer(t, State{Chats: []*Chat{first, second}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Size: func() (int, int) { return 80, 30 }}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s
	app.frame(80, 30)
	app.submit(ctx, "/search")
	if !strings.Contains(app.notice, "/search TEXT") {
		t.Fatalf("usage: %q", app.notice)
	}
	app.submit(ctx, "/search tokenizer")
	if len(f.searches) != 1 || f.searches[0] != "q=tokenizer&limit=40" {
		t.Fatalf("request: %v", f.searches)
	}
	if app.menu == nil || app.menu.Trigger.Kind != "hit" || len(app.menu.Items) != 2 || !strings.Contains(app.notice, "2 hits for \"tokenizer\"") {
		t.Fatalf("menu: %+v notice %q", app.menu, app.notice)
	}
	if it := app.menu.Items[0]; it.Insert != "/search 1" || !it.Run || !strings.HasPrefix(it.Label, "1  Second chat") || !strings.Contains(it.Hint, "claude in a subagent") {
		t.Fatalf("first row: %+v", it)
	}
	if it := app.menu.Items[1]; !strings.Contains(it.Hint, "you ·") {
		t.Fatalf("second row: %+v", it)
	}
	// Down, then Enter: the second hit (the user's message) opens chat 2
	// and prints the message.
	app.handleKey(ctx, Key{Kind: KeyDown})
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.ChatID != "chat2" || app.menu != nil || app.expanded || !strings.HasPrefix(app.notice, "Second chat · you · ") || !strings.Contains(app.notice, "\nyou › Find the tokenizer") {
		t.Fatalf("jump: chat %s expanded %v notice %q", app.ChatID, app.expanded, app.notice)
	}
	// /search N: the subagent's message needs the transcript expanded;
	// its card is printed with the message under it.
	app.submit(ctx, "/search 1")
	if !app.expanded || !strings.Contains(app.notice, "(output expanded):\n") || !strings.Contains(app.notice, "Agent: look (Explore)") || !strings.Contains(app.notice, "Explore › The tokenizer-marker is in lex.go.") {
		t.Fatalf("nested jump: expanded %v notice %q", app.expanded, app.notice)
	}
	app.submit(ctx, "/search 9")
	if !strings.Contains(app.notice, "N from the last search") {
		t.Fatalf("out of range: %q", app.notice)
	}
	app.submit(ctx, "/search zzzz-nothing")
	if app.menu != nil || !strings.Contains(app.notice, "no chat mentions") {
		t.Fatalf("no hits: %q", app.notice)
	}
	// A title hit just opens the chat.
	app.submit(ctx, "/search preview")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.ChatID != "chat1" || !strings.Contains(app.notice, "opened Local preview test") {
		t.Fatalf("title jump: chat %s notice %q", app.ChatID, app.notice)
	}
}

// entryLines is an entry as the transcript renders it: a subagent's entry
// comes with its card; a long block is cut around the match.
func TestEntryLines(t *testing.T) {
	c := &Chat{ID: "c", Provider: "claude"}
	c.Conversation.Entries = []Entry{
		{ID: "u", Role: "user", Text: "one\ntwo"},
		{ID: "agent", Role: "activity", Text: "Agent: look", Tool: &Tool{Kind: "task", Status: "completed"}},
		{ID: "child", Role: "assistant", Text: "inside", ParentID: "agent"},
		{ID: "r", Role: "assistant", Text: strings.Repeat("filler\n\n", 30) + "needle here\n\n" + strings.Repeat("after\n\n", 10)},
	}
	if got := entryLines(c, 80, true, "u", ""); len(got) != 2 || !strings.Contains(got[0], "you › one") {
		t.Fatalf("first: %q", got)
	}
	if got := entryLines(c, 80, true, "child", ""); len(got) < 2 || !strings.Contains(got[0], "Agent: look") || !strings.Contains(strings.Join(got, "\n"), "inside") {
		t.Fatalf("nested: %q", got)
	}
	got := entryLines(c, 80, true, "r", "needle")
	if len(got) > findLimit+2 || !strings.Contains(got[0], "…") || !strings.Contains(strings.Join(got, "\n"), "needle here") || !strings.Contains(got[len(got)-1], "more lines") {
		t.Fatalf("long: %q", got)
	}
	if got := entryLines(c, 80, true, "nope", ""); got != nil {
		t.Fatalf("unknown: %q", got)
	}
}

func TestTabExpandsFindAndCopy(t *testing.T) {
	c := sampleChat()
	c.Conversation.Entries[1].Detail = strings.Repeat("output line\n", 20) + "needle here\n"
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	var copied string
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Clipboard: func(s string) error { copied = s; return nil }, Size: func() (int, int) { return 80, 30 }}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s
	before := len(composed(app, 80))
	app.handleKey(ctx, Key{Kind: KeyTab})
	after := len(composed(app, 80))
	if !app.expanded || after <= before {
		t.Fatalf("Tab did not expand: %d -> %d", before, after)
	}
	// /find prints the matching lines as they show (the terminal's own
	// search jumps to them).
	app.submit(ctx, "/find counter page")
	if !strings.Contains(app.notice, `1 line(s) contain "counter page"`) || !strings.Contains(app.notice, "you › Create a counter page") || len(app.prints) != 1 {
		t.Fatalf("find: %q", app.notice)
	}
	app.submit(ctx, "/find output line")
	if !strings.HasPrefix(app.notice, `20 line(s) contain "output line"`) || strings.Count(app.notice, "\n") != 20 {
		t.Fatalf("find many: %q", app.notice)
	}
	app.submit(ctx, "/find line")
	if !strings.HasPrefix(app.notice, `21 lines contain "line"; the first 20`) || strings.Count(app.notice, "\n") != 20 {
		t.Fatalf("find more than the limit: %q", app.notice)
	}
	app.submit(ctx, "/find nowhere-to-be-found")
	if app.notice != `"nowhere-to-be-found" is not in the transcript` {
		t.Fatalf("find none: %q", app.notice)
	}
	app.submit(ctx, "/copy")
	if !strings.HasPrefix(copied, "Done.") || !strings.Contains(app.notice, "copied") {
		t.Fatalf("copy: %q notice %q", copied, app.notice)
	}
	c.Status = "running"
	c.Conversation.Entries[0].CreatedAt = float64(time.Now().Unix() - 42)
	app.state = &State{Chats: []*Chat{c}}
	if ind := plain(RunIndicator(c, time.Now())); !strings.Contains(ind, "42s") {
		t.Fatalf("run indicator %q", ind)
	}
}

func TestDecodeEditingKeys(t *testing.T) {
	keys, rest := DecodeKeys([]byte("\x01\x05\x17\x0f\x12\x07\x1b\x7f\x1b[Z"))
	kinds := []KeyKind{KeyCtrlA, KeyCtrlE, KeyCtrlW, KeyCtrlO, KeyCtrlR, KeyCtrlG, KeyCtrlW, KeyShiftTab}
	if len(rest) != 0 || len(keys) != len(kinds) {
		t.Fatalf("keys %+v rest %q", keys, rest)
	}
	for i, k := range kinds {
		if keys[i].Kind != k {
			t.Fatalf("key %d: %v, want %v", i, keys[i].Kind, k)
		}
	}
}

func TestEditorLineEditingAndHistoryEdges(t *testing.T) {
	var e Editor
	e.Set("one two\nthree four")
	// Ctrl+W deletes the word before the cursor, never past the line start.
	e.Handle(Key{Kind: KeyCtrlW})
	if e.Text() != "one two\nthree " {
		t.Fatalf("ctrl-w: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyCtrlW})
	e.Handle(Key{Kind: KeyCtrlW})
	if e.Text() != "one two\n" {
		t.Fatalf("ctrl-w twice more: %q", e.Text())
	}
	// Ctrl+A/E and Ctrl+U/K work on the cursor's line.
	e.Set("first line\nsecond")
	e.Handle(Key{Kind: KeyCtrlA})
	if _, row, col := e.Lines(); row != 1 || col != 0 {
		t.Fatalf("ctrl-a: row %d col %d", row, col)
	}
	e.Handle(Key{Kind: KeyRight})
	e.Handle(Key{Kind: KeyRight})
	e.Handle(Key{Kind: KeyCtrlK})
	if e.Text() != "first line\nse" {
		t.Fatalf("ctrl-k: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyCtrlU})
	if e.Text() != "first line\n" {
		t.Fatalf("ctrl-u: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyCtrlE})
	// Up moves between lines and recalls history only from the first one.
	e.SetHistory([]string{"older", "newer"})
	e.Set("a\nb")
	e.Handle(Key{Kind: KeyUp})
	if _, row, _ := e.Lines(); row != 0 || e.Text() != "a\nb" {
		t.Fatalf("up within the draft: row %d text %q", row, e.Text())
	}
	e.Handle(Key{Kind: KeyUp})
	if e.Text() != "newer" {
		t.Fatalf("up from the first line recalls history: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyDown})
	if e.Text() != "a\nb" {
		t.Fatalf("down restores the draft: %q", e.Text())
	}
	e.Handle(Key{Kind: KeyDown})
	if _, row, _ := e.Lines(); row != 1 {
		t.Fatalf("down moves to the last line: row %d", row)
	}
	e.Handle(Key{Kind: KeyDown}) // at the last line with nothing newer: no change
	if e.Text() != "a\nb" {
		t.Fatalf("down past the end: %q", e.Text())
	}
	e.Remember("newer") // a repeat of the last prompt is not recorded twice
	if h := e.History(); len(h) != 2 {
		t.Fatalf("history %v", h)
	}
}

func TestTriggerAt(t *testing.T) {
	cases := []struct {
		text  string
		caret int
		kind  string
		query string
		start int
		end   int
	}{
		{"/sw", 3, "command", "sw", 0, 3},
		{"/switch 2", 9, "chat", "2", 8, 9},
		{"/switch 2", 3, "command", "sw", 0, 7},
		{"/attach ~/Doc", 13, "local", "~/Doc", 8, 13},
		{"/rename my chat", 15, "", "", 0, 0},
		{"/help\nsecond line", 8, "", "", 0, 0},
		{"see @src/comp", 13, "path", "src/comp", 4, 13},
		{"see @src/comp then", 8, "path", "src", 4, 13},
		{"mail me@example.com", 19, "", "", 0, 0},
		{"@", 1, "path", "", 0, 1},
		{"plain text", 5, "", "", 0, 0},
	}
	for _, c := range cases {
		tr, ok := triggerAt([]rune(c.text), c.caret)
		if ok != (c.kind != "") {
			t.Fatalf("%q@%d: ok %v, want kind %q", c.text, c.caret, ok, c.kind)
		}
		if ok && (tr.Kind != c.kind || tr.Query != c.query || tr.Start != c.start || tr.End != c.end) {
			t.Fatalf("%q@%d: %+v", c.text, c.caret, tr)
		}
	}
	if items := commandItems("ex", nil); len(items) != 2 || items[0].Name != "export" || items[1].Name != "expand" {
		t.Fatalf("command items: %+v", items)
	}
	if items := commandItems("", []Command{{Name: "compact", Hint: "from the chat"}}); items[len(items)-1].Name != "compact" {
		t.Fatal("chat commands are not listed after the built-ins")
	}
	if mentionFor("src/") != "@src/" || mentionFor("src/app.go") != "@src/app.go " {
		t.Fatal("mentionFor")
	}
	text, caret := replaceRange([]rune("see @src/comp then"), 4, 13, "@src/components/")
	if string(text) != "see @src/components/ then" || caret != 20 {
		t.Fatalf("replaceRange: %q %d", string(text), caret)
	}
}

func typeText(app *App, ctx context.Context, s string) {
	for _, r := range s {
		app.handleKey(ctx, Key{Kind: KeyRune, Rune: r})
	}
}

func TestSlashMenuCompletesAndRuns(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	f := newFakeServer(t, State{Chats: []*Chat{c, {ID: "chat2", Title: "Second", Provider: "claude", Status: "idle"}}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/sw")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Label != "/switch N" {
		t.Fatalf("menu after /sw: %+v", app.menu)
	}
	fr := app.frame(80, 24)
	if len(fr.Extra) != 2 || !strings.Contains(plain(fr.Extra[0]), "/switch N") || !strings.Contains(plain(fr.Extra[0]), "open chat N") {
		t.Fatalf("menu rows: %q", fr.Extra)
	}
	app.handleKey(ctx, Key{Kind: KeyTab})
	if app.editor.Text() != "/switch " || app.editor.Cursor() != 8 {
		t.Fatalf("tab completed to %q (cursor %d)", app.editor.Text(), app.editor.Cursor())
	}
	// The argument has its own list: the chats. Enter picks one and runs.
	if app.menu == nil || app.menu.Trigger.Kind != "chat" || len(app.menu.Items) != 2 {
		t.Fatalf("chat menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyDown})
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.ChatID != "chat2" || app.editor.Text() != "" {
		t.Fatalf("enter on a chat: %q draft %q", app.ChatID, app.editor.Text())
	}
	// Enter on a command without an argument runs it ("/qu" would also
	// match /queue).
	typeText(app, ctx, "/qui")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if !app.quit {
		t.Fatal("enter on /quit did not quit")
	}
	app.quit = false
	// Esc closes the menu and keeps it closed until the draft changes.
	typeText(app, ctx, "/ch")
	if app.menu == nil {
		t.Fatal("menu did not open")
	}
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.menu != nil {
		t.Fatal("esc did not close the menu")
	}
	app.handleKey(ctx, Key{Kind: KeyRune, Rune: 'a'})
	if app.menu == nil || app.menu.Items[0].Label != "/chats" {
		t.Fatalf("menu did not reopen on typing: %+v", app.menu)
	}
	// Up and Down move the selection; a full name sent with Enter runs it.
	app.editor.Clear()
	app.menu = nil
	typeText(app, ctx, "/")
	// chat2 is a Claude chat, so the agent's /compact joins the built-ins.
	if app.menu == nil || len(app.menu.Items) != len(Commands)+1 || app.menu.Items[len(Commands)].Label != "/compact [INSTRUCTIONS]" {
		t.Fatalf("all commands: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.menu.Selected != len(Commands) {
		t.Fatalf("up wraps to the last item: %d", app.menu.Selected)
	}
	// Commands the chat offers join the menu after the built-ins.
	app.ChatCommands = func(*Chat) []Command { return []Command{{Name: "compact", Hint: "from the chat"}} }
	app.editor.Clear()
	typeText(app, ctx, "/comp")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Label != "/compact" {
		t.Fatalf("chat command: %+v", app.menu)
	}
}

func TestPathCompletion(t *testing.T) {
	old := pathDebounce
	pathDebounce = 0
	defer func() { pathDebounce = old }()
	c := sampleChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	// Every keystroke asked (no debounce here); only the newest answer counts.
	awaitPaths := func() {
		t.Helper()
		for {
			select {
			case r := <-app.pathResults:
				app.applyPaths(r)
				if r.seq == app.pathSeq.Load() {
					return
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no paths answer")
			}
		}
	}
	typeText(app, ctx, "look at @sr")
	if app.menu == nil || app.menu.Trigger.Kind != "path" || app.menu.Note != "Looking up paths…" {
		t.Fatalf("path menu: %+v", app.menu)
	}
	awaitPaths()
	if len(app.menu.Items) != 3 || app.menu.Items[0].Label != "src/" || app.menu.Note != "" {
		t.Fatalf("paths: %+v", app.menu)
	}
	// A directory keeps completing; a file ends the mention with a space.
	app.handleKey(ctx, Key{Kind: KeyTab})
	if app.editor.Text() != "look at @src/" || app.menu == nil || app.menu.Trigger.Query != "src/" {
		t.Fatalf("after a directory: %q menu %+v", app.editor.Text(), app.menu)
	}
	awaitPaths()
	if len(app.menu.Items) != 3 || app.menu.Items[2].Label != "src/components/" {
		t.Fatalf("second listing: %+v", app.menu.Items)
	}
	app.handleKey(ctx, Key{Kind: KeyDown})
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.editor.Text() != "look at @src/app.go " || app.menu != nil {
		t.Fatalf("after a file: %q menu %v", app.editor.Text(), app.menu != nil)
	}
	// A stale answer (older sequence) is ignored.
	typeText(app, ctx, "@zz")
	awaitPaths()
	app.applyPaths(pathResult{seq: 1, query: "zz", paths: []string{"zzz"}})
	if app.menu == nil || len(app.menu.Items) != 0 {
		t.Fatalf("stale answer applied: %+v", app.menu)
	}
	app.applyPaths(pathResult{seq: app.pathSeq.Load(), query: "zz"})
	if app.menu.Note != "No matching paths" {
		t.Fatalf("empty answer: %+v", app.menu)
	}
	// Enter with nothing to pick sends the message as typed.
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.editor.Text() != "" {
		t.Fatalf("enter did not send: %q", app.editor.Text())
	}
	time.Sleep(20 * time.Millisecond)
	s, _ := app.Client.State(ctx)
	entries := s.Chats[0].Conversation.Entries
	if got := entries[len(entries)-1].Text; got != "look at @src/app.go @zz" {
		t.Fatalf("sent %q", got)
	}
}

func TestAttachUploadsAndSendsWithTheMessage(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Approvals = nil
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0600)
	os.WriteFile(filepath.Join(dir, "shot.png"), []byte("\x89PNG..."), 0600)
	os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0600)
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.submit(ctx, "/attach "+filepath.Join(dir, "notes.txt"))
	app.submit(ctx, "/attach "+filepath.Join(dir, "shot.png"))
	if !strings.Contains(app.notice, "attached shot.png (image, 7 B)") {
		t.Fatalf("notice %q", app.notice)
	}
	app.submit(ctx, "/attach "+filepath.Join(dir, "empty.txt"))
	if !strings.Contains(app.notice, "is empty") {
		t.Fatalf("empty file: %q", app.notice)
	}
	app.submit(ctx, "/attach "+filepath.Join(dir, "missing.txt"))
	if !strings.Contains(app.notice, "no such file") {
		t.Fatalf("missing file: %q", app.notice)
	}
	if f.uploads[0] != "notes.txt:hello" || len(f.uploads) != 2 {
		t.Fatalf("uploads %v", f.uploads)
	}
	fr := app.frame(80, 24)
	if len(fr.Extra) != 1 || !strings.Contains(plain(fr.Extra[0]), "attached: 1 notes.txt (5 B) · 2 shot.png (7 B)") {
		t.Fatalf("attachments line: %q", fr.Extra)
	}
	app.submit(ctx, "/attachments")
	if !strings.Contains(app.notice, "notes.txt") || !strings.Contains(app.notice, "image") {
		t.Fatalf("/attachments: %q", app.notice)
	}
	app.submit(ctx, "/detach 1")
	if !strings.Contains(app.notice, "dropped notes.txt") || len(app.attachments["chat1"]) != 1 {
		t.Fatalf("/detach: %q %+v", app.notice, app.attachments)
	}
	// The /attach argument completes local paths, files running on Enter.
	app.ExportDir = dir
	typeText(app, ctx, "/attach "+dir+"/sh")
	if app.menu == nil || app.menu.Trigger.Kind != "local" || len(app.menu.Items) != 1 || !app.menu.Items[0].Run {
		t.Fatalf("local menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if len(app.attachments["chat1"]) != 2 {
		t.Fatalf("enter on a local file did not attach: %+v", app.attachments)
	}
	app.submit(ctx, "here are the files")
	s, _ := app.Client.State(ctx)
	var sent Entry
	for _, e := range s.Chats[0].Conversation.Entries {
		if e.Text == "here are the files" {
			sent = e
		}
	}
	if sent.ID == "" || len(sent.Attachments) != 2 || sent.Attachments[0].ID != fmt.Sprintf("%032d", 2) {
		t.Fatalf("message attachments: %+v", sent)
	}
	if len(app.attachments["chat1"]) != 0 {
		t.Fatal("attachments not cleared after sending")
	}
	if len(app.frame(80, 24).Extra) != 0 {
		t.Fatal("attachments line still shown")
	}
}

func exportChat() *Chat {
	turn := "turn"
	c := &Chat{ID: "c1", Title: "Fix the build: part 2", Provider: "claude", Model: "opus", SandboxID: "s", Repository: "github://owner/repo", Status: "idle"}
	c.Conversation.ThreadID = ptr("t")
	c.Conversation.Turns = []Turn{{ID: "turn", StartedAt: 1_789_000_001, EndedAt: 1_789_000_065, Usage: &Usage{Input: 1200, Cached: 800, Output: 300, Total: 1500, CostUSD: 0.02}}}
	at := 1_789_000_000.0
	c.Conversation.Entries = []Entry{
		{ID: "u", Role: "user", Text: "Please fix it", TurnID: &turn, CreatedAt: at, Attachments: []Attachment{{ID: "a", Name: "shot.png", Path: ".warden/attachments/a.png", Kind: "image", Size: 2048}}},
		{ID: "s", Role: "activity", Text: "Ran tests", Detail: "ok\n", CreatedAt: at},
		{ID: "r", Role: "assistant", Text: "# Done\n\nFixed.", TurnID: &turn, CreatedAt: at},
		{ID: "m", Role: "system", Text: "Turn ended\nearly", CreatedAt: at},
		{ID: "i", Role: "image", Text: "The result", Detail: "img", CreatedAt: at},
		{ID: "f", Role: "user", Text: "Again", Delivery: "failed", Detail: "Service down", CreatedAt: at},
	}
	return c
}

func ptr(s string) *string { return &s }

func TestExportMatchesTheWeb(t *testing.T) {
	c := exportChat()
	at := time.Date(2026, 9, 17, 13, 5, 0, 0, time.UTC)
	stamp := func(s float64) string { return fmt.Sprintf("T%v", s) }
	md := ExportMarkdown(c, true, at, stamp)
	want := strings.Join([]string{
		"# Fix the build: part 2", "",
		"- Agent: Claude (opus)",
		"- Repository: github://owner/repo",
		fmt.Sprintf("- Exported: T%v", float64(at.UnixMilli())/1000),
		"", "---", "",
		"## You — T1.789e+09", "",
		"Please fix it", "",
		"Attachments: `shot.png` (2 KB, `.warden/attachments/a.png`)", "",
		"### Activity — Ran tests", "",
		"```", "ok", "```", "",
		"## Claude — T1.789e+09", "",
		"# Done", "", "Fixed.", "",
		"_Turn: 1m 05s · 1.5k tokens (1.2k in, 300 out) · $0.02_", "",
		"> Turn ended", "> early", "",
		"_Image: The result_", "",
		"## You — T1.789e+09", "",
		"_Not delivered: Service down_", "",
		"Again", "",
	}, "\n")
	if md != want {
		t.Fatalf("markdown:\n%s\nwant:\n%s", md, want)
	}
	plainMD := ExportMarkdown(c, false, at, stamp)
	if strings.Contains(plainMD, "### Activity") || !strings.HasSuffix(plainMD, "Again\n") {
		t.Fatalf("without activity:\n%s", plainMD)
	}
	if fenceFor("a\n```\nb") != "````" || fenceFor("`````x") != "``````" {
		t.Fatal("fenceFor")
	}
	thought := &Chat{Provider: "codex"}
	thought.Conversation.Entries = []Entry{{ID: "t", Role: "thinking", Text: "**Plan**\n\nRead, then test."}, {ID: "m", Role: "assistant", Text: "Done."}}
	if got := ExportMarkdown(thought, true, at, stamp); !strings.Contains(got, "### Thinking\n\n> **Plan**\n> \n> Read, then test.\n") {
		t.Fatalf("thinking:\n%s", got)
	}
	if got := ExportMarkdown(thought, false, at, stamp); strings.Contains(got, "Thinking") {
		t.Fatal("thinking exported without activity")
	}
	if LocalTime(1_789_000_000) != time.Unix(1_789_000_000, 0).Local().Format("2006-01-02 15:04") {
		t.Fatal("LocalTime")
	}
	// JSON: the chat's records under a named format, entries as the service
	// sent them (unknown fields included), tool steps left out unless asked.
	raw := `{"id":"c9","title":"Raw","provider":"codex","model":"","sandboxID":"s","status":"idle","archived":false,"approvals":[],"conversation":{"threadID":"th","entries":[{"id":"u","role":"user","text":"hi","detail":"","createdAt":1,"isStreaming":false,"delivery":"","future":{"card":true}},{"id":"a","role":"activity","text":"ls","detail":"","createdAt":2,"isStreaming":false,"delivery":""}],"turns":[{"id":"t1","startedAt":1,"endedAt":3,"usage":{"input":10,"cached":0,"output":5,"total":15}}]}}`
	var rc Chat
	if err := json.Unmarshal([]byte(raw), &rc); err != nil {
		t.Fatal(err)
	}
	out, err := ExportJSON(&rc, false, at)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Format     string           `json:"format"`
		Version    int              `json:"version"`
		ExportedAt string           `json:"exportedAt"`
		Chat       map[string]any   `json:"chat"`
		Entries    []map[string]any `json:"entries"`
		Turns      []map[string]any `json:"turns"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Format != "warden-chat" || parsed.Version != 1 || parsed.ExportedAt != "2026-09-17T13:05:00.000Z" {
		t.Fatalf("header: %+v", parsed)
	}
	if parsed.Chat["id"] != "c9" || parsed.Chat["threadID"] != "th" || parsed.Chat["model"] != "" {
		t.Fatalf("chat: %+v", parsed.Chat)
	}
	if len(parsed.Entries) != 1 || parsed.Entries[0]["id"] != "u" || parsed.Entries[0]["future"] == nil {
		t.Fatalf("entries: %+v", parsed.Entries)
	}
	if len(parsed.Turns) != 1 || parsed.Turns[0]["id"] != "t1" {
		t.Fatalf("turns: %+v", parsed.Turns)
	}
	if !strings.HasPrefix(string(out), "{\n  \"format\": \"warden-chat\",\n") {
		t.Fatalf("not indented:\n%s", out[:60])
	}
	all, _ := ExportJSON(&rc, true, at)
	json.Unmarshal(all, &parsed)
	if len(parsed.Entries) != 2 {
		t.Fatalf("all entries: %d", len(parsed.Entries))
	}
	empty := &Chat{}
	out, _ = ExportJSON(empty, true, at)
	if !strings.Contains(string(out), "\"turns\": []") || !strings.Contains(string(out), "\"entries\": []") {
		t.Fatalf("empty chat:\n%s", out)
	}
	if got := ExportName("Fix the build: part 2", "md", at); got != "fix-the-build-part-2-20260917-1305.md" {
		t.Fatal(got)
	}
	if got := ExportName("", "json", at); got != "chat-20260917-1305.json" {
		t.Fatal(got)
	}
	if got := ExportName("..Weird...Title..", "md", at); !strings.HasPrefix(got, "weird...title-") {
		t.Fatal(got)
	}
	// Formatting helpers as turns.ts.
	for _, tc := range []struct {
		got, want string
	}{
		{FormatDuration(0.8), "0.8s"}, {FormatDuration(12.4), "12s"}, {FormatDuration(65), "1m 05s"}, {FormatDuration(3720), "1h 02m"},
		{FormatTokens(842), "842"}, {FormatTokens(9500), "9.5k"}, {FormatTokens(13400), "13k"}, {FormatTokens(1_200_000), "1.2M"},
		{FormatCost(0.004), "<$0.01"}, {FormatCost(0.04), "$0.04"},
		{FormatSize(900), "900 B"}, {FormatSize(2048), "2 KB"}, {FormatSize(1_600_000), "1.5 MB"},
	} {
		if tc.got != tc.want {
			t.Fatalf("%q, want %q", tc.got, tc.want)
		}
	}
}

// The export nests a subagent's work under its card (quoted in markdown,
// `children` in JSON, fields the client does not model kept) and keeps a
// command the person ran, attributed to them, with or without the steps —
// the same file export.ts writes.
func TestExportNestsSubagentsAndKeepsPersonsCommands(t *testing.T) {
	raw := `{"id":"c9","title":"Nested","provider":"claude","model":"","sandboxID":"s","status":"idle","archived":false,"approvals":[],"conversation":{"entries":[
	{"id":"u","role":"user","text":"Look","detail":"","createdAt":1789000000,"isStreaming":false,"delivery":""},
	{"id":"agent","role":"activity","text":"Agent: look around (Explore)","detail":"It is in lex.go.","createdAt":1789000000,"isStreaming":false,"delivery":"","tool":{"kind":"task","name":"Agent","status":"completed"},"future":1},
	{"id":"grep","role":"activity","text":"Grep \"tokenizer\" in .","detail":"lex.go:12\n","createdAt":1789000000,"isStreaming":false,"delivery":"","parentID":"agent","tool":{"kind":"search","name":"Grep","status":"completed"}},
	{"id":"inner","role":"activity","text":"Agent: dig (Plan)","detail":"","createdAt":1789000000,"isStreaming":false,"delivery":"","parentID":"agent","tool":{"kind":"task","name":"Agent","status":"completed"}},
	{"id":"deep","role":"activity","text":"Read lex.go","detail":"` + "```\\nx\\n```" + `","createdAt":1789000000,"isStreaming":false,"delivery":"","parentID":"inner","tool":{"kind":"read","name":"Read","status":"completed"}},
	{"id":"said","role":"assistant","text":"Found it.","detail":"","createdAt":1789000000,"isStreaming":false,"delivery":"","parentID":"agent"},
	{"id":"r","role":"assistant","text":"It is in lex.go.","detail":"","createdAt":1789000000,"isStreaming":false,"delivery":""},
	{"id":"mine","role":"activity","text":"git status","detail":"clean\n","createdAt":1789000000,"isStreaming":false,"delivery":"","sender":{"principalID":"owner"},"tool":{"kind":"command","name":"Bash","status":"completed"}}
	]}}`
	var c Chat
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 17, 13, 5, 0, 0, time.UTC)
	stamp := func(s float64) string { return fmt.Sprintf("T%v", s) }
	md := ExportMarkdown(&c, true, at, stamp)
	want := strings.Join([]string{
		"### Activity — Agent: look around (Explore)", "",
		"> ### Activity — Grep \"tokenizer\" in .", ">",
		"> ```", "> lex.go:12", "> ```", ">",
		"> ### Activity — Agent: dig (Plan)", ">",
		"> > ### Activity — Read lex.go", "> >",
		"> > ````", "> > ```", "> > x", "> > ```", "> > ````", "> >",
		">",
		"> ## Claude — T1.789e+09", ">",
		"> Found it.", ">",
		"",
		"```", "It is in lex.go.", "```", "",
		"## Claude — T1.789e+09", "",
		"It is in lex.go.", "",
		"### Command by You — git status", "",
		"```", "clean", "```", "",
	}, "\n")
	if !strings.Contains(md, want) {
		t.Fatalf("markdown:\n%s\nwant:\n%s", md, want)
	}
	plain := ExportMarkdown(&c, false, at, stamp)
	if strings.Contains(plain, "Explore") || !strings.Contains(plain, "### Command by You — git status") {
		t.Fatalf("without steps:\n%s", plain)
	}
	if n := exportCount(exportTree(&c, true)); n != 8 {
		t.Fatalf("count all: %d", n)
	}
	if n := exportCount(exportTree(&c, false)); n != 3 {
		t.Fatalf("count messages: %d", n)
	}
	out, err := ExportJSON(&c, true, at)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Entries []struct {
			ID       string `json:"id"`
			Future   int    `json:"future"`
			Children []struct {
				ID       string `json:"id"`
				ParentID string `json:"parentID"`
				Children []struct {
					ID string `json:"id"`
				} `json:"children"`
			} `json:"children"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(parsed.Entries) != 4 || parsed.Entries[1].ID != "agent" || parsed.Entries[1].Future != 1 || len(parsed.Entries[1].Children) != 3 || parsed.Entries[1].Children[0].ParentID != "agent" || len(parsed.Entries[1].Children[1].Children) != 1 || parsed.Entries[1].Children[1].Children[0].ID != "deep" || parsed.Entries[3].ID != "mine" {
		t.Fatalf("json nesting:\n%s", out)
	}
	if !strings.Contains(string(out), "\n      \"children\": [\n") {
		t.Fatalf("not indented:\n%s", out)
	}
	// A chat built by hand (no raw records) nests the same way.
	c.Conversation.Raw = nil
	out, err = ExportJSON(&c, false, at)
	if err != nil || !strings.Contains(string(out), `"id": "mine"`) || strings.Contains(string(out), "children") {
		t.Fatalf("hand-built: %v\n%s", err, out)
	}
}

func TestExportCommandWritesTheFile(t *testing.T) {
	c := exportChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	dir := t.TempDir()
	app := &App{Client: f.client(), ChatID: "c1", Output: io.Discard, ExportDir: dir, Now: func() time.Time { return time.Date(2026, 9, 17, 13, 5, 0, 0, time.UTC) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.submit(ctx, "/export")
	name := filepath.Join(dir, "fix-the-build-part-2-20260917-1305.md")
	if !strings.Contains(app.notice, "exported 5 messages to "+name) {
		t.Fatalf("notice %q", app.notice)
	}
	md, err := os.ReadFile(name)
	if err != nil || !strings.HasPrefix(string(md), "# Fix the build: part 2\n") || strings.Contains(string(md), "### Activity") {
		t.Fatalf("file: %v\n%s", err, md)
	}
	app.submit(ctx, "/export json all transcript.json")
	raw, err := os.ReadFile(filepath.Join(dir, "transcript.json"))
	if err != nil || !strings.Contains(app.notice, "6 entries (tool steps included)") {
		t.Fatalf("json export: %v %q", err, app.notice)
	}
	var doc struct {
		Format  string           `json:"format"`
		Entries []map[string]any `json:"entries"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Format != "warden-chat" || len(doc.Entries) != 6 || doc.Entries[1]["role"] != "activity" {
		t.Fatalf("json: %s", raw)
	}
	app.submit(ctx, "/export md "+filepath.Join(dir, "sub", "x.md"))
	if !strings.Contains(app.notice, "no such file") {
		t.Fatalf("unwritable path: %q", app.notice)
	}
}

func TestLifecycleCommands(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.SandboxID = "sb1"
	sibling := &Chat{ID: "chat2", Title: "Sibling", Provider: "codex", Status: "idle", SandboxID: "sb1"}
	f := newFakeServer(t, State{Chats: []*Chat{c, sibling}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.submit(ctx, "/rename")
	if app.notice != "/rename TITLE" {
		t.Fatalf("rename without a title: %q", app.notice)
	}
	app.submit(ctx, "/rename Counter work")
	if app.chat().Title != "Counter work" || app.notice != "renamed to Counter work" {
		t.Fatalf("rename: %q %q", app.chat().Title, app.notice)
	}
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/archive")
	if app.notice != "stop the chat before archiving" {
		t.Fatalf("archive while running: %q", app.notice)
	}
	f.mu.Lock()
	f.state.Chats[0].Status = "idle"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/archive")
	if !app.chat().Archived || !strings.Contains(app.notice, "archived") || len(app.sortedChats()) != 1 {
		t.Fatalf("archive: %v %q", app.chat().Archived, app.notice)
	}
	app.submit(ctx, "/restore")
	if app.chat().Archived || app.notice != "restored" {
		t.Fatalf("restore: %v %q", app.chat().Archived, app.notice)
	}
	// Delete asks first; anything but y cancels.
	app.submit(ctx, "/delete")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, `Delete the workspace of "Counter work" and its 1 other chat(s)?`) {
		t.Fatalf("confirm: %+v", app.confirm)
	}
	fr := app.frame(100, 24)
	if len(fr.Extra) == 0 || !strings.Contains(plain(fr.Extra[0]), "Delete the workspace") {
		t.Fatalf("confirmation not shown: %q", fr.Extra)
	}
	app.submit(ctx, "no way")
	if app.confirm != nil || app.notice != "cancelled" || app.chat().Archived {
		t.Fatalf("cancel: %+v %q", app.confirm, app.notice)
	}
	app.submit(ctx, "/delete")
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.confirm != nil || app.notice != "cancelled" {
		t.Fatal("esc did not cancel")
	}
	app.submit(ctx, "/delete")
	app.submit(ctx, "y")
	if app.confirm != nil || app.notice != "workspace deleted; its chats are archived" {
		t.Fatalf("delete: %+v %q", app.confirm, app.notice)
	}
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	archived := f.state.Chats[0].Archived && f.state.Chats[1].Archived
	f.mu.Unlock()
	if !strings.Contains(calls, "POST environments/sb1/delete") || !archived {
		t.Fatalf("delete route: archived %v calls:\n%s", archived, calls)
	}
	if strings.Count(calls, "environments/sb1/delete") != 1 {
		t.Fatal("cancelled deletes reached the server")
	}
}

func TestKeyboardSet(t *testing.T) {
	c := sampleChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	now := time.Unix(1000, 0)
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return now }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	// Ctrl+C clears the draft; a second within two seconds quits.
	typeText(app, ctx, "half a thought")
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if app.editor.Text() != "" || app.quit || !strings.Contains(app.notice, "draft cleared") {
		t.Fatalf("ctrl-c: %q quit %v notice %q", app.editor.Text(), app.quit, app.notice)
	}
	now = now.Add(3 * time.Second)
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if app.quit {
		t.Fatal("a late second ctrl-c quit")
	}
	now = now.Add(time.Second)
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if !app.quit {
		t.Fatal("ctrl-c twice did not quit")
	}
	app.quit = false
	// Ctrl+D quits only on an empty draft; with text it deletes forward.
	typeText(app, ctx, "ab")
	app.handleKey(ctx, Key{Kind: KeyLeft})
	app.handleKey(ctx, Key{Kind: KeyCtrlD})
	if app.editor.Text() != "a" || app.quit {
		t.Fatalf("ctrl-d with text: %q quit %v", app.editor.Text(), app.quit)
	}
	app.editor.Clear()
	app.handleKey(ctx, Key{Kind: KeyCtrlD})
	if !app.quit {
		t.Fatal("ctrl-d on an empty draft did not quit")
	}
	app.quit = false
	// Esc interrupts a running turn through the stop route.
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.notice != "interrupting the agent" {
		t.Fatalf("esc: %q", app.notice)
	}
	f.mu.Lock()
	stopped := strings.Contains(strings.Join(f.calls, "\n"), "POST chats/chat1/stop")
	f.mu.Unlock()
	if !stopped {
		t.Fatal("esc did not call the stop route")
	}
	// Ctrl+O hides tool steps and thinking, and says so in the status.
	c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: "th", Role: "thinking", Text: "hmm"})
	app.state = &State{Chats: []*Chat{c}}
	before := plain(strings.Join(composed(app, 80), "\n"))
	app.handleKey(ctx, Key{Kind: KeyCtrlO})
	after := plain(strings.Join(composed(app, 80), "\n"))
	if !app.quiet || !strings.Contains(before, "http.server") || strings.Contains(after, "http.server") || strings.Contains(after, "Thought") || !strings.Contains(after, "Create a counter page") {
		t.Fatalf("ctrl-o:\n%s", after)
	}
	if !strings.Contains(plain(strings.Join(app.frame(80, 24).Status, "\n")), "steps hidden") {
		t.Fatal("status lacks the hidden marker")
	}
	app.submit(ctx, "/verbose")
	if app.quiet {
		t.Fatal("/verbose did not toggle back")
	}
	// Ctrl+L clears the screen and prints the transcript again.
	var out bytes.Buffer
	app.Output = &out
	app.draw()
	out.Reset()
	app.handleKey(ctx, Key{Kind: KeyCtrlL})
	app.draw()
	if !strings.Contains(out.String(), "\x1b[2J") || app.redraw || !strings.Contains(out.String(), "Create a counter page") {
		t.Fatalf("ctrl-l did not clear the screen and reprint:\n%q", out.String())
	}
	out.Reset()
	app.draw()
	if strings.Contains(out.String(), "\x1b[2J") {
		t.Fatal("every draw clears the screen")
	}
	// Ctrl+R searches the history; Enter takes the match into the composer.
	app.editor.SetHistory([]string{"git status", "run the tests", "git push"})
	app.handleKey(ctx, Key{Kind: KeyCtrlR})
	typeText(app, ctx, "git")
	fr := app.frame(80, 24)
	if app.search == nil || app.search.index != 2 || !strings.Contains(fr.PromptLines[0], "(reverse-i-search)'git': git push") {
		t.Fatalf("search: %+v prompt %q", app.search, fr.PromptLines)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlR})
	if app.search.index != 0 {
		t.Fatalf("ctrl-r again: %d", app.search.index)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlR}) // nothing older: stays
	if app.search.index != 0 {
		t.Fatalf("ctrl-r past the oldest: %d", app.search.index)
	}
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.search != nil || app.editor.Text() != "git status" {
		t.Fatalf("accept: %q", app.editor.Text())
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlR})
	typeText(app, ctx, "zzz")
	if app.search.index != -1 || !strings.Contains(plain(app.frame(80, 24).PromptLines[0]), "no match") {
		t.Fatal("no match not shown")
	}
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.search != nil || app.editor.Text() != "git status" {
		t.Fatalf("esc restores the draft: %q", app.editor.Text())
	}
	// Up with an empty draft recalls the last prompt.
	app.editor.Clear()
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.editor.Text() != "git push" {
		t.Fatalf("up: %q", app.editor.Text())
	}
}

func TestHistoryPersistsPerChat(t *testing.T) {
	dir := t.TempDir()
	c := sampleChat()
	c.Status = "idle"
	c.Approvals = nil
	f := newFakeServer(t, State{Chats: []*Chat{c, {ID: "chat2", Title: "Second", Provider: "codex", Status: "idle"}}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, HistoryDir: dir, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.loadHistory()
	for _, text := range []string{"first prompt", "second\nline", "second\nline", "/chats"} {
		app.editor.Set(text)
		app.send(ctx)
	}
	if got := LoadHistory(dir, "chat1"); strings.Join(got, "|") != "first prompt|second\nline|/chats" {
		t.Fatalf("history file: %q", got)
	}
	app.submit(ctx, "/switch 2")
	if app.ChatID != "chat2" || len(app.editor.History()) != 0 {
		t.Fatalf("switch: %q history %v", app.ChatID, app.editor.History())
	}
	app.editor.Set("other chat")
	app.send(ctx)
	app.submit(ctx, "/switch 1")
	if got := app.editor.History(); strings.Join(got, "|") != "first prompt|second\nline|/chats" {
		t.Fatalf("history back on chat1: %q", got)
	}
	// A fresh client reads the same file.
	again := &App{Client: f.client(), ChatID: "chat1", HistoryDir: dir, Output: io.Discard}
	again.state = app.state
	again.loadHistory()
	again.handleKey(ctx, Key{Kind: KeyUp})
	if again.editor.Text() != "/chats" {
		t.Fatalf("reloaded history: %q", again.editor.Text())
	}
	if got := LoadHistory(dir, "chat2"); len(got) != 1 || got[0] != "other chat" {
		t.Fatalf("chat2 history: %q", got)
	}
	// The file stays bounded.
	for i := 0; i < 2*historyCap+5; i++ {
		AppendHistory(dir, "big", fmt.Sprint("p", i))
	}
	got := LoadHistory(dir, "big")
	if len(got) > historyCap || got[len(got)-1] != fmt.Sprint("p", 2*historyCap+4) {
		t.Fatalf("cap: %d entries, last %q", len(got), got[len(got)-1])
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "big"))
	if n := strings.Count(string(raw), "\n"); n > 2*historyCap {
		t.Fatalf("file not compacted: %d lines", n)
	}
	if SearchHistory([]string{"Git status", "tests"}, "git", 2) != 0 || SearchHistory([]string{"a"}, "zz", 1) != -1 {
		t.Fatal("SearchHistory")
	}
	// Without a directory the history is kept per chat in memory.
	mem := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard}
	mem.state = app.state
	mem.loadHistory()
	mem.editor.Set("in memory")
	mem.send(ctx)
	mem.submit(ctx, "/switch 2")
	mem.submit(ctx, "/switch 1")
	if h := mem.editor.History(); len(h) != 1 || h[0] != "in memory" {
		t.Fatalf("in-memory history: %v", h)
	}
}

func TestStatusLineShowsTheTurn(t *testing.T) {
	c := sampleChat()
	c.Approvals = nil
	c.Model = "opus"
	turn := "t1"
	c.Conversation.Entries[0].TurnID = &turn
	c.Conversation.Entries[0].CreatedAt = 1000
	c.Conversation.Entries[2].TurnID = &turn
	c.Conversation.ActiveTurnID = &turn
	c.Conversation.Turns = []Turn{{ID: "t1", StartedAt: 1001, Usage: &Usage{Input: 9500, Output: 2100, Total: 11600, CostUSD: 0.04}}}
	now := time.Unix(1042, 0)
	status := plain(StatusLine(c, nil, true, now))
	for _, want := range []string{"Local preview test", "codex · opus", "running", "42s", "12k tokens (9.5k in, 2.1k out) · $0.04"} {
		if !strings.Contains(status, want) {
			t.Fatalf("running status lacks %q: %q", want, status)
		}
	}
	if strings.Contains(status, "approval") {
		t.Fatalf("approvals shown with none pending: %q", status)
	}
	c.Status = "idle"
	c.Conversation.ActiveTurnID = nil
	c.Conversation.Turns[0].EndedAt = 1065
	c.Conversation.Entries[2].IsStreaming = false
	status = plain(StatusLine(c, nil, false, now))
	if !strings.Contains(status, "○") || !strings.Contains(status, "idle") || !strings.Contains(status, "1m 05s · 12k tokens (9.5k in, 2.1k out) · $0.04") || strings.Contains(status, "⠋") {
		t.Fatalf("idle status: %q", status)
	}
	c.Approvals = []Approval{{ID: "1", State: "pending", Method: "warden/ports/bind", Params: map[string]any{}}}
	if status = plain(StatusLine(c, nil, true, now)); !strings.Contains(status, "⚠ 1 approval ") {
		t.Fatalf("one approval: %q", status)
	}
	// A chat without turn records shows no stats and times the run from
	// the last message.
	old := &Chat{ID: "o", Title: "Old", Provider: "claude", Status: "running"}
	old.Conversation.Entries = []Entry{{Role: "user", Text: "go", CreatedAt: 1030}}
	status = plain(StatusLine(old, nil, true, now))
	if !strings.Contains(status, "12s") || strings.Contains(status, "tokens") || !strings.Contains(status, "claude · default") {
		t.Fatalf("old chat: %q", status)
	}
	old.Startup = &struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	}{Stage: "creating", Detail: "pulling image"}
	if status = plain(StatusLine(old, nil, true, now)); !strings.Contains(status, "creating: pulling image") {
		t.Fatalf("startup stage: %q", status)
	}
	// Once the turn runs, the status says what the agent is doing
	// (activity.go), in place of "running".
	old.Startup = nil
	old.Conversation.Entries = append(old.Conversation.Entries, Entry{ID: "e1", Role: "activity", Text: "Edit engine.go", IsStreaming: true, Tool: &Tool{Kind: "edit", Name: "Edit", Status: "running", Paths: []string{"/home/agent/workspace/engine.go"}}})
	if status = plain(StatusLine(old, nil, true, now)); !strings.Contains(status, "12s Editing engine.go") || strings.Contains(status, "running") {
		t.Fatalf("activity status: %q", status)
	}
	old.Conversation.Entries[1].IsStreaming = false
	old.Conversation.Entries[1].Tool.Status = "completed"
	if status = plain(StatusLine(old, nil, true, now)); !strings.Contains(status, "12s running") {
		t.Fatalf("nothing running: %q", status)
	}
}

func TestReadKeysReportsALoneEscape(t *testing.T) {
	pr, pw := io.Pipe()
	app := &App{Input: pr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keys := make(chan Key, 8)
	go app.readKeys(ctx, keys)
	pw.Write([]byte("\x1b"))
	select {
	case k := <-keys:
		if k.Kind != KeyEscape {
			t.Fatalf("got %v", k.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("lone escape never reported")
	}
	pw.Write([]byte("\x1b[A"))
	select {
	case k := <-keys:
		if k.Kind != KeyUp {
			t.Fatalf("got %v", k.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("arrow never reported")
	}
	// A sequence split across reads is still one key.
	pw.Write([]byte("\x1b"))
	time.Sleep(10 * time.Millisecond)
	pw.Write([]byte("[B"))
	select {
	case k := <-keys:
		if k.Kind != KeyDown {
			t.Fatalf("split sequence: %v", k.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("split sequence never reported")
	}
	pw.Close()
	select {
	case k := <-keys:
		if k.Kind != KeyEOF {
			t.Fatalf("eof: %v", k.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("eof never reported")
	}
}

// Typed tool entries render by kind: a command as `$ command` with its
// description and its output's last lines, a file change with its counts
// and the diff's lines coloured (the git header left out), a read or
// search as one line with what it returned until expanded, a failure with
// its message; long bodies fold with the Tab hint.
func TestRenderToolEntries(t *testing.T) {
	c := &Chat{ID: "c", Title: "t", Provider: "claude"}
	long := ""
	for i := 1; i <= 20; i++ {
		long += fmt.Sprintf("line %d\n", i)
	}
	c.Conversation.Entries = []Entry{
		{ID: "b", Role: "activity", Text: "ls -la", Detail: long, Tool: &Tool{Kind: "command", Name: "Bash", Status: "completed", Description: "List files"}},
		{ID: "f", Role: "activity", Text: "false", Detail: "Exit code 1", Tool: &Tool{Kind: "command", Name: "Bash", Status: "failed"}},
		{ID: "e", Role: "activity", Text: "Edit notes.txt", Detail: "notes.txt\ndiff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n@@ -1,3 +1,3 @@\n alpha\n-gamma\n+GAMMA\n+GAMMA2\n", Tool: &Tool{Kind: "edit", Name: "Edit", Status: "completed", Paths: []string{"notes.txt"}}},
		{ID: "r", Role: "activity", Text: "Read notes.txt", Detail: "1\talpha\n2\tbeta\n", Tool: &Tool{Kind: "read", Name: "Read", Status: "completed", Paths: []string{"notes.txt"}}},
		{ID: "g", Role: "activity", Text: "Grep 'x' in .", Detail: "Found 3 files\na\nb\nc\n", Tool: &Tool{Kind: "search", Name: "Grep", Status: "completed", Query: "x"}},
		{ID: "n", Role: "activity", Text: "Read missing.txt", Detail: "File does not exist.", Tool: &Tool{Kind: "read", Name: "Read", Status: "failed"}},
		{ID: "m", Role: "activity", Text: "warden · preview_attach", Detail: "https://p", Tool: &Tool{Kind: "mcp", Name: "preview_attach", Server: "warden", Status: "completed", Input: map[string]any{"port": 3000, "title": "Preview"}}},
		{ID: "s", Role: "activity", Text: "sleep 5", Detail: "", IsStreaming: true, Tool: &Tool{Kind: "command", Name: "Bash", Status: "running"}},
	}
	collapsed := plain(strings.Join(RenderTranscript(c, 60, false), "\n"))
	for _, want := range []string{"· $ ls -la", "    List files", "12 more lines (Tab to expand)", "│ line 20", "✗ $ false failed", "│ Exit code 1", "Edit notes.txt  +2 −1", "│ @@ -1,3 +1,3 @@", "│  alpha", "│ -gamma", "│ +GAMMA2", "Read notes.txt  2 lines", "Grep 'x' in .  3 files", "✗ Read missing.txt failed", "│ File does not exist.", "warden · preview_attach", "│ https://p", "⋯ $ sleep 5"} {
		if !strings.Contains(collapsed, want) {
			t.Fatalf("missing %q in:\n%s", want, collapsed)
		}
	}
	for _, unwanted := range []string{"│ line 1\n", "diff --git", "+++ b/notes.txt", "--- a/notes.txt", "│ 1    alpha", "│ Found 3 files", "port: 3000"} {
		if strings.Contains(collapsed, unwanted) {
			t.Fatalf("unexpected %q in:\n%s", unwanted, collapsed)
		}
	}
	styled := strings.Join(RenderTranscript(c, 60, false), "\n")
	if !strings.Contains(styled, green+"    │ +GAMMA") || !strings.Contains(styled, red+"    │ -gamma") || !strings.Contains(styled, cyan+"    │ @@") {
		t.Fatalf("diff colours:\n%s", styled)
	}
	expanded := plain(strings.Join(RenderTranscript(c, 60, true), "\n"))
	for _, want := range []string{"│ line 1\n", "│ line 20", "│ 1    alpha", "│ Found 3 files", "│ c\n", "│ port: 3000", "│ title: Preview"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("missing %q when expanded in:\n%s", want, expanded)
		}
	}
	if strings.Contains(expanded, "more lines") {
		t.Fatal("expanded output still folded")
	}
	for _, l := range RenderTranscript(c, 40, false) {
		if clipLen(l) > 40 {
			t.Fatalf("line wider than 40: %q", l)
		}
	}
	// A change over several files heads each file's lines with its path.
	multi := Entry{ID: "x", Role: "activity", Text: "Updated 2 files", Detail: "a\ndiff --git a/a b/a\n--- a/a\n+++ b/a\n-1\n+2\n\nb\ndiff --git a/b b/b\nnew file mode 100644\n--- /dev/null\n+++ b/b\n@@ -0,0 +1,1 @@\n+hi\n\n", Tool: &Tool{Kind: "edit", Status: "completed", Paths: []string{"a", "b"}}}
	lines, adds, dels := diffLines(strings.TrimRight(multi.Detail, "\n"))
	if adds != 2 || dels != 1 || strings.Join(lines, "|") != "§ a|-1|+2|§ b|@@ -0,0 +1,1 @@|+hi" {
		t.Fatalf("multi-file diff: %d %d %q", adds, dels, lines)
	}
	// A removed line that itself starts with "-- " is a change, not a header.
	if lines, _, dels = diffLines("a\ndiff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1,1 +0,0 @@\n---- rule\n"); dels != 1 || lines[len(lines)-1] != "---- rule" {
		t.Fatalf("header ambiguity: %q", lines)
	}
}

func permissionAsk(id, tool string, entry *Entry, extra map[string]any) Approval {
	params := map[string]any{"tool": tool}
	if entry != nil {
		params["entry"] = entry
	}
	for k, v := range extra {
		params[k] = v
	}
	return Approval{ID: id, Method: "item/tool/requestPermission", State: "pending", Params: params}
}

// A tool ask renders as the call (a command, a diff, a plan) with the keys
// that answer it; typed answers reach the service as allow, allow always
// with the remembered rule, or deny with the message; a plan's answers
// carry the mode.
func TestPermissionCardsAndAnswers(t *testing.T) {
	c := &Chat{ID: "chat1", Title: "Claude", Provider: "claude", Status: "running", Mode: "ask"}
	command := &Entry{ID: "toolu_1", Role: "activity", Text: "touch x", Tool: &Tool{Kind: "command", Name: "Bash", Status: "running"}}
	edit := &Entry{ID: "toolu_2", Role: "activity", Text: "Write notes.md", Detail: "notes.md\ndiff --git a/notes.md b/notes.md\nnew file mode 100644\n--- /dev/null\n+++ b/notes.md\n@@ -0,0 +1,1 @@\n+hello\n", Tool: &Tool{Kind: "edit", Name: "Write", Status: "running", Paths: []string{"notes.md"}}}
	c.Approvals = []Approval{
		permissionAsk("p1", "Bash", command, map[string]any{"always": "`touch` commands", "description": "Create x"}),
		permissionAsk("p2", "Write", edit, map[string]any{"always": "file edits"}),
		permissionAsk("p3", "ExitPlanMode", nil, map[string]any{"plan": "# Plan\n\n1. Write notes.md"}),
	}
	joined := plain(strings.Join(RenderApprovals(c, 80), "\n"))
	for _, want := range []string{"run a command: Create x", "y = allow · a = allow always (`touch` commands) · A = for the workspace · n [message] = deny", "$ touch x", "Write notes.md", "+1 −0", "+hello", "Claude has a plan", "1. Write notes.md"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "diff --git") {
		t.Fatalf("git header shown:\n%s", joined)
	}
	status := plain(StatusLine(c, nil, true, time.Unix(0, 0)))
	if !strings.Contains(status, "claude · default · ask") {
		t.Fatalf("status without the mode: %q", status)
	}
	if strings.Contains(plain(StatusLine(sampleChat(), nil, true, time.Unix(0, 0))), "auto") {
		t.Fatal("a Codex chat shows no mode")
	}
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard}
	ctx := context.Background()
	refresh := func() { s, _ := app.Client.State(ctx); app.state = s }
	refresh()
	app.submit(ctx, "a")
	refresh()
	s := app.state
	if got := s.Chats[0].Approvals[0]; got.State != "allowed" || got.Params["always"] != true || got.Params["scope"] != "chat" || !strings.Contains(app.notice, "allowed always for this chat: `touch` commands") {
		t.Fatalf("allow always: %+v %q", got, app.notice)
	}
	app.submit(ctx, "n keep the notes in docs/")
	refresh()
	if got := app.state.Chats[0].Approvals[1]; got.State != "declined" || got.Params["message"] != "keep the notes in docs/" || got.Params["always"] != false {
		t.Fatalf("deny with message: %+v", got)
	}
	app.submit(ctx, "a")
	refresh()
	if got := app.state.Chats[0].Approvals[2]; got.State != "allowed" || got.Params["mode"] != "ask" || got.Params["always"] != false {
		t.Fatalf("plan approved into ask: %+v", got)
	}
	// With nothing pending, "a" is a message again.
	app.submit(ctx, "a")
	time.Sleep(100 * time.Millisecond)
	refresh()
	entries := app.state.Chats[0].Conversation.Entries
	if entries[len(entries)-1].Text != "echo: a" {
		t.Fatalf("message not sent: %+v", entries[len(entries)-1])
	}
}

// Shift+Tab cycles a Claude chat's mode auto → ask → plan → auto; /mode
// sets one by name; a Codex chat has none.
func TestShiftTabCyclesMode(t *testing.T) {
	claude := &Chat{ID: "c1", Title: "Claude", Provider: "claude", Status: "idle"}
	codex := &Chat{ID: "c2", Title: "Codex", Provider: "codex", Status: "idle"}
	f := newFakeServer(t, State{Chats: []*Chat{claude, codex}})
	app := &App{Client: f.client(), ChatID: "c1", Output: io.Discard}
	ctx := context.Background()
	refresh := func() { s, _ := app.Client.State(ctx); app.state = s }
	refresh()
	for _, want := range []string{"ask", "plan", "auto"} {
		app.handleKey(ctx, Key{Kind: KeyShiftTab})
		refresh()
		if got := app.state.Chats[0].Mode; got != want || !strings.Contains(app.notice, "permission mode "+want) {
			t.Fatalf("cycle: mode %q notice %q, want %s", got, app.notice, want)
		}
	}
	app.submit(ctx, "/mode plan")
	refresh()
	if app.state.Chats[0].Mode != "plan" {
		t.Fatal(app.state.Chats[0].Mode)
	}
	app.submit(ctx, "/mode")
	if !strings.Contains(app.notice, "permission mode plan") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/mode bypass")
	if !strings.Contains(app.notice, "/mode auto|ask|plan") {
		t.Fatal(app.notice)
	}
	if !strings.Contains(plain(StatusLine(app.state.Chats[0], nil, true, time.Unix(0, 0))), "· plan") {
		t.Fatal("status line without the mode")
	}
	app.ChatID = "c2"
	app.handleKey(ctx, Key{Kind: KeyShiftTab})
	if !strings.Contains(app.notice, "Claude chats") {
		t.Fatal(app.notice)
	}
	// The summary a watcher prints for a tool ask and a plan.
	ask := permissionAsk("p", "Bash", &Entry{Text: "touch x", Tool: &Tool{Kind: "command"}}, nil)
	if ask.Summary() != "run command: touch x" || permissionAsk("q", "ExitPlanMode", nil, nil).Summary() != "plan ready for review" {
		t.Fatal(ask.Summary())
	}
}

// A subagent's entries render under its card: a count line until
// expanded, then indented with the subagent's type as the speaker; the
// card shows how long the subagent took and its final text. A background
// call is marked; the todo list is a checklist.
func TestRenderSubagentBackgroundAndTodo(t *testing.T) {
	c := &Chat{ID: "c", Title: "t", Provider: "claude"}
	c.Conversation.Entries = []Entry{
		{ID: "a", Role: "activity", Text: "Agent: List files (Explore)", Detail: "a.txt and b.txt", CreatedAt: 100, EndedAt: 112, Tool: &Tool{Kind: "task", Name: "Agent", Status: "completed", Input: map[string]any{"subagent_type": "Explore", "prompt": "List the files."}}},
		{ID: "b", Role: "activity", Text: "ls", Detail: "a.txt\nb.txt", ParentID: "a", Tool: &Tool{Kind: "command", Name: "Bash", Status: "completed"}},
		{ID: "m", Role: "assistant", Text: "The files are a.txt and b.txt.", ParentID: "a"},
		{ID: "bg", Role: "activity", Text: "sleep 9", Detail: "", ParentID: "", Tool: &Tool{Kind: "command", Name: "Bash", Status: "running", Background: true, Description: "Wait"}},
		{ID: "todo", Role: "activity", Text: "Todo list · 1 of 3 done · Testing", Detail: "[x] Parse\n[>] Test\n[ ] Ship\n", Tool: &Tool{Kind: "todo", Status: "completed"}},
		{ID: "orphan", Role: "activity", Text: "pwd", Detail: "/w", ParentID: "gone", Tool: &Tool{Kind: "command", Name: "Bash", Status: "completed"}},
	}
	collapsed := plain(strings.Join(RenderTranscript(c, 60, false), "\n"))
	for _, want := range []string{"· Agent: List files (Explore)  12s", "    List the files.", "│ … 1 tool calls, 1 messages (Tab to expand)", "│ a.txt and b.txt", "⋯ $ sleep 9 [background]", "Todo list · 1 of 3 done · Testing", "    ✓ Parse", "    ▸ Test", "    ○ Ship", "· $ pwd"} {
		if !strings.Contains(collapsed, want) {
			t.Fatalf("missing %q in:\n%s", want, collapsed)
		}
	}
	for _, unwanted := range []string{"Explore ›", "$ ls", "[x]"} {
		if strings.Contains(collapsed, unwanted) {
			t.Fatalf("unexpected %q in:\n%s", unwanted, collapsed)
		}
	}
	expanded := plain(strings.Join(RenderTranscript(c, 60, true), "\n"))
	for _, want := range []string{"      · $ ls", "        │ a.txt", "    Explore › The files are a.txt and b.txt.", "│ a.txt and b.txt"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("missing %q when expanded in:\n%s", want, expanded)
		}
	}
	if strings.Contains(expanded, "Tab to expand") {
		t.Fatalf("expanded transcript still folded:\n%s", expanded)
	}
	for _, l := range RenderTranscript(c, 40, true) {
		if clipLen(l) > 40 {
			t.Fatalf("line wider than 40: %q", l)
		}
	}
	if formatSeconds(4.4) != "4s" || formatSeconds(72) != "1m 12s" || formatSeconds(7500) != "2h 5m" {
		t.Fatal("formatSeconds")
	}
	// The quiet view (Ctrl+O) hides a subagent's messages with the steps.
	a := &App{quiet: true}
	if v := a.visible(c); len(v.Conversation.Entries) != 0 {
		t.Fatalf("quiet view shows a subagent's message: %+v", v.Conversation.Entries)
	}
}

// /thinking, /effort and /fast set a Claude chat's session settings (a
// budget as 8k, a level, on/off), say what is set when bare, refuse what
// the service refuses, and the status line shows the settings and the
// model the session resolved.
func TestThinkingEffortAndFastCommands(t *testing.T) {
	claude := &Chat{ID: "c1", Title: "Claude", Provider: "claude", Model: "opus", Status: "idle"}
	codex := &Chat{ID: "c2", Title: "Codex", Provider: "codex", Status: "idle"}
	f := newFakeServer(t, State{Chats: []*Chat{claude, codex}})
	app := &App{Client: f.client(), ChatID: "c1", Output: io.Discard}
	ctx := context.Background()
	refresh := func() { s, _ := app.Client.State(ctx); app.state = s }
	refresh()
	app.submit(ctx, "/thinking")
	if !strings.Contains(app.notice, "thinking default") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/thinking 8k")
	refresh()
	if app.state.Chats[0].Thinking != "8000" || !strings.Contains(app.notice, "thinking 8k tokens") {
		t.Fatalf("%q %q", app.state.Chats[0].Thinking, app.notice)
	}
	app.submit(ctx, "/thinking off")
	refresh()
	if app.state.Chats[0].Thinking != "off" {
		t.Fatal(app.state.Chats[0].Thinking)
	}
	app.submit(ctx, "/thinking lots")
	if !strings.Contains(app.notice, "on, off or a budget") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/effort low")
	refresh()
	if app.state.Chats[0].Effort != "low" || !strings.Contains(app.notice, "effort low") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/effort ultra")
	if !strings.Contains(app.notice, "/effort low|medium|high|xhigh|max|default") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/fast on")
	if !strings.Contains(app.notice, "allowFastMode") {
		t.Fatal(app.notice)
	}
	f.mu.Lock()
	f.fastMode = true
	f.mu.Unlock()
	app.submit(ctx, "/fast on")
	refresh()
	if !app.state.Chats[0].Fast || !strings.Contains(app.notice, "fast mode on") {
		t.Fatal(app.notice)
	}
	f.mu.Lock()
	f.state.Chats[0].Session = &SessionInfo{Model: "claude-opus-5", FastMode: "on"}
	f.mu.Unlock()
	refresh()
	line := plain(StatusLine(app.state.Chats[0], nil, true, time.Unix(0, 0)))
	for _, want := range []string{"claude · opus (claude-opus-5)", "· thinking off", "· effort low", "· fast"} {
		if !strings.Contains(line, want) {
			t.Fatalf("status line %q lacks %q", line, want)
		}
	}
	app.submit(ctx, "/fast")
	if !strings.Contains(app.notice, "fast mode on (session: on)") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/effort default")
	refresh()
	if app.state.Chats[0].Effort != "" {
		t.Fatal(app.state.Chats[0].Effort)
	}
	// /model on a Claude chat says the session takes it; a Codex chat's
	// next run does.
	app.submit(ctx, "/model opus")
	if !strings.Contains(app.notice, "model opus · a running session switches now") {
		t.Fatal(app.notice)
	}
	app.ChatID = "c2"
	app.submit(ctx, "/effort low")
	if !strings.Contains(app.notice, "Claude chats") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/model gpt-5.5")
	if !strings.Contains(app.notice, "next run uses codex · gpt-5.5") {
		t.Fatal(app.notice)
	}
	if items := commandItems("thi", nil); len(items) != 1 || items[0].Name != "thinking" {
		t.Fatalf("%+v", items)
	}
}

func TestRewindCommandListsConfirmsAndRewinds(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Rewind", Provider: "claude", Status: "idle", Conversation: Conversation{Entries: []Entry{
		{ID: "u1", Role: "user", Text: "Add a counter component"},
		{ID: "a1", Role: "assistant", Text: "Done."},
		{ID: "u2", Role: "user", Text: "Now make it   blue\nplease"},
		{ID: "a2", Role: "assistant", Text: "Blue now."},
	}}}}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	app.refreshState(ctx)
	app.submit(ctx, "/rewind")
	notice := plain(app.notice)
	if !strings.Contains(notice, " 1   Add a counter component") || !strings.Contains(notice, " 2 • Now make it blue please") || !strings.Contains(notice, "/rewind N") {
		t.Fatalf("listing: %q", notice)
	}
	for _, bad := range []string{"/rewind 3", "/rewind x", "/rewind 2 files-and-more extra", "/rewind 1 nothing"} {
		app.submit(ctx, bad)
		if app.confirm != nil || !strings.HasPrefix(app.notice, "/rewind N") {
			t.Fatalf("%s: confirm %v notice %q", bad, app.confirm != nil, app.notice)
		}
	}
	app.submit(ctx, "/rewind 2 conv")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, `Rewind to before message 2 "Now make it blue please" (conversation)?`) {
		t.Fatalf("confirm: %+v", app.confirm)
	}
	app.submit(ctx, "n")
	if app.confirm != nil || app.notice != "cancelled" {
		t.Fatalf("cancel: %+v %q", app.confirm, app.notice)
	}
	app.submit(ctx, "/rewind 2")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, "(code and conversation)") {
		t.Fatalf("default scope: %+v", app.confirm)
	}
	app.submit(ctx, "y")
	if app.confirm != nil || app.notice != "rewound to before message 2 (code and conversation); 1 file(s) restored, 0 removed" {
		t.Fatalf("rewind: %+v %q", app.confirm, app.notice)
	}
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	entries := f.state.Chats[0].Conversation.Entries
	f.mu.Unlock()
	if !strings.Contains(calls, "POST chats/chat1/rewind") || len(entries) != 3 || entries[2].Role != "rewind" {
		t.Fatalf("rewind route: %d entries, calls:\n%s", len(entries), calls)
	}
	// The marker renders as a line of its own, with the detail.
	lines := RenderTranscript(app.chat(), 100, false)
	joined := plain(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "↶ Rewound to before “u2” (both)") || !strings.Contains(joined, "1 file restored, 0 files removed") {
		t.Fatalf("marker: %s", joined)
	}
	// A running chat is refused before asking.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/rewind 1 code")
	if app.confirm != nil || app.notice != "stop the agent first (Esc)" {
		t.Fatalf("running: %+v %q", app.confirm, app.notice)
	}
}

func TestDiffCommandShowsChangesFoldedAndExpanded(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Diff", Provider: "claude", Status: "idle", Conversation: Conversation{Entries: []Entry{{ID: "u1", Role: "user", Text: "go"}}}}}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }, Size: func() (int, int) { return 100, 40 }}
	app.refreshState(ctx)
	app.submit(ctx, "/diff")
	if app.diff == nil || app.notice != "2 changed file(s); Tab expands the hunks, /diff hides them" {
		t.Fatalf("diff: %v %q", app.diff != nil, app.notice)
	}
	folded := plain(strings.Join(composed(app, 100), "\n"))
	for _, want := range []string{"Changes since this chat began: 2 file(s), +2 −1", "src/app.go  +2 −1", "logo.png  binary", "Tab expands the hunks"} {
		if !strings.Contains(folded, want) {
			t.Fatalf("folded lacks %q:\n%s", want, folded)
		}
	}
	if strings.Contains(folded, "+new") {
		t.Fatal("folded view shows hunks")
	}
	app.handleKey(ctx, Key{Kind: KeyTab})
	expanded := plain(strings.Join(composed(app, 100), "\n"))
	for _, want := range []string{"@@ -1,2 +1,3 @@", "-old", "+new", "+more", "(not in the diff)"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded lacks %q:\n%s", want, expanded)
		}
	}
	app.submit(ctx, "/diff")
	if app.diff != nil || app.notice != "changes hidden" {
		t.Fatalf("hide: %v %q", app.diff != nil, app.notice)
	}
	// Switching chats drops it.
	app.submit(ctx, "/diff")
	app.selectChat("chat1")
	if app.diff != nil {
		t.Fatal("selectChat kept the diff")
	}
	if got := splitDiff("diff --git a/x b/x\n+1\ndiff --git a/y b/y\n+2\n"); got["x"] != "diff --git a/x b/x\n+1\n" || got["y"] != "diff --git a/y b/y\n+2\n" {
		t.Fatalf("splitDiff: %v", got)
	}
}

// "!cmd" runs the rest as a shell command in the workspace by the person
// (off the loop, reported when it ends) and "#note" appends to CLAUDE.md;
// neither is sent to the agent as a message.
func TestBangAndHashPrefixes(t *testing.T) {
	c := sampleChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, later: make(chan func(context.Context), 8)}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s
	app.submit(ctx, "!ls -la")
	if !strings.Contains(app.notice, "running in the workspace: ls -la") {
		t.Fatalf("notice %q", app.notice)
	}
	select {
	case fn := <-app.later:
		fn(ctx)
	case <-time.After(3 * time.Second):
		t.Fatal("no report")
	}
	if app.notice != "command finished" {
		t.Fatalf("notice %q", app.notice)
	}
	app.submit(ctx, "! false now")
	select {
	case fn := <-app.later:
		fn(ctx)
	case <-time.After(3 * time.Second):
		t.Fatal("no report")
	}
	if app.notice != "command exited 1" {
		t.Fatalf("notice %q", app.notice)
	}
	app.submit(ctx, "#use tabs")
	if app.notice != "added to CLAUDE.md" {
		t.Fatalf("notice %q", app.notice)
	}
	// A lone "!" or "#" is a message like any other.
	app.submit(ctx, "!")
	app.submit(ctx, "#")
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	notes := f.notes
	f.mu.Unlock()
	if strings.Count(calls, "POST chats/chat1/exec") != 2 || strings.Count(calls, "POST chats/chat1/memory") != 1 || strings.Count(calls, "POST chats/chat1/message") != 2 {
		t.Fatalf("calls:\n%s", calls)
	}
	if len(notes) != 1 || notes[0] != "use tabs" {
		t.Fatalf("notes %q", notes)
	}
	// The person's command renders as their own card, failure by status.
	s, _ = app.Client.State(ctx)
	app.state = s
	lines := plain(strings.Join(RenderTranscript(app.chat(), 80, true), "\n"))
	if !strings.Contains(lines, "you $ ls -la") || !strings.Contains(lines, "out of ls -la") || !strings.Contains(lines, "✗ you $ false now exit 1") || !strings.Contains(lines, "Added to CLAUDE.md") {
		t.Fatalf("transcript:\n%s", lines)
	}
}

// A long bracketed paste stands in the draft as a placeholder and is put
// back when the prompt is sent; a short one is inserted as it is.
func TestLongPasteCollapsesToAPlaceholder(t *testing.T) {
	var e Editor
	e.Handle(Key{Kind: KeyPaste, Text: "one\ntwo"})
	if e.Text() != "one\ntwo" || len(e.Pastes()) != 0 {
		t.Fatalf("short paste: %q %d", e.Text(), len(e.Pastes()))
	}
	e.Clear()
	long := strings.Repeat("line\n", 20)
	e.Insert("see ")
	e.Handle(Key{Kind: KeyPaste, Text: long})
	e.Insert(" and ")
	wide := strings.Repeat("x", 1200)
	e.Handle(Key{Kind: KeyPaste, Text: wide})
	if e.Text() != "see [Pasted text #1 — 20 lines] and [Pasted text #2 — 1 line]" || len(e.Pastes()) != 2 {
		t.Fatalf("placeholders: %q %d", e.Text(), len(e.Pastes()))
	}
	// Deleting a placeholder drops that paste from the prompt; a typed
	// placeholder for a paste that does not exist stays as written.
	e.Insert(" [Pasted text #9 — 3 lines]")
	got := e.Submit()
	want := "see " + long + " and " + wide + " [Pasted text #9 — 3 lines]"
	if got != want {
		t.Fatalf("expanded: %d chars, want %d: %q…", len(got), len(want), got[:40])
	}
	if e.Text() != "" || len(e.Pastes()) != 0 {
		t.Fatal("editor not cleared")
	}
	if !LongPaste(strings.Repeat("a\n", 9)) || LongPaste(strings.Repeat("a\n", 8)) || !LongPaste(strings.Repeat("b", 1001)) || LongPaste(strings.Repeat("b", 1000)) {
		t.Fatal("thresholds")
	}
}

// A compaction entry is a divider: the trigger and the token counts, the
// summary only when expanded, yellow while it runs and red when it
// failed. The status line shows the context against the window, yellow
// from 80 % and red from 95 %. /compact, with or without instructions,
// goes to a Claude chat as the message; a Codex chat does not offer it.
func TestCompactionDividerContextAndPassthrough(t *testing.T) {
	c := &Chat{ID: "c", Title: "t", Provider: "claude", Status: "idle"}
	c.Conversation.Entries = []Entry{
		{ID: "k1", Role: "compaction", Text: "Compacting context…", IsStreaming: true, Compaction: &Compaction{Status: "running"}},
		{ID: "k2", Role: "compaction", Text: "Context compacted", Detail: "Summary:\n1. Files read: a.txt (lima)", Compaction: &Compaction{Status: "completed", Trigger: "manual", PreTokens: 171238, PostTokens: 2194}},
		{ID: "k3", Role: "compaction", Text: "Context compacted", Compaction: &Compaction{Status: "completed", Trigger: "auto", PreTokens: 184293}},
		{ID: "k4", Role: "compaction", Text: "Compaction failed", Compaction: &Compaction{Status: "failed", Error: "API Error: refused"}},
		{ID: "k5", Role: "compaction", Text: "Context compacted"},
	}
	collapsed := plain(strings.Join(RenderTranscript(c, 70, false), "\n"))
	for _, want := range []string{"── Compacting context… ──", "── Context compacted · manual · 171k → 2.2k tokens ──", "── Context compacted · automatic · from 184k tokens ──", "── Compaction failed: API Error: refused ──", "── Context compacted ──"} {
		if !strings.Contains(collapsed, want) {
			t.Fatalf("missing %q in:\n%s", want, collapsed)
		}
	}
	if strings.Contains(collapsed, "Files read") {
		t.Fatalf("summary shown collapsed:\n%s", collapsed)
	}
	styled := strings.Join(RenderTranscript(c, 70, false), "\n")
	if !strings.Contains(styled, yellow+"  ── Compacting") || !strings.Contains(styled, red+"  ── Compaction failed") {
		t.Fatalf("colours:\n%s", styled)
	}
	if expanded := plain(strings.Join(RenderTranscript(c, 70, true), "\n")); !strings.Contains(expanded, "     1. Files read: a.txt (lima)") {
		t.Fatalf("summary not expanded:\n%s", expanded)
	}
	md := ExportMarkdown(c, false, time.Unix(0, 0), func(float64) string { return "t" })
	if !strings.Contains(md, "### Context compacted · manual · 171k → 2.2k tokens\n\n> Summary:\n> 1. Files read: a.txt (lima)") || !strings.Contains(md, "### Compaction failed: API Error: refused") {
		t.Fatalf("export:\n%s", md)
	}
	// The context indicator.
	now := time.Unix(1000, 0)
	if s := plain(StatusLine(c, nil, true, now)); strings.Contains(s, "ctx") {
		t.Fatalf("indicator without a context: %q", s)
	}
	c.Conversation.Context = &Context{Used: 42787, Window: 200000, Model: "claude-sonnet-5"}
	if s := StatusLine(c, nil, true, now); !strings.Contains(s, "  ctx 43k/200k (21%)") || strings.Contains(s, yellow+"ctx") {
		t.Fatalf("indicator: %q", s)
	}
	c.Conversation.Context.Used = 171238
	if s := StatusLine(c, nil, true, now); !strings.Contains(s, yellow+"ctx 171k/200k (86%)"+reset) {
		t.Fatalf("yellow indicator: %q", s)
	}
	c.Conversation.Context.Used = 191000
	if s := StatusLine(c, nil, true, now); !strings.Contains(s, red+"ctx 191k/200k (96%)"+reset) {
		t.Fatalf("red indicator: %q", s)
	}
	// With the agent's own threshold (the window less its buffer), the
	// colours follow the way to that: 142k is 71 % of the window but 85 %
	// of the way to a compaction at 167k.
	c.Conversation.Context = &Context{Used: 142000, Window: 200000, Threshold: 167000}
	if s := StatusLine(c, nil, true, now); !strings.Contains(s, yellow+"ctx 142k/200k (71%)"+reset) {
		t.Fatalf("threshold indicator: %q", s)
	}
	c.Conversation.Context.Used = 160000
	if s := StatusLine(c, nil, true, now); !strings.Contains(s, red+"ctx 160k/200k (80%)"+reset) {
		t.Fatalf("threshold red indicator: %q", s)
	}
	c.Conversation.Context = &Context{Used: 42787}
	if s := plain(StatusLine(c, nil, true, now)); !strings.Contains(s, "ctx 43k") || strings.Contains(s, "/") {
		t.Fatalf("windowless indicator: %q", s)
	}
	// A /compact turn reports no tokens, only the compaction's cost.
	c.Conversation.Turns = []Turn{{ID: "t", StartedAt: 990, EndedAt: 1017, Usage: &Usage{CostUSD: 0.16}}}
	c.Conversation.Entries[1].TurnID = &c.Conversation.Turns[0].ID
	if s := plain(StatusLine(c, nil, true, now)); strings.Contains(s, "0 tokens") || !strings.Contains(s, "27s · $0.16") {
		t.Fatalf("compaction turn stats: %q", s)
	}
	var out bytes.Buffer
	printEntry(&out, c, c.Conversation.Entries[1])
	if out.String() != "  ── Context compacted · manual · 171k → 2.2k tokens ──\n" {
		t.Fatalf("follow line: %q", out.String())
	}
	// /compact is sent as text to a Claude chat.
	codex := sampleChat()
	codex.Status = "idle"
	f := newFakeServer(t, State{Chats: []*Chat{c, codex}})
	app := &App{Client: f.client(), ChatID: "c", Output: io.Discard, Now: func() time.Time { return now }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/comp")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Label != "/compact [INSTRUCTIONS]" || !app.menu.Items[0].Run {
		t.Fatalf("menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyEnter})
	time.Sleep(100 * time.Millisecond)
	s, _ := app.Client.State(ctx)
	entries := s.Chats[0].Conversation.Entries
	if got := entries[len(entries)-1].Text; got != "echo: /compact" {
		t.Fatalf("enter on /compact: %q (notice %q)", got, app.notice)
	}
	app.submit(ctx, "/compact keep the file list")
	time.Sleep(100 * time.Millisecond)
	s, _ = app.Client.State(ctx)
	entries = s.Chats[0].Conversation.Entries
	if got := entries[len(entries)-1].Text; got != "echo: /compact keep the file list" {
		t.Fatalf("/compact with instructions: %q", got)
	}
	// Once the chat reports its list, the menu is that list, with the
	// known argument and hint for /compact and the reported description
	// for the workspace's own.
	c.Commands = []AgentCommand{{Name: "compact"}, {Name: "probe-cmd", Description: "From the workspace"}}
	app.state, _ = app.Client.State(ctx)
	f.mu.Lock()
	f.state.Chats[0].Commands = c.Commands
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	app.editor.Clear()
	app.menu = nil
	typeText(app, ctx, "/")
	labels := []string{}
	for _, it := range app.menu.Items {
		labels = append(labels, it.Label+"|"+it.Hint)
	}
	if got := strings.Join(labels, "\n"); !strings.Contains(got, "/compact [INSTRUCTIONS]|replace the history with a summary; say what to keep") || !strings.Contains(got, "/probe-cmd|From the workspace") {
		t.Fatalf("reported list: %s", got)
	}
	app.editor.Clear()
	app.menu = nil
	app.submit(ctx, "/probe-cmd now")
	time.Sleep(100 * time.Millisecond)
	s, _ = app.Client.State(ctx)
	entries = s.Chats[0].Conversation.Entries
	if got := entries[len(entries)-1].Text; got != "echo: /probe-cmd now" {
		t.Fatalf("a reported command is sent as text: %q", got)
	}
	// A Codex chat has no /compact: the menu leaves it out and typing it
	// is an unknown command.
	app.ChatID = "chat1"
	app.editor.Clear()
	app.menu = nil
	typeText(app, ctx, "/comp")
	if app.menu != nil && len(app.menu.Items) > 0 {
		t.Fatalf("codex menu offers: %+v", app.menu.Items)
	}
	app.editor.Clear()
	app.submit(ctx, "/compact")
	if !strings.Contains(app.notice, "unknown command /compact") {
		t.Fatalf("codex notice: %q", app.notice)
	}
}

// The queue: markers under queued messages, /queue, /withdraw N, ↑ on an
// empty draft editing the last queued message (its files back on the
// list), a held queue after Esc and /queue send.
func TestQueueMarkersWithdrawAndEditLastQueued(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Queue", Provider: "claude", Status: "running", Conversation: Conversation{Entries: []Entry{
		{ID: "u1", Role: "user", Text: "first", Delivery: "sent"},
		{ID: "q1", Role: "user", Text: "second, while it runs", Delivery: "queued"},
		{ID: "q2", Role: "user", Text: "third\nwith a file", Delivery: "queued", Attachments: []Attachment{{ID: "att1", Name: "notes.txt", Kind: "file", Size: 5}}},
	}}}}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	app.refreshState(ctx)
	joined := plain(strings.Join(RenderTranscript(app.chat(), 100, false), "\n"))
	if strings.Count(joined, "(queued · sends when the agent finishes)") != 2 {
		t.Fatalf("markers: %s", joined)
	}
	// Queued messages render last, after what the running turn appends.
	f.mu.Lock()
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "a1", Role: "assistant", Text: "still working"})
	f.mu.Unlock()
	app.refreshState(ctx)
	joined = plain(strings.Join(RenderTranscript(app.chat(), 100, false), "\n"))
	if strings.Index(joined, "still working") > strings.Index(joined, "second, while it runs") {
		t.Fatalf("queued messages not last: %s", joined)
	}
	app.submit(ctx, "/queue")
	notice := plain(app.notice)
	if !strings.Contains(notice, " 1  second, while it runs") || !strings.Contains(notice, " 2  third with a file") || !strings.Contains(notice, "sent in order after this turn") {
		t.Fatalf("/queue: %q", notice)
	}
	app.submit(ctx, "/withdraw 3")
	if !strings.HasPrefix(app.notice, "/withdraw N") {
		t.Fatalf("bad index: %q", app.notice)
	}
	app.submit(ctx, "/withdraw 1")
	if app.notice != "withdrawn: second, while it runs" || len(queuedMessages(app.chat())) != 1 {
		t.Fatalf("/withdraw 1: %q, %d queued", app.notice, len(queuedMessages(app.chat())))
	}
	// ↑ on an empty draft loads the last queued message into the editor
	// in place: it stays queued (its file with it) while edited.
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.editor.Text() != "third\nwith a file" || len(queuedMessages(app.chat())) != 1 || app.editing == nil || app.attachments["chat1"] != nil {
		t.Fatalf("↑: draft %q, %d queued, editing %v, files %+v", app.editor.Text(), len(queuedMessages(app.chat())), app.editing != nil, app.attachments["chat1"])
	}
	if !strings.HasPrefix(app.notice, "editing the queued message in place") {
		t.Fatalf("notice: %q", app.notice)
	}
	// Esc leaves it as it was.
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.editing != nil || app.editor.Text() != "" || !strings.Contains(app.notice, "cancelled") {
		t.Fatalf("Esc: editing %v draft %q notice %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	app.refreshState(ctx)
	if q := queuedMessages(app.chat()); len(q) != 1 || q[0].Text != "third\nwith a file" {
		t.Fatalf("after the cancelled edit: %+v", q)
	}
	// Enter saves the edit into the message's slot: same ID, same file.
	app.handleKey(ctx, Key{Kind: KeyUp})
	app.editor.Set("third, edited\nwith a file")
	app.send(ctx)
	if app.editing != nil || app.editor.Text() != "" || app.notice != "saved queued message" {
		t.Fatalf("save: editing %v draft %q notice %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	f.mu.Lock()
	entries := f.state.Chats[0].Conversation.Entries
	f.mu.Unlock()
	edited := entries[1] // its slot: after the first message, before the reply that came later
	if edited.ID != "q2" || edited.Text != "third, edited\nwith a file" || edited.Delivery != "queued" || len(edited.Attachments) != 1 || edited.Attachments[0].ID != "att1" {
		t.Fatalf("edited in place: %+v", edited)
	}
	// An empty edit is refused (withdraw is the way to drop it).
	app.handleKey(ctx, Key{Kind: KeyUp})
	app.editor.Set("  ")
	app.send(ctx)
	if app.editing == nil || !strings.Contains(app.notice, "would be empty") {
		t.Fatalf("empty edit: editing %v notice %q", app.editing != nil, app.notice)
	}
	// The agent got the message meanwhile: the save is refused, the edit
	// ends and the draft stays to send as a new message.
	app.editor.Set("third, again")
	f.mu.Lock()
	f.state.Chats[0].Conversation.Entries[1].Delivery = "sent"
	f.mu.Unlock()
	app.send(ctx)
	if app.editing != nil || app.editor.Text() != "third, again" || !strings.Contains(app.notice, "got the message before the edit was saved") {
		t.Fatalf("conflict: editing %v draft %q notice %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	app.editor.Clear()
	app.refreshState(ctx)
	// With nothing queued, ↑ recalls the history as before.
	app.editor.SetHistory([]string{"older prompt"})
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.editor.Text() != "older prompt" {
		t.Fatalf("↑ without a queue: %q", app.editor.Text())
	}
	app.editor.Clear()
	// A held queue: Esc interrupted the turn, the queued message stays.
	f.mu.Lock()
	f.state.Chats[0].Status = "interrupted"
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "q3", Role: "user", Text: "held one", Delivery: "queued"})
	f.mu.Unlock()
	app.refreshState(ctx)
	joined = plain(strings.Join(RenderTranscript(app.chat(), 100, false), "\n"))
	if !strings.Contains(joined, "(queued · held: /queue send lets it go, or the next message)") {
		t.Fatalf("held marker: %s", joined)
	}
	app.submit(ctx, "/queue")
	if !strings.Contains(plain(app.notice), "held since the agent was stopped") {
		t.Fatalf("/queue held: %q", app.notice)
	}
	app.submit(ctx, "/queue send")
	if app.notice != "sending the queued messages" || app.chat().Status != "queued" {
		t.Fatalf("/queue send: %q %s", app.notice, app.chat().Status)
	}
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	f.mu.Unlock()
	if !strings.Contains(calls, "POST chats/chat1/withdraw") || !strings.Contains(calls, "POST chats/chat1/send-queued") || strings.Count(calls, "POST chats/chat1/queued/q2/edit") != 2 {
		t.Fatalf("calls:\n%s", calls)
	}
	app.submit(ctx, "/queue nonsense")
	if !strings.HasPrefix(app.notice, "/queue lists") {
		t.Fatalf("/queue nonsense: %q", app.notice)
	}
	// Esc while running says the queue is held.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if !strings.Contains(app.notice, "queued messages are held") {
		t.Fatalf("Esc notice: %q", app.notice)
	}
}

// /edit N on a message the agent got confirms, rewinds the conversation
// to before it (the code too with "both") and puts it in the editor;
// Esc-Esc on an empty draft is /edit on the last message; a queued
// message just leaves the queue.
func TestEditCommandRewindsAndPrefills(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Edit", Provider: "claude", Status: "idle", Conversation: Conversation{Entries: []Entry{
		{ID: "u1", Role: "user", Text: "Add a counter component", Delivery: "sent", Attachments: []Attachment{{ID: "att1", Name: "spec.md", Kind: "file", Size: 9}}},
		{ID: "a1", Role: "assistant", Text: "Done."},
		{ID: "u2", Role: "user", Text: "Now make it blue", Delivery: "sent"},
		{ID: "a2", Role: "assistant", Text: "Blue now."},
	}}}}})
	ctx := context.Background()
	now := time.Unix(0, 0)
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return now }}
	app.refreshState(ctx)
	for _, bad := range []string{"/edit 3", "/edit x", "/edit 2 sideways", "/edit 2 both extra"} {
		app.submit(ctx, bad)
		if app.confirm != nil || !strings.HasPrefix(app.notice, "/edit [N]") {
			t.Fatalf("%s: confirm %v notice %q", bad, app.confirm != nil, app.notice)
		}
	}
	app.submit(ctx, "/edit 2")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, `Edit message 2 "Now make it blue"? The conversation goes back to before it`) {
		t.Fatalf("confirm: %+v", app.confirm)
	}
	app.submit(ctx, "n")
	if app.confirm != nil || app.notice != "cancelled" || app.editor.Text() != "" {
		t.Fatalf("cancel: %+v %q %q", app.confirm, app.notice, app.editor.Text())
	}
	app.submit(ctx, "/edit 2")
	app.submit(ctx, "y")
	if app.confirm != nil || app.editor.Text() != "Now make it blue" || !strings.HasPrefix(app.notice, "rewound to before message 2 (conversation)") || !strings.HasSuffix(app.notice, "edit the message and Enter sends it") {
		t.Fatalf("edit: %+v %q %q", app.confirm, app.editor.Text(), app.notice)
	}
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	entries := f.state.Chats[0].Conversation.Entries
	f.mu.Unlock()
	if !strings.Contains(calls, "POST chats/chat1/rewind") || len(entries) != 3 || entries[2].Role != "rewind" {
		t.Fatalf("rewind route: %d entries, calls:\n%s", len(entries), calls)
	}
	app.editor.Clear()
	// Esc-Esc on an empty draft: /edit on the last message left, with
	// its file back on the list once confirmed; "both" rewinds the code.
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.confirm != nil {
		t.Fatal("one Esc asked to edit")
	}
	now = now.Add(200 * time.Millisecond)
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, `Edit message 1 "Add a counter component"?`) {
		t.Fatalf("Esc-Esc: %+v", app.confirm)
	}
	app.submit(ctx, "n")
	app.submit(ctx, "/edit 1 both")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, "Code and conversation goes back") {
		t.Fatalf("/edit 1 both: %+v", app.confirm)
	}
	app.submit(ctx, "y")
	if app.editor.Text() != "Add a counter component" || len(app.attachments["chat1"]) != 1 || app.attachments["chat1"][0].Name != "spec.md" || !strings.HasPrefix(app.notice, "rewound to before message 1 (code and conversation)") {
		t.Fatalf("/edit 1 both: %q files %+v notice %q", app.editor.Text(), app.attachments["chat1"], app.notice)
	}
	// Two Escapes far apart do not edit; a running chat is refused.
	app.editor.Clear()
	app.handleKey(ctx, Key{Kind: KeyEscape})
	now = now.Add(2 * time.Second)
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.confirm != nil {
		t.Fatal("slow Esc Esc asked to edit")
	}
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "u3", Role: "user", Text: "sent one", Delivery: "sent"}, Entry{ID: "q1", Role: "user", Text: "queued one", Delivery: "queued"})
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/edit 1")
	if app.confirm != nil || app.notice != "stop the agent first (Esc)" {
		t.Fatalf("running: %+v %q", app.confirm, app.notice)
	}
	// A queued message: no rewind, it is edited in place and stays queued.
	n := len(rewindTargets(app.chat()))
	app.submit(ctx, "/edit "+strconv.Itoa(n))
	if app.confirm != nil || app.editor.Text() != "queued one" || len(queuedMessages(app.chat())) != 1 || app.editing == nil {
		t.Fatalf("/edit of a queued message: %+v %q editing %v", app.confirm, app.editor.Text(), app.editing != nil)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if app.editing != nil || !strings.Contains(app.notice, "cancelled") {
		t.Fatalf("Ctrl+C: %q", app.notice)
	}
}

// /undo-rewind puts back what the last rewind removed (the marker says
// so while it can), after confirmation; `code` restores the workspace
// too when the rewind recorded it; a `!` command while the queue is held
// says the queue stays held.
func TestUndoRewindCommandAndHeldNotes(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Undo", Provider: "claude", Status: "idle", UndoRewind: "marker", Conversation: Conversation{Entries: []Entry{
		{ID: "u1", Role: "user", Text: "first", Delivery: "sent"},
		{ID: "marker", Role: "rewind", Text: "Rewound to before “second” (code and conversation)", Detail: "1 file restored, 0 files removed", Rewind: &RewindMark{MessageID: "u2", What: "both", Conversation: "rewound", Before: "marker"}},
	}}}}})
	f.tail = []Entry{{ID: "u2", Role: "user", Text: "second", Delivery: "sent"}, {ID: "a2", Role: "assistant", Text: "done"}}
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	app.refreshState(ctx)
	joined := plain(strings.Join(RenderTranscript(app.chat(), 120, false), "\n"))
	if !strings.Contains(joined, "/undo-rewind puts the removed messages back") || !strings.Contains(joined, "/undo-rewind code restores the workspace") {
		t.Fatalf("marker hint: %s", joined)
	}
	app.submit(ctx, "/undo-rewind nonsense")
	if app.notice != "/undo-rewind [code]" {
		t.Fatalf("bad argument: %q", app.notice)
	}
	app.submit(ctx, "/undo-rewind code")
	if app.confirm == nil || !strings.Contains(app.confirm.prompt, "the conversation and the workspace") {
		t.Fatalf("confirmation: %+v", app.confirm)
	}
	app.submit(ctx, "n")
	if app.confirm != nil || app.notice != "cancelled" {
		t.Fatalf("n: %+v %q", app.confirm, app.notice)
	}
	app.submit(ctx, "/undo-rewind code")
	app.submit(ctx, "y")
	if !strings.Contains(app.notice, "rewind undone: 2 entries restored") || !strings.Contains(app.notice, "cannot take the messages back") || !strings.Contains(app.notice, "back as it was before the rewind (1 file(s) restored") {
		t.Fatalf("undo notice: %q", app.notice)
	}
	joined = plain(strings.Join(RenderTranscript(app.chat(), 120, false), "\n"))
	if strings.Contains(joined, "Rewound to before") || !strings.Contains(joined, "second") || !strings.Contains(joined, "Rewind undone") {
		t.Fatalf("transcript after the undo: %s", joined)
	}
	app.submit(ctx, "/undo-rewind")
	if app.notice != "nothing to undo: no rewind since the last turn" {
		t.Fatalf("nothing to undo: %q", app.notice)
	}
	// A conversation-only rewind offers no workspace restore.
	f.mu.Lock()
	f.state.Chats[0].UndoRewind = "m2"
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "m2", Role: "rewind", Text: "Rewound to before “x” (conversation)", Rewind: &RewindMark{MessageID: "x", What: "conversation", Conversation: "pending"}})
	f.mu.Unlock()
	app.refreshState(ctx)
	joined = plain(strings.Join(RenderTranscript(app.chat(), 120, false), "\n"))
	if !strings.Contains(joined, "/undo-rewind puts the removed messages back (until the next turn)") || strings.Contains(joined, "restores the workspace") {
		t.Fatalf("conversation marker hint: %s", joined)
	}
	app.submit(ctx, "/undo-rewind code")
	if app.confirm != nil || !strings.Contains(app.notice, "no checkpoint of the workspace before the rewind") {
		t.Fatalf("code on a conversation rewind: %+v %q", app.confirm, app.notice)
	}
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/undo-rewind")
	if app.notice != "stop the agent first (Esc)" {
		t.Fatalf("while running: %q", app.notice)
	}
	// A held queue: `!` and `#` say it stays held.
	f.mu.Lock()
	f.state.Chats[0].Status = "interrupted"
	f.state.Chats[0].Conversation.Entries = append(f.state.Chats[0].Conversation.Entries, Entry{ID: "q1", Role: "user", Text: "held", Delivery: "queued"})
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "#keep it small")
	if !strings.Contains(app.notice, "added to CLAUDE.md · 1 queued message(s) still held (/queue send lets them go)") {
		t.Fatalf("# while held: %q", app.notice)
	}
	app.submit(ctx, "!ls")
	if !strings.Contains(app.notice, "running in the workspace: ls · 1 queued message(s) still held") {
		t.Fatalf("! while held: %q", app.notice)
	}
}

// /instructions shows and edits the person's standing instructions in the
// composer (Enter saves as typed, Esc cancels, a failed save keeps the
// draft); /memory lists the workspace's files, shows one by number or
// label, and edits one in the composer, a new workspace file included.
func TestInstructionsAndMemoryCommands(t *testing.T) {
	c := sampleChat()
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	f.memory = MemoryView{Root: "/home/agent/workspace", AutoDir: "/home/agent/.claude/projects/-home-agent-workspace/memory", Exists: true, Files: []MemoryFile{
		{Scope: "workspace", Path: "CLAUDE.md", Size: 12, Text: "# Project\n\n- use tabs\n"},
		{Scope: "workspace", Path: ".claude/rules/style.md", Size: 5, Text: "style"},
		{Scope: "auto", Path: "MEMORY.md", Size: 5, Text: "index"},
	}, Hint: "Claude reads these only once the workspace's settings are loaded, which the current launch does not do yet."}
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard}
	ctx := context.Background()
	s, _ := app.Client.State(ctx)
	app.state = s

	app.submit(ctx, "/instructions")
	if !strings.Contains(app.notice, "no standing instructions") {
		t.Fatalf("notice %q", app.notice)
	}
	app.submit(ctx, "/instructions edit")
	if app.editing == nil || app.editing.label != "your instructions" || app.editor.Text() != "" {
		t.Fatalf("editing %+v text %q", app.editing, app.editor.Text())
	}
	typeText(app, ctx, "Answer in haiku form.")
	app.handleKey(ctx, Key{Kind: KeyNewline})
	typeText(app, ctx, "Be brief.")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.editing != nil || app.editor.Text() != "" || app.notice != "saved your instructions" {
		t.Fatalf("after save: editing %v text %q notice %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	f.mu.Lock()
	saved := f.instructions
	f.mu.Unlock()
	if saved != "Answer in haiku form.\nBe brief." {
		t.Fatalf("saved %q", saved)
	}
	if h := app.editor.History(); len(h) != 0 {
		t.Fatalf("a file edit went into the prompt history: %v", h)
	}
	app.submit(ctx, "/instructions")
	if !strings.Contains(app.notice, "Answer in haiku form.") {
		t.Fatalf("notice %q", app.notice)
	}
	// Editing loads the current text; Esc cancels without saving.
	app.submit(ctx, "/instructions edit")
	if app.editor.Text() != "Answer in haiku form.\nBe brief." {
		t.Fatalf("loaded %q", app.editor.Text())
	}
	typeText(app, ctx, " NOT")
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.editing != nil || app.editor.Text() != "" || !strings.Contains(app.notice, "cancelled") {
		t.Fatalf("after Esc: %v %q %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	f.mu.Lock()
	saved = f.instructions
	f.mu.Unlock()
	if saved != "Answer in haiku form.\nBe brief." {
		t.Fatalf("Esc saved: %q", saved)
	}
	frame := app.frame(80, 24)
	if strings.Contains(strings.Join(frame.Extra, "\n"), "editing") {
		t.Fatalf("banner after cancel: %v", frame.Extra)
	}
	app.submit(ctx, "/instructions clear")
	f.mu.Lock()
	saved = f.instructions
	f.mu.Unlock()
	if saved != "" || app.notice != "instructions cleared" {
		t.Fatalf("clear: %q %q", saved, app.notice)
	}

	// /memory lists; N or a label shows; edit loads.
	app.submit(ctx, "/memory")
	for _, want := range []string{" 1  CLAUDE.md", " 2  .claude/rules/style.md", " 3  auto:MEMORY.md", "auto-memory: /home/agent/.claude/projects/-home-agent-workspace/memory", "Claude reads these only"} {
		if !strings.Contains(app.notice, want) {
			t.Fatalf("listing lacks %q:\n%s", want, app.notice)
		}
	}
	app.submit(ctx, "/memory 1")
	if !strings.Contains(app.notice, "CLAUDE.md (12 B):\n# Project\n\n- use tabs") {
		t.Fatalf("show: %q", app.notice)
	}
	app.submit(ctx, "/memory auto:MEMORY.md")
	if !strings.HasPrefix(app.notice, "auto:MEMORY.md (5 B):\nindex") {
		t.Fatalf("show by label: %q", app.notice)
	}
	app.submit(ctx, "/memory 9")
	if !strings.Contains(app.notice, "no such memory file") {
		t.Fatalf("%q", app.notice)
	}
	app.submit(ctx, "/memory edit 2")
	if app.editing == nil || app.editing.label != ".claude/rules/style.md" || app.editor.Text() != "style" {
		t.Fatalf("edit: %+v %q", app.editing, app.editor.Text())
	}
	frame = app.frame(80, 24)
	if !strings.Contains(plain(strings.Join(frame.Extra, "\n")), "editing .claude/rules/style.md · Enter saves") {
		t.Fatalf("banner: %v", frame.Extra)
	}
	typeText(app, ctx, " guide")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.editing != nil || app.notice != "saved .claude/rules/style.md" {
		t.Fatalf("after save: %v %q", app.editing != nil, app.notice)
	}
	// An auto-memory file edits under its scope; a new workspace path is
	// created; a path the service refuses keeps the draft.
	app.submit(ctx, "/memory edit auto:MEMORY.md")
	typeText(app, ctx, "!")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	app.submit(ctx, "/memory edit .claude/rules/new.md")
	if app.editing == nil || app.editor.Text() != "" {
		t.Fatalf("new file: %+v %q", app.editing, app.editor.Text())
	}
	typeText(app, ctx, "fresh")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	app.submit(ctx, "/memory edit README.md")
	typeText(app, ctx, "nope")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.editing == nil || app.editor.Text() != "nope" || !strings.Contains(app.notice, "a workspace memory file is") {
		t.Fatalf("refused save: %v %q %q", app.editing != nil, app.editor.Text(), app.notice)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if app.editing != nil || app.editor.Text() != "" {
		t.Fatal("Ctrl+C did not cancel the edit")
	}
	f.mu.Lock()
	writes := strings.Join(f.writes, "|")
	f.mu.Unlock()
	if writes != "workspace:.claude/rules/style.md=style guide|auto:MEMORY.md=index!|workspace:.claude/rules/new.md=fresh" {
		t.Fatalf("writes %q", writes)
	}
	// The commands are in the menu.
	app.editor.Set("/mem")
	app.refreshMenu(ctx)
	if app.menu == nil || len(app.menu.Items) != 1 || !strings.HasPrefix(app.menu.Items[0].Label, "/memory ") {
		t.Fatalf("menu %+v", app.menu)
	}
}

func TestCostForkStyleAndBellCommands(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Long tail", Provider: "claude", Status: "idle", Conversation: Conversation{
		Entries: []Entry{
			{ID: "u1", Role: "user", Text: "Add a counter", TurnID: ptr("t1")},
			{ID: "a1", Role: "assistant", Text: "Done.", TurnID: ptr("t1")},
			{ID: "u2", Role: "user", Text: "Make it blue", TurnID: ptr("t2")},
			{ID: "a2", Role: "assistant", Text: "Blue.", TurnID: ptr("t2")},
		},
		Turns: []Turn{
			{ID: "t1", StartedAt: 100, EndedAt: 112, Usage: &Usage{Input: 10000, Cached: 8000, CacheWrite: 500, Output: 300, Reasoning: 100, Total: 10300, CostUSD: 0.03}},
			{ID: "t2", StartedAt: 200, EndedAt: 260, Usage: &Usage{Input: 20000, Cached: 15000, Output: 700, Total: 20700, CostUSD: 0.05}},
		},
	}}}})
	ctx := context.Background()
	dir := t.TempDir()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }, BellFile: filepath.Join(dir, "bell")}
	app.refreshState(ctx)
	// /cost sums the turn records into a notice.
	app.submit(ctx, "/cost")
	notice := plain(app.notice)
	for _, want := range []string{"cost so far", "turns                2", "input tokens         30k", "  read from cache    23k", "  written to cache   500", "output tokens        1.0k", "  thinking           100", "total tokens         31k", "cost                 $0.08", "time in turns        1m 12s"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("/cost lacks %q:\n%s", want, notice)
		}
	}
	codex := SessionCost(&Chat{Conversation: Conversation{Turns: []Turn{{ID: "t", StartedAt: 1, EndedAt: 2, Usage: &Usage{Input: 10, Output: 5, Total: 15}}}}}, 0)
	if codex.Priced || !strings.Contains(strings.Join(CostLines(codex), "\n"), "not reported by this agent") {
		t.Fatalf("codex cost: %+v", codex)
	}
	// /style shows and sets the output style.
	app.submit(ctx, "/style")
	if !strings.Contains(app.notice, "output style default for the next session") {
		t.Fatalf("style: %q", app.notice)
	}
	app.submit(ctx, "/style nope")
	if app.notice != "/style default|Explanatory|Learning" {
		t.Fatalf("bad style: %q", app.notice)
	}
	app.submit(ctx, "/style learning")
	if !strings.Contains(app.notice, "output style Learning applies when the next session starts") || app.chat().OutputStyle != "Learning" {
		t.Fatalf("set style: %q %q", app.notice, app.chat().OutputStyle)
	}
	app.submit(ctx, "/style default")
	if app.chat().OutputStyle != "" {
		t.Fatalf("default style: %q", app.chat().OutputStyle)
	}
	// /bell shows, sets and persists.
	app.submit(ctx, "/bell")
	if !strings.HasPrefix(app.notice, "bell on:") {
		t.Fatalf("bell: %q", app.notice)
	}
	app.submit(ctx, "/bell off")
	if app.bellOn() || app.notice != "bell off" {
		t.Fatalf("bell off: %v %q", app.bellOn(), app.notice)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "bell")); err != nil || strings.TrimSpace(string(b)) != "off" {
		t.Fatalf("bell file: %q %v", b, err)
	}
	fresh := &App{BellFile: filepath.Join(dir, "bell")}
	if fresh.bellOn() {
		t.Fatal("the bell setting was not read back")
	}
	app.submit(ctx, "/bell maybe")
	if app.notice != "/bell on|off" {
		t.Fatalf("bad bell: %q", app.notice)
	}
	app.submit(ctx, "/bell on")
	// /fork lists, refuses bad arguments, forks and switches to the fork.
	app.submit(ctx, "/fork")
	if !strings.Contains(plain(app.notice), " 2   Make it blue") || !strings.Contains(app.notice, "/fork N") {
		t.Fatalf("fork listing: %q", app.notice)
	}
	for _, bad := range []string{"/fork 3", "/fork x"} {
		app.submit(ctx, bad)
		if app.notice != "/fork N (from /fork) or /fork all" {
			t.Fatalf("%s: %q", bad, app.notice)
		}
	}
	app.submit(ctx, "/fork 2")
	if app.ChatID != "chat1-fork" || !strings.Contains(app.notice, `forked before message 2 "Make it blue" into "Long tail (fork)"; the agent continues from a copy of its session`) {
		t.Fatalf("fork: chat %s notice %q", app.ChatID, app.notice)
	}
	fork := app.chat()
	if fork == nil || len(fork.Conversation.Entries) != 3 || fork.Conversation.Entries[2].Role != "fork" {
		t.Fatalf("fork chat: %+v", fork)
	}
	lines := plain(strings.Join(RenderTranscript(fork, 100, false), "\n"))
	if !strings.Contains(lines, "⑂ Forked from “Long tail”") || !strings.Contains(lines, "copy of its session") {
		t.Fatalf("fork marker: %s", lines)
	}
	app.selectChat("chat1")
	app.submit(ctx, "/fork all")
	if !strings.HasPrefix(app.notice, "forked the whole conversation into") {
		t.Fatalf("fork all: %q", app.notice)
	}
	app.selectChat("chat1")
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/fork 1")
	if app.notice != "stop the agent first (Esc)" {
		t.Fatalf("fork while running: %q", app.notice)
	}
}

func TestSideQuestionAndItsCard(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{
		{ID: "chat1", Title: "Aside", Provider: "claude", Status: "idle"},
		{ID: "chat2", Title: "Codex", Provider: "codex", Status: "idle"},
	}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }, later: make(chan func(context.Context), 8)}
	app.refreshState(ctx)
	app.submit(ctx, "/btw")
	if !strings.HasPrefix(app.notice, "/btw QUESTION") {
		t.Fatalf("usage: %q", app.notice)
	}
	app.submit(ctx, "/btw what is the answer?")
	if !strings.HasPrefix(app.notice, "asking a copy of the session: what is the answer?") {
		t.Fatalf("asking: %q", app.notice)
	}
	select {
	case report := <-app.later:
		report(ctx)
	case <-time.After(3 * time.Second):
		t.Fatal("no report")
	}
	if app.notice != "side question answered ($0.03)" {
		t.Fatalf("answered: %q", app.notice)
	}
	app.refreshState(ctx)
	c := app.chat()
	if len(c.Conversation.Entries) != 1 || c.Conversation.Entries[0].Role != "aside" {
		t.Fatalf("entries: %+v", c.Conversation.Entries)
	}
	lines := plain(strings.Join(RenderTranscript(c, 100, false), "\n"))
	for _, want := range []string{"you (aside) › what is the answer?", "claude (aside) › Forty-two.", "2.6s · 1.2k tokens · $0.03 · not sent to the agent"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("aside card lacks %q:\n%s", want, lines)
		}
	}
	running := RenderTranscript(&Chat{Provider: "claude", Conversation: Conversation{Entries: []Entry{{ID: "x", Role: "aside", Text: "hm?", IsStreaming: true, Aside: &Aside{Status: "running"}}}}}, 80, false)
	if !strings.Contains(plain(strings.Join(running, "\n")), "⋯ answering from a copy of the session") {
		t.Fatalf("running aside: %v", running)
	}
	failed := RenderTranscript(&Chat{Provider: "claude", Conversation: Conversation{Entries: []Entry{{ID: "x", Role: "aside", Text: "hm?", Aside: &Aside{Status: "failed", Error: "API Error: 503"}}}}}, 80, false)
	if !strings.Contains(plain(strings.Join(failed, "\n")), "! could not answer: API Error: 503") {
		t.Fatalf("failed aside: %v", failed)
	}
	// Refused while running, and for Codex.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.refreshState(ctx)
	app.submit(ctx, "/btw now?")
	if !strings.HasPrefix(app.notice, "wait for the agent's turn") {
		t.Fatalf("running: %q", app.notice)
	}
	app.selectChat("chat2")
	app.submit(ctx, "/btw hm?")
	if app.notice != "side questions are a Claude chat's" {
		t.Fatalf("codex: %q", app.notice)
	}
}

func TestTitleAndBellEvents(t *testing.T) {
	if TitleFor(nil) != "Warden" {
		t.Fatal(TitleFor(nil))
	}
	idle := &Chat{ID: "c", Title: "Fix  the\nbuild", Status: "idle"}
	running := &Chat{ID: "c", Title: "Fix the build", Status: "running"}
	asking := &Chat{ID: "c", Title: "Fix the build", Status: "running", Approvals: []Approval{{ID: "ap1", State: "pending"}}}
	failed := &Chat{ID: "c", Title: "Fix the build", Status: "failed", Error: "boom"}
	if got := TitleFor(idle); got != "Warden · Fix the build · idle" {
		t.Fatalf("%q", got)
	}
	if got := TitleFor(running); got != "Warden · Fix the build · running" {
		t.Fatalf("%q", got)
	}
	if got := TitleFor(asking); got != "Warden · Fix the build · approval" {
		t.Fatalf("%q", got)
	}
	if got := BellEvents(running, idle); len(got) != 1 || got[0] != "completed" {
		t.Fatalf("%v", got)
	}
	if got := BellEvents(running, failed); len(got) != 1 || got[0] != "failed" {
		t.Fatalf("%v", got)
	}
	if got := BellEvents(running, asking); len(got) != 1 || got[0] != "approval" {
		t.Fatalf("%v", got)
	}
	if got := BellEvents(asking, asking); len(got) != 0 {
		t.Fatalf("a known approval rang again: %v", got)
	}
	if got := BellEvents(idle, running); len(got) != 0 {
		t.Fatalf("a start rang: %v", got)
	}
	if got := BellEvents(running, &Chat{ID: "c", Status: "interrupted"}); len(got) != 0 {
		t.Fatalf("a stop rang: %v", got)
	}
	if got := BellEvents(nil, idle); len(got) != 0 {
		t.Fatalf("the first snapshot rang: %v", got)
	}
	if got := BellEvents(running, &Chat{ID: "other", Status: "idle"}); len(got) != 0 {
		t.Fatalf("another chat rang: %v", got)
	}
	// The app writes the title once per change and rings the bell on the
	// events, not when it is off.
	var out bytes.Buffer
	app := &App{Output: &out, ChatID: "c", state: &State{Chats: []*Chat{running}}, Size: func() (int, int) { return 80, 24 }}
	app.bellForEvents()
	app.draw()
	if !strings.Contains(out.String(), "\x1b]0;Warden · Fix the build · running\a") || strings.Count(out.String(), "\x1b]0;") != 1 {
		t.Fatalf("title: %q", out.String())
	}
	out.Reset()
	app.draw()
	if strings.Contains(out.String(), "\x1b]0;") {
		t.Fatal("the title was rewritten without a change")
	}
	app.state = &State{Chats: []*Chat{idle}}
	app.bellForEvents()
	app.draw()
	if !strings.Contains(out.String(), "\a\x1b") && !strings.HasPrefix(out.String(), "\a") || !strings.Contains(out.String(), "\x1b]0;Warden · Fix the build · idle\a") {
		t.Fatalf("bell and title on completion: %q", out.String())
	}
	out.Reset()
	app.setBell(false)
	app.state = &State{Chats: []*Chat{running}}
	app.bellForEvents()
	app.state = &State{Chats: []*Chat{idle}}
	app.bellForEvents()
	if strings.Contains(out.String(), "\a") {
		t.Fatal("the bell rang while off")
	}
}

// `A` (or /allow) on a tool ask allows it always for the whole workspace
// where `a` does for the chat; /rules lists, adds and removes the
// workspace's rules and the chats' by number; /permissions lists how the
// chat's asks were decided.
func TestWorkspaceAllowRulesAndPermissions(t *testing.T) {
	c := &Chat{ID: "chat1", Title: "Claude", Provider: "claude", Status: "running", Mode: "ask", SandboxID: "sbx1"}
	command := &Entry{ID: "toolu_1", Role: "activity", Text: "touch x", Tool: &Tool{Kind: "command", Name: "Bash", Status: "running"}}
	c.Approvals = []Approval{
		permissionAsk("p1", "Bash", command, map[string]any{"always": "`touch` commands", "rule": "Bash(touch *)"}),
		permissionAsk("p2", "Bash", command, map[string]any{"always": "`touch` commands", "rule": "Bash(touch *)"}),
		permissionAsk("p3", "ExitPlanMode", nil, map[string]any{"plan": "# Plan"}),
	}
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	f.rules = RulesView{Workspace: "sbx1", Rules: []Rule{{ID: "w1", Kind: "deny", Pattern: "Bash(rm *)", Origin: "editor", By: &Actor{PrincipalID: "owner"}}}, Chats: []ChatRules{{ID: "chat1", Title: "Claude", Rules: []Rule{{ID: "c1", Kind: "allow", Pattern: "Bash(touch *)", Origin: "always", By: &Actor{Name: "Dan"}}}}}}
	f.events = []PermissionEvent{
		{At: 1_000_000, Tool: "Bash", Summary: "rm -rf build", Decision: "deny", How: "rule", Scope: "workspace", Rule: &Rule{Kind: "deny", Pattern: "Bash(rm *)"}},
		{At: 1_000_060, Tool: "Write", Summary: "notes.md", Decision: "allow", How: "card", By: &Actor{Name: "Dan"}, Rule: &Rule{Pattern: "Edit"}, Scope: "workspace"},
		{At: 1_000_120, Tool: "Bash", Summary: "touch y", Decision: "allow", How: "auto"},
		{At: 1_000_180, Tool: "Bash", Summary: "curl x", Decision: "deny", How: "card", By: &Actor{PrincipalID: "owner"}, Message: "use the proxy"},
	}
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard}
	ctx := context.Background()
	refresh := func() { s, _ := app.Client.State(ctx); app.state = s }
	refresh()
	app.submit(ctx, "A")
	refresh()
	if got := app.state.Chats[0].Approvals[0]; got.State != "allowed" || got.Params["always"] != true || got.Params["scope"] != "workspace" || app.notice != "allowed always for this workspace: `touch` commands" {
		t.Fatalf("A: %+v %q", got, app.notice)
	}
	app.submit(ctx, "/allow")
	refresh()
	if got := app.state.Chats[0].Approvals[1]; got.State != "allowed" || got.Params["scope"] != "workspace" {
		t.Fatalf("/allow: %+v %q", got, app.notice)
	}
	// A plan is never remembered: A is not an answer to it, /allow finds
	// no tool ask.
	app.submit(ctx, "/allow")
	if app.notice != "no tool ask pending" {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/rules")
	listing := plain(app.notice)
	for _, want := range []string{"workspace rules", " 1  deny  Bash(rm *)", "from the editor by the owner", "chat Claude (this chat)", " 2  allow Bash(touch *)", "allow always by Dan", "/rules add allow|deny|ask PATTERN"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("missing %q in:\n%s", want, listing)
		}
	}
	app.submit(ctx, `/rules add ask "Edit(src/**)"`)
	if app.notice != "workspace rule added: ask Edit(src/**)" || len(f.rules.Rules) != 2 || f.rules.Rules[1].Pattern != "Edit(src/**)" {
		t.Fatalf("%q %+v", app.notice, f.rules.Rules)
	}
	app.submit(ctx, "/rules add deny nonsense")
	if !strings.Contains(app.notice, "missing the tool name") {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/rules add")
	if !strings.HasPrefix(app.notice, "/rules add allow|deny|ask PATTERN") {
		t.Fatal(app.notice)
	}
	// rm by the listing's numbers: 3 is the chat's rule (after the two
	// workspace rules), and the listing is fetched when stale.
	app.submit(ctx, "/rules rm 3")
	if app.notice != "rule 3 removed" || len(f.rules.Chats[0].Rules) != 0 || len(f.rules.Rules) != 2 {
		t.Fatalf("%q %+v", app.notice, f.rules)
	}
	app.submit(ctx, "/rules rm 9")
	if app.notice != "/rules rm N with N from /rules" {
		t.Fatal(app.notice)
	}
	app.submit(ctx, "/rules rm 1")
	if app.notice != "rule 1 removed" || len(f.rules.Rules) != 1 || f.rules.Rules[0].Pattern != "Edit(src/**)" {
		t.Fatalf("%q %+v", app.notice, f.rules.Rules)
	}
	app.submit(ctx, "/permissions")
	lines := strings.Split(plain(app.notice), "\n")
	if len(lines) != 4 {
		t.Fatalf("%q", app.notice)
	}
	for i, want := range []string{"deny  Bash         rm -rf build  — workspace rule deny Bash(rm *)", "allow Write        notes.md  — Dan, always for the workspace (Edit)", "allow Bash         touch y  — auto mode", "deny  Bash         curl x  — the owner: use the proxy"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d: %q, want suffix %q", i, lines[i], want)
		}
	}
	f.events = nil
	app.submit(ctx, "/permissions")
	if app.notice != "no tool asks decided in this chat yet" {
		t.Fatal(app.notice)
	}
	// The / menu offers the commands.
	if !strings.Contains(strings.Join(commandNames(), " "), "rules permissions allow") {
		t.Fatal(commandNames())
	}
}

func commandNames() []string {
	var out []string
	for _, c := range Commands {
		out = append(out, c.Name)
	}
	return out
}

// The /model menu lists the provider's rows: the CLI's catalog when the
// service reported one (the default row first, a 1M row the operator has
// not allowed with its hint), else the static rows; picking the default
// row sends the provider default. /effort lists the model's levels from
// the catalog, and a model without any refuses a level.
func TestModelAndEffortMenusFromTheCatalog(t *testing.T) {
	claude := &Chat{ID: "c1", Title: "Claude", Provider: "claude", Model: "haiku", Status: "idle"}
	codex := &Chat{ID: "c2", Title: "Codex", Provider: "codex", Status: "idle"}
	catalog := []ModelInfo{
		{Value: "default", Resolved: "claude-sonnet-5", Label: "Default (recommended)", Description: "Sonnet 5 · Efficient", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, AdaptiveThinking: true},
		{Value: "sonnet", Resolved: "claude-sonnet-5", Label: "Sonnet", Description: "Sonnet 5 · Efficient", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, AdaptiveThinking: true},
		{Value: "opus[1m]", Resolved: "claude-opus-5[1m]", Label: "Opus (1M context)", Efforts: []string{"low", "max"}, FastMode: true},
		{Value: "haiku", Resolved: "claude-haiku-4-5", Label: "Haiku"},
	}
	f := newFakeServer(t, State{Chats: []*Chat{claude, codex}, AgentOptions: AgentOptions{Models: map[string][]ModelInfo{"claude": catalog}}})
	app := &App{Client: f.client(), ChatID: "c1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/model ")
	if app.menu == nil || app.menu.Trigger.Kind != "model" || len(app.menu.Items) != 4 {
		t.Fatalf("model menu: %+v", app.menu)
	}
	items := app.menu.Items
	if items[0].Insert != "default" || !strings.Contains(items[0].Label, "Provider default") || !strings.Contains(items[0].Hint, "claude-sonnet-5") {
		t.Fatalf("default row: %+v", items[0])
	}
	if items[1].Insert != "sonnet" || !strings.Contains(items[1].Label, "Sonnet") || items[2].Insert != "opus[1m]" || items[2].Hint != LongContextHint || items[3].Insert != "haiku" {
		t.Fatalf("rows: %+v", items)
	}
	// Typing narrows by value or label; Enter picks and runs.
	typeText(app, ctx, "son")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Insert != "sonnet" {
		t.Fatalf("narrowed: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyEnter})
	app.state, _ = app.Client.State(ctx)
	if app.state.Chats[0].Model != "sonnet" || app.editor.Text() != "" {
		t.Fatalf("picked: %q draft %q", app.state.Chats[0].Model, app.editor.Text())
	}
	// The default row sends "" (the provider default).
	typeText(app, ctx, "/model def")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	app.state, _ = app.Client.State(ctx)
	if app.state.Chats[0].Model != "" {
		t.Fatalf("default row sent %q", app.state.Chats[0].Model)
	}
	// With the operator's leave the 1M row has its own hint.
	f.mu.Lock()
	f.state.AgentOptions.LongContext = true
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/model 1m")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Hint == LongContextHint || !strings.Contains(app.menu.Items[0].Hint, "claude-opus-5[1m]") {
		t.Fatalf("allowed 1M row: %+v", app.menu)
	}
	app.editor.Set("")
	app.menu = nil
	// /effort on the default model lists the default row's levels; on
	// Haiku (no levels) it refuses one.
	typeText(app, ctx, "/effort ")
	if app.menu == nil || app.menu.Trigger.Kind != "effort" || len(app.menu.Items) != 6 || app.menu.Items[0].Insert != "default" || app.menu.Items[5].Insert != "max" {
		t.Fatalf("effort menu: %+v", app.menu)
	}
	app.editor.Set("")
	app.menu = nil
	app.submit(ctx, "/model haiku")
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/effort ")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Insert != "default" {
		t.Fatalf("effort menu on haiku: %+v", app.menu)
	}
	app.editor.Set("")
	app.menu = nil
	app.submit(ctx, "/effort high")
	if !strings.Contains(app.notice, "takes no effort level") {
		t.Fatalf("effort on haiku: %q", app.notice)
	}
	app.submit(ctx, "/effort")
	if !strings.Contains(app.notice, "takes no effort level") {
		t.Fatalf("effort on haiku: %q", app.notice)
	}
	// Without a catalog the static rows serve, every level offered.
	f.mu.Lock()
	f.state.AgentOptions.Models = nil
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	typeText(app, ctx, "/model ")
	if app.menu == nil || len(app.menu.Items) != 6 || app.menu.Items[1].Insert != "sonnet" || app.menu.Items[4].Insert != "sonnet[1m]" {
		t.Fatalf("static rows: %+v", app.menu)
	}
	if levels := EffortsFor(app.chat(), app.agentOptions()); len(levels) != 5 {
		t.Fatalf("static efforts: %v", levels)
	}
	app.editor.Set("")
	app.menu = nil
	// Codex has its own static rows.
	app.selectChat("c2")
	typeText(app, ctx, "/model ")
	if app.menu == nil || len(app.menu.Items) != 6 || app.menu.Items[1].Insert != "gpt-6-astra" {
		t.Fatalf("codex rows: %+v", app.menu)
	}
	app.editor.Set("")
	app.menu = nil
	typeText(app, ctx, "/effort ")
	if app.menu != nil {
		t.Fatalf("effort menu on codex: %+v", app.menu)
	}
}

// /cost ends with the workspace's total when the chat shares it: every
// chat's turns summed, archived ones included.
func TestCostShowsTheWorkspaceTotal(t *testing.T) {
	turns := func(cost float64, total int64) Conversation {
		return Conversation{Turns: []Turn{{ID: "t", StartedAt: 1, EndedAt: 2, Usage: &Usage{Input: total - 10, Output: 10, Total: total, CostUSD: cost}}}}
	}
	f := newFakeServer(t, State{Chats: []*Chat{
		{ID: "a", Title: "A", Provider: "claude", SandboxID: "ws", Status: "idle", Conversation: turns(0.10, 1000)},
		{ID: "b", Title: "B", Provider: "claude", SandboxID: "ws", Status: "idle", Archived: true, Conversation: turns(0.20, 2000)},
		{ID: "c", Title: "C", Provider: "claude", SandboxID: "other", Status: "idle", Conversation: turns(5, 50000)},
	}})
	app := &App{Client: f.client(), ChatID: "a", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.refreshState(ctx)
	app.submit(ctx, "/cost")
	notice := plain(app.notice)
	if !strings.Contains(notice, "cost                 $0.10") || !strings.Contains(notice, "this workspace: 2 chats · 2 turns · 3.0k tokens · $0.30") {
		t.Fatalf("/cost:\n%s", notice)
	}
	// Alone on its workspace: no total line.
	app.selectChat("c")
	app.submit(ctx, "/cost")
	if strings.Contains(plain(app.notice), "this workspace") {
		t.Fatalf("/cost alone:\n%s", app.notice)
	}
	codex, n := WorkspaceCost(&Chat{SandboxID: "x"}, []*Chat{{SandboxID: "x", Conversation: turns(0, 15)}, {SandboxID: "x"}}, 0)
	if n != 2 || codex.Priced || WorkspaceCostLine(codex, n) != "this workspace: 2 chats · 1 turn · 15 tokens" {
		t.Fatalf("codex: %q", WorkspaceCostLine(codex, n))
	}
}

// The status bar keeps every part on a narrow terminal by taking more
// rows, never splitting a part, and the transcript gives those rows up.
func TestStatusWrapsToRows(t *testing.T) {
	parts := []string{"● " + bold + "title" + reset, "claude · opus · auto", yellow + "running" + reset, "3.3s · 12k tokens (12k in, 226 out) · $0.20", "ctx 46k/200k (23%)", dim + "/help" + reset}
	one := LayoutStatus(parts, 200, StatusMaxRows)
	if len(one) != 1 || plain(one[0]) != strings.Join([]string{"● title", "claude · opus · auto", "running", "3.3s · 12k tokens (12k in, 226 out) · $0.20", "ctx 46k/200k (23%)", "/help"}, "  ") {
		t.Fatalf("wide layout: %q", one)
	}
	rows := LayoutStatus(parts, 48, StatusMaxRows)
	if len(rows) != 3 {
		t.Fatalf("48 columns: want 3 rows, got %d: %q", len(rows), rows)
	}
	for i, r := range rows {
		if w := visibleWidth(r); w > 48 {
			t.Fatalf("row %d is %d wide: %q", i, w, plain(r))
		}
	}
	joined := plain(strings.Join(rows, "\n"))
	for _, part := range parts {
		if !strings.Contains(joined, plain(part)) {
			t.Fatalf("part %q lost:\n%s", plain(part), joined)
		}
	}
	// A part wider than the row stands alone; the cap drops the tail.
	capped := LayoutStatus(parts, 20, 2)
	if len(capped) != 2 || plain(capped[0]) != "● title" || plain(capped[1]) != "claude · opus · auto" {
		t.Fatalf("capped layout: %q", capped)
	}
	if got := LayoutStatus(nil, 40, 2); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty layout: %q", got)
	}
}

func TestFrameBudgetsStatusRows(t *testing.T) {
	app := &App{Now: func() time.Time { return time.Unix(100, 0) }}
	c := sampleChat()
	c.Status = "idle"
	c.Conversation.Turns = []Turn{{ID: "t1", StartedAt: 90, EndedAt: 93.3, Usage: &Usage{Input: 12000, Output: 226, Total: 12226, CostUSD: 0.1975}}}
	c.Conversation.Context = &Context{Used: 46449, Window: 200000, Threshold: 167000, Model: "claude-sonnet-5"}
	for i := 0; i < 40; i++ {
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: fmt.Sprint("e", i), Role: "assistant", Text: fmt.Sprintf("line %d", i), TurnID: ptr("t1")})
	}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "chat1"
	app.live = true
	wide := app.frame(200, 24)
	if len(wide.Status) != 1 || !strings.Contains(plain(strings.Join(wide.Lines, "\n")), "bind sandbox port") || len(wide.Lines)+len(wide.Status)+len(wide.PromptLines) > 23 {
		t.Fatalf("wide frame: %d status rows, live lines %q", len(wide.Status), wide.Lines)
	}
	narrow := app.frame(50, 24)
	if len(narrow.Status) < 2 || len(narrow.Status) > StatusMaxRows {
		t.Fatalf("narrow frame: %d status rows: %q", len(narrow.Status), narrow.Status)
	}
	if tail := len(narrow.Lines) + len(narrow.Notice) + len(narrow.Status) + len(narrow.PromptLines) + len(narrow.Extra); tail > 23 {
		t.Fatalf("narrow frame's tail is taller than the screen: %d lines, %d status, %d prompt, %d extra", len(narrow.Lines), len(narrow.Status), len(narrow.PromptLines), len(narrow.Extra))
	}
	joined := plain(strings.Join(narrow.Status, "\n"))
	for _, want := range []string{"Local preview test", "codex", "12k tokens", "$0.20", "ctx 46k/200k (23%)", "1 approval", "/help"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("narrow status lost %q:\n%s", want, joined)
		}
	}
	for i, r := range narrow.Status {
		if w := visibleWidth(r); w > 50 {
			t.Fatalf("status row %d is %d wide: %q", i, w, plain(r))
		}
	}
	// A short screen caps the bar at a quarter of its rows.
	short := app.frame(30, 8)
	if len(short.Status) != 2 {
		t.Fatalf("short screen: %d status rows: %q", len(short.Status), short.Status)
	}
}

// Styling takes no columns: a styled line wraps where its plain text
// would, and a style that is open at a break carries onto the next line.
func TestWrapMeasuresVisibleWidth(t *testing.T) {
	head := "Write hello.txt  " + green + "+1" + reset + " " + red + "−0" + reset
	got := wrap(head, 50, dim+"  · "+reset, "    ")
	if len(got) != 1 || plain(got[0]) != "  · Write hello.txt  +1 −0" {
		t.Fatalf("styled head wrapped: %q", got)
	}
	plainWords := strings.Repeat("word ", 20)
	styledWords := yellow + plainWords + reset
	p, s := wrap(strings.TrimSpace(plainWords), 30, "  ", "  "), wrap(strings.TrimSpace(styledWords), 30, "  ", "  ")
	if len(p) != len(s) {
		t.Fatalf("styled text wraps differently: %d vs %d lines\n%q\n%q", len(p), len(s), p, s)
	}
	for i := range p {
		if plain(s[i]) != p[i] {
			t.Fatalf("line %d differs: %q vs %q", i, plain(s[i]), p[i])
		}
		if w := visibleWidth(s[i]); w > 30 {
			t.Fatalf("line %d is %d wide", i, w)
		}
	}
	if !strings.HasPrefix(s[1], "  "+yellow) || !strings.HasSuffix(s[0], reset) {
		t.Fatalf("style not carried across the break: %q", s)
	}
	// A single over-long styled token is cut by visible runes.
	long := cyan + strings.Repeat("x", 40) + reset
	cut := wrap(long, 20, "", "")
	if len(cut) != 2 || plain(cut[0]) != strings.Repeat("x", 20) || plain(cut[1]) != strings.Repeat("x", 20) {
		t.Fatalf("over-long token: %q", cut)
	}
}

// Widths are columns, not runes: wide East Asian text and emoji take two,
// combining marks and variation selectors none, a tab is expanded, so a
// line the painter counts as one row is one row on the terminal.
func TestColumnWidths(t *testing.T) {
	cases := map[string]int{
		"abc":         3,
		"日本語":         6,
		"é":          1, // e + combining acute
		"👍":           2,
		"👍🏽":          2, // skin tone modifier
		"⚠":           1, // a symbol without VS16
		"⚠️":          2, // with VS16: the emoji picture
		"✓ ok":        4,
		"a‍b":         2, // zero-width joiner
		"\x1b[31mred": 3,
	}
	for s, want := range cases {
		if got := visibleWidth(s); got != want {
			t.Errorf("width(%q) = %d, want %d", s, got, want)
		}
	}
	if got := sanitize("1\talpha"); got != "1    alpha" {
		t.Errorf("tab: %q", got)
	}
	// wrap breaks by columns; clip cuts by columns and keeps a wide rune whole.
	lines := wrap("日本語 日本語 日本語", 13, "", "")
	if len(lines) != 2 || lines[0] != "日本語 日本語" {
		t.Errorf("wrap wide: %q", lines)
	}
	if got := plain(clip("ab日本語", 4)); got != "ab日" {
		t.Errorf("clip wide: %q", got)
	}
	if got := plain(clip("abcdef", 3)); got != "abc" {
		t.Errorf("clip: %q", got)
	}
}

// The todo list is a live panel at the bottom of the transcript, never
// committed, above the queued messages; it goes away once the chat is
// idle with every item done.
func TestTodoListIsALivePanel(t *testing.T) {
	c := &Chat{ID: "c", Title: "Todo", Provider: "claude", Status: "running"}
	c.Conversation.Entries = []Entry{
		{ID: "u1", Role: "user", Text: "plan it", Delivery: "sent"},
		{ID: "todo", Role: "activity", Text: "Todo list · 1 of 2 done", Detail: "[x] Parse\n[>] Test\n", Tool: &Tool{Kind: "todo", Status: "completed"}},
		{ID: "a1", Role: "activity", Text: "go test", Detail: "ok", Tool: &Tool{Kind: "command", Status: "completed"}},
		{ID: "u2", Role: "user", Text: "later", Delivery: "queued"},
	}
	blocks := RenderBlocks(c, 80, false)
	if len(blocks) != 4 || !blocks[0].Final || !blocks[1].Final || blocks[2].Final || blocks[3].Final {
		t.Fatalf("blocks: %+v", blocks)
	}
	if got := plain(strings.Join(blocks[2].Lines, "\n")); !strings.Contains(got, "Todo list") || !strings.Contains(got, "▸ Test") {
		t.Fatalf("todo panel:\n%s", got)
	}
	if got := plain(strings.Join(blocks[3].Lines, "\n")); !strings.Contains(got, "you › later") {
		t.Fatalf("queued last:\n%s", got)
	}
	// Every write replaces the entry in place without a reprint: the
	// committed prefix (the message and the command) is unchanged.
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }, Output: io.Discard, Size: func() (int, int) { return 80, 24 }}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "c"
	app.draw()
	var out bytes.Buffer
	app.Output = &out
	c.Conversation.Entries[1].Detail = "[x] Parse\n[x] Test\n"
	c.Conversation.Entries[1].Text = "Todo list · 2 of 2 done"
	app.draw()
	if strings.Contains(out.String(), "\x1b[2J") || !strings.Contains(plain(out.String()), "2 of 2 done") {
		t.Fatalf("todo write:\n%q", out.String())
	}
	// Idle with everything done: the panel goes, nothing is reprinted.
	out.Reset()
	c.Status = "idle"
	c.Conversation.Entries = c.Conversation.Entries[:3]
	app.draw()
	if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(plain(out.String()), "Todo list") {
		t.Fatalf("idle and done:\n%q", out.String())
	}
	if !todoDone(c.Conversation.Entries[1]) || todoDone(Entry{Detail: "[ ] one\n"}) {
		t.Fatal("todoDone")
	}
}

// A burst of resize events (a window being dragged) reprints the
// transcript once, after it settles, and only when the size changed.
func TestResizeBurstReprintsOnce(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Conversation.Entries[2].IsStreaming = false
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	var mu sync.Mutex
	var out bytes.Buffer
	w, h := 100, 30
	size := func() (int, int) { mu.Lock(); defer mu.Unlock(); return w, h }
	setSize := func(nw, nh int) { mu.Lock(); w, h = nw, nh; mu.Unlock() }
	pr, pw := io.Pipe()
	resize := make(chan struct{}, 1)
	app := &App{Client: f.client(), ChatID: "chat1", Input: pr, Output: &syncWriter{w: &out, mu: &mu}, Size: size, Resize: resize, Now: time.Now}
	old := resizeSettle
	resizeSettle = 30 * time.Millisecond
	defer func() { resizeSettle = old }()
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background()) }()
	time.Sleep(150 * time.Millisecond)
	count := func() int { mu.Lock(); defer mu.Unlock(); return strings.Count(out.String(), "\x1b[2J") }
	if count() != 0 {
		t.Fatal("the first paint cleared the screen")
	}
	setSize(80, 24)
	for i := 0; i < 3; i++ {
		resize <- struct{}{}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("a resize burst reprinted %d times", n)
	}
	resize <- struct{}{} // the same size again: nothing to reprint
	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("an unchanged size reprinted: %d", n)
	}
	pw.Write([]byte{0x04}) // Ctrl+D quits
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	mu.Lock()
	final := out.String()
	mu.Unlock()
	for _, mode := range []string{"\x1b[?1049", "\x1b[?1000", "\x1b[?1006", "\x1b[3J"} {
		if strings.Contains(final, mode) {
			t.Fatalf("Run wrote %q", mode)
		}
	}
	if !strings.HasSuffix(final, "\x1b[?2004l\x1b[?25h\x1b[23;0t") || !strings.HasPrefix(final, "\x1b[?2004h\x1b[22;0t") {
		t.Fatalf("Run's modes:\n%q", final)
	}
}

// syncWriter serialises writes to a buffer read by the test.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// A message's delivery shows only when there is something to say: the
// queue marker while held, a red not-delivered line with the reason when
// it failed, nothing while it is being handed over or once it is sent.
func TestDeliveryMarks(t *testing.T) {
	c := &Chat{Provider: "claude", Status: "running"}
	// Lines are joined with single spaces so the check does not depend on
	// where the width wraps the detail.
	render := func(delivery, detail string) string {
		return strings.Join(strings.Fields(plain(strings.Join(renderEntry(c, Entry{ID: "u", Role: "user", Text: "hi", Delivery: delivery, Detail: detail}, 80, false, nil, ""), "\n"))), " ")
	}
	for _, d := range []string{"", "sending", "sent"} {
		if got := render(d, ""); got != "you › hi" {
			t.Fatalf("%q: %q", d, got)
		}
	}
	if got := render("queued", ""); !strings.Contains(got, "queued") {
		t.Fatalf("queued: %q", got)
	}
	if got := render("failed", "Delivery unconfirmed. Check the agent response before retrying."); got != "you › hi ! not delivered: Delivery unconfirmed. Check the agent response before retrying." {
		t.Fatalf("failed: %q", got)
	}
	if got := render("failed", ""); got != "you › hi ! not delivered" {
		t.Fatalf("failed without detail: %q", got)
	}
}

// keysOf feeds vim-notation keys (vim_test.go) to the app.
func keysOf(app *App, ctx context.Context, s string) {
	for _, k := range vimKeys(s) {
		app.handleKey(ctx, k)
	}
}

// Vim mode in the app (R2.16): /vim on is remembered in VimFile; the
// status bar names the mode; Esc enters normal mode, where the keys edit
// the draft, Enter and :w send it (the machine back in insert mode), :q
// quits, :set novim turns it off, "/" searches the transcript and Esc
// with nothing pending is the app's (interrupting a running agent).
func TestVimModeInTheApp(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Approvals = nil
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	dir := t.TempDir()
	vimFile := filepath.Join(dir, "vim")
	now := time.Unix(1000, 0)
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return now }, VimFile: vimFile}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	if app.vimOn() {
		t.Fatal("vim on before /vim on")
	}
	status := func() string { return plain(strings.Join(app.frame(120, 24).Status, " ")) }
	if strings.Contains(status(), "-- INSERT --") {
		t.Fatalf("mode shown while off: %q", status())
	}
	app.submit(ctx, "/vim on")
	if !app.vim.Enabled || !strings.Contains(app.notice, "vim mode on") {
		t.Fatalf("/vim on: %v %q", app.vim.Enabled, app.notice)
	}
	if b, _ := os.ReadFile(vimFile); strings.TrimSpace(string(b)) != "on" {
		t.Fatalf("vim file: %q", b)
	}
	if !strings.Contains(status(), "-- INSERT --") {
		t.Fatalf("insert mode not shown: %q", status())
	}
	// Type, Esc to normal mode, edit with vim keys.
	typeText(app, ctx, "hello there world")
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.vim.Mode != VimNormal || !strings.Contains(status(), "-- NORMAL --") {
		t.Fatalf("normal mode: %v %q", app.vim.Mode, status())
	}
	if app.editor.Cursor() != len("hello there world")-1 {
		t.Fatalf("cursor after Esc: %d", app.editor.Cursor())
	}
	keysOf(app, ctx, "bdw0")
	if app.editor.Text() != "hello there " || app.editor.Cursor() != 0 {
		t.Fatalf("bdw0: %q@%d", app.editor.Text(), app.editor.Cursor())
	}
	keysOf(app, ctx, "cwhi<Esc>")
	if app.editor.Text() != "hi there " {
		t.Fatalf("cw: %q", app.editor.Text())
	}
	// The block cursor is requested in normal mode, the default in insert.
	var out bytes.Buffer
	app.Output = &out
	app.draw()
	if !strings.Contains(out.String(), "\x1b[2 q") {
		t.Fatal("no block cursor in normal mode")
	}
	// The : line shows in the composer; :w sends the draft, then insert mode.
	keysOf(app, ctx, ":w")
	if fr := app.frame(120, 24); len(fr.PromptLines) != 1 || fr.PromptLines[0] != ":w" || fr.CursorCol != 2 {
		t.Fatalf("command line: %+v", fr.PromptLines)
	}
	keysOf(app, ctx, "<CR>")
	time.Sleep(20 * time.Millisecond)
	s, _ := app.Client.State(ctx)
	last := s.Chats[0].Conversation.Entries[len(s.Chats[0].Conversation.Entries)-1]
	if last.Role != "user" || last.Text != "hi there" {
		t.Fatalf(":w did not send: %+v", last)
	}
	if app.vim.Mode != VimInsert || app.editor.Text() != "" {
		t.Fatalf("after :w: mode %v text %q", app.vim.Mode, app.editor.Text())
	}
	out.Reset()
	app.draw()
	if !strings.Contains(out.String(), "\x1b[0 q") {
		t.Fatal("cursor shape not restored in insert mode")
	}
	app.Output = io.Discard
	// Enter in normal mode sends too.
	typeText(app, ctx, "second")
	keysOf(app, ctx, "<Esc><CR>")
	time.Sleep(20 * time.Millisecond)
	s, _ = app.Client.State(ctx)
	if e := s.Chats[0].Conversation.Entries; e[len(e)-1].Text != "second" {
		t.Fatalf("Enter in normal mode: %q", e[len(e)-1].Text)
	}
	// :w with nothing typed says so.
	keysOf(app, ctx, "<Esc>:w<CR>")
	if app.notice != "nothing to send" {
		t.Fatalf(":w on empty: %q", app.notice)
	}
	// "/" searches the transcript (item 6's /find): the matching line is
	// printed.
	app.state, _ = app.Client.State(ctx)
	keysOf(app, ctx, "/counter<CR>")
	if !strings.Contains(app.notice, `contain "counter"`) || !strings.Contains(app.notice, "Create a counter page") {
		t.Fatalf("/ search: %q", app.notice)
	}
	keysOf(app, ctx, "n")
	if !strings.Contains(app.notice, `contain "counter"`) {
		t.Fatalf("n: %q", app.notice)
	}
	// Esc in normal mode with nothing pending is the app's: on a running
	// chat it interrupts the agent.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	if app.vim.Mode != VimNormal {
		t.Fatalf("mode before Esc: %v", app.vim.Mode)
	}
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if !strings.Contains(strings.Join(f.calls, "\n"), "POST chats/chat1/stop") || !strings.Contains(app.notice, "interrupting") {
		t.Fatalf("Esc did not interrupt: %q", app.notice)
	}
	// Esc in insert mode enters normal mode instead of interrupting.
	f.mu.Lock()
	f.state.Chats[0].Status = "running"
	f.calls = nil
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	app.vim.Reset()
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if strings.Contains(strings.Join(f.calls, "\n"), "/stop") || app.vim.Mode != VimNormal {
		t.Fatalf("Esc from insert interrupted or did not switch: %v %v", f.calls, app.vim.Mode)
	}
	// G on an empty draft reprints the chat so the terminal shows its
	// end; gg says the transcript is the terminal's.
	app.redraw = false
	keysOf(app, ctx, "G")
	if !app.redraw || !strings.Contains(app.notice, "reprinted") {
		t.Fatalf("G: redraw %v notice %q", app.redraw, app.notice)
	}
	app.redraw = false
	keysOf(app, ctx, "gg")
	if app.redraw || !strings.Contains(app.notice, "scroll up") {
		t.Fatalf("gg: redraw %v notice %q", app.redraw, app.notice)
	}
	if !strings.Contains(helpText, "vim        /vim on") {
		t.Fatal("help does not describe vim mode")
	}
	// :set novim turns it off and remembers; a new app reads the file.
	keysOf(app, ctx, ":set novim<CR>")
	if app.vim.Enabled || !strings.Contains(app.notice, "vim mode off") {
		t.Fatalf(":set novim: %v %q", app.vim.Enabled, app.notice)
	}
	if b, _ := os.ReadFile(vimFile); strings.TrimSpace(string(b)) != "off" {
		t.Fatalf("vim file after novim: %q", b)
	}
	os.WriteFile(vimFile, []byte("on\n"), 0600)
	again := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, VimFile: vimFile}
	if !again.vimOn() || again.vim.Mode != VimInsert {
		t.Fatal("a new app did not read the vim setting")
	}
	// :q quits.
	again.state, _ = again.Client.State(ctx)
	keysOf(again, ctx, "<Esc>:q<CR>")
	if !again.quit {
		t.Fatal(":q did not quit")
	}
	// /vim with no argument reports; a bad argument is refused.
	app.submit(ctx, "/vim")
	if !strings.Contains(app.notice, "vim mode off") {
		t.Fatalf("/vim: %q", app.notice)
	}
	app.submit(ctx, "/vim sideways")
	if app.notice != "/vim on|off" {
		t.Fatalf("/vim sideways: %q", app.notice)
	}
	// Ctrl-C in normal mode clears the draft and returns to insert mode.
	app.submit(ctx, "/vim on")
	typeText(app, ctx, "draft")
	keysOf(app, ctx, "<Esc><C-c>")
	if app.editor.Text() != "" || app.vim.Mode != VimInsert {
		t.Fatalf("Ctrl-C in normal mode: %q %v", app.editor.Text(), app.vim.Mode)
	}
	// The / menu still completes in insert mode.
	typeText(app, ctx, "/vi")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Label != "/vim [on|off]" {
		t.Fatalf("menu in insert mode: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyEscape}) // closes the menu
	app.handleKey(ctx, Key{Kind: KeyEscape}) // normal mode
	if app.menu != nil || app.vim.Mode != VimNormal {
		t.Fatalf("Esc Esc with a menu: %v %v", app.menu != nil, app.vim.Mode)
	}
}

// /attach takes several paths and globs (R2.17); a local @./path or
// @~/path mention in the draft is attached when the message is sent and
// rewritten to the upload's workspace path; the local prefix completes
// this machine's files while a workspace path still asks the service.
func TestAttachSeveralFilesAndLocalMentions(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Approvals = nil
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, "a.txt"), []byte("aaa"), 0600)
	os.WriteFile(filepath.Join(home, "b.txt"), []byte("bb"), 0600)
	os.WriteFile(filepath.Join(home, "c.md"), []byte("c"), 0600)
	os.WriteFile(filepath.Join(home, "with space.txt"), []byte("sp"), 0600)
	os.MkdirAll(filepath.Join(home, "sub"), 0700)
	os.WriteFile(filepath.Join(home, "sub", "d.txt"), []byte("dddd"), 0600)
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	// Several paths, quotes, a glob and ~.
	app.submit(ctx, `/attach ~/a.txt "~/with space.txt" `+filepath.Join(home, "*.md")+" ~/missing.txt")
	if got := f.uploads; len(got) != 3 || got[0] != "a.txt:aaa" || got[1] != "with space.txt:sp" || got[2] != "c.md:c" {
		t.Fatalf("uploads %v", got)
	}
	if !strings.Contains(app.notice, "attached 3 files: a.txt (file, 3 B), with space.txt (file, 2 B), c.md (file, 1 B)") || !strings.Contains(app.notice, "missing.txt") {
		t.Fatalf("notice %q", app.notice)
	}
	if paths := AttachArgs(`x "y z" 'w'`); strings.Join(paths, "|") != "x|y z|w" {
		t.Fatalf("AttachArgs %v", paths)
	}
	// The count limit stops the batch and says so.
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(home, fmt.Sprintf("n%d.txt", i)), []byte("n"), 0600)
	}
	app.submit(ctx, "/attach ~/n*.txt ~/b.txt")
	if len(app.attachments["chat1"]) != 8 || !strings.Contains(app.notice, "at most 8 attachments") || !strings.Contains(app.notice, "attached 5 files") {
		t.Fatalf("limit: %d %q", len(app.attachments["chat1"]), app.notice)
	}
	app.submit(ctx, "/clear")
	f.mu.Lock()
	f.uploads = nil
	f.mu.Unlock()
	// A local mention in the message attaches the file and is rewritten;
	// a workspace mention and a mention of a missing file stay as written.
	app.submit(ctx, "compare @~/sub/d.txt with @src/app.go and @./nope.txt, then @~/b.txt.")
	time.Sleep(20 * time.Millisecond)
	if got := f.uploads; len(got) != 2 || got[0] != "d.txt:dddd" || got[1] != "b.txt:bb" {
		t.Fatalf("mention uploads %v", got)
	}
	s, _ := app.Client.State(ctx)
	var sent Entry
	for _, e := range s.Chats[0].Conversation.Entries {
		if strings.HasPrefix(e.Text, "compare ") {
			sent = e
		}
	}
	want := "compare @.warden/attachments/00000000000000000000000000000001.bin with @src/app.go and @./nope.txt, then @.warden/attachments/00000000000000000000000000000002.bin."
	if sent.Text != want || len(sent.Attachments) != 2 {
		t.Fatalf("sent %q with %d attachments, want %q", sent.Text, len(sent.Attachments), want)
	}
	if !strings.Contains(app.notice, "attached d.txt (4 B)") || !strings.Contains(app.notice, "no local file ./nope.txt") {
		t.Fatalf("mention notice %q", app.notice)
	}
	if m := LocalMentions("a @./x.txt, @../y @~/z) @src/w @./x.txt"); strings.Join(m, "|") != "./x.txt|../y|~/z" {
		t.Fatalf("LocalMentions %v", m)
	}
	// A local mention completes from this machine, closed with a space;
	// a workspace mention still asks the service.
	typeText(app, ctx, "see @~/su")
	if app.menu == nil || app.menu.Trigger.Kind != "localpath" || len(app.menu.Items) != 1 || app.menu.Items[0].Insert != "@~/sub/" {
		t.Fatalf("local mention menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyTab})
	typeText(app, ctx, "d")
	if app.menu == nil || len(app.menu.Items) != 1 || app.menu.Items[0].Insert != "@~/sub/d.txt " {
		t.Fatalf("local file menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyTab})
	if app.editor.Text() != "see @~/sub/d.txt " {
		t.Fatalf("completed %q", app.editor.Text())
	}
	typeText(app, ctx, "and @sr")
	if app.menu == nil || app.menu.Trigger.Kind != "path" {
		t.Fatalf("workspace mention menu: %+v", app.menu)
	}
	app.editor.Clear()
	app.menu = nil
	// An attachment that cannot be made keeps the draft.
	os.WriteFile(filepath.Join(home, "empty.txt"), nil, 0600)
	app.submit(ctx, "read @~/empty.txt")
	if app.editor.Text() != "read @~/empty.txt" || !strings.Contains(app.notice, "is empty") {
		t.Fatalf("empty mention: %q %q", app.editor.Text(), app.notice)
	}
}

// A collapsed paste can be looked at (R2.17): /paste lists the draft's
// pastes with sizes, /paste N shows one in the pager in place of the
// transcript, Ctrl+P on a placeholder previews it, cycles to the next and
// closes after the last (elsewhere Ctrl+P is the history's), and a chip
// above the status bar lists the pastes while the draft holds them.
func TestPastePreviewAndChip(t *testing.T) {
	c := sampleChat()
	c.Status = "idle"
	c.Approvals = nil
	f := newFakeServer(t, State{Chats: []*Chat{c}})
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.editor.SetHistory([]string{"earlier prompt"})
	app.submit(ctx, "/paste")
	if !strings.Contains(app.notice, "no collapsed paste") {
		t.Fatalf("/paste empty: %q", app.notice)
	}
	var long strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&long, "log line %d\n", i)
	}
	typeText(app, ctx, "see ")
	app.handleKey(ctx, Key{Kind: KeyPaste, Text: long.String()})
	typeText(app, ctx, " and ")
	app.handleKey(ctx, Key{Kind: KeyPaste, Text: strings.Repeat("y", 1200)})
	if app.editor.Text() != "see [Pasted text #1 — 30 lines] and [Pasted text #2 — 1 line]" {
		t.Fatalf("draft %q", app.editor.Text())
	}
	fr := app.frame(120, 30)
	if !strings.Contains(plain(strings.Join(fr.Extra, "\n")), "pasted: #1 30 lines (351 B) · #2 1 line (1 KB)") {
		t.Fatalf("chip: %q", fr.Extra)
	}
	app.submit(ctx, "/paste")
	if !strings.Contains(app.notice, "#1  30 lines, 351 B: log line 1") || !strings.Contains(app.notice, "#2  1 line, 1 KB: yyy") {
		t.Fatalf("/paste listing: %q", app.notice)
	}
	if app.editor.Text() == "" {
		t.Fatal("/paste cleared the draft")
	}
	// /paste 1 prints the whole paste into the scrollback, numbered. Typed
	// as its own draft, the command does not consume the kept pastes.
	app.prints = nil
	app.submit(ctx, "/paste 1")
	if !strings.HasPrefix(app.notice, "Pasted text #1 — 30 lines, 351 B\n   1 log line 1\n   2 log line 2") || !strings.HasSuffix(app.notice, "  30 log line 30") || len(app.prints) != 1 {
		t.Fatalf("/paste 1: %q (%d to print)", app.notice, len(app.prints))
	}
	app.submit(ctx, "/paste 7")
	if !strings.Contains(app.notice, "/paste N with N from /paste (the draft holds 2)") {
		t.Fatalf("/paste 7: %q", app.notice)
	}
	// Ctrl+P on the first placeholder previews its first lines above the
	// status bar, again moves to the second, again closes; off a
	// placeholder it recalls the history.
	app.editor.SetCursor(6)
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	if app.preview == nil || app.preview.paste != 1 {
		t.Fatalf("Ctrl+P: %+v", app.preview)
	}
	extra := plain(strings.Join(app.frame(120, 30).Extra, "\n"))
	if !strings.Contains(extra, "Pasted text #1 — 30 lines, 351 B") || !strings.Contains(extra, "   1 log line 1") || !strings.Contains(extra, "   8 log line 8") || strings.Contains(extra, "log line 9") || !strings.Contains(extra, "… 22 more lines") {
		t.Fatalf("preview: %q", extra)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	if app.preview == nil || app.preview.paste != 2 {
		t.Fatalf("Ctrl+P cycle: %+v", app.preview)
	}
	if extra := plain(strings.Join(app.frame(120, 30).Extra, "\n")); !strings.Contains(extra, "   1 yyyy") || strings.Contains(extra, "more line") {
		t.Fatalf("second paste not shown: %q", extra)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	if app.preview != nil {
		t.Fatal("Ctrl+P did not close after the last paste")
	}
	if app.editor.Text() != "see [Pasted text #1 — 30 lines] and [Pasted text #2 — 1 line]" {
		t.Fatalf("Ctrl+P touched the draft: %q", app.editor.Text())
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.preview != nil {
		t.Fatal("Esc did not close the preview")
	}
	app.editor.SetCursor(0)
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	if app.editor.Text() != "earlier prompt" || app.preview != nil {
		t.Fatalf("Ctrl+P off a placeholder: %q", app.editor.Text())
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlN})
	// The preview closes when the draft is sent, and the message carries
	// the pastes in full.
	app.editor.SetCursor(6)
	app.handleKey(ctx, Key{Kind: KeyCtrlP})
	app.handleKey(ctx, Key{Kind: KeyEnter})
	time.Sleep(20 * time.Millisecond)
	s, _ := app.Client.State(ctx)
	last := s.Chats[0].Conversation.Entries[len(s.Chats[0].Conversation.Entries)-1]
	if app.preview != nil || !strings.HasPrefix(last.Text, "see log line 1\nlog line 2") || !strings.HasSuffix(last.Text, strings.Repeat("y", 1200)) {
		t.Fatalf("sent: preview %v text %.40q", app.preview != nil, last.Text)
	}
	if len(app.frame(120, 30).Extra) != 0 {
		t.Fatal("chip still shown")
	}
	// A command typed as its own draft keeps the pastes; a message sends
	// and drops them; the placeholder typed again names the kept paste.
	app.handleKey(ctx, Key{Kind: KeyPaste, Text: strings.Repeat("z\n", 12)})
	app.editor.Clear()
	app.handleKey(ctx, Key{Kind: KeyPaste, Text: strings.Repeat("z\n", 12)})
	app.editor.Set("")
	app.editor.SetPastes([]string{strings.Repeat("z\n", 12)})
	typeText(app, ctx, "/paste")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if len(app.editor.Pastes()) != 1 || !strings.Contains(app.notice, "#1  12 lines") {
		t.Fatalf("a command consumed the pastes: %d %q", len(app.editor.Pastes()), app.notice)
	}
	typeText(app, ctx, "here: [Pasted text #1 — 12 lines]")
	app.handleKey(ctx, Key{Kind: KeyEnter})
	time.Sleep(20 * time.Millisecond)
	s, _ = app.Client.State(ctx)
	last = s.Chats[0].Conversation.Entries[len(s.Chats[0].Conversation.Entries)-1]
	if last.Text != "here: "+strings.TrimSpace(strings.Repeat("z\n", 12)) || len(app.editor.Pastes()) != 0 {
		t.Fatalf("typed placeholder: %q, %d pastes left", last.Text, len(app.editor.Pastes()))
	}
	if placeholderAt("a [Pasted text #3 — 2 lines] b", 1) != 0 || placeholderAt("a [Pasted text #3 — 2 lines] b", 2) != 3 || placeholderAt("a [Pasted text #3 — 2 lines] b", 28) != 3 || placeholderAt("a [Pasted text #3 — 2 lines] b", 29) != 0 {
		t.Fatal("placeholderAt")
	}
}

// Unread entries (R2.18): the last entry seen per chat is kept in
// SeenFile and advances with every frame of the selected chat; /chats
// and the /switch menu mark the other chats' new messages, the status
// bar counts them while the chat is idle, switching into a chat prints
// "── new ──" before the first unseen entry with a notice counting them,
// and End, /bottom and vim's G reprint the chat so the terminal shows its
// end.
func TestUnreadMarkersDividerAndJump(t *testing.T) {
	c1 := &Chat{ID: "chat1", Title: "First", Provider: "codex", Status: "idle"}
	c1.Conversation.Entries = []Entry{{ID: "u1", Role: "user", Text: "hi", CreatedAt: 1}, {ID: "m1", Role: "assistant", Text: "hello", CreatedAt: 2}}
	c2 := &Chat{ID: "chat2", Title: "Second", Provider: "claude", Status: "idle"}
	c2.Conversation.Entries = []Entry{{ID: "u2", Role: "user", Text: "start", CreatedAt: 1}, {ID: "m2", Role: "assistant", Text: "ok", CreatedAt: 2}}
	c3 := &Chat{ID: "chat3", Title: "Never opened", Provider: "codex", Status: "idle"}
	c3.Conversation.Entries = []Entry{{ID: "u3", Role: "user", Text: "x", CreatedAt: 1}}
	f := newFakeServer(t, State{Chats: []*Chat{c1, c2, c3}})
	seenFile := filepath.Join(t.TempDir(), "seen.json")
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }, SeenFile: seenFile}
	ctx := context.Background()
	app.state, _ = app.Client.State(ctx)
	app.markUnread()
	if app.unreadID != "" {
		t.Fatalf("a first visit has no divider: %q", app.unreadID)
	}
	// Following the tail marks the last entry seen; the frame writes it.
	app.draw()
	if app.seen["chat1"].ID != "m1" {
		t.Fatalf("seen after a frame: %+v", app.seen)
	}
	if got := LoadSeen(seenFile); got["chat1"].ID != "m1" || got["chat1"].At != 2 {
		t.Fatalf("seen file: %+v", got)
	}
	// Switch to chat2 and back: chat2 is now marked at m2.
	app.submit(ctx, "/switch 2")
	app.draw()
	app.submit(ctx, "/switch 1")
	app.draw()
	if app.seen["chat2"].ID != "m2" {
		t.Fatalf("seen for chat2: %+v", app.seen)
	}
	// New entries arrive in chat2 (a message sent from elsewhere and the
	// reply, with tool steps that do not count) and in chat3, never opened.
	f.mu.Lock()
	f.state.Chats[1].Conversation.Entries = append(f.state.Chats[1].Conversation.Entries,
		Entry{ID: "u4", Role: "user", Text: "another question", CreatedAt: 3},
		Entry{ID: "a4", Role: "activity", Text: "ls", CreatedAt: 4},
		Entry{ID: "t4", Role: "thinking", Text: "hmm", CreatedAt: 4.5},
		Entry{ID: "m4", Role: "assistant", Text: "the answer", CreatedAt: 5},
	)
	f.state.Chats[2].Conversation.Entries = append(f.state.Chats[2].Conversation.Entries, Entry{ID: "m3", Role: "assistant", Text: "y", CreatedAt: 2})
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	if n := app.unreadOf(app.state.Chats[1]); n != 2 {
		t.Fatalf("unread of chat2: %d", n)
	}
	if n := app.unreadOf(app.state.Chats[2]); n != 0 {
		t.Fatalf("a chat never opened counts nothing: %d", n)
	}
	app.submit(ctx, "/chats")
	lines := strings.Split(plain(app.notice), "\n")
	if len(lines) != 3 || strings.Contains(lines[0], "•") || !strings.HasSuffix(lines[1], "• 2") || strings.Contains(lines[2], "•") {
		t.Fatalf("/chats: %q", lines)
	}
	if !strings.Contains(plain(strings.Join(app.frame(120, 24).Status, " ")), "2 unread") {
		t.Fatalf("status: %q", app.frame(120, 24).Status)
	}
	typeText(app, ctx, "/switch ")
	if app.menu == nil || len(app.menu.Items) != 3 || !strings.HasSuffix(plain(app.menu.Items[1].Hint), "• 2") || strings.Contains(app.menu.Items[0].Hint, "•") {
		t.Fatalf("/switch menu: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyDown})
	app.handleKey(ctx, Key{Kind: KeyEnter})
	if app.ChatID != "chat2" || app.unreadID != "u4" || !app.redraw {
		t.Fatalf("switch: chat %s divider %q redraw %v", app.ChatID, app.unreadID, app.redraw)
	}
	if app.notice != "switched to Second · 2 new messages since you were here; ── new ── marks the first" {
		t.Fatalf("notice on switch: %q", app.notice)
	}
	// The divider is printed before "another question", after what was
	// seen, and is part of that entry's block (committed with it).
	body, _ := app.compose(80)
	view := plain(strings.Join(body, "\n"))
	div, first := strings.Index(view, UnreadDivider), strings.Index(view, "another question")
	if div < 0 || first < div || strings.Index(view, "ok") > div {
		t.Fatalf("divider: %d %d\n%s", div, first, view)
	}
	var out bytes.Buffer
	app.Output = &out
	app.draw()
	if printed := plain(out.String()); strings.Index(printed, UnreadDivider) < 0 || strings.Index(printed, UnreadDivider) > strings.Index(printed, "another question") {
		t.Fatalf("divider not printed before the entry:\n%q", printed)
	}
	app.Output = io.Discard
	// The mark advanced with the frame, the divider stays.
	if app.seen["chat2"].ID != "m4" || app.unreadID != "u4" {
		t.Fatalf("after the frame: seen %+v divider %q", app.seen["chat2"], app.unreadID)
	}
	if strings.Contains(plain(strings.Join(app.frame(120, 24).Status, " ")), "unread") {
		t.Fatal("status still counts chat2's own entries")
	}
	// Switching back and forth with nothing new draws no divider and says
	// nothing.
	app.submit(ctx, "/switch 1")
	app.submit(ctx, "/switch 2")
	if app.unreadID != "" || app.notice != "switched to Second" {
		t.Fatalf("nothing new: divider %q notice %q", app.unreadID, app.notice)
	}
	// End on an empty draft, /bottom and vim's G reprint the chat so the
	// terminal shows its end; End with a draft moves the cursor.
	app.redraw = false
	app.handleKey(ctx, Key{Kind: KeyEnd})
	if !app.redraw || !strings.Contains(app.notice, "reprinted") {
		t.Fatalf("End: redraw %v notice %q", app.redraw, app.notice)
	}
	app.redraw = false
	typeText(app, ctx, "ab")
	app.handleKey(ctx, Key{Kind: KeyLeft})
	app.handleKey(ctx, Key{Kind: KeyEnd})
	if app.redraw || app.editor.Cursor() != 2 {
		t.Fatalf("End with a draft: redraw %v cursor %d", app.redraw, app.editor.Cursor())
	}
	app.editor.Clear()
	app.submit(ctx, "/bottom")
	if !app.redraw {
		t.Fatal("/bottom did not reprint")
	}
	app.redraw = false
	app.vim.Enable()
	keysOf(app, ctx, "<Esc>G")
	if !app.redraw {
		t.Fatal("G did not reprint")
	}
	// The marks survive in the file for the next session.
	app.flushSeen()
	if got := LoadSeen(seenFile); got["chat2"].ID != "m4" || got["chat1"].ID != "m1" {
		t.Fatalf("seen file at the end: %+v", got)
	}
	// A mark whose entry is gone (a rewound transcript) divides by time; a
	// chat seen empty (a mark with no entry) counts everything since as
	// new but gets no divider, as on the web; nothing is new without a
	// mark.
	entries := []Entry{{ID: "a", Role: "user", CreatedAt: 1}, {ID: "b", Role: "assistant", CreatedAt: 2}, {ID: "c", Role: "assistant", CreatedAt: 3}}
	if UnreadStart(entries, Seen{ID: "gone", At: 1.5}, true) != 1 || UnreadStart(entries, Seen{ID: "c", At: 3}, true) != -1 || UnreadStart(entries, Seen{}, false) != -1 || UnreadStart(entries, Seen{At: 0.5}, true) != -1 {
		t.Fatal("UnreadStart")
	}
	if UnreadCount(entries, Seen{At: 0.5}, true) != 3 || UnreadCount(entries, Seen{At: 2.5}, true) != 1 || UnreadCount(entries, Seen{}, false) != 0 || UnreadCount(entries, Seen{ID: "gone", At: 9}, true) != 0 {
		t.Fatal("UnreadCount")
	}
	// Visiting an empty chat marks it: what arrives later is new.
	f.mu.Lock()
	f.state.Chats = append(f.state.Chats, &Chat{ID: "chat4", Title: "Empty", Provider: "codex", Status: "idle"})
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	app.selectChat("chat4")
	app.frame(80, 24)
	if m, ok := app.seen["chat4"]; !ok || m.ID != "" {
		t.Fatalf("empty chat not marked: %+v %v", m, ok)
	}
	f.mu.Lock()
	f.state.Chats[3].Conversation.Entries = []Entry{{ID: "u5", Role: "user", Text: "later", CreatedAt: 1e12}, {ID: "m5", Role: "assistant", Text: "reply", CreatedAt: 1e12 + 1}}
	f.mu.Unlock()
	app.state, _ = app.Client.State(ctx)
	app.selectChat("chat1")
	if n := app.unreadOf(app.state.Chats[3]); n != 2 {
		t.Fatalf("unread of a chat seen empty: %d", n)
	}
	app.submit(ctx, "/switch 4")
	if app.notice != "switched to Empty · 2 new messages since you were here" || app.unreadID != "" {
		t.Fatalf("switch into a chat seen empty: %q divider %q", app.notice, app.unreadID)
	}
	if LoadSeen(filepath.Join(t.TempDir(), "none.json")) == nil {
		t.Fatal("LoadSeen of a missing file")
	}
	// The chat whose number is exactly what was typed comes first in the
	// /switch menu, ahead of a title that contains the digits.
	items := chatItems([]*Chat{{ID: "a", Title: "build 12"}, {ID: "b", Title: "two"}}, "2", nil)
	if len(items) != 2 || items[0].Insert != "2" || items[1].Insert != "1" {
		t.Fatalf("chat menu order: %+v", items)
	}
}
