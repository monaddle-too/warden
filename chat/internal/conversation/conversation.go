package conversation

import (
	"fmt"
	"strings"
	"time"
	"warden/chat/internal/agent"
)

// now is the entry clock, replaced by tests.
var now = func() float64 { return float64(time.Now().UnixMilli()) / 1000 }

func (c *Conversation) Upsert(item map[string]any, turn string, completed bool) {
	id, kind := agent.String(item["id"]), agent.String(item["type"])
	if id == "" {
		return
	}
	e := NewEntry("activity", "")
	e.ID = id
	e.TurnID = Ptr(turn)
	e.IsStreaming = !completed
	// An item a subagent produced names the Agent call it belongs to.
	e.ParentID = agent.String(item["parentId"])
	switch kind {
	case "userMessage":
		if client := agent.String(item["clientId"]); client != "" {
			e.ID = client
		}
		e.Role = "user"
		parts := []string{}
		for _, p := range agent.Array(item["content"]) {
			parts = append(parts, agent.String(agent.Map(p)["text"]))
		}
		e.Text = strings.Join(parts, "\n")
		e.IsStreaming = false
	case "agentMessage":
		e.Role = "assistant"
		e.Text = agent.String(item["text"])
	case "commandExecution":
		e.Text = agent.String(item["command"])
		e.Detail = tail(agent.String(item["aggregatedOutput"]), 30000)
		e.Tool = &Tool{Kind: "command", Name: agent.String(item["tool"]), Status: toolStatus(item), Description: agent.String(item["description"]), Background: item["background"] == true}
	case "fileChange":
		changes := agent.Array(item["changes"])
		paths := []string{}
		for _, ch := range changes {
			m := agent.Map(ch)
			paths = append(paths, agent.String(m["path"]))
			e.Detail += agent.String(m["path"]) + "\n" + agent.String(m["diff"]) + "\n"
		}
		e.Text = fileChangeTitle(agent.String(item["tool"]), changes)
		e.Tool = &Tool{Kind: "edit", Name: agent.String(item["tool"]), Status: toolStatus(item), Paths: paths}
	case "dynamicToolCall":
		// Codex calling a Warden tool: the counterpart of Claude's MCP call.
		e.Text = agent.String(item["tool"])
		e.Tool = &Tool{Kind: "mcp", Name: agent.String(item["tool"]), Server: "warden", Status: toolStatus(item), Input: agent.Map(item["arguments"])}
	case "mcpToolCall":
		e.Text = agent.String(item["server"]) + " · " + agent.String(item["tool"])
		e.Detail = tail(mcpResultText(item), 30000)
		e.Tool = &Tool{Kind: "mcp", Name: agent.String(item["tool"]), Server: agent.String(item["server"]), Status: toolStatus(item), Input: agent.Map(item["arguments"])}
	case "webSearch":
		e.Text = "Search: " + agent.String(item["query"])
		e.Detail = tail(agent.String(item["output"]), 30000)
		e.Tool = &Tool{Kind: "webSearch", Name: agent.String(item["tool"]), Status: toolStatus(item), Query: agent.String(item["query"])}
	case "toolCall":
		// Any other tool the Claude adapter typed: a read, a search, a
		// fetch, a subagent, or one it only names.
		e.Text = agent.String(item["title"])
		e.Detail = tail(agent.String(item["output"]), 30000)
		paths := []string{}
		for _, p := range agent.Array(item["paths"]) {
			paths = append(paths, agent.String(p))
		}
		e.Tool = &Tool{Kind: agent.String(item["kind"]), Name: agent.String(item["tool"]), Status: toolStatus(item), Paths: paths, Query: agent.String(item["query"]), Input: agent.Map(item["input"]), Background: item["background"] == true, Progress: ProgressFrom(agent.Map(item["progress"]))}
		if e.Tool.Kind == "" {
			e.Tool.Kind = "other"
		}
		if e.Text == "" {
			e.Text = e.Tool.Name
		}
	case "todoList":
		// The agent's todo list as it stands after a write: one entry per
		// list, replaced in place by every write, never a card per write.
		todos := agent.Array(item["todos"])
		e.Text = todoTitle(todos)
		e.Detail = todoText(todos)
		e.Tool = &Tool{Kind: "todo", Name: agent.String(item["tool"]), Status: "completed", Input: map[string]any{"todos": todos}}
		e.IsStreaming = false
	case "compaction":
		// The agent compacted its context (Claude's /compact, or its own
		// auto-compaction near the window): a divider in the transcript
		// with the trigger and the token counts, and the summary the
		// agent continues from as the detail.
		e.Role = "compaction"
		n := func(k string) int64 { f, _ := item[k].(float64); return int64(f) }
		e.Compaction = &Compaction{Trigger: agent.String(item["trigger"]), PreTokens: n("preTokens"), PostTokens: n("postTokens"), Status: toolStatus(item), Error: agent.String(item["error"])}
		e.Detail = tail(agent.String(item["summary"]), 30000)
		switch e.Compaction.Status {
		case "running":
			e.Text = "Compacting context…"
		case "failed":
			e.Text = "Compaction failed"
		default:
			e.Text = "Context compacted"
		}
	case "reasoning":
		// The model's thinking (the long silence before a first reply is
		// usually this): its summary, or the text itself where the agent
		// streams that, as a thinking entry the transcript shows the way
		// the agent's own app does. EndedAt says how long it took.
		e.Role = "thinking"
		e.Text = reasoningSummary(item)
		if completed {
			e.EndedAt = now()
		}
	default:
		return
	}
	if completed && e.Tool != nil && e.Tool.Kind == "task" {
		// The subagent finished: the card says how long it took.
		e.EndedAt = now()
	}
	for i, old := range c.Entries {
		if old.ID == e.ID {
			e.CreatedAt = old.CreatedAt
			if e.Role == "user" {
				e.Sender = old.Sender
				e.Detail = old.Detail
				if old.Sender != nil {
					e.Text = old.Text
				}
			}
			if e.Text == "" && (!completed || e.Role == "thinking") {
				e.Text = old.Text
			}
			if old.EndedAt != 0 {
				e.EndedAt = old.EndedAt // an end, once seen, stays
			}
			c.Entries[i] = e
			return
		}
	}
	c.Entries = append(c.Entries, e)
}
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// toolStatus is an item's status in the transcript's words: running,
// completed or failed; Codex's inProgress is running, any other word of
// an agent's (declined) stays as it is.
func toolStatus(item map[string]any) string {
	switch s := agent.String(item["status"]); s {
	case "", "inProgress", "running":
		return "running"
	case "completed", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	default:
		return s
	}
}

// fileChangeTitle names a file change by its tool and path: "Edit
// chat/main.go", "Write notes.md", or a count when several files changed.
// Codex names no tool, so its single change reads by its kind.
func fileChangeTitle(tool string, changes []any) string {
	if len(changes) != 1 {
		return fmt.Sprintf("Updated %d files", len(changes))
	}
	m := agent.Map(changes[0])
	if tool == "" {
		switch agent.String(m["kind"]) {
		case "add":
			tool = "Add"
		case "delete":
			tool = "Delete"
		default:
			tool = "Update"
		}
	}
	return tool + " " + agent.String(m["path"])
}

// mcpResultText is an MCP call's result as text: its text content joined,
// or its error.
func mcpResultText(item map[string]any) string {
	if msg := agent.String(agent.Map(item["error"])["message"]); msg != "" {
		return msg
	}
	var parts []string
	for _, v := range agent.Array(agent.Map(item["result"])["content"]) {
		if m := agent.Map(v); agent.String(m["type"]) == "text" {
			parts = append(parts, agent.String(m["text"]))
		}
	}
	return strings.Join(parts, "\n")
}

// todoTitle names the todo list by its progress: "Todo list · 2 of 5
// done", or the item in progress when there is one.
func todoTitle(todos []any) string {
	done, active := 0, ""
	for _, v := range todos {
		m := agent.Map(v)
		switch agent.String(m["status"]) {
		case "completed":
			done++
		case "in_progress":
			if active == "" {
				active = agent.String(m["activeForm"])
				if active == "" {
					active = agent.String(m["content"])
				}
			}
		}
	}
	title := fmt.Sprintf("Todo list · %d of %d done", done, len(todos))
	if active != "" {
		title += " · " + active
	}
	return title
}

// todoText is the todo list as lines a plain surface can show: `[x]`
// done, `[>]` in progress, `[ ]` pending.
func todoText(todos []any) string {
	var b strings.Builder
	for _, v := range todos {
		m := agent.Map(v)
		mark := "[ ]"
		switch agent.String(m["status"]) {
		case "completed":
			mark = "[x]"
		case "in_progress":
			mark = "[>]"
		}
		b.WriteString(mark + " " + agent.String(m["content"]) + "\n")
	}
	return b.String()
}

// Delta appends streamed text to entry `id`, adding the entry when the
// agent streams before announcing it: `role` is what it then is, an
// assistant message, a thinking entry, or a command whose output the
// delta is.
func (c *Conversation) Delta(id, turn, delta, role string) {
	command := role == "activity"
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.ID == id {
			if command {
				e.Detail = tail(e.Detail+delta, 30000)
			} else {
				e.Text += delta
			}
			e.IsStreaming = true
			return
		}
	}
	e := NewEntry(role, delta)
	e.ID = id
	e.TurnID = Ptr(turn)
	e.IsStreaming = true
	if command {
		e.Text = "Running command"
		e.Detail = delta
		e.Tool = &Tool{Kind: "command", Status: "running"}
	}
	c.Entries = append(c.Entries, e)
}

// Break starts a new paragraph in thinking entry `id`: Codex streams a
// reasoning summary as parts, each of which reads on its own.
func (c *Conversation) Break(id string) {
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.ID == id && e.Text != "" && !strings.HasSuffix(e.Text, "\n\n") {
			e.Text = strings.TrimRight(e.Text, "\n") + "\n\n"
			return
		}
	}
}

// Turn is the record of turn `id`, added when there is none yet.
func (c *Conversation) Turn(id string) *Turn {
	for i := range c.Turns {
		if c.Turns[i].ID == id {
			return &c.Turns[i]
		}
	}
	c.Turns = append(c.Turns, Turn{ID: id})
	return &c.Turns[len(c.Turns)-1]
}

// Begin records that the agent accepted a message into turn `id` at `at`;
// a turn already begun (a steer joining it) keeps its start.
func (c *Conversation) Begin(id string, at float64) {
	if t := c.Turn(id); t.StartedAt == 0 {
		t.StartedAt = at
	}
}

// Report records the tokens turn `id` has used so far.
func (c *Conversation) Report(id string, usage Usage) {
	c.Turn(id).Usage = &usage
}

// Finish ends turn `id` at `at`: its entries stop streaming and its record
// gets its end (kept if the turn already ended).
func (c *Conversation) Finish(turn string, at float64) {
	if c.ActiveTurnID != nil && *c.ActiveTurnID == turn {
		c.ActiveTurnID = nil
	}
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.TurnID != nil && *e.TurnID == turn {
			if e.IsStreaming && e.Role == "thinking" && e.EndedAt == 0 {
				e.EndedAt = at
			}
			e.IsStreaming = false
		}
	}
	if t := c.Turn(turn); t.EndedAt == 0 {
		t.EndedAt = at
	}
}

// EndTurns ends every begun turn that has no end yet, for a run that
// stopped or failed without the agent completing its turn; thinking that
// was still streaming ends with it.
func (c *Conversation) EndTurns(at float64) {
	for i := range c.Turns {
		if c.Turns[i].EndedAt == 0 {
			c.Turns[i].EndedAt = at
		}
	}
	for i := range c.Entries {
		if e := &c.Entries[i]; e.IsStreaming && e.Role == "thinking" && e.EndedAt == 0 {
			e.EndedAt = at
		}
	}
}

// Match reconstructed server IDs by turn and message content while preserving
// live IDs, timestamps, local notices and unsent messages.
func (c *Conversation) Hydrate(thread map[string]any) {
	if id := agent.String(thread["id"]); id != "" {
		c.ThreadID = Ptr(id)
	}
	c.ActiveTurnID = nil
	previous := c.Entries
	restored := []Entry{}
	matched, duplicates := map[string]bool{}, map[string]bool{}
	for _, raw := range agent.Array(thread["turns"]) {
		turn := agent.Map(raw)
		id := agent.String(turn["id"])
		active := turn["status"] == "inProgress"
		for _, rawItem := range agent.Array(turn["items"]) {
			tmp := Conversation{}
			tmp.Upsert(agent.Map(rawItem), id, !active)
			if len(tmp.Entries) == 0 {
				continue
			}
			e := tmp.Entries[0]
			match := -1
			for i, p := range previous {
				if matched[p.ID] {
					continue
				}
				sameTurn := p.TurnID != nil && *p.TurnID == id
				if p.ID == e.ID && (sameTurn || p.TurnID == nil) {
					match = i
					break
				}
			}
			if match < 0 || strings.HasPrefix(e.ID, "item-") {
				for i, p := range previous {
					if !matched[p.ID] && p.TurnID != nil && *p.TurnID == id && p.Role == e.Role && (p.Role == "assistant" || p.Role == "user") && p.Text == e.Text {
						if match < 0 || !strings.HasPrefix(p.ID, "item-") {
							match = i
							break
						}
					}
				}
			}
			if match >= 0 {
				old := previous[match]
				if old.ID != e.ID {
					duplicates[e.ID] = true
				}
				e.ID = old.ID
				e.CreatedAt = old.CreatedAt
				if old.EndedAt != 0 {
					e.EndedAt = old.EndedAt
				}
				if e.Role == "user" {
					e.Sender = old.Sender
					e.Detail = old.Detail
					if old.Sender != nil {
						e.Text = old.Text
					}
				}
				if active && e.Text == "" {
					e.Text = old.Text
				}
			}
			matched[e.ID] = true
			restored = append(restored, e)
		}
		if active {
			c.ActiveTurnID = Ptr(id)
		}
	}
	ordered, pending := []Entry{}, []Entry{}
	cursor := 0
	appendLocal := func(end int) {
		for cursor < end {
			e := previous[cursor]
			if !matched[e.ID] && !duplicates[e.ID] {
				if e.Role == "user" && e.TurnID == nil {
					pending = append(pending, e)
				} else {
					ordered = append(ordered, e)
				}
			}
			cursor++
		}
	}
	for _, e := range restored {
		for i, p := range previous {
			if p.ID == e.ID {
				appendLocal(i)
				cursor = max(cursor, i+1)
				break
			}
		}
		ordered = append(ordered, e)
	}
	appendLocal(len(previous))
	c.Entries = append(ordered, pending...)
}

// reasoningSummary joins a reasoning item's summary, whichever shape the
// agent used: strings, or objects with a text field.
func reasoningSummary(item map[string]any) string {
	var parts []string
	for _, v := range agent.Array(item["summary"]) {
		text := ""
		switch t := v.(type) {
		case string:
			text = t
		case map[string]any:
			text = agent.String(t["text"])
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
