package chats

import (
	"testing"
)

// A Claude session's start asks the CLI for its model catalog, which the
// engine keeps per provider and hands to clients as agentOptions.models
// (never as part of the chats' state); a refusal keeps the last one, a
// changed answer replaces it, and Codex is never asked.
func TestModelCatalogCachedAndExposed(t *testing.T) {
	e, w, id := claudeResidentSetup(t)
	if models := e.View().AgentOptions.Models; models != nil {
		t.Fatalf("catalog before any session: %+v", models)
	}
	oneTurn(t, e, id, "hello")
	until(t, func() bool { return e.sessionIdle(id) })
	w.mu.Lock()
	listed := w.listed
	w.mu.Unlock()
	if listed != 1 {
		t.Fatalf("list_models asked %d times", listed)
	}
	view := e.View()
	rows := view.AgentOptions.Models["claude"]
	if len(rows) != 2 || rows[0].Value != "sonnet" || rows[0].Resolved != "claude-sonnet-5" || rows[0].Label != "Sonnet" || rows[0].Description != "Sonnet 5 · Efficient" || !rows[0].AdaptiveThinking || rows[0].FastMode || len(rows[0].Efforts) != 3 || rows[0].Efforts[2] != "high" {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[1].Value != "haiku" || rows[1].Efforts != nil || rows[1].AdaptiveThinking {
		t.Fatalf("haiku row: %+v", rows[1])
	}
	if view.Catalog != nil {
		t.Fatal("the catalog rode on the state")
	}
	first := e.Store.Snapshot().Catalog["claude"]
	if first == nil || first.At == 0 {
		t.Fatalf("stored catalog: %+v", first)
	}
	// The next session refused: the cached rows stand.
	w.mu.Lock()
	w.catalog = []map[string]any{}
	w.mu.Unlock()
	e.releaseChat(t.Context(), id)
	until(t, func() bool { return !e.sessionAlive(id) })
	oneTurn(t, e, id, "again")
	until(t, func() bool { return e.sessionIdle(id) })
	if rows := e.View().AgentOptions.Models["claude"]; len(rows) != 2 || rows[0].Value != "sonnet" {
		t.Fatalf("after a refusal: %+v", rows)
	}
	// A changed catalog replaces the rows (a new row with fast mode).
	w.mu.Lock()
	w.catalog = []map[string]any{{"value": "opus", "displayName": "Opus", "supportedEffortLevels": []any{"low", "max"}, "supportsFastMode": true}, {"value": "", "displayName": "dropped"}}
	w.mu.Unlock()
	e.releaseChat(t.Context(), id)
	until(t, func() bool { return !e.sessionAlive(id) })
	oneTurn(t, e, id, "once more")
	until(t, func() bool { return e.sessionIdle(id) })
	rows = e.View().AgentOptions.Models["claude"]
	if len(rows) != 1 || rows[0].Value != "opus" || !rows[0].FastMode || len(rows[0].Efforts) != 2 {
		t.Fatalf("replaced rows: %+v", rows)
	}
	// Codex is never asked and has no rows.
	ce, cw := residentSetup(t)
	cid, _ := ce.Create("", "", "", nil, "codex", "")
	sendAndDeliver(t, ce, cid, "hi")
	completeTurn(t, ce, cw, cid)
	if _, ok := ce.View().AgentOptions.Models["codex"]; ok {
		t.Fatal("a Codex catalog")
	}
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for _, m := range cw.methods {
		if m == "models/list" {
			t.Fatal("Codex was asked for models")
		}
	}
}

// The CLI's answer parsed: rows without a value dropped, strings bounded,
// the label defaulting to the value.
func TestParseCatalog(t *testing.T) {
	rows := ParseCatalog(map[string]any{"models": []any{
		map[string]any{"value": "default", "resolvedModel": "claude-opus-5[1m]", "displayName": "Default (recommended)", "description": "Opus 5 with 1M context", "supportsEffort": true, "supportedEffortLevels": []any{"low", "medium", "high", "xhigh", "max"}, "supportsAdaptiveThinking": true, "supportsFastMode": true},
		map[string]any{"value": "haiku", "resolvedModel": "claude-haiku-4-5-20251001"},
		map[string]any{"displayName": "no value"},
		map[string]any{"value": "weird\x00\n", "description": ""},
	}})
	if len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	if rows[0].Value != "default" || rows[0].Resolved != "claude-opus-5[1m]" || rows[0].Label != "Default (recommended)" || len(rows[0].Efforts) != 5 || !rows[0].FastMode || !rows[0].AdaptiveThinking {
		t.Fatalf("%+v", rows[0])
	}
	if rows[1].Label != "haiku" || rows[1].Efforts != nil || rows[1].FastMode {
		t.Fatalf("%+v", rows[1])
	}
	if rows[2].Value != "weird" {
		t.Fatalf("%q", rows[2].Value)
	}
	if ParseCatalog(nil) != nil || ParseCatalog(map[string]any{"models": "x"}) != nil {
		t.Fatal("garbage parsed")
	}
}
