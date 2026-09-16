package chats

import (
	"context"
	"errors"
	"sort"
	"strings"
	"warden/chat/internal/agent"
	"warden/chat/internal/sandbox"
)

// An environment is the sandbox behind one or more chats. Documents, repositories
// and published ports belong to it, not to the chat that asked for them: every
// chat on the environment shares its disk and its network lease.
type Environment struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Repository   string               `json:"repository"`
	Chats        []EnvironmentChat    `json:"chats"`
	Runtime      *sandbox.SandboxInfo `json:"runtime"`
	Documents    []map[string]any     `json:"documents"`
	Repositories []any                `json:"repositories"`
	Ports        []PortBinding        `json:"ports"`
	Deleted      bool                 `json:"deleted"`
	Archived     bool                 `json:"archived"`
}
type EnvironmentChat struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Archived bool   `json:"archived"`
}

func (s State) deleted(sandboxID string) bool {
	for _, id := range s.DeletedSandboxes {
		if id == sandboxID {
			return true
		}
	}
	return false
}

// environmentChats returns the chats on a sandbox in creation order.
func (s State) environmentChats(sandboxID string) []*Chat {
	var out []*Chat
	for _, c := range s.Chats {
		if c.SandboxID == sandboxID {
			out = append(out, c)
		}
	}
	return out
}

// ranChat picks a chat the worker knows about, for sandbox-level operations.
func ranChat(chats []*Chat) *Chat {
	for _, c := range chats {
		if c.Conversation.ThreadID != nil || len(c.Conversation.Entries) > 0 || c.RunID != "" {
			return c
		}
	}
	return nil
}
func busy(chats []*Chat) *Chat {
	for _, c := range chats {
		if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
			return c
		}
	}
	return nil
}

func (e *Engine) Environments(ctx context.Context) ([]Environment, error) {
	st := e.Store.Snapshot()
	var grants []any
	if e.WardenSocket != "" {
		if result, err := e.sharingCall(ctx, "state", map[string]any{}); err == nil {
			grants = agent.Array(result["requests"])
		}
	}
	seen := map[string]bool{}
	var out []Environment
	for _, c := range st.Chats {
		if seen[c.SandboxID] {
			continue
		}
		seen[c.SandboxID] = true
		chats := st.environmentChats(c.SandboxID)
		env := Environment{ID: c.SandboxID, Name: chats[0].Title, Repository: chats[0].Repository, Documents: []map[string]any{}, Repositories: []any{}, Ports: []PortBinding{}, Deleted: st.deleted(c.SandboxID)}
		env.Archived = true
		for _, chat := range chats {
			env.Chats = append(env.Chats, EnvironmentChat{ID: chat.ID, Title: chat.Title, Status: chat.Status, Archived: chat.Archived})
			if !chat.Archived {
				env.Archived = false
			}
		}
		for _, p := range st.Ports {
			if p.SandboxID == c.SandboxID && p.State == "approved" {
				env.Ports = append(env.Ports, p)
			}
		}
		for _, value := range grants {
			r := agent.Map(value)
			if agent.String(r["sandboxID"]) == c.SandboxID && agent.String(r["status"]) == "granted" {
				env.Documents = append(env.Documents, r)
			}
		}
		if ran := ranChat(chats); ran != nil && !env.Deleted {
			if res, err := e.Runtime(ctx, ran.ID, "status"); err == nil && res.Sandbox != nil {
				env.Runtime = res.Sandbox
			}
			if e.WardenSocket != "" {
				if result, err := e.sharingCall(ctx, "github_list", map[string]any{"chatID": ran.ID, "sandboxID": c.SandboxID}); err == nil {
					env.Repositories = agent.Array(result["repositories"])
				}
			}
		}
		out = append(out, env)
	}
	retired := func(env Environment) bool { return env.Deleted || env.Archived }
	sort.SliceStable(out, func(i, j int) bool { return retired(out[i]) != retired(out[j]) && !retired(out[i]) })
	if out == nil {
		out = []Environment{}
	}
	return out, nil
}

// StopEnvironment stops the sandbox behind an idle environment. A chat that is
// running keeps its own Stop path; stopping under it would interrupt its run.
func (e *Engine) StopEnvironment(ctx context.Context, id string) error {
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 {
		return errors.New("workspace not found")
	}
	if c := busy(chats); c != nil {
		return errors.New("workspace is running chat “" + c.Title + "”; stop that chat first")
	}
	ran := ranChat(chats)
	if ran == nil {
		return nil
	}
	e.releaseSandbox(ctx, id, "")
	_, err := e.Worker.Call(ctx, request(ran, "stop"))
	return err
}

// ArchiveEnvironment stops the sandbox and archives every chat on it. Files
// and grants stay; restoring any of its chats brings the workspace back.
func (e *Engine) ArchiveEnvironment(ctx context.Context, id string) error {
	if err := e.StopEnvironment(ctx, id); err != nil {
		return err
	}
	return e.Store.update(func(st *State) error {
		if c := busy(st.environmentChats(id)); c != nil {
			return errors.New("workspace started running while archiving")
		}
		for _, c := range st.Chats {
			if c.SandboxID == id {
				c.Archived = true
			}
		}
		return nil
	})
}

// DeleteEnvironment revokes everything the environment can reach, removes its
// sandbox and archives its chats. Revocations are durable before the worker is
// asked to delete anything, so a failed removal never leaves access behind.
func (e *Engine) DeleteEnvironment(ctx context.Context, id string) error {
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 {
		return errors.New("workspace not found")
	}
	if c := busy(chats); c != nil {
		return errors.New("workspace is running chat “" + c.Title + "”; stop that chat first")
	}
	ran := ranChat(chats)
	if e.WardenSocket != "" {
		result, err := e.sharingCall(ctx, "state", map[string]any{})
		if err != nil {
			return errors.New("sharing service unavailable; workspace not deleted")
		}
		for _, value := range agent.Array(result["requests"]) {
			r := agent.Map(value)
			if agent.String(r["sandboxID"]) == id && agent.String(r["status"]) == "granted" {
				if _, err = e.sharingCall(ctx, "revoke", map[string]any{"id": r["request_id"]}); err != nil {
					return errors.New("document grant could not be revoked; workspace not deleted")
				}
			}
		}
		if ran != nil {
			// A missing GitHub connection means there is nothing to revoke.
			if _, err = e.sharingCall(ctx, "github_select", map[string]any{"chatID": ran.ID, "sandboxID": id, "repositories": []string{}}); err != nil && !strings.Contains(err.Error(), "Sharing unavailable") {
				return errors.New("repository access could not be revoked; workspace not deleted")
			}
		}
	}
	for _, p := range st.Ports {
		if p.SandboxID == id && p.State == "approved" {
			// Public access is revoked durably inside RevokePort; the worker's
			// mapping goes away with the sandbox regardless of this result.
			_ = e.RevokePort(ctx, p.ID)
		}
	}
	if ran != nil {
		e.releaseSandbox(ctx, id, "")
		if _, err := e.Worker.Call(ctx, request(ran, "remove")); err != nil && !strings.Contains(err.Error(), "not authorized for this project sandbox") {
			return err
		}
	}
	return e.Store.update(func(st *State) error {
		if c := busy(st.environmentChats(id)); c != nil {
			return errors.New("workspace started running during deletion")
		}
		for _, c := range st.Chats {
			if c.SandboxID == id {
				c.Archived = true
			}
		}
		if !st.deleted(id) {
			st.DeletedSandboxes = append(st.DeletedSandboxes, id)
		}
		return nil
	})
}
