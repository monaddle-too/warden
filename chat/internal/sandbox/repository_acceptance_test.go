package sandbox

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

type loseRepositoryImportResponse struct {
	RuntimeDriver
	lost bool
}

func (d *loseRepositoryImportResponse) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	out, err := d.RuntimeDriver.Exec(ctx, name, dir, args...)
	if err == nil && !d.lost && len(args) > 2 && args[2] == repositoryImportScript {
		d.lost = true
		return "", errors.New("synthetic lost response after successful import")
	}
	return out, err
}

// Opt in with a newly created, disposable, network-denied shell sandbox with no
// host workspace mount. This tests public fetching and the real Linux import;
// it does not start an agent or claim Warden/model/cloud end-to-end acceptance.
func TestPublicRepositorySBXAcceptance(t *testing.T) {
	name := os.Getenv("WORKSPACE_CHECKOUT_SANDBOX")
	if name == "" {
		t.Skip("opt-in SBX public repository acceptance")
	}
	if !strings.HasPrefix(name, "panta-checkout-") || !validIdentity(name) {
		t.Fatal("use a disposable panta-checkout-* sandbox")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	w := NewWorker(t.TempDir(), "/opt/homebrew/bin/sbx", "docker/sandbox-templates:shell-docker")
	w.defaultsLocked()
	real := &sbxRuntime{worker: w}
	w.Runtime = &loseRepositoryImportResponse{RuntimeDriver: real}
	root := "/tmp/panta-checkout-acceptance-" + randomID()
	if _, err := real.Exec(ctx, name, "/tmp", "mkdir", "-m", "700", root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = real.Exec(cleanup, name, "/tmp", "rm", "-rf", "--", root)
	}()
	if _, err := real.Exec(ctx, name, root, "python3", "-c", "open('existing.txt','w').write('preserve conversation files')"); err != nil {
		t.Fatal(err)
	}
	s := &managedSandbox{SandboxInfo: SandboxInfo{ID: "acceptance-sandbox", ProjectID: "acceptance-project", RuntimeName: name, Directory: root, State: "running"}, Repository: "github://Canton-Network/cf-docs"}
	w.managed.Sandboxes[s.ID] = s
	if err := w.prepareRepositoryLocked(ctx, s); err == nil || !strings.Contains(err.Error(), "synthetic lost response after successful import") {
		t.Fatal("expected the synthetic lost response after import", err)
	}
	if !commit.MatchString(s.Base) || s.RepositoryReady {
		t.Fatal("lost response lost the pinned pending checkout")
	}
	base := s.Base
	if _, err := real.Exec(ctx, name, s.RepositoryCheckout, "python3", "-c", "open('panta-acceptance-note.txt','w').write('preserve local work')"); err != nil {
		t.Fatal(err)
	}
	if _, err := real.Exec(ctx, name, s.RepositoryCheckout, "git", "add", "panta-acceptance-note.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := real.Exec(ctx, name, s.RepositoryCheckout, "git", "commit", "-m", "Disposable local acceptance change"); err != nil {
		t.Fatal(err)
	}
	head, err := real.Exec(ctx, name, s.RepositoryCheckout, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) == base {
		t.Fatal("local test commit was not created", err)
	}
	if err = w.prepareRepositoryLocked(ctx, s); err != nil {
		t.Fatal("retry did not reuse completed import", err)
	}
	if err = w.prepareRepositoryLocked(ctx, s); err != nil {
		t.Fatal("resume failed", err)
	}
	actual, err := real.Exec(ctx, name, s.Directory, "git", "rev-parse", "HEAD")
	if err != nil || actual != head || s.Base != base {
		t.Fatal("retry/resume reset local commits", err)
	}
	if _, err = real.Exec(ctx, name, s.Directory, "python3", "-c", "import pathlib; assert pathlib.Path('panta-acceptance-note.txt').read_text()=='preserve local work'; assert pathlib.Path('../existing.txt').read_text()=='preserve conversation files'"); err != nil {
		t.Fatal(err)
	}
	origin, err := real.Exec(ctx, name, s.Directory, "git", "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != "https://github.com/Canton-Network/cf-docs.git" {
		t.Fatal(origin, err)
	}
	// A pre-existing destination must remain untouched even on an import retry.
	if _, err = real.Exec(ctx, name, root, "python3", "-c", repositoryImportScript, "/absent.bundle", root, strings.TrimSpace(origin), base); err == nil {
		t.Fatal("overwrote existing conversation directory")
	}
	if _, err = real.Exec(ctx, name, root, "python3", "-c", "import pathlib; assert pathlib.Path('existing.txt').read_text()=='preserve conversation files'"); err != nil {
		t.Fatal(err)
	}

	// Exercise the actual Linux export and trusted-host verification on the same
	// disposable public checkout. The gate is a fixture; no provider is started.
	w.Gate = &testGate{}
	s.PrincipalID = "acceptance-owner"
	chat := &chatBinding{ID: "acceptance-chat", ProjectID: s.ProjectID, SandboxID: s.ID}
	w.managed.Chats[chat.ID] = chat
	if _, err = real.Exec(ctx, name, s.Directory, "python3", "-c", "open('panta-review-unstaged.md','w').write('Review this untracked example\\n')"); err != nil {
		t.Fatal(err)
	}
	before, err := real.Exec(ctx, name, s.Directory, "python3", "-c", "import hashlib; print(hashlib.sha256(open('.git/index','rb').read()).hexdigest())")
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Operation: "review.capture", CallID: "acceptance-review", ProjectID: s.ProjectID, ChatID: chat.ID, SandboxID: s.ID, PrincipalID: s.PrincipalID, Repository: s.Repository}
	captured, err := w.dispatch(ctx, request)
	if err != nil {
		diagnostic := "import subprocess,sys; p=subprocess.run(['python3','-c',sys.argv[1],sys.argv[2]],stdout=subprocess.PIPE,stderr=subprocess.STDOUT); print(p.stdout.decode()[-8000:])"
		script := strings.Replace(reviewExportScript, "stderr=subprocess.DEVNULL", "stderr=None", 1)
		details, _ := real.Exec(ctx, name, s.Directory, "python3", "-c", diagnostic, script, s.Base)
		t.Fatal("real Linux review capture", err, details)
	}
	if captured.Review == nil || !strings.Contains(captured.Review.Diff, "preserve local work") || !strings.Contains(captured.Review.Diff, "Review this untracked example") {
		t.Fatal("capture missed committed or untracked changes", captured.Review)
	}
	after, err := real.Exec(ctx, name, s.Directory, "python3", "-c", "import hashlib; print(hashlib.sha256(open('.git/index','rb').read()).hexdigest())")
	actual, headErr := real.Exec(ctx, name, s.Directory, "git", "rev-parse", "HEAD")
	if err != nil || headErr != nil || before != after || actual != head {
		t.Fatal("capture changed agent index or branch", err, headErr)
	}
	request.Operation, request.Expected = "review.publish-plan", captured.Review.Head
	planned, err := w.dispatch(ctx, request)
	if err != nil || planned.PublishPlan == nil || planned.PublishPlan.Head != captured.Review.Head || len(planned.PublishPlan.Requests) < 2 {
		t.Fatal("real public review publication plan", err)
	}
	t.Logf("Derived %d exact GitHub object requests for reviewed commit %s; no remote mutation performed", len(planned.PublishPlan.Requests), planned.PublishPlan.Head)
	t.Logf("Verified real Linux capture %s with %d changed files; original branch and index preserved", captured.Review.Head, len(captured.Review.Files))
	t.Logf("Public CF Docs base %s imported into %s; local commit, retry, resume, and existing-file preservation verified", base, path.Base(s.Directory))
}
