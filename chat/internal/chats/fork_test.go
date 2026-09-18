package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// claudeResidentSetup is claudeSetup with resident sessions, so a chat's
// session stays up and idle between turns (side questions need it).
func claudeResidentSetup(t *testing.T) (*Engine, *claudeWorker, string) {
	t.Helper()
	e, w, id := claudeSetup(t)
	e.ResidentProviders = []string{"claude"}
	return e, w, id
}

// oneTurn sends text and waits for its turn to end.
func oneTurn(t *testing.T, e *Engine, id, text string) string {
	t.Helper()
	mid := cv.ID()
	if err := e.Message(id, text, mid); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	return mid
}

// A fork of a Claude chat copies the transcript (entry IDs, turns, the
// allow-always rules, the mode, the style), marks where it came from, and
// carries the source's session to be copied at its first launch: the
// runner is asked to fork it, the agent reports a new session, and from
// then on the fork resumes its own while the source keeps the original.
func TestForkCopiesTranscriptAndForksTheSession(t *testing.T) {
	e, w, id := claudeSetup(t)
	first := oneTurn(t, e, id, "remember the codeword")
	if err := e.SetMode(context.Background(), id, ModeAsk); err != nil {
		t.Fatal(err)
	}
	_ = e.Store.update(func(st *State) error {
		st.chat(id).Allowed = []PermissionRule{{Tool: "Bash", Command: "git status"}}
		st.chat(id).OutputStyle = "Learning"
		return nil
	})
	result, err := e.Fork(context.Background(), id, "", cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != "forked" || result.Title != "Modes (fork)" || result.ID == id {
		t.Fatalf("%+v", result)
	}
	st := e.Store.Snapshot()
	source, fork := st.chat(id), st.chat(result.ID)
	if fork == nil || fork.SandboxID != source.SandboxID || fork.Provider != "claude" || fork.Mode != ModeAsk || len(fork.Allowed) != 1 || fork.OutputStyle != "Learning" || fork.Status != "idle" {
		t.Fatalf("fork: %+v", fork)
	}
	if fork.Conversation.ThreadID == nil || *fork.Conversation.ThreadID != "claude-session" || !fork.ForkSession || fork.Rewind != nil || fork.Recap != "" || fork.NewSession {
		t.Fatalf("fork session: thread %v fork %v rewind %v recap %q", fork.Conversation.ThreadID, fork.ForkSession, fork.Rewind, fork.Recap)
	}
	entries := fork.Conversation.Entries
	n := len(source.Conversation.Entries)
	if len(entries) != n+1 || entries[0].ID != first || entries[0].Role != "user" || entries[n].Role != "fork" {
		t.Fatalf("fork transcript: %d entries vs %d, last %+v", len(entries), n, entries[len(entries)-1])
	}
	marker := entries[n]
	if marker.Text != "Forked from “Modes”" || marker.Fork == nil || marker.Fork.ChatID != id || marker.Fork.MessageID != "" || !strings.Contains(marker.Detail, "copy of its session") {
		t.Fatalf("marker: %+v", marker)
	}
	if len(fork.Conversation.Turns) != len(source.Conversation.Turns) || len(fork.Conversation.Turns) == 0 {
		t.Fatalf("turns: %d vs %d", len(fork.Conversation.Turns), len(source.Conversation.Turns))
	}
	// The fork's first run: the runner forks the source's session and the
	// agent reports the copy's own, which the fork keeps.
	w.mu.Lock()
	w.session = "forked-session"
	w.mu.Unlock()
	oneTurn(t, e, result.ID, "what was it?")
	prepares, streams := w.requestsOf("prepare"), w.requestsOf("stream")
	if len(prepares) < 2 || len(streams) < 2 {
		t.Fatalf("requests: %d prepares, %d streams", len(prepares), len(streams))
	}
	prep, stream := prepares[len(prepares)-1], streams[len(streams)-1]
	if prep.ChatID != result.ID || !prep.ForkSession || prep.ThreadID != "claude-session" {
		t.Fatalf("fork prepare: %+v", prep)
	}
	if stream.ChatID != result.ID || !stream.ForkSession || stream.ThreadID != "claude-session" || stream.OutputStyle != "Learning" {
		t.Fatalf("fork stream: %+v", stream)
	}
	st = e.Store.Snapshot()
	source, fork = st.chat(id), st.chat(result.ID)
	if *fork.Conversation.ThreadID != "forked-session" || fork.ForkSession {
		t.Fatalf("fork after its first turn: thread %s fork %v", *fork.Conversation.ThreadID, fork.ForkSession)
	}
	if *source.Conversation.ThreadID != "claude-session" || len(source.Conversation.Entries) != n {
		t.Fatalf("source changed: %s, %d entries", *source.Conversation.ThreadID, len(source.Conversation.Entries))
	}
	// The next run of the fork resumes its own session, no fork.
	oneTurn(t, e, result.ID, "again")
	streams = w.requestsOf("stream")
	if last := streams[len(streams)-1]; last.ForkSession || last.ThreadID != "" {
		t.Fatalf("second stream of the fork: %+v", last)
	}
	if prepares = w.requestsOf("prepare"); prepares[len(prepares)-1].ThreadID != "forked-session" || prepares[len(prepares)-1].ForkSession {
		t.Fatalf("second prepare of the fork: %+v", prepares[len(prepares)-1])
	}
}

// A fork cut before a message keeps only what came before and rewinds
// the copied session to that message as soon as the copy starts.
func TestForkAtAMessageRewindsTheCopy(t *testing.T) {
	e, w, id := claudeSetup(t)
	first := oneTurn(t, e, id, "first")
	second := oneTurn(t, e, id, "second")
	result, err := e.Fork(context.Background(), id, second, cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	fork := e.Store.Snapshot().chat(result.ID)
	for _, v := range fork.Conversation.Entries {
		if v.ID == second || v.Text == "second" {
			t.Fatalf("the cut message is in the fork: %+v", v)
		}
	}
	if fork.Conversation.Entries[0].ID != first || fork.Rewind == nil || fork.Rewind.TargetID != second || fork.Rewind.LastSeenID != second {
		t.Fatalf("fork: first %s rewind %+v", fork.Conversation.Entries[0].ID, fork.Rewind)
	}
	marker := fork.Conversation.Entries[len(fork.Conversation.Entries)-1]
	if marker.Role != "fork" || marker.Text != "Forked from “Modes” at “second”" || marker.Fork.MessageID != second {
		t.Fatalf("marker: %+v", marker)
	}
	w.mu.Lock()
	w.session = "forked-session"
	w.mu.Unlock()
	oneTurn(t, e, result.ID, "go on")
	w.mu.Lock()
	rewinds := append([]map[string]any(nil), w.rewinds...)
	w.mu.Unlock()
	if len(rewinds) != 1 || rewinds[0]["target_message_uuid"] != second || rewinds[0]["last_seen_user_message_uuid"] != second {
		t.Fatalf("rewinds: %+v", rewinds)
	}
	fork = e.Store.Snapshot().chat(result.ID)
	if fork.Rewind != nil || *fork.Conversation.ThreadID != "forked-session" {
		t.Fatalf("fork after its first turn: %+v", fork)
	}
	// A fork at a message that is not the chat's is refused, as is one
	// while the chat runs.
	if _, err := e.Fork(context.Background(), id, "nope", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no such message") {
		t.Fatalf("bad target: %v", err)
	}
	w.mu.Lock()
	w.hold = make(chan struct{})
	hold := w.hold
	w.mu.Unlock()
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "running" })
	if _, err := e.Fork(context.Background(), id, "", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "wait for the agent") {
		t.Fatalf("fork while running: %v", err)
	}
	close(hold)
	idle(t, e, id)
}

// A chat whose agent cannot copy its session (Codex) forks as a fresh
// session with the copied transcript as its recap; a chat with no
// session yet forks with nothing to copy.
func TestForkWithoutASessionCopyIsFresh(t *testing.T) {
	e, w := residentSetup(t)
	id, first, _ := twoTurns(t, e, w)
	result, err := e.Fork(context.Background(), id, "", cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	fork := e.Store.Snapshot().chat(result.ID)
	if result.Session != "fresh" || !fork.NewSession || fork.Conversation.ThreadID != nil || !strings.HasPrefix(fork.Recap, forkPreamble) || !strings.Contains(fork.Recap, "User: first") || !strings.Contains(fork.Recap, "Assistant: done") {
		t.Fatalf("%+v %+v", result, fork)
	}
	if fork.Conversation.Entries[0].ID != first {
		t.Fatal("entry ids not kept")
	}
	// The fork's first message carries the recap ahead of it and starts a
	// new session; the source's is untouched.
	if err := e.Message(result.ID, "continue", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == 3 })
	items := w.lastInput()
	if len(items) != 2 || !strings.HasPrefix(agent.String(agent.Map(items[0])["text"]), forkPreamble) || agent.String(agent.Map(items[1])["text"]) != "continue" {
		t.Fatalf("fork's first input: %+v", items)
	}
	if prepares := w.requestsOf("prepare"); !prepares[len(prepares)-1].NewSession || prepares[len(prepares)-1].ForkSession {
		t.Fatalf("fork prepare: %+v", prepares[len(prepares)-1])
	}
	empty, _ := e.Create("Empty", "", "", nil)
	result, err = e.Fork(context.Background(), empty, "", cv.Actor{})
	if err != nil || result.Session != "none" {
		t.Fatalf("%+v %v", result, err)
	}
	if fork := e.Store.Snapshot().chat(result.ID); len(fork.Conversation.Entries) != 1 || fork.Conversation.Entries[0].Role != "fork" || fork.Recap != "" {
		t.Fatalf("empty fork: %+v", fork)
	}
}

// A side question runs while the session is idle, as an aside entry the
// question and answer make (never a turn, never in the recap), and is
// refused while a turn runs, without a live session, and for Codex.
func TestAsideAnswersFromAnIdleSession(t *testing.T) {
	e, w, id := claudeResidentSetup(t)
	if _, err := e.Aside(context.Background(), id, "early?", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "no agent session") {
		t.Fatalf("aside before any session: %v", err)
	}
	oneTurn(t, e, id, "hello")
	until(t, func() bool { return e.sessionIdle(id) })
	_ = e.Store.update(func(st *State) error { st.chat(id).OutputStyle = "Explanatory"; return nil })
	result, err := e.Aside(context.Background(), id, "what did I say?", cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "the answer" || result.Cost != 0.01 || result.Input != 100 || result.Error != "" {
		t.Fatalf("%+v", result)
	}
	asides := w.requestsOf("aside")
	if len(asides) != 1 || asides[0].Command != "what did I say?" || asides[0].ThreadID != "claude-session" || asides[0].OutputStyle != "Explanatory" || asides[0].ChatID != id {
		t.Fatalf("aside request: %+v", asides)
	}
	c := e.Store.Snapshot().chat(id)
	entry := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if entry.ID != result.ID || entry.Role != "aside" || entry.Text != "what did I say?" || entry.Detail != "the answer" || entry.IsStreaming || entry.Aside == nil || entry.Aside.Status != "completed" || entry.Aside.CostUSD != 0.01 || entry.Sender == nil || entry.EndedAt == 0 {
		t.Fatalf("aside entry: %+v", entry)
	}
	if c.Status != "idle" || w.turns != 1 {
		t.Fatalf("an aside became a turn: status %s, %d turns", c.Status, w.turns)
	}
	if r := recap(c.Conversation.Entries); strings.Contains(r, "what did I say") || strings.Contains(r, "the answer") {
		t.Fatalf("the recap carries the aside: %q", r)
	}
	// A failed one-shot is a failed entry with the reason.
	w.mu.Lock()
	w.aside = &sandbox.AsideResult{Error: "API Error: 503"}
	w.mu.Unlock()
	result, err = e.Aside(context.Background(), id, "again?", cv.Actor{})
	if err != nil || result.Error != "API Error: 503" {
		t.Fatalf("%+v %v", result, err)
	}
	c = e.Store.Snapshot().chat(id)
	if entry := c.Conversation.Entries[len(c.Conversation.Entries)-1]; entry.Aside.Status != "failed" || entry.Aside.Error != "API Error: 503" {
		t.Fatalf("failed aside: %+v", entry)
	}
	// While a turn runs the question is refused, not queued.
	w.mu.Lock()
	w.hold = make(chan struct{})
	hold := w.hold
	w.mu.Unlock()
	if err := e.Message(id, "work", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "running" })
	if _, err := e.Aside(context.Background(), id, "now?", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "wait for the agent") {
		t.Fatalf("aside during a turn: %v", err)
	}
	close(hold)
	idle(t, e, id)
	// Without a live session (released) the question is refused too.
	e.releaseChat(context.Background(), id)
	until(t, func() bool { return !e.sessionAlive(id) })
	if _, err := e.Aside(context.Background(), id, "later?", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("aside without a session: %v", err)
	}
	if entries := e.Store.Snapshot().chat(id).Conversation.Entries; entries[len(entries)-1].Role == "aside" && entries[len(entries)-1].Text == "later?" {
		t.Fatal("a refused question left an entry")
	}
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	if _, err := e.Aside(context.Background(), codex, "hm?", cv.Actor{}); err == nil || !strings.Contains(err.Error(), "Claude chat") {
		t.Fatalf("aside on Codex: %v", err)
	}
}

// The output style is a Claude chat's launch setting: set while idle, the
// resident session ends so the next message launches with it.
func TestOutputStyleAppliesAtTheNextLaunch(t *testing.T) {
	e, w, id := claudeResidentSetup(t)
	oneTurn(t, e, id, "hello")
	until(t, func() bool { return e.sessionIdle(id) })
	for _, bad := range []string{"explanatory", "Nope"} {
		if err := e.SetOutputStyle(context.Background(), id, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := e.SetOutputStyle(context.Background(), id, "Explanatory"); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	if c := e.Store.Snapshot().chat(id); c.OutputStyle != "Explanatory" || c.Status != "idle" {
		t.Fatalf("%+v", c)
	}
	oneTurn(t, e, id, "again")
	streams := w.requestsOf("stream")
	if last := streams[len(streams)-1]; last.OutputStyle != "Explanatory" || last.ForkSession {
		t.Fatalf("stream: %+v", last)
	}
	if err := e.SetOutputStyle(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	if err := e.SetOutputStyle(context.Background(), codex, "Learning"); err == nil {
		t.Fatal("style set on a Codex chat")
	}
	if styles := OutputStyles(); len(styles) != 3 || styles[0] != "" || styles[1] != "Explanatory" {
		t.Fatalf("%v", styles)
	}
}

func TestForkAsideAndStyleRoutes(t *testing.T) {
	e, w, id := claudeResidentSetup(t)
	first := oneTurn(t, e, id, "first")
	until(t, func() bool { return e.sessionIdle(id) })
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest("POST", h.Origin+"/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer private")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		var v map[string]any
		_ = json.Unmarshal(out.Body.Bytes(), &v)
		return out.Code, v
	}
	code, v := call("chats/"+id+"/aside", `{"text":"why?"}`)
	if code != 200 || v["text"] != "the answer" || v["id"] == "" {
		t.Fatalf("aside route: %d %v", code, v)
	}
	code, v = call("chats/"+id+"/style", `{"style":"Learning"}`)
	if code != 200 || e.Store.Snapshot().chat(id).OutputStyle != "Learning" {
		t.Fatalf("style route: %d %v", code, v)
	}
	if code, _ = call("chats/"+id+"/style", `{"style":"bad"}`); code == 200 {
		t.Fatal("bad style accepted")
	}
	code, v = call("chats/"+id+"/fork", `{"turnID":"`+first+`"}`)
	if code != 200 || v["session"] != "forked" || v["id"] == "" {
		t.Fatalf("fork route: %d %v", code, v)
	}
	if fork := e.Store.Snapshot().chat(agent.String(v["id"])); fork == nil || fork.Rewind == nil || fork.Rewind.TargetID != first || len(fork.Conversation.Entries) != 1 {
		t.Fatalf("fork: %+v", fork)
	}
	_ = w
}
