package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// attachment-write only ever writes beneath .warden/attachments under a
// name the chat service chose, needs a running sandbox, and reaches the
// workspace through the runtime's copy plus a privileged move.
func TestAttachmentWriteIsBoundedAndPlacedByTheRuntime(t *testing.T) {
	w, d, _, r := managedFixture(t)
	r.Operation = "attachment-write"
	r.Directory = ".warden/attachments/" + strings.Repeat("a", 32) + ".png"
	r.Bytes = []byte("png bytes")
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped sandbox accepted a write: %v", err)
	}
	prepareFixture(t, w, r)
	for _, path := range []string{"", "notes.txt", "/etc/passwd", "../x.png", ".warden/attachments/../../.bashrc", ".warden/attachments/" + strings.Repeat("A", 32) + ".png", ".warden/attachments/" + strings.Repeat("a", 32), ".warden/attachments/" + strings.Repeat("a", 32) + ".tar.gz", ".warden/attachments/" + strings.Repeat("a", 31) + ".png"} {
		bad := r
		bad.Directory = path
		if _, err := w.dispatch(context.Background(), bad); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	for _, content := range [][]byte{nil, make([]byte, MaxAttachmentBytes+1)} {
		bad := r
		bad.Bytes = content
		if _, err := w.dispatch(context.Background(), bad); err == nil {
			t.Fatalf("accepted %d bytes", len(content))
		}
	}
	before := len(d.calls)
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Directory != r.Directory {
		t.Fatalf("%+v", res)
	}
	calls := d.calls[before:]
	guest := "/tmp/warden-attachment-" + strings.Repeat("a", 32) + ".png"
	if len(calls) != 2 || calls[0] != "copy:"+w.managed.Sandboxes[r.SandboxID].RuntimeName+":"+guest || !strings.HasPrefix(calls[1], "exec:") || !strings.Contains(calls[1], "sudo sh -c "+attachmentScript+" warden-attachment "+guest+" "+r.Directory) {
		t.Fatalf("%q", calls)
	}
	if entries, _ := os.ReadDir(filepath.Join(w.Root, "attachments")); len(entries) != 0 {
		t.Fatal("staged copy left on the worker host")
	}
}

// The guest-side move: refuses a symlinked attachment directory, creates
// the directory when missing, and replaces a symlinked target instead of
// writing through it. chown needs root, so the script runs with a shim.
func TestAttachmentScriptDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(staged, target string) error {
		cmd := exec.Command("sh", "-c", attachmentScript, "warden-attachment", staged, target)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		return cmd.Run()
	}
	outside := t.TempDir()
	stage := func() string {
		f := filepath.Join(t.TempDir(), "staged")
		os.WriteFile(f, []byte("content"), 0600)
		return f
	}
	// A fresh workspace: the directory is created and the file lands.
	if err := run(stage(), ".warden/attachments/one.bin"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".warden/attachments/one.bin")); string(b) != "content" {
		t.Fatal("file not placed")
	}
	// A symlinked target is replaced, and nothing lands at its referent.
	os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0600)
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, ".warden/attachments/two.bin"))
	if err := run(stage(), ".warden/attachments/two.bin"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "keep" {
		t.Fatal("wrote through a symlinked target")
	}
	if info, err := os.Lstat(filepath.Join(root, ".warden/attachments/two.bin")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("target still a symlink")
	}
	// A symlinked attachment directory or .warden is refused.
	os.RemoveAll(filepath.Join(root, ".warden/attachments"))
	os.Symlink(outside, filepath.Join(root, ".warden/attachments"))
	if err := run(stage(), ".warden/attachments/three.bin"); err == nil {
		t.Fatal("followed a symlinked attachment directory")
	}
	os.RemoveAll(filepath.Join(root, ".warden"))
	os.Symlink(outside, filepath.Join(root, ".warden"))
	if err := run(stage(), ".warden/attachments/four.bin"); err == nil {
		t.Fatal("followed a symlinked .warden")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside directory changed: %d entries", len(entries))
	}
}
