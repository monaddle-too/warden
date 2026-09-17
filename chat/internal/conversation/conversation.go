package conversation

import (
	"fmt"
	"strings"
	"warden/chat/internal/agent"
)

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
			if e.Text == "" && !completed {
				e.Text = old.Text
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
func (c *Conversation) Delta(id, turn, delta string, command bool) {
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
	e := NewEntry("assistant", delta)
	e.ID = id
	e.TurnID = Ptr(turn)
	e.IsStreaming = true
	if command {
		e.Role = "activity"
		e.Text = "Running command"
		e.Detail = delta
	}
	c.Entries = append(c.Entries, e)
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
			e.IsStreaming = false
		}
	}
	if t := c.Turn(turn); t.EndedAt == 0 {
		t.EndedAt = at
	}
}

// EndTurns ends every begun turn that has no end yet, for a run that
// stopped or failed without the agent completing its turn.
func (c *Conversation) EndTurns(at float64) {
	for i := range c.Turns {
		if c.Turns[i].EndedAt == 0 {
			c.Turns[i].EndedAt = at
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
