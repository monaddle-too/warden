package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// A side question (the composer's "/btw"; docs/claude-parity.md, item
// 15 and R2.19): answered from a copy of the chat's Claude session by a
// one-shot CLI the runner starts beside the resident one (sandbox/aside.go),
// so the answer comes from the conversation's context without entering
// it. The question and its answer are one `aside` entry in the
// transcript, which the agent never sees: the recap skips it, and it is
// no turn. A released session (idle past its timeout) is brought up for
// the question the way a message brings it up — the run starts with
// nothing to send and waits idle (engine.go, run) — since the gateway
// serves the provider credential only for a live run; the entry says
// "starting" meanwhile and the chat shows its startup stages. A
// completed aside can be promoted into the chat as a message of the
// person's ("Ask in chat"): the question with the answer quoted, so the
// agent can build on it (PromoteAside).

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

// AsideStartTimeout bounds how long a side question waits for the chat's
// released session to come up (a cold sandbox start included).
const AsideStartTimeout = 5 * time.Minute

// Aside asks question of a copy of chat id's agent session, by actor. The
// chat must be a Claude chat with a session: a running turn (the answer
// would race the turn) and Codex are refused, so the person can ask
// again after the turn; a released session is started for the question,
// unless the chat's queue is held (starting the session would send the
// held messages: they are sent or withdrawn first). One side question
// at a time per chat.
func (e *Engine) Aside(ctx context.Context, id, question string, actor cv.Actor) (AsideResult, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	question = strings.TrimSpace(question)
	if question == "" || len(question) > sandbox.MaxAsideQuestion || strings.ContainsRune(question, 0) {
		return AsideResult{}, errors.New("a question of at most 16 KiB is required")
	}
	e.mu.Lock()
	live := e.sessionLiveLocked(id)
	if e.asides == nil {
		e.asides = map[string]bool{}
	}
	busy := e.asides[id]
	if !busy {
		e.asides[id] = true
	}
	e.mu.Unlock()
	if busy {
		return AsideResult{}, errors.New("a side question is being answered; ask the next one when it is")
	}
	release := func() {
		e.mu.Lock()
		delete(e.asides, id)
		e.mu.Unlock()
	}
	entry := cv.NewEntry("aside", question)
	sender := actor
	entry.Sender = &sender
	entry.IsStreaming = true
	entry.Aside = &cv.Aside{Status: "running"}
	start := false
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
			return errors.New("the agent's turn is running; ask the side question after this turn")
		}
		if c.Conversation.ThreadID == nil || c.NewSession {
			return errors.New("the chat has no agent session yet; send a message first")
		}
		if !live {
			// The session was released: bring it up for the question, as
			// a message would. A held queue would go with the start, so
			// it is the person's call first.
			if held := len(queuedEntries(c)); held > 0 {
				return fmt.Errorf("the agent's session is not running and %d message%s held in the queue would go with its start; send or withdraw %s first", held, plural(held, " is", "s are"), plural(held, "it", "them"))
			}
			entry.Aside.Status = "starting"
			c.Status = "queued"
			c.Error = ""
			start = true
		}
		c.Conversation.Entries = append(c.Conversation.Entries, entry)
		return nil
	})
	if err != nil {
		release()
		return AsideResult{}, err
	}
	if start {
		e.Wake()
		log.Printf("chat %s: starting the agent session for %s's side question", id, actorLabel(actor))
		if err = e.awaitSession(ctx, id); err != nil {
			release()
			return e.finishAside(id, entry.ID, actor, sandbox.AsideResult{Error: err.Error()}, nil)
		}
	}
	var req sandbox.Request
	err = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if c.Conversation.ThreadID == nil {
			return errors.New("the chat has no agent session")
		}
		req = request(c, "aside")
		req.ThreadID = *c.Conversation.ThreadID
		req.OutputStyle = c.OutputStyle
		for i := range c.Conversation.Entries {
			if v := &c.Conversation.Entries[i]; v.ID == entry.ID && v.Aside != nil {
				v.Aside.Status = "running"
			}
		}
		return nil
	})
	if err != nil {
		release()
		return e.finishAside(id, entry.ID, actor, sandbox.AsideResult{Error: err.Error()}, nil)
	}
	req.PrincipalID = actor.PrincipalID
	req.Command = question
	res, callErr := e.Worker.Call(ctx, req)
	release()
	result := sandbox.AsideResult{}
	switch {
	case callErr != nil:
		result.Error = callErr.Error()
	case res.Aside == nil:
		result.Error = "the runner gave no answer"
	default:
		result = *res.Aside
	}
	return e.finishAside(id, entry.ID, actor, result, callErr)
}

// finishAside completes the aside entry with what the one-shot came to
// and answers the route; callErr, when the runner refused, is returned
// as the error beside the (failed) result.
func (e *Engine) finishAside(id, entryID string, actor cv.Actor, res sandbox.AsideResult, callErr error) (AsideResult, error) {
	aside := cv.Aside{Status: "completed", CostUSD: res.CostUSD, Input: res.Input, Output: res.Output, DurationMS: res.DurationMS}
	if res.Error != "" {
		aside.Status, aside.Error = "failed", res.Error
	}
	result := AsideResult{ID: entryID, Text: res.Text, Error: aside.Error, Cost: aside.CostUSD, Input: aside.Input, Output: aside.Output}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.ID == entryID {
				v.Detail = res.Text
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

// sessionLiveLocked reports whether chat id's resident session is up and
// idle (the engine lock held).
func (e *Engine) sessionLiveLocked(id string) bool {
	a := e.active[id]
	return a != nil && a.client != nil && a.idle.Load()
}

// awaitSession waits for chat id's session, queued for a start, to be up
// and idle: nil then; an error when the start failed (the chat's error),
// the chat was stopped or archived meanwhile, the run ended without a
// session, ctx ended, or AsideStartTimeout passed.
func (e *Engine) awaitSession(ctx context.Context, id string) error {
	deadline := time.NewTimer(AsideStartTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("the request ended while the agent's session was starting")
		case <-deadline.C:
			return errors.New("the agent's session did not start within " + AsideStartTimeout.String())
		case <-tick.C:
		}
		e.mu.Lock()
		live := e.sessionLiveLocked(id)
		active := e.active[id] != nil
		e.mu.Unlock()
		if live {
			return nil
		}
		c := e.Store.Chat(id)
		switch {
		case c == nil:
			return errors.New("chat not found")
		case c.Archived:
			return errors.New("the chat was archived")
		case c.Status == "failed":
			if c.Error != "" {
				return errors.New("the agent's session could not start: " + c.Error)
			}
			return errors.New("the agent's session could not start")
		case c.Status == "stopping" || c.Status == "interrupted":
			return errors.New("the agent was stopped while its session was starting")
		case c.Status == "idle" && !active:
			return errors.New("the agent's session ended before the question could be asked")
		}
	}
}

// queuedEntries are the chat's messages waiting to be sent.
func queuedEntries(c *Chat) []cv.Entry {
	var out []cv.Entry
	for _, v := range c.Conversation.Entries {
		if v.Delivery == "queued" {
			out = append(out, v)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// PromoteResult is what PromoteAside answers: the message the question
// went as and its text.
type PromoteResult struct {
	MessageID string `json:"messageID"`
	Text      string `json:"text"`
}

// PromoteAside sends the side question of aside entry entryID in chat id
// as a message of actor's — the question with the aside's answer quoted
// under it, so the agent can build on the answer — through the ordinary
// message path (queued behind a running turn like any message). The
// aside must have completed with an answer and not have been promoted
// before; the entry then records the message (Aside.Promoted).
func (e *Engine) PromoteAside(id, entryID string, actor cv.Actor) (PromoteResult, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	var text string
	c := e.Store.Chat(id)
	if c == nil {
		return PromoteResult{}, errors.New("chat not found")
	}
	var aside *cv.Entry
	for i := range c.Conversation.Entries {
		if v := &c.Conversation.Entries[i]; v.ID == entryID && v.Role == "aside" {
			aside = v
		}
	}
	switch {
	case aside == nil:
		return PromoteResult{}, errors.New("no such side question")
	case aside.Aside == nil || aside.Aside.Status == "running" || aside.Aside.Status == "starting":
		return PromoteResult{}, errors.New("the side question is still being answered")
	case aside.Aside.Status != "completed" || strings.TrimSpace(aside.Detail) == "":
		return PromoteResult{}, errors.New("the side question got no answer to build on; ask it in chat as a message")
	case aside.Aside.Promoted != "":
		return PromoteResult{}, errors.New("the side question was already asked in chat")
	}
	text = PromotedText(aside.Text, aside.Detail)
	messageID := cv.ID()
	if err := e.MessageFrom(id, text, messageID, actor); err != nil {
		return PromoteResult{}, err
	}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		for i := range c.Conversation.Entries {
			if v := &c.Conversation.Entries[i]; v.ID == entryID && v.Aside != nil {
				v.Aside.Promoted = messageID
			}
		}
		return nil
	})
	log.Printf("chat %s: %s asked a side question in chat", id, actorLabel(actor))
	return PromoteResult{MessageID: messageID, Text: text}, nil
}

// PromotedText is the message a promoted side question goes as: the
// question, then the answer quoted as what a side question (which the
// agent never saw) came to.
func PromotedText(question, answer string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(question))
	b.WriteString("\n\n(I asked this as a side question of a copy of your session; its answer, to build on:)\n")
	for _, line := range strings.Split(strings.TrimSpace(answer), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
