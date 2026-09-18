package chats

import (
	"context"
	"log"
	"strings"
	"time"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// Automatic titles (docs/claude-parity.md, R2.1): a chat created with
// the default title is named after its first turn from the first message
// and the first reply, the way Claude Code names its sessions — by a
// one-shot, tool-less CLI on the cheapest model beside the chat's
// resident session (the runner op "oneshot", sandbox/aside.go; the
// gateway serves the credential only while the run is live, so it is
// asked right after the turn). When the one-shot cannot run (Codex, a run
// that ended with the turn, a failure) the first message's first line is
// the title. A person's title, given at creation or by a rename, always
// wins and ends the naming; Chat.Titled records which it was. The
// transcript gets no entry.

// DefaultTitle is the title a chat gets when its creator gives none.
const DefaultTitle = "New chat"

// TitleModel is the model the title one-shot runs on: the CLI's alias
// for its cheapest.
const TitleModel = "haiku"

// TitleSystemPrompt is the one-shot's whole system prompt, in place of
// the CLI's own (which is thousands of tokens the naming does not need).
const TitleSystemPrompt = "You name chat conversations. Given the first user message and the first reply of a conversation, answer with a title of 3 to 6 words that says what the conversation is about: plain words, no quotes, no trailing period, no preamble, nothing else."

// titleTextLimit bounds each side of the exchange the prompt carries, in
// runes; titleLimit bounds a title, in runes (the store admits 160).
const (
	titleTextLimit = 2000
	titleLimit     = 60
)

// untitled reports whether the chat still waits to be named: the default
// title, and no naming yet.
func (c *Chat) untitled() bool { return c.Titled == "" && c.Title == DefaultTitle }

// autoTitle names chat id when it is still untitled: from the one-shot
// when a is a resident Claude run that is live, from the first message
// otherwise or when the one-shot fails. Safe to call after every turn;
// one naming runs at a time per chat and a title that arrived meanwhile
// (a rename) is kept.
func (e *Engine) autoTitle(ctx context.Context, id string, a *activeRun) {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil || !c.untitled() {
		return
	}
	user, reply := firstExchange(c.Conversation.Entries)
	if user == "" {
		return
	}
	e.mu.Lock()
	if e.titling == nil {
		e.titling = map[string]bool{}
	}
	busy := e.titling[id]
	e.titling[id] = true
	live := a != nil && a.client != nil && c.Provider == "claude"
	e.mu.Unlock()
	if busy {
		return
	}
	defer func() {
		e.mu.Lock()
		delete(e.titling, id)
		e.mu.Unlock()
	}()
	title := ""
	if live {
		req := request(c, "oneshot")
		req.Model = TitleModel
		req.Command = titlePrompt(user, reply)
		req.Instructions = TitleSystemPrompt
		ctx, cancel := context.WithTimeout(ctx, sandbox.OneShotTimeout+20*time.Second)
		res, err := e.Worker.Call(ctx, req)
		cancel()
		switch {
		case err != nil:
			log.Printf("chat %s: title one-shot failed: %v", id, err)
		case res.Aside == nil || res.Aside.Error != "":
			reason := "no answer"
			if res.Aside != nil {
				reason = res.Aside.Error
			}
			log.Printf("chat %s: title one-shot failed: %s", id, reason)
		default:
			title = CleanTitle(res.Aside.Text)
		}
	}
	how := "generated"
	if title == "" {
		title = FallbackTitle(user)
		how = "from the first message"
	}
	if title == "" {
		return
	}
	renamed := false
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || !c.untitled() {
			return nil
		}
		c.Title, c.Titled = title, "auto"
		renamed = true
		return nil
	})
	if renamed {
		log.Printf("chat %s: titled %q (%s)", id, title, how)
	}
}

// firstExchange is the text of the chat's first user message and of the
// first agent reply after it ("" when there is none yet), each cut to the
// prompt's limit.
func firstExchange(entries []cv.Entry) (user, reply string) {
	seen := false
	for _, v := range entries {
		switch {
		case !seen && v.Role == "user":
			user = cutRunes(strings.TrimSpace(v.Text), titleTextLimit)
			seen = true
		case seen && v.Role == "assistant" && strings.TrimSpace(v.Text) != "":
			return user, cutRunes(strings.TrimSpace(v.Text), titleTextLimit)
		}
	}
	return user, ""
}

// titlePrompt is what the one-shot reads: the exchange, labelled.
func titlePrompt(user, reply string) string {
	var b strings.Builder
	b.WriteString("User: ")
	b.WriteString(user)
	if reply != "" {
		b.WriteString("\n\nAssistant: ")
		b.WriteString(reply)
	}
	return b.String()
}

// CleanTitle makes a title out of what the model answered: the first
// non-empty line, quotes and a trailing period stripped, whitespace
// collapsed, cut to the limit. "" when nothing usable is left.
func CleanTitle(answer string) string {
	line := ""
	for _, l := range strings.Split(answer, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = strings.Trim(line, "\"'“”‘’`*#")
	line = strings.TrimSpace(strings.TrimRight(line, ".。!"))
	line = strings.Join(strings.Fields(line), " ")
	if strings.HasPrefix(strings.ToLower(line), "title:") {
		line = strings.TrimSpace(line[len("title:"):])
	}
	line = strings.Trim(line, "\"'“”‘’`")
	return strings.TrimSpace(cutRunes(line, titleLimit))
}

// FallbackTitle is the title made from the first message alone: its
// first non-empty line (a leading slash command or mention kept as
// written), whitespace collapsed, cut to the limit with an ellipsis.
func FallbackTitle(text string) string {
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			line = l
			break
		}
	}
	line = strings.TrimSpace(strings.TrimLeft(line, "#>-* "))
	if line == "" {
		return ""
	}
	if r := []rune(line); len(r) > titleLimit {
		cut := string(r[:titleLimit-1])
		if i := strings.LastIndex(cut, " "); i > titleLimit/2 {
			cut = cut[:i]
		}
		return strings.TrimRight(cut, " ,;:") + "…"
	}
	return line
}

// cutRunes cuts s to at most n runes.
func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
