package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The launch flags a chat's session settings add: --resume for a recorded
// session, --fork-session for a forked chat's first run, the output style
// as the CLI's settings key (JSON-encoded, so the name is data).
func TestClaudeSessionArgs(t *testing.T) {
	run := RunSpec{Broker: BrokerConfig{Provider: "claude", APIKeyPlaceholder: "k", ProviderBaseURL: "http://p/anthropic", ProxyURL: "http://p"}}
	plain := strings.Join(AgentCommand(run, LaunchOptions{}), "\n")
	if strings.Contains(plain, "--resume") || strings.Contains(plain, "--settings") || strings.Contains(plain, "--fork-session") {
		t.Fatalf("a fresh launch carries session flags: %s", plain)
	}
	run.Broker.ThreadID, run.Broker.ForkSession, run.Broker.OutputStyle = "abc-123", true, "Explanatory"
	args := AgentCommand(run, LaunchOptions{})
	joined := strings.Join(args, "\n")
	for _, want := range []string{"\n--resume\nabc-123\n--fork-session\n", "\n--settings\n{\"outputStyle\":\"Explanatory\"}"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	run.Broker.ForkSession = false
	if joined := strings.Join(AgentCommand(run, LaunchOptions{}), "\n"); strings.Contains(joined, "--fork-session") || !strings.Contains(joined, "\n--resume\nabc-123\n") {
		t.Fatalf("a plain resume: %s", joined)
	}
	// Without a session to resume there is nothing to fork.
	run.Broker.ThreadID, run.Broker.ForkSession = "", true
	if joined := strings.Join(AgentCommand(run, LaunchOptions{}), "\n"); strings.Contains(joined, "--fork-session") || strings.Contains(joined, "--resume") {
		t.Fatalf("fork without a session: %s", joined)
	}
}

func TestValidOutputStyle(t *testing.T) {
	for _, ok := range []string{"", "Explanatory", "Learning"} {
		if !ValidOutputStyle(ok) {
			t.Fatalf("%q refused", ok)
		}
	}
	for _, bad := range []string{"explanatory", "NoSuchStyle", "a\"b", " Learning", strings.Repeat("x", 80)} {
		if ValidOutputStyle(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// The side question's launch: the brokered environment, the guest script
// feeding the question on stdin, the CLI resuming the session as a copy
// with no tools (added by the script: the SBX exec API refuses an empty
// argument) in the chat's model and style, and no MCP or stream input.
func TestAsideCommand(t *testing.T) {
	run := RunSpec{Directory: "/home/agent/workspace", Broker: BrokerConfig{Provider: "claude", APIKeyPlaceholder: "k", ProviderBaseURL: "http://p/anthropic", ProxyURL: "http://p", ThreadID: "sess-1", ForkSession: true, Model: "claude-opus-5", OutputStyle: "Learning"}}
	args := AsideCommand(run, "what did we decide?")
	joined := strings.Join(args, "\n")
	if args[0] != "env" || !strings.Contains(joined, "\nCLAUDE_CODE_OAUTH_TOKEN=k\n") || !strings.Contains(joined, "\nANTHROPIC_BASE_URL=http://p/anthropic\n") {
		t.Fatalf("environment: %s", joined)
	}
	i := strings.Index(joined, "\npython3\n-c\n")
	if i < 0 {
		t.Fatalf("no guest script: %s", joined)
	}
	rest := args[len(claudeEnvironment(run.Broker)):]
	if rest[0] != "python3" || rest[1] != "-c" || rest[2] != asideScript || rest[3] != "what did we decide?" || rest[4] != "180" || rest[6] != defaultClaudePath || rest[7] != "-p" {
		t.Fatalf("script arguments: %q", rest[:8])
	}
	for _, want := range []string{"\n--output-format\nstream-json\n", "\n--resume\nsess-1\n--fork-session\n", "\n--model\nclaude-opus-5\n", "\n--settings\n{\"outputStyle\":\"Learning\"}", "\n--strict-mcp-config\n", "\n--setting-sources=\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--input-format", "--mcp-config", "--permission-prompt-tool", "--include-partial-messages"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("%s in a one-shot launch: %s", bad, joined)
		}
	}
	for _, a := range args {
		if a == "" {
			t.Fatal("an empty argument would be refused by the SBX exec API")
		}
	}
	if !strings.Contains(asideScript, "['--tools','']") {
		t.Fatal("the script must disable the tools")
	}
}

// The guest script feeds the question on stdin, keeps the CLI's stdout
// and the tail of its stderr, reports the exit code, and kills a one-shot
// that overruns its timeout.
func TestAsideScriptRunsTheOneShot(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\nq=$(cat)\necho \"arg:$*\" >&2\nprintf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s2\"}\\n'\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"you asked: %s\",\"total_cost_usd\":0.02,\"duration_ms\":1200,\"session_id\":\"s2\",\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":10,\"output_tokens\":7}}\\n' \"$q\"\nexit 0\n"), 0755)
	raw, err := exec.Command("python3", "-c", asideScript, "hello there", "10", strconv.Itoa(asideOutputLimit), fake, "-p", "--resume", "s1").Output()
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Output, Stderr string
		ExitCode       int
		TimedOut       bool
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if report.ExitCode != 0 || report.TimedOut || !strings.Contains(report.Stderr, "arg:-p --resume s1 --tools ") {
		t.Fatalf("%+v", report)
	}
	result := ParseAsideOutput(report.Output)
	if result.Text != "you asked: hello there" || result.CostUSD != 0.02 || result.Input != 112 || result.Output != 7 || result.DurationMS != 1200 || result.SessionID != "s2" || result.Error != "" {
		t.Fatalf("%+v", result)
	}
	// A one-shot that hangs is killed at the timeout.
	os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nsleep 30\n"), 0755)
	raw, err = exec.Command("python3", "-c", asideScript, "q", "1", "1000", fake).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil || !report.TimedOut || report.ExitCode != -1 {
		t.Fatalf("%s: %v", raw, err)
	}
}

// The parser: an error result is the error; a result without text falls
// back to the assistant frames; nothing readable is nothing.
func TestParseAsideOutput(t *testing.T) {
	r := ParseAsideOutput(`{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"second"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.5}`)
	if r.Text != "first\n\nsecond" || r.CostUSD != 0.5 || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	r = ParseAsideOutput(`not json
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 503"}`)
	if r.Error != "API Error: 503" || r.Text != "" {
		t.Fatalf("%+v", r)
	}
	if r := ParseAsideOutput(""); r.Text != "" || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if got := asideFailure(1, "Warning: no stdin data received in 3s, proceeding without it.\nError: Invalid session"); got != "Error: Invalid session" {
		t.Fatalf("%q", got)
	}
	if got := asideFailure(2, "Warning: no stdin data received in 3s"); got != "Claude exited with code 2" {
		t.Fatalf("%q", got)
	}
	if got := asideFailure(0, ""); got != "Claude gave no answer" {
		t.Fatalf("%q", got)
	}
}

// The aside operation needs the chat's active, streaming Claude run (it
// reuses that run's brokered environment), validates the question and
// the session, reaches the guest through the runtime with the question
// as an argument, and returns what the one-shot answered.
func TestAsideOpRunsThroughTheRuntime(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	r.Operation = "aside"
	r.Command = "what now?"
	r.ThreadID = "sess-1"
	r.Model = "claude-opus-5"
	r.OutputStyle = "Learning"
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("aside without a streaming run: %v", err)
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	s.Active.Streaming = true
	s.Active.broker = BrokerConfig{Provider: "claude", APIKeyPlaceholder: "k", ProviderBaseURL: "http://p/anthropic", ProxyURL: "http://p", ThreadID: "sess-1"}
	w.mu.Unlock()
	for _, bad := range []struct{ command, thread string }{{"", "sess-1"}, {strings.Repeat("x", MaxAsideQuestion+1), "sess-1"}, {"a\x00b", "sess-1"}, {"ok", ""}, {"ok", "not valid!"}} {
		req := r
		req.Command, req.ThreadID = bad.command, bad.thread
		if _, err := w.dispatch(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	d.mu.Lock()
	d.execOutput = `{"output":"{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"forty-two\",\"total_cost_usd\":0.03,\"session_id\":\"sess-2\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}\n","stderr":"","exitCode":0}`
	before := len(d.calls)
	d.mu.Unlock()
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Aside == nil || res.Aside.Text != "forty-two" || res.Aside.CostUSD != 0.03 || res.Aside.SessionID != "sess-2" || res.Aside.Input != 5 || res.Aside.Error != "" {
		t.Fatalf("%+v", res.Aside)
	}
	d.mu.Lock()
	call := d.calls[before]
	d.mu.Unlock()
	name := w.managed.Sandboxes[r.SandboxID].RuntimeName
	for _, want := range []string{"exec:" + name + ":env -u ANTHROPIC_API_KEY", " CLAUDE_CODE_OAUTH_TOKEN=k ", " python3 -c " + asideScript + " what now? 180 ", " --resume sess-1 --fork-session --settings {\"outputStyle\":\"Learning\"}", " --model claude-opus-5 "} {
		if !strings.Contains(call, want) {
			t.Fatalf("missing %q in %q", want, call)
		}
	}
	// A one-shot that answered nothing reports why.
	d.mu.Lock()
	d.execOutput = `{"output":"","stderr":"Error: No conversation found with session ID: sess-1","exitCode":1}`
	d.mu.Unlock()
	res, err = w.dispatch(context.Background(), r)
	if err != nil || res.Aside == nil || res.Aside.Error != "Error: No conversation found with session ID: sess-1" {
		t.Fatalf("%+v %v", res.Aside, err)
	}
	// A Codex run has no session to copy.
	w.mu.Lock()
	s.Active.broker.Provider = "codex"
	w.mu.Unlock()
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("aside on a Codex run: %v", err)
	}
}

// A forked chat's first run: prepare with ForkSession records no thread
// (the source's is the request's), the stream resumes the source's
// session as a copy with the chat's output style, and the run keeps its
// broker for side questions; the next prepare records the session the
// agent reported.
func TestForkSessionPrepareAndStream(t *testing.T) {
	w, d, _, r := managedFixture(t)
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("release one"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.ClaudePath = path
	r.Provider, r.Model = "claude", ""
	fork := r
	fork.Operation = "prepare"
	fork.ThreadID = "source-session"
	fork.ForkSession = true
	fork.OutputStyle = "Explanatory"
	if _, err := w.dispatch(context.Background(), fork); err != nil {
		t.Fatal(err)
	}
	if c := w.managed.Chats[r.ChatID]; c.ThreadID != "" {
		t.Fatalf("a fork's prepare recorded the source's thread %q", c.ThreadID)
	}
	stream := fork
	_, done := openManagedTestStream(t, w, stream)
	run := d.lastRun()
	if run.Broker.ThreadID != "source-session" || !run.Broker.ForkSession || run.Broker.OutputStyle != "Explanatory" {
		t.Fatalf("stream broker: %+v", run.Broker)
	}
	w.mu.Lock()
	kept := w.managed.Sandboxes[r.SandboxID].Active.broker
	w.mu.Unlock()
	if kept.ThreadID != "source-session" || kept.Provider != "claude" {
		t.Fatalf("run broker: %+v", kept)
	}
	cancel := r
	cancel.Operation = "cancel"
	_, _ = w.dispatch(context.Background(), cancel)
	waitManagedStream(t, done)
	// The session the agent reported is recorded by the next prepare as
	// the chat's own.
	next := r
	next.RunID = "run-two"
	next.Operation = "bind-chat"
	if _, err := w.dispatch(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	next.Operation = "prepare"
	next.ThreadID = "forked-session"
	if _, err := w.dispatch(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if c := w.managed.Chats[r.ChatID]; c.ThreadID != "forked-session" {
		t.Fatalf("thread after the fork: %q", c.ThreadID)
	}
	// A stream refuses a style the CLI would not know, and a fork of a
	// malformed session id.
	bad := next
	bad.Operation = "stream"
	bad.OutputStyle = "NoSuchStyle"
	a, b := net.Pipe()
	go func() { defer a.Close(); w.handle(context.Background(), a) }()
	_ = json.NewEncoder(b).Encode(bad)
	var res Response
	if err := json.NewDecoder(b).Decode(&res); err != nil || !strings.Contains(res.Error, "output style") {
		t.Fatalf("%+v %v", res, err)
	}
	b.Close()
}
