package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runExecScript(t *testing.T, root, cmd string, timeout int, cap int) ExecResult {
	t.Helper()
	raw, err := exec.Command("python3", "-c", execScript, root, cmd, strconv.Itoa(timeout), strconv.Itoa(cap)).Output()
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	var r ExecResult
	if json.Unmarshal(raw, &r) != nil {
		t.Fatalf("%q: invalid reply %s", cmd, raw)
	}
	return r
}

// The guest script runs the line with bash in the workspace, merges
// stderr into the output, reports the exit code, keeps a bounded tail of
// a flood, kills a command that overruns its timeout, and does not wait
// for a background process the command left behind.
func TestExecScriptRunsInTheWorkspace(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0644)
	r := runExecScript(t, root, "ls; echo err >&2; cat hello.txt; exit 3", 10, 30000)
	if r.Output != "hello.txt\nerr\nhi" || r.ExitCode != 3 || r.TimedOut {
		t.Fatalf("%+v", r)
	}
	// bash, not sh: the person's own shell habits work.
	r = runExecScript(t, root, "[ -n \"$BASH_VERSION\" ] && echo bash-ok", 10, 30000)
	if r.Output != "bash-ok\n" || r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	// A flood keeps only its tail.
	r = runExecScript(t, root, "seq 1 20000", 10, 1000)
	if r.ExitCode != 0 || !strings.HasPrefix(r.Output, "[output cut: only the last 1000 characters are kept]\n") || !strings.HasSuffix(r.Output, "20000\n") || len(r.Output) > 1100 {
		t.Fatalf("%d %q…%q", len(r.Output), r.Output[:60], r.Output[len(r.Output)-20:])
	}
	// The timeout kills the command and its children.
	start := time.Now()
	r = runExecScript(t, root, "echo before; sleep 30; echo after", 1, 30000)
	if !r.TimedOut || r.ExitCode != -1 || r.Output != "before\n" || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v after %s", r, time.Since(start))
	}
	// A background process holding the pipe does not hold the answer.
	start = time.Now()
	r = runExecScript(t, root, "sleep 20 & echo started", 10, 30000)
	if r.TimedOut || r.ExitCode != 0 || r.Output != "started\n" || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", r, time.Since(start))
	}
}

func runMemoryScript(root, note string) error {
	return exec.Command("python3", "-c", memoryScript, root, note).Run()
}

// The guest script creates CLAUDE.md at the workspace root or appends to
// it on a fresh line, and never writes through a symlink in its place.
func TestMemoryScriptAppendsABullet(t *testing.T) {
	root := t.TempDir()
	if err := runMemoryScript(root, MemoryBullet("use tabs")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md")); string(b) != "- use tabs\n" {
		t.Fatalf("%q", b)
	}
	os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Project\n\nSome text without a final newline"), 0644)
	if err := runMemoryScript(root, MemoryBullet("run the tests\nwith -race")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md")); string(b) != "# Project\n\nSome text without a final newline\n- run the tests\n  with -race\n" {
		t.Fatalf("%q", b)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "victim.md"), []byte("keep\n"), 0644)
	os.Remove(filepath.Join(root, "CLAUDE.md"))
	os.Symlink(filepath.Join(outside, "victim.md"), filepath.Join(root, "CLAUDE.md"))
	if err := runMemoryScript(root, "- x"); err == nil {
		t.Fatal("wrote through a symlinked CLAUDE.md")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim.md")); string(b) != "keep\n" {
		t.Fatalf("victim changed: %q", b)
	}
}

func TestMemoryBullet(t *testing.T) {
	if got := MemoryBullet("  use tabs  "); got != "- use tabs" {
		t.Fatalf("%q", got)
	}
	if got := MemoryBullet("first\nsecond  \n\nthird"); got != "- first\n  second\n  \n  third" {
		t.Fatalf("%q", got)
	}
}

// The exec and memory-append operations need a running, admitted
// sandbox, validate their text, reach the guest through the runtime with
// the text as arguments, and count as activity in the workspace.
func TestExecAndMemoryOpsRunThroughTheRuntime(t *testing.T) {
	w, d, _, r := managedFixture(t)
	r.Operation = "exec"
	r.Command = "ls -la"
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped sandbox ran a command: %v", err)
	}
	prepareFixture(t, w, r)
	for _, cmd := range []string{"", "   ", strings.Repeat("x", MaxExecCommand+1), "ls\x00-la"} {
		bad := r
		bad.Command = cmd
		if _, err := w.dispatch(context.Background(), bad); err == nil {
			t.Fatalf("accepted %q", cmd)
		}
	}
	d.mu.Lock()
	d.execOutput = `{"output":"total 0\n","exitCode":0}`
	before := len(d.calls)
	d.mu.Unlock()
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].LastActivity = time.Time{}
	w.mu.Unlock()
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Exec == nil || res.Exec.Output != "total 0\n" || res.Exec.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	d.mu.Lock()
	call := d.calls[before]
	d.mu.Unlock()
	name := w.managed.Sandboxes[r.SandboxID].RuntimeName
	if !strings.HasPrefix(call, "exec:"+name+":python3 -c "+execScript+" /home/agent/workspace ls -la 60 30000") {
		t.Fatalf("%q", call)
	}
	if w.managed.Sandboxes[r.SandboxID].LastActivity.IsZero() {
		t.Fatal("a person's command did not count as activity")
	}
	// A guest failure is reported without the runtime's detail.
	d.mu.Lock()
	d.execHook = func([]string) error { return context.DeadlineExceeded }
	d.mu.Unlock()
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "could not run the command") {
		t.Fatalf("failure: %v", err)
	}
	d.mu.Lock()
	d.execHook = nil
	d.execOutput = ""
	before = len(d.calls)
	d.mu.Unlock()

	r.Operation = "memory-append"
	r.Command = ""
	for _, note := range []string{"", "  \n", strings.Repeat("x", MaxMemoryNote+1), "a\x00b"} {
		bad := r
		bad.Bytes = []byte(note)
		if _, err := w.dispatch(context.Background(), bad); err == nil {
			t.Fatalf("accepted %q", note)
		}
	}
	r.Bytes = []byte("- use tabs")
	res, err = w.dispatch(context.Background(), r)
	if err != nil || res.Directory != "CLAUDE.md" {
		t.Fatalf("%+v %v", res, err)
	}
	d.mu.Lock()
	call = d.calls[before]
	d.mu.Unlock()
	if call != "exec:"+name+":python3 -c "+memoryScript+" /home/agent/workspace - use tabs" {
		t.Fatalf("%q", call)
	}
}
