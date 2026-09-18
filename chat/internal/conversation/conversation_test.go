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

// A tool item is an activity entry with its Tool recorded: the title in
// Text, the output or diff alone in Detail, the kind, status and what the
// call names on Tool; a status word of the agent's is kept as it is.
func TestToolItemsRecordTheirTool(t *testing.T) {
	var c Conversation
	c.Upsert(map[string]any{"id": "b", "type": "commandExecution", "tool": "Bash", "command": "ls", "description": "List", "status": "inProgress"}, "turn", false)
	e := c.Entries[0]
	if e.Role != "activity" || e.Text != "ls" || e.Detail != "" || e.Tool == nil || e.Tool.Kind != "command" || e.Tool.Name != "Bash" || e.Tool.Status != "running" || e.Tool.Description != "List" || !e.IsStreaming {
		t.Fatalf("command started: %+v %+v", e, e.Tool)
	}
	// Streamed output (Codex) appends to the detail; the completed item
	// carries the whole.
	c.Delta("b", "turn", "a.go\n", "activity")
	c.Delta("b", "turn", "b.go\n", "activity")
	if c.Entries[0].Detail != "a.go\nb.go\n" || c.Entries[0].Tool == nil {
		t.Fatalf("streamed output: %+v", c.Entries[0])
	}
	c.Upsert(map[string]any{"id": "b", "type": "commandExecution", "tool": "Bash", "command": "ls", "status": "failed", "aggregatedOutput": "a.go\nb.go\nExit code 1"}, "turn", true)
	if e = c.Entries[0]; e.Detail != "a.go\nb.go\nExit code 1" || e.Tool.Status != "failed" || e.IsStreaming {
		t.Fatalf("command failed: %+v %+v", e, e.Tool)
	}
	c.Upsert(map[string]any{"id": "d", "type": "commandExecution", "command": "rm -rf x", "status": "declined"}, "turn", true)
	if c.Entries[1].Tool.Status != "declined" {
		t.Fatalf("declined: %+v", c.Entries[1].Tool)
	}
	// A delta before the item is announced makes a running command entry.
	c.Delta("early", "turn", "out", "activity")
	if e = c.Entries[2]; e.Text != "Running command" || e.Detail != "out" || e.Tool == nil || e.Tool.Kind != "command" || e.Tool.Status != "running" {
		t.Fatalf("delta-first command: %+v %+v", e, e.Tool)
	}

	// A file change: the tool and path name it, each change's path and
	// diff make the detail, the paths are on the tool.
	c.Upsert(map[string]any{"id": "e", "type": "fileChange", "tool": "Edit", "status": "completed", "changes": []any{map[string]any{"path": "a.go", "kind": "update", "diff": "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n-x\n+y\n"}}}, "turn", true)
	if e = c.Entries[3]; e.Text != "Edit a.go" || e.Tool.Kind != "edit" || e.Tool.Name != "Edit" || len(e.Tool.Paths) != 1 || e.Tool.Paths[0] != "a.go" || e.Detail != "a.go\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n-x\n+y\n\n" {
		t.Fatalf("edit: %+v %+v", e, e.Tool)
	}
	// Codex names no tool: a single change reads by its kind, several by
	// their count.
	c.Upsert(map[string]any{"id": "f", "type": "fileChange", "status": "completed", "changes": []any{map[string]any{"path": "new.txt", "kind": "add", "diff": "+hi\n"}}}, "turn", true)
	c.Upsert(map[string]any{"id": "g", "type": "fileChange", "status": "completed", "changes": []any{map[string]any{"path": "a", "kind": "update", "diff": ""}, map[string]any{"path": "b", "kind": "delete", "diff": ""}}}, "turn", true)
	if c.Entries[4].Text != "Add new.txt" || c.Entries[5].Text != "Updated 2 files" || len(c.Entries[5].Tool.Paths) != 2 {
		t.Fatalf("codex changes: %q %q", c.Entries[4].Text, c.Entries[5].Text)
	}

	// A generic tool call: kind, title, paths, query and input carried
	// over; the result's text is the detail.
	c.Upsert(map[string]any{"id": "r", "type": "toolCall", "tool": "Grep", "kind": "search", "title": "Grep 'x' in .", "status": "completed", "paths": []any{"."}, "query": "x", "output": "a.go:1:x", "input": map[string]any{"pattern": "x"}}, "turn", true)
	if e = c.Entries[6]; e.Text != "Grep 'x' in ." || e.Detail != "a.go:1:x" || e.Tool.Kind != "search" || e.Tool.Name != "Grep" || e.Tool.Query != "x" || e.Tool.Paths[0] != "." || e.Tool.Input["pattern"] != "x" {
		t.Fatalf("grep: %+v %+v", e, e.Tool)
	}
	c.Upsert(map[string]any{"id": "u", "type": "toolCall", "tool": "Whatever", "status": "running"}, "turn", false)
	if e = c.Entries[7]; e.Text != "Whatever" || e.Tool.Kind != "other" {
		t.Fatalf("untitled tool: %+v %+v", e, e.Tool)
	}

	// An MCP call: server and tool, the arguments as input, the result's
	// text (or its error) as the detail.
	c.Upsert(map[string]any{"id": "m", "type": "mcpToolCall", "server": "warden", "tool": "preview_attach", "status": "completed", "arguments": map[string]any{"port": 3000.0}, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "https://p"}}}}, "turn", true)
	if e = c.Entries[8]; e.Text != "warden · preview_attach" || e.Detail != "https://p" || e.Tool.Kind != "mcp" || e.Tool.Server != "warden" || e.Tool.Name != "preview_attach" || e.Tool.Input["port"] != 3000.0 {
		t.Fatalf("mcp: %+v %+v", e, e.Tool)
	}
	c.Upsert(map[string]any{"id": "m2", "type": "mcpToolCall", "server": "warden", "tool": "x", "status": "failed", "error": map[string]any{"message": "denied"}}, "turn", true)
	if c.Entries[9].Detail != "denied" || c.Entries[9].Tool.Status != "failed" {
		t.Fatalf("mcp error: %+v", c.Entries[9])
	}
	// Codex's dynamic tool call is the same kind of thing.
	c.Upsert(map[string]any{"id": "dt", "type": "dynamicToolCall", "tool": "request_network_access", "status": "completed", "arguments": map[string]any{"host": "x"}}, "turn", true)
	if e = c.Entries[10]; e.Text != "request_network_access" || e.Tool.Kind != "mcp" || e.Tool.Server != "warden" || e.Tool.Input["host"] != "x" {
		t.Fatalf("dynamic tool: %+v %+v", e, e.Tool)
	}
	c.Upsert(map[string]any{"id": "w", "type": "webSearch", "query": "warden", "status": "completed", "output": "hits"}, "turn", true)
	if e = c.Entries[11]; e.Text != "Search: warden" || e.Detail != "hits" || e.Tool.Kind != "webSearch" || e.Tool.Query != "warden" {
		t.Fatalf("web search: %+v %+v", e, e.Tool)
	}
	// Finishing the turn ends the streaming of a call still running.
	c.Finish("turn", 10)
	if c.Entries[2].IsStreaming {
		t.Fatal("finish left a tool streaming")
	}
}

// A subagent's items name their Agent call: the entry keeps the parent's
// ID, the Agent card gets its end when the subagent finishes (and keeps
// it), a background call says so, and the todo list is one entry replaced
// in place. Hydration keeps all of it.
func TestSubagentEntriesNestUnderTheirCall(t *testing.T) {
	clock := 50.0
	now = func() float64 { return clock }
	var c Conversation
	c.Upsert(map[string]any{"id": "agent_1", "type": "toolCall", "tool": "Agent", "kind": "task", "title": "Agent: list files (Explore)", "status": "running", "input": map[string]any{"prompt": "List the files."}}, "t1", false)
	c.Upsert(map[string]any{"id": "bash_1", "type": "commandExecution", "tool": "Bash", "command": "ls", "status": "running", "parentId": "agent_1"}, "t1", false)
	c.Upsert(map[string]any{"id": "bash_1", "type": "commandExecution", "tool": "Bash", "command": "ls", "status": "completed", "aggregatedOutput": "a\nb", "parentId": "agent_1"}, "t1", true)
	c.Upsert(map[string]any{"id": "msg_1", "type": "agentMessage", "text": "a and b", "parentId": "agent_1"}, "t1", true)
	if len(c.Entries) != 3 || c.Entries[0].ParentID != "" || c.Entries[1].ParentID != "agent_1" || c.Entries[2].ParentID != "agent_1" || c.Entries[2].Role != "assistant" || c.Entries[2].IsStreaming {
		t.Fatalf("nesting: %+v", c.Entries)
	}
	if c.Entries[0].EndedAt != 0 || !c.Entries[0].IsStreaming {
		t.Fatalf("running agent card: %+v", c.Entries[0])
	}
	// An async launch: the card is re-announced as background, still running.
	c.Upsert(map[string]any{"id": "agent_1", "type": "toolCall", "tool": "Agent", "kind": "task", "title": "Agent: list files (Explore)", "status": "running", "background": true}, "t1", false)
	if e := c.Entries[0]; !e.Tool.Background || e.Tool.Status != "running" || e.EndedAt != 0 {
		t.Fatalf("background card: %+v", e)
	}
	clock = 63
	c.Upsert(map[string]any{"id": "agent_1", "type": "toolCall", "tool": "Agent", "kind": "task", "title": "Agent: list files (Explore)", "status": "completed", "output": "a and b", "background": true}, "t1", true)
	if e := c.Entries[0]; e.EndedAt != 63 || e.Detail != "a and b" || e.Tool.Status != "completed" || e.IsStreaming {
		t.Fatalf("completed card: %+v", e)
	}
	clock = 70
	c.Upsert(map[string]any{"id": "agent_1", "type": "toolCall", "tool": "Agent", "kind": "task", "title": "Agent: list files (Explore)", "status": "completed", "output": "a and b", "background": true}, "t1", true)
	if c.Entries[0].EndedAt != 63 {
		t.Fatalf("an end, once seen, stays: %+v", c.Entries[0])
	}
	// A background command: its card, and a todo list.
	c.Upsert(map[string]any{"id": "bash_2", "type": "commandExecution", "tool": "Bash", "command": "sleep 9", "status": "running", "background": true, "description": "Wait"}, "t1", false)
	if e := c.Entries[3]; !e.Tool.Background || e.Tool.Description != "Wait" {
		t.Fatalf("background command: %+v", e)
	}
	todos := []any{map[string]any{"content": "Parse", "status": "completed"}, map[string]any{"content": "Test", "status": "in_progress", "activeForm": "Testing"}, map[string]any{"content": "Ship", "status": "pending"}}
	c.Upsert(map[string]any{"id": "todos-1", "type": "todoList", "tool": "TaskUpdate", "todos": todos}, "t1", true)
	e := c.Entries[4]
	if e.Role != "activity" || e.Tool.Kind != "todo" || e.Text != "Todo list · 1 of 3 done · Testing" || e.Detail != "[x] Parse\n[>] Test\n[ ] Ship\n" || len(e.Tool.Input["todos"].([]any)) != 3 || e.IsStreaming {
		t.Fatalf("todo entry: %+v", e)
	}
	c.Upsert(map[string]any{"id": "todos-1", "type": "todoList", "tool": "TaskUpdate", "todos": todos[:1]}, "t1", true)
	if len(c.Entries) != 5 || c.Entries[4].Text != "Todo list · 1 of 1 done" {
		t.Fatalf("todo list replaced in place: %+v", c.Entries[4])
	}
	// Hydration from the agent's own record keeps the nesting and the end.
	items := []any{
		map[string]any{"id": "agent_1", "type": "toolCall", "tool": "Agent", "kind": "task", "title": "Agent: list files (Explore)", "status": "completed", "output": "a and b", "background": true},
		map[string]any{"id": "bash_1", "type": "commandExecution", "tool": "Bash", "command": "ls", "status": "completed", "aggregatedOutput": "a\nb", "parentId": "agent_1"},
		map[string]any{"id": "msg_1", "type": "agentMessage", "text": "a and b", "parentId": "agent_1"},
	}
	clock = 99
	c.Hydrate(map[string]any{"id": "thread", "turns": []any{map[string]any{"id": "t1", "status": "completed", "items": items}}})
	if len(c.Entries) != 5 || c.Entries[0].EndedAt != 63 || c.Entries[1].ParentID != "agent_1" || c.Entries[2].ParentID != "agent_1" || c.Entries[2].Text != "a and b" {
		t.Fatalf("hydrated: %+v", c.Entries)
	}
	// Finish ends the turn's streaming entries whether nested or not.
	c.Upsert(map[string]any{"id": "bash_3", "type": "commandExecution", "command": "ls", "status": "running", "parentId": "agent_1"}, "t1", false)
	c.Finish("t1", 120)
	for _, e := range c.Entries {
		if e.IsStreaming {
			t.Fatalf("still streaming after Finish: %+v", e)
		}
	}
}

// A compaction item is a compaction entry: running while the agent
// compacts, then the divider with the trigger and token counts, the
// summary as its detail; a failed one says so with the error.
func TestCompactionItemsAreDividers(t *testing.T) {
	var c Conversation
	c.Upsert(map[string]any{"id": "k1", "type": "compaction", "status": "running"}, "turn", false)
	e := c.Entries[0]
	if len(c.Entries) != 1 || e.Role != "compaction" || e.Text != "Compacting context…" || !e.IsStreaming || e.Compaction == nil || e.Compaction.Status != "running" {
		t.Fatalf("running compaction: %+v", c.Entries)
	}
	c.Upsert(map[string]any{"id": "k1", "type": "compaction", "status": "completed", "trigger": "manual", "preTokens": 171238.0, "postTokens": 2194.0}, "turn", true)
	e = c.Entries[0]
	if len(c.Entries) != 1 || e.Text != "Context compacted" || e.IsStreaming || e.Compaction.Trigger != "manual" || e.Compaction.PreTokens != 171238 || e.Compaction.PostTokens != 2194 || e.Compaction.Status != "completed" || e.Detail != "" {
		t.Fatalf("completed compaction: %+v %+v", e, e.Compaction)
	}
	c.Upsert(map[string]any{"id": "k1", "type": "compaction", "status": "completed", "trigger": "manual", "preTokens": 171238.0, "postTokens": 2194.0, "summary": "This session is being continued…"}, "turn", true)
	if e = c.Entries[0]; e.Detail != "This session is being continued…" || e.Compaction.PreTokens != 171238 {
		t.Fatalf("summary: %+v %+v", e, e.Compaction)
	}
	c.Upsert(map[string]any{"id": "k2", "type": "compaction", "status": "failed", "error": "API Error: refused"}, "turn", true)
	if e = c.Entries[1]; e.Role != "compaction" || e.Text != "Compaction failed" || e.Compaction.Status != "failed" || e.Compaction.Error != "API Error: refused" {
		t.Fatalf("failed compaction: %+v %+v", e, e.Compaction)
	}
	if ctx := ContextFrom(map[string]any{"used": 42787.0, "window": 200000.0, "model": "claude-sonnet-5"}); ctx == nil || ctx.Used != 42787 || ctx.Window != 200000 || ctx.Model != "claude-sonnet-5" {
		t.Fatalf("context: %+v", ctx)
	}
	if ContextFrom(map[string]any{}) != nil {
		t.Fatal("an empty context is nil")
	}
}
