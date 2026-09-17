package chats

import (
	"context"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

func (f *fakeWorker) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Operation == op {
			n++
		}
	}
	return n
}
func (f *fakeWorker) turnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.turns
}
func (e *Engine) sessionAlive(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active[id] != nil
}
func (e *Engine) sessionIdle(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.active[id]
	return a != nil && a.idle.Load()
}
func residentSetup(t *testing.T) (*Engine, *fakeWorker) {
	t.Helper()
	e, w, _ := setup(t)
	e.ResidentProviders = []string{"codex"} // the fake speaks the Codex protocol
	return e, w
}
func completeTurn(t *testing.T, e *Engine, w *fakeWorker, id string) {
	t.Helper()
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
}
func sendAndDeliver(t *testing.T, e *Engine, id, text string) {
	t.Helper()
	if err := e.Message(id, text, cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		c := e.Store.Snapshot().chat(id)
		entries := c.Conversation.Entries
		return c.Status == "running" && len(entries) > 0 && entries[len(entries)-1].Delivery == "sent"
	})
}

func TestResidentSessionReusedAcrossTurns(t *testing.T) {
	e, w := residentSetup(t)
	id, _ := e.Create("Resident", "", "")
	sendAndDeliver(t, e, id, "first")
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.sessionIdle(id) })
	if !e.sessionAlive(id) {
		t.Fatal("session ended with the turn")
	}
	sendAndDeliver(t, e, id, "second")
	until(t, func() bool { return w.turnCount() == 2 })
	if got := w.count("prepare"); got != 1 {
		t.Fatalf("second turn prepared a new run (%d prepares)", got)
	}
	for _, m := range w.methods {
		if m == "thread/resume" {
			t.Fatal("resident session resumed the thread again")
		}
	}
	completeTurn(t, e, w, id)
	c := e.Store.Snapshot().chat(id)
	if len(c.Conversation.Entries) != 2 || c.Conversation.Entries[1].Delivery != "sent" || c.RunID == "" {
		t.Fatalf("bad transcript after second turn: %+v", c.Conversation.Entries)
	}
}

func TestResidentSessionEndsAfterIdleTimeoutAndNextMessageStartsFresh(t *testing.T) {
	e, w := residentSetup(t)
	e.ResidentIdle = 50 * time.Millisecond
	id, _ := e.Create("Idle", "", "")
	sendAndDeliver(t, e, id, "first")
	completeTurn(t, e, w, id)
	until(t, func() bool { return !e.sessionAlive(id) })
	if c := e.Store.Snapshot().chat(id); c.Status != "idle" || c.Error != "" {
		t.Fatalf("idle expiry must end cleanly, got %s %q", c.Status, c.Error)
	}
	sendAndDeliver(t, e, id, "second")
	until(t, func() bool { return w.count("prepare") == 2 })
}

func TestStopEndsIdleResidentSession(t *testing.T) {
	e, w := residentSetup(t)
	id, _ := e.Create("Stop", "", "")
	sendAndDeliver(t, e, id, "first")
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.sessionIdle(id) })
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	if c := e.Store.Snapshot().chat(id); c.Status != "interrupted" {
		t.Fatalf("status after stop: %s", c.Status)
	}
	if w.count("stop") != 1 {
		t.Fatal("worker stop not requested")
	}
}

func TestIdleResidentSessionYieldsSandboxToAnotherChat(t *testing.T) {
	e, w := residentSetup(t)
	first, _ := e.Create("First", "", "")
	sendAndDeliver(t, e, first, "hello")
	completeTurn(t, e, w, first)
	until(t, func() bool { return e.sessionIdle(first) })
	sandbox := e.Store.Snapshot().chat(first).SandboxID
	second, err := e.Create("Second", sandbox, "")
	if err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, second, "take over")
	if e.sessionAlive(first) {
		t.Fatal("first chat's idle session still holds the shared sandbox")
	}
	if c := e.Store.Snapshot().chat(first); c.Status != "idle" || c.Error != "" {
		t.Fatalf("yielding must be a clean end, got %s %q", c.Status, c.Error)
	}
}

// evaluated returns once the Serve loop has completed a pass that began after
// the caller's last change: the loop takes a wake only from its select, so
// once a second wake has been taken the pass between the two is over.
func evaluated(t *testing.T, e *Engine) {
	t.Helper()
	until(t, func() bool { return len(e.wake) == 0 })
	e.Wake()
	until(t, func() bool { return len(e.wake) == 0 })
}

func TestQueuedChatStartsWhenSiblingSessionGoesIdle(t *testing.T) {
	e, w := residentSetup(t)
	first, _ := e.Create("First", "", "")
	sendAndDeliver(t, e, first, "hello") // mid-turn: the session cannot yield yet
	sandbox := e.Store.Snapshot().chat(first).SandboxID
	second, err := e.Create("Second", sandbox, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Message(second, "waiting", cv.ID()); err != nil {
		t.Fatal(err)
	}
	evaluated(t, e)
	if e.Store.Snapshot().chat(second).Status != "queued" || !e.sessionAlive(first) {
		t.Fatal("second chat ran on a sandbox whose session is mid-turn")
	}
	// The first chat's turn ends and its session waits between turns. No
	// message, stop or timeout follows: going idle alone must let the
	// waiting chat take the sandbox.
	completeTurn(t, e, w, first)
	until(t, func() bool {
		c := e.Store.Snapshot().chat(second)
		return c.Status == "running" && c.Conversation.Entries[0].Delivery == "sent"
	})
	if e.sessionAlive(first) {
		t.Fatal("first chat's idle session still holds the shared sandbox")
	}
	if c := e.Store.Snapshot().chat(first); c.Status != "idle" || c.Error != "" {
		t.Fatalf("yielding must be a clean end, got %s %q", c.Status, c.Error)
	}
	if got := w.count("prepare"); got != 2 {
		t.Fatalf("second chat prepared %d runs, want its own", got)
	}
}

func TestIdleResidentSessionYieldsRunSlot(t *testing.T) {
	e, w := residentSetup(t)
	var ids []string
	for _, title := range []string{"A", "B"} {
		id, _ := e.Create(title, "", "")
		sendAndDeliver(t, e, id, "hello "+title)
		completeTurn(t, e, w, id)
		until(t, func() bool { return e.sessionIdle(id) })
		ids = append(ids, id)
	}
	third, _ := e.Create("C", "", "")
	sendAndDeliver(t, e, third, "needs a slot")
	if e.sessionAlive(ids[0]) && e.sessionAlive(ids[1]) {
		t.Fatal("no idle session yielded its slot")
	}
}

func TestMessageDuringResidentTurnRunsNextOnSameSession(t *testing.T) {
	e, w := residentSetup(t)
	e.SteeringProviders = []string{} // like Claude: a message during a turn waits for the next turn
	id, _ := e.Create("Queued", "", "")
	sendAndDeliver(t, e, id, "first")
	if err := e.Message(id, "during", cv.ID()); err != nil {
		t.Fatal(err)
	}
	// The chat never reports idle: the queued message becomes the next turn at once.
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 2 && e.Store.Snapshot().chat(id).Status == "running" })
	until(t, func() bool {
		entries := e.Store.Snapshot().chat(id).Conversation.Entries
		return len(entries) == 2 && entries[1].Delivery == "sent"
	})
	if w.count("prepare") != 1 {
		t.Fatal("queued message started a new run instead of a turn on the live session")
	}
}

func TestChangingModelEndsResidentSession(t *testing.T) {
	e, w := residentSetup(t)
	id, _ := e.Create("Model", "", "")
	sendAndDeliver(t, e, id, "first")
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.sessionIdle(id) })
	if err := e.ConfigureAgentAndRelease(context.Background(), id, "codex", "gpt-5"); err != nil {
		t.Fatal(err)
	}
	if e.sessionAlive(id) {
		t.Fatal("session survived a model change")
	}
}

func TestResidentSessionsDefaultToClaudeAndCodex(t *testing.T) {
	e := &Engine{}
	if !e.resident("claude") || !e.resident("codex") || e.resident("other") {
		t.Fatal("Claude and Codex chats keep their session open by default")
	}
	if !e.steers("codex") || e.steers("claude") {
		t.Fatal("only Codex steers a message into the running turn")
	}
}
