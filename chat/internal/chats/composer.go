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

// The composer's "!" and "#" prefixes: a person running a shell command in
// the chat's workspace, and a person adding a note to its CLAUDE.md. Both
// happen beside the agent, not through it — the transcript records them
// attributed to the person, and nothing is sent to the model unless the
// person quotes it into a message.

// ExecResult is what a person's command came to, as the route answers it.
type ExecResult struct {
	// ID is the transcript entry (a command card by the person).
	ID       string `json:"id"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Output   string `json:"output"`
}

// Exec runs command in chat id's workspace as actor, recording it as a
// command card attributed to them: running while it runs, then with the
// output and the exit code. It runs while a turn runs too (the runner does
// not take the run's lock or slot). The entry is transcript-only: the
// agent is not told, and the person can quote the card into a message
// when the agent should see it. The person's shell command is logged.
func (e *Engine) Exec(ctx context.Context, id, command string, actor cv.Actor) (ExecResult, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	command = strings.TrimSpace(command)
	if command == "" || len(command) > sandbox.MaxExecCommand || strings.ContainsRune(command, 0) {
		return ExecResult{}, errors.New("a command of at most 4096 characters is required")
	}
	var req sandbox.Request
	entry := cv.NewEntry("activity", command)
	sender := actor
	entry.Sender = &sender
	entry.Tool = &cv.Tool{Kind: "command", Name: "shell", Status: "running"}
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
		c.Conversation.Entries = append(c.Conversation.Entries, entry)
		req = request(c, "exec")
		return nil
	})
	if err != nil {
		return ExecResult{}, err
	}
	req.PrincipalID = actor.PrincipalID
	req.Command = command
	res, callErr := e.Worker.Call(ctx, req)
	result := ExecResult{ID: entry.ID}
	status, detail := "completed", ""
	switch {
	case callErr != nil:
		status, detail = "failed", callErr.Error()
		result.ExitCode = -1
	case res.Exec == nil:
		status, detail = "failed", "the runner gave no result"
		result.ExitCode = -1
	default:
		result.ExitCode, result.TimedOut, result.Output = res.Exec.ExitCode, res.Exec.TimedOut, res.Exec.Output
		detail = res.Exec.Output
		if res.Exec.TimedOut {
			status = "timed out"
		} else if res.Exec.ExitCode != 0 {
			status = fmt.Sprintf("exit %d", res.Exec.ExitCode)
		}
	}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.ID == entry.ID {
				v.Detail = detail
				v.IsStreaming = false
				if v.Tool != nil {
					v.Tool.Status = status
				}
				v.EndedAt = e.at()
			}
		}
		return nil
	})
	log.Printf("chat %s: %s ran %q in the workspace: %s", id, actorLabel(actor), command, status)
	if callErr != nil {
		return result, callErr
	}
	return result, nil
}

// memoryHint says whether the agent will read the note: Claude Code reads
// the workspace's CLAUDE.md only when its project settings are loaded,
// which Warden's launch flags do not do yet (parity item 7); Codex reads
// AGENTS.md instead.
func memoryHint(provider string) string {
	if provider == "codex" {
		return "Codex reads AGENTS.md, not CLAUDE.md; the note is kept for Claude."
	}
	return "The agent reads it only once the workspace's settings are loaded, which the current launch does not do yet."
}

// AppendMemory appends note to CLAUDE.md at chat id's workspace root as a
// bullet, by actor, and records a system line saying so with the hint
// about when the agent reads it.
func (e *Engine) AppendMemory(ctx context.Context, id, note string, actor cv.Actor) error {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	note = strings.TrimSpace(note)
	if note == "" || len(note) > sandbox.MaxMemoryNote || strings.ContainsRune(note, 0) {
		return errors.New("a note of at most 4096 characters is required")
	}
	st := e.Store.Snapshot()
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
	req := request(c, "memory-append")
	req.PrincipalID = actor.PrincipalID
	req.Bytes = []byte(sandbox.MemoryBullet(note))
	if _, err := e.Worker.Call(ctx, req); err != nil {
		return err
	}
	first := []rune(strings.SplitN(note, "\n", 2)[0])
	if len(first) > 80 {
		first = append(first[:77], '…')
	}
	text := fmt.Sprintf("Added to CLAUDE.md: “%s”. %s", string(first), memoryHint(c.Provider))
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		v := cv.NewEntry("system", text)
		sender := actor
		v.Sender = &sender
		c.Conversation.Entries = append(c.Conversation.Entries, v)
		return nil
	})
	log.Printf("chat %s: %s added a note to CLAUDE.md", id, actorLabel(actor))
	return err
}

// actorLabel names a person for the log: their principal, never their
// email (owner identifiers stay out of logs that may be shipped).
func actorLabel(a cv.Actor) string {
	if a.PrincipalID == "owner" || a.PrincipalID == "" {
		return "the owner"
	}
	return "principal " + a.PrincipalID
}
