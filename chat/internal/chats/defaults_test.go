package chats

import (
	"context"
	"testing"
	"time"
)

// A chat always has a concrete model: one created without a choice gets
// the provider's default (the operator's, else the built-in), a chat
// recorded without one gets it when the engine starts, and clients are
// told the defaults so their pickers lead with the right row.
func TestDefaultModels(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A chat from before defaults existed.
	if err := s.update(func(st *State) error {
		st.Chats = append(st.Chats, &Chat{ID: "old", Provider: "claude", Title: "Old", Status: "idle"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &claudeWorker{})
	e.DefaultModels = map[string]string{"codex": "gpt-5.5"}
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
	if e.DefaultModel("claude") != DefaultClaudeModel || e.DefaultModel("codex") != "gpt-5.5" || e.DefaultModel("") != "gpt-5.5" {
		t.Fatalf("defaults: %q %q", e.DefaultModel("claude"), e.DefaultModel("codex"))
	}
	claude, err := e.Create("Claude", "", "", nil, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := e.Create("Codex", "", "", nil, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := e.Create("Chosen", "", "", nil, "claude", "haiku")
	if err != nil {
		t.Fatal(err)
	}
	snap := e.Store.Snapshot()
	if got := snap.chat(claude).Model; got != "opus" {
		t.Fatalf("claude default %q", got)
	}
	if got := snap.chat(codex).Model; got != "gpt-5.5" {
		t.Fatalf("codex default %q", got)
	}
	if got := snap.chat(chosen).Model; got != "haiku" {
		t.Fatalf("chosen %q", got)
	}
	// The old chat was filled in when the engine started.
	deadline := time.Now().Add(3 * time.Second)
	for e.Store.Snapshot().chat("old").Model == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.Store.Snapshot().chat("old").Model; got != "opus" {
		t.Fatalf("old chat %q", got)
	}
	// Reconfiguring without a model means the default too.
	if err := e.ConfigureAgentAndRelease(ctx, chosen, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.Store.Snapshot().chat(chosen).Model; got != "opus" {
		t.Fatalf("reconfigured %q", got)
	}
	if v := e.View(); v.AgentOptions.Defaults["claude"] != "opus" || v.AgentOptions.Defaults["codex"] != "gpt-5.5" {
		t.Fatalf("view defaults %v", v.AgentOptions.Defaults)
	}
}
