package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// Checkpoints, rewind and the session diff (docs/claude-parity.md, item
// 11). Before every user turn the runner snapshots the workspace as a
// checkpoint named after the message (sandbox/checkpoint.go). Rewinding
// code restores that snapshot; rewinding the conversation truncates the
// transcript to before the message and asks the agent's session to forget
// the same — Claude Code's `rewind_conversation`, through the adapter's
// `conversation/rewind` (on a live idle session at once, otherwise right
// after the next resume) — falling back to a fresh session given the kept
// transcript as context when the agent cannot. The session diff is the
// workspace now against the chat's first checkpoint, or the one its last
// code rewind restored.

// PendingRewind is a conversation rewind the agent's session has yet to
// apply: recorded on the chat when no session was live, applied by the
// next run once its session is resumed and before its first turn.
type PendingRewind struct {
	TargetID   string `json:"targetID"`
	LastSeenID string `json:"lastSeenID,omitempty"`
}

// RewindResult is what a rewind did.
type RewindResult struct {
	MessageID string `json:"messageID"`
	What      string `json:"what"`
	// Restored and Removed are the workspace paths a code rewind wrote
	// back and deleted.
	Restored []string `json:"restored,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	// Conversation says how the agent's session followed a conversation
	// rewind: "rewound" (the session forgot the messages), "pending"
	// (applied when the session next resumes), "fresh" (the session could
	// not rewind; the next message starts a new one with the kept
	// transcript as context), "" for a code-only rewind.
	Conversation string `json:"conversation,omitempty"`
	// Withdrawn counts the queued messages a conversation rewind took
	// out of the queue (they were never handed to the agent).
	Withdrawn int `json:"withdrawn,omitempty"`
}

// maxRecap bounds the transcript re-sent to a fresh session.
const maxRecap = 24 << 10

// checkpointTimeout bounds the snapshot taken before a turn; a workspace
// that takes longer goes without one (logged).
const checkpointTimeout = 90 * time.Second

// checkpoint records the workspace before message messageID is handed to
// the agent. A failure is logged and does not stop the turn; the chat's
// first checkpoint becomes its diff base.
func (e *Engine) checkpoint(ctx context.Context, c *Chat, messageID string) {
	r := request(c, "checkpoint")
	r.CallID = messageID
	cctx, cancel := context.WithTimeout(ctx, checkpointTimeout)
	res, err := e.Worker.Call(cctx, r)
	cancel()
	if err != nil || res.Checkpoint == nil {
		if err == nil {
			err = errors.New("runner returned no checkpoint")
		}
		log.Printf("chat %s: no checkpoint before message %s: %v", c.ID, messageID, err)
		return
	}
	_ = e.Store.update(func(st *State) error {
		if c := st.chat(c.ID); c != nil && c.DiffBase == "" {
			c.DiffBase = res.Checkpoint.ID
		}
		return nil
	})
}

// beforeTurn is what happens between a message's input being ready and
// its turn starting: the workspace checkpoint named after the message,
// and the recap a fresh session gets ahead of it (cleared by confirm once
// the turn is accepted).
func (e *Engine) beforeTurn(ctx context.Context, id string, c *Chat, messageID string, items []any) []any {
	e.setStartup(id, stageSending, "recording a checkpoint of the workspace")
	e.checkpoint(ctx, c, messageID)
	e.setStartup(id, stageSending, "handing your message to the agent")
	if current := e.Store.Snapshot().chat(id); current != nil && current.Recap != "" {
		items = append([]any{map[string]any{"type": "text", "text": current.Recap, "text_elements": []any{}}}, items...)
	}
	return items
}

// Checkpoints lists the chat's checkpoints as the runner holds them.
func (e *Engine) Checkpoints(ctx context.Context, id string) ([]sandbox.Checkpoint, error) {
	c := e.Store.Snapshot().chat(id)
	if c == nil {
		return nil, errors.New("chat not found")
	}
	res, err := e.Worker.Call(ctx, request(c, "checkpoints"))
	if err != nil {
		return nil, err
	}
	list := res.Checkpoints
	if list == nil {
		list = []sandbox.Checkpoint{}
	}
	return list, nil
}

// Diff is the workspace's changes since the chat's diff base: its first
// checkpoint, or the one its last code rewind restored.
func (e *Engine) Diff(ctx context.Context, id string) (*sandbox.WorkspaceChanges, error) {
	c := e.Store.Snapshot().chat(id)
	if c == nil {
		return nil, errors.New("chat not found")
	}
	if c.DiffBase == "" {
		return nil, errors.New("no checkpoint yet: the first one is taken when the agent gets a message")
	}
	r := request(c, "diff")
	r.CallID = c.DiffBase
	res, err := e.Worker.Call(ctx, r)
	if err != nil {
		return nil, err
	}
	if res.Changes == nil {
		return nil, errors.New("runner returned no diff")
	}
	return res.Changes, nil
}

// Rewind takes the chat back to before one of its user messages: turnID
// is the message's turn or the message's own ID; what is "code",
// "conversation" or "both". The chat must be idle.
func (e *Engine) Rewind(ctx context.Context, id, turnID, what string) (RewindResult, error) {
	if what != "code" && what != "conversation" && what != "both" {
		return RewindResult{}, errors.New("what must be code, conversation or both")
	}
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return RewindResult{}, errors.New("chat not found")
	}
	if c.Archived || st.deleted(c.SandboxID) {
		return RewindResult{}, errors.New("this chat is archived or its workspace was deleted")
	}
	if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
		return RewindResult{}, errors.New("wait for the agent to finish, or stop it, before rewinding")
	}
	target := -1
	for i, v := range c.Conversation.Entries {
		if v.Role == "user" && v.ParentID == "" && (v.ID == turnID || (v.TurnID != nil && *v.TurnID == turnID)) {
			target = i
			break
		}
	}
	if target == -1 {
		return RewindResult{}, errors.New("no such message in this chat")
	}
	message := c.Conversation.Entries[target]
	result := RewindResult{MessageID: message.ID, What: what}
	if what != "conversation" {
		r := request(c, "restore")
		r.CallID = message.ID
		res, err := e.Worker.Call(ctx, r)
		if err != nil {
			return RewindResult{}, err
		}
		if res.Restore == nil {
			return RewindResult{}, errors.New("runner returned no restore")
		}
		result.Restored, result.Removed = res.Restore.Restored, res.Restore.Removed
	}
	if what != "code" {
		result.Conversation = e.rewindConversation(ctx, c, message)
	}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if what != "code" {
			// The queue goes with the conversation: a message queued
			// behind the rewound turn was written for the old thread.
			result.Withdrawn = withdrawQueued(c)
			truncate(c, message.ID)
		}
		if what != "conversation" {
			c.DiffBase = message.ID
		}
		marker := cv.NewEntry("rewind", rewindLabel(message.Text, what))
		marker.CreatedAt = e.at()
		marker.Detail = rewindDetail(result)
		c.Conversation.Entries = append(c.Conversation.Entries, marker)
		return nil
	})
	if err != nil {
		return RewindResult{}, err
	}
	e.Wake()
	return result, nil
}

// rewindConversation asks the agent's session to forget message and what
// followed it, and says how that went (RewindResult.Conversation). A chat
// that never had a session has nothing to forget. The command is the
// adapter's to answer whatever the provider: Claude Code rewinds, an agent
// without a rewind (Codex today) refuses and gets the fresh session.
func (e *Engine) rewindConversation(ctx context.Context, c *Chat, message cv.Entry) string {
	if c.Conversation.ThreadID == nil {
		return "rewound"
	}
	lastSeen := ""
	for i := len(c.Conversation.Entries) - 1; i >= 0; i-- {
		if v := c.Conversation.Entries[i]; v.Role == "user" && v.ParentID == "" && v.Delivery == "sent" {
			lastSeen = v.ID
			break
		}
	}
	e.mu.Lock()
	a := e.active[c.ID]
	var client *agent.Client
	thread := ""
	if a != nil && a.idle.Load() {
		client, thread = a.client, a.threadID
	}
	e.mu.Unlock()
	fresh := func() string {
		_ = e.Store.update(func(st *State) error {
			if c := st.chat(c.ID); c != nil {
				c.Conversation.ThreadID = nil
				c.NewSession = true
				c.Rewind = nil
				c.Recap = recap(kept(c.Conversation.Entries, message.ID))
			}
			return nil
		})
		e.releaseChat(ctx, c.ID)
		return "fresh"
	}
	if client == nil {
		_ = e.Store.update(func(st *State) error {
			if c := st.chat(c.ID); c != nil {
				c.Rewind = &PendingRewind{TargetID: message.ID, LastSeenID: lastSeen}
			}
			return nil
		})
		return "pending"
	}
	if e.callRewind(ctx, client, thread, message.ID, lastSeen) {
		return "rewound"
	}
	return fresh()
}

// callRewind sends the adapter's `conversation/rewind` and reports whether
// the session rewound.
func (e *Engine) callRewind(ctx context.Context, client *agent.Client, thread, target, lastSeen string) bool {
	callCtx, done := context.WithTimeout(ctx, 20*time.Second)
	defer done()
	result, err := client.Call(callCtx, "conversation/rewind", map[string]any{"threadId": thread, "targetMessageId": target, "lastSeenMessageId": lastSeen})
	if err != nil {
		log.Printf("conversation rewind to %s refused: %v", target, err)
		return false
	}
	if result["rewound"] != true {
		log.Printf("conversation rewind to %s not applied: %s", target, agent.String(result["reason"]))
		return false
	}
	return true
}

// applyPendingRewind runs a rewind recorded while no session was live, on
// the session just resumed and before its first turn. It reports false
// when the session could not rewind: the run then ends so the next one
// starts fresh with the kept transcript as context.
func (e *Engine) applyPendingRewind(ctx context.Context, id string, client *agent.Client, thread string) bool {
	c := e.Store.Snapshot().chat(id)
	if c == nil || c.Rewind == nil {
		return true
	}
	pending := *c.Rewind
	if e.callRewind(ctx, client, thread, pending.TargetID, pending.LastSeenID) {
		_ = e.Store.update(func(st *State) error {
			if c := st.chat(id); c != nil {
				c.Rewind = nil
			}
			return nil
		})
		return true
	}
	_ = e.Store.update(func(st *State) error {
		if c := st.chat(id); c != nil {
			c.Rewind = nil
			c.Conversation.ThreadID = nil
			c.NewSession = true
			c.Recap = recap(c.Conversation.Entries)
		}
		return nil
	})
	return false
}

// truncate drops message messageID and everything after it from the
// transcript (a conversation rewind withdraws the queue first,
// withdrawQueued); the turns of the dropped entries go with them.
func truncate(c *Chat, messageID string) {
	entries := c.Conversation.Entries
	at := -1
	for i, v := range entries {
		if v.ID == messageID {
			at = i
			break
		}
	}
	if at == -1 {
		return
	}
	kept := append([]cv.Entry(nil), entries[:at]...)
	turns := map[string]bool{}
	for _, v := range kept {
		if v.TurnID != nil {
			turns[*v.TurnID] = true
		}
	}
	var keptTurns []cv.Turn
	for _, t := range c.Conversation.Turns {
		if turns[t.ID] {
			keptTurns = append(keptTurns, t)
		}
	}
	c.Conversation.Entries = kept
	c.Conversation.Turns = keptTurns
	for i := range c.Approvals {
		if c.Approvals[i].State == "pending" || c.Approvals[i].State == "resolving" {
			c.Approvals[i].State = "expired"
		}
	}
}

// kept is the transcript before messageID.
func kept(entries []cv.Entry, messageID string) []cv.Entry {
	for i, v := range entries {
		if v.ID == messageID {
			return entries[:i]
		}
	}
	return entries
}

// recap renders a transcript for a fresh session to continue from: the
// people's and the agent's messages and the agent's top-level tool calls
// by name and title, the last maxRecap bytes when longer. Empty for a
// transcript with nothing to continue from.
func recap(entries []cv.Entry) string {
	var b strings.Builder
	for _, v := range entries {
		switch v.Role {
		case "user":
			fmt.Fprintf(&b, "User: %s\n\n", strings.TrimSpace(v.Text))
		case "assistant":
			if strings.TrimSpace(v.Text) != "" {
				fmt.Fprintf(&b, "Assistant: %s\n\n", strings.TrimSpace(v.Text))
			}
		case "activity":
			if v.Tool != nil && v.ParentID == "" && v.Text != "" {
				fmt.Fprintf(&b, "[%s] %s\n\n", v.Tool.Name, strings.TrimSpace(v.Text))
			}
		}
	}
	s := b.String()
	if s == "" {
		return ""
	}
	if len(s) > maxRecap {
		s = "…\n" + s[len(s)-maxRecap:]
	}
	return "Context: this session was started after the conversation was rewound; the transcript so far is below. Continue from it without repeating it.\n\n" + s
}

// rewindLabel is the marker's text: which message and what was rewound.
func rewindLabel(text, what string) string {
	excerpt := strings.Join(strings.Fields(text), " ")
	if r := []rune(excerpt); len(r) > 60 {
		excerpt = string(r[:60]) + "…"
	}
	if excerpt == "" {
		excerpt = "(attachments)"
	}
	label := map[string]string{"code": "code", "conversation": "conversation", "both": "code and conversation"}[what]
	return fmt.Sprintf("Rewound to before “%s” (%s)", excerpt, label)
}

// rewindDetail is the marker's second line: the files a code rewind moved
// and how the agent's session followed a conversation rewind.
func rewindDetail(r RewindResult) string {
	var parts []string
	if r.What != "conversation" {
		switch {
		case len(r.Restored)+len(r.Removed) == 0:
			parts = append(parts, "the workspace already matched the checkpoint")
		default:
			parts = append(parts, fmt.Sprintf("%s restored, %s removed", count(len(r.Restored), "file"), count(len(r.Removed), "file")))
		}
	}
	switch r.Conversation {
	case "pending":
		parts = append(parts, "the agent forgets the messages when its session resumes")
	case "fresh":
		parts = append(parts, "the agent's session could not rewind; the next message starts a new one with the conversation so far as context")
	}
	if r.Withdrawn > 0 {
		parts = append(parts, count(r.Withdrawn, "queued message")+" withdrawn")
	}
	return strings.Join(parts, "; ")
}

func count(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
