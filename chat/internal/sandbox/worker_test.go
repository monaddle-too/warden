package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
)

func TestSnapshotPreservesIndexAndBindsAllChanges(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s: %v", b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "delete-me"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	os.Remove(filepath.Join(root, "delete-me"))
	os.WriteFile(filepath.Join(root, "binary"), []byte{0, 1, 2, 255}, 0600)
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("review me\n"), 0600)
	os.Symlink("new.txt", filepath.Join(root, "link"))
	before := git("status", "--porcelain=v1")
	index := git("write-tree")
	snapshot := func(mode string) Response {
		t.Helper()
		cmd := exec.Command("bash", "-c", snapshotScript, "test", base, mode)
		cmd.Dir = root
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("snapshot: %s: %v", stderr.String(), err)
		}
		var s Response
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	preview := snapshot("snapshot")
	export := snapshot("export")
	if preview.Head != export.Head || preview.Base != base || len(export.Bundle) == 0 {
		t.Fatal("preview/export identity differs")
	}
	if before != git("status", "--porcelain=v1") || index != git("write-tree") || base != git("rev-parse", "HEAD") {
		t.Fatal("snapshot changed user's checkout or index")
	}
	if !strings.Contains(preview.Diff, "GIT binary patch") || !strings.Contains(preview.Diff, "deleted file") || !strings.Contains(preview.Diff, "120000") {
		t.Fatal("snapshot omitted changes")
	}
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("later edit\n"), 0600)
	if snapshot("snapshot").Head == preview.Head {
		t.Fatal("changed code kept reviewed identity")
	}
}
func TestWorkerRejectsCrossProjectSession(t *testing.T) {
	root := t.TempDir()
	w := NewWorker(root, "/not-called", "template")
	if err := atomicJSON(filepath.Join(root, "sessions", "session-one.json"), session{ProjectID: "project-one", Directory: "/workspace", Base: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	_, _, err := w.acquire(context.Background(), Request{ProjectID: "project-two", SessionID: "session-one", Operation: "prepare"})
	if err == nil || !strings.Contains(err.Error(), "another project") {
		t.Fatalf("cross-project request accepted: %v", err)
	}
}
func TestGitCredentialIsScopedAndNotPersistent(t *testing.T) {
	env := strings.Join(gitEnvironment("secret-token"), "\n")
	if !strings.Contains(env, "http.https://github.com/.extraheader") || strings.Contains(env, "secret-token") {
		t.Fatal("credential transport changed")
	}
	if !strings.Contains(env, "GIT_CONFIG_GLOBAL=/dev/null") || !strings.Contains(env, "HOME=/nonexistent") {
		t.Fatal("Git inherits host credential configuration")
	}
}

// Observe admission at the instant a response starts writing, before the client
// can issue its next request. This deterministically catches the response race.
type admissionConn struct {
	net.Conn
	worker *Worker
	t      *testing.T
}

func (c admissionConn) Write(p []byte) (int, error) {
	c.worker.mu.Lock()
	users := c.worker.users
	c.worker.mu.Unlock()
	if users != 0 {
		c.t.Error("completed operation still holds sandbox admission")
	}
	return c.Conn.Write(p)
}
func TestResponseReleasesAdmissionBeforeNextRequest(t *testing.T) {
	w := NewWorker(t.TempDir(), "/not-called", "template")
	if err := atomicJSON(filepath.Join(w.Root, "sessions", "session-one.json"), session{ProjectID: "project-one", Directory: "/workspace", Base: strings.Repeat("a", 40), RuntimeVersion: "0.154.0"}); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.handleLegacy(ctx, admissionConn{a, w, t}) }()
	if err := json.NewEncoder(b).Encode(Request{Version: 1, Operation: "prepare", ProjectID: "project-one", SessionID: "session-one"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(b).Decode(&response); err != nil || response.Error != "" {
		t.Fatal(response, err)
	}
	<-done
}
func TestNextRunWaitsForPreviousStreamCleanup(t *testing.T) {
	w := NewWorker(t.TempDir(), "/not-called", "template")
	if err := atomicJSON(filepath.Join(w.Root, "sessions", "session-one.json"), session{ProjectID: "project-one", Directory: "/workspace", Base: strings.Repeat("a", 40), RuntimeVersion: "0.154.0"}); err != nil {
		t.Fatal(err)
	}
	_, release, err := w.acquire(context.Background(), Request{Operation: "stream", ProjectID: "project-one", SessionID: "session-one"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, done, err := w.acquire(context.Background(), Request{Operation: "stream", ProjectID: "project-one", SessionID: "session-one"})
		if done != nil {
			done()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("next stream bypassed active stream: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("next stream did not wake after cleanup")
	}
}

func TestIndependentSessionsRunTogetherAndEvictOnlyIdleVMs(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "sbx")
	log := filepath.Join(root, "stops")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+log+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(root, script, "test")
	for _, id := range []string{"session-one", "session-two", "session-three"} {
		if err := atomicJSON(filepath.Join(root, "sessions", id+".json"), session{ProjectID: "project-one", Directory: "/workspace", Base: strings.Repeat("a", 40), RuntimeVersion: "0.154.0"}); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(id string) (func(), error) {
		_, release, err := w.acquire(context.Background(), Request{ProjectID: "project-one", SessionID: id, Operation: "stream"})
		return release, err
	}
	one, err := acquire("session-one")
	if err != nil {
		t.Fatal(err)
	}
	defer one()
	two, err := acquire("session-two")
	if err != nil {
		t.Fatal("independent stream blocked", err)
	}
	defer two()
	if w.activeSessions.Load() != 2 {
		t.Fatal("two sessions not admitted")
	}
	if _, err := acquire("session-three"); err == nil {
		t.Fatal("exceeded host capacity")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("active VM stopped to admit another task")
	}
	// Admission release is idempotent at the protocol boundary.
	one()
	one = func() {}
	three, err := acquire("session-three")
	if err != nil {
		t.Fatal(err)
	}
	defer three()
	data, _ := os.ReadFile(log)
	if string(data) != "stop ws-session-one\n" {
		t.Fatalf("stopped wrong VM: %s", data)
	}
	if w.sessions["session-two"].users != 1 || w.activeSessions.Load() != 2 {
		t.Fatal("unrelated stream disrupted")
	}
}

// The startup handshake asks a running worker for its identity with the
// "health" operation; the answer must carry the release protocol number and
// the revision the worker was built with.
func TestWorkerAnswersTheVersionHandshake(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wsw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	w := NewWorker(dir, "/not-called", "template")
	w.Revision = "built-here"
	socket := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Serve(ctx, l) }()
	defer func() { cancel(); l.Close(); <-done }()
	peer, err := handshake.Runner(ctx, "unix://"+socket, nil)
	if err != nil || peer.Protocol != release.Protocol || peer.Revision != "built-here" || peer.Name != "warden-runner" {
		t.Fatalf("%+v %v", peer, err)
	}
	if _, err = handshake.Compare(handshake.Peer{Name: "warden-chat", Revision: "built-here", Protocol: release.Protocol}, peer); err != nil {
		t.Fatal(err)
	}
}
