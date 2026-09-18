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

// An "Allow always" answered for the workspace answers the same call in a
// sibling chat (present or created later) without a card, and the rule
// lives on the environment record with its origin and author.
func TestWorkspaceRuleReachesSiblingChats(t *testing.T) {
	e, w, one := claudeSetup(t)
	ctx := context.Background()
	sandbox := e.Store.Snapshot().chat(one).SandboxID
	w.asks = [][]map[string]any{{bashAsk("touch a")}, {bashAsk("touch b"), bashAsk("rm b")}}
	if err := e.SetMode(ctx, one, ModeAsk); err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, one, "one")
	ap := pendingApproval(t, e, one)
	if ap.Params["rule"] != "Bash(touch *)" || ap.Params["always"] != "`touch` commands" {
		t.Fatalf("%+v", ap.Params)
	}
	dan := cv.Actor{PrincipalID: "p-dan", Name: "Dan"}
	if err := e.Answer(one, ap.ID, Answer{Always: true, Scope: "workspace"}, dan); err != nil {
		t.Fatal(err)
	}
	idle(t, e, one)
	st := e.Store.Snapshot()
	env := st.Environments[sandbox]
	if env == nil || len(env.Rules) != 1 || env.Rules[0].Pattern != "Bash(touch *)" || env.Rules[0].Kind != RuleAllow || env.Rules[0].Origin != "always" || env.Rules[0].ChatID != one || env.Rules[0].By == nil || env.Rules[0].By.Name != "Dan" || env.Rules[0].ID == "" {
		t.Fatalf("%+v", env)
	}
	if len(st.chat(one).Rules) != 0 {
		t.Fatal("rule also on the chat")
	}
	// The sibling, created after the rule, in ask mode too.
	two, err := e.Create("Two", sandbox, "", nil, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetMode(ctx, two, ModeAsk); err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, two, "two")
	ap = pendingApproval(t, e, two)
	if agent.Map(ap.Params["input"])["command"] != "rm b" {
		t.Fatalf("touch b asked: %+v", ap.Params)
	}
	if err := e.Answer(two, ap.ID, Answer{Allow: false, Message: "keep it"}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	idle(t, e, two)
	answers := w.answersSoFar()
	if len(answers) != 3 || agent.Map(answers[1]["response"])["behavior"] != "allow" || agent.Map(answers[2]["response"])["behavior"] != "deny" {
		t.Fatalf("%+v", answers)
	}
	// History: chat one's card answer with the rule it made; chat two's
	// rule decision and its card denial.
	h1, _ := e.Permissions(one)
	h2, _ := e.Permissions(two)
	if len(h1) != 1 || h1[0].How != "card" || h1[0].Decision != "allow" || h1[0].By.Name != "Dan" || h1[0].Rule == nil || h1[0].Rule.Pattern != "Bash(touch *)" || h1[0].Scope != "workspace" || h1[0].Summary != "touch a" || h1[0].Tool != "Bash" {
		t.Fatalf("%+v", h1)
	}
	if len(h2) != 2 || h2[0].How != "rule" || h2[0].Decision != "allow" || h2[0].Scope != "workspace" || h2[0].Rule.Pattern != "Bash(touch *)" || h2[0].By != nil || h2[1].How != "card" || h2[1].Decision != "deny" || h2[1].Message != "keep it" || h2[1].By.PrincipalID != "owner" || h2[1].Summary != "rm b" {
		t.Fatalf("%+v", h2)
	}
	// The state clients stream leaves the history out; the workspace view
	// carries the rules.
	if e.View().Chats[0].Permissions != nil {
		t.Fatal("history in the state")
	}
	envs, _ := e.Environments(ctx)
	if len(envs) != 1 || len(envs[0].Rules) != 1 || envs[0].Rules[0].Pattern != "Bash(touch *)" {
		t.Fatalf("%+v", envs)
	}
	// Removing the rule brings the card back.
	if err := e.RemoveRule(sandbox, env.Rules[0].ID); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Environments[sandbox] != nil {
		t.Fatal("empty record kept")
	}
	if err := e.RemoveRule(sandbox, "nope"); err == nil || err.Error() != "no such rule" {
		t.Fatal(err)
	}
}

// In auto, a deny rule declines without asking (the model reads the
// rule), an ask rule makes a card; the rest is allowed as auto does.
func TestDenyAndAskRulesInAuto(t *testing.T) {
	e, w, id := claudeSetup(t)
	sandbox := e.Store.Snapshot().chat(id).SandboxID
	w.asks = [][]map[string]any{{bashAsk("rm -rf build"), writeAsk("/home/agent/workspace/f", "x"), bashAsk("touch y"), bashAsk("git status && rm x")}}
	owner := cv.Actor{PrincipalID: "owner"}
	if _, err := e.AddRule(sandbox, "deny", "Bash(rm *)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(id, "ask", "Edit(*)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRule(id, "ask", "Edit(*)", owner); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := e.AddRule(sandbox, "deny", "Bash(", owner); err == nil {
		t.Fatal("bad pattern accepted")
	}
	if _, err := e.AddRule("nope", "deny", "Bash", owner); err == nil {
		t.Fatal("unknown workspace accepted")
	}
	sendAndDeliver(t, e, id, "go")
	ap := pendingApproval(t, e, id)
	if ap.Params["tool"] != "Write" {
		t.Fatalf("%+v", ap.Params)
	}
	if err := e.Answer(id, ap.ID, Answer{Allow: true}, owner); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	answers := w.answersSoFar()
	if len(answers) != 4 {
		t.Fatalf("%+v", answers)
	}
	first := agent.Map(answers[0]["response"])
	if first["behavior"] != "deny" || !strings.Contains(agent.String(first["message"]), "the user said: a permission rule of this workspace denies it (deny Bash(rm *))") {
		t.Fatalf("%+v", first)
	}
	for i, want := range []string{"deny", "allow", "allow", "deny"} {
		if got := agent.Map(answers[i]["response"])["behavior"]; got != want {
			t.Fatalf("answer %d: %v, want %s", i, got, want)
		}
	}
	events, _ := e.Permissions(id)
	var got []string
	for _, ev := range events {
		line := ev.How + " " + ev.Decision + " " + ev.Tool + " " + ev.Summary
		if ev.Rule != nil {
			line += " [" + ev.Scope + " " + ev.Rule.Kind + " " + ev.Rule.Pattern + "]"
		}
		got = append(got, line)
	}
	want := []string{"rule deny Bash rm -rf build [workspace deny Bash(rm *)]", "card allow Write /home/agent/workspace/f", "auto allow Bash touch y", "rule deny Bash git status && rm x [workspace deny Bash(rm *)]"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("history:\n%s", strings.Join(got, "\n"))
	}
}

// A deny wins over an ask over an allow whichever scope holds it; the
// chat's rules are consulted before the workspace's within a kind.
func TestRulePrecedence(t *testing.T) {
	st := &State{Environments: map[string]*EnvironmentRecord{"sbx": {Rules: []Rule{{ID: "w1", Kind: RuleDeny, Pattern: "Bash(rm *)"}, {ID: "w2", Kind: RuleAllow, Pattern: "Bash(git *)"}, {ID: "w3", Kind: RuleAsk, Pattern: "Edit"}}}}}
	c := &Chat{SandboxID: "sbx", Mode: ModeAuto, Rules: []Rule{{ID: "c1", Kind: RuleAllow, Pattern: "Bash(rm *)"}, {ID: "c2", Kind: RuleAllow, Pattern: "Bash(git *)"}, {ID: "c3", Kind: RuleAllow, Pattern: "Edit(src/**)"}}}
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	cases := []struct {
		tool  string
		input map[string]any
		rule  string
		want  string // decision, "card" for none
	}{
		{"Bash", bash("rm x"), "w1", "decline"},
		{"Bash", bash("git status"), "c2", "accept"},
		{"Edit", map[string]any{"file_path": "src/a.go"}, "w3", "card"},
		{"Bash", bash("ls"), "", "accept"},
		{"ExitPlanMode", nil, "", "card"},
	}
	for _, tc := range cases {
		d := st.decideByRule(c, tc.tool, tc.input)
		if (d == nil) != (tc.rule == "") || d != nil && d.Rule.ID != tc.rule {
			t.Errorf("%s %v: %+v", tc.tool, tc.input, d)
		}
		v := st.decide(c, tc.tool, tc.input)
		got := "card"
		if v != nil {
			got = v.Decision
		}
		if got != tc.want {
			t.Errorf("%s %v: %s, want %s", tc.tool, tc.input, got, tc.want)
		}
	}
	c.Mode = ModeAsk
	if v := st.decide(c, "Bash", bash("ls")); v != nil {
		t.Error("ask mode without a rule accepted")
	}
	if v := st.decide(c, "Bash", bash("git log")); v == nil || v.Decision != "accept" || v.Rule.Scope != "chat" {
		t.Errorf("%+v", v)
	}
	// The history keeps the last 200.
	for i := 0; i < historyCap+5; i++ {
		c.record(PermissionEvent{Tool: "Bash", Summary: strings.Repeat("x", i%3)})
	}
	if len(c.Permissions) != historyCap || c.Permissions[0].ID == "" || c.Permissions[0].At == 0 {
		t.Fatal(len(c.Permissions))
	}
}

// The routes: the rules of a workspace or a chat listed, added and
// removed; the chat's permission history.
func TestRulesRoutes(t *testing.T) {
	e, _, id := claudeSetup(t)
	sandbox := e.Store.Snapshot().chat(id).SandboxID
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	call := func(method, path, body string) (int, map[string]any) {
		var r *httptest.ResponseRecorder
		req := httptest.NewRequest(method, h.Origin+"/api/"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer private")
		req.Header.Set("X-Warden-Principal", "p-dan")
		req.Header.Set("X-Warden-Name", "Dan")
		r = httptest.NewRecorder()
		h.ServeHTTP(r, req)
		var out map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &out)
		return r.Code, out
	}
	code, out := call("POST", "environments/"+sandbox+"/rules", `{"kind":"deny","pattern":"Bash(rm *)"}`)
	if code != 200 || out["kind"] != "deny" || out["pattern"] != "Bash(rm *)" || out["origin"] != "editor" || agent.Map(out["by"])["name"] != "Dan" || out["id"] == "" {
		t.Fatalf("%d %+v", code, out)
	}
	ruleID := agent.String(out["id"])
	if code, out = call("POST", "environments/"+sandbox+"/rules", `{"kind":"deny","pattern":"Bash("}`); code != 409 || !strings.Contains(agent.String(out["error"]), "parenthesis") {
		t.Fatalf("%d %+v", code, out)
	}
	if code, out = call("POST", "chats/"+id+"/rules", `{"kind":"allow","pattern":"Edit(src/**)"}`); code != 200 || out["pattern"] != "Edit(src/**)" {
		t.Fatalf("%d %+v", code, out)
	}
	chatRule := agent.String(out["id"])
	code, out = call("GET", "environments/"+sandbox+"/rules", "")
	rules, chats := agent.Array(out["rules"]), agent.Array(out["chats"])
	if code != 200 || out["workspace"] != sandbox || len(rules) != 1 || agent.Map(rules[0])["pattern"] != "Bash(rm *)" || len(chats) != 1 || agent.Map(chats[0])["id"] != id || len(agent.Array(agent.Map(chats[0])["rules"])) != 1 {
		t.Fatalf("%d %+v", code, out)
	}
	// The same view through the chat.
	if code, out2 := call("GET", "chats/"+id+"/rules", ""); code != 200 || out2["workspace"] != sandbox || len(agent.Array(out2["rules"])) != 1 {
		t.Fatalf("%d %+v", code, out2)
	}
	if code, _ = call("POST", "chats/"+id+"/rules/"+chatRule+"/remove", `{}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ = call("POST", "environments/"+sandbox+"/rules/"+ruleID+"/remove", `{}`); code != 200 {
		t.Fatal(code)
	}
	if code, out = call("POST", "environments/"+sandbox+"/rules/"+ruleID+"/remove", `{}`); code != 409 {
		t.Fatalf("%d %+v", code, out)
	}
	code, out = call("GET", "environments/"+sandbox+"/rules", "")
	if code != 200 || len(agent.Array(out["rules"])) != 0 || len(agent.Array(agent.Map(agent.Array(out["chats"])[0])["rules"])) != 0 {
		t.Fatalf("%d %+v", code, out)
	}
	if code, out = call("GET", "environments/nope/rules", ""); code != 409 {
		t.Fatalf("%d %+v", code, out)
	}
	if code, out = call("GET", "chats/"+id+"/permissions", ""); code != 200 || len(agent.Array(out["events"])) != 0 {
		t.Fatalf("%d %+v", code, out)
	}
	if code, _ = call("GET", "chats/nope/permissions", ""); code != 409 {
		t.Fatal(code)
	}
	// A Codex chat takes no rules of its own.
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	if code, out = call("POST", "chats/"+codex+"/rules", `{"kind":"allow","pattern":"Bash"}`); code != 409 {
		t.Fatalf("%d %+v", code, out)
	}
}
