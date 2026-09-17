package chats

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// fakeSharing answers the policy socket: it records every operation and
// returns canned results per action.
type fakeSharing struct {
	mu      sync.Mutex
	ops     []map[string]any
	results map[string]map[string]any
	errors  map[string]string // action -> refusal, as the policy words it
}

func newFakeSharing(t *testing.T) (*fakeSharing, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSharing{results: map[string]map[string]any{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var m map[string]any
				_ = json.NewDecoder(conn).Decode(&m)
				f.mu.Lock()
				f.ops = append(f.ops, m)
				result := f.results[agent.String(m["action"])]
				refusal := f.errors[agent.String(m["action"])]
				f.mu.Unlock()
				if refusal != "" {
					_ = json.NewEncoder(conn).Encode(map[string]any{"ok": false, "error": refusal})
					return
				}
				if result == nil {
					result = map[string]any{}
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "result": result})
			}()
		}
	}()
	return f, socket
}

// refuse makes the policy answer one action with a refusal from now on.
func (f *fakeSharing) refuse(action, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errors == nil {
		f.errors = map[string]string{}
	}
	f.errors[action] = message
}

func (f *fakeSharing) op(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[i]
}

// Grant tools park a pending approval with the exact request shown to the
// owner; the answer performs the effect with the answering person recorded
// and becomes the agent's tool result. Host directories need local mode.
func TestGrantRequestsBecomeApprovalsAndResolve(t *testing.T) {
	e, w, c := portEngine(t, "")
	sharing, socket := newFakeSharing(t)
	e.WardenSocket = socket
	call := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		if err := e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": tool, "arguments": args}}); err != nil {
			t.Fatal(err)
		}
		approvals := e.Store.Snapshot().chat(c.ID).Approvals
		return map[string]any{"method": approvals[len(approvals)-1].Method, "params": approvals[len(approvals)-1].Params, "n": len(approvals)}
	}
	// Network: the approval carries host, reason and duration; approval
	// applies it through the policy socket for this sandbox.
	got := call("request_network_access", map[string]any{"host": "PyPI.org", "reason": "pip install"})
	if got["method"] != methodNetworkAllow || got["params"].(map[string]any)["host"] != "pypi.org" || got["params"].(map[string]any)["duration_minutes"] != float64(60) {
		t.Fatalf("network approval: %v", got)
	}
	a := e.Store.Snapshot().chat(c.ID).Approvals[0]
	if v := e.resolveGrant(c, a, false, cv.Actor{PrincipalID: "owner"}).(map[string]any); v["success"] != false {
		t.Fatal("decline succeeded")
	}
	sharing.results["network_allow"] = map[string]any{"host": "pypi.org", "expires_at": 1e9}
	v := e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "sub", Name: "Ada"}).(map[string]any)
	if v["success"] != true {
		t.Fatalf("allow: %v", v)
	}
	op := sharing.op(0)
	data := agent.Map(op["data"])
	if op["action"] != "network_allow" || data["sandboxID"] != c.SandboxID || data["host"] != "pypi.org" || data["duration"] != float64(3600) || data["actor"] != "Ada" {
		t.Fatalf("network op: %v", op)
	}
	// Repository: widening merges with the current selection (the request
	// lists it once before asking the owner, the answer once more).
	sharing.results["github_list"] = map[string]any{"repositories": []any{map[string]any{"full_name": "Owner/Repo", "access": []any{"contents"}}, map[string]any{"full_name": "owner/other", "access": []any{"issues"}}}}
	sharing.results["github_select"] = map[string]any{"repositories": []any{}}
	call("request_repository_access", map[string]any{"repository": "owner/repo", "categories": []any{"issues"}, "reason": "read issues"})
	a = e.Store.Snapshot().chat(c.ID).Approvals[1]
	v = e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "owner"}).(map[string]any)
	if v["success"] != true || !strings.Contains(agent.String(agent.Map(agent.Array(v["contentItems"])[0])["text"]), `"widened_from":["contents"]`) {
		t.Fatalf("repository allow: %v", v)
	}
	sel := agent.Map(sharing.op(3)["data"])
	if sharing.op(3)["action"] != "github_select" || len(agent.Array(sel["repositories"])) != 2 || len(agent.Array(agent.Map(sel["access"])["owner/repo"])) != 2 || len(agent.Array(agent.Map(sel["access"])["owner/other"])) != 1 {
		t.Fatalf("select op: %v", sharing.op(3))
	}
	// GitHub write: the exact payload is what the owner sees and what is sent.
	call("github_write", map[string]any{"repository": "owner/repo", "action": "comment_issue", "number": 4, "body": "thanks"})
	a = e.Store.Snapshot().chat(c.ID).Approvals[2]
	if a.Params["number"] != float64(4) || a.Params["body"] != "thanks" {
		t.Fatalf("write params: %v", a.Params)
	}
	e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "owner"})
	if wr := sharing.op(4); wr["action"] != "github_write" || agent.Map(wr["data"])["body"] != "thanks" || agent.Map(wr["data"])["sandboxID"] != c.SandboxID {
		t.Fatalf("write op: %v", wr)
	}
	// Bad input never reaches the owner.
	before := len(e.Store.Snapshot().chat(c.ID).Approvals)
	for tool, args := range map[string]map[string]any{"request_network_access": {"host": "https://x.org/", "reason": "r"}, "request_repository_access": {"repository": "norepo", "categories": []any{"issues"}, "reason": "r"}, "github_write": {"repository": "o/r", "action": "delete"}} {
		_ = e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": tool, "arguments": args}})
	}
	if len(e.Store.Snapshot().chat(c.ID).Approvals) != before {
		t.Fatal("invalid request became an approval")
	}
	// Host directories: refused unless local mode; then a worker operation.
	_ = e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_host_directory", "arguments": map[string]any{"path": "/tmp/x", "reason": "r"}}})
	if len(e.Store.Snapshot().chat(c.ID).Approvals) != before {
		t.Fatal("host directory approved outside local mode")
	}
	e.LocalMode = true
	call("request_host_directory", map[string]any{"path": "/tmp/x", "reason": "r"})
	a = e.Store.Snapshot().chat(c.ID).Approvals[before]
	if v := e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "owner"}).(map[string]any); v["success"] != true {
		t.Fatalf("host import: %v", v)
	}
	if last := w.calls[len(w.calls)-1]; last != "host.import" {
		t.Fatalf("worker op: %s", last)
	}
	_ = context.Background
}

// A repository request the workspace already satisfies is answered at once
// without an approval; when GitHub cannot be used the agent is told why and
// what the owner can do, both before and after the owner's approval.
func TestRepositoryAccessAlreadySharedAndFailures(t *testing.T) {
	e, _, c := portEngine(t, "")
	sharing, socket := newFakeSharing(t)
	e.WardenSocket = socket
	e.LocalMode = true
	sharing.results["github_list"] = map[string]any{"repositories": []any{map[string]any{"full_name": "Owner/Repo", "access": []any{"contents", "issues"}}}}
	text := func(err error) string {
		t.Helper()
		if err == nil {
			t.Fatal("expected a tool result error")
		}
		return err.Error()
	}
	before := len(e.Store.Snapshot().chat(c.ID).Approvals)
	// Already covered (a subset of the held categories): no approval.
	err := e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_repository_access", "arguments": map[string]any{"repository": "owner/repo", "categories": []any{"issues"}, "reason": "r"}}})
	if err != nil || len(e.Store.Snapshot().chat(c.ID).Approvals) != before {
		t.Fatalf("already shared request: err=%v approvals=%d", err, len(e.Store.Snapshot().chat(c.ID).Approvals))
	}
	// Not covered: becomes an approval.
	if err = e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_repository_access", "arguments": map[string]any{"repository": "owner/repo", "categories": []any{"pull_requests"}, "reason": "r"}}}); err != nil || len(e.Store.Snapshot().chat(c.ID).Approvals) != before+1 {
		t.Fatalf("widening request: err=%v", err)
	}
	a := e.Store.Snapshot().chat(c.ID).Approvals[before]
	// The policy's refusal after approval reaches the agent with a remedy.
	sharing.refuse("github_select", "Refresh the GitHub sign-in before using repositories")
	v := e.resolveGrant(c, a, true, cv.Actor{PrincipalID: "owner"}).(map[string]any)
	got := agent.String(agent.Map(agent.Array(v["contentItems"])[0])["text"])
	if v["success"] != false || !strings.Contains(got, "widen owner/repo from contents, issues to contents, issues, pull_requests") || !strings.Contains(got, "Refresh the GitHub sign-in") || !strings.Contains(got, "warden login github") {
		t.Fatalf("resolve error: %v", got)
	}
	// A listing failure before approval is reported at once, not parked.
	sharing.refuse("github_list", "GitHub is not connected")
	msg := text(e.requestGrant(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"tool": "request_repository_access", "arguments": map[string]any{"repository": "owner/new", "categories": []any{"contents"}, "reason": "r"}}}))
	if !strings.Contains(msg, "GitHub is not connected") || !strings.Contains(msg, "warden login github") || len(e.Store.Snapshot().chat(c.ID).Approvals) != before+1 {
		t.Fatalf("listing failure: %q", msg)
	}
}
