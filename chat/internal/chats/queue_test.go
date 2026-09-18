package chats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// queueSetup is a resident session that, like Claude's, takes a message
// sent during a turn as the next turn rather than steering the running one.
func queueSetup(t *testing.T) (*Engine, *fakeWorker, string) {
	t.Helper()
	e, w := residentSetup(t)
	e.SteeringProviders = []string{}
	id, _ := e.Create("Queue", "", "", nil)
	sendAndDeliver(t, e, id, "first")
	return e, w, id
}

func queuedTexts(e *Engine, id string) []string {
	var out []string
	for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
		if v.Role == "user" && v.Delivery == "queued" {
			out = append(out, v.Text)
		}
	}
	return out
}

func textOf(items []any) string {
	var parts []string
	for _, item := range items {
		if m := agent.Map(item); m["type"] == "text" {
			parts = append(parts, agent.String(m["text"]))
		}
	}
	return strings.Join(parts, "|")
}

func nthInput(w *fakeWorker, n int) []any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n >= len(w.inputs) {
		return nil
	}
	return w.inputs[n]
}

// Messages sent during a turn wait in the transcript as queued, then go
// in order, one turn each, on the same session.
func TestQueuedMessagesSendInOrderEachAsItsOwnTurn(t *testing.T) {
	e, w, id := queueSetup(t)
	for _, text := range []string{"second", "third"} {
		if err := e.Message(id, text, cv.ID()); err != nil {
			t.Fatal(err)
		}
	}
	if got := queuedTexts(e, id); strings.Join(got, ",") != "second,third" {
		t.Fatalf("queued: %v", got)
	}
	if c := e.Store.Snapshot().chat(id); c.Status != "running" || w.turnCount() != 1 {
		t.Fatalf("a queued message reached the agent during the turn: %s, %d turns", c.Status, w.turnCount())
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 2 })
	if got := textOf(nthInput(w, 1)); got != "second" {
		t.Fatalf("second turn's input: %q", got)
	}
	until(t, func() bool { return strings.Join(queuedTexts(e, id), ",") == "third" })
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 3 })
	if got := textOf(nthInput(w, 2)); got != "third" {
		t.Fatalf("third turn's input: %q", got)
	}
	if w.count("prepare") != 1 {
		t.Fatal("the queue started a new run instead of turns on the live session")
	}
}

// A queued message sent while the earlier turn still appends (its reply)
// opens its own turn after that reply in the transcript: confirm moves
// the entry to the end when the agent gets it.
func TestSentQueuedMessageMovesPastTheEarlierTurn(t *testing.T) {
	e, w, id := queueSetup(t)
	if err := e.Message(id, "second", cv.ID()); err != nil {
		t.Fatal(err)
	}
	w.send(agent.Frame{Method: "item/completed", Params: map[string]any{"turnId": "turn-one", "item": map[string]any{"id": "reply-1", "type": "agentMessage", "text": "first reply"}}})
	until(t, func() bool { return len(e.Store.Snapshot().chat(id).Conversation.Entries) == 3 })
	order := func() string {
		var out []string
		for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
			out = append(out, v.Role+":"+v.Text)
		}
		return strings.Join(out, ",")
	}
	if got := order(); got != "user:first,user:second,assistant:first reply" {
		t.Fatalf("while queued: %s", got)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 2 })
	until(t, func() bool { return order() == "user:first,assistant:first reply,user:second" })
	c := e.Store.Snapshot().chat(id)
	if last := c.Conversation.Entries[2]; last.Delivery != "sent" || last.TurnID == nil {
		t.Fatalf("moved entry: %+v", last)
	}
}

// Withdraw takes a queued message out before the agent gets it; a sent
// message, or another person's queued one, is refused.
func TestWithdrawQueuedMessage(t *testing.T) {
	e, w, id := queueSetup(t)
	second, third := cv.ID(), cv.ID()
	if err := e.MessageFrom(id, "second", second, cv.Actor{PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	first := e.Store.Snapshot().chat(id).Conversation.Entries[0]
	if _, err := e.Withdraw(id, first.ID, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Fatalf("withdrawing a sent message: %v", err)
	}
	if _, err := e.Withdraw(id, second, cv.Actor{PrincipalID: "bob"}); err == nil || !strings.Contains(err.Error(), "only the sender") {
		t.Fatalf("withdrawing another person's message: %v", err)
	}
	entry, err := e.Withdraw(id, second, cv.Actor{PrincipalID: "alice"})
	if err != nil || entry.Text != "second" || entry.Sender == nil || entry.Sender.PrincipalID != "alice" {
		t.Fatalf("withdraw by the sender: %v %+v", err, entry)
	}
	if got := queuedTexts(e, id); strings.Join(got, ",") != "third" {
		t.Fatalf("queued after withdraw: %v", got)
	}
	if _, err = e.Withdraw(id, second, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no such message") {
		t.Fatalf("withdrawing twice: %v", err)
	}
	// The owner may withdraw anyone's.
	if _, err = e.Withdraw(id, third, cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
	if w.turnCount() != 1 {
		t.Fatalf("a withdrawn message was sent: %d turns", w.turnCount())
	}
	// A withdrawn message sent again (edited) is a new message at the end.
	sendAndDeliver(t, e, id, "second, edited")
	if got := textOf(nthInput(w, 1)); got != "second, edited" {
		t.Fatalf("edited message's input: %q", got)
	}
}

// A queued message keeps its attachments through a withdraw: the entry
// names them and the same uploads can be sent again with the edit.
func TestWithdrawnMessageKeepsItsAttachments(t *testing.T) {
	e, w, id := queueSetup(t)
	c := e.Store.Snapshot().chat(id)
	upload, err := e.storeAttachment(context.Background(), c, "notes.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	queued := cv.ID()
	if err = e.MessageFrom(id, "see the file", queued, cv.Actor{}, upload.ID); err != nil {
		t.Fatal(err)
	}
	entry, err := e.Withdraw(id, queued, cv.Actor{})
	if err != nil || len(entry.Attachments) != 1 || entry.Attachments[0].ID != upload.ID || entry.Attachments[0].Name != "notes.txt" {
		t.Fatalf("withdrawn entry: %v %+v", err, entry)
	}
	if err = e.MessageFrom(id, "see the file, please", cv.ID(), cv.Actor{}, entry.Attachments[0].ID); err != nil {
		t.Fatal(err)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 2 })
	if got := textOf(nthInput(w, 1)); !strings.Contains(got, "see the file, please") || !strings.Contains(got, "notes.txt") {
		t.Fatalf("the edited message's input lost the attachment: %q", got)
	}
	if w.count("attachment-write") != 1 {
		t.Fatalf("attachment delivered %d times", w.count("attachment-write"))
	}
}

// Stop holds the queue: the turn is interrupted, the queued messages stay
// queued and go nowhere until SendQueued, or a new message, lets them go
// — in order, the new message last.
func TestStopHoldsTheQueueUntilSent(t *testing.T) {
	e, w, id := queueSetup(t)
	if err := e.Message(id, "second", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Status != "interrupted" || strings.Join(queuedTexts(e, id), ",") != "second" {
		t.Fatalf("after stop: %s, queued %v", c.Status, queuedTexts(e, id))
	}
	until(t, func() bool { return e.sessionIdle(id) })
	time.Sleep(600 * time.Millisecond) // two idle ticks: nothing goes out
	if c := e.Store.Snapshot().chat(id); c.Status != "interrupted" || w.turnCount() != 1 {
		t.Fatalf("the held queue was sent: %s, %d turns", c.Status, w.turnCount())
	}
	if err := e.SendQueued(id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 2 && e.Store.Snapshot().chat(id).Status == "running" })
	if got := textOf(nthInput(w, 1)); got != "second" {
		t.Fatalf("released turn's input: %q", got)
	}
	if err := e.SendQueued(id); err == nil || !strings.Contains(err.Error(), "nothing is queued") {
		t.Fatalf("send-queued with nothing queued: %v", err)
	}
	// Held again, then a new message: the held one goes first.
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.sessionIdle(id) })
	if err := e.Message(id, "fourth", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if got := textOf(nthInput(w, 2)); got != "third" {
		t.Fatalf("after a new message, the held one went %q", got)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 4 })
	if got := textOf(nthInput(w, 3)); got != "fourth" {
		t.Fatalf("the new message went %q", got)
	}
	if w.count("prepare") != 1 || w.count("stop") != 0 || w.count("cancel") != 0 {
		t.Fatalf("runner calls: prepare %d stop %d cancel %d", w.count("prepare"), w.count("stop"), w.count("cancel"))
	}
}

// A conversation rewind on a chat with a held queue withdraws the queued
// messages and says so; a code rewind leaves them.
func TestRewindWithdrawsTheQueue(t *testing.T) {
	e, w, id := queueSetup(t)
	if err := e.Message(id, "second", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.sessionIdle(id) })
	first := e.Store.Snapshot().chat(id).Conversation.Entries[0]
	result, err := e.Rewind(context.Background(), id, first.ID, "code")
	if err != nil || result.Withdrawn != 0 || strings.Join(queuedTexts(e, id), ",") != "second" {
		t.Fatalf("code rewind: %v %+v, queued %v", err, result, queuedTexts(e, id))
	}
	result, err = e.Rewind(context.Background(), id, first.ID, "conversation")
	if err != nil || result.Withdrawn != 1 || result.Conversation != "rewound" {
		t.Fatalf("conversation rewind: %v %+v", err, result)
	}
	c := e.Store.Snapshot().chat(id)
	if len(queuedTexts(e, id)) != 0 {
		t.Fatalf("queue after the rewind: %v", queuedTexts(e, id))
	}
	last := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if last.Role != "rewind" || !strings.Contains(last.Detail, "1 queued message withdrawn") {
		t.Fatalf("marker: %+v", last)
	}
	if c.Status != "interrupted" || w.turnCount() != 1 {
		t.Fatalf("after the rewind: %s, %d turns", c.Status, w.turnCount())
	}
}

// Withdrawing the only message of a chat waiting for its run takes the
// chat off the queue.
func TestWithdrawTheOnlyQueuedMessageIdlesTheChat(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := NewEngine(s, &fakeWorker{})
	id, _ := e.Create("Waiting", "", "", nil)
	message := cv.ID()
	if err = e.Message(id, "Hello", message); err != nil {
		t.Fatal(err)
	}
	if c := s.Snapshot().chat(id); c.Status != "queued" {
		t.Fatalf("status before: %s", c.Status)
	}
	if _, err = e.Withdraw(id, message, cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	if c := s.Snapshot().chat(id); c.Status != "idle" || len(c.Conversation.Entries) != 0 {
		t.Fatalf("after withdraw: %s %d entries", c.Status, len(c.Conversation.Entries))
	}
}

func TestWithdrawAndSendQueuedRoutes(t *testing.T) {
	e, _, id := queueSetup(t)
	queued := cv.ID()
	if err := e.Message(id, "second", queued); err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	call := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", h.Origin+"/api/"+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer private")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// While the turn runs the queue is not held: send-queued has nothing
	// to let go and leaves the chat as it is.
	if code, out := call("chats/"+id+"/send-queued", map[string]any{}); code != 200 {
		t.Fatalf("send-queued while running: %d %v", code, out)
	}
	code, out := call("chats/"+id+"/withdraw", map[string]any{"id": queued})
	if code != 200 || agent.String(out["text"]) != "second" || agent.String(out["delivery"]) != "queued" {
		t.Fatalf("withdraw route: %d %v", code, out)
	}
	code, out = call("chats/"+id+"/withdraw", map[string]any{"id": queued})
	if code != http.StatusConflict || !strings.Contains(agent.String(out["error"]), "no such message") {
		t.Fatalf("second withdraw: %d %v", code, out)
	}
	code, out = call("chats/"+id+"/send-queued", map[string]any{})
	if code != http.StatusConflict || !strings.Contains(agent.String(out["error"]), "nothing is queued") {
		t.Fatalf("send-queued with nothing queued: %d %v", code, out)
	}
}

func TestWithdrawQueuedHelper(t *testing.T) {
	c := &Chat{Conversation: cv.Conversation{Entries: []cv.Entry{
		{ID: "a", Role: "user", Delivery: "sent"},
		{ID: "b", Role: "assistant"},
		{ID: "c", Role: "user", Delivery: "queued"},
		{ID: "d", Role: "user", Delivery: "queued"},
	}}}
	if gone := withdrawQueued(c); len(gone) != 2 || gone[0].ID != "c" || gone[1].ID != "d" || len(c.Conversation.Entries) != 2 || c.Conversation.Entries[1].ID != "b" {
		t.Fatalf("withdrawQueued: %+v %+v", gone, c.Conversation.Entries)
	}
	if queuedLeft(c) {
		t.Fatal("queuedLeft after withdrawing all")
	}
}
