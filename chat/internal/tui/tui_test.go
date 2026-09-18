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
	mu      sync.Mutex
	state   State
	calls   []string
	uploads []string // name:content, in upload order
	srv     *httptest.Server
	token   string
	// fastMode is whether the fake Warden allows fast mode.
	fastMode bool
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
	case strings.HasSuffix(path, "/remove"):
		w.Write([]byte(`{"ok":true}`))
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
				for _, k := range []string{"always", "message", "mode"} {
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
	app.handleKey(ctx, Key{Kind: KeyUp}) // Up recalls history (none yet), never scrolls
	if app.scroll != 3 {
		t.Fatalf("wheel+up scrolled %d", app.scroll)
	}
	app.handleKey(ctx, Key{Kind: KeyPageUp})
	if app.scroll != 3+app.page() || app.page() != 18 {
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
	if len(fr.Lines)+len(fr.Extra)+1+len(fr.PromptLines) != 24 {
		t.Fatal("frame with a menu does not fill the screen")
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
	// Enter on a command without an argument runs it.
	typeText(app, ctx, "/qu")
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
	if app.menu == nil || len(app.menu.Items) != len(Commands) {
		t.Fatalf("all commands: %+v", app.menu)
	}
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.menu.Selected != len(Commands)-1 {
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
	before := plain(strings.Join(app.compose(80), "\n"))
	app.handleKey(ctx, Key{Kind: KeyCtrlO})
	after := plain(strings.Join(app.compose(80), "\n"))
	if !app.quiet || !strings.Contains(before, "http.server") || strings.Contains(after, "http.server") || strings.Contains(after, "Thought") || !strings.Contains(after, "Create a counter page") {
		t.Fatalf("ctrl-o:\n%s", after)
	}
	if !strings.Contains(plain(app.frame(80, 24).Status), "steps hidden") {
		t.Fatal("status lacks the hidden marker")
	}
	app.submit(ctx, "/verbose")
	if app.quiet {
		t.Fatal("/verbose did not toggle back")
	}
	// Ctrl+L repaints from a cleared screen.
	var out bytes.Buffer
	app.Output = &out
	app.handleKey(ctx, Key{Kind: KeyCtrlL})
	app.draw()
	if !strings.Contains(out.String(), "\x1b[2J") || app.redraw {
		t.Fatal("ctrl-l did not clear the screen")
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
	// Up with an empty draft recalls the last prompt; Home/End scroll.
	app.editor.Clear()
	app.handleKey(ctx, Key{Kind: KeyUp})
	if app.editor.Text() != "git push" {
		t.Fatalf("up: %q", app.editor.Text())
	}
	app.editor.Clear()
	app.frame(80, 24)
	app.handleKey(ctx, Key{Kind: KeyHome})
	if app.scroll == 0 {
		t.Fatal("home did not scroll to the top")
	}
	app.handleKey(ctx, Key{Kind: KeyEnd})
	if app.scroll != 0 {
		t.Fatal("end did not follow")
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
	for _, unwanted := range []string{"│ line 1\n", "diff --git", "+++ b/notes.txt", "--- a/notes.txt", "│ 1\talpha", "│ Found 3 files", "port: 3000"} {
		if strings.Contains(collapsed, unwanted) {
			t.Fatalf("unexpected %q in:\n%s", unwanted, collapsed)
		}
	}
	styled := strings.Join(RenderTranscript(c, 60, false), "\n")
	if !strings.Contains(styled, green+"    │ +GAMMA") || !strings.Contains(styled, red+"    │ -gamma") || !strings.Contains(styled, cyan+"    │ @@") {
		t.Fatalf("diff colours:\n%s", styled)
	}
	expanded := plain(strings.Join(RenderTranscript(c, 60, true), "\n"))
	for _, want := range []string{"│ line 1\n", "│ line 20", "│ 1\talpha", "│ Found 3 files", "│ c\n", "│ port: 3000", "│ title: Preview"} {
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
	for _, want := range []string{"run a command: Create x", "y = allow · a = allow always (`touch` commands) · n [message] = deny", "$ touch x", "Write notes.md", "+1 −0", "+hello", "Claude has a plan", "1. Write notes.md"} {
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
	if got := s.Chats[0].Approvals[0]; got.State != "allowed" || got.Params["always"] != true || !strings.Contains(app.notice, "allowed always: `touch` commands") {
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
	f.state.Chats[0].Session = &struct {
		Model    string `json:"model"`
		FastMode string `json:"fastMode"`
	}{Model: "claude-opus-5", FastMode: "on"}
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
