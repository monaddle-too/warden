package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// The host tools travel with the session of a jailbroken workspace only:
// turned on, the next session start advertises them and the prompt says
// what they are; turned off, the next start drops them; a workspace never
// opted in never sees them; and the switch needs this Warden's
// dogfood.jailbreak, is audited through the policy service, and is
// refused for an unknown workspace.
func TestHostToolsTravelWithTheJailbrokenSession(t *testing.T) {
	sharing, socket := newFakeSharing(t)
	e, w, _ := setup(t, func(e *Engine) { e.PolicyAddress = "unix://" + socket; e.Jailbreak = true })
	ctx := context.Background()
	owner := cv.Actor{PrincipalID: "sub-owner", Name: "The Owner"}
	id, err := e.Create("Dogfood", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sbx := e.Store.Snapshot().chat(id).SandboxID
	if err := e.SetWorkspaceJailbreak(ctx, "nope", true, owner); err == nil {
		t.Fatal("unknown workspace accepted")
	}
	if err := e.SetWorkspaceJailbreak(ctx, sbx, true, owner); err != nil {
		t.Fatal(err)
	}
	if err := e.SetWorkspaceJailbreak(ctx, sbx, true, owner); err != nil {
		t.Fatal("setting it again is not an error")
	}
	events := sharing.actions("host_event")
	if len(events) != 1 {
		t.Fatalf("host events: %v", events)
	}
	if d := agent.Map(events[0]["data"]); d["event"] != "workspace.jailbreak" || d["on"] != true || d["actor"] != "The Owner" || d["sandboxID"] != sbx || d["chatID"] != id {
		t.Fatalf("workspace.jailbreak: %v", d)
	}
	if !e.Store.Snapshot().chat(id).Jailbroken {
		t.Fatal("chat not marked")
	}
	if envs, _ := e.Environments(ctx); len(envs) != 1 || !envs[0].Jailbroken {
		t.Fatalf("environments: %+v", envs)
	}
	if v := e.View(); !v.AgentOptions.Jailbreak {
		t.Fatal("agentOptions.jailbreak not advertised")
	}
	tools, developer := sessionStart(t, e, w, id, "hi", 1)
	for _, name := range []string{"host_run", "host_put", "host_get", "host_expose", "host_status"} {
		if !slices.Contains(tools, name) {
			t.Fatalf("%s not advertised: %v", name, tools)
		}
	}
	if !strings.Contains(developer, "This workspace has host access") {
		t.Fatalf("prompt: %s", developer)
	}
	// A sibling on the same workspace is jailbroken too; a fresh one not.
	sibling, err := e.Create("Sibling", sbx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.Create("Other", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.Store.Snapshot(); !st.chat(sibling).Jailbroken || st.chat(other).Jailbroken {
		t.Fatal("host access did not follow the workspace")
	}
	tools, developer = sessionStart(t, e, w, other, "hi", 2)
	if slices.Contains(tools, "host_run") || strings.Contains(developer, "host access") {
		t.Fatalf("a plain workspace got the host tools: %v", tools)
	}
	// Off: the next start drops them, the switch is audited.
	if err := e.SetWorkspaceJailbreak(ctx, sbx, false, owner); err != nil {
		t.Fatal(err)
	}
	if st := e.Store.Snapshot(); st.chat(id).Jailbroken || st.chat(sibling).Jailbroken {
		t.Fatal("still marked")
	}
	tools, _ = sessionStart(t, e, w, id, "again", 3)
	if slices.Contains(tools, "host_run") {
		t.Fatalf("host tools survived the switch: %v", tools)
	}
	if events := sharing.actions("host_event"); len(events) != 2 || agent.Map(events[1]["data"])["on"] != false {
		t.Fatalf("host events: %v", events)
	}
	// Without dogfood.jailbreak nothing can be turned on; off stays possible.
	e.Jailbreak = false
	if err := e.SetWorkspaceJailbreak(ctx, sbx, true, owner); err == nil || !strings.Contains(err.Error(), "dogfood.jailbreak") {
		t.Fatalf("turned on without the install setting: %v", err)
	}
	if err := e.SetWorkspaceJailbreak(ctx, sbx, false, owner); err != nil {
		t.Fatal(err)
	}
	if v := e.View(); v.AgentOptions.Jailbreak {
		t.Fatal("agentOptions.jailbreak advertised while off")
	}
}

// sessionStart sends text to the chat, waits for the fake worker's n-th
// turn to begin (the session started with its tools), stops the chat and
// returns the tool names and developer instructions of that start.
func sessionStart(t *testing.T, e *Engine, w *fakeWorker, id, text string, n int) ([]string, string) {
	t.Helper()
	if err := e.Message(id, text, cv.ID()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return w.turnCount() == n })
	w.mu.Lock()
	tools, developer := append([]string(nil), w.tools...), w.developer
	w.mu.Unlock()
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { s := e.Store.Snapshot().chat(id).Status; return s == "interrupted" || s == "idle" })
	return tools, developer
}

// Each host tool is refused on a workspace without host access, validates
// its arguments, becomes the runner's host.* operation with the answer as
// the tool's result, and is audited through the policy service.
func TestHostCallsRunOnTheRunnerAndAreAudited(t *testing.T) {
	sharing, socket := newFakeSharing(t)
	e, w, _ := setup(t, func(e *Engine) {
		e.PolicyAddress = "unix://" + socket
		e.Jailbreak = true
		e.PublicPreviewSuffix = "localhost"
		e.PreviewScheme, e.PreviewPort = "http", "18781"
	})
	ctx := context.Background()
	id, err := e.Create("Dogfood", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := *e.Store.Snapshot().chat(id)
	call := func(c *Chat, name, args string) (map[string]any, error) {
		t.Helper()
		v, err := e.hostCall(ctx, c, name, []byte(args))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out, nil
	}
	if _, err := call(&plain, "host_run", `{"command":"uname -a"}`); err == nil || err.Error() != "host access is off for this workspace" {
		t.Fatalf("plain workspace: %v", err)
	}
	if len(w.requestsOf("host.exec")) != 0 {
		t.Fatal("the runner was asked")
	}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		c.Jailbroken, c.Status, c.RunID = true, "running", "run"
		return nil
	})
	c := e.Store.Snapshot().chat(id)
	// host_run: the command, cwd and timeout reach the runner; the answer
	// is the result; the audit says what ran and how it ended.
	for _, bad := range []string{`{"command":""}`, `{"command":"x","timeout":5000}`, `{"command":"x","extra":1}`, `{"command":"` + strings.Repeat("y", sandbox.MaxHostCommand+1) + `"}`} {
		if _, err := call(c, "host_run", bad); err == nil {
			t.Fatalf("accepted %s", bad[:20])
		}
	}
	out, err := call(c, "host_run", `{"command":"uname -a && echo hello-from-host","cwd":"/Users/me/src","timeout":30}`)
	if err != nil {
		t.Fatal(err)
	}
	if out["exitCode"] != 0.0 || out["output"] != "hello-from-host\n" || out["cwd"] != "/Users/me/src" || out["timedOut"] != false {
		t.Fatalf("%v", out)
	}
	reqs := w.requestsOf("host.exec")
	if len(reqs) != 1 || reqs[0].Command != "uname -a && echo hello-from-host" || reqs[0].Directory != "/Users/me/src" || reqs[0].Timeout != 30 || reqs[0].ChatID != id {
		t.Fatalf("%+v", reqs)
	}
	events := sharing.actions("host_event")
	if len(events) != 1 {
		t.Fatalf("%v", events)
	}
	if d := agent.Map(events[0]["data"]); d["event"] != "host.exec" || d["command"] != "uname -a && echo hello-from-host" || d["cwd"] != "/Users/me/src" || d["exit"] != 0.0 || d["bytes"] != 16.0 || d["duration_ms"] != 5.0 || d["chatID"] != id || d["sandboxID"] != c.SandboxID || d["principal"] != "agent" || d["actor"] != "agent" {
		t.Fatalf("host.exec audit: %v", d)
	}
	// host_put and host_get: the paths each way, the size moved.
	if _, err := call(c, "host_put", `{"from":"/home/agent/workspace/x"}`); err == nil {
		t.Fatal("put without to accepted")
	}
	out, err = call(c, "host_put", `{"from":"/home/agent/workspace/out.txt","to":"/Users/me/out.txt"}`)
	if err != nil || out["bytes"] != 12.0 || out["direction"] != "sandbox → host" {
		t.Fatalf("%v %v", out, err)
	}
	puts := w.requestsOf("host.put")
	if len(puts) != 1 || puts[0].Directory != "/home/agent/workspace/out.txt" || puts[0].Path != "/Users/me/out.txt" || puts[0].Replace {
		t.Fatalf("%+v", puts)
	}
	// replace reaches the runner and the audit entry.
	if _, err = call(c, "host_put", `{"from":"/home/agent/workspace/tree","to":"/Users/me/tree","replace":true}`); err != nil {
		t.Fatal(err)
	}
	if puts = w.requestsOf("host.put"); len(puts) != 2 || !puts[1].Replace || puts[1].Path != "/Users/me/tree" {
		t.Fatalf("%+v", puts)
	}
	out, err = call(c, "host_get", `{"from":"/Users/me/src","to":"/home/agent/workspace/src"}`)
	if err != nil || out["bytes"] != 12.0 || out["direction"] != "host → sandbox" {
		t.Fatalf("%v %v", out, err)
	}
	gets := w.requestsOf("host.get")
	if len(gets) != 1 || gets[0].Path != "/Users/me/src" || gets[0].Directory != "/home/agent/workspace/src" {
		t.Fatalf("%+v", gets)
	}
	events = sharing.actions("host_event")
	if len(events) != 4 || agent.Map(events[1]["data"])["event"] != "host.file" || agent.Map(events[1]["data"])["direction"] != "sandbox → host" || agent.Map(events[2]["data"])["replace"] != true || agent.Map(events[3]["data"])["from"] != "/Users/me/src" || agent.Map(events[3]["data"])["bytes"] != 12.0 {
		t.Fatalf("host.file audit: %v", events)
	}
	// host_status: the runner's answer as is.
	w.mu.Lock()
	w.host = &sandbox.HostStatus{OS: "darwin", Arch: "arm64", Home: "/Users/me", StateDir: "/Users/me/.warden", Instances: []sandbox.HostInstance{{Name: "default", StateDir: "/Users/me/.warden", This: true}}}
	w.mu.Unlock()
	out, err = call(c, "host_status", `{}`)
	if err != nil || out["os"] != "darwin" || len(agent.Array(out["instances"])) != 1 {
		t.Fatalf("%v %v", out, err)
	}
	if out, err = call(c, "host_status", ``); err != nil || out["home"] != "/Users/me" {
		t.Fatalf("empty arguments: %v %v", out, err)
	}
	// host_expose: a preview binding whose upstream is the host, recorded
	// with the mark, reused for the same port, audited.
	if _, err := call(c, "host_expose", `{"port":0}`); err == nil {
		t.Fatal("port 0 accepted")
	}
	out, err = call(c, "host_expose", `{"port":18830,"name":"Dev Warden"}`)
	if err != nil {
		t.Fatal(err)
	}
	url := agent.String(out["url"])
	if !strings.HasPrefix(url, "http://") || !strings.HasSuffix(url, ".localhost:18781/") || out["port"] != 18830.0 || out["title"] != "Dev Warden" {
		t.Fatalf("%v", out)
	}
	exposes := w.requestsOf("host.expose")
	if len(exposes) != 1 || exposes[0].Port != 18830 || exposes[0].Title != "Dev Warden" {
		t.Fatalf("%+v", exposes)
	}
	ports := e.Store.Snapshot().Ports
	if len(ports) != 1 || ports[0].Upstream != sandbox.UpstreamHost || ports[0].Port != 18830 || ports[0].URL != url || ports[0].State != "approved" {
		t.Fatalf("%+v", ports)
	}
	if out2, err := call(c, "host_expose", `{"port":18830}`); err != nil || out2["url"] != url || len(e.Store.Snapshot().Ports) != 1 {
		t.Fatalf("second exposure: %v %v %d ports", out2, err, len(e.Store.Snapshot().Ports))
	}
	events = sharing.actions("host_event")
	if last := agent.Map(events[len(events)-1]["data"]); last["event"] != "host.expose" || last["port"] != 18830.0 || last["url"] != url {
		t.Fatalf("host.expose audit: %v", last)
	}
	if envs, _ := e.Environments(ctx); len(envs) != 1 || len(envs[0].Ports) != 1 || envs[0].Ports[0].Upstream != "host" {
		t.Fatalf("environments: %+v", envs)
	}
	if _, err := call(c, "host_bogus", `{}`); err == nil {
		t.Fatal("unknown host tool accepted")
	}
	// Host access turned off while the session runs refuses the next
	// call at once, even though the run's copy of the chat still says on
	// (found live: the switch was read from the run's snapshot).
	if err := e.SetWorkspaceJailbreak(ctx, c.SandboxID, false, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	before := len(w.requestsOf("host.exec"))
	if _, err := call(c, "host_run", `{"command":"echo should-be-refused"}`); err == nil || err.Error() != "host access is off for this workspace" || len(w.requestsOf("host.exec")) != before {
		t.Fatalf("call after the switch: %v", err)
	}
	if err := e.SetWorkspaceJailbreak(ctx, c.SandboxID, true, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	e.PublicPreviewSuffix = "localhost"
	// Without previews configured the exposure is refused with the reason.
	e.PublicPreviewSuffix = ""
	if _, err := call(c, "host_expose", `{"port":1}`); err == nil || !strings.Contains(err.Error(), "previews are not configured") {
		t.Fatalf("%v", err)
	}
}

// Host access is the owner's choice on POST chats and at
// environments/{id}/jailbreak (POST or PUT): admitted people cannot set
// it, a shared workspace already has its setting, it needs this Warden's
// dogfood.jailbreak, and a refusal at creation leaves no chat behind.
func TestHostAccessRoutes(t *testing.T) {
	sharing, socket := newFakeSharing(t)
	e, _, _ := setup(t, func(e *Engine) { e.PolicyAddress = "unix://" + socket; e.Jailbreak = true })
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
	code, v := call("POST", "chats", `{"title":"Jailbroken","provider":"codex","jailbreak":true}`, owner)
	if code != 200 {
		t.Fatalf("create: %d %v", code, v)
	}
	c := e.Store.Snapshot().chat(agent.String(v["id"]))
	if c == nil || !c.Jailbroken {
		t.Fatalf("chat: %+v", c)
	}
	if events := sharing.actions("host_event"); len(events) != 1 || agent.Map(events[0]["data"])["actor"] != "The Owner" {
		t.Fatalf("audit: %v", events)
	}
	if code, _ = call("POST", "chats", `{"title":"Alice","provider":"codex","jailbreak":true}`, alice); code != 403 {
		t.Fatalf("alice creates: %d", code)
	}
	if code, _ = call("POST", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":false}`, alice); code != 403 {
		t.Fatalf("alice sets: %d", code)
	}
	before := len(e.Store.Snapshot().Chats)
	if code, _ = call("POST", "chats", `{"title":"Shared","provider":"codex","sandboxID":"`+c.SandboxID+`","jailbreak":true}`, owner); code != 409 || len(e.Store.Snapshot().Chats) != before {
		t.Fatalf("shared with the option: %d, %d chats", code, len(e.Store.Snapshot().Chats))
	}
	// A plain chat on the workspace inherits it.
	if code, v = call("POST", "chats", `{"title":"Shared plain","provider":"codex","sandboxID":"`+c.SandboxID+`"}`, alice); code != 200 || !e.Store.Snapshot().chat(agent.String(v["id"])).Jailbroken {
		t.Fatalf("sibling: %d %v", code, v)
	}
	if code, _ = call("POST", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":false}`, owner); code != 200 || e.Store.Snapshot().chat(c.ID).Jailbroken {
		t.Fatalf("owner turns it off: %d", code)
	}
	if code, _ = call("PUT", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":true}`, owner); code != 200 || !e.Store.Snapshot().chat(c.ID).Jailbroken {
		t.Fatalf("owner turns it on with PUT: %d", code)
	}
	if code, _ = call("PUT", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":true}`, nil); code != 200 {
		t.Fatalf("local owner: %d", code)
	}
	if code, _ = call("POST", "environments/nope/jailbreak", `{"jailbreak":true}`, owner); code != 409 {
		t.Fatalf("unknown workspace: %d", code)
	}
	if envs, _ := e.Environments(context.Background()); len(envs) != 1 || !envs[0].Jailbroken {
		t.Fatalf("environments: %+v", envs)
	}
	// Without dogfood.jailbreak: on is refused, a creation with the option
	// leaves nothing behind, off still works.
	e.Jailbreak = false
	before = len(e.Store.Snapshot().Chats)
	if code, v = call("POST", "chats", `{"title":"Refused","provider":"codex","jailbreak":true}`, owner); code != 409 || !strings.Contains(agent.String(v["error"]), "dogfood.jailbreak") || len(e.Store.Snapshot().Chats) != before {
		t.Fatalf("refused creation: %d %v, %d chats", code, v, len(e.Store.Snapshot().Chats))
	}
	if code, _ = call("POST", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":false}`, owner); code != 200 || e.Store.Snapshot().chat(c.ID).Jailbroken {
		t.Fatalf("off without the install setting: %d", code)
	}
	if code, _ = call("POST", "environments/"+c.SandboxID+"/jailbreak", `{"jailbreak":true}`, owner); code != 409 {
		t.Fatalf("on without the install setting: %d", code)
	}
}

// A rule on mcp__warden__host_run takes a command spec like a Bash rule,
// "Allow always" records one the same way, and the chat's mode decides a
// host call as it does any tool: auto runs it unprompted, ask makes a
// card, a deny rule declines.
func TestHostRunRulesAndModes(t *testing.T) {
	if _, err := NewRule(RuleAllow, "mcp__warden__host_run(warden *)"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRule(RuleAllow, "mcp__warden__host_status(x)"); err == nil {
		t.Fatal("a spec on host_status accepted")
	}
	run := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	allow := Rule{Kind: RuleAllow, Pattern: "mcp__warden__host_run(warden *)"}
	if !allow.Matches(hostRunTool, run("warden release list")) || allow.Matches(hostRunTool, run("rm -rf x")) || allow.Matches(hostRunTool, run("warden x && rm y")) || allow.Matches("Bash", run("warden release list")) {
		t.Fatal("allow rule matching")
	}
	deny := Rule{Kind: RuleDeny, Pattern: "mcp__warden__host_run(rm *)"}
	if !deny.Matches(hostRunTool, run("ls; rm x")) || deny.Matches(hostRunTool, run("ls")) {
		t.Fatal("deny rule matching")
	}
	if got := RuleFor(hostRunTool, run("warden release install x --instance dev")); got != "mcp__warden__host_run(warden release *)" {
		t.Fatalf("RuleFor: %s", got)
	}
	if got := RuleFor(hostRunTool, run("cd x && make")); got != "mcp__warden__host_run(cd x && make)" {
		t.Fatalf("RuleFor chained: %s", got)
	}
	if got := RuleLabel("mcp__warden__host_run(warden release *)"); got != "`warden release` host commands" {
		t.Fatalf("RuleLabel: %s", got)
	}
	if got := RuleLabel("mcp__warden__host_run"); got != "host commands" {
		t.Fatalf("RuleLabel bare: %s", got)
	}
	st := &State{Environments: map[string]*EnvironmentRecord{"sbx": {Rules: []Rule{{ID: "w1", Kind: RuleDeny, Pattern: "mcp__warden__host_run(rm *)"}}}}}
	c := &Chat{SandboxID: "sbx", Mode: ModeAuto, Rules: []Rule{{ID: "c1", Kind: RuleAllow, Pattern: "mcp__warden__host_run(warden *)"}}}
	if v := st.decide(c, hostRunTool, run("uname -a")); v == nil || v.Decision != "accept" || v.Rule != nil {
		t.Fatalf("auto: %+v", v)
	}
	if v := st.decide(c, hostRunTool, run("rm -rf /")); v == nil || v.Decision != "decline" || v.Rule == nil || v.Rule.Rule.ID != "w1" {
		t.Fatalf("deny rule under auto: %+v", v)
	}
	c.Mode = ModeAsk
	if v := st.decide(c, hostRunTool, run("uname -a")); v != nil {
		t.Fatalf("ask: %+v", v)
	}
	if v := st.decide(c, hostRunTool, run("warden instance list")); v == nil || v.Decision != "accept" || v.Rule == nil || v.Rule.Rule.ID != "c1" {
		t.Fatalf("allow rule under ask: %+v", v)
	}
	if v := st.decide(c, "mcp__warden__host_status", nil); v != nil {
		t.Fatalf("host_status under ask: %+v", v)
	}
}

// Stop ends the chat's host calls in flight and forgets them.
func TestStopEndsHostCalls(t *testing.T) {
	e, _, _ := setup(t)
	ctx, done := e.trackHost(context.Background(), "chat-one")
	other, doneOther := e.trackHost(context.Background(), "chat-two")
	defer doneOther()
	if n := e.cancelHostCalls("chat-one"); n != 1 || ctx.Err() == nil || other.Err() != nil {
		t.Fatalf("cancelled %d, ctx %v, other %v", n, ctx.Err(), other.Err())
	}
	done()
	if n := e.cancelHostCalls("chat-one"); n != 0 {
		t.Fatalf("forgot nothing: %d", n)
	}
}
