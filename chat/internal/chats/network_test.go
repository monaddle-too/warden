package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// A workspace's network access is chosen by the owner on POST chats and
// changed at environments/{id}/network: the policy service is told for
// the sandbox (egress_set with the sandboxID, the actor named), every chat
// of the workspace records it, the environments view shows it, and a
// refusal at creation leaves no chat behind. Admitted people cannot
// choose it, a shared workspace already has its access, and only
// restricted, open or empty are accepted.
func TestWorkspaceNetworkAccessIsTheOwnersChoice(t *testing.T) {
	sharing, socket := newFakeSharing(t)
	e, _, _ := setup(t, func(e *Engine) { e.PolicyAddress = "unix://" + socket })
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	call := func(method, path, body string, identity map[string]string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, h.Origin+"/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer private")
		for k, v := range identity {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	owner := map[string]string{"X-Warden-Principal": "sub-owner", "X-Warden-Email": "owner@example.com", "X-Warden-Name": "The Owner", "X-Warden-Role": "admin"}
	alice := map[string]string{"X-Warden-Principal": "sub-alice", "X-Warden-Email": "alice@example.com", "X-Warden-Name": "Alice Example"}

	// Created open by the owner: the policy service hears it for the fresh
	// sandbox, the chat records it.
	code, v := call("POST", "chats", `{"title":"Open one","provider":"codex","network":"open"}`, owner)
	if code != 200 {
		t.Fatalf("create open: %d %v", code, v)
	}
	c := e.Store.Snapshot().chat(agent.String(v["id"]))
	if c == nil || c.Network != "open" {
		t.Fatalf("chat: %+v", c)
	}
	sets := sharing.actions("egress_set")
	if len(sets) != 1 {
		t.Fatalf("egress_set calls: %v", sets)
	}
	if d := agent.Map(sets[0]["data"]); d["sandboxID"] != c.SandboxID || d["mode"] != "open" || d["actor"] != "The Owner" {
		t.Fatalf("egress_set: %v", d)
	}
	if envs, _ := e.Environments(context.Background()); len(envs) != 1 || envs[0].Network != "open" {
		t.Fatalf("environments: %+v", envs)
	}
	// A chat sharing the workspace inherits the record.
	code, v = call("POST", "chats", `{"title":"Sibling","provider":"codex","sandboxID":"`+c.SandboxID+`"}`, owner)
	if code != 200 || e.Store.Snapshot().chat(agent.String(v["id"])).Network != "open" {
		t.Fatalf("sibling: %d %v", code, v)
	}
	// Changed from the panel: both chats follow; cleared with "".
	if code, v = call("POST", "environments/"+c.SandboxID+"/network", `{"network":"restricted"}`, owner); code != 200 {
		t.Fatalf("set restricted: %d %v", code, v)
	}
	for _, ch := range e.Store.Snapshot().environmentChats(c.SandboxID) {
		if ch.Network != "restricted" {
			t.Fatalf("chat %s: %q", ch.Title, ch.Network)
		}
	}
	if code, v = call("POST", "environments/"+c.SandboxID+"/network", `{}`, owner); code != 200 || e.Store.Snapshot().chat(c.ID).Network != "" {
		t.Fatalf("clear: %d %v", code, v)
	}
	if sets = sharing.actions("egress_set"); len(sets) != 3 || agent.Map(sets[2]["data"])["mode"] != "" {
		t.Fatalf("egress_set calls: %v", sets)
	}
	// Not the owner's to choose, on either route; a plain creation is fine.
	if code, _ = call("POST", "chats", `{"title":"Alice open","provider":"codex","network":"open"}`, alice); code != 403 {
		t.Fatalf("alice creates open: %d", code)
	}
	if code, _ = call("POST", "environments/"+c.SandboxID+"/network", `{"network":"open"}`, alice); code != 403 {
		t.Fatalf("alice sets: %d", code)
	}
	if code, _ = call("POST", "chats", `{"title":"Alice plain","provider":"codex"}`, alice); code != 200 {
		t.Fatalf("alice creates: %d", code)
	}
	// Without an edge the capability holder is the owner.
	if code, _ = call("POST", "environments/"+c.SandboxID+"/network", `{"network":"open"}`, nil); code != 200 {
		t.Fatalf("local owner sets: %d", code)
	}
	// Invalid values and unknown workspaces are refused; a shared workspace
	// cannot be given access at creation.
	for _, bad := range []string{`{"network":"public"}`, `{"network":"any"}`} {
		if code, _ = call("POST", "environments/"+c.SandboxID+"/network", bad, owner); code != 409 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if code, _ = call("POST", "environments/nope/network", `{"network":"open"}`, owner); code != 409 {
		t.Fatalf("unknown workspace: %d", code)
	}
	before := len(e.Store.Snapshot().Chats)
	if code, _ = call("POST", "chats", `{"title":"Shared open","provider":"codex","sandboxID":"`+c.SandboxID+`","network":"open"}`, owner); code != 409 || len(e.Store.Snapshot().Chats) != before {
		t.Fatalf("shared with access: %d, %d chats", code, len(e.Store.Snapshot().Chats))
	}
	// The policy service's refusal is the creation's error, and the chat
	// it would have been is gone.
	sharing.refuse("egress_set", "egress switch unavailable")
	if code, v = call("POST", "chats", `{"title":"Refused","provider":"codex","network":"open"}`, owner); code != 409 || !strings.Contains(agent.String(v["error"]), "unavailable") || len(e.Store.Snapshot().Chats) != before {
		t.Fatalf("refused creation: %d %v, %d chats", code, v, len(e.Store.Snapshot().Chats))
	}
}

// A fork with a copy of the workspace keeps the original's own network
// access, declared for the copy's sandbox before the clone; a fork on the
// same workspace shares it with nothing to declare; a workspace that
// follows the install declares nothing either.
func TestForkCopyKeepsTheWorkspaceNetworkAccess(t *testing.T) {
	sharing, socket := newFakeSharing(t)
	e, w, id := claudeSetup(t, func(e *Engine) { e.PolicyAddress = "unix://" + socket })
	oneTurn(t, e, id, "make a file")
	_ = e.Store.update(func(st *State) error { st.chat(id).Network = "open"; return nil })
	shared, err := e.Fork(context.Background(), id, "", false, cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().chat(shared.ID).Network != "open" || len(sharing.actions("egress_set")) != 0 {
		t.Fatalf("shared fork: %q, %v", e.Store.Snapshot().chat(shared.ID).Network, sharing.actions("egress_set"))
	}
	copied, err := e.Fork(context.Background(), id, "", true, cv.Actor{PrincipalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	fork := e.Store.Snapshot().chat(copied.ID)
	sets := sharing.actions("egress_set")
	if fork.Network != "open" || len(sets) != 1 || agent.Map(sets[0]["data"])["sandboxID"] != fork.SandboxID || agent.Map(sets[0]["data"])["mode"] != "open" {
		t.Fatalf("copied fork: %q %v", fork.Network, sets)
	}
	if clones := w.requestsOf("clone"); len(clones) != 1 {
		t.Fatalf("clones: %+v", clones)
	}
	_ = e.Store.update(func(st *State) error { st.chat(id).Network = ""; return nil })
	if _, err = e.Fork(context.Background(), id, "", true, cv.Actor{PrincipalID: "owner"}); err != nil || len(sharing.actions("egress_set")) != 1 {
		t.Fatalf("copy of a workspace on the install's setting: %v %v", err, sharing.actions("egress_set"))
	}
}
