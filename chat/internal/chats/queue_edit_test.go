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

// Round 2 D (docs/claude-parity.md): a queued message edited in place,
// the explicit hold, and undoing a conversation rewind.

// An edit in place replaces the text and attachments of a queued message
// and keeps its slot and ID: the queue goes in the same order and the
// agent gets the edited text.
func TestEditQueuedInPlaceKeepsOrderAndID(t *testing.T) {
	e, w, id := queueSetup(t)
	second, third := cv.ID(), cv.ID()
	if err := e.MessageFrom(id, "second", second, cv.Actor{PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	upload, err := e.storeAttachment(context.Background(), c, "notes.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.EditQueued(id, third, "third, edited", []string{upload.ID}, cv.Actor{PrincipalID: "bob"}); err == nil || !strings.Contains(err.Error(), "only the sender") {
		t.Fatalf("editing another person's message: %v", err)
	}
	if _, err := e.EditQueued(id, second, "", nil, cv.Actor{PrincipalID: "alice"}); err == nil || !strings.Contains(err.Error(), "needs text") {
		t.Fatalf("emptying a message: %v", err)
	}
	entry, err := e.EditQueued(id, third, "third, edited", []string{upload.ID}, cv.Actor{})
	if err != nil || entry.ID != third || entry.Text != "third, edited" || len(entry.Attachments) != 1 || entry.Attachments[0].Name != "notes.txt" || entry.Delivery != "queued" {
		t.Fatalf("edited entry: %v %+v", err, entry)
	}
	// The sender may edit their own; nil attachments keep the set.
	if entry, err = e.EditQueued(id, second, "second, edited", nil, cv.Actor{PrincipalID: "alice"}); err != nil || entry.Text != "second, edited" || entry.Sender.PrincipalID != "alice" {
		t.Fatalf("edit by the sender: %v %+v", err, entry)
	}
	if got := queuedTexts(e, id); strings.Join(got, ",") != "second, edited,third, edited" {
		t.Fatalf("queue after the edits: %v", got)
	}
	if c := e.Store.Snapshot().chat(id); c.Conversation.Entries[1].ID != second || c.Conversation.Entries[2].ID != third {
		t.Fatal("an edit moved the message or changed its ID")
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 2 })
	if got := textOf(nthInput(w, 1)); got != "second, edited" {
		t.Fatalf("second turn's input: %q", got)
	}
	// Once the message went to the agent an edit is a conflict.
	if _, err := e.EditQueued(id, second, "too late", nil, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Fatalf("editing a sent message: %v", err)
	}
	w.send(agent.Frame{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "turn-one", "status": "completed"}}})
	until(t, func() bool { return w.turnCount() == 3 })
	if got := textOf(nthInput(w, 2)); !strings.Contains(got, "third, edited") || !strings.Contains(got, "notes.txt") {
		t.Fatalf("third turn's input lost the edit or the attachment: %q", got)
	}
}

// A `!` command and a `#` note run beside a held queue and leave it held:
// the person stopped the agent to look around, not to restart it.
func TestExecAndMemoryLeaveAHeldQueueHeld(t *testing.T) {
	e, w, id := queueSetup(t)
	if err := e.Message(id, "second", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.sessionIdle(id) })
	if _, err := e.Exec(context.Background(), id, "ls", cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	if err := e.AppendMemory(context.Background(), id, "prefer small commits", cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // two idle ticks: nothing goes out
	c := e.Store.Snapshot().chat(id)
	if c.Status != "interrupted" || strings.Join(queuedTexts(e, id), ",") != "second" || w.turnCount() != 1 {
		t.Fatalf("after ! and #: %s, queued %v, %d turns", c.Status, queuedTexts(e, id), w.turnCount())
	}
	if last := c.Conversation.Entries[len(c.Conversation.Entries)-1]; last.Role != "system" || !strings.Contains(last.Text, "CLAUDE.md") {
		t.Fatalf("the note's line: %+v", last)
	}
	if err := e.SendQueued(id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 2 })
}

func undoEntryIDs(e *Engine, id string) string {
	var out []string
	for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
		out = append(out, v.Role+":"+v.Text)
	}
	return strings.Join(out, ",")
}

// Undoing a rewind the session had not seen (pending) puts the entries
// back in place, removes the marker and cancels the pending rewind: the
// next message resumes the session without a conversation/rewind.
func TestUndoRewindPendingCancelsIt(t *testing.T) {
	e, w := residentSetup(t)
	e.ResidentIdle = 50 * time.Millisecond
	id, _, second := twoTurns(t, e, w)
	until(t, func() bool { return !e.sessionAlive(id) })
	before := undoEntryIDs(e, id)
	result, err := e.Rewind(context.Background(), id, second, "conversation")
	if err != nil || result.Conversation != "pending" {
		t.Fatalf("rewind: %v %+v", err, result)
	}
	c := e.Store.Snapshot().chat(id)
	marker := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if c.RewoundTail == nil || c.RewoundTail.MarkerID != marker.ID || len(c.RewoundTail.Entries) != 2 || marker.Rewind == nil || marker.Rewind.MessageID != second || marker.Rewind.Conversation != "pending" {
		t.Fatalf("tail after the rewind: %+v marker %+v", c.RewoundTail, marker.Rewind)
	}
	if view := e.View().chat(id); view.UndoRewind != marker.ID || view.RewoundTail != nil {
		t.Fatalf("clients must see the undoable marker, not the tail: %q %v", view.UndoRewind, view.RewoundTail)
	}
	// A `!` command after the rewind stays after the restored entries.
	if _, err := e.Exec(context.Background(), id, "ls", cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.UndoRewind(context.Background(), id, "nope", false, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no longer be undone") {
		t.Fatalf("undo with the wrong marker: %v", err)
	}
	undo, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if undo.MessageID != second || undo.What != "conversation" || undo.Entries != 2 || undo.Session != "cancelled" || undo.Code != "" {
		t.Fatalf("undo result: %+v", undo)
	}
	c = e.Store.Snapshot().chat(id)
	if got := undoEntryIDs(e, id); !strings.HasPrefix(got, before+",activity:ls,system:Rewind undone") {
		t.Fatalf("transcript after the undo: %s", got)
	}
	// (The fake names every turn "turn-one", so one turn record stands.)
	if c.Rewind != nil || c.RewoundTail != nil || c.Conversation.ThreadID == nil || c.NewSession || c.Recap != "" || len(c.Conversation.Turns) != 1 {
		t.Fatalf("chat after the undo: rewind %+v tail %v thread %v new %v recap %q turns %d", c.Rewind, c.RewoundTail, c.Conversation.ThreadID, c.NewSession, c.Recap, len(c.Conversation.Turns))
	}
	for _, v := range c.Conversation.Entries {
		if v.Role == "rewind" {
			t.Fatal("the marker stayed")
		}
	}
	if _, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{}); err == nil {
		t.Fatal("undone twice")
	}
	third := cv.ID()
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	for _, m := range w.methodList() {
		if m == "conversation/rewind" {
			t.Fatal("the cancelled rewind reached the session")
		}
	}
	if input := w.lastInput(); len(input) != 1 || agent.String(agent.Map(input[0])["text"]) != "third" {
		t.Fatalf("the next message must go alone: %v", input)
	}
}

// Undoing a rewind the live session applied starts a fresh session with
// the restored transcript as context: the CLI cannot un-rewind.
func TestUndoRewindLiveStartsFreshWithTheRecap(t *testing.T) {
	e, w := residentSetup(t)
	id, first, second := twoTurns(t, e, w)
	result, err := e.Rewind(context.Background(), id, second, "conversation")
	if err != nil || result.Conversation != "rewound" {
		t.Fatalf("rewind: %v %+v", err, result)
	}
	marker := e.Store.Snapshot().chat(id).Conversation.Entries[1]
	undo, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{PrincipalID: "alice"})
	if err != nil || undo.Session != "fresh" || undo.Entries != 2 {
		t.Fatalf("undo: %v %+v", err, undo)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	c := e.Store.Snapshot().chat(id)
	if c.Conversation.ThreadID != nil || !c.NewSession || !strings.Contains(c.Recap, "User: second") || !strings.Contains(c.Recap, "Assistant: done") {
		t.Fatalf("fallback state: thread %v new %v recap %q", c.Conversation.ThreadID, c.NewSession, c.Recap)
	}
	if c.Conversation.Entries[0].ID != first || c.Conversation.Entries[1].ID != second || c.Conversation.Entries[2].Text != "done" {
		t.Fatalf("entries after the undo: %+v", c.Conversation.Entries)
	}
	if last := c.Conversation.Entries[len(c.Conversation.Entries)-1]; last.Role != "system" || !strings.Contains(last.Text, "cannot take the messages back") || last.Sender == nil || last.Sender.PrincipalID != "alice" {
		t.Fatalf("notice: %+v", last)
	}
	third := cv.ID()
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	prepares := w.requestsOf("prepare")
	if len(prepares) != 2 || !prepares[1].NewSession || prepares[1].ThreadID != "" {
		t.Fatalf("the fresh run must prepare a new session: %+v", prepares)
	}
	input := w.lastInput()
	if len(input) != 2 || !strings.Contains(agent.String(agent.Map(input[0])["text"]), "User: second") || agent.String(agent.Map(input[1])["text"]) != "third" {
		t.Fatalf("the recap must precede the message: %v", input)
	}
}

// Undoing a rewind the session refused (fresh fallback) gives the chat
// its thread back: the session was dropped, not rewound, so the next
// message resumes it without a recap.
func TestUndoRewindFreshResumesTheDroppedThread(t *testing.T) {
	e, w := residentSetup(t)
	w.mu.Lock()
	w.rewindAnswer = map[string]any{"rewound": false, "reason": "target_not_found"}
	w.mu.Unlock()
	id, _, second := twoTurns(t, e, w)
	result, err := e.Rewind(context.Background(), id, second, "conversation")
	if err != nil || result.Conversation != "fresh" {
		t.Fatalf("rewind: %v %+v", err, result)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	c := e.Store.Snapshot().chat(id)
	marker := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if c.Conversation.ThreadID != nil || c.RewoundTail == nil || c.RewoundTail.ThreadID == nil || *c.RewoundTail.ThreadID != "thread-one" {
		t.Fatalf("the tail must keep the dropped thread: %+v", c.RewoundTail)
	}
	undo, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{})
	if err != nil || undo.Session != "resumed" {
		t.Fatalf("undo: %v %+v", err, undo)
	}
	c = e.Store.Snapshot().chat(id)
	if c.Conversation.ThreadID == nil || *c.Conversation.ThreadID != "thread-one" || c.NewSession || c.Recap != "" || c.Rewind != nil {
		t.Fatalf("session after the undo: thread %v new %v recap %q", c.Conversation.ThreadID, c.NewSession, c.Recap)
	}
	w.mu.Lock()
	w.rewindAnswer = nil
	w.mu.Unlock()
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	prepares := w.requestsOf("prepare")
	if last := prepares[len(prepares)-1]; last.NewSession || last.ThreadID != "thread-one" {
		t.Fatalf("the next run must resume the thread: %+v", last)
	}
	if input := w.lastInput(); len(input) != 1 {
		t.Fatalf("no recap on a resumed thread: %v", input)
	}
}

// The tail is dropped when the next turn starts, and by another rewind:
// only the latest rewind, and only until the chat moves on, is undoable.
func TestUndoRewindTailDroppedByATurnOrAnotherRewind(t *testing.T) {
	e, w := residentSetup(t)
	id, first, second := twoTurns(t, e, w)
	if _, err := e.Rewind(context.Background(), id, second, "conversation"); err != nil {
		t.Fatal(err)
	}
	marker := e.Store.Snapshot().chat(id).Conversation.Entries[1]
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if c := e.Store.Snapshot().chat(id); c.RewoundTail != nil {
		t.Fatal("the tail survived a turn")
	}
	completeTurn(t, e, w, id)
	if _, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no longer be undone") {
		t.Fatalf("undo after a turn: %v", err)
	}
	if view := e.View().chat(id); view.UndoRewind != "" {
		t.Fatalf("clients still offered the undo: %q", view.UndoRewind)
	}
	// Two rewinds: the first's tail is gone, the second's stands.
	third := e.Store.Snapshot().chat(id).Conversation.Entries[2]
	if _, err := e.Rewind(context.Background(), id, third.ID, "conversation"); err != nil {
		t.Fatal(err)
	}
	firstMarker := e.Store.Snapshot().chat(id).Conversation.Entries[2]
	if _, err := e.Rewind(context.Background(), id, first, "conversation"); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.RewoundTail == nil || c.RewoundTail.MarkerID == firstMarker.ID || len(c.RewoundTail.Entries) != 3 {
		t.Fatalf("tail after two rewinds: %+v", c.RewoundTail)
	}
	if _, err := e.UndoRewind(context.Background(), id, firstMarker.ID, false, cv.Actor{}); err == nil {
		t.Fatal("the earlier rewind was undone")
	}
	undo, err := e.UndoRewind(context.Background(), id, c.RewoundTail.MarkerID, false, cv.Actor{})
	if err != nil || undo.Entries != 3 {
		t.Fatalf("undo of the latest: %v %+v", err, undo)
	}
	// The first rewind's marker is back among the restored entries.
	if got := undoEntryIDs(e, id); !strings.Contains(got, "rewind:Rewound to before “third”") {
		t.Fatalf("transcript after the undo: %s", got)
	}
}

// A both-rewind records the workspace as it was under the marker's ID
// before restoring, and its undo may write that back; without asking,
// the workspace stays as the rewind left it. A queued message the rewind
// withdrew comes back held.
func TestUndoRewindBothRestoresTheCheckpointTakenBefore(t *testing.T) {
	e, w := residentSetup(t)
	e.SteeringProviders = []string{}
	id, first, second := twoTurns(t, e, w)
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if err := e.Message(id, "fourth", cv.ID()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.sessionIdle(id) })
	result, err := e.Rewind(context.Background(), id, second, "both")
	if err != nil || result.Withdrawn != 1 || result.Conversation != "rewound" {
		t.Fatalf("rewind: %v %+v", err, result)
	}
	c := e.Store.Snapshot().chat(id)
	marker := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	restores := w.requestsOf("restore")
	if len(restores) != 1 || restores[0].CallID != second || restores[0].Before != marker.ID {
		t.Fatalf("the restore must record the workspace under the marker's ID: %+v", restores)
	}
	if marker.Rewind == nil || marker.Rewind.Before != marker.ID || c.RewoundTail.Before != marker.ID || c.RewoundTail.DiffBase != first || c.DiffBase != second {
		t.Fatalf("marker %+v tail %+v base %s", marker.Rewind, c.RewoundTail, c.DiffBase)
	}
	if list, _ := e.Checkpoints(context.Background(), id); len(list) != 4 || list[3].ID != marker.ID {
		t.Fatalf("checkpoints: %+v", list)
	}
	undo, err := e.UndoRewind(context.Background(), id, marker.ID, true, cv.Actor{})
	if err != nil || undo.Code != "restored" || len(undo.Restored) != 1 || undo.Requeued != 1 || undo.Entries != 4 || undo.Session != "fresh" {
		t.Fatalf("undo: %v %+v", err, undo)
	}
	restores = w.requestsOf("restore")
	if len(restores) != 2 || restores[1].CallID != marker.ID || restores[1].Before != "" {
		t.Fatalf("the undo must restore the marker's checkpoint: %+v", restores)
	}
	c = e.Store.Snapshot().chat(id)
	if c.DiffBase != first {
		t.Fatalf("diff base after the undo: %s", c.DiffBase)
	}
	if got := queuedTexts(e, id); strings.Join(got, ",") != "fourth" {
		t.Fatalf("the withdrawn message must be queued again: %v", got)
	}
	if c.Status != "interrupted" || !queueHeldFor(c) {
		t.Fatalf("the restored queue must be held: %s", c.Status)
	}
	if last := c.Conversation.Entries[len(c.Conversation.Entries)-1]; !strings.Contains(last.Text, "back as it was before the rewind") || !strings.Contains(last.Text, "1 message queued again and held") {
		t.Fatalf("notice: %s", last.Text)
	}
	// The queued entry renders last: the notice sits before it in the
	// stored order only because queued entries are appended last.
	if c.Conversation.Entries[len(c.Conversation.Entries)-2].Text != "fourth" {
		t.Fatalf("entries after the undo: %+v", c.Conversation.Entries)
	}
	// Without asking for the code, a both-rewind's undo keeps the workspace.
	if _, err := e.Rewind(context.Background(), id, second, "both"); err != nil {
		t.Fatal(err)
	}
	marker = e.Store.Snapshot().chat(id).Conversation.Entries[len(e.Store.Snapshot().chat(id).Conversation.Entries)-1]
	undo, err = e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{})
	if err != nil || undo.Code != "kept" || len(w.requestsOf("restore")) != 3 {
		t.Fatalf("undo without the code: %v %+v restores %d", err, undo, len(w.requestsOf("restore")))
	}
	if c := e.Store.Snapshot().chat(id); c.DiffBase != second || !strings.Contains(c.Conversation.Entries[len(c.Conversation.Entries)-1].Text, "stays as the rewind left it") {
		t.Fatalf("after the kept undo: base %s", c.DiffBase)
	}
}

// queueHeldFor is the surfaces' rule: queued messages with nothing running.
func queueHeldFor(c *Chat) bool {
	return c.Status != "running" && c.Status != "queued" && c.Status != "stopping" && queuedLeft(c)
}

func TestUndoRewindRefusals(t *testing.T) {
	e, w := residentSetup(t)
	id, _, second := twoTurns(t, e, w)
	if _, err := e.UndoRewind(context.Background(), id, "x", false, cv.Actor{}); err == nil {
		t.Fatal("undo with nothing rewound")
	}
	// A conversation-only rewind has no workspace checkpoint to restore.
	if _, err := e.Rewind(context.Background(), id, second, "conversation"); err != nil {
		t.Fatal(err)
	}
	marker := e.Store.Snapshot().chat(id).Conversation.Entries[1]
	if _, err := e.UndoRewind(context.Background(), id, marker.ID, true, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no checkpoint of the workspace") {
		t.Fatalf("code restore on a conversation rewind: %v", err)
	}
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if _, err := e.UndoRewind(context.Background(), id, marker.ID, false, cv.Actor{}); err == nil || !strings.Contains(err.Error(), "wait for the agent") {
		t.Fatalf("undo during a turn: %v", err)
	}
	// A code-only rewind keeps no tail: nothing to undo.
	completeTurn(t, e, w, id)
	c := e.Store.Snapshot().chat(id)
	third := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if _, err := e.Rewind(context.Background(), id, third.ID, "code"); err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.RewoundTail != nil || e.View().chat(id).UndoRewind != "" {
		t.Fatal("a code rewind kept a tail")
	}
}

func TestEditQueuedAndUndoRewindRoutes(t *testing.T) {
	e, w := residentSetup(t)
	e.SteeringProviders = []string{}
	id, _, second := twoTurns(t, e, w)
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
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	queued := cv.ID()
	if err := e.Message(id, "fourth", queued); err != nil {
		t.Fatal(err)
	}
	code, out := call("chats/"+id+"/queued/"+queued+"/edit", map[string]any{"text": "fourth, edited"})
	if code != 200 || agent.String(out["text"]) != "fourth, edited" || agent.String(out["id"]) != queued {
		t.Fatalf("edit route: %d %v", code, out)
	}
	code, out = call("chats/"+id+"/queued/"+second+"/edit", map[string]any{"text": "nope"})
	if code != http.StatusConflict || !strings.Contains(agent.String(out["error"]), "already sent") {
		t.Fatalf("edit of a sent message: %d %v", code, out)
	}
	if _, err := e.Withdraw(id, queued, cv.Actor{}); err != nil {
		t.Fatal(err)
	}
	completeTurn(t, e, w, id)
	code, out = call("chats/"+id+"/rewind", map[string]any{"turnID": second, "what": "conversation"})
	if code != 200 {
		t.Fatalf("rewind route: %d %v", code, out)
	}
	c := e.Store.Snapshot().chat(id)
	marker := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	code, out = call("chats/"+id+"/undo-rewind", map[string]any{"id": "wrong"})
	if code != http.StatusConflict {
		t.Fatalf("undo with the wrong marker: %d %v", code, out)
	}
	code, out = call("chats/"+id+"/undo-rewind", map[string]any{"id": marker.ID, "code": false})
	if code != 200 || out["session"] != "fresh" || out["entries"] != float64(3) {
		t.Fatalf("undo route: %d %v", code, out)
	}
}
