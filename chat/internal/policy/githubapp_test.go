package policy

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const installationToken = "ghs_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func brokerResult(repo string, perms map[string]string) map[string]any {
	permsAny := map[string]any{}
	for k, v := range perms {
		permsAny[k] = v
	}
	if perms == nil {
		permsAny = map[string]any{"metadata": "read"}
	}
	return map[string]any{"token": installationToken, "expires_at": time.Now().Add(3500 * time.Second).UTC().Format(time.RFC3339),
		"permissions": permsAny, "repository": repo, "app_id": 123, "installation_id": 456}
}

type appFixture struct {
	t      *testing.T
	dir    string
	engine *Engine
	calls  []map[string]any
	run    func(input []byte) ([]byte, error)
}

func newAppFixture(t *testing.T) *appFixture {
	dir := t.TempDir()
	engine := newTestEngine(t, dir, nil)
	f := &appFixture{t: t, dir: dir, engine: engine}
	creds := NewGitHubAppCredentials([]string{"/trusted/broker"}, "owner", 123, engine.Redactor, engine.Now)
	creds.run = func(input []byte) ([]byte, error) {
		var value map[string]any
		_ = json.Unmarshal(input, &value)
		f.calls = append(f.calls, value)
		if f.run != nil {
			return f.run(input)
		}
		perms, _ := GitHubPermissions(value["operation"].(string))
		return mustJSON(brokerResult("owner/repo", perms)), nil
	}
	engine.GitHubApp = creds
	return f
}

func (f *appFixture) req(path, method string) map[string]any {
	if path == "" {
		path = "/repos/owner/repo"
	}
	if method == "" {
		method = "GET"
	}
	return map[string]any{"host": "api.github.com", "path": path, "method": method, "headers": []any{}}
}

func (f *appFixture) grant(req map[string]any) map[string]any {
	f.t.Helper()
	if req == nil {
		req = f.req("", "")
	}
	r, err := f.engine.Authorize(req)
	if err != nil {
		f.t.Fatal(err)
	}
	if statusOf(r) != 428 {
		f.t.Fatalf("expected 428, got %v", r)
	}
	grant, err := f.engine.Approve(r["request_id"].(string), "exact", 60, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return grant
}

func TestAppNoTokenNeededOrMintedBeforeApproval(t *testing.T) {
	f := newAppFixture(t)
	f.grant(nil)
	if len(f.calls) != 0 {
		t.Fatal("broker called before approval")
	}
	r, _ := f.engine.Authorize(f.req("", ""))
	if r["authorization"] != "Bearer "+installationToken {
		t.Fatalf("authorization: %v", r)
	}
	if len(f.calls) != 1 || f.calls[0]["repository"] != "owner/repo" || f.calls[0]["operation"] != "repos/get" {
		t.Fatalf("calls: %v", f.calls)
	}
	r, _ = f.engine.Authorize(f.req("", ""))
	if statusOf(r) != 428 {
		t.Fatal("exact grant reused")
	}
}

func TestAppHostOwnerAndOperationBoundary(t *testing.T) {
	f := newAppFixture(t)
	for _, p := range []string{"/repos/other/repo", "/user", "/repos/owner/repo/actions/runs"} {
		r, _ := f.engine.Authorize(f.req(p, ""))
		if statusOf(r) != 403 {
			t.Fatalf("expected 403 for %s: %v", p, r)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("broker called")
	}
}

func TestAppSourceFailurePreservesGrantAndNeverFallsBack(t *testing.T) {
	f := newAppFixture(t)
	grant := f.grant(nil)
	f.engine.token = "ghp_" + strings.Repeat("z", 40)
	f.run = func([]byte) ([]byte, error) { return nil, errors.New("secret diagnostic") }
	r, _ := f.engine.Authorize(f.req("", ""))
	if statusOf(r) != 503 || r["authorization"] != nil {
		t.Fatalf("expected 503 without credential: %v", r)
	}
	var remaining int64
	if err := f.engine.DB.QueryRow("SELECT remaining FROM grants WHERE id=?", grant["grant_id"]).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("grant consumed: %d %v", remaining, err)
	}
}

func TestAppManualTokenRejected(t *testing.T) {
	f := newAppFixture(t)
	if err := f.engine.SetToken("ghp_" + strings.Repeat("z", 40)); err == nil {
		t.Fatal("manual token accepted in app mode")
	}
}

func TestAppMalformedOrOverbroadBrokerResponse(t *testing.T) {
	f := newAppFixture(t)
	f.grant(nil)
	for _, changed := range []map[string]any{{"repository": "owner/other"}, {"permissions": map[string]any{"contents": "write"}}, {"app_id": 999},
		{"token": "ghp_" + strings.Repeat("x", 30)}, {"expires_at": "2000-01-01T00:00:00Z"}} {
		f.run = func([]byte) ([]byte, error) {
			result := brokerResult("owner/repo", nil)
			for k, v := range changed {
				result[k] = v
			}
			return mustJSON(result), nil
		}
		r, _ := f.engine.Authorize(f.req("", ""))
		if statusOf(r) != 503 {
			t.Fatalf("expected 503 for %v: %v", changed, r)
		}
	}
}

func TestAppFigmaCredentialsStaySeparate(t *testing.T) {
	f := newAppFixture(t)
	f.engine.Figma.AccessToken = "figma_test_token_value"
	f.engine.Figma.Expires = f.engine.Now() + 3600
	r := map[string]any{"host": "api.figma.com", "method": "GET", "path": "/v1/me", "headers": []any{}}
	f.grant(r)
	result, _ := f.engine.Authorize(r)
	if result["authorization"] != "Bearer figma_test_token_value" || len(f.calls) != 0 {
		t.Fatalf("figma: %v calls=%v", result, f.calls)
	}
}

func TestAppStoreNeverContainsToken(t *testing.T) {
	f := newAppFixture(t)
	f.grant(nil)
	f.engine.Authorize(f.req("", ""))
	if hits := filesContain(t, f.dir, []byte(installationToken)); len(hits) > 0 {
		t.Fatalf("token persisted: %v", hits)
	}
}

func TestAppWritePermissionsAreNarrow(t *testing.T) {
	f := newAppFixture(t)
	req := f.req("/repos/owner/repo/git/blobs", "POST")
	req["headers"] = []any{[]any{"Content-Type", "application/json"}}
	req["body_base64"] = base64.StdEncoding.EncodeToString([]byte(`{"content":"test","encoding":"utf-8"}`))
	f.grant(req)
	r, _ := f.engine.Authorize(req)
	if r["allow"] != true || f.calls[len(f.calls)-1]["operation"] != "git/create-blob" {
		t.Fatalf("blob: %v %v", r, f.calls)
	}
	perms, _ := GitHubPermissions("git/create-blob")
	if perms["contents"] != "write" || perms["metadata"] != "read" {
		t.Fatal("permissions")
	}
	perms, _ = GitHubPermissions("pulls/create")
	if perms["pull_requests"] != "write" {
		t.Fatal("pull permissions")
	}
	for _, op := range []string{"admin/delete", "issues/delete-comment", "users/get-authenticated"} {
		if _, err := GitHubPermissions(op); err == nil {
			t.Fatalf("%s accepted", op)
		}
	}
}

func TestAppMultipleEnginesShareSourceWithoutSharingGrants(t *testing.T) {
	f := newAppFixture(t)
	other := newTestEngine(t, filepath.Join(f.dir, "other"), nil)
	other.GitHubApp = f.engine.GitHubApp
	f.grant(nil)
	r, _ := other.Authorize(f.req("", ""))
	if statusOf(r) != 428 || len(f.calls) != 0 {
		t.Fatalf("other engine: %v", r)
	}
	r, _ = f.engine.Authorize(f.req("", ""))
	if r["allow"] != true {
		t.Fatal("own grant denied")
	}
}

func TestAppExpiredGrantAfterBrokerDoesNotDispatch(t *testing.T) {
	clock := &testClock{now: 1000, mono: 500}
	dir := t.TempDir()
	engine := newTestEngine(t, dir, clock)
	calls := 0
	creds := NewGitHubAppCredentials([]string{"/trusted/broker"}, "owner", 123, engine.Redactor, engine.Now)
	creds.run = func(input []byte) ([]byte, error) {
		calls++
		clock.advance(120)
		result := brokerResult("owner/repo", nil)
		result["expires_at"] = time.Unix(int64(clock.wall())+3500, 0).UTC().Format(time.RFC3339)
		return mustJSON(result), nil
	}
	engine.GitHubApp = creds
	req := map[string]any{"host": "api.github.com", "path": "/repos/owner/repo", "method": "GET", "headers": []any{}}
	pending, _ := engine.Authorize(req)
	if _, err := engine.Approve(pending["request_id"].(string), "exact", 60, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := engine.Authorize(req)
	if r["allow"] == true || calls != 1 {
		t.Fatalf("expired grant dispatched: %v", r)
	}
}

type storeFixture struct {
	t      *testing.T
	dir    string
	key    *rsa.PrivateKey
	calls  [][4]any
	broker *StoreBroker
}

func newStoreFixture(t *testing.T) *storeFixture {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	aesKey := make([]byte, 32)
	rand.Read(aesKey)
	writePrivate(t, filepath.Join(dir, "encryption.key"), aesKey)
	db, err := sql.Open("sqlite", filepath.Join(dir, "github.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE secrets(key TEXT PRIMARY KEY,value BLOB)"); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(aesKey)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	sealed := append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, mustJSON(map[string]any{"id": 123, "slug": "monaddle-workspace", "pem": pemText}), []byte("app"))...)
	if _, err = db.Exec("INSERT INTO secrets VALUES(?,?)", "app", sealed); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO secrets VALUES(?,?)", "user:never-read", []byte("invalid encrypted user record")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	f := &storeFixture{t: t, dir: dir, key: key}
	f.broker = NewStoreBroker(dir, 123, "owner", func(method, path, token string, body map[string]any) (map[string]any, error) {
		f.calls = append(f.calls, [4]any{method, path, token, body})
		if method == "GET" {
			return map[string]any{"id": 456, "app_id": 123, "account": map[string]any{"login": "owner"}, "suspended_at": nil}, nil
		}
		result := brokerResult("owner/repo", nil)
		result["permissions"] = body["permissions"]
		result["repositories"] = []any{map[string]any{"full_name": "owner/repo"}}
		return result, nil
	})
	return f
}

func TestStoreExistingAppKeySignsJWTWithoutUserTokens(t *testing.T) {
	f := newStoreFixture(t)
	out, err := f.broker.Issue(map[string]any{"repository": "owner/repo", "operation": "git/read"})
	if err != nil {
		t.Fatal(err)
	}
	if out["token"] != installationToken {
		t.Fatalf("token: %v", out)
	}
	body := f.calls[1][3].(map[string]any)
	if !jsonEqual(body, map[string]any{"repositories": []any{"repo"}, "permissions": map[string]any{"contents": "read", "metadata": "read"}}) {
		t.Fatalf("token request: %v", body)
	}
	jwt := f.calls[0][2].(string)
	parts := strings.Split(jwt, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatal("JWT signature invalid")
	}
}

func TestStoreAppSlugComesFromConfiguration(t *testing.T) {
	f := newStoreFixture(t)
	if f.broker.AppSlug != DefaultGitHubAppSlug {
		t.Fatal("the OVH App slug must remain the default", f.broker.AppSlug)
	}
	f.broker.AppSlug = "another-app"
	if _, err := f.broker.Issue(map[string]any{"repository": "owner/repo", "operation": "git/read"}); err == nil || len(f.calls) != 0 {
		t.Fatal("stored App accepted under a different configured slug", err)
	}
	f.broker.AppSlug = "monaddle-workspace"
	if _, err := f.broker.Issue(map[string]any{"repository": "owner/repo", "operation": "git/read"}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreUnauthorizedOwnerRejectedBeforeKeyAccess(t *testing.T) {
	f := newStoreFixture(t)
	f.broker.app = func() (map[string]any, error) { t.Fatal("must not read"); return nil, nil }
	if _, err := f.broker.Issue(map[string]any{"repository": "other/repo", "operation": "repos/get"}); err == nil {
		t.Fatal("other owner accepted")
	}
}

func TestStoreSuspendedOrWrongInstallationFails(t *testing.T) {
	f := newStoreFixture(t)
	for _, change := range []map[string]any{{"app_id": 999}, {"suspended_at": "now"}, {"account": map[string]any{"login": "other"}}} {
		f.broker.Transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
			result := map[string]any{"id": 456, "app_id": 123, "account": map[string]any{"login": "owner"}}
			for k, v := range change {
				result[k] = v
			}
			return result, nil
		}
		if _, err := f.broker.Issue(map[string]any{"repository": "owner/repo", "operation": "repos/get"}); err == nil {
			t.Fatalf("%v accepted", change)
		}
	}
}

func TestStorePrivateConfigRequired(t *testing.T) {
	f := newStoreFixture(t)
	p := filepath.Join(f.dir, "source.json")
	if err := os.WriteFile(p, mustJSON(map[string]any{"command": []any{"/broker"}, "owner": "owner", "app_id": 123}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGitHubAppConfig(p, NewRedactor(), nil); err == nil {
		t.Fatal("public config accepted")
	}
	os.Chmod(p, 0o600)
	creds, err := LoadGitHubAppConfig(p, NewRedactor(), nil)
	if err != nil || creds.Owner != "owner" {
		t.Fatalf("config: %v %v", creds, err)
	}
}

func TestStoreDiscoveryUsesMetadataOnlyAndFiltersOwnedRepositories(t *testing.T) {
	f := newStoreFixture(t)
	var calls [][3]any
	f.broker.Transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		calls = append(calls, [3]any{method, path, body})
		if strings.HasPrefix(path, "/users/") {
			return map[string]any{"id": 456, "app_id": 123, "account": map[string]any{"login": "owner", "type": "User"}}, nil
		}
		if method == "POST" {
			return map[string]any{"token": installationToken, "permissions": map[string]any{"metadata": "read"}}, nil
		}
		return map[string]any{"repositories": []any{map[string]any{"id": 1, "full_name": "owner/private", "private": true, "owner": map[string]any{"login": "owner"}},
			map[string]any{"id": 2, "full_name": "other/private", "owner": map[string]any{"login": "other"}}}}, nil
	}
	out, err := f.broker.Repositories(2)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(out["repositories"], []any{map[string]any{"id": 1, "full_name": "owner/private", "private": true}}) {
		t.Fatalf("repositories: %v", out["repositories"])
	}
	if !jsonEqual(calls[1][2], map[string]any{"permissions": map[string]any{"metadata": "read"}}) || calls[2][1] != "/installation/repositories?per_page=100&page=2" {
		t.Fatalf("calls: %v", calls)
	}
	if strings.Contains(Dumps(out), installationToken) {
		t.Fatal("token in listing")
	}
	if _, err := f.broker.Repositories(0); err == nil {
		t.Fatal("page 0 accepted")
	}
}

func TestBrokerSubcommandProtocol(t *testing.T) {
	var out strings.Builder
	code := RunGitHubBroker([]string{"--store", t.TempDir(), "--app-id", "123", "--owner", "owner"}, strings.NewReader(`{"repository":"owner/repo","operation":"repos/get"}`), &out)
	if code != 1 || out.String() != `{"error":"GitHub App credential unavailable"}` {
		t.Fatalf("missing store: %d %s", code, out.String())
	}
}
