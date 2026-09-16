package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type localReviewRuntime struct {
	*testRuntime
	directory string
	exports   int
	started   chan struct{}
	release   chan struct{}
}

func (r *localReviewRuntime) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	if len(args) > 2 && args[2] == reviewExportScript {
		if r.started != nil {
			close(r.started)
			select {
			case <-r.release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		r.exports++
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = r.directory
		cmd.Env = publicGitEnvironment()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return r.testRuntime.Exec(ctx, name, dir, args...)
}
func reviewFixture(t *testing.T) (*Worker, *localReviewRuntime, Request, func(...string) string) {
	t.Helper()
	w, driver, _, req := managedFixture(t)
	dir := t.TempDir()
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
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte("Original plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "Initial")
	req.Repository = "github://Canton-Network/cf-docs"
	if _, err := w.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := w.managed.Sandboxes[req.SandboxID]
	s.Base = git("rev-parse", "HEAD")
	baseRoot := filepath.Join(w.Root, "repository-bundles", s.ID)
	if err := os.MkdirAll(baseRoot, 0700); err != nil {
		t.Fatal(err)
	}
	git("bundle", "create", filepath.Join(baseRoot, "review-base.bundle"), "HEAD")
	s.RepositoryReady = true
	s.State = "running"
	s.Directory = dir
	runtime := &localReviewRuntime{testRuntime: driver, directory: dir}
	w.Runtime = runtime
	return w, runtime, req, git
}
func TestReviewCapturesVerifiedTreeWithoutChangingAgentIndexOrBranch(t *testing.T) {
	w, runtime, req, git := reviewFixture(t)
	dir := runtime.directory
	ctx := context.Background()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("plan.md", "Staged version\n")
	git("add", "plan.md")
	write("plan.md", "Working version for review\n")
	write(".gitignore", "ignored.md\n")
	write("ignored.md", "Explicitly staged despite ignore\n")
	git("add", "-f", "ignored.md")
	write("new.md", "New example\n")
	write("binary.dat", string([]byte{0, 1, 2, 3}))
	beforeHead := git("rev-parse", "HEAD")
	beforeIndex, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	req.Operation = "review.capture"
	req.CallID = "review-one"
	result, err := w.dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	review := result.Review
	if review == nil || review.Head == review.Base || !strings.Contains(review.Diff, "Working version for review") || !strings.Contains(review.Diff, "Explicitly staged despite ignore") || len(review.Files) != 5 || review.BundleSHA256 == "" {
		t.Fatal("wrong captured snapshot", review)
	}
	afterIndex, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if string(afterIndex) != string(beforeIndex) || git("rev-parse", "HEAD") != beforeHead {
		t.Fatal("snapshot changed original index or branch")
	}
	if refs := git("for-each-ref", "--format=%(refname)", "refs/panta/"); refs != "" {
		t.Fatal("temporary refs retained", refs)
	}
	write("plan.md", "Changed after review\n")
	retry, err := w.dispatch(ctx, req)
	if err != nil || retry.Review.Head != review.Head || runtime.exports != 1 {
		t.Fatal("review retry re-snapshotted mutable files", err)
	}
	req.Operation = "review.latest"
	w.managed.Sandboxes[req.SandboxID].State = "stopped"
	latest, err := w.dispatch(ctx, req)
	if err != nil || latest.Review.Head != review.Head {
		t.Fatal("saved review unavailable after stop", err)
	}
	req.Operation = "review.read"
	saved, err := w.dispatch(ctx, req)
	if err != nil || saved.Review.Head != review.Head || runtime.exports != 1 {
		t.Fatal("reading a selected capture changed or lost its snapshot", err)
	}
	// Recheck the retained bundle independently, then reject a foreign trusted base.
	bundle := filepath.Join(w.reviewRoot(req.ChatID), review.ID, "snapshot.bundle")
	verified, err := verifyReviewBundle(ctx, t.TempDir(), bundle, review.Base, filepath.Join(w.Root, "repository-bundles", req.SandboxID, "review-base.bundle"))
	if err != nil || verified.Head != review.Head || verified.Diff != review.Diff {
		t.Fatal("retained bundle does not match displayed diff", err)
	}
	if _, err = verifyReviewBundle(ctx, t.TempDir(), bundle, strings.Repeat("1", 40), filepath.Join(w.Root, "repository-bundles", req.SandboxID, "review-base.bundle")); err == nil {
		t.Fatal("foreign base accepted")
	}
}
func TestReviewRejectsActiveRunsForeignBindingsAndUnfinishedMerges(t *testing.T) {
	w, runtime, req, git := reviewFixture(t)
	ctx := context.Background()
	req.Operation = "review.capture"
	req.CallID = "review-two"
	s := w.managed.Sandboxes[req.SandboxID]
	s.Active = &managedRun{ID: "active"}
	if _, err := w.dispatch(ctx, req); err == nil || runtime.exports != 0 {
		t.Fatal("captured active run")
	}
	s.Active = nil
	foreign := req
	foreign.PrincipalID = "other-principal"
	if _, err := w.dispatch(ctx, foreign); err == nil || runtime.exports != 0 {
		t.Fatal("foreign binding captured review")
	}
	if err := os.WriteFile(filepath.Join(runtime.directory, ".git", "MERGE_HEAD"), []byte(git("rev-parse", "HEAD")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.dispatch(ctx, req); err == nil {
		t.Fatal("captured unfinished merge")
	}
	if _, err := os.Stat(filepath.Join(w.reviewRoot(req.ChatID), req.CallID, "review.json")); !os.IsNotExist(err) {
		t.Fatal("failed review persisted")
	}
}

func TestReviewRetryRepairsPointerWithoutReplacingNewerSnapshot(t *testing.T) {
	w, runtime, req, _ := reviewFixture(t)
	req.Operation, req.CallID = "review.capture", "first"
	first, err := w.dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(w.reviewRoot(req.ChatID), "latest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	latestReq := req
	latestReq.Operation = "review.latest"
	repaired, err := w.dispatch(context.Background(), latestReq)
	if err != nil || repaired.Review.ID != first.Review.ID {
		t.Fatal("retry failed to repair pointer", err)
	}
	if err = os.WriteFile(filepath.Join(runtime.directory, "plan.md"), []byte("Newer content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	newerReq := req
	newerReq.CallID = "second"
	newer, err := w.dispatch(context.Background(), newerReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	latest, err := w.dispatch(context.Background(), latestReq)
	if err != nil || latest.Review.ID != newer.Review.ID || runtime.exports != 2 {
		t.Fatal("old retry replaced newer review", err)
	}
}
func TestReviewAllowsControlsButPreventsConcurrentExecution(t *testing.T) {
	w, runtime, req, _ := reviewFixture(t)
	runtime.started, runtime.release = make(chan struct{}), make(chan struct{})
	req.Operation, req.CallID = "review.capture", "slow-review"
	done := make(chan error, 1)
	go func() { _, err := w.dispatch(context.Background(), req); done <- err }()
	<-runtime.started
	defer func() {
		close(runtime.release)
		if err := <-done; err == nil {
			t.Error("persisted review after environment stopped")
		}
	}()
	controls := make(chan error, 1)
	go func() {
		status := req
		status.Operation = "status"
		if _, err := w.dispatch(context.Background(), status); err != nil {
			controls <- err
			return
		}
		prepare := req
		prepare.Operation, prepare.RunID = "prepare", "new-run"
		if _, err := w.dispatch(context.Background(), prepare); !errors.Is(err, ErrBusy) {
			controls <- errors.New("execution raced review")
			return
		}
		duplicate := req
		duplicate.CallID = "another-review"
		if _, err := w.dispatch(context.Background(), duplicate); !errors.Is(err, ErrBusy) {
			controls <- errors.New("overlapping review accepted")
			return
		}
		w.mu.Lock()
		w.managed.Sandboxes[req.SandboxID].LastActivity = time.Time{}
		w.mu.Unlock()
		if err := w.SweepIdle(context.Background()); err != nil {
			controls <- err
			return
		}
		w.mu.Lock()
		s := w.managed.Sandboxes[req.SandboxID]
		running := s.State == "running"
		w.mu.Unlock()
		if !running {
			controls <- errors.New("idle sweep interrupted review")
			return
		}
		stop := req
		stop.Operation = "stop"
		_, err := w.dispatch(context.Background(), stop)
		controls <- err
	}()
	select {
	case err := <-controls:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture blocked worker controls")
	}
}

func TestReviewRevocationDuringExportPreventsPersistence(t *testing.T) {
	w, runtime, req, _ := reviewFixture(t)
	runtime.started, runtime.release = make(chan struct{}), make(chan struct{})
	req.Operation, req.CallID = "review.capture", "revoked-review"
	done := make(chan error, 1)
	go func() { _, err := w.dispatch(context.Background(), req); done <- err }()
	<-runtime.started
	gate := w.Gate.(*testGate)
	gate.mu.Lock()
	gate.deny = true
	gate.mu.Unlock()
	close(runtime.release)
	if err := <-done; err == nil {
		t.Fatal("revoked capture succeeded")
	}
	if _, err := os.Stat(filepath.Join(w.reviewRoot(req.ChatID), req.CallID, "review.json")); !os.IsNotExist(err) {
		t.Fatal("revoked capture was retained", err)
	}
}
