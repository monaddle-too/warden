package chats

import (
	"context"
	"errors"
	"log"
	"strings"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// A side question (the composer's "/btw"; docs/claude-parity.md, item
// 15): answered from a copy of the chat's Claude session by a one-shot
// CLI the runner starts beside the resident one (sandbox/aside.go), so
// the answer comes from the conversation's context without entering it.
// The question and its answer are one `aside` entry in the transcript,
// which the agent never sees: the recap skips it, and it is no turn.

// AsideResult is what a side question came to, as the route answers it.
type AsideResult struct {
	// ID is the transcript entry (the question, then the answer).
	ID     string  `json:"id"`
	Text   string  `json:"text,omitempty"`
	Error  string  `json:"error,omitempty"`
	Cost   float64 `json:"costUSD,omitempty"`
	Input  int64   `json:"input,omitempty"`
	Output int64   `json:"output,omitempty"`
}

// Aside asks question of a copy of chat id's agent session, by actor. The
// chat must be a Claude chat whose session is up and idle: a running
// turn, a released session (idle past its timeout, or the workspace
// stopped) and Codex are refused, so the person can ask again later or
// send a message first. One side question at a time per chat.
func (e *Engine) Aside(ctx context.Context, id, question string, actor cv.Actor) (AsideResult, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	question = strings.TrimSpace(question)
	if question == "" || len(question) > sandbox.MaxAsideQuestion || strings.ContainsRune(question, 0) {
		return AsideResult{}, errors.New("a question of at most 16 KiB is required")
	}
	e.mu.Lock()
	a := e.active[id]
	live := a != nil && a.client != nil && a.idle.Load()
	if e.asides == nil {
		e.asides = map[string]bool{}
	}
	busy := e.asides[id]
	if live && !busy {
		e.asides[id] = true
	}
	e.mu.Unlock()
	if busy {
		return AsideResult{}, errors.New("a side question is being answered; ask the next one when it is")
	}
	var req sandbox.Request
	entry := cv.NewEntry("aside", question)
	sender := actor
	entry.Sender = &sender
	entry.IsStreaming = true
	entry.Aside = &cv.Aside{Status: "running"}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if st.deleted(c.SandboxID) {
			return errors.New("this chat's workspace was deleted; start a new chat")
		}
		if c.Archived {
			return errors.New("chat is archived")
		}
		if c.Provider != "claude" {
			return errors.New("side questions are a Claude chat's; Codex has no session to copy")
		}
		if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
			return errors.New("wait for the agent's turn to finish before asking a side question")
		}
		if c.Conversation.ThreadID == nil || c.ForkSession || c.NewSession {
			return errors.New("the chat has no agent session yet; send a message first")
		}
		if !live {
			return errors.New("the agent's session is not running (it is released after a while idle); send a message first, then ask while it is idle")
		}
		req = request(c, "aside")
		req.ThreadID = *c.Conversation.ThreadID
		req.OutputStyle = c.OutputStyle
		c.Conversation.Entries = append(c.Conversation.Entries, entry)
		return nil
	})
	if err != nil {
		e.mu.Lock()
		delete(e.asides, id)
		e.mu.Unlock()
		return AsideResult{}, err
	}
	req.PrincipalID = actor.PrincipalID
	req.Command = question
	res, callErr := e.Worker.Call(ctx, req)
	e.mu.Lock()
	delete(e.asides, id)
	e.mu.Unlock()
	result := AsideResult{ID: entry.ID}
	aside := cv.Aside{Status: "completed"}
	answer := ""
	switch {
	case callErr != nil:
		aside.Status, aside.Error = "failed", callErr.Error()
	case res.Aside == nil:
		aside.Status, aside.Error = "failed", "the runner gave no answer"
	default:
		answer = res.Aside.Text
		aside.CostUSD, aside.Input, aside.Output, aside.DurationMS = res.Aside.CostUSD, res.Aside.Input, res.Aside.Output, res.Aside.DurationMS
		if res.Aside.Error != "" {
			aside.Status, aside.Error = "failed", res.Aside.Error
		}
	}
	result.Text, result.Error, result.Cost, result.Input, result.Output = answer, aside.Error, aside.CostUSD, aside.Input, aside.Output
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.ID == entry.ID {
				v.Detail = answer
				v.IsStreaming = false
				v.EndedAt = e.at()
				copy := aside
				v.Aside = &copy
			}
		}
		return nil
	})
	log.Printf("chat %s: %s asked a side question: %s", id, actorLabel(actor), aside.Status)
	if callErr != nil {
		return result, callErr
	}
	return result, nil
}
