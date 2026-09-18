package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// claudeWorker is a worker whose agent stream speaks Claude Code's
// stream-json, so the engine's permission handling is exercised through
// the adapter. Each user message is one turn: the scripted asks are sent
// as can_use_tool requests, their answers recorded, then a result ends
// the turn. Control requests from Warden (set_permission_mode, set_model,
// set_max_thinking_tokens, apply_flag_settings) are recorded and answered
// with success (bar two models, see below).
type claudeWorker struct {
	mu       sync.Mutex
	asks     [][]map[string]any // per turn, the can_use_tool requests to make
	answers  []map[string]any   // the control responses received, in order
	controls []map[string]any   // set_permission_mode requests received
	turns    int
	// hold, when set, keeps a turn open (after its asks) until closed.
	hold chan struct{}
	// frames are extra CLI frames to emit at the start of a turn.
	frames []map[string]any
	// session is the id system/init reports ("claude-session" when
	// empty); requests are every worker request, calls and streams
	// (fork_test.go reads the prepare and stream ones); rewinds the
	// rewind_conversation requests received, answered as rewound; aside
	// is what an "aside" call answers.
	session  string
	requests []sandbox.Request
	rewinds  []map[string]any
	aside    *sandbox.AsideResult
	// oneshot is what a "oneshot" call answers (nil: a title); catalog
	// is what list_models answers (nil: two rows; an empty list: a
	// refusal).
	oneshot *sandbox.AsideResult
	catalog []map[string]any
	listed  int // list_models requests received
	// oneshotGate, when set, holds a "oneshot" answer until it is closed.
	oneshotGate chan struct{}
}

// initModel is the model the scripted CLI reports in its system/init: the
// last set_model it accepted, resolved the way the CLI names models, else
// sonnet.
func (w *claudeWorker) initModel() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	model := "sonnet"
	for _, c := range w.controls {
		if m, ok := c["model"].(string); ok && c["subtype"] == "set_model" && m != "bogus" && m != "unswitchable" {
			model = m
		}
	}
	if model == "default" {
		model = "sonnet"
	}
	return "claude-" + model + "-5"
}

func (w *claudeWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	w.mu.Lock()
	w.requests = append(w.requests, r)
	aside, oneshot, gate := w.aside, w.oneshot, w.oneshotGate
	w.mu.Unlock()
	if r.Operation == "oneshot" && gate != nil {
		<-gate
	}
	if r.Operation == "aside" {
		if aside == nil {
			aside = &sandbox.AsideResult{Text: "the answer", CostUSD: 0.01, Input: 100, Output: 5}
		}
		return sandbox.Response{Version: 2, Aside: aside}, nil
	}
	if r.Operation == "oneshot" {
		if oneshot == nil {
			oneshot = &sandbox.AsideResult{Text: "\"Greeting the assistant.\"\n", CostUSD: 0.0004, Input: 60, Output: 4}
		}
		return sandbox.Response{Version: 2, Aside: oneshot}, nil
	}
	return sandbox.Response{Version: 2, Directory: "/home/agent/workspace", Sandbox: &sandbox.SandboxInfo{ID: r.SandboxID, ProjectID: r.ProjectID}}, nil
}

func (w *claudeWorker) requestsOf(op string) []sandbox.Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []sandbox.Request
	for _, r := range w.requests {
		if r.Operation == op {
			out = append(out, r)
		}
	}
	return out
}

func (w *claudeWorker) Open(ctx context.Context, r sandbox.Request) (io.ReadWriteCloser, sandbox.Response, error) {
	client, server := net.Pipe()
	w.mu.Lock()
	w.requests = append(w.requests, r)
	w.mu.Unlock()
	go func() {
		defer server.Close()
		enc := json.NewEncoder(server)
		var encMu sync.Mutex
		send := func(v map[string]any) { encMu.Lock(); defer encMu.Unlock(); _ = enc.Encode(v) }
		scan := bufio.NewScanner(server)
		scan.Buffer(make([]byte, 65536), 8<<20)
		for scan.Scan() {
			var v map[string]any
			if json.Unmarshal(scan.Bytes(), &v) != nil {
				return
			}
			switch v["type"] {
			case "control_request":
				req := agent.Map(v["request"])
				switch req["subtype"] {
				case "initialize":
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{}}})
				case "list_models":
					// The catalog (catalog.go): two rows unless the test
					// scripted others; an empty script is a refusal.
					w.mu.Lock()
					catalog := w.catalog
					w.listed++
					w.mu.Unlock()
					if catalog == nil {
						catalog = []map[string]any{
							{"value": "sonnet", "resolvedModel": "claude-sonnet-5", "displayName": "Sonnet", "description": "Sonnet 5 · Efficient", "supportsEffort": true, "supportedEffortLevels": []any{"low", "medium", "high"}, "supportsAdaptiveThinking": true},
							{"value": "haiku", "resolvedModel": "claude-haiku-4-5", "displayName": "Haiku", "description": "Haiku 4.5 · Fastest"},
						}
					}
					if len(catalog) == 0 {
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": v["request_id"], "error": "list_models is not available"}})
						continue
					}
					rows := []any{}
					for _, row := range catalog {
						rows = append(rows, row)
					}
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{"models": rows}}})
				case "set_permission_mode":
					w.mu.Lock()
					w.controls = append(w.controls, req)
					w.mu.Unlock()
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{"mode": req["mode"]}}})
					send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": req["mode"]})
				case "rewind_conversation":
					w.mu.Lock()
					w.rewinds = append(w.rewinds, req)
					w.mu.Unlock()
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{"rewound": true, "targetMessageUuid": req["target_message_uuid"]}}})
				case "set_model", "set_max_thinking_tokens", "apply_flag_settings":
					// The session settings (settings_test.go): recorded and
					// accepted, except a model the CLI does not know
					// ("bogus", as the real CLI words it) and one it
					// cannot switch to for another reason ("unswitchable").
					w.mu.Lock()
					w.controls = append(w.controls, req)
					w.mu.Unlock()
					switch req["model"] {
					case "bogus":
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": v["request_id"], "error": "Model 'bogus' not found"}})
					case "unswitchable":
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": v["request_id"], "error": "set_model is not available here"}})
					default:
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"]}})
					}
				}
			case "control_response":
				w.mu.Lock()
				w.answers = append(w.answers, agent.Map(v["response"]))
				w.mu.Unlock()
			case "user":
				w.mu.Lock()
				turn := w.turns
				w.turns++
				var asks []map[string]any
				if turn < len(w.asks) {
					asks = w.asks[turn]
				}
				frames := w.frames
				w.frames = nil
				hold := w.hold
				session := w.session
				w.mu.Unlock()
				if session == "" {
					session = "claude-session"
				}
				go func() {
					send(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": w.initModel(), "fast_mode_state": "off"})
					for _, f := range frames {
						send(f)
					}
					for i, ask := range asks {
						send(map[string]any{"type": "control_request", "request_id": "ask-" + cv.ID(), "request": ask})
						// Wait for the answer before the next ask.
						want := len(w.answersSoFar()) + 1
						_ = i
						for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline) && len(w.answersSoFar()) < want; {
							time.Sleep(5 * time.Millisecond)
						}
					}
					if hold != nil {
						<-hold
					}
					send(map[string]any{"type": "result", "is_error": false, "result": "done"})
				}()
			}
		}
	}()
	return client, sandbox.Response{Version: 2}, nil
}

func (w *claudeWorker) answersSoFar() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.answers...)
}
func (w *claudeWorker) controlsSoFar() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.controls...)
}

func claudeSetup(t *testing.T) (*Engine, *claudeWorker, string) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &claudeWorker{}
	e := NewEngine(s, w)
	e.ResidentProviders = []string{} // one run per message; residency is covered elsewhere
	ctx, cancel := context.WithCancel(context.Background())
	go e.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Error("engine did not stop")
		}
		s.Close()
	})
	id, err := e.Create("Modes", "", "", nil, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	return e, w, id
}

func bashAsk(command string) map[string]any {
	return map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "toolu_" + cv.ID(), "description": "run it", "input": map[string]any{"command": command}}
}
func writeAsk(path, content string) map[string]any {
	return map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "tool_use_id": "toolu_" + cv.ID(), "input": map[string]any{"file_path": path, "content": content}}
}

func pendingApproval(t *testing.T, e *Engine, id string) Approval {
	t.Helper()
	var ap Approval
	until(t, func() bool {
		for _, a := range e.Store.Snapshot().chat(id).Approvals {
			if a.State == "pending" {
				ap = a
				return true
			}
		}
		return false
	})
	return ap
}

func idle(t *testing.T, e *Engine, id string) {
	t.Helper()
	until(t, func() bool { return e.Store.Snapshot().chat(id).Status == "idle" })
}

func notices(c *Chat) []string {
	var out []string
	for _, v := range c.Conversation.Entries {
		if v.Role == "notice" {
			out = append(out, v.Text)
		}
	}
	return out
}

func TestPermissionRules(t *testing.T) {
	cases := []struct {
		tool, command, pattern, label string
	}{
		{"Bash", "touch x", "Bash(touch *)", "`touch` commands"},
		{"Bash", "git commit -m x", "Bash(git commit *)", "`git commit` commands"},
		{"Bash", "git --no-pager log", "Bash(git *)", "`git` commands"},
		{"Bash", "FOO=1 make test", "Bash(make test *)", "`make test` commands"},
		{"Bash", "touch x && rm x", "Bash(touch x && rm x)", "`touch x && rm x` commands"},
		{"Bash", "cat a | head", "Bash(cat a | head)", "`cat a | head` commands"},
		{"Write", "", "Edit", "file edits"},
		{"NotebookEdit", "", "Edit", "file edits"},
		{"WebFetch", "", "WebFetch", "WebFetch calls"},
	}
	for _, c := range cases {
		if r := RuleFor(c.tool, map[string]any{"command": c.command}); r != c.pattern {
			t.Errorf("%s %q: %q", c.tool, c.command, r)
		} else if l := RuleLabel(r); l != c.label {
			t.Errorf("%s label %q", r, l)
		}
	}
	// A chat stored by item 3 comes back with its rules converted.
	var legacy Chat
	if err := json.Unmarshal([]byte(`{"allowed":[{"tool":"Bash","command":"git commit"},{"tool":"edit"},{"tool":"Bash","command":"touch x && rm x"},{"tool":"WebFetch"}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range legacy.Allowed {
		got = append(got, r.Kind+" "+r.Pattern+" "+r.Origin)
	}
	if strings.Join(got, ", ") != "allow Bash(git commit *) always, allow Edit always, allow Bash(touch x && rm x) always, allow WebFetch always" {
		t.Fatal(got)
	}
}

// The matcher, in Claude Code's rule syntax: command prefixes and globs
// (chained commands covered part by part for an allow, any part for a
// deny or ask), path globs against the call's path, domains, bare and
// globbed tool names.
func TestRuleMatcher(t *testing.T) {
	bash := func(c string) map[string]any { return map[string]any{"command": c} }
	file := func(p string) map[string]any { return map[string]any{"file_path": p} }
	cases := []struct {
		kind, pattern, tool string
		input               map[string]any
		want                bool
	}{
		{"allow", "Bash(git *)", "Bash", bash("git commit -m x"), true},
		{"allow", "Bash(git *)", "Bash", bash("git"), true},
		{"allow", "Bash(git *)", "Bash", bash("gitk"), false},
		{"allow", "Bash(git *)", "Bash", bash("FOO=1 git status"), true},
		{"allow", "Bash(git *)", "Bash", bash("git status && rm -rf /"), false},
		{"allow", "Bash(git *)", "Bash", bash("git status && git log"), true},
		{"allow", "Bash(git *)", "Bash", bash("git log | head"), false},
		{"allow", "Bash(git *)", "Bash", bash("git log $(cat x)"), false},
		{"allow", "Bash(go *)", "Bash", bash("go test ./... 2>&1"), true},
		{"deny", "Bash(rm *)", "Bash", bash("rm -rf x"), true},
		{"deny", "Bash(rm *)", "Bash", bash("git status && rm -rf x"), true},
		{"deny", "Bash(rm *)", "Bash", bash("echo rm"), false},
		{"deny", "Bash(rm *)", "Bash", bash("rmdir x"), false},
		{"ask", "Bash(*sudo*)", "Bash", bash("echo hi && sudo ls"), true},
		{"allow", "Bash(npm run test:*)", "Bash", bash("npm run test:unit"), true},
		{"allow", "Bash(npm run test:*)", "Bash", bash("npm run test"), true},
		{"allow", "Bash(npm run test:*)", "Bash", bash("npm run build"), false},
		{"allow", "Bash(npm test)", "Bash", bash("npm test"), true},
		{"allow", "Bash(npm test)", "Bash", bash("npm test -- x"), false},
		{"allow", "Bash(git * main)", "Bash", bash("git checkout main"), true},
		{"allow", "Bash(git * main)", "Bash", bash("git checkout dev"), false},
		{"allow", "Bash", "Bash", bash("anything"), true},
		{"allow", "Bash", "Write", file("x"), false},
		{"allow", "Edit", "Write", file("x"), true},
		{"allow", "Edit", "NotebookEdit", map[string]any{"notebook_path": "n.ipynb"}, true},
		{"allow", "Edit", "Read", file("x"), false},
		{"allow", "Edit(src/**)", "Edit", file("/home/agent/workspace/src/a/b.go"), true},
		{"allow", "Edit(src/**)", "Write", file("src/a.go"), true},
		{"allow", "Edit(src/**)", "Write", file("/home/agent/workspace/docs/a.md"), false},
		{"allow", "Edit(src/*.go)", "Edit", file("src/a.go"), true},
		{"allow", "Edit(src/*.go)", "Edit", file("src/a/b.go"), false},
		{"allow", "Edit(**/*.md)", "Edit", file("/home/agent/workspace/docs/a.md"), true},
		{"deny", "Edit(/etc/**)", "Write", file("/etc/hosts"), true},
		{"deny", "Edit(//etc/**)", "Write", file("/etc/hosts"), true},
		{"deny", "Edit(/etc/**)", "Write", file("/home/agent/etc/hosts"), false},
		{"deny", "Edit(~/.ssh/**)", "Write", file("/home/agent/.ssh/config"), true},
		{"deny", "Edit(~/.ssh/**)", "Write", file("/root/.ssh/config"), true},
		{"deny", "Edit(~/.ssh/**)", "Write", file("/home/agent/workspace/.ssh/config"), false},
		{"allow", "Read", "Grep", map[string]any{"pattern": "x"}, true},
		{"allow", "Read(docs/**)", "Read", file("docs/a.md"), true},
		{"allow", "Read(docs/**)", "Glob", map[string]any{"pattern": "*.md"}, false},
		{"allow", "WebFetch(domain:example.com)", "WebFetch", map[string]any{"url": "https://example.com/a"}, true},
		{"allow", "WebFetch(domain:example.com)", "WebFetch", map[string]any{"url": "https://docs.example.com/a"}, true},
		{"allow", "WebFetch(domain:example.com)", "WebFetch", map[string]any{"url": "https://example.com.evil.net/"}, false},
		{"allow", "WebFetch", "WebFetch", map[string]any{"url": "https://x/"}, true},
		{"allow", "mcp__warden__*", "mcp__warden__request_network_access", nil, true},
		{"allow", "mcp__warden__*", "mcp__other__tool", nil, false},
		{"allow", "mcp__warden__request_network_access", "mcp__warden__request_network_access", nil, true},
		{"allow", "*", "Bash", bash("x"), true},
	}
	for _, c := range cases {
		r, err := NewRule(c.kind, c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if got := r.Matches(c.tool, c.input); got != c.want {
			t.Errorf("%s %s vs %s %v: got %v", c.kind, c.pattern, c.tool, c.input, got)
		}
	}
	for _, bad := range []string{"", "Bash(", "Bash()", "(x)", "Bash x", "WebFetch(example.com)", "TodoWrite(x)", "Bash(a\nb)"} {
		if _, err := NewRule("allow", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := NewRule("maybe", "Bash"); err == nil {
		t.Error("kind accepted")
	}
	if r, err := NewRule(" DENY ", " Bash(rm *) "); err != nil || r.Kind != "deny" || r.Pattern != "Bash(rm *)" {
		t.Errorf("%+v %v", r, err)
	}
}

func TestPermissionModeRoute(t *testing.T) {
	e, _, id := claudeSetup(t)
	ctx := context.Background()
	if err := e.SetMode(ctx, id, "bypass"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	if err := e.SetMode(ctx, codex, ModeAsk); err == nil {
		t.Fatal("mode set on a Codex chat")
	}
	if err := e.SetMode(ctx, id, ModeAsk); err != nil {
		t.Fatal(err)
	}
	if err := e.SetMode(ctx, id, ModeAsk); err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Mode != ModeAsk || len(notices(c)) != 1 || !strings.HasPrefix(notices(c)[0], "Permission mode: ask") {
		t.Fatalf("%+v %v", c.Mode, notices(c))
	}
	if c.mode() != ModeAsk || (&Chat{}).mode() != ModeAuto {
		t.Fatal("mode()")
	}
	// The route body reaches SetMode.
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	post := func(body string) int {
		r := httptest.NewRequest("POST", h.Origin+"/api/chats/"+id+"/mode", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer private")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out.Code
	}
	if code := post(`{"mode":"plan"}`); code != 200 {
		t.Fatal(code)
	}
	if code := post(`{"mode":"nope"}`); code != 409 {
		t.Fatal(code)
	}
	if e.Store.Snapshot().chat(id).Mode != ModePlan {
		t.Fatal("route did not set the mode")
	}
}

// In auto the engine answers every ask itself; in ask it is the owner's
// card, with allow-always remembered for the chat and a denial's message
// handed to the CLI.
func TestPermissionAsksPerMode(t *testing.T) {
	e, w, id := claudeSetup(t)
	ctx := context.Background()
	w.asks = [][]map[string]any{{bashAsk("touch a")}, {bashAsk("touch b"), writeAsk("/home/agent/workspace/f", "x")}, {bashAsk("touch c"), bashAsk("rm c")}}
	// Turn 1: auto.
	sendAndDeliver(t, e, id, "one")
	idle(t, e, id)
	answers := w.answersSoFar()
	if len(answers) != 1 || agent.Map(answers[0]["response"])["behavior"] != "allow" || len(e.Store.Snapshot().chat(id).Approvals) != 0 {
		t.Fatalf("auto: %+v", answers)
	}
	// Turn 2: ask; the first ask is a card, answered allow-always.
	if err := e.SetMode(ctx, id, ModeAsk); err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, id, "two")
	ap := pendingApproval(t, e, id)
	entry, _ := ap.Params["entry"].(map[string]any)
	if ap.Method != methodPermission || ap.Params["tool"] != "Bash" || ap.Params["always"] != "`touch` commands" || ap.Params["description"] != "run it" || entry == nil || entry["text"] != "touch b" || agent.Map(entry["tool"])["kind"] != "command" {
		t.Fatalf("%+v", ap.Params)
	}
	if err := e.Answer(id, ap.ID, Answer{Always: true}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	ap = pendingApproval(t, e, id)
	entry, _ = ap.Params["entry"].(map[string]any)
	if ap.Params["tool"] != "Write" || ap.Params["always"] != "file edits" || agent.Map(entry["tool"])["kind"] != "edit" || !strings.Contains(agent.String(entry["detail"]), "+x") {
		t.Fatalf("%+v", ap.Params)
	}
	if err := e.Answer(id, ap.ID, Answer{Allow: false, Message: "not that file"}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	answers = w.answersSoFar()
	if len(answers) != 3 || agent.Map(answers[1]["response"])["behavior"] != "allow" || agent.Map(answers[2]["response"])["behavior"] != "deny" || !strings.HasSuffix(agent.String(agent.Map(answers[2]["response"])["message"]), "the user said: not that file") {
		t.Fatalf("ask: %+v", answers)
	}
	c := e.Store.Snapshot().chat(id)
	if len(c.Rules) != 1 || c.Rules[0].Pattern != "Bash(touch *)" || c.Rules[0].Kind != RuleAllow || c.Rules[0].Origin != "always" || c.Rules[0].By == nil || c.Rules[0].By.PrincipalID != "owner" || c.Rules[0].ID == "" {
		t.Fatalf("rules %+v", c.Rules)
	}
	// Turn 3: the rule answers `touch c`; `rm c` is a card.
	sendAndDeliver(t, e, id, "three")
	ap = pendingApproval(t, e, id)
	if ap.Params["tool"] != "Bash" || agent.Map(ap.Params["input"])["command"] != "rm c" {
		t.Fatalf("%+v", ap.Params)
	}
	if err := e.Answer(id, ap.ID, Answer{Allow: true}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	answers = w.answersSoFar()
	if len(answers) != 5 || agent.Map(answers[3]["response"])["behavior"] != "allow" || agent.Map(answers[4]["response"])["behavior"] != "allow" {
		t.Fatalf("rule: %+v", answers)
	}
	// Rules and mode are on the chat record.
	b, err := os.ReadFile(filepath.Join(e.Store.dir(), "chats.json"))
	if err != nil || !strings.Contains(string(b), `"kind":"allow","pattern":"Bash(touch *)","origin":"always"`) || !strings.Contains(string(b), `"mode":"ask"`) {
		t.Fatalf("%v %s", err, b)
	}
}

// A plan is always the owner's: approving it moves the chat (and, with
// the answer, the CLI) into auto or ask; feedback keeps planning.
func TestPlanApproval(t *testing.T) {
	e, w, id := claudeSetup(t)
	ctx := context.Background()
	plan := map[string]any{"subtype": "can_use_tool", "tool_name": "ExitPlanMode", "tool_use_id": "toolu_plan", "input": map[string]any{"plan": "# Plan\n\n1. Do it", "planFilePath": "/home/agent/.claude/plans/p.md"}}
	w.asks = [][]map[string]any{{plan, plan}}
	if err := e.SetMode(ctx, id, ModePlan); err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, id, "plan it")
	// The session started in the CLI's default mode; the chat's plan mode
	// was pushed before the turn.
	until(t, func() bool { return len(w.controlsSoFar()) == 1 })
	if w.controlsSoFar()[0]["mode"] != "plan" {
		t.Fatal(w.controlsSoFar())
	}
	ap := pendingApproval(t, e, id)
	if ap.Params["tool"] != "ExitPlanMode" || ap.Params["plan"] != "# Plan\n\n1. Do it" || ap.Params["always"] != nil {
		t.Fatalf("%+v", ap.Params)
	}
	if err := e.Answer(id, ap.ID, Answer{Allow: false, Message: "add tests"}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().chat(id).Mode != ModePlan {
		t.Fatal("feedback left plan mode")
	}
	ap = pendingApproval(t, e, id)
	if err := e.Answer(id, ap.ID, Answer{Allow: true, Mode: ModeAsk}, cv.Actor{PrincipalID: "owner"}); err != nil {
		t.Fatal(err)
	}
	idle(t, e, id)
	answers := w.answersSoFar()
	first, second := agent.Map(answers[0]["response"]), agent.Map(answers[1]["response"])
	updates := agent.Array(second["updatedPermissions"])
	if len(answers) != 2 || first["behavior"] != "deny" || !strings.HasSuffix(agent.String(first["message"]), "the user said: add tests") || second["behavior"] != "allow" || len(updates) != 1 || agent.Map(updates[0])["mode"] != "default" {
		t.Fatalf("%+v", answers)
	}
	c := e.Store.Snapshot().chat(id)
	if c.Mode != ModeAsk {
		t.Fatal(c.Mode)
	}
	ns := notices(c)
	if len(ns) != 2 || !strings.HasPrefix(ns[1], "Plan approved") {
		t.Fatal(ns)
	}
}

// The CLI entering plan mode by itself (the model's EnterPlanMode) is
// reported through its status frame and puts the chat in plan mode; a mode
// set while the session runs reaches it at once.
func TestModeReportedByCLIAndPushedLive(t *testing.T) {
	e, w, id := claudeSetup(t)
	ctx := context.Background()
	w.frames = []map[string]any{{"type": "system", "subtype": "status", "status": nil, "permissionMode": "plan"}}
	w.hold = make(chan struct{})
	if err := e.Message(id, "go", cv.ID()); err != nil {
		t.Fatal(err)
	}
	// The marker lands after the message, so wait for the mode itself.
	until(t, func() bool { return e.Store.Snapshot().chat(id).Mode == ModePlan })
	if ns := notices(e.Store.Snapshot().chat(id)); len(ns) != 1 || !strings.HasPrefix(ns[0], "Claude entered plan mode") {
		t.Fatal(ns)
	}
	if err := e.SetMode(ctx, id, ModeAsk); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return len(w.controlsSoFar()) == 1 })
	if w.controlsSoFar()[0]["mode"] != "default" {
		t.Fatal(w.controlsSoFar())
	}
	close(w.hold)
	idle(t, e, id)
	if e.Store.Snapshot().chat(id).Mode != ModeAsk {
		t.Fatal("mode")
	}
}
