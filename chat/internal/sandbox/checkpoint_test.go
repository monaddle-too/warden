package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// localCheckpointRuntime runs the checkpoint scripts on the host against a
// directory standing in for the guest workspace, as the review tests do.
type localCheckpointRuntime struct {
	*testRuntime
	directory string
}

func (r *localCheckpointRuntime) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	if len(args) > 3 && args[0] == "python3" && strings.HasPrefix(args[2], checkpointCommon) {
		for i, a := range args {
			if a == "" {
				// The SBX exec API refuses an empty argument.
				return "", fmt.Errorf("cmd element %d is empty", i)
			}
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = r.directory
		cmd.Env = publicGitEnvironment()
		out, err := cmd.Output()
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				return "", fmt.Errorf("script failed: %s", e.Stderr)
			}
			return "", err
		}
		return string(out), nil
	}
	return r.testRuntime.Exec(ctx, name, dir, args...)
}

func checkpointFixture(t *testing.T, repository bool) (*Worker, Request, string, func(...string) string) {
	t.Helper()
	w, driver, _, req := managedFixture(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(publicGitEnvironment(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	if repository {
		git("init", "-q")
	}
	s := w.managed.Sandboxes[req.SandboxID]
	s.State = "running"
	s.Created = true
	s.Directory = dir
	w.Runtime = &localCheckpointRuntime{testRuntime: driver, directory: dir}
	return w, req, dir, git
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

const (
	checkpointOne   = "11111111111111111111111111111111"
	checkpointTwo   = "22222222222222222222222222222222"
	checkpointThree = "33333333333333333333333333333333"
)

func checkpointCall(t *testing.T, w *Worker, req Request, op, id string) (Response, error) {
	t.Helper()
	req.Operation = op
	req.CallID = id
	return w.dispatch(context.Background(), req)
}

func TestCheckpointRepositoryStoreSnapshotsRestoresAndDiffs(t *testing.T) {
	w, req, dir, git := checkpointFixture(t, true)
	write(t, dir, "a.txt", "one\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "initial")
	write(t, dir, "b.txt", "bee\n")
	write(t, dir, ".gitignore", "ignored.txt\n")
	write(t, dir, "ignored.txt", "ignored\n")
	write(t, dir, ".warden/attachments/x.png", "png")
	head := git("rev-parse", "HEAD")
	index, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	status := git("status", "--porcelain")

	res, err := checkpointCall(t, w, req, "checkpoint", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	cp := res.Checkpoint
	if cp == nil || cp.ID != checkpointOne || cp.ChatID != req.ChatID || cp.Store != "repository" || !cp.Changed || !commit.MatchString(cp.Commit) || !commit.MatchString(cp.Tree) {
		t.Fatalf("bad checkpoint: %+v", cp)
	}
	if got := git("rev-parse", "refs/warden/checkpoints/"+checkpointOne); got != cp.Commit {
		t.Fatalf("ref names %s, record %s", got, cp.Commit)
	}
	// The agent's index, HEAD and status are as they were; the snapshot
	// holds the untracked file, not the ignored one nor Warden's directory.
	after, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if string(after) != string(index) || git("rev-parse", "HEAD") != head || git("status", "--porcelain") != status {
		t.Fatal("the checkpoint changed the agent's repository state")
	}
	if tree := git("ls-tree", "-r", "--name-only", cp.Tree); tree != "a.txt\nb.txt" && tree != ".gitignore\na.txt\nb.txt" {
		t.Fatalf("snapshot tree: %q", tree)
	}
	// Nothing changed: the next checkpoint reuses the commit.
	res, err = checkpointCall(t, w, req, "checkpoint", checkpointTwo)
	if err != nil || res.Checkpoint.Changed || res.Checkpoint.Commit != cp.Commit {
		t.Fatalf("unchanged workspace: %+v %v", res.Checkpoint, err)
	}
	if got := git("rev-parse", "refs/warden/checkpoints/"+checkpointTwo); got != cp.Commit {
		t.Fatal("alias ref not written")
	}
	// Edits: a change, a deletion, additions in a new directory.
	write(t, dir, "a.txt", "two\n")
	os.Remove(filepath.Join(dir, "b.txt"))
	write(t, dir, "c.txt", "sea\n")
	write(t, dir, "sub/d.txt", "dee\n")
	write(t, dir, "ignored.txt", "still ignored\n")
	res, err = checkpointCall(t, w, req, "checkpoint", checkpointThree)
	if err != nil || !res.Checkpoint.Changed || res.Checkpoint.Commit == cp.Commit {
		t.Fatalf("changed workspace: %+v %v", res.Checkpoint, err)
	}
	if list := w.managed.Sandboxes[req.SandboxID].Checkpoints; len(list) != 3 || list[2].ID != checkpointThree {
		t.Fatalf("records: %+v", list)
	}
	res, err = checkpointCall(t, w, req, "checkpoints", "")
	if err != nil || len(res.Checkpoints) != 3 {
		t.Fatalf("checkpoints op: %+v %v", res.Checkpoints, err)
	}

	// The session diff against the first checkpoint.
	res, err = checkpointCall(t, w, req, "diff", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	changes := res.Changes
	if changes == nil || changes.Base != checkpointOne || changes.Truncated {
		t.Fatalf("bad diff: %+v", changes)
	}
	want := []ReviewFile{{Path: "a.txt", Added: 1, Removed: 1}, {Path: "b.txt", Removed: 1}, {Path: "c.txt", Added: 1}, {Path: "sub/d.txt", Added: 1}}
	if !reflect.DeepEqual(changes.Files, want) {
		t.Fatalf("files: %+v", changes.Files)
	}
	for _, s := range []string{"diff --git a/a.txt b/a.txt", "-one", "+two", "deleted file mode", "new file mode", "+dee"} {
		if !strings.Contains(changes.Diff, s) {
			t.Fatalf("diff lacks %q:\n%s", s, changes.Diff)
		}
	}
	if strings.Contains(changes.Diff, "ignored") {
		t.Fatal("ignored file in the diff")
	}

	// Restore the first checkpoint: only the differing paths move.
	res, err = checkpointCall(t, w, req, "restore", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	restore := res.Restore
	if restore == nil || restore.Checkpoint.ID != checkpointOne || !reflect.DeepEqual(restore.Restored, []string{"a.txt", "b.txt"}) || !reflect.DeepEqual(restore.Removed, []string{"c.txt", "sub/d.txt"}) {
		t.Fatalf("restore: %+v", restore)
	}
	if read(t, dir, "a.txt") != "one\n" || read(t, dir, "b.txt") != "bee\n" || read(t, dir, "ignored.txt") != "still ignored\n" || read(t, dir, ".warden/attachments/x.png") != "png" {
		t.Fatal("restore wrote the wrong files")
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt survived the restore")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatal("the emptied directory survived the restore")
	}
	if git("rev-parse", "HEAD") != head {
		t.Fatal("restore moved HEAD")
	}
	res, err = checkpointCall(t, w, req, "diff", checkpointOne)
	if err != nil || len(res.Changes.Files) != 0 || res.Changes.Diff != "" {
		t.Fatalf("diff after restore: %+v %v", res.Changes, err)
	}

	// A ref the agent moved is not restored from.
	git("update-ref", "refs/warden/checkpoints/"+checkpointOne, head)
	if _, err = checkpointCall(t, w, req, "restore", checkpointOne); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatalf("altered ref: %v", err)
	}
	if _, err = checkpointCall(t, w, req, "restore", "44444444444444444444444444444444"); err == nil || !strings.Contains(err.Error(), "no checkpoint") {
		t.Fatalf("unknown checkpoint: %v", err)
	}
	if _, err = checkpointCall(t, w, req, "checkpoint", "not-an-id"); err == nil {
		t.Fatal("accepted a bad checkpoint ID")
	}
}

func TestCheckpointPrivateStoreForNonRepositoryWorkspace(t *testing.T) {
	w, req, dir, _ := checkpointFixture(t, false)
	write(t, dir, "notes.md", "first\n")
	write(t, dir, "src/app.py", "print(1)\n")
	write(t, dir, "node_modules/left-pad/index.js", "module.exports = 1\n")
	res, err := checkpointCall(t, w, req, "checkpoint", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	cp := res.Checkpoint
	if cp.Store != "private" || !cp.Changed {
		t.Fatalf("bad checkpoint: %+v", cp)
	}
	private := filepath.Join(filepath.Dir(dir), ".warden-checkpoints.git")
	if _, err := os.Stat(filepath.Join(private, "refs", "warden", "checkpoints", checkpointOne)); err != nil {
		t.Fatalf("private store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("the workspace gained a .git")
	}
	write(t, dir, "notes.md", "second\n")
	write(t, dir, "src/extra.py", "print(2)\n")
	write(t, dir, "node_modules/left-pad/index.js", "module.exports = 2\n")
	res, err = checkpointCall(t, w, req, "diff", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewFile{{Path: "notes.md", Added: 1, Removed: 1}, {Path: "src/extra.py", Added: 1}}
	if !reflect.DeepEqual(res.Changes.Files, want) {
		t.Fatalf("files: %+v", res.Changes.Files)
	}
	res, err = checkpointCall(t, w, req, "restore", checkpointOne)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Restore.Restored, []string{"notes.md"}) || !reflect.DeepEqual(res.Restore.Removed, []string{"src/extra.py"}) {
		t.Fatalf("restore: %+v", res.Restore)
	}
	if read(t, dir, "notes.md") != "first\n" || read(t, dir, "src/app.py") != "print(1)\n" || read(t, dir, "node_modules/left-pad/index.js") != "module.exports = 2\n" {
		t.Fatal("restore wrote the wrong files (dependencies are outside the snapshot)")
	}
	// Over the cap the snapshot is refused, with the reason.
	f, err := os.Create(filepath.Join(dir, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(300 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = checkpointCall(t, w, req, "checkpoint", checkpointTwo); err == nil || !strings.Contains(err.Error(), "over 256 MiB") {
		t.Fatalf("cap: %v", err)
	}
}

func TestCheckpointNeedsARunningSandbox(t *testing.T) {
	w, req, _, _ := checkpointFixture(t, false)
	w.managed.Sandboxes[req.SandboxID].State = "stopped"
	if _, err := checkpointCall(t, w, req, "checkpoint", checkpointOne); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped sandbox: %v", err)
	}
	if res, err := checkpointCall(t, w, req, "checkpoints", ""); err != nil || len(res.Checkpoints) != 0 {
		t.Fatalf("listing a stopped sandbox: %v %v", res.Checkpoints, err)
	}
}

func TestCheckpointRecordsAreBounded(t *testing.T) {
	w, req, _, _ := checkpointFixture(t, false)
	s := w.managed.Sandboxes[req.SandboxID]
	for i := 0; i < maxCheckpoints+7; i++ {
		w.recordCheckpoint(s, Checkpoint{ID: fmt.Sprintf("%032d", i)})
	}
	if len(s.Checkpoints) != maxCheckpoints || s.Checkpoints[0].ID != fmt.Sprintf("%032d", 7) {
		t.Fatalf("records: %d, first %s", len(s.Checkpoints), s.Checkpoints[0].ID)
	}
	w.recordCheckpoint(s, Checkpoint{ID: fmt.Sprintf("%032d", 9), Commit: "again"})
	if len(s.Checkpoints) != maxCheckpoints || s.Checkpoints[maxCheckpoints-1].Commit != "again" {
		t.Fatal("a repeated ID must replace its record")
	}
}
