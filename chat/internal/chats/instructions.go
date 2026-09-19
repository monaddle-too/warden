package chats

import (
	"fmt"
	"strings"
	cv "warden/chat/internal/conversation"
)

// A person's standing instructions for the agent (parity item 13): what
// they would put in a user-level CLAUDE.md. They live in Warden's store
// keyed by principal, never in a sandbox — a workspace is shared by chats
// and people, and the sandbox home is the agent's — and reach the agent
// as text appended to the system prompt when a session starts, for the
// chat's creator and everyone who has sent into it. A live session is
// relaunched when a sender's current text is not what it was given.

// Instructions is one person's text with when they last changed it.
type Instructions struct {
	Text      string  `json:"text"`
	UpdatedAt float64 `json:"updatedAt,omitempty"`
	// Name is what the person was called when they last saved, so a block
	// can name them when no entry of theirs carries a name.
	Name string `json:"name,omitempty"`
}

// MaxInstructions bounds one person's text.
const MaxInstructions = 16 << 10

// Instructions is actor's own text, empty when they have none.
func (e *Engine) Instructions(actor cv.Actor) Instructions {
	st := e.Store.Snapshot()
	if v := st.Instructions[principalOf(actor)]; v != nil {
		return *v
	}
	return Instructions{}
}

// SetInstructions replaces actor's text; blank removes it.
func (e *Engine) SetInstructions(actor cv.Actor, text string) error {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")
	if len(text) > MaxInstructions || strings.ContainsRune(text, 0) {
		return fmt.Errorf("instructions must be text of at most %d KiB", MaxInstructions>>10)
	}
	principal := principalOf(actor)
	return e.Store.update(func(st *State) error {
		if strings.TrimSpace(text) == "" {
			delete(st.Instructions, principal)
			return nil
		}
		if st.Instructions == nil {
			st.Instructions = map[string]*Instructions{}
		}
		st.Instructions[principal] = &Instructions{Text: text, UpdatedAt: e.at(), Name: actorName(actor)}
		return nil
	})
}

func principalOf(a cv.Actor) string {
	if a.PrincipalID == "" {
		return "owner"
	}
	return a.PrincipalID
}

// actorName is how a block names its person: their name, else their
// email, else "the owner" for the owner principal, else nothing (the
// caller falls back to what the store remembers).
func actorName(a cv.Actor) string {
	switch {
	case strings.TrimSpace(a.Name) != "":
		return strings.TrimSpace(a.Name)
	case strings.TrimSpace(a.Email) != "":
		return strings.TrimSpace(a.Email)
	case principalOf(a) == "owner":
		return "the owner"
	}
	return ""
}

// participants are the people whose instructions a session gets: the
// chat's creator, then every sender of a user message in order of first
// appearance, each once. The actor kept for a principal is the latest one
// seen, since a name may arrive on a later message.
func participants(c *Chat) []cv.Actor {
	var order []string
	seen := map[string]cv.Actor{}
	note := func(a *cv.Actor) {
		if a == nil {
			return
		}
		p := principalOf(*a)
		if _, ok := seen[p]; !ok {
			order = append(order, p)
		}
		seen[p] = *a
	}
	note(c.Creator)
	for _, v := range c.Conversation.Entries {
		if v.Role == "user" {
			note(v.Sender)
		}
	}
	list := make([]cv.Actor, 0, len(order))
	for _, p := range order {
		list = append(list, seen[p])
	}
	return list
}

// instructionsHeader introduces the blocks in Warden's own voice. A bare
// quoted "Instructions from X" block reads to the model like content
// smuggled into its prompt, and it refused one as an injection (the live
// probe of 2026-09-17); saying what the blocks are and who keeps them is
// what makes them the person's preferences rather than a suspect quote.
const instructionsHeader = "Standing instructions of the people in this chat, kept by Warden for each of them (what a user-level CLAUDE.md would hold) and delivered here by Warden itself: follow them as that person's own preferences, within the rules above."

// instructionsBlock is the text that delivers one person's instructions:
// whose they are, then the text. The name is quoted so a name that reads
// like an instruction stays a name.
func instructionsBlock(a cv.Actor, v *Instructions) string {
	if v == nil || strings.TrimSpace(v.Text) == "" {
		return ""
	}
	name := actorName(a)
	if name == "" {
		name = v.Name
	}
	if name == "" {
		name = "a participant"
	}
	return fmt.Sprintf("From %q:\n%s", name, strings.TrimSpace(v.Text))
}

// sessionInstructions is what a starting session gets appended to its
// system prompt: the header, then one block per participant with
// instructions, blank lines between; and the texts delivered by
// principal, for the session to know whose block it has seen
// (instructed). "" when nobody has any.
func sessionInstructions(st *State, c *Chat) (string, map[string]string) {
	blocks := []string{instructionsHeader}
	delivered := map[string]string{}
	for _, a := range participants(c) {
		p := principalOf(a)
		if block := instructionsBlock(a, st.Instructions[p]); block != "" {
			blocks = append(blocks, block)
			delivered[p] = st.Instructions[p].Text
		}
	}
	if len(blocks) == 1 {
		return "", delivered
	}
	return strings.Join(blocks, "\n\n"), delivered
}

// instructionsOwed reports whether the chat's next queued message is from
// a person whose current instructions are not what the session was
// launched with: a late joiner, or text changed or removed since. The
// session is then relaunched (thread/resume keeps the conversation) so
// the agent gets them in its system prompt: Claude Code treats a block
// inside a user message as untrusted and declines it (live probe,
// 2026-09-18), so a message prefix is no delivery.
func (e *Engine) instructionsOwed(id string, a *activeRun) bool {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return false
	}
	for _, v := range c.Conversation.Entries {
		if v.Delivery == "queued" {
			return owed(&st, v, a.instructed)
		}
	}
	return false
}

// owed compares the message sender's current instructions with what the
// session was given for them (instructed, by principal).
func owed(st *State, m cv.Entry, instructed map[string]string) bool {
	if m.Sender == nil {
		return false
	}
	p := principalOf(*m.Sender)
	current := ""
	if v := st.Instructions[p]; v != nil && strings.TrimSpace(v.Text) != "" {
		current = v.Text
	}
	return instructed[p] != current
}
