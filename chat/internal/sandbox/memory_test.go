package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMemoryPath(t *testing.T) {
	for _, ok := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".claude/CLAUDE.md", ".claude/rules/style.md", ".claude/rules/go/errors.md", ".claude/rules/a/b/c.md"} {
		if err := ValidateMemoryPath(MemoryScopeWorkspace, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, ok := range []string{"MEMORY.md", "notes/go-toolchain.md", "a/b/c.md"} {
		if err := ValidateMemoryPath(MemoryScopeAuto, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "README.md", "claude.md", "../CLAUDE.md", "/CLAUDE.md", ".claude/rules", ".claude/rules/", ".claude/rules/../x.md", ".claude/rules/.hidden.md", ".claude/rules/x.txt", ".claude/rules/a/b/c/d.md", ".claude/settings.json", "src/CLAUDE.md", "CLAUDE.md\n", "a\\b.md"} {
		if ValidateMemoryPath(MemoryScopeWorkspace, bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"", "../MEMORY.md", ".hidden.md", "a/b/c/d.md", "MEMORY.txt", "/MEMORY.md"} {
		if ValidateMemoryPath(MemoryScopeAuto, bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ValidateMemoryPath("other", "CLAUDE.md") == nil {
		t.Error("unknown scope accepted")
	}
}

func runMemoryList(t *testing.T, root, home, reported string) MemoryListing {
	t.Helper()
	raw, err := exec.Command("python3", "-c", memoryListScript, root, home, reported).Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			t.Fatalf("%v: %s", err, e.Stderr)
		}
		t.Fatal(err)
	}
	var l MemoryListing
	if err = json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("invalid reply %s", raw)
	}
	return l
}

// The guest listing finds the fixed workspace files, the rules three
// levels down, and the auto-memory directory the CLI names or the one
// derived from the workspace path; it never follows a symlink, marks a
// file cut at the text cap, and leaves other files alone.
func TestMemoryListScript(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "CLAUDE.md"), "# Project\n")
	write(filepath.Join(root, "AGENTS.md"), "agents")
	write(filepath.Join(root, ".claude", "CLAUDE.md"), "nested")
	write(filepath.Join(root, ".claude", "rules", "style.md"), "style")
	write(filepath.Join(root, ".claude", "rules", "go", "errors.md"), "errors")
	write(filepath.Join(root, ".claude", "rules", "go", "deep", "deeper.md"), "deeper")
	write(filepath.Join(root, ".claude", "rules", "go", "deep", "toodeep", "x.md"), "too deep")
	write(filepath.Join(root, ".claude", "rules", "notes.txt"), "not markdown")
	write(filepath.Join(root, ".claude", "rules", ".hidden.md"), "hidden")
	write(filepath.Join(root, "README.md"), "readme")
	write(filepath.Join(root, "big.md"), "x")
	os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "CLAUDE.local.md"))
	big := strings.Repeat("y", MemoryTextCap+10)
	write(filepath.Join(root, ".claude", "rules", "big.md"), big)
	derived := filepath.Join(home, ".claude", "projects", memoryProjectName(root), "memory")
	write(filepath.Join(derived, "MEMORY.md"), "index")
	write(filepath.Join(derived, "topics", "go.md"), "go notes")

	l := runMemoryList(t, root, home, "")
	got := map[string]MemoryFile{}
	for _, f := range l.Files {
		got[f.Scope+":"+f.Path] = f
	}
	for key, text := range map[string]string{
		"workspace:CLAUDE.md":                       "# Project\n",
		"workspace:AGENTS.md":                       "agents",
		"workspace:.claude/CLAUDE.md":               "nested",
		"workspace:.claude/rules/style.md":          "style",
		"workspace:.claude/rules/go/errors.md":      "errors",
		"workspace:.claude/rules/go/deep/deeper.md": "deeper",
		"auto:MEMORY.md":                            "index",
		"auto:topics/go.md":                         "go notes",
	} {
		if f, ok := got[key]; !ok || f.Text != text {
			t.Errorf("%s: %+v", key, f)
		}
	}
	for _, absent := range []string{"workspace:CLAUDE.local.md", "workspace:.claude/rules/notes.txt", "workspace:.claude/rules/.hidden.md", "workspace:README.md", "workspace:big.md", "workspace:.claude/rules/go/deep/toodeep/x.md"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s listed", absent)
		}
	}
	if f := got["workspace:.claude/rules/big.md"]; !f.Truncated || len(f.Text) != MemoryTextCap || f.Size != int64(len(big)) {
		t.Errorf("big rule: truncated=%v len=%d size=%d", f.Truncated, len(f.Text), f.Size)
	}
	if l.AutoDir != derived || !l.Exists {
		t.Errorf("auto dir %q exists=%v, want %q", l.AutoDir, l.Exists, derived)
	}
	// The CLI's own report wins when it is a memory directory under the
	// home's projects; anything else is ignored.
	reported := filepath.Join(home, ".claude", "projects", "-other-name", "memory")
	l = runMemoryList(t, root, home, reported)
	if l.AutoDir != reported || l.Exists {
		t.Errorf("reported %q → %q exists=%v", reported, l.AutoDir, l.Exists)
	}
	for _, bad := range []string{"/etc", filepath.Join(home, ".claude", "projects", "x", "memory", "deeper"), filepath.Join(home, ".ssh"), "relative/memory"} {
		if l = runMemoryList(t, root, home, bad); l.AutoDir != derived {
			t.Errorf("reported %q → %q", bad, l.AutoDir)
		}
	}
	// An empty workspace lists nothing and no auto-memory directory.
	l = runMemoryList(t, t.TempDir(), t.TempDir(), "")
	if len(l.Files) != 0 || l.Exists {
		t.Errorf("%+v", l)
	}
}

func runMemoryWrite(t *testing.T, root, path, text string) error {
	t.Helper()
	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", "-c", memoryWriteScript, root, path, staged).CombinedOutput()
	if err != nil {
		return &scriptError{err: err, out: string(out)}
	}
	if _, statErr := os.Stat(staged); statErr == nil {
		t.Fatal("staged file left behind")
	}
	return nil
}

type scriptError struct {
	err error
	out string
}

func (e *scriptError) Error() string { return e.err.Error() + ": " + e.out }

// The guest write creates the file and its folders, replaces an existing
// file's contents, and refuses to write through a symlink at the target
// or on the way to it.
func TestMemoryWriteScript(t *testing.T) {
	root := t.TempDir()
	if err := runMemoryWrite(t, root, "CLAUDE.md", "# One\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md")); string(b) != "# One\n" {
		t.Fatalf("%q", b)
	}
	if err := runMemoryWrite(t, root, "CLAUDE.md", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md")); string(b) != "" {
		t.Fatalf("not replaced: %q", b)
	}
	if err := runMemoryWrite(t, root, ".claude/rules/go/errors.md", "wrap errors"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".claude", "rules", "go", "errors.md")); string(b) != "wrap errors" {
		t.Fatalf("%q", b)
	}
	// The auto-memory tree relative to a home.
	home := t.TempDir()
	if err := runMemoryWrite(t, home, ".claude/projects/-w/memory/MEMORY.md", "index"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "-w", "memory", "MEMORY.md")); string(b) != "index" {
		t.Fatalf("%q", b)
	}
	// Symlinks: at the target, and at a directory on the way.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "target.md"), []byte("keep"), 0644)
	os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "AGENTS.md"))
	if err := runMemoryWrite(t, root, "AGENTS.md", "pwned"); err == nil {
		t.Fatal("wrote through a symlinked file")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "target.md")); string(b) != "keep" {
		t.Fatalf("symlink target changed: %q", b)
	}
	os.Symlink(outside, filepath.Join(root, ".claude", "rules", "link"))
	if err := runMemoryWrite(t, root, ".claude/rules/link/x.md", "pwned"); err == nil {
		t.Fatal("wrote through a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.md")); err == nil {
		t.Fatal("file landed outside")
	}
	if err := runMemoryWrite(t, root, "../escape.md", "x"); err == nil {
		t.Fatal("escaped the root")
	}
}

// The memory operations need a running sandbox, validate scope and path
// on this side, list through one guest round-trip with the reported
// auto-memory directory, and write by staging a copy and placing it with
// the script relative to the workspace or the agent home.
func TestMemoryOpsRunThroughTheRuntime(t *testing.T) {
	w, d, _, r := managedFixture(t)
	r.Operation = "memory-list"
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped sandbox listed memory: %v", err)
	}
	prepareFixture(t, w, r)
	d.mu.Lock()
	d.execOutput = `{"files":[{"scope":"workspace","path":"CLAUDE.md","size":4,"text":"# hi"},{"scope":"workspace","path":"../etc/passwd","size":1,"text":"x"},{"scope":"auto","path":"MEMORY.md","size":3,"text":"abc"}],"autoDir":"/home/agent/.claude/projects/-home-agent-workspace/memory","autoDirExists":true}`
	before := len(d.calls)
	d.mu.Unlock()
	r.Path = "/home/agent/.claude/projects/-home-agent-workspace/memory"
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Memory == nil || len(res.Memory.Files) != 2 || res.Memory.Files[0].Path != "CLAUDE.md" || res.Memory.Files[1].Scope != "auto" || res.Memory.Root != "/home/agent/workspace" || !res.Memory.Exists {
		t.Fatalf("%+v", res.Memory)
	}
	d.mu.Lock()
	call := d.calls[before]
	d.mu.Unlock()
	name := w.managed.Sandboxes[r.SandboxID].RuntimeName
	if call != "exec:"+name+":python3 -c "+memoryListScript+" /home/agent/workspace /home/agent "+r.Path {
		t.Fatalf("%q", call)
	}
	d.mu.Lock()
	d.execOutput = "not json"
	d.mu.Unlock()
	if _, err = w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "invalid sandbox memory response") {
		t.Fatalf("garbage accepted: %v", err)
	}
	d.mu.Lock()
	d.execOutput = ""
	d.mu.Unlock()

	r.Operation = "memory-write"
	r.Path = ""
	for _, bad := range []struct{ scope, path string }{{"workspace", "README.md"}, {"workspace", "../CLAUDE.md"}, {"auto", ".hidden.md"}, {"", "CLAUDE.md"}, {"home", "x.md"}} {
		b := r
		b.Scope, b.Directory, b.Bytes = bad.scope, bad.path, []byte("x")
		if _, err = w.dispatch(context.Background(), b); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	r.Scope, r.Directory = "workspace", "CLAUDE.md"
	for _, bad := range [][]byte{[]byte(strings.Repeat("x", MaxMemoryFile+1)), []byte("a\x00b"), {0xff, 0xfe}} {
		b := r
		b.Bytes = bad
		if _, err = w.dispatch(context.Background(), b); err == nil {
			t.Fatalf("accepted %d bytes", len(bad))
		}
	}
	d.mu.Lock()
	before = len(d.calls)
	d.mu.Unlock()
	r.Bytes = []byte("# Project\n")
	if res, err = w.dispatch(context.Background(), r); err != nil || res.Directory != "CLAUDE.md" {
		t.Fatalf("%+v %v", res, err)
	}
	d.mu.Lock()
	calls := append([]string(nil), d.calls[before:]...)
	d.mu.Unlock()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "copy:"+name+":/tmp/warden-memory-") {
		t.Fatalf("%q", calls)
	}
	guest := strings.TrimPrefix(calls[0], "copy:"+name+":")
	if calls[1] != "exec:"+name+":python3 -c "+memoryWriteScript+" /home/agent/workspace CLAUDE.md "+guest {
		t.Fatalf("%q", calls[1])
	}
	if entries, _ := os.ReadDir(filepath.Join(w.Root, "attachments")); len(entries) != 0 {
		t.Fatal("staged file left on the worker host")
	}
	// An auto-memory write is placed relative to the home, under the
	// reported memory directory when the chat knows it.
	r.Scope, r.Directory, r.Path = "auto", "MEMORY.md", "/home/agent/.claude/projects/-reported/memory"
	d.mu.Lock()
	before = len(d.calls)
	d.mu.Unlock()
	if _, err = w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	call = d.calls[before+1]
	d.mu.Unlock()
	if !strings.Contains(call, " /home/agent .claude/projects/-reported/memory/MEMORY.md /tmp/warden-memory-") {
		t.Fatalf("%q", call)
	}
	r.Path = ""
	d.mu.Lock()
	before = len(d.calls)
	d.mu.Unlock()
	if _, err = w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	call = d.calls[before+1]
	d.mu.Unlock()
	if !strings.Contains(call, " /home/agent .claude/projects/-home-agent-workspace/memory/MEMORY.md /tmp/warden-memory-") {
		t.Fatalf("%q", call)
	}
	// A guest failure is reported without the runtime's detail.
	d.mu.Lock()
	d.execHook = func([]string) error { return context.DeadlineExceeded }
	d.mu.Unlock()
	if _, err = w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "could not write MEMORY.md") {
		t.Fatalf("failure: %v", err)
	}
}

// The stream request's instructions reach the launch: Warden's own prompt
// first, the participants' blocks after a blank line, nothing when empty,
// and an over-long text cut at the cap.
func TestClaudeSystemPromptCarriesInstructions(t *testing.T) {
	run := RunSpec{Broker: BrokerConfig{Provider: "claude"}}
	args := AgentCommand(run, LaunchOptions{})
	prompt := ""
	for i, a := range args {
		if a == "--append-system-prompt" {
			prompt = args[i+1]
		}
	}
	if prompt != WardenSystemPrompt {
		t.Fatalf("plain launch prompt: %q", prompt)
	}
	run.Instructions = "From Ada:\nAnswer in haiku form."
	if got := claudeSystemPrompt(run); got != WardenSystemPrompt+"\n\nFrom Ada:\nAnswer in haiku form." {
		t.Fatalf("%q", got)
	}
	run.Instructions = strings.Repeat("x", MaxInstructions+100)
	got := claudeSystemPrompt(run)
	if !strings.HasSuffix(got, "\n[instructions cut at the size limit]") || len(got) > len(WardenSystemPrompt)+MaxInstructions+100 {
		t.Fatalf("not cut: %d", len(got))
	}
	// The worker passes the stream request's instructions to the driver.
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	w.mu.Unlock()
	stream, err := w.launchLocked(context.Background(), s, BrokerConfig{Provider: "codex"}, "From Ada:\nbe brief")
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	d.mu.Lock()
	last := d.runs[len(d.runs)-1]
	d.mu.Unlock()
	if last.Instructions != "From Ada:\nbe brief" {
		t.Fatalf("%q", last.Instructions)
	}
}
