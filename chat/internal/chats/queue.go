package chats

import (
	"errors"

	cv "warden/chat/internal/conversation"
)

// The message queue (docs/claude-parity.md, item 10). A message sent
// while the agent's turn runs is held by Warden as a user entry with
// `Delivery: queued`, in transcript order, and becomes its own turn once
// the turn ends (engine.go: settleTurn, awaitMessage, resume). Until it is
// handed over it can be withdrawn — edited on a surface by withdrawing it
// into the composer and sending it again — and Stop holds the queue: an
// interrupted chat keeps its queued entries until SendQueued or a new
// message lets them go.

// Withdraw takes a queued message out of the chat before the agent gets
// it and returns the entry (its text and attachments, for a composer).
// The message's sender or the owner may; a message already handed over
// is refused. A chat left with nothing queued while it waited for a run
// goes back to idle.
func (e *Engine) Withdraw(id, messageID string, actor cv.Actor) (cv.Entry, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	var out cv.Entry
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		at := -1
		for i, v := range c.Conversation.Entries {
			if v.ID == messageID && v.Role == "user" {
				at = i
				break
			}
		}
		if at == -1 {
			return errors.New("no such message in this chat")
		}
		v := c.Conversation.Entries[at]
		if v.Delivery != "queued" {
			return errors.New("that message was already sent to the agent")
		}
		sender := "owner"
		if v.Sender != nil && v.Sender.PrincipalID != "" {
			sender = v.Sender.PrincipalID
		}
		if actor.PrincipalID != "owner" && actor.PrincipalID != sender {
			return errors.New("only the sender or the owner can withdraw a queued message")
		}
		out = v
		c.Conversation.Entries = append(c.Conversation.Entries[:at:at], c.Conversation.Entries[at+1:]...)
		if c.Status == "queued" && !queuedLeft(c) {
			// Waiting for a run (or between turns) with nothing to send:
			// the run finds the chat no longer queued and does nothing.
			c.Status = "idle"
		}
		return nil
	})
	if err == nil {
		e.Wake()
	}
	return out, err
}

// SendQueued lets a held queue go: a chat whose Stop interrupted the turn
// (or whose run ended) with messages still queued is marked queued again,
// so its session takes the first of them as the next turn. A chat with
// nothing queued, or one already running, is left as it is.
func (e *Engine) SendQueued(id string) error {
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if st.deleted(c.SandboxID) {
			return errors.New("this chat's workspace was deleted; start a new chat")
		}
		if c.Archived || c.Status == "stopping" {
			return errors.New("chat is archived or stopping")
		}
		if !queuedLeft(c) {
			return errors.New("nothing is queued")
		}
		if c.Status != "running" && c.Status != "queued" {
			c.Status = "queued"
			c.Error = ""
		}
		return nil
	})
	if err == nil {
		e.Wake()
	}
	return err
}

// A queued entry sits where it was sent until confirm hands it over and
// moves it to the end of the transcript, so the surfaces render queued
// entries last (the web's queuedLast, the TUI's) and the persisted order
// agrees once the message opens its turn.

// queuedLeft reports whether the chat still has a message waiting for
// the agent.
func queuedLeft(c *Chat) bool {
	for _, v := range c.Conversation.Entries {
		if v.Role == "user" && v.Delivery == "queued" {
			return true
		}
	}
	return false
}

// withdrawQueued drops every queued message from the chat (a rewind of
// the conversation takes the queue with it) and says how many went.
func withdrawQueued(c *Chat) int {
	kept := c.Conversation.Entries[:0:0]
	n := 0
	for _, v := range c.Conversation.Entries {
		if v.Role == "user" && v.Delivery == "queued" {
			n++
			continue
		}
		kept = append(kept, v)
	}
	c.Conversation.Entries = kept
	return n
}
