package conversation

import "testing"

// A reasoning item is a thinking entry: its summary streams in by delta
// and by part, the completed item carries the whole, and EndedAt says
// when the thinking stopped. One that completes without a summary keeps
// what streamed; one that never had any still leaves the entry, so the
// silence before a reply is accounted for.
func TestReasoningItemsAreThinkingEntries(t *testing.T) {
	clock := 100.0
	now = func() float64 { return clock }
	var c Conversation
	c.Upsert(map[string]any{"id": "r1", "type": "reasoning"}, "turn", false)
	if len(c.Entries) != 1 || c.Entries[0].Role != "thinking" || c.Entries[0].Text != "" || !c.Entries[0].IsStreaming || c.Entries[0].EndedAt != 0 {
		t.Fatalf("streaming reasoning: %+v", c.Entries)
	}
	c.Delta("r1", "turn", "**Checking**", "thinking")
	c.Delta("r1", "turn", " the repo layout", "thinking")
	c.Break("r1")
	c.Break("r1")
	c.Delta("r1", "turn", "then the tests", "thinking")
	if got := c.Entries[0].Text; got != "**Checking** the repo layout\n\nthen the tests" {
		t.Fatalf("streamed summary: %q", got)
	}
	clock = 112
	c.Upsert(map[string]any{"id": "r1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "**Checking** the repo layout"}, "then the tests"}}, "turn", true)
	e := c.Entries[0]
	if len(c.Entries) != 1 || e.Text != "**Checking** the repo layout\n\nthen the tests" || e.IsStreaming || e.EndedAt != 112 {
		t.Fatalf("completed reasoning: %+v", c.Entries)
	}
	// Codex's completed item may carry no summary although one streamed.
	c.Upsert(map[string]any{"id": "r2", "type": "reasoning"}, "turn", false)
	c.Delta("r2", "turn", "streamed", "thinking")
	c.Upsert(map[string]any{"id": "r2", "type": "reasoning"}, "turn", true)
	if c.Entries[1].Text != "streamed" || c.Entries[1].IsStreaming {
		t.Fatalf("summary-less completion: %+v", c.Entries[1])
	}
	// A delta before the item is announced makes the entry.
	c.Delta("r3", "turn", "early", "thinking")
	if len(c.Entries) != 3 || c.Entries[2].Role != "thinking" || c.Entries[2].Text != "early" {
		t.Fatalf("delta-first reasoning: %+v", c.Entries)
	}
	// A turn ending (or a run stopping) ends thinking still streaming.
	c.Finish("turn", 130)
	if c.Entries[2].IsStreaming || c.Entries[2].EndedAt != 130 || c.Entries[0].EndedAt != 112 {
		t.Fatalf("finish: %+v", c.Entries)
	}
	c.Delta("r4", "turn2", "cut", "thinking")
	c.EndTurns(140)
	if c.Entries[3].EndedAt != 140 {
		t.Fatalf("end turns: %+v", c.Entries[3])
	}
}
