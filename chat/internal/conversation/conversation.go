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
		e.Detail = agent.String(item["status"]) + "\n" + tail(agent.String(item["aggregatedOutput"]), 30000)
	case "fileChange":
		changes := agent.Array(item["changes"])
		e.Text = fmt.Sprintf("Updated %d files", len(changes))
		for _, ch := range changes {
			m := agent.Map(ch)
			e.Detail += agent.String(m["path"]) + "\n" + agent.String(m["diff"]) + "\n"
		}
	case "dynamicToolCall":
		e.Text = agent.String(item["tool"])
		e.Detail = agent.String(item["status"])
	case "mcpToolCall":
		e.Text = agent.String(item["server"]) + " · " + agent.String(item["tool"])
	case "webSearch":
		e.Text = "Search: " + agent.String(item["query"])
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
			if e.Role == "thinking" && old.EndedAt != 0 {
				e.EndedAt = old.EndedAt
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
