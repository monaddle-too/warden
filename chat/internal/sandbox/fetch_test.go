package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchAcrossPrivateCopyBoundary(t *testing.T) {
	root := t.TempDir()
	upstream, task := filepath.Join(root, "upstream"), filepath.Join(root, "task")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for transport fixture")
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(gitPath, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main", upstream)
	git("-C", upstream, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "base")
	git("clone", upstream, task)
	git("-C", upstream, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "new main")
	want := git("-C", upstream, "rev-parse", "HEAD")
	before := git("-C", task, "rev-parse", "HEAD")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!"+python+"\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("git", fmt.Sprintf(`import os,sys
args=[%q if a=='https://github.com/owner/repo.git' else a for a in sys.argv[1:]]
os.execv(%q, ['git']+args)
`, upstream, gitPath))
	// Model SBX's copied host ownership: the guest cannot read/remove a copied
	// private file until ownership is transferred. Run the real import script
	// and Git operations after that boundary, with no credentials in the guest.
	sbx := write("sbx", `import os,sys,shutil,subprocess,stat
assert 'GIT_CONFIG_VALUE_0' not in os.environ, 'host credential reached guest'
args=sys.argv[1:]
def local(p): return os.path.join(os.environ['FETCH_TRANSFER'],os.path.basename(p))
if args[0]=='cp':
 target=local(args[2].split(':',1)[1]); shutil.copyfile(args[1],target); os.chmod(target,0); sys.exit(0)
assert args[:3]==['exec','-i','-w']
directory=args[3]; cmd=args[5:]
if cmd[:3]==['sudo','chown','agent:agent']:
 os.chmod(local(cmd[3]),0o600); sys.exit(0)
assert cmd[:2]==['bash','-c']
target=local(cmd[4]); assert stat.S_IMODE(os.stat(target).st_mode)==0o600, 'guest cannot read copied bundle'
cmd[4]=target
sys.exit(subprocess.run(cmd,cwd=directory).returncode)
`)
	t.Setenv("FETCH_TRANSFER", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := Worker{Root: root, Executable: sbx}
	result, err := w.fetch(context.Background(), Request{SessionID: "test", Repository: "owner/repo", Token: "host-only-test-token", Args: []string{"main"}}, session{Directory: task})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, want) || git("-C", task, "rev-parse", "origin/main") != want {
		t.Fatal("latest ref was not imported")
	}
	if git("-C", task, "rev-parse", "HEAD") != before {
		t.Fatal("fetch changed HEAD")
	}
	left, err := filepath.Glob(filepath.Join(root, "workspace-fetch-*.bundle"))
	if err != nil || len(left) != 0 {
		t.Fatal("guest transfer was not removed")
	}
}

func TestFetchAndConflictResolutionPreserveWorkAndPublishedAncestry(t *testing.T) {
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream")
	task := filepath.Join(root, "task")
	os.Mkdir(upstream, 0700)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git(upstream, "init", "-b", "main")
	git(upstream, "config", "user.name", "Test")
	git(upstream, "config", "user.email", "test@example.test")
	os.WriteFile(filepath.Join(upstream, "shared.txt"), []byte("base\n"), 0600)
	git(upstream, "add", ".")
	git(upstream, "commit", "-m", "base")
	base := git(upstream, "rev-parse", "HEAD")
	git(root, "clone", upstream, task)
	git(task, "config", "user.name", "Test")
	git(task, "config", "user.email", "test@example.test")
	snapshot := func(script, mode, previous string) (Response, error) {
		cmd := exec.Command("bash", "-c", script, "snapshot-test", base, mode, previous)
		cmd.Dir = task
		out, err := cmd.Output()
		var result Response
		if err == nil {
			err = json.Unmarshal(out, &result)
		}
		return result, err
	}
	os.WriteFile(filepath.Join(task, "shared.txt"), []byte("task change\n"), 0600)
	old, err := snapshot(snapshotScript, "export", "")
	if err != nil {
		t.Fatal(err)
	}
	git(task, "add", ".")
	git(task, "commit", "-m", "task checkpoint")
	os.WriteFile(filepath.Join(task, "scratch.txt"), []byte("staged user work\n"), 0600)
	git(task, "add", "scratch.txt")
	os.WriteFile(filepath.Join(task, "untracked.txt"), []byte("preserve me\n"), 0600)
	before, index := git(task, "status", "--porcelain"), git(task, "write-tree")
	os.WriteFile(filepath.Join(upstream, "shared.txt"), []byte("main change\n"), 0600)
	os.WriteFile(filepath.Join(upstream, "upstream.txt"), []byte("only upstream\n"), 0600)
	git(upstream, "add", ".")
	git(upstream, "commit", "-m", "advance main")
	main := git(upstream, "rev-parse", "HEAD")
	git(upstream, "update-ref", "refs/workspace/main", main)
	git(upstream, "fetch", task, "refs/workspace/export:refs/workspace/pr")
	bundle := filepath.Join(root, "transfer.bundle")
	git(upstream, "bundle", "create", bundle, "refs/workspace/main", "refs/workspace/pr")
	cmd := exec.Command("bash", "-c", fetchImportScript, "fetch-test", bundle, "main")
	cmd.Dir = task
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch: %s %v", out, err)
	}
	if before != git(task, "status", "--porcelain") || index != git(task, "write-tree") {
		t.Fatal("fetch modified working files/index")
	}
	if git(task, "rev-parse", "origin/main") != main || git(task, "rev-parse", "refs/remotes/workspace/pr") != old.Head {
		t.Fatal("wrong imported refs")
	}
	// A remote edit not yet incorporated in HEAD cannot be silently parented
	// into a snapshot whose tree would discard that edit.
	remoteCheck := exec.Command("bash", "-c", snapshotV2Script, "snapshot-test", base, "snapshot", old.Head, main)
	remoteCheck.Dir = task
	if err := remoteCheck.Run(); err == nil {
		t.Fatal("export accepted unseen remote changes")
	}
	git(task, "add", ".")
	git(task, "commit", "-m", "preserve local work")
	cmd = exec.Command("git", "-C", task, "merge", "refs/remotes/workspace/main")
	if cmd.Run() == nil {
		t.Fatal("fixture must conflict")
	}
	if _, err = snapshot(snapshotV2Script, "snapshot", old.Head); err == nil {
		t.Fatal("published unresolved conflict")
	}
	os.WriteFile(filepath.Join(task, "shared.txt"), []byte("main change\ntask change\n"), 0600)
	git(task, "add", "shared.txt")
	git(task, "commit", "-m", "resolve merge")
	head := git(task, "rev-parse", "HEAD")
	before, index = git(task, "status", "--porcelain"), git(task, "write-tree")
	preview, err := snapshot(snapshotV2Script, "snapshot", old.Head)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := snapshot(snapshotV2Script, "export", old.Head)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Head != exported.Head || len(exported.Bundle) == 0 {
		t.Fatal("review/export identity mismatch")
	}
	git(task, "merge-base", "--is-ancestor", main, exported.Head)
	git(task, "merge-base", "--is-ancestor", old.Head, exported.Head)
	if before != git(task, "status", "--porcelain") || index != git(task, "write-tree") || head != git(task, "rev-parse", "HEAD") {
		t.Fatal("export modified task checkout")
	}
	stable, err := snapshot(snapshotV2Script, "snapshot", exported.Head)
	if err != nil || stable.Head != exported.Head {
		t.Fatal("unchanged published tree generated another update", stable.Head, err)
	}
	if strings.Contains(preview.Diff, "only upstream") {
		t.Fatal("review diff includes unchanged upstream additions")
	}
}
