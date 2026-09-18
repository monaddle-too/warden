package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/tui"
)

// chatFixture is a state root with a written warden.json and an endpoint
// file pointing at a fake chat service.
func chatFixture(t *testing.T) (configPath string, calls func() []string) {
	t.Helper()
	state, configPath := loginFixture(t)
	var mu sync.Mutex
	var seen []string
	type chat struct {
		ID        string           `json:"id"`
		Title     string           `json:"title"`
		Provider  string           `json:"provider"`
		Status    string           `json:"status"`
		Archived  bool             `json:"archived"`
		Approvals []map[string]any `json:"approvals"`
		Conv      struct {
			Entries []map[string]any `json:"entries"`
		} `json:"conversation"`
	}
	chats := []*chat{{ID: "abc123", Title: "First", Provider: "codex", Status: "idle", Approvals: []map[string]any{{"id": "ap1", "method": "warden/ports/bind", "state": "pending", "params": map[string]any{"port": 8000}}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Warden sign-in required", 401)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/")
		mu.Lock()
		seen = append(seen, r.Method+" "+path)
		mu.Unlock()
		switch {
		case path == "state":
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "chats": chats, "ports": []any{}})
		case path == "events":
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 20; i++ {
				mu.Lock()
				b, _ := json.Marshal(map[string]any{"version": 1, "chats": chats})
				mu.Unlock()
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
				time.Sleep(20 * time.Millisecond)
			}
		case path == "chats":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			chats = append(chats, &chat{ID: "def456", Title: body["title"].(string), Provider: body["provider"].(string), Status: "idle"})
			mu.Unlock()
			w.Write([]byte(`{"id":"def456"}`))
		case strings.HasSuffix(path, "/message"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			mid, _ := body["id"].(string)
			chats[0].Conv.Entries = append(chats[0].Conv.Entries, map[string]any{"id": "u-" + mid, "role": "user", "text": body["text"]}, map[string]any{"id": "a-" + mid, "role": "assistant", "text": "reply to " + body["text"].(string)})
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.Contains(path, "/approvals/"):
			mu.Lock()
			chats[0].Approvals[0]["state"] = "allowed"
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	os.MkdirAll(filepath.Join(state, "app"), 0o700)
	os.WriteFile(filepath.Join(state, "app", "endpoint.json"), []byte(`{"url":"`+srv.URL+`","token":"tok"}`), 0o600)
	return configPath, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, seen...) }
}

func TestChatListNewSendApprove(t *testing.T) {
	configPath, calls := chatFixture(t)
	code, out := runCLI("", false, "chat", "list", "--config", configPath)
	if code != 0 || !strings.Contains(out, "First") || !strings.Contains(out, "abc123") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	code, out = runCLI("", false, "chat", "new", "--config", configPath, "--provider", "claude", "Planning")
	if code != 0 || strings.TrimSpace(out) != "def456" {
		t.Fatalf("new (%d):\n%s", code, out)
	}
	// A chat can be named by number, id prefix or title.
	for _, ref := range []string{"1", "abc", "First"} {
		code, out = runCLI("", false, "chat", "send", "--config", configPath, ref, "hello", "world")
		if code != 0 || strings.TrimSpace(out) != "sent" {
			t.Fatalf("send %s (%d):\n%s", ref, code, out)
		}
	}
	// Flags may follow the positional arguments, as people type them.
	code, out = runCLI("", false, "chat", "send", "1", "with wait", "--wait", "--config", configPath)
	if code != 0 || !strings.Contains(out, "codex: reply to with wait") || !strings.Contains(out, "approval pending (warden/ports/bind)") {
		t.Fatalf("send --wait (%d):\n%s", code, out)
	}
	code, out = runCLI("", false, "chat", "approve", "--config", configPath, "abc123")
	if code != 0 || !strings.Contains(out, "allowed warden/ports/bind") {
		t.Fatalf("approve (%d):\n%s", code, out)
	}
	if code, out = runCLI("", false, "chat", "send", "--config", configPath, "zzz", "x"); code == 0 || !strings.Contains(out, "no chat matches") {
		t.Fatalf("unknown chat (%d):\n%s", code, out)
	}
	joined := strings.Join(calls(), "\n")
	for _, want := range []string{"POST chats", "POST chats/abc123/message", "POST chats/abc123/approvals/ap1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing call %q in:\n%s", want, joined)
		}
	}
	// Interactive mode refuses a non-terminal rather than corrupting output.
	if code, out = runCLI("", false, "chat", "--config", configPath); code == 0 || !strings.Contains(out, "interactive terminal") {
		t.Fatalf("non-terminal chat (%d):\n%s", code, out)
	}
}

func TestStatusAndStopWithoutABackgroundInstance(t *testing.T) {
	configPath, _ := chatFixture(t)
	code, out := runCLI("", false, "status", "--config", configPath)
	if code != 0 || !strings.Contains(out, "no background instance") || !strings.Contains(out, "chat:    http") {
		t.Fatalf("status (%d):\n%s", code, out)
	}
	if code, out = runCLI("", false, "stop", "--config", configPath); code == 0 || !strings.Contains(out, "not running in the background") {
		t.Fatalf("stop (%d):\n%s", code, out)
	}
	// A stale pid file is reported and cleaned up.
	_, configPath2 := loginFixture(t)
	dir := filepath.Dir(configPath2)
	os.WriteFile(filepath.Join(dir, "warden.pid"), []byte("999999999\n"), 0o600)
	code, out = runCLI("", false, "status", "--config", configPath2)
	if code != 0 || !strings.Contains(out, "stale pid file") {
		t.Fatalf("stale status (%d):\n%s", code, out)
	}
	runCLI("", false, "stop", "--config", configPath2)
	if _, err := os.Stat(filepath.Join(dir, "warden.pid")); !os.IsNotExist(err) {
		t.Fatal("stale pid file not removed by stop")
	}
}

func TestInterleavedFlags(t *testing.T) {
	got := strings.Join(interleaved([]string{"send", "1", "hello --wait world", "--wait", "--config", "/c", "--", "--literal"}, map[string]bool{"config": true}), "|")
	if got != "--wait|--config|/c|send|1|hello --wait world|--literal" {
		t.Fatalf("%s", got)
	}
}

func TestNotifyCommandPassesTextAsArguments(t *testing.T) {
	cmd := notifyCommand("Warden: t", `say "hi" & run`)
	if cmd == nil {
		t.Skip("no notification command on this host")
	}
	args := strings.Join(cmd.Args, "\x00")
	if !strings.Contains(args, "Warden: t") || !strings.Contains(args, `say "hi" & run`) {
		t.Fatalf("text not passed as arguments: %q", cmd.Args)
	}
	for _, a := range cmd.Args[1:] {
		if strings.Contains(a, "display notification") && strings.Contains(a, "hi") {
			t.Fatalf("text interpolated into the script: %q", a)
		}
	}
}

func TestStartRejectsUnknownPopupsMode(t *testing.T) {
	_, configPath := loginFixture(t)
	code, out := runCLI("", false, "start", "--config", configPath, "--popups", "loud")
	if code == 0 || !strings.Contains(out, "--popups must be auto, browser, notify, none or silent") {
		t.Fatalf("(%d) %s", code, out)
	}
}

// A review only the app can do opens the app on its chat under every
// popups mode but silent, the default none included; an approval opens
// nothing under none and is notified only in notify and browser modes.
func TestPopupsOpenTheAppForReviews(t *testing.T) {
	chat := &tui.Chat{ID: "abc123", Title: "Docs", Provider: "claude"}
	review := tui.Review{ID: "pr1", Kind: "pull_request", Status: "pending", Title: "Fix the README", Repository: "owner/repo"}
	approval := tui.Approval{ID: "ap1", Method: "warden/ports/bind", State: "pending", Params: map[string]any{"port": 8000, "title": "Counter"}}
	for _, tc := range []struct {
		mode                         string
		detached                     bool
		reviewOpens, reviewNotes     bool
		approvalOpens, approvalNotes bool
	}{
		{popupsNone, false, true, false, false, false},
		{popupsNone, true, true, false, false, false},
		{popupsSilent, true, false, false, false, false},
		{popupsNotify, false, true, true, false, true},
		{popupsBrowser, false, true, true, true, true},
		{popupsAuto, true, true, true, true, true},
		{popupsAuto, false, true, true, false, true},
	} {
		var opened, notified []string
		var log strings.Builder
		p := &popupper{mode: resolvePopups(tc.mode, tc.detached), appURL: "http://127.0.0.1:18781/?launch=5#session=abc", log: &log,
			notify: func(title, body string) error { notified = append(notified, title+": "+body); return nil },
			open:   func(url string) error { opened = append(opened, url); return nil }}
		p.review(chat, review)
		if got := len(opened) == 1; got != tc.reviewOpens {
			t.Fatalf("%s detached=%v: review opened %v (%v)", tc.mode, tc.detached, opened, log.String())
		}
		if tc.reviewOpens && (opened[0] != "http://127.0.0.1:18781/?launch=5&chat=abc123#session=abc" || !strings.Contains(log.String(), `review pending in "Docs": Claude proposed a pull request “Fix the README” to owner/repo; opening the app`)) {
			t.Fatalf("%s: review opened %v, log %q", tc.mode, opened, log.String())
		}
		if got := len(notified) == 1; got != tc.reviewNotes {
			t.Fatalf("%s detached=%v: review notified %v", tc.mode, tc.detached, notified)
		}
		if tc.reviewNotes && notified[0] != "Warden: Docs: Claude proposed a pull request “Fix the README” to owner/repo — review it in the app" {
			t.Fatalf("%s: notification %q", tc.mode, notified[0])
		}
		opened, notified = nil, nil
		p.approval(chat, approval)
		if got := len(opened) == 1; got != tc.approvalOpens {
			t.Fatalf("%s detached=%v: approval opened %v", tc.mode, tc.detached, opened)
		}
		if got := len(notified) == 1; got != tc.approvalNotes {
			t.Fatalf("%s detached=%v: approval notified %v", tc.mode, tc.detached, notified)
		}
	}
	// $BROWSER names the command openBrowser runs with the URL.
	stub := filepath.Join(t.TempDir(), "browser.sh")
	seen := filepath.Join(t.TempDir(), "opened")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s' \"$1\" > "+seen+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", stub)
	if err := openBrowser("http://127.0.0.1:1/?chat=abc123"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(seen); string(b) != "http://127.0.0.1:1/?chat=abc123" {
		t.Fatalf("opened %q", b)
	}
	// Without a launch URL the review is logged, not opened.
	var log strings.Builder
	p := &popupper{mode: popupsNone, log: &log, notify: desktopNotify, open: func(string) error { t.Fatal("opened without a URL"); return nil }}
	p.review(chat, review)
	if !strings.Contains(log.String(), `cannot open the app on "Docs": no launch URL`) {
		t.Fatalf("log: %q", log.String())
	}
}

func TestMostRecentlyActiveChat(t *testing.T) {
	s := &tui.State{Chats: []*tui.Chat{{ID: "old"}, {ID: "busy"}, {ID: "new"}, {ID: "archived", Archived: true}}}
	s.Chats[0].Conversation.Entries = []tui.Entry{{CreatedAt: 10}}
	s.Chats[1].Conversation.Entries = []tui.Entry{{CreatedAt: 5}, {CreatedAt: 50}}
	s.Chats[3].Conversation.Entries = []tui.Entry{{CreatedAt: 99}}
	if got := mostRecentlyActive(s); got != "busy" {
		t.Fatal(got)
	}
	empty := &tui.State{Chats: []*tui.Chat{{ID: "a"}, {ID: "b"}}}
	if got := mostRecentlyActive(empty); got != "a" {
		t.Fatal(got)
	}
}
