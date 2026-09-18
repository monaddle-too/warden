package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

func (f *fakeWorker) requestsOf(op string) []sandbox.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sandbox.Request
	for _, r := range f.requests {
		if r.Operation == op {
			out = append(out, r)
		}
	}
	return out
}
func (f *fakeWorker) methodList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}
func (f *fakeWorker) lastInput() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// twoTurns runs two messages through a resident session and leaves the
// chat idle with the session live; it returns the chat and the two
// message IDs.
func twoTurns(t *testing.T, e *Engine, w *fakeWorker) (string, string, string) {
	t.Helper()
	id, _ := e.Create("Rewind", "", "", nil)
	first := cv.ID()
	if err := e.Message(id, "first", first); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 1 })
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.sessionIdle(id) })
	second := cv.ID()
	if err := e.Message(id, "second", second); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 2 })
	w.send(agent.Frame{Method: "item/completed", Params: map[string]any{"turnId": "turn-one", "item": map[string]any{"id": "reply-two", "type": "agentMessage", "text": "done"}}})
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.sessionIdle(id) })
	return id, first, second
}

func TestCheckpointBeforeEveryTurnSetsTheDiffBase(t *testing.T) {
	e, w := residentSetup(t)
	id, first, second := twoTurns(t, e, w)
	checkpoints := w.requestsOf("checkpoint")
	if len(checkpoints) != 2 || checkpoints[0].CallID != first || checkpoints[1].CallID != second || checkpoints[0].ChatID != id {
		t.Fatalf("checkpoints: %+v", checkpoints)
	}
	// The checkpoint precedes the turn: the runner saw it before the
	// adapter saw turn/start.
	c := e.Store.Snapshot().chat(id)
	if c.DiffBase != first {
		t.Fatalf("diff base %q, want the first checkpoint %q", c.DiffBase, first)
	}
	changes, err := e.Diff(context.Background(), id)
	if err != nil || changes.Base != first || len(changes.Files) != 1 {
		t.Fatalf("diff: %+v %v", changes, err)
	}
	list, err := e.Checkpoints(context.Background(), id)
	if err != nil || len(list) != 2 {
		t.Fatalf("checkpoints: %+v %v", list, err)
	}
}

func TestDiffNeedsACheckpoint(t *testing.T) {
	e, _ := residentSetup(t)
	id, _ := e.Create("Empty", "", "", nil)
	if _, err := e.Diff(context.Background(), id); err == nil || !strings.Contains(err.Error(), "no checkpoint") {
		t.Fatalf("diff without a checkpoint: %v", err)
	}
}

func TestRewindCodeRestoresTheCheckpointAndKeepsTheTranscript(t *testing.T) {
	e, w := residentSetup(t)
	id, _, second := twoTurns(t, e, w)
	before := len(e.Store.Snapshot().chat(id).Conversation.Entries)
	result, err := e.Rewind(context.Background(), id, second, "code")
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != second || result.What != "code" || result.Conversation != "" || len(result.Restored) != 1 || len(result.Removed) != 1 {
		t.Fatalf("result: %+v", result)
	}
	if restores := w.requestsOf("restore"); len(restores) != 1 || restores[0].CallID != second {
		t.Fatalf("restore requests: %+v", restores)
	}
	c := e.Store.Snapshot().chat(id)
	entries := c.Conversation.Entries
	if len(entries) != before+1 || entries[len(entries)-1].Role != "rewind" || !strings.Contains(entries[len(entries)-1].Text, "Rewound to before “second” (code)") || !strings.Contains(entries[len(entries)-1].Detail, "1 file restored, 1 file removed") {
		t.Fatalf("transcript after a code rewind: %+v", entries)
	}
	if c.DiffBase != second || c.Conversation.ThreadID == nil || c.Status != "idle" {
		t.Fatalf("chat after a code rewind: base %q status %s", c.DiffBase, c.Status)
	}
	// The session was not touched.
	for _, m := range w.methodList() {
		if m == "conversation/rewind" {
			t.Fatal("a code rewind must not rewind the conversation")
		}
	}
	if !e.sessionAlive(id) {
		t.Fatal("a code rewind ended the session")
	}
}

func TestRewindRefusesWhileTheAgentRuns(t *testing.T) {
	e, w := residentSetup(t)
	id, _ := e.Create("Busy", "", "", nil)
	message := cv.ID()
	if err := e.Message(id, "first", message); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 1 })
	if _, err := e.Rewind(context.Background(), id, message, "both"); err == nil || !strings.Contains(err.Error(), "wait for the agent") {
		t.Fatalf("rewind during a turn: %v", err)
	}
	completeTurn(t, e, w, id)
	if _, err := e.Rewind(context.Background(), id, "nope", "code"); err == nil || !strings.Contains(err.Error(), "no such message") {
		t.Fatalf("rewind to an unknown message: %v", err)
	}
	if _, err := e.Rewind(context.Background(), id, message, "files"); err == nil {
		t.Fatal("rewind with a bad scope accepted")
	}
	// The turn's ID names the message too.
	if _, err := e.Rewind(context.Background(), id, "turn-one", "code"); err != nil {
		t.Fatal(err)
	}
}

func TestRewindConversationOnALiveSessionAsksTheAgent(t *testing.T) {
	e, w := residentSetup(t)
	id, first, second := twoTurns(t, e, w)
	result, err := e.Rewind(context.Background(), id, second, "both")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conversation != "rewound" || len(result.Restored) != 1 {
		t.Fatalf("result: %+v", result)
	}
	methods := w.methodList()
	if methods[len(methods)-1] != "conversation/rewind" {
		t.Fatalf("the session was not asked to rewind: %v", methods)
	}
	c := e.Store.Snapshot().chat(id)
	entries := c.Conversation.Entries
	if len(entries) != 2 || entries[0].ID != first || entries[1].Role != "rewind" || !strings.Contains(entries[1].Text, "(code and conversation)") {
		t.Fatalf("transcript after the rewind: %+v", entries)
	}
	if len(c.Conversation.Turns) != 1 || c.Conversation.Turns[0].ID != "turn-one" {
		t.Fatalf("turns after the rewind: %+v", c.Conversation.Turns)
	}
	if c.Rewind != nil || c.Recap != "" || c.NewSession || c.Conversation.ThreadID == nil {
		t.Fatalf("a rewound session needs no fallback: %+v", c)
	}
	// The next message continues on the same session.
	third := cv.ID()
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	if w.count("prepare") != 1 {
		t.Fatal("the rewound session was replaced")
	}
	if input := w.lastInput(); len(input) != 1 || agent.String(agent.Map(input[0])["text"]) != "third" {
		t.Fatalf("the next message carried a recap: %v", input)
	}
}

func TestRewindConversationFallsBackToAFreshSessionWithARecap(t *testing.T) {
	e, w := residentSetup(t)
	w.mu.Lock()
	w.rewindAnswer = map[string]any{"rewound": false, "reason": "target_not_found"}
	w.mu.Unlock()
	id, _, second := twoTurns(t, e, w)
	result, err := e.Rewind(context.Background(), id, second, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conversation != "fresh" || result.Restored != nil {
		t.Fatalf("result: %+v", result)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	c := e.Store.Snapshot().chat(id)
	if c.Conversation.ThreadID != nil || !c.NewSession || !strings.Contains(c.Recap, "User: first") || strings.Contains(c.Recap, "second") {
		t.Fatalf("fallback state: thread %v new %v recap %q", c.Conversation.ThreadID, c.NewSession, c.Recap)
	}
	if last := c.Conversation.Entries[len(c.Conversation.Entries)-1]; last.Role != "rewind" || !strings.Contains(last.Detail, "could not rewind") {
		t.Fatalf("marker: %+v", last)
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
	for _, m := range w.methodList()[len(w.methodList())-3:] {
		if m == "thread/resume" {
			t.Fatal("the fresh run resumed the dropped thread")
		}
	}
	input := w.lastInput()
	if len(input) != 2 || !strings.HasPrefix(agent.String(agent.Map(input[0])["text"]), "Context:") || !strings.Contains(agent.String(agent.Map(input[0])["text"]), "User: first") || agent.String(agent.Map(input[1])["text"]) != "third" {
		t.Fatalf("the recap must precede the message once: %v", input)
	}
	c = e.Store.Snapshot().chat(id)
	if c.Recap != "" || c.NewSession {
		t.Fatal("the recap must be consumed by the turn")
	}
}

func TestRewindConversationWithoutALiveSessionIsAppliedOnResume(t *testing.T) {
	e, w := residentSetup(t)
	e.ResidentIdle = 50 * time.Millisecond
	id, first, second := twoTurns(t, e, w)
	until(t, func() bool { return !e.sessionAlive(id) })
	result, err := e.Rewind(context.Background(), id, second, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conversation != "pending" {
		t.Fatalf("result: %+v", result)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Rewind == nil || c.Rewind.TargetID != second || c.Rewind.LastSeenID != second || len(c.Conversation.Entries) != 2 || c.Conversation.Entries[0].ID != first {
		t.Fatalf("pending rewind: %+v entries %d", c.Rewind, len(c.Conversation.Entries))
	}
	third := cv.ID()
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	methods := w.methodList()
	resumed, rewound := -1, -1
	for i, m := range methods {
		if m == "thread/resume" {
			resumed = i
		}
		if m == "conversation/rewind" {
			rewound = i
		}
	}
	if resumed == -1 || rewound == -1 || rewound < resumed || rewound > len(methods)-1 || methods[len(methods)-1] != "turn/start" {
		t.Fatalf("the rewind must follow the resume and precede the turn: %v", methods)
	}
	if c := e.Store.Snapshot().chat(id); c.Rewind != nil || c.Recap != "" {
		t.Fatal("the pending rewind was not cleared")
	}
	if input := w.lastInput(); len(input) != 1 {
		t.Fatalf("no recap after a rewind the session applied: %v", input)
	}
}

func TestRewindConversationPendingFallsBackWhenResumeCannotRewind(t *testing.T) {
	e, w := residentSetup(t)
	e.ResidentIdle = 50 * time.Millisecond
	w.mu.Lock()
	w.rewindAnswer = map[string]any{"rewound": false, "reason": "target_not_found"}
	w.mu.Unlock()
	id, _, second := twoTurns(t, e, w)
	until(t, func() bool { return !e.sessionAlive(id) })
	if _, err := e.Rewind(context.Background(), id, second, "conversation"); err != nil {
		t.Fatal(err)
	}
	third := cv.ID()
	if err := e.Message(id, "third", third); err != nil {
		t.Fatal(err)
	}
	// The resumed session refuses; the run ends and a fresh one delivers
	// the message with the recap.
	until(t, func() bool { return w.turnCount() == 3 })
	prepares := w.requestsOf("prepare")
	if last := prepares[len(prepares)-1]; !last.NewSession || last.ThreadID != "" || prepares[len(prepares)-2].ThreadID == "" {
		t.Fatalf("the refused resume must be followed by a fresh session: %+v", prepares)
	}
	input := w.lastInput()
	if len(input) != 2 || !strings.Contains(agent.String(agent.Map(input[0])["text"]), "User: first") {
		t.Fatalf("recap: %v", input)
	}
	if c := e.Store.Snapshot().chat(id); c.Status != "running" || c.Error != "" || c.Rewind != nil {
		t.Fatalf("chat after the fallback: %s %q", c.Status, c.Error)
	}
}

// truncate cuts the transcript from the target on, the queue included
// (a conversation rewind withdraws it: queue.go, item 10).
func TestTruncateCutsFromTheTargetQueueIncluded(t *testing.T) {
	c := &Chat{Conversation: cv.Conversation{Entries: []cv.Entry{
		{ID: "a", Role: "user", Delivery: "sent", TurnID: cv.Ptr("t1")},
		{ID: "b", Role: "assistant", TurnID: cv.Ptr("t1")},
		{ID: "c", Role: "user", Delivery: "sent", TurnID: cv.Ptr("t2")},
		{ID: "d", Role: "activity", TurnID: cv.Ptr("t2"), ParentID: ""},
		{ID: "e", Role: "user", Delivery: "queued"},
	}, Turns: []cv.Turn{{ID: "t1"}, {ID: "t2"}}}, Approvals: []Approval{{ID: "p", State: "pending"}}}
	truncate(c, "c")
	ids := []string{}
	for _, v := range c.Conversation.Entries {
		ids = append(ids, v.ID)
	}
	if strings.Join(ids, ",") != "a,b" || len(c.Conversation.Turns) != 1 || c.Approvals[0].State != "expired" {
		t.Fatalf("truncate: %v turns %v approvals %v", ids, c.Conversation.Turns, c.Approvals)
	}
}

func TestRecapAndMarkerText(t *testing.T) {
	entries := []cv.Entry{
		{Role: "user", Text: "Add a test"},
		{Role: "thinking", Text: "hmm"},
		{Role: "activity", Text: "go test ./...", Tool: &cv.Tool{Name: "Bash"}},
		{Role: "activity", Text: "nested", Tool: &cv.Tool{Name: "Read"}, ParentID: "agent-1"},
		{Role: "assistant", Text: "Done."},
	}
	r := recap(entries)
	for _, s := range []string{"Context:", "User: Add a test", "[Bash] go test ./...", "Assistant: Done."} {
		if !strings.Contains(r, s) {
			t.Fatalf("recap lacks %q: %q", s, r)
		}
	}
	if strings.Contains(r, "hmm") || strings.Contains(r, "nested") {
		t.Fatalf("recap carries thinking or a subagent's step: %q", r)
	}
	if recap(nil) != "" {
		t.Fatal("an empty transcript needs no recap")
	}
	long := recap([]cv.Entry{{Role: "user", Text: strings.Repeat("x", 2*maxRecap)}})
	if len(long) > maxRecap+200 || !strings.Contains(long, "…") {
		t.Fatalf("recap not bounded: %d", len(long))
	}
	if got := rewindLabel("  Make   it\nblue  ", "both"); got != "Rewound to before “Make it blue” (code and conversation)" {
		t.Fatalf("label: %q", got)
	}
	if got := rewindLabel(strings.Repeat("y", 80), "conversation"); !strings.Contains(got, "…”") {
		t.Fatalf("long label not cut: %q", got)
	}
	if got := rewindLabel("", "code"); !strings.Contains(got, "(attachments)") {
		t.Fatalf("empty label: %q", got)
	}
	if got := rewindDetail(RewindResult{What: "code"}); got != "the workspace already matched the checkpoint" {
		t.Fatalf("detail: %q", got)
	}
}

func TestRewindAndDiffRoutes(t *testing.T) {
	e, w := residentSetup(t)
	id, _, second := twoTurns(t, e, w)
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		r := httptest.NewRequest(method, h.Origin+"/api/"+path, reader)
		r.Header.Set("Authorization", "Bearer private")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		var v map[string]any
		_ = json.Unmarshal(out.Body.Bytes(), &v)
		return out.Code, v
	}
	code, v := call("GET", "chats/"+id+"/diff", "")
	if code != 200 || v["base"] == "" || !strings.HasPrefix(agent.String(v["diff"]), "diff --git") {
		t.Fatalf("diff route: %d %v", code, v)
	}
	code, v = call("GET", "chats/"+id+"/checkpoints", "")
	if code != 200 || len(agent.Array(v["checkpoints"])) != 2 {
		t.Fatalf("checkpoints route: %d %v", code, v)
	}
	code, v = call("POST", "chats/"+id+"/rewind", `{"turnID":"`+second+`","what":"code"}`)
	if code != 200 || v["messageID"] != second || v["what"] != "code" {
		t.Fatalf("rewind route: %d %v", code, v)
	}
	if code, _ = call("POST", "chats/"+id+"/rewind", `{"turnID":"`+second+`","what":"nothing"}`); code == 200 {
		t.Fatal("bad scope accepted")
	}
}
