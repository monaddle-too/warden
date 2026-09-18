package chats

import (
	"context"
	"strings"
	"testing"
	"time"
	cv "warden/chat/internal/conversation"
)

// claudeResidentSetup is claudeSetup with resident sessions (the
// default), so a control request reaches a live process between turns.
func claudeResidentSetup(t *testing.T) (*Engine, *claudeWorker, string) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &claudeWorker{}
	e := NewEngine(s, w)
	ctx, cancel := context.WithCancel(context.Background())
	go e.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Error("engine did not stop")
		}
		s.Close()
	})
	id, err := e.Create("Settings", "", "", nil, "claude", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	return e, w, id
}

// controlsNamed are the worker's recorded control requests of a subtype.
func controlsNamed(w *claudeWorker, subtype string) []map[string]any {
	var out []map[string]any
	for _, c := range w.controlsSoFar() {
		if c["subtype"] == subtype {
			out = append(out, c)
		}
	}
	return out
}

func TestSettingsValidation(t *testing.T) {
	for arg, want := range map[string]string{"on": "", "default": "", "off": ThinkingOff, "0": ThinkingOff, "8000": "8000", "8k": "8000", "16K": "16000"} {
		if got, err := ParseThinking(arg); err != nil || got != want {
			t.Errorf("ParseThinking(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	for _, arg := range []string{"", "lots", "-5", "999999", "8m"} {
		if _, err := ParseThinking(arg); err == nil {
			t.Errorf("ParseThinking(%q) accepted", arg)
		}
	}
	if !ValidThinking("") || !ValidThinking("off") || !ValidThinking("4000") || ValidThinking("-1") || ValidThinking("x") || ValidThinking("0") {
		t.Fatal("ValidThinking")
	}
	if !ValidEffort("") || !ValidEffort("max") || ValidEffort("ultra") {
		t.Fatal("ValidEffort")
	}
	if thinkingNotice("") == "" || thinkingNotice("off") != "Thinking off" || thinkingNotice("8000") != "Thinking budget: 8.0k tokens" || effortNotice("") != "Effort: model default" || effortNotice("low") != "Effort: low" || modelNotice("") != "Model → provider default" || modelNotice("opus") != "Model → opus" {
		t.Fatal(thinkingNotice("8000"))
	}
	if !longContextModel("sonnet[1m]") || !longContextModel("claude-opus-5[1M]") || longContextModel("opus") {
		t.Fatal("longContextModel")
	}
	if !modelRefused(errorString("Model 'x' not found")) || modelRefused(errorString("set_model is not available")) || modelRefused(nil) {
		t.Fatal("modelRefused")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// Settings are recorded with markers, refused for Codex chats, for a
// level the CLI does not have and for fast mode where the operator did
// not allow it; the 1M-context models likewise.
func TestSettingsRouteAndPolicy(t *testing.T) {
	e, _, id := claudeSetup(t)
	ctx := context.Background()
	str := func(s string) *string { return &s }
	yes := true
	if err := e.SetSettings(ctx, id, Settings{Effort: str("ultra")}); err == nil || !strings.Contains(err.Error(), "unknown effort level") {
		t.Fatal(err)
	}
	if err := e.SetSettings(ctx, id, Settings{Thinking: str("lots")}); err == nil {
		t.Fatal("thinking accepted")
	}
	if err := e.SetSettings(ctx, id, Settings{Fast: &yes}); err == nil || !strings.Contains(err.Error(), "allowFastMode") {
		t.Fatal(err)
	}
	if err := e.SetSettings(ctx, id, Settings{Thinking: str("off"), Effort: str("low")}); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Thinking != "off" || c.Effort != "low" || c.Fast {
		t.Fatalf("%+v", c)
	}
	if ns := notices(c); len(ns) != 2 || ns[0] != "Thinking off" || ns[1] != "Effort: low" {
		t.Fatal(ns)
	}
	// The same value again leaves no marker.
	if err := e.SetSettings(ctx, id, Settings{Effort: str("low")}); err != nil || len(notices(e.Store.Snapshot().chat(id))) != 2 {
		t.Fatal(err)
	}
	e.AllowFastMode = true
	if err := e.SetSettings(ctx, id, Settings{Fast: &yes}); err != nil || !e.Store.Snapshot().chat(id).Fast {
		t.Fatal(err)
	}
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "opus[1m]"); err == nil || !strings.Contains(err.Error(), "allowLongContext") {
		t.Fatal(err)
	}
	e.AllowLongContext = true
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "opus[1m]"); err != nil || e.Store.Snapshot().chat(id).Model != "opus[1m]" {
		t.Fatal(err)
	}
	if v := e.View(); !v.AgentOptions.FastMode || !v.AgentOptions.LongContext {
		t.Fatalf("%+v", v.AgentOptions)
	}
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	if err := e.SetSettings(ctx, codex, Settings{Effort: str("low")}); err == nil || !strings.Contains(err.Error(), "Claude chats") {
		t.Fatal(err)
	}
}

// A model change on a live session is set_model on that session (the
// run and the thread stay, a marker lands, the CLI's next init reports
// the model); a model the CLI does not know is refused and nothing
// changes; a refusal for another reason falls back to ending the session,
// so the next message launches with the model. The thinking, effort and
// fast-mode settings reach a live session at once and every new session
// before its first turn.
func TestModelSwitchLiveAndFallback(t *testing.T) {
	e, w, id := claudeResidentSetup(t)
	ctx := context.Background()
	if err := e.Message(id, "first", cv.ID()); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	until(t, func() bool { return e.sessionIdle(id) })
	before := e.Store.Snapshot().chat(id)
	if before.Session == nil || before.Session.Model != "claude-sonnet-5" {
		t.Fatalf("%+v", before.Session)
	}
	// Live: the session stays, the store follows, a marker lands.
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "opus"); err != nil {
		t.Fatal(err)
	}
	if got := controlsNamed(w, "set_model"); len(got) != 1 || got[0]["model"] != "opus" {
		t.Fatal(got)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Model != "opus" || c.RunID != before.RunID || !e.sessionAlive(id) {
		t.Fatalf("model %q run %q alive %v", c.Model, c.RunID, e.sessionAlive(id))
	}
	if ns := notices(c); len(ns) != 1 || ns[0] != "Model → opus" {
		t.Fatal(ns)
	}
	// Refused as unknown: the error comes back, the model stays.
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "bogus"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.Model != "opus" || !e.sessionAlive(id) || len(notices(c)) != 1 {
		t.Fatalf("%+v", c.Model)
	}
	// Settings reach the live session at once.
	off := "off"
	low := "low"
	if err := e.SetSettings(ctx, id, Settings{Thinking: &off, Effort: &low}); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool {
		return len(controlsNamed(w, "set_max_thinking_tokens")) == 1 && len(controlsNamed(w, "apply_flag_settings")) == 1
	})
	if got := controlsNamed(w, "set_max_thinking_tokens"); got[0]["max_thinking_tokens"] != 0.0 {
		t.Fatal(got)
	}
	if got := controlsNamed(w, "apply_flag_settings"); agentMap(got[0]["settings"])["effortLevel"] != "low" {
		t.Fatal(got)
	}
	// The next turn on the same session reports the switched model and
	// sends the settings nothing new.
	if err := e.Message(id, "second", cv.ID()); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	if c := e.Store.Snapshot().chat(id); c.Session == nil || c.Session.Model != "claude-opus-5" || c.RunID != before.RunID {
		t.Fatalf("%+v run %q", c.Session, c.RunID)
	}
	if len(controlsNamed(w, "set_max_thinking_tokens")) != 1 || len(controlsNamed(w, "apply_flag_settings")) != 1 {
		t.Fatal(w.controlsSoFar())
	}
	// Refused for another reason: the session ends and the next message
	// starts one with the model, which gets the settings before its turn.
	until(t, func() bool { return e.sessionIdle(id) })
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "unswitchable"); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return !e.sessionAlive(id) })
	if c := e.Store.Snapshot().chat(id); c.Model != "unswitchable" || notices(c)[len(notices(c))-1] != "Model → unswitchable" {
		t.Fatalf("%+v", c.Model)
	}
	if err := e.Message(id, "third", cv.ID()); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	if c := e.Store.Snapshot().chat(id); c.RunID == before.RunID {
		t.Fatal("no new run")
	}
	if len(controlsNamed(w, "set_max_thinking_tokens")) != 2 || len(controlsNamed(w, "apply_flag_settings")) != 2 {
		t.Fatal(w.controlsSoFar())
	}
	// A running Claude chat with a live session takes the model too.
	w.hold = make(chan struct{})
	if err := e.Message(id, "fourth", cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.turns == 4 })
	if c := e.Store.Snapshot().chat(id); c.Status != "running" {
		t.Fatal(c.Status)
	}
	if err := e.ConfigureAgentAndRelease(ctx, id, "claude", "haiku"); err != nil {
		t.Fatal(err)
	}
	if got := controlsNamed(w, "set_model"); got[len(got)-1]["model"] != "haiku" {
		t.Fatal(got)
	}
	close(w.hold)
	idle(t, e, id)
}

// agentMap is agent.Map for the test's maps.
func agentMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
