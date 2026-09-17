package conversation

import "testing"

func TestUsageGrowthBetweenTotals(t *testing.T) {
	first := UsageFrom(map[string]any{"inputTokens": 1000.0, "cachedInputTokens": 600.0, "outputTokens": 200.0, "reasoningOutputTokens": 50.0, "totalTokens": 1200.0, "costUSD": 0.02})
	second := UsageFrom(map[string]any{"inputTokens": 2500.0, "cachedInputTokens": 1500.0, "cacheWriteInputTokens": 100.0, "outputTokens": 700.0, "reasoningOutputTokens": 150.0, "totalTokens": 3200.0, "costUSD": 0.05})
	got := second.Sub(first)
	want := Usage{Input: 1500, Cached: 900, CacheWrite: 100, Output: 500, Reasoning: 100, Total: 2000, CostUSD: 0.03}
	if got.Input != want.Input || got.Cached != want.Cached || got.CacheWrite != want.CacheWrite || got.Output != want.Output || got.Reasoning != want.Reasoning || got.Total != want.Total || got.CostUSD < 0.0299 || got.CostUSD > 0.0301 {
		t.Fatalf("growth %+v, want %+v", got, want)
	}
	// A total that restarted (a new process) counts from zero.
	if restarted := first.Sub(second); restarted.Total != 1200 || restarted.CostUSD != 0.02 {
		t.Fatalf("restarted total %+v", restarted)
	}
	if empty := UsageFrom(nil); empty != (Usage{}) {
		t.Fatalf("missing breakdown %+v", empty)
	}
}

func TestTurnRecordsBeginReportFinish(t *testing.T) {
	c := Conversation{}
	c.Begin("t1", 10)
	c.Begin("t1", 11) // a steer joining the turn keeps its start
	c.Report("t1", Usage{Total: 5})
	c.Report("t1", Usage{Total: 9})
	e := NewEntry("assistant", "hi")
	e.TurnID, e.IsStreaming = Ptr("t1"), true
	c.Entries = append(c.Entries, e)
	c.ActiveTurnID = Ptr("t1")
	c.Finish("t1", 20)
	c.Finish("t1", 30) // the end is kept
	if len(c.Turns) != 1 || c.Turns[0] != (Turn{ID: "t1", StartedAt: 10, EndedAt: 20, Usage: c.Turns[0].Usage}) || c.Turns[0].Usage.Total != 9 {
		t.Fatalf("turn %+v", c.Turns)
	}
	if c.ActiveTurnID != nil || c.Entries[0].IsStreaming {
		t.Fatal("finish did not settle the turn")
	}
	// A run that stops ends every open turn and leaves the ended ones alone.
	c.Begin("t2", 40)
	c.Finish("t3", 45) // completed without a start: still recorded
	c.EndTurns(50)
	if c.Turns[0].EndedAt != 20 || c.Turns[1] != (Turn{ID: "t2", StartedAt: 40, EndedAt: 50}) || c.Turns[2] != (Turn{ID: "t3", EndedAt: 45}) {
		t.Fatalf("turns %+v", c.Turns)
	}
}
