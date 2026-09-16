package policy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pkt(data []byte) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(data)+4)), data...)
}

func pushPayload(old, new, ref string, caps []byte) []byte {
	if old == "" {
		old = strings.Repeat("1", 40)
	}
	if new == "" {
		new = strings.Repeat("2", 40)
	}
	if ref == "" {
		ref = "refs/heads/work"
	}
	if caps == nil {
		caps = []byte("report-status side-band-64k agent=git/2.43.0")
	}
	line := append([]byte(old+" "+new+" "+ref), 0)
	return append(pkt(append(line, caps...)), []byte("0000")...)
}

func TestGitOnlyCanonicalServices(t *testing.T) {
	for _, c := range [][2]string{{"GET", "/acme/demo.git/info/refs?service=git-upload-pack"}, {"POST", "/acme/demo.git/git-upload-pack"}, {"POST", "/acme/demo.git/git-receive-pack"}} {
		if GitRouteFor(c[0], c[1]) == nil {
			t.Fatalf("%v not routed", c)
		}
	}
	for _, path := range []string{"/acme/demo/info/refs?service=git-upload-pack", "/acme/demo.git/info/refs?service=git-upload-pack&x=1", "/acme/../demo.git/git-upload-pack", "/acme/demo.git/HEAD", "/acme/demo.git/git-upload-pack?x=1", "/acme/demo.git/objects/info/packs"} {
		if GitRouteFor("GET", path) != nil {
			t.Fatalf("%s routed", path)
		}
	}
}

func TestGitPushRestrictions(t *testing.T) {
	update, _, err := GitPush(pushPayload("", "", "", nil))
	if err != nil || update.Ref != "refs/heads/work" {
		t.Fatalf("push: %v %v", update, err)
	}
	extra := bytes.Replace(pushPayload("", "", "", nil), []byte("0000"), append(pkt([]byte("extra")), []byte("0000")...), 1)
	for _, body := range [][]byte{pushPayload("", ZeroOID, "", nil), pushPayload("", "", "refs/tags/v1", nil), pushPayload("", "", "refs/heads/../x", nil), pushPayload("", "", "refs/heads/x.lock", nil),
		pushPayload("", "", "", []byte("push-options")), pushPayload("", "", "", []byte("object-format=sha256")), append(pushPayload("", "", "", nil), []byte("junk")...), []byte("0001"), []byte("fffftiny"), extra} {
		if _, _, err := GitPush(body); err == nil {
			t.Fatalf("%q accepted", body)
		}
	}
}

// gitRepo runs git in a directory with a deterministic environment.
func gitRun(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "credential.helper=", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Warden Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Warden Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return append(out, stderr.Bytes()...), fmt.Errorf("git %v: %v: %s", args, err, stderr.String())
	}
	return out, nil
}

func mustGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := gitRun(t, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type repoFixture struct {
	t        *testing.T
	root     string
	remote   string
	seed     string
	work     string
	gw       *gatewayFixture
	engine   *Engine
	token    string
	proxyEnv []string
	caFile   string
}

func newRepoFixture(t *testing.T) *repoFixture {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	gw := newGatewayFixture(t)
	f := &repoFixture{t: t, gw: gw, root: filepath.Join(gw.dir, "repos")}
	f.remote = filepath.Join(f.root, "acme", "demo.git")
	f.seed = filepath.Join(gw.dir, "seed")
	f.work = filepath.Join(gw.dir, "work")
	os.MkdirAll(filepath.Dir(f.remote), 0o755)
	os.MkdirAll(f.seed, 0o755)
	mustGit(t, f.seed, "init", "-b", "main")
	os.WriteFile(filepath.Join(f.seed, "hello.txt"), []byte("hello\n"), 0o644)
	mustGit(t, f.seed, "add", ".")
	mustGit(t, f.seed, "commit", "-m", "Initial")
	mustGit(t, gw.dir, "clone", "--bare", f.seed, f.remote)
	mustGit(t, f.remote, "config", "http.receivepack", "true")
	f.engine = gw.registry.Bindings["s1"].Engine
	f.token = "ghp_" + strings.Repeat("TESTONLY", 8)
	if err := f.engine.SetToken(f.token); err != nil {
		t.Fatal(err)
	}
	// Registry engines start with an empty repository allowlist; the Python
	// integration test used a standalone engine without one.
	policy := f.engine.PolicyCopy()
	policy["allowed_repositories"] = []any{"acme/demo", "acme/other"}
	if err := f.engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	gitPath, _ := exec.LookPath("git")
	handler := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + f.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "REMOTE_USER=warden-test"}}
	gw.setHandler(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) })
	// The review fetches from the local bare repository instead of GitHub,
	// exactly like the Python integration test patched inspect_push.
	gw.gateway.cfg.Review = func(ctx context.Context, repository string, body []byte, auth, upstream string, active func() (bool, error)) (map[string]any, []byte, error) {
		if repository != "acme/demo" {
			return nil, nil, fmt.Errorf("unexpected repository %s", repository)
		}
		if auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+f.token)) {
			return nil, nil, fmt.Errorf("unexpected review credential")
		}
		directory, err := os.MkdirTemp("", "warden-review-test-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(directory)
		reviewer := &gitReviewer{directory: directory, active: active}
		git := func(args []string, data []byte, network bool, limit int) ([]byte, error) {
			return reviewer.run(ctx, args, data, false, limit)
		}
		if _, err = git([]string{"init", "--bare", "--quiet", "--template="}, nil, false, gitOutputLimit); err != nil {
			return nil, nil, err
		}
		if _, err = git([]string{"-c", "protocol.file.allow=always", "fetch", "--quiet", f.remote, "+refs/heads/*:refs/heads/*"}, nil, false, gitOutputLimit); err != nil {
			return nil, nil, err
		}
		if heads, _ := git([]string{"for-each-ref", "--format=%(refname)", "refs/heads/"}, nil, false, gitOutputLimit); len(heads) > 0 {
			if _, err = git([]string{"-c", "protocol.file.allow=always", "fetch", "--quiet", f.remote, "+HEAD:refs/warden/base"}, nil, false, gitOutputLimit); err != nil {
				return nil, nil, err
			}
		}
		update, pack, err := GitPush(body)
		if err != nil {
			return nil, nil, err
		}
		return InspectObjects(git, update, pack, body)
	}
	f.caFile = filepath.Join(gw.dir, "gateway-ca.pem")
	os.WriteFile(f.caFile, gw.ca.CertPEM, 0o644)
	proxy := "http://127.0.0.1:" + itoa(gw.port)
	f.proxyEnv = []string{"http_proxy=" + proxy, "https_proxy=" + proxy, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "GIT_SSL_CAINFO=" + f.caFile, "no_proxy=", "NO_PROXY="}
	return f
}

func (f *repoFixture) git(dir string, args ...string) ([]byte, error) {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "credential.helper=", "-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), f.proxyEnv...), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Warden Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Warden Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.Bytes(), err
}

func (f *repoFixture) pending(operation string) map[string]any {
	f.t.Helper()
	rows, err := f.engine.PendingRequests()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, row := range rows {
		if row["operation"] == operation {
			return row
		}
	}
	f.t.Fatalf("%s missing from approval queue", operation)
	return nil
}

func (f *repoFixture) approve(id, kind string, ttl int64) map[string]any {
	f.t.Helper()
	grant, err := f.engine.Approve(id, kind, ttl, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return grant
}

const remoteURL = "https://github.com/acme/demo.git"

func (f *repoFixture) checkout() map[string]any {
	f.t.Helper()
	out, err := f.git(f.gw.dir, "clone", remoteURL, f.work)
	if err == nil || !bytes.Contains(out, []byte("428")) {
		f.t.Fatalf("clone should be pending: %v %s", err, out)
	}
	if len(f.gw.upstreamRequests()) != 0 {
		f.t.Fatal("upstream contacted before approval")
	}
	grant := f.approve(f.pending("git/read")["id"].(string), "scoped", 600)
	if out, err := f.git(f.gw.dir, "clone", remoteURL, f.work); err != nil {
		f.t.Fatalf("clone: %v %s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(f.work, "hello.txt")); string(data) != "hello\n" {
		f.t.Fatal("clone content")
	}
	mustGit(f.t, f.work, "switch", "-c", "feature")
	return grant
}

func (f *repoFixture) change(text string) {
	f.t.Helper()
	if text == "" {
		text = "verified-source-private-marker\n"
	}
	os.WriteFile(filepath.Join(f.work, "hello.txt"), []byte(text), 0o644)
	mustGit(f.t, f.work, "add", ".")
	mustGit(f.t, f.work, "commit", "-m", "Test change")
}

func (f *repoFixture) push() ([]byte, error) {
	return f.git(f.work, "-c", "http.postBuffer=8388608", "push", "--no-thin", "origin", "HEAD:refs/heads/feature")
}

func TestGitCompleteCloneReviewPushFetchAndCredentialRetention(t *testing.T) {
	f := newRepoFixture(t)
	grant := f.checkout()
	f.change("")
	out, err := f.push()
	if err == nil || !bytes.Contains(out, []byte("428")) {
		t.Fatalf("push should be pending: %v %s", err, out)
	}
	row := f.pending("git/push")
	review, err := f.engine.GitReview(row["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review["patch"].(string), "verified-source-private-marker") || review["update"].(map[string]any)["ref"] != "refs/heads/feature" {
		t.Fatalf("review: %v", review)
	}
	if _, err := f.engine.Approve(row["id"].(string), "scoped", 60, nil); err == nil {
		t.Fatal("scoped push approved")
	}
	for _, needle := range []string{"verified-source-private-marker", f.token} {
		if hits := filesContain(t, filepath.Join(f.gw.dir, "state"), []byte(needle)); len(hits) > 0 {
			t.Fatalf("%s persisted in %v", needle, hits)
		}
	}
	f.approve(row["id"].(string), "exact", 60)
	if out, err := f.push(); err != nil {
		t.Fatalf("approved push: %v %s", err, out)
	}
	remoteHead := mustGit(t, f.remote, "rev-parse", "refs/heads/feature")
	workHead := mustGit(t, f.work, "rev-parse", "HEAD")
	if string(remoteHead) != string(workHead) {
		t.Fatal("push did not land")
	}
	if out, err := f.git(f.work, "fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v %s", err, out)
	}
	for _, r := range f.gw.upstreamRequests() {
		auth := r.headers.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") || strings.Contains(auth, "guest-credential") {
			t.Fatalf("upstream authorization %q", auth)
		}
	}
	if err := f.engine.Revoke(grant["grant_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.git(f.work, "fetch", "origin"); err == nil {
		t.Fatal("fetch after revocation succeeded")
	}
}

func TestGitApprovalCannotPublishAChangedCommit(t *testing.T) {
	f := newRepoFixture(t)
	f.checkout()
	f.change("")
	f.push()
	f.approve(f.pending("git/push")["id"].(string), "exact", 60)
	f.change("second change\n")
	out, err := f.push()
	if err == nil || !bytes.Contains(out, []byte("428")) {
		t.Fatalf("changed commit pushed: %v %s", err, out)
	}
	if _, err := gitRun(t, f.remote, "rev-parse", "--verify", "refs/heads/feature"); err == nil {
		t.Fatal("feature branch published")
	}
}

func TestGitBinaryHiddenInHistoryIsBlocked(t *testing.T) {
	f := newRepoFixture(t)
	f.checkout()
	os.WriteFile(filepath.Join(f.work, "binary"), []byte("\x00secret"), 0o644)
	mustGit(t, f.work, "add", ".")
	mustGit(t, f.work, "commit", "-m", "Binary")
	mustGit(t, f.work, "rm", "binary")
	mustGit(t, f.work, "commit", "-m", "Remove binary")
	out, err := f.push()
	if err == nil || !bytes.Contains(out, []byte("403")) {
		t.Fatalf("binary push: %v %s", err, out)
	}
	rows, _ := f.engine.PendingRequests()
	for _, row := range rows {
		if row["operation"] == "git/push" {
			t.Fatal("push request queued")
		}
	}
}

func TestGitForcePushRejectedEvenWithReadPermission(t *testing.T) {
	f := newRepoFixture(t)
	f.checkout()
	f.change("")
	f.push()
	f.approve(f.pending("git/push")["id"].(string), "exact", 60)
	if out, err := f.push(); err != nil {
		t.Fatalf("push: %v %s", err, out)
	}
	mustGit(t, f.work, "reset", "--hard", "main")
	f.change("divergent\n")
	out, err := f.git(f.work, "push", "--force", "--no-thin", "origin", "HEAD:refs/heads/feature")
	if err == nil || !bytes.Contains(out, []byte("403")) {
		t.Fatalf("force push: %v %s", err, out)
	}
}

func TestGitFirstPushToEmptyRepository(t *testing.T) {
	f := newRepoFixture(t)
	mustGit(t, f.remote, "update-ref", "-d", "refs/heads/main")
	out, err := f.git(f.gw.dir, "clone", remoteURL, f.work)
	if err == nil || !bytes.Contains(out, []byte("428")) {
		t.Fatalf("clone: %v %s", err, out)
	}
	f.approve(f.pending("git/read")["id"].(string), "scoped", 600)
	if out, err := f.git(f.gw.dir, "clone", remoteURL, f.work); err != nil {
		t.Fatalf("empty clone: %v %s", err, out)
	}
	mustGit(t, f.work, "switch", "-c", "feature")
	f.change("")
	out, err = f.push()
	if err == nil || !bytes.Contains(out, []byte("428")) {
		t.Fatalf("push: %v %s", err, out)
	}
	row := f.pending("git/push")
	review, _ := f.engine.GitReview(row["id"].(string))
	if !strings.Contains(review["patch"].(string), "verified-source-private-marker") {
		t.Fatal("review patch")
	}
	f.approve(row["id"].(string), "exact", 60)
	if out, err := f.push(); err != nil {
		t.Fatalf("push: %v %s", err, out)
	}
}

func TestGitRepositoryScopeAndReviewExpiry(t *testing.T) {
	f := newRepoFixture(t)
	f.checkout()
	value, _ := f.engine.Authorize(map[string]any{"method": "GET", "host": "github.com", "path": "/acme/other.git/info/refs?service=git-upload-pack", "headers": []any{}})
	if statusOf(value) != 428 || value["authorization"] != nil {
		t.Fatalf("other repository: %v", value)
	}
	f.change("")
	f.push()
	row := f.pending("git/push")
	f.engine.ExpireGitReview(row["id"].(string))
	if _, err := f.engine.Approve(row["id"].(string), "exact", 60, nil); err == nil {
		t.Fatal("expired review approved")
	}
	if _, err := f.engine.GitReview(row["id"].(string)); err == nil {
		t.Fatal("expired review returned")
	}
}

func TestGitRepackDiscardsUnreachableGuestObjects(t *testing.T) {
	f := newRepoFixture(t)
	f.checkout()
	f.change("")
	// Construct a valid pack containing an extra private blob that is not in
	// the branch. The stock client's normal pack would not include it.
	secretPath := filepath.Join(f.work, "untracked-secret")
	os.WriteFile(secretPath, []byte("unreachable-secret-marker"), 0o644)
	hidden := strings.TrimSpace(string(mustGit(t, f.work, "hash-object", "-w", secretPath)))
	head := strings.TrimSpace(string(mustGit(t, f.work, "rev-parse", "HEAD")))
	objects := mustGit(t, f.work, "rev-list", "--objects", "HEAD", "--not", "--remotes")
	var ids bytes.Buffer
	for _, line := range strings.Split(strings.TrimSpace(string(objects)), "\n") {
		ids.WriteString(strings.SplitN(line, " ", 2)[0] + "\n")
	}
	ids.WriteString(hidden + "\n")
	packCmd := exec.Command("git", "-C", f.work, "pack-objects", "--stdout")
	packCmd.Stdin = &ids
	pack, err := packCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	body := append(pushPayload(ZeroOID, head, "refs/heads/feature", nil), pack...)
	directory := t.TempDir()
	reviewer := &gitReviewer{directory: directory}
	git := func(args []string, data []byte, network bool, limit int) ([]byte, error) {
		return reviewer.run(context.Background(), args, data, false, limit)
	}
	if _, err = git([]string{"init", "--bare", "--quiet", "--template="}, nil, false, gitOutputLimit); err != nil {
		t.Fatal(err)
	}
	if _, err = git([]string{"-c", "protocol.file.allow=always", "fetch", "--quiet", f.remote, "+refs/heads/*:refs/heads/*", "+HEAD:refs/warden/base"}, nil, false, gitOutputLimit); err != nil {
		t.Fatal(err)
	}
	update, _, err := GitPush(body)
	if err != nil {
		t.Fatal(err)
	}
	review, clean, err := InspectObjects(git, update, pack, body)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	outputReviewer := &gitReviewer{directory: output}
	run := func(args []string, data []byte) ([]byte, error) {
		return outputReviewer.run(context.Background(), args, data, false, gitOutputLimit)
	}
	if _, err = run([]string{"init", "--bare", "--quiet", "--template="}, nil); err != nil {
		t.Fatal(err)
	}
	_, cleanPack, err := GitPush(clean)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"index-pack", "--stdin"}, cleanPack); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cat-file", "-t", hidden}, nil); err == nil {
		t.Fatal("unreachable object survived repack")
	}
	if strings.Contains(review["patch"].(string), "unreachable-secret-marker") {
		t.Fatal("review leaked unreachable object")
	}
	if review["pack_request_sha256"] != sha256Hex(clean) {
		t.Fatal("pack digest")
	}
}

func TestVisibleTextEscapesHiddenCharacters(t *testing.T) {
	got := visibleText([]byte("ok\ttab\n\x1b[31mred\xff‮"))
	bs := string(rune(92))
	if got != "ok\ttab\n"+bs+"u001b[31mred"+bs+"xff"+bs+"u202e" {
		t.Fatalf("visible: %q", got)
	}
}

var _ = tls.Config{}
var _ = time.Second
var _ = net.IPv4zero
