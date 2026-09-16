package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeKeys(t *testing.T) {
	keys, rest := DecodeKeys([]byte("hi\r\x7f\x1b[A\x1b[3~\x1b[5~\x03\x04\x1b"))
	kinds := []KeyKind{KeyRune, KeyRune, KeyEnter, KeyBackspace, KeyUp, KeyDelete, KeyPageUp, KeyCtrlC, KeyCtrlD}
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
	status := RenderStatus(c, []Port{{ChatID: "chat1", State: "approved", Port: 8000}}, true, 80)
	if !strings.Contains(status, "Local preview test") || !strings.Contains(status, "running") || !strings.Contains(status, "previews:1") {
		t.Fatalf("status: %q", status)
	}
}

func TestFrameShowsTailAndScrolls(t *testing.T) {
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }}
	c := sampleChat()
	for i := 0; i < 40; i++ {
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: fmt.Sprint("e", i), Role: "assistant", Text: fmt.Sprintf("line %d", i)})
	}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "chat1"
	app.live = true
	f := app.frame(80, 12)
	if len(f.Lines) != 10 || len(f.PromptLines) != 1 {
		t.Fatalf("viewport rows %d prompt lines %d", len(f.Lines), len(f.PromptLines))
	}
	joined := plain(strings.Join(f.Lines, "\n"))
	if !strings.Contains(joined, "bind sandbox port 8000") || !strings.Contains(joined, "line 39") {
		t.Fatalf("tail not shown:\n%s", joined)
	}
	app.scroll = 20
	joined = plain(strings.Join(app.frame(80, 12).Lines, "\n"))
	if strings.Contains(joined, "line 39") || !strings.Contains(joined, "line 2") {
		t.Fatalf("scroll not applied:\n%s", joined)
	}
	app.scroll = 10000
	app.frame(80, 12)
	if app.scroll > 200 {
		t.Fatal("scroll not clamped")
	}
	app.ChatID = "missing"
	joined = plain(strings.Join(app.frame(80, 12).Lines, "\n"))
	if !strings.Contains(joined, "no chat selected") || !strings.Contains(joined, "Local preview test") {
		t.Fatalf("chat list when no chat selected:\n%s", joined)
	}
}

// fakeServer is a minimal warden-chat: state, chats, messages, approvals,
// and an event stream that emits the current state whenever it changes.
type fakeServer struct {
	mu    sync.Mutex
	state State
	calls []string
	srv   *httptest.Server
	token string
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
	case strings.HasSuffix(path, "/message"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "chats/"), "/message")
		f.mu.Lock()
		c := f.state.Chat(id)
		c.Status = "running"
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: body["id"].(string), Role: "user", Text: body["text"].(string)})
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
	case strings.HasSuffix(path, "/agent"), strings.HasSuffix(path, "/revoke"):
		w.Write([]byte(`{"ok":true}`))
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
	status, err := Follow(ctx, client, "c1", &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "idle" || !strings.Contains(out.String(), "you: hello there") || !strings.Contains(out.String(), "codex: echo: hello there") {
		t.Fatalf("status %q output:\n%s", status, out.String())
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

func TestScrollKeysAndWheel(t *testing.T) {
	keys, rest := DecodeKeys([]byte("\x1b[<64;10;5M\x1b[<65;10;5M\x1b[<0;3;4m\x10\x0e"))
	if len(rest) != 0 || len(keys) != 4 || keys[0].Kind != KeyWheelUp || keys[1].Kind != KeyWheelDown || keys[2].Kind != KeyCtrlP || keys[3].Kind != KeyCtrlN {
		t.Fatalf("keys %+v rest %q", keys, rest)
	}
	app := &App{Now: func() time.Time { return time.Unix(0, 0) }, Output: io.Discard}
	c := sampleChat()
	for i := 0; i < 60; i++ {
		c.Conversation.Entries = append(c.Conversation.Entries, Entry{ID: fmt.Sprint("e", i), Role: "assistant", Text: fmt.Sprintf("line %d", i)})
	}
	app.state = &State{Chats: []*Chat{c}}
	app.ChatID = "chat1"
	app.frame(80, 22) // sets rows
	ctx := context.Background()
	app.handleKey(ctx, Key{Kind: KeyWheelUp})
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.scroll != 4 {
		t.Fatalf("wheel+up scrolled %d", app.scroll)
	}
	app.handleKey(ctx, Key{Kind: KeyPageUp})
	if app.scroll != 4+app.page() || app.page() != 18 {
		t.Fatalf("page: %d (page %d)", app.scroll, app.page())
	}
	f := app.frame(80, 22)
	if !strings.Contains(plain(f.Status), "lines below") {
		t.Fatalf("status lacks scroll indicator: %q", plain(f.Status))
	}
	app.handleKey(ctx, Key{Kind: KeyEnd})
	if app.scroll != 0 {
		t.Fatal("End did not return to the tail")
	}
	// With text in the composer, Up recalls history instead of scrolling.
	app.editor.Set("draft")
	app.editor.Submit()
	app.editor.Set("x")
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.scroll != 0 || app.editor.Text() != "draft" {
		t.Fatalf("history with text: scroll %d text %q", app.scroll, app.editor.Text())
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
	if len(f.Lines)+1+len(f.PromptLines) != 24 {
		t.Fatalf("frame does not fill the screen: %d lines", len(f.Lines))
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
	before := len(app.compose(80))
	app.handleKey(ctx, Key{Kind: KeyTab})
	after := len(app.compose(80))
	if !app.expanded || after <= before {
		t.Fatalf("Tab did not expand: %d -> %d", before, after)
	}
	app.frame(80, 30)
	app.submit(ctx, "/find counter page")
	if app.scroll == 0 || !strings.Contains(app.notice, "found") {
		t.Fatalf("find: scroll %d notice %q", app.scroll, app.notice)
	}
	app.submit(ctx, "/find Done.")
	if !strings.Contains(app.notice, "on screen") && !strings.Contains(app.notice, "found") {
		t.Fatalf("find visible: %q", app.notice)
	}
	app.submit(ctx, "/find zzzz-not-there")
	if !strings.Contains(app.notice, "not found") {
		t.Fatalf("find missing: %q", app.notice)
	}
	app.submit(ctx, "/copy")
	if !strings.HasPrefix(copied, "Done.") || !strings.Contains(app.notice, "copied") {
		t.Fatalf("copy: %q notice %q", copied, app.notice)
	}
	c.Status = "running"
	c.Conversation.Entries[0].CreatedAt = float64(time.Now().Unix() - 42)
	app.state = &State{Chats: []*Chat{c}}
	if ind := plain(app.runIndicator(c)); !strings.Contains(ind, "42s") {
		t.Fatalf("run indicator %q", ind)
	}
}
