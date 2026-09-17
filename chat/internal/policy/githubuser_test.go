package policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const userToken = "gho_" + "abcdefghijklmnopqrstuvwxyz0123456789"

func writeGitHubUser(t *testing.T, path, token, login string) {
	t.Helper()
	writePrivate(t, path, mustJSON(map[string]any{"token": token, "login": login, "scopes": []any{"repo", "read:org"}, "obtained": 1700000000}))
}

// userAPI is an httptest GitHub API: repositories by full name, with the
// user's listing paged 100 at a time, recording every request.
type userAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	repos    []map[string]any
	status   int // when non-zero, every response uses it
}

func newUserAPI(t *testing.T, count int) *userAPI {
	a := &userAPI{t: t}
	for i := 1; i <= count; i++ {
		owner := "owner"
		if i%2 == 0 {
			owner = "org"
		}
		a.repos = append(a.repos, map[string]any{"id": i, "full_name": owner + "/repo" + strconv.Itoa(i), "private": i%3 == 0, "owner": map[string]any{"login": owner}})
	}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.server.Close)
	return a
}

func (a *userAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r)
	status := a.status
	a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+userToken {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
		return
	}
	if r.URL.Path == "/user/repos" {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("affiliation") != "owner,collaborator,organization_member" || page < 1 {
			w.WriteHeader(400)
			return
		}
		start := (page - 1) * 100
		end := start + 100
		if start > len(a.repos) {
			start = len(a.repos)
		}
		if end > len(a.repos) {
			end = len(a.repos)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.repos[start:end])
		return
	}
	if strings.HasPrefix(r.URL.Path, "/repos/") {
		name := strings.TrimPrefix(r.URL.Path, "/repos/")
		for _, repo := range a.repos {
			if repo["full_name"] == name {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(repo)
				return
			}
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
		return
	}
	w.WriteHeader(404)
}

func (a *userAPI) transport() GitHubUserTransport {
	return func(method, path, token string) (int, []byte, error) {
		req, err := http.NewRequest(method, a.server.URL+path, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := a.server.Client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		return res.StatusCode, data, err
	}
}

func (a *userAPI) paths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.requests {
		out = append(out, r.URL.RequestURI())
	}
	return out
}

func newUserSource(t *testing.T, api *userAPI) (*GitHubUserCredentials, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.json")
	writeGitHubUser(t, path, userToken, "owner")
	source := NewGitHubUserCredentials(path, NewRedactor(), nil)
	source.Transport = api.transport()
	return source, path
}

func TestUserTokenFileShapeAndPermissions(t *testing.T) {
	api := newUserAPI(t, 3)
	source, path := newUserSource(t, api)
	if !source.Available() {
		t.Fatal("valid file unavailable")
	}
	owner, appID, err := source.Identity()
	if err != nil || owner != "owner" || appID != 0 {
		t.Fatalf("identity: %s %d %v", owner, appID, err)
	}
	if !contains(source.Redactor.Secrets(), userToken) {
		t.Fatal("token not registered with the redactor")
	}
	snapshot := source.Snapshot()
	if snapshot["identity"] != "user" || snapshot["owner"] != "owner" || snapshot["configured"] != true || Dumps(snapshot) == "" || strings.Contains(Dumps(snapshot), userToken) {
		t.Fatalf("snapshot: %v", snapshot)
	}
	for name, document := range map[string]any{
		"array":            []any{},
		"missing token":    map[string]any{"login": "owner"},
		"wrong prefix":     map[string]any{"token": "ghs_" + strings.Repeat("a", 40), "login": "owner"},
		"short token":      map[string]any{"token": "gho_short", "login": "owner"},
		"injected login":   map[string]any{"token": userToken, "login": "owner\r\nInjected"},
		"scopes not list":  map[string]any{"token": userToken, "login": "owner", "scopes": "repo"},
		"obtained not num": map[string]any{"token": userToken, "login": "owner", "obtained": "yesterday"},
	} {
		writePrivate(t, path, mustJSON(document))
		if source.Available() {
			t.Fatalf("%s accepted", name)
		}
		if _, err := source.Authorization("owner/repo1", "repos/get", nil); err == nil || err.Error() != GitHubRefreshMessage {
			t.Fatalf("%s: %v", name, err)
		}
	}
	writeGitHubUser(t, path, userToken, "owner")
	os.Chmod(path, 0o644)
	if source.Available() {
		t.Fatal("public file accepted")
	}
	os.Chmod(path, 0o600)
	target := filepath.Join(filepath.Dir(path), "actual.json")
	os.Rename(path, target)
	os.Symlink(target, path)
	if source.Available() {
		t.Fatal("symlink accepted")
	}
	if len(api.paths()) != 0 {
		t.Fatalf("GitHub reached without a valid file: %v", api.paths())
	}
}

func TestUserTokenMissingFileFailsClosedEverywhere(t *testing.T) {
	api := newUserAPI(t, 1)
	source, path := newUserSource(t, api)
	os.Remove(path)
	if _, _, err := source.Identity(); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("identity: %v", err)
	}
	if _, err := source.Repositories(1); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("repositories: %v", err)
	}
	if _, err := source.Authorization("owner/repo1", "repos/get", nil); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("authorization: %v", err)
	}
	sharing, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	status, _ := sharing.Dispatch("status", nil)
	// configured: the source exists; connected: only once the file is readable.
	if !jsonEqual(status["github"], map[string]any{"configured": true, "connected": false, "owner": "", "appSlug": "", "mode": "user", "disconnectable": true}) {
		t.Fatalf("status: %v", status)
	}
	if _, err := sharing.Dispatch("github_repositories", nil); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("listing: %v", err)
	}
	if len(api.paths()) != 0 {
		t.Fatal("GitHub reached without a sign-in")
	}
	// A sign-in written later is picked up without a restart.
	writeGitHubUser(t, path, userToken, "owner")
	status, _ = sharing.Dispatch("status", nil)
	github, _ := status["github"].(map[string]any)
	if github["connected"] != true || github["owner"] != "owner" || github["login"] != "owner" || github["disconnectable"] != true {
		t.Fatalf("status after login: %v", status)
	}
}

func TestUserTokenInjectedOnlyAfterApprovalWithoutOwnerBoundary(t *testing.T) {
	api := newUserAPI(t, 2)
	dir := t.TempDir()
	engine := newTestEngine(t, dir, nil)
	source, path := newUserSource(t, api)
	source.Redactor = engine.Redactor
	engine.GitHubApp = source
	if err := engine.SetToken("ghp_" + strings.Repeat("z", 40)); err == nil {
		t.Fatal("manual token accepted beside the sign-in file")
	}
	// The organisation repository is reachable: a user token has no owner boundary.
	req := map[string]any{"host": "api.github.com", "path": "/repos/org/repo2", "method": "GET", "headers": []any{}}
	r, err := engine.Authorize(req)
	if err != nil || statusOf(r) != 428 {
		t.Fatalf("expected 428: %v %v", r, err)
	}
	if len(api.paths()) != 0 {
		t.Fatal("GitHub reached before approval")
	}
	if _, err = engine.Approve(r["request_id"].(string), "exact", 60, nil); err != nil {
		t.Fatal(err)
	}
	r, _ = engine.Authorize(req)
	if r["authorization"] != "Bearer "+userToken {
		t.Fatalf("authorization: %v", r)
	}
	if paths := api.paths(); len(paths) != 1 || paths[0] != "/repos/org/repo2" {
		t.Fatalf("calls: %v", paths)
	}
	if r, _ = engine.Authorize(req); statusOf(r) != 428 {
		t.Fatal("exact grant reused")
	}
	// Unsupported operations stay denied; a revoked sign-in fails closed but
	// keeps the grant for after the refresh.
	if r, _ = engine.Authorize(map[string]any{"host": "api.github.com", "path": "/repos/org/repo2/actions/runs", "method": "GET", "headers": []any{}}); statusOf(r) != 403 {
		t.Fatalf("unsupported operation: %v", r)
	}
	r, _ = engine.Authorize(req)
	engine.Approve(r["request_id"].(string), "exact", 60, nil)
	os.Remove(path)
	r, _ = engine.Authorize(req)
	if statusOf(r) != 503 || r["reason"] != "connect GitHub on the host" {
		t.Fatalf("missing file: %v", r)
	}
	writeGitHubUser(t, path, userToken, "owner")
	if r, _ = engine.Authorize(req); r["authorization"] != "Bearer "+userToken {
		t.Fatalf("grant not preserved: %v", r)
	}
	// Audit and decision records never carry the token.
	if hits := filesContain(t, dir, []byte(userToken)); len(hits) != 0 {
		t.Fatalf("token persisted: %v", hits)
	}
}

func TestUserTokenRevokedOrRenamedRepositoryFailsClosed(t *testing.T) {
	api := newUserAPI(t, 2)
	source, path := newUserSource(t, api)
	id := int64(1)
	if _, err := source.Authorization("owner/repo1", "repos/get", &id); err != nil {
		t.Fatal(err)
	}
	other := int64(2)
	if _, err := source.Authorization("owner/repo1", "repos/get", &other); err == nil || !strings.Contains(err.Error(), "select it again") {
		t.Fatalf("identity change: %v", err)
	}
	if _, err := source.Authorization("owner/missing", "repos/get", nil); err == nil || strings.Contains(err.Error(), "Refresh") {
		t.Fatalf("missing repository: %v", err)
	}
	if _, err := source.Authorization("owner/repo1", "issues/create", nil); err == nil {
		t.Fatal("unsupported operation authorized")
	}
	writeGitHubUser(t, path, "gho_"+strings.Repeat("r", 36), "owner")
	if _, err := source.Authorization("owner/repo1", "repos/get", nil); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := source.Repositories(1); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("revoked listing: %v", err)
	}
}

func TestUserTokenListingPaginationAndSelection(t *testing.T) {
	api := newUserAPI(t, 103)
	source, _ := newUserSource(t, api)
	first, err := source.Repositories(1)
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := first["repositories"].([]any)
	next, _ := asInt(first["next_page"])
	if len(repos) != 100 || next != 2 || first["owner"] != "owner" {
		t.Fatalf("page 1: %d %v", len(repos), first["next_page"])
	}
	second, err := source.Repositories(2)
	if err != nil {
		t.Fatal(err)
	}
	repos, _ = second["repositories"].([]any)
	if len(repos) != 3 || second["next_page"] != nil {
		t.Fatalf("page 2: %d %v", len(repos), second["next_page"])
	}
	if _, err = source.Repositories(0); err == nil {
		t.Fatal("page 0 accepted")
	}
	sharing, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	// Selection validates against the listing across pages; owner and
	// organisation repositories are both selectable.
	result, err := sharing.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo1", "org/repo102"}})
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := result["repositories"].([]any)
	if len(selected) != 2 || result["owner"] != "owner" {
		t.Fatalf("selected: %v", result)
	}
	if _, err = sharing.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"other/unknown"}}); err == nil {
		t.Fatal("unlisted repository selected")
	}
	var principal string
	if err = sharing.DB.QueryRow("SELECT principal FROM repositories WHERE name='org/repo102'").Scan(&principal); err != nil || principal != "owner" {
		t.Fatalf("principal: %q %v", principal, err)
	}
}

func TestUserTokenReachesGatewayAndSharingGrants(t *testing.T) {
	f := newGatewayFixture(t)
	api := newUserAPI(t, 2)
	source, _ := newUserSource(t, api)
	sharing, err := NewSharing(filepath.Join(f.dir, "sharing"), nil, f.clock.wall, source)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	f.registry.Sharing = sharing
	if _, err = sharing.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"org/repo2"}}); err != nil {
		t.Fatal(err)
	}
	f.setHandler(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"full_name":"org/repo2"}`))
	})
	res, _, err := f.do("GET", "https://api.github.com/repos/org/repo2", map[string]string{"Cookie": "forged", "Authorization": "Bearer forged"}, "")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("read: %v %v", res, err)
	}
	reqs := f.upstreamRequests()
	if len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "Bearer "+userToken || reqs[0].headers.Get("Cookie") != "" {
		t.Fatalf("upstream: %+v", reqs)
	}
	// Writes are never covered by the selection grant.
	res, _, err = f.do("DELETE", "https://api.github.com/repos/org/repo2/git/refs/heads/x", nil, "")
	if err != nil || res.StatusCode < 400 || len(f.upstreamRequests()) != 1 {
		t.Fatalf("write without approval: %v %v", res, err)
	}
	// The selection listed /user/repos once; the approved read rechecked the
	// repository once; the refused write never reached GitHub.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(api.paths()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if paths := api.paths(); len(paths) != 2 || !strings.HasPrefix(paths[0], "/user/repos?") || paths[1] != "/repos/org/repo2" {
		t.Fatalf("repository recheck: %v", paths)
	}
}

func TestRegistryRefusesAppBrokerBesideUserTokenFile(t *testing.T) {
	dir := t.TempDir()
	options := RegistryOptions{Operations: testOperations(t), PolicyTemplate: templatePath(t), Verifier: &fixtureVerifier{enabled: true}, GitHubAppConfig: filepath.Join(dir, "broker.json"), GitHubAuthFile: filepath.Join(dir, "github.json")}
	registry, err := NewRegistry(filepath.Join(dir, "state"), options)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if _, err = registry.Register(runContext(nil)); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both sources accepted: %v", err)
	}
	options.GitHubAppConfig = ""
	registry2, err := NewRegistry(filepath.Join(dir, "state2"), options)
	if err != nil {
		t.Fatal(err)
	}
	defer registry2.Close()
	if _, err = registry2.Register(runContext(nil)); err != nil {
		t.Fatal(err)
	}
	engine := registry2.Bindings["s1"].Engine
	if _, ok := engine.GitHubApp.(*GitHubUserCredentials); !ok {
		t.Fatalf("engine source: %T", engine.GitHubApp)
	}
	if _, err = engine.Authorize(map[string]any{"host": "api.github.com", "path": "/repos/owner/repo", "method": "GET", "headers": []any{}}); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
