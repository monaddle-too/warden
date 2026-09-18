package chats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	cv "warden/chat/internal/conversation"
)

// Forking a chat (docs/claude-parity.md, item 15): a sibling chat on the
// same workspace whose transcript is a copy of the source's up to a
// message, and whose agent session, for a Claude chat with one, is a copy
// of the source's session (the CLI's `--resume <session> --fork-session`
// at the fork's first launch: the runner resumes the source's session and
// the agent reports the copy's own id, which the chat then records). A
// fork cut before an earlier message rewinds the copied session to that
// message the way item 11 does, right after it starts. A chat whose agent
// cannot fork its session (Codex; no session yet) starts a fresh one with
// the copied transcript re-sent as a recap.

// ForkResult is what a fork made.
type ForkResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Session says how the fork's agent session follows the source's:
	// "forked" (the source's session is copied at the fork's first
	// launch), "fresh" (a new session with the copied transcript as
	// context), "none" (the source had no session; the fork starts one).
	Session string `json:"session"`
}

// Fork creates a chat forked from chat id: turnID, when given, is the
// user message (its own ID or its turn's) the copy stops before; the
// whole transcript otherwise. The source must be idle.
func (e *Engine) Fork(ctx context.Context, id, turnID string, actor cv.Actor) (ForkResult, error) {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return ForkResult{}, errors.New("chat not found")
	}
	if c.Archived || st.deleted(c.SandboxID) {
		return ForkResult{}, errors.New("this chat is archived or its workspace was deleted")
	}
	if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
		return ForkResult{}, errors.New("wait for the agent to finish, or stop it, before forking")
	}
	cut := len(c.Conversation.Entries)
	var target *cv.Entry
	if turnID != "" {
		for i, v := range c.Conversation.Entries {
			if v.Role == "user" && v.ParentID == "" && (v.ID == turnID || (v.TurnID != nil && *v.TurnID == turnID)) {
				cut, target = i, &c.Conversation.Entries[i]
				break
			}
		}
		if target == nil {
			return ForkResult{}, errors.New("no such message in this chat")
		}
	}
	kept := copyEntries(c.Conversation.Entries[:cut])
	fork := &Chat{ID: cv.ID(), Provider: c.Provider, Model: c.Model, Title: forkTitle(c.Title), SandboxID: c.SandboxID, Repository: c.Repository, Resources: c.Resources, Mode: c.Mode, Rules: append([]Rule(nil), c.Rules...), OutputStyle: c.OutputStyle, Status: "idle", Approvals: []Approval{}, Commands: append([]Command(nil), c.Commands...)}
	fork.Conversation = cv.Conversation{Entries: kept, Turns: keptTurns(c.Conversation.Turns, kept), Context: c.Conversation.Context}
	if c.Session != nil {
		session := *c.Session
		fork.Session = &session
	}
	result := ForkResult{ID: fork.ID, Title: fork.Title, Session: "none"}
	switch {
	case c.Conversation.ThreadID == nil:
		// Nothing to copy: the fork starts a session of its own, with the
		// copied transcript as context when there is one.
		if recapText := recapWith(kept, forkPreamble); recapText != "" {
			fork.Recap = recapText
		}
	case c.Provider == "claude":
		result.Session = "forked"
		fork.Conversation.ThreadID = cv.Ptr(*c.Conversation.ThreadID)
		fork.ForkSession = true
		if target != nil {
			// The copied session holds the whole conversation; the fork
			// forgets from the cut on as soon as its session starts
			// (rewind.go, applyPendingRewind), or starts fresh with the
			// kept transcript when it cannot.
			fork.Rewind = &PendingRewind{TargetID: target.ID, LastSeenID: lastSent(c.Conversation.Entries)}
		}
	default:
		result.Session = "fresh"
		fork.NewSession = true
		fork.Recap = recapWith(kept, forkPreamble)
	}
	marker := cv.NewEntry("fork", forkLabel(c.Title, target))
	marker.CreatedAt = e.at()
	sender := actor
	marker.Sender = &sender
	marker.Fork = &cv.Fork{ChatID: c.ID, Title: c.Title}
	if target != nil {
		marker.Fork.MessageID = target.ID
	}
	marker.Detail = forkDetail(result.Session)
	fork.Conversation.Entries = append(fork.Conversation.Entries, marker)
	if err := e.copyAttachments(c.ID, fork.ID, kept); err != nil {
		return ForkResult{}, err
	}
	err := e.Store.update(func(st *State) error {
		source := st.chat(id)
		if source == nil {
			return errors.New("chat not found")
		}
		if source.Status == "running" || source.Status == "queued" || source.Status == "stopping" {
			return errors.New("wait for the agent to finish, or stop it, before forking")
		}
		if st.deleted(source.SandboxID) {
			return errors.New("workspace was deleted")
		}
		st.Chats = append(st.Chats, fork)
		return nil
	})
	if err != nil {
		os.RemoveAll(e.attachmentDir(fork.ID))
		return ForkResult{}, err
	}
	return result, nil
}

// copyEntries is a copy of entries with their own attachment and tool
// slices, so the fork's transcript never shares memory with the source's.
func copyEntries(entries []cv.Entry) []cv.Entry {
	out := make([]cv.Entry, 0, len(entries))
	for _, v := range entries {
		v.IsStreaming = false
		if v.Delivery == "queued" {
			// A message the source never delivered is not the fork's to
			// send; it stays with the source.
			continue
		}
		v.Attachments = append([]cv.Attachment(nil), v.Attachments...)
		if v.Tool != nil {
			tool := *v.Tool
			v.Tool = &tool
		}
		if v.Sender != nil {
			sender := *v.Sender
			v.Sender = &sender
		}
		out = append(out, v)
	}
	return out
}

// keptTurns are the turn records the kept entries name.
func keptTurns(turns []cv.Turn, kept []cv.Entry) []cv.Turn {
	named := map[string]bool{}
	for _, v := range kept {
		if v.TurnID != nil {
			named[*v.TurnID] = true
		}
	}
	var out []cv.Turn
	for _, t := range turns {
		if named[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

// lastSent is the ID of the last user message the agent saw, which a
// rewind names so a later message cannot make its target stale.
func lastSent(entries []cv.Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if v := entries[i]; v.Role == "user" && v.ParentID == "" && v.Delivery == "sent" {
			return v.ID
		}
	}
	return ""
}

// copyAttachments gives the fork its own copies of the stored files the
// kept messages carry (the transcript route reads them per chat).
func (e *Engine) copyAttachments(from, to string, kept []cv.Entry) error {
	src, dst := e.attachmentDir(from), e.attachmentDir(to)
	for _, v := range kept {
		for _, a := range v.Attachments {
			if !attachmentID.MatchString(a.ID) {
				continue
			}
			if err := os.MkdirAll(dst, 0700); err != nil {
				return err
			}
			for _, name := range []string{a.ID, a.ID + ".json"} {
				data, err := os.ReadFile(filepath.Join(src, name))
				if err != nil {
					continue // the source lost it; the fork shows the record without the file
				}
				if err := os.WriteFile(filepath.Join(dst, name), data, 0600); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// forkTitle names the fork after its source.
func forkTitle(title string) string {
	base := strings.TrimSpace(title)
	if base == "" {
		base = "Chat"
	}
	t := base + " (fork)"
	if r := []rune(t); len(r) > 160 {
		t = string(r[:157]) + "…"
	}
	return t
}

// forkLabel is the marker's text: which chat, and which message the copy
// stops before.
func forkLabel(title string, target *cv.Entry) string {
	label := fmt.Sprintf("Forked from “%s”", strings.TrimSpace(title))
	if target != nil {
		label += fmt.Sprintf(" at “%s”", excerpt(target.Text))
	}
	return label
}

// forkDetail is the marker's second line: how the agent's session follows.
func forkDetail(session string) string {
	switch session {
	case "forked":
		return "the agent continues from a copy of its session; the original chat keeps its own"
	case "fresh":
		return "the agent's session cannot be copied; the next message starts a new one with the conversation so far as context"
	}
	return ""
}

// excerpt is a message's first 60 runes on one line.
func excerpt(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	if s == "" {
		s = "(attachments)"
	}
	return s
}

const forkPreamble = "Context: this session was started for a chat forked from another; the transcript so far is below. Continue from it without repeating it."
