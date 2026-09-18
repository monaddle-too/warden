package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// The workspace's memory files seen and edited from the chat (parity item
// 13): the instruction files at the workspace root, the rules under
// .claude/rules, and the CLI's auto-memory directory. The runner lists and
// writes them (sandbox/memory.go); this side says which chat's workspace,
// passes the auto-memory directory the CLI reported for it, and leaves a
// notice in the transcript saying who edited what.

// MemoryView is what GET chats/{id}/memory answers.
type MemoryView struct {
	sandbox.MemoryListing
	// Read says whether the agent's launch reads these files: Claude Code
	// reads none of them under Warden's current launch flags (item 7);
	// Codex reads AGENTS.md.
	Read bool `json:"read"`
	// Hint is the sentence the surfaces show about that.
	Hint string `json:"hint,omitempty"`
}

// Memory lists chat id's memory files with their contents. The sandbox
// must be running (only the guest can read its workspace).
func (e *Engine) Memory(ctx context.Context, id string) (MemoryView, error) {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return MemoryView{}, errors.New("chat not found")
	}
	if st.deleted(c.SandboxID) {
		return MemoryView{}, errors.New("this chat's workspace was deleted")
	}
	req := request(c, "memory-list")
	if c.Session != nil {
		req.Path = c.Session.AutoMemory
	}
	res, err := e.Worker.Call(ctx, req)
	if err != nil {
		return MemoryView{}, err
	}
	if res.Memory == nil {
		return MemoryView{}, errors.New("the runner gave no listing")
	}
	view := MemoryView{MemoryListing: *res.Memory, Read: c.Provider == "codex"}
	view.Hint = memoryReadHint(c.Provider)
	return view, nil
}

// memoryReadHint says what the agent's launch does with these files.
func memoryReadHint(provider string) string {
	if provider == "codex" {
		return "Codex reads AGENTS.md at the workspace root; the other files are kept for Claude."
	}
	return "Claude reads these only once the workspace's settings are loaded, which the current launch does not do yet."
}

// WriteMemory replaces the file at path under scope in chat id's workspace
// with text, by actor, and records a notice naming them and the file.
func (e *Engine) WriteMemory(ctx context.Context, id, scope, path, text string, actor cv.Actor) error {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	if err := sandbox.ValidateMemoryPath(scope, path); err != nil {
		return err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) > sandbox.MaxMemoryFile || strings.ContainsRune(text, 0) {
		return errors.New("a memory file is text of at most 1 MiB")
	}
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return errors.New("chat not found")
	}
	if st.deleted(c.SandboxID) {
		return errors.New("this chat's workspace was deleted")
	}
	if c.Archived {
		return errors.New("chat is archived")
	}
	req := request(c, "memory-write")
	req.PrincipalID = actor.PrincipalID
	req.Scope, req.Directory, req.Bytes = scope, path, []byte(text)
	if c.Session != nil {
		req.Path = c.Session.AutoMemory
	}
	if _, err := e.Worker.Call(ctx, req); err != nil {
		return err
	}
	label := path
	if scope == sandbox.MemoryScopeAuto {
		label = "auto-memory " + path
	}
	notice := fmt.Sprintf("%s edited %s", memoryEditor(actor), label)
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		v := cv.NewEntry("notice", notice)
		sender := actor
		v.Sender = &sender
		c.Conversation.Entries = append(c.Conversation.Entries, v)
		return nil
	})
	log.Printf("chat %s: %s edited %s (%s)", id, actorLabel(actor), path, scope)
	return err
}

// memoryEditor names the person in the notice: their name, else their
// email, else "The owner".
func memoryEditor(a cv.Actor) string {
	name := actorName(a)
	if name == "the owner" {
		return "The owner"
	}
	return name
}
