package chats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"warden/chat/internal/agent"
	"warden/chat/internal/sandbox"
)

// An environment is the sandbox behind one or more chats. Documents, repositories
// and published ports belong to it, not to the chat that asked for them: every
// chat on the environment shares its disk and its network lease.
type Environment struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	Repository string                `json:"repository"`
	Chats      []EnvironmentChat     `json:"chats"`
	Runtime    *sandbox.SandboxInfo  `json:"runtime"`
	Usage      *sandbox.SandboxUsage `json:"usage"`
	// Pod is the sandbox pod on the Kubernetes shape (nil elsewhere, and
	// while the sandbox is stopped).
	Pod *sandbox.PodInfo `json:"pod"`
	// Resources is the workspace's size: the runner's record once the
	// sandbox exists, else what its first chat asked for, else nil (the
	// runner's default).
	Resources *sandbox.Resources `json:"resources,omitempty"`
	// Network is the workspace's own network access ("" follows the
	// install; network.go).
	Network string `json:"network,omitempty"`
	// Resizing is a resize in flight or how the last one ended.
	Resizing     *Resizing        `json:"resizing,omitempty"`
	Documents    []map[string]any `json:"documents"`
	Repositories []any            `json:"repositories"`
	Ports        []PortBinding    `json:"ports"`
	// Rules are the workspace's permission rules (rules.go), applied to
	// every chat of it.
	Rules    []Rule `json:"rules"`
	Deleted  bool   `json:"deleted"`
	Archived bool   `json:"archived"`
	// CopiedFrom is set on a workspace created as a copy of another (a
	// fork with copyWorkspace, fork.go): which one and when.
	CopiedFrom *WorkspaceOrigin `json:"copiedFrom,omitempty"`
}
type EnvironmentChat struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Archived bool   `json:"archived"`
	// Stage is the chat's startup stage while it is starting (startup.go).
	Stage string `json:"stage,omitempty"`
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
	if e.PolicyAddress != "" {
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
		env := Environment{ID: c.SandboxID, Name: chats[0].Title, Repository: chats[0].Repository, Resources: chats[0].Resources, Network: chats[0].Network, Resizing: e.resizingOf(c.SandboxID), Documents: []map[string]any{}, Repositories: []any{}, Ports: []PortBinding{}, Rules: []Rule{}, Deleted: st.deleted(c.SandboxID)}
		if rec := st.Environments[c.SandboxID]; rec != nil && len(rec.Rules) > 0 {
			env.Rules = rec.Rules
		}
		env.Archived = true
		for _, chat := range chats {
			if chat.Origin != nil && env.CopiedFrom == nil {
				env.CopiedFrom = chat.Origin
			}
			ec := EnvironmentChat{ID: chat.ID, Title: chat.Title, Status: chat.Status, Archived: chat.Archived}
			if s := e.startupOf(chat.ID); s != nil {
				ec.Stage = s.Stage
			}
			env.Chats = append(env.Chats, ec)
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
				// An expired grant stays listed, marked, so people see that
				// access ended rather than wondering where the document went.
				expires, _ := r["expires_at"].(float64)
				r["expired"] = expires > 0 && expires <= float64(time.Now().UnixNano())/1e9
				env.Documents = append(env.Documents, r)
			}
		}
		if ran := ranChat(chats); ran != nil && !env.Deleted {
			if res, err := e.Runtime(ctx, ran.ID, "status"); err == nil && res.Sandbox != nil {
				env.Runtime = res.Sandbox
				if !res.Sandbox.Resources.IsZero() {
					size := res.Sandbox.Resources
					env.Resources = &size
				}
				// Provisioned and used CPU, memory and disk, as the guest
				// reports them; the panel refreshes this every few seconds.
				if res, err := e.Runtime(ctx, ran.ID, "usage"); err == nil {
					env.Usage = res.Usage
				}
				if res, err := e.Runtime(ctx, ran.ID, "pod"); err == nil {
					env.Pod = res.Pod
				}
			}
		}
		// Shared repositories belong to the workspace, not to a run: a
		// chat that has not sent its first message lists them too.
		if e.PolicyAddress != "" && !env.Deleted {
			if result, err := e.sharingCall(ctx, "github_list", map[string]any{"chatID": chats[0].ID, "sandboxID": c.SandboxID}); err == nil {
				env.Repositories = agent.Array(result["repositories"])
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
	if err := e.stopChats(ctx, id); err != nil {
		return err
	}
	ran := ranChat(chats)
	if ran == nil {
		return nil
	}
	e.releaseSandbox(ctx, id, "")
	return e.stopSandbox(ctx, ran)
}

// StartEnvironment brings a stopped workspace's sandbox back without a
// message, so the next one starts at once. A workspace that never ran has
// nothing to start.
func (e *Engine) StartEnvironment(ctx context.Context, id string) error {
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 {
		return errors.New("workspace not found")
	}
	if st.deleted(id) {
		return errors.New("workspace was deleted")
	}
	ran := ranChat(chats)
	if ran == nil {
		return errors.New("the workspace has no sandbox yet; its first message creates one")
	}
	_, err := e.Worker.Call(ctx, request(ran, "start"))
	return err
}

// stopChats ends every running chat on the workspace (the agent's turn,
// then its session) and waits for them to settle: the owner asked for the
// workspace to stop or change, and a chat that is merely resident between
// turns is not worth a refusal. A chat stopping already is waited for.
// Stop on the chat refuses while a sibling runs, so the chats are stopped
// one at a time. Stop alone leaves the session resident for the next
// message; here the sandbox is about to go, so the sessions end too.
func (e *Engine) stopChats(ctx context.Context, id string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		chats := e.Store.Snapshot().environmentChats(id)
		c := busy(chats)
		if c == nil {
			for _, c := range chats {
				e.endSession(ctx, c.ID)
			}
			return nil
		}
		if c.Status != "stopping" {
			if err := e.Stop(ctx, c.ID); err != nil && !strings.Contains(err.Error(), "stop already pending") && !strings.Contains(err.Error(), "another chat") {
				return fmt.Errorf("stopping chat “%s”: %w", c.Title, err)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("chat “" + c.Title + "” did not stop in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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
	if e.PolicyAddress != "" {
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
			if _, err = e.sharingCall(ctx, "github_select", map[string]any{"chatID": ran.ID, "sandboxID": id, "repositories": []string{}}); err != nil && !githubDisconnected(err) {
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
