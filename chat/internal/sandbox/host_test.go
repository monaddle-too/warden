package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeShell is a login shell for the tests: it records that it was
// called as `shell -lc CMD` and runs CMD with /bin/sh, so the owner's real
// profile never colours the output.
func fakeShell(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shell")
	script := "#!/bin/sh\n[ \"$1\" = -lc ] || { echo not-a-login-shell; exit 97; }\nexec /bin/sh -c \"$2\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A host command runs through the login shell in the given directory with
// its output merged, reports its exit code, is cut to the tail when it
// prints too much, is killed with its process group on the timeout, and
// is killed and reported cancelled when the request's context ends.
func TestRunHostCommand(t *testing.T) {
	shell := fakeShell(t)
	tracker := &hostExecs{byRun: map[string]map[*hostExec]struct{}{}}
	dir := realDir(t)
	res, err := runHostCommand(context.Background(), tracker, "chat:run", shell, "pwd; echo home=$HOME; echo out; echo err 1>&2; exit 3", dir, dir, time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.TimedOut || res.Output != dir+"\nhome="+dir+"\nout\nerr\n" || res.Bytes != int64(2*len(dir)+15) || res.DurationMS < 0 {
		t.Fatalf("%+v", res)
	}
	// Too much output: the tail, with a note.
	res, err = runHostCommand(context.Background(), tracker, "chat:run", shell, "i=0; while [ $i -lt 500 ]; do echo line-$i; i=$((i+1)); done", dir, dir, time.Minute, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Output, "[output cut: only the last 200 characters are kept]\n") || !strings.HasSuffix(res.Output, "line-499\n") || len(res.Output) > 260 || res.Bytes < 4000 {
		t.Fatalf("%q bytes=%d", res.Output, res.Bytes)
	}
	// The timeout kills the whole group, a grandchild included.
	pidFile := filepath.Join(dir, "pid")
	start := time.Now()
	res, err = runHostCommand(context.Background(), tracker, "chat:run", shell, "sleep 30 & echo $! > "+pidFile+"; wait", dir, dir, 300*time.Millisecond, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v after %v", res, time.Since(start))
	}
	expectDead(t, pidFile)
	// A cancelled context kills it too and the caller learns it was
	// cancelled rather than reading a result; the tracker forgets it.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err = runHostCommand(ctx, tracker, "chat:run", shell, "sleep 30 & echo $! > "+pidFile+"; wait", dir, dir, time.Minute, 1000); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancel: %v", err)
	}
	expectDead(t, pidFile)
	if len(tracker.byRun) != 0 {
		t.Fatalf("tracker kept %v", tracker.byRun)
	}
	// A shell that is not a login shell is reported through its output.
	res, err = runHostCommand(context.Background(), tracker, "chat:run", shell, "true", dir, dir, time.Minute, 1000)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

// realDir is a temporary directory with its symlinks resolved (macOS's
// /var is /private/var), as pwd prints it.
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// expectDead waits for the process whose pid the file holds to be gone.
func expectDead(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		// A zombie of the killed group still answers kill -0 until
		// reaped by init; it is dead either way.
		if state, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && strings.Contains(string(state), " Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("process %d survived the kill", pid)
	}
}

// The host operations are refused unless the runner runs with the
// jailbreak flag, whatever the chat service asks.
func TestHostOpsNeedTheJailbreakFlag(t *testing.T) {
	w, _, _, r := managedFixture(t)
	for _, op := range []string{"host.exec", "host.put", "host.get", "host.expose", "host.status"} {
		req := r
		req.Operation, req.Command, req.Path, req.Directory, req.Port = op, "true", "/x", "/y", 80
		if _, err := w.dispatch(context.Background(), req); err == nil || !strings.Contains(err.Error(), "host access is off") {
			t.Fatalf("%s: %v", op, err)
		}
	}
	if got := w.operationTimeout(Request{Operation: "host.exec"}); got != HostExecDefaultTimeout+30*time.Second {
		t.Fatalf("default exec timeout %v", got)
	}
	if got := w.operationTimeout(Request{Operation: "host.exec", Timeout: 7200}); got != HostExecMaxTimeout+30*time.Second {
		t.Fatalf("capped exec timeout %v", got)
	}
	if got := w.operationTimeout(Request{Operation: "host.exec", Timeout: 5}); got != 35*time.Second {
		t.Fatalf("exec timeout %v", got)
	}
}

// host.exec runs on the host for a registered chat, validates its command
// and cwd, defaults the cwd to the home directory, and is killed when the
// chat's run is cancelled.
func TestHostExecOp(t *testing.T) {
	w, _, _, r := managedFixture(t)
	w.Jailbreak = true
	home := realDir(t)
	w.HostHome = home
	t.Setenv("SHELL", fakeShell(t))
	r.Operation = "host.exec"
	for _, bad := range []Request{{Command: ""}, {Command: strings.Repeat("x", MaxHostCommand+1)}, {Command: "a\x00b"}, {Command: "true", Directory: "relative"}, {Command: "true", Directory: filepath.Join(home, "missing")}} {
		req := r
		req.Command, req.Directory = bad.Command, bad.Directory
		if _, err := w.dispatch(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	other := r
	other.ChatID = "someone-else"
	other.Command = "true"
	if _, err := w.dispatch(context.Background(), other); err == nil {
		t.Fatal("an unregistered chat ran a host command")
	}
	r.Command = "pwd; uname"
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Exec == nil || res.Exec.ExitCode != 0 || !strings.HasPrefix(res.Exec.Output, home+"\n") {
		t.Fatalf("%+v", res.Exec)
	}
	// Cancelling the chat's run kills a command in flight.
	pidFile := filepath.Join(home, "pid")
	r.Command = "sleep 30 & echo $! > " + pidFile + "; wait"
	done := make(chan error, 1)
	go func() {
		_, err := w.dispatch(context.Background(), r)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := w.cancelHostExecs(r.ChatID); n != 1 {
		t.Fatalf("cancelled %d", n)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not end the command")
	}
	expectDead(t, pidFile)
}

// Host paths for a copy stay under the home directory and outside this
// Warden's own state; the copies go through the runtime with the size
// checked first and refuse what is too large or missing.
func TestHostCopies(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Jailbreak = true
	home := t.TempDir()
	w.HostHome = home
	w.Root = filepath.Join(home, ".warden", "runner")
	for _, bad := range []string{"relative/x", "/etc/passwd", filepath.Join(home, ".warden", "policy", "x"), filepath.Join(home, ".warden"), filepath.Dir(home)} {
		if _, err := w.hostPath(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if p, err := w.hostPath(filepath.Join(home, ".warden-dev", "x")); err != nil || p != filepath.Join(home, ".warden-dev", "x") {
		t.Fatalf("%s %v", p, err)
	}
	for _, bad := range []string{"", "relative", "/", "/x\x00y"} {
		if _, err := hostCopyGuestPath(bad); err == nil {
			t.Fatalf("accepted guest path %q", bad)
		}
	}
	r.Operation = "host.put"
	r.Directory, r.Path = "/home/agent/workspace/out.txt", filepath.Join(home, "out.txt")
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped sandbox copied: %v", err)
	}
	prepareFixture(t, w, r)
	d.mu.Lock()
	d.execOutput = "missing"
	d.mu.Unlock()
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing guest path: %v", err)
	}
	d.mu.Lock()
	d.execOutput = strconv.Itoa(HostFileLimit + 1)
	d.mu.Unlock()
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("oversize guest path: %v", err)
	}
	d.mu.Lock()
	d.execOutput = "12"
	before := len(d.calls)
	d.mu.Unlock()
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Size != 12 || res.Directory != r.Directory || res.Output != r.Path {
		t.Fatalf("%+v %v", res, err)
	}
	name := w.managed.Sandboxes[r.SandboxID].RuntimeName
	d.mu.Lock()
	calls := strings.Join(d.calls[before:], "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "copyout:"+name+":/home/agent/workspace/out.txt:"+r.Path) {
		t.Fatalf("calls: %s", calls)
	}
	// host.get: the host side is measured, the guest directory made and
	// the copy handed to the runtime.
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Operation = "host.get"
	r.Path, r.Directory = src, "/home/agent/workspace/src"
	d.mu.Lock()
	before = len(d.calls)
	d.mu.Unlock()
	res, err = w.dispatch(context.Background(), r)
	if err != nil || res.Size != 5 {
		t.Fatalf("%+v %v", res, err)
	}
	d.mu.Lock()
	calls = strings.Join(d.calls[before:], "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "sudo mkdir -p /home/agent/workspace") || !strings.Contains(calls, "copy:"+name+":/home/agent/workspace/src") || !strings.Contains(calls, "chown -R agent:agent /home/agent/workspace/src") {
		t.Fatalf("calls: %s", calls)
	}
	r.Path = filepath.Join(home, "nope")
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing host path: %v", err)
	}
	if _, err := hostTreeSize(src); err != nil {
		t.Fatal(err)
	}
}

// host.expose publishes a host loopback port under the runner's own
// availability listener: the attachment is marked as the host's, its URL
// is the loopback one the chat admits, requests reach the host service,
// and removing the attachment retires the publication without touching
// the guest.
func TestHostExpose(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Jailbreak = true
	w.HostHome = t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		io.WriteString(rw, "host says "+req.URL.Path)
	}))
	defer upstream.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(upstream.URL, "http://127.0.0.1:"))
	r.Operation = "host.expose"
	r.Port, r.Path, r.Title, r.CallID = port, "/", "Dev Warden", "call-host"
	if _, err := w.dispatch(context.Background(), r); err == nil {
		t.Fatal("exposed without a run")
	}
	prepareFixture(t, w, r)
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active.Streaming = true
	w.mu.Unlock()
	d.mu.Lock()
	before := len(d.calls)
	d.mu.Unlock()
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Attachment
	if a == nil || a.Upstream != "host" || a.Port != port || a.State != "available" || !strings.HasPrefix(a.URL, "http://127.0.0.1:") || a.ChatID != r.ChatID {
		t.Fatalf("%+v", a)
	}
	d.mu.Lock()
	calls := strings.Join(d.calls[before:], "\n")
	d.mu.Unlock()
	if strings.Contains(calls, "publish") {
		t.Fatalf("the guest was touched: %s", calls)
	}
	resp, err := http.Get(a.URL + "hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "host says /hello" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	// The status lists it; a sandbox preview on the same port number is a
	// different publication.
	status, err := w.dispatch(context.Background(), Request{Version: 2, Operation: "status", ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, RunID: r.RunID, PrincipalID: r.PrincipalID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range status.Attachments {
		if x.ID == a.ID && x.Upstream == "host" {
			found = true
		}
	}
	if !found {
		t.Fatalf("status attachments: %+v", status.Attachments)
	}
	if _, ok := w.managed.Publications[hostPubKey(r.SandboxID, port)]; !ok {
		t.Fatal("no host publication")
	}
	if _, ok := w.managed.Publications[pubKey(r.SandboxID, port)]; ok {
		t.Fatal("a sandbox publication was recorded for the host port")
	}
	// Removal retires it: the listener answers 410 and the guest is not
	// asked to unpublish anything.
	d.mu.Lock()
	before = len(d.calls)
	d.mu.Unlock()
	remove := r
	remove.Operation, remove.AttachmentID = "preview.remove", a.ID
	if _, err = w.dispatch(context.Background(), remove); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	calls = strings.Join(d.calls[before:], "\n")
	d.mu.Unlock()
	if strings.Contains(calls, "unpublish") || strings.Contains(calls, "mappings") {
		t.Fatalf("the guest was touched on removal: %s", calls)
	}
	resp, err = http.Get(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("after removal: %d", resp.StatusCode)
	}
}

// host.status lists the Warden instances under the home directory, marks
// this runner's own and reads each one's release link.
func TestHostStatus(t *testing.T) {
	w, _, _, r := managedFixture(t)
	w.Jailbreak = true
	home := t.TempDir()
	w.HostHome = home
	w.Root = filepath.Join(home, ".warden", "runner")
	for _, name := range []string{".warden", ".warden-dev", ".warden-empty", "other"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".warden", ".warden-dev"} {
		if err := os.WriteFile(filepath.Join(home, name, "warden.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".warden-dev", "releases", "v0.1.0-alpha.13"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".warden-dev", "releases", "v0.1.0-alpha.13"), filepath.Join(home, ".warden-dev", "release")); err != nil {
		t.Fatal(err)
	}
	r.Operation = "host.status"
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	h := res.Host
	if h == nil || h.Home != home || h.StateDir != filepath.Join(home, ".warden") || h.OS == "" || len(h.Instances) != 2 {
		t.Fatalf("%+v", h)
	}
	if h.Instances[0].Name != "default" || !h.Instances[0].This || h.Instances[0].Release != "" || h.Instances[1].Name != "dev" || h.Instances[1].This || h.Instances[1].Release != "v0.1.0-alpha.13" {
		t.Fatalf("%+v", h.Instances)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("unreachable")
	}
}

// A host command sees the owner's home, not the SBX namespace the runner
// itself is pointed at (the first dogfood run resolved `~/.warden` under
// the namespace).
func TestHostEnvRestoresTheOwnersHome(t *testing.T) {
	env := hostEnv([]string{"PATH=/bin", "HOME=/Users/o/.warden/sbx/home", "XDG_DATA_HOME=/Users/o/.warden/sbx/data", "SHELL=/bin/zsh"}, "/Users/o")
	want := []string{"PATH=/bin", "SHELL=/bin/zsh", "HOME=/Users/o"}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Fatalf("hostEnv = %q, want %q", env, want)
	}
}
