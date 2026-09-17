package conversation

import "testing"

// A reasoning item is a step that says the model is thinking while it
// streams and carries the summary once it is done; one without a summary
// still leaves a line, so the silence before a reply is accounted for.
func TestReasoningItemsAreThinkingSteps(t *testing.T) {
	var c Conversation
	c.Upsert(map[string]any{"id": "r1", "type": "reasoning"}, "turn", false)
	if len(c.Entries) != 1 || c.Entries[0].Role != "activity" || c.Entries[0].Text != "Thinking…" || !c.Entries[0].IsStreaming {
		t.Fatalf("streaming reasoning: %+v", c.Entries)
	}
	c.Upsert(map[string]any{"id": "r1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "Checking the repo layout"}, "then the tests"}}, "turn", true)
	if len(c.Entries) != 1 || c.Entries[0].Text != "Thought: Checking the repo layout\nthen the tests" || c.Entries[0].IsStreaming {
		t.Fatalf("completed reasoning: %+v", c.Entries)
	}
	c.Upsert(map[string]any{"id": "r2", "type": "reasoning"}, "turn", true)
	if len(c.Entries) != 2 || c.Entries[1].Text != "Thought about it" {
		t.Fatalf("reasoning without a summary: %+v", c.Entries)
	}
}
