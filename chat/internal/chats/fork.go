package chats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
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
//
// A fork with a copy of the workspace (docs/claude-parity.md, R2.13) gets
// a workspace of its own, created by the runner as a copy of the source
// sandbox's disk (op `clone`, sandbox/clone.go: files, the repository
// checkout and the checkpoint refs come along, at the same size), taken
// before the fork exists so nothing the source does afterwards reaches
// the copy. Access grants (documents, repositories, network) stay with
// the source: the policy service scopes every grant to one sandbox
// (policy/sharing.go: "grants belong to the environment"), each one an
// approval the owner gave for that sandbox, so a copy shares nothing until
// it is shared with — the marker says so. Both chats get a marker: the
// copy's names the source, the source's names the copy (Fork.Into).

// ForkResult is what a fork made.
type ForkResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Session says how the fork's agent session follows the source's:
	// "forked" (the source's session is copied at the fork's first
	// launch), "fresh" (a new session with the copied transcript as
	// context), "none" (the source had no session; the fork starts one).
	Session string `json:"session"`
	// SandboxID is the fork's workspace: the source's, or with
	// Workspace "copied" a new one cloned from it.
	SandboxID string `json:"sandboxID"`
	Workspace string `json:"workspace"`
}

// Fork creates a chat forked from chat id: turnID, when given, is the
// user message (its own ID or its turn's) the copy stops before; the
// whole transcript otherwise. The source must be idle. With
// copyWorkspace the fork gets a copy of the workspace (see above), which
// needs every chat on it idle; the source's idle sessions are released
// for the copy and resume at their next message.
func (e *Engine) Fork(ctx context.Context, id, turnID string, copyWorkspace bool, actor cv.Actor) (ForkResult, error) {
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
	if sibling := busy(st.environmentChats(c.SandboxID)); copyWorkspace && sibling != nil {
		return ForkResult{}, fmt.Errorf("wait for chat “%s” on this workspace to finish, or stop it, before copying the workspace", sibling.Title)
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
	result := ForkResult{ID: fork.ID, Title: fork.Title, Session: "none", SandboxID: c.SandboxID, Workspace: "shared"}
	if copyWorkspace {
		fork.SandboxID = cv.ID()
		fork.Origin = &WorkspaceOrigin{SandboxID: c.SandboxID, Name: st.environmentChats(c.SandboxID)[0].Title, ChatID: c.ID, At: e.at()}
		result.SandboxID, result.Workspace = fork.SandboxID, "copied"
	}
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
	marker := cv.NewEntry("fork", forkLabel(c.Title, target, copyWorkspace))
	marker.CreatedAt = e.at()
	sender := actor
	marker.Sender = &sender
	marker.Fork = &cv.Fork{ChatID: c.ID, Title: c.Title, Workspace: copyWorkspace}
	if target != nil {
		marker.Fork.MessageID = target.ID
	}
	marker.Detail = forkDetail(result.Session, copyWorkspace)
	fork.Conversation.Entries = append(fork.Conversation.Entries, marker)
	if copyWorkspace {
		if err := e.copyWorkspace(ctx, c, fork); err != nil {
			return ForkResult{}, err
		}
	}
	if err := e.copyAttachments(c.ID, fork.ID, kept); err != nil {
		e.dropCopy(fork)
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
		if copyWorkspace {
			// The source's own marker: which chat took the copy.
			into := cv.NewEntry("fork", fmt.Sprintf("Forked into “%s” with a copy of the workspace", fork.Title))
			into.CreatedAt = marker.CreatedAt
			into.Sender = &sender
			into.Fork = &cv.Fork{ChatID: fork.ID, Title: fork.Title, Workspace: true, Into: true}
			source.Conversation.Entries = append(source.Conversation.Entries, into)
		}
		return nil
	})
	if err != nil {
		os.RemoveAll(e.attachmentDir(fork.ID))
		e.dropCopy(fork)
		return ForkResult{}, err
	}
	return result, nil
}

// copyWorkspace asks the runner for the fork's workspace as a copy of the
// source's: the workspace's idle sessions are released first (the SBX
// snapshot needs the sandbox stopped, and the runner refuses a sandbox
// with a run), then the clone, retried for a moment while the runner is
// still letting a released run go.
func (e *Engine) copyWorkspace(ctx context.Context, source, fork *Chat) error {
	e.releaseSandbox(ctx, source.SandboxID, "")
	r := request(fork, "clone")
	r.Source = source.SandboxID
	for start := time.Now(); ; {
		_, err := e.Worker.Call(ctx, r)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sandbox.ErrBusy) || time.Since(start) > 15*time.Second {
			return fmt.Errorf("copying the workspace: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// dropCopy removes the sandbox a fork's workspace copy made when the fork
// itself could not be recorded; a fork on the source's workspace has
// nothing to drop.
func (e *Engine) dropCopy(fork *Chat) {
	if fork.Origin == nil {
		return
	}
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	_, _ = e.Worker.Call(ctx, request(fork, "remove"))
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

// forkLabel is the marker's text: which chat, which message the copy
// stops before, and whether the workspace was copied too.
func forkLabel(title string, target *cv.Entry, workspace bool) string {
	label := fmt.Sprintf("Forked from “%s”", strings.TrimSpace(title))
	if target != nil {
		label += fmt.Sprintf(" at “%s”", excerpt(target.Text))
	}
	if workspace {
		label += " with a copy of the workspace"
	}
	return label
}

// forkDetail is the marker's second line: how the agent's session
// follows, and what a workspace copy carried.
func forkDetail(session string, workspace bool) string {
	var parts []string
	switch session {
	case "forked":
		parts = append(parts, "the agent continues from a copy of its session; the original chat keeps its own")
	case "fresh":
		parts = append(parts, "the agent's session cannot be copied; the next message starts a new one with the conversation so far as context")
	}
	if workspace {
		parts = append(parts, forkCopyNote)
	}
	return strings.Join(parts, ". ")
}

// forkCopyNote says what a workspace copy took and what it did not.
const forkCopyNote = "the workspace is a copy of the original's disk as it was when the fork was made (files, the repository checkout, checkpoints); access grants — shared documents, repositories and network — stay with the original workspace, so share them again with this one from its panel"

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
