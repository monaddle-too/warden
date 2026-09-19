package main

import (
	"bytes"
	"context"
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

	"warden/chat/internal/chats"
	"warden/chat/internal/config"
	"warden/chat/internal/tui"
)

// The model: running chats first with what the agent does, idle ones by
// last activity, eight rows at most; approvals, reviews and a recent
// failure need attention; archived chats are nothing.
func TestMenuModelOrdersChatsAndCollectsAttention(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	at := func(secondsAgo float64) float64 { return float64(now.Unix()) - secondsAgo }
	var chatsIn []*tui.Chat
	turn := func(id string, ended float64) tui.Conversation {
		return tui.Conversation{Turns: []tui.Turn{{ID: id, StartedAt: ended - 30, EndedAt: ended}}}
	}
	chatsIn = append(chatsIn,
		&tui.Chat{ID: "old", Title: "Old", Status: "idle", Conversation: turn("t", at(7200))},
		&tui.Chat{ID: "asks", Title: "Asks", Provider: "claude", Status: "running", Conversation: tui.Conversation{Turns: []tui.Turn{{ID: "t", StartedAt: at(10)}}, Entries: []tui.Entry{{ID: "e1", Role: "activity", Text: "go test ./...", Tool: &tui.Tool{Kind: "command", Status: "running"}}}},
			Approvals: []tui.Approval{{ID: "a1", Method: "warden/network/allow", State: "pending", Params: map[string]any{"host": "example.com", "duration_minutes": 30, "reason": "docs"}}, {ID: "a0", Method: "warden/ports/bind", State: "resolved"}}},
		&tui.Chat{ID: "review", Title: "Review", Provider: "codex", Status: "idle", Conversation: turn("t", at(600)), Reviews: []tui.Review{{ID: "r1", Kind: "pull_request", Title: "Add tray", Repository: "o/r"}}},
		&tui.Chat{ID: "failed", Title: "Failed", Status: "failed", Error: "sandbox exited", Conversation: turn("t", at(300))},
		&tui.Chat{ID: "stale", Title: "Stale failure", Status: "failed", Error: "gone", Conversation: turn("t", at(2*3600))},
		&tui.Chat{ID: "gone", Title: "Archived", Status: "idle", Archived: true, Approvals: []tui.Approval{{ID: "a9", State: "pending", Method: "x"}}},
		&tui.Chat{ID: "starting", Title: "Starting", Status: "queued", Startup: &struct {
			Stage  string `json:"stage"`
			Detail string `json:"detail"`
		}{Stage: "creating", Detail: "pulling the image"}},
	)
	for i := 0; i < 6; i++ {
		chatsIn = append(chatsIn, &tui.Chat{ID: fmt.Sprintf("i%d", i), Title: fmt.Sprintf("Idle %d", i), Status: "idle", Conversation: turn("t", at(float64(1000+i)))})
	}
	spend := &chats.SpendReport{Today: chats.SpendPeriod{Spend: chats.Spend{Turns: 3, CostUSD: 4.12, Priced: true}}}
	m := menuModel(&tui.State{Chats: chatsIn}, spend, now)
	if m.Service != "running" || m.Working != 2 {
		t.Fatalf("service %q working %d", m.Service, m.Working)
	}
	var order []string
	for _, c := range m.Chats {
		order = append(order, c.ID)
	}
	want := "asks starting failed review i0 i1 i2 i3"
	if got := strings.Join(order, " "); got != want || m.More != 4 {
		t.Fatalf("order %q (more %d), want %q (more 4)", got, m.More, want)
	}
	if m.Chats[0].Activity != "Running go test ./..." || m.Chats[1].Activity != "creating: pulling the image" || m.Chats[2].Activity != "" {
		t.Fatalf("activities: %q %q %q", m.Chats[0].Activity, m.Chats[1].Activity, m.Chats[2].Activity)
	}
	if m.Chats[2].LastActive != at(300) {
		t.Fatalf("lastActive %v, want %v", m.Chats[2].LastActive, at(300))
	}
	var attention []string
	for _, a := range m.Attention {
		attention = append(attention, a.Kind+":"+a.ChatID+":"+a.Label)
	}
	wantAttention := []string{
		"approval:asks:allow network access to example.com for 30 min: docs",
		"review:review:Codex proposed a pull request “Add tray” to o/r",
		"error:failed:sandbox exited",
	}
	if strings.Join(attention, "\n") != strings.Join(wantAttention, "\n") {
		t.Fatalf("attention:\n%s\nwant:\n%s", strings.Join(attention, "\n"), strings.Join(wantAttention, "\n"))
	}
	if m.Spend == nil || m.Spend.TodayUSD != 4.12 || m.Spend.Turns != 3 || !m.Spend.Priced {
		t.Fatalf("spend %+v", m.Spend)
	}
	if empty := menuModel(&tui.State{}, nil, now); empty.Attention == nil || empty.Chats == nil || empty.Spend != nil {
		t.Fatalf("an empty state must still marshal arrays: %+v", empty)
	}
}

// The feed: stopped while nothing answers (starting when the manager
// runs the unit), one line per snapshot once the stream connects, the
// spend fetched once and again when a turn ends, stopped again after
// the stream drops. Lines repeat nothing.
func TestMenuFeedFollowsTheServiceUpAndDown(t *testing.T) {
	state := t.TempDir()
	cfg := config.Defaults(state)
	var mu sync.Mutex
	spendCalls := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Warden sign-in required", 401)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/api/") {
		case "spend":
			mu.Lock()
			spendCalls++
			n := spendCalls
			mu.Unlock()
			json.NewEncoder(w).Encode(chats.SpendReport{Today: chats.SpendPeriod{Spend: chats.Spend{Turns: n, CostUSD: float64(n), Priced: true}}})
		case "events":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, st := range []string{"running", "running", "idle"} {
				chat := map[string]any{"id": "c1", "title": "Fix build", "status": st, "conversation": map[string]any{"entries": []any{}, "turns": []any{map[string]any{"id": "t", "startedAt": 100}}}, "approvals": []any{}}
				b, _ := json.Marshal(map[string]any{"version": 1, "chats": []any{chat}})
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
			}
			<-release
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := newFakeService(t, state)
	var out lockedBuffer
	clock := time.Unix(1_700_000_000, 0)
	slept := make(chan struct{}, 10)
	f := &menuFeeder{cfg: cfg, service: func() serviceManager { return svc }, out: &out, now: func() time.Time { return clock }, sleep: func(ctx context.Context, d time.Duration) {
		slept <- struct{}{}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.run(ctx); close(done) }()

	// No endpoint file: stopped. The manager running the unit: starting.
	<-slept
	svc.run()
	<-slept
	<-slept
	// The chat service comes up.
	os.MkdirAll(filepath.Dir(cfg.OwnerTokenFile()), 0o700)
	os.WriteFile(cfg.OwnerTokenFile(), []byte(`{"url":"`+srv.URL+`","token":"tok"}`), 0o600)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(out.String(), "\n") >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The stream ends: stopped (the fake unit is still "running", so starting).
	close(release)
	<-slept
	cancel()
	<-done

	var lines []menuState
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m menuState
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		lines = append(lines, m)
	}
	var got []string
	for _, m := range lines {
		s := m.Service
		if len(m.Chats) > 0 {
			s += ":" + m.Chats[0].Status + ":" + m.Chats[0].Activity
		}
		if m.Spend != nil {
			s += fmt.Sprintf(":$%.0f", m.Spend.TodayUSD)
		}
		got = append(got, s)
	}
	want := []string{"stopped", "starting", "running:running:Agent is working:$1", "running:idle::$2", "starting"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("feed:\n%s\nwant:\n%s\n%s", strings.Join(got, " | "), strings.Join(want, " | "), out.String())
	}
	for _, m := range lines {
		if m.State != state || !m.Registered {
			t.Fatalf("every line names the state and the registration: %+v", m)
		}
	}
}

// lockedBuffer is a bytes.Buffer the feeder writes while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The feed names a non-default instance so the menu bar item can carry it;
// the default instance's feed has no instance field.
func TestMenuFeedNamesTheInstance(t *testing.T) {
	home := fakeHome(t)
	def, _ := defaultStateDir()
	for state, want := range map[string]string{def: "", filepath.Join(home, ".warden-dev"): `"instance":"dev"`} {
		var out bytes.Buffer
		f := &menuFeeder{cfg: config.Defaults(state), service: func() serviceManager { return nil }, out: &out, now: time.Now, sleep: func(context.Context, time.Duration) {}}
		f.emit(menuState{Service: "stopped"})
		if got := out.String(); (want == "" && strings.Contains(got, `"instance"`)) || (want != "" && !strings.Contains(got, want)) {
			t.Fatalf("%s: %s", state, got)
		}
	}
}
