package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const repositoryTestBase = "0123456789012345678901234567890123456789"

type fixtureRepositorySource struct{ calls int }

func (s *fixtureRepositorySource) Bundle(_ context.Context, repo, pinned, dest string) (string, error) {
	s.calls++
	return repositoryTestBase, os.WriteFile(dest, []byte("fixture offline bundle"), 0600)
}

type repositoryRuntime struct {
	*testRuntime
	lostResponse bool
	imports      int
	resumes      int
}

func (r *repositoryRuntime) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	if len(args) > 2 && args[2] == repositoryImportScript {
		r.imports++
		if r.lostResponse {
			r.lostResponse = false
			return "", errors.New("lost import response")
		}
		return repositoryTestBase, nil
	}
	if len(args) > 2 && args[2] == repositoryResumeScript {
		r.resumes++
	}
	return r.testRuntime.Exec(ctx, name, dir, args...)
}

func TestRepositoryBindingAndImportRetryPreserveCheckout(t *testing.T) {
	w, driver, gate, req := managedFixture(t)
	ctx := context.Background()
	source := &fixtureRepositorySource{}
	runtime := &repositoryRuntime{testRuntime: driver, lostResponse: true}
	w.RepositorySource, w.Runtime = source, runtime
	req.Repository = "github://Canton-Network/cf-docs"
	if _, err := w.dispatch(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Operation = "prepare"
	if _, err := w.dispatch(ctx, req); err == nil {
		t.Fatal("lost response should fail preparation")
	}
	old := *w.managed.Sandboxes[req.SandboxID]
	if old.RepositoryReady || old.Base != repositoryTestBase || old.Directory == old.RepositoryCheckout || source.calls != 1 {
		t.Fatal("pending checkout identity was not retained", old)
	}
	// A worker restart must use the pinned import, not refetch a moving HEAD.
	restarted := NewWorker(w.Root, "/never-host-exec", "template")
	restarted.Runtime, restarted.Gate, restarted.RepositorySource = runtime, gate, source
	restarted.RuntimeDir = w.RuntimeDir
	if err := restarted.initializeManaged(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := restarted.dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	s := restarted.managed.Sandboxes[req.SandboxID]
	if !s.RepositoryReady || res.Directory != old.RepositoryCheckout || res.Base != repositoryTestBase || source.calls != 1 || runtime.imports != 2 {
		t.Fatal("retry replaced or refetched checkout", res, s)
	}
	if _, err = os.Stat(filepath.Join(w.Root, "repository-bundles", s.ID, "checkout.bundle")); !os.IsNotExist(err) {
		t.Fatal("completed transfer was retained", err)
	}
	s.Active = nil
	req.RunID = "run-two"
	if _, err = restarted.dispatch(ctx, req); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || runtime.imports != 2 || runtime.resumes != 1 {
		t.Fatal("resume reset the working tree")
	}
	for _, replacement := range []string{"", "github://someone/else"} {
		req.Operation = "bind-chat"
		req.Repository = replacement
		if _, err = restarted.dispatch(ctx, req); err == nil {
			t.Fatal("silently replaced repository", replacement)
		}
	}
}

func TestRepositoryRejectsUnsafeSourcesAndChecksEnforcementBeforeFetch(t *testing.T) {
	w, _, gate, req := managedFixture(t)
	source := &fixtureRepositorySource{}
	w.RepositorySource = source
	for _, repo := range []string{"/host/private", "https://github.com/o/r", "github://o/r?token=x", "github://o/../r", "github://o/r.git", "github://o/r\n", "github://token@github.com/o/r"} {
		req.Repository = repo
		if _, err := w.dispatch(context.Background(), req); err == nil {
			t.Fatal("accepted", repo)
		}
	}
	req.Repository = "github://Canton-Network/cf-docs"
	if _, err := w.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	gate.deny = true
	req.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), req); err == nil || source.calls != 0 {
		t.Fatal("fetched before enforcement authorization", err)
	}
}

// Real Git creates/verifies a bundle while a PATH shim redirects the one fixed
// GitHub URL to a disposable local repository. No network or real credentials.
func TestPublicRepositorySourceIgnoresAmbientCredentials(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "origin")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		cmd.Env = append(publicGitEnvironment(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	if err = os.WriteFile(filepath.Join(repo, "plan.md"), []byte("public fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "plan.md")
	run("commit", "-m", "Initial")
	base := run("rev-parse", "HEAD")
	bin := filepath.Join(dir, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	encode := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	shim := "#!" + python + "\nimport os,sys\nassert 'GIT_CONFIG_COUNT' not in os.environ\nassert 'GH_TOKEN' not in os.environ\nassert os.environ['HOME']=='/nonexistent'\nargs=[(" + encode(repo) + " if a=='https://github.com/Canton-Network/cf-docs.git' else a) for a in sys.argv[1:]]\nos.execv(" + encode(git) + ",[" + encode(git) + "]+args)\n"
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "synthetic-secret")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraheader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: synthetic-secret")
	dest := filepath.Join(dir, "checkout.bundle")
	actual, err := (publicGitHubSource{}).Bundle(ctx, "github://Canton-Network/cf-docs", "", dest)
	if err != nil || actual != base {
		t.Fatal(actual, err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil || strings.Contains(string(raw), "synthetic-secret") {
		t.Fatal("credential in bundle", err)
	}
	if out, err := exec.Command(git, "-C", repo, "bundle", "verify", dest).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
}
