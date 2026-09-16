package policy

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
)

const (
	googleToken   = "google_docs_test_access_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	googleRefresh = "google_docs_test_refresh_yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	googleSecret  = "google_docs_test_client_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
)

func googleRequest(path string, fields map[string]any) map[string]any {
	if path == "" {
		path = "/v1/documents/TestFile"
	}
	req := map[string]any{"host": "docs.googleapis.com", "scheme": "https", "port": 443, "method": "GET", "path": path, "headers": []any{}}
	for k, v := range fields {
		req[k] = v
	}
	return req
}

type googleFixture struct {
	t         *testing.T
	engine    *Engine
	responses []map[string]string
	fail      error
}

func newGoogleFixture(t *testing.T) *googleFixture {
	f := &googleFixture{t: t, engine: newTestEngine(t, t.TempDir(), nil)}
	if err := f.engine.SetToken(testToken); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.GoogleDocs.Configure("test-client.apps.googleusercontent.com", googleSecret, "http://127.0.0.1:18765/oauth/google_docs/callback", false); err != nil {
		t.Fatal(err)
	}
	f.engine.GoogleDocs.Transport = func(clientID, secret, endpoint string, fields map[string]string) (map[string]any, error) {
		if f.fail != nil {
			return nil, f.fail
		}
		record := map[string]string{"endpoint": endpoint}
		for k, v := range fields {
			record[k] = v
		}
		f.responses = append(f.responses, record)
		return map[string]any{"access_token": googleToken, "refresh_token": googleRefresh, "expires_in": 3600, "scope": strings.Join(googleDocsProvider.scopes, " "), "token_type": "bearer"}, nil
	}
	f.connect()
	return f
}

func (f *googleFixture) connect() url.Values {
	f.t.Helper()
	u, err := f.engine.GoogleDocs.Start()
	if err != nil {
		f.t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	params := parsed.Query()
	if err = f.engine.GoogleDocs.Complete(params.Get("state"), "authorization-code"); err != nil {
		f.t.Fatal(err)
	}
	return params
}

func (f *googleFixture) grant(req map[string]any, kind string) map[string]any {
	f.t.Helper()
	if req == nil {
		req = googleRequest("", nil)
	}
	pending, _ := f.engine.Authorize(req)
	if statusOf(pending) != 428 {
		f.t.Fatalf("expected 428: %v", pending)
	}
	grant, err := f.engine.Approve(pending["request_id"].(string), kind, 60, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return grant
}

func TestGooglePKCEAndScopeConnection(t *testing.T) {
	f := newGoogleFixture(t)
	params := f.connect()
	if !sameStringSet(strings.Fields(params.Get("scope")), googleDocsProvider.scopes) || params.Get("code_challenge_method") != "S256" || params.Get("access_type") != "offline" {
		t.Fatalf("params: %v", params)
	}
	last := f.responses[len(f.responses)-1]
	sum := sha256.Sum256([]byte(last["code_verifier"]))
	if params.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || last["redirect_uri"] != "http://127.0.0.1:18765/oauth/google_docs/callback" {
		t.Fatal("pkce")
	}
}

func TestGooglePendingRequestNeverContainsToken(t *testing.T) {
	f := newGoogleFixture(t)
	pending, _ := f.engine.Authorize(googleRequest("", nil))
	if statusOf(pending) != 428 || pending["authorization"] != nil {
		t.Fatal("pending leaked")
	}
}

func TestGoogleDocsGetsOnlyGoogleDocsCredential(t *testing.T) {
	f := newGoogleFixture(t)
	f.grant(nil, "scoped")
	decision, _ := f.engine.Authorize(googleRequest("", nil))
	if decision["authorization"] != "Bearer "+googleToken || strings.Contains(Dumps(decision), testToken) {
		t.Fatalf("decision: %v", decision)
	}
	github := map[string]any{"host": "api.github.com", "method": "GET", "path": "/user", "headers": []any{}}
	f.grant(github, "scoped")
	decision, _ = f.engine.Authorize(github)
	if decision["authorization"] != "Bearer "+testToken {
		t.Fatal("github credential")
	}
}

func TestGoogleDocumentAndQueryBoundaries(t *testing.T) {
	f := newGoogleFixture(t)
	f.grant(nil, "scoped")
	for _, req := range []map[string]any{googleRequest("/v1/documents/OtherFile", nil), googleRequest("/v1/documents/TestFile?includeTabsContent=true", nil), googleRequest("/v1/documents/TestFile?suggestionsViewMode=SUGGESTIONS_INLINE", nil)} {
		r, _ := f.engine.Authorize(req)
		if statusOf(r) != 428 {
			t.Fatalf("expected 428 for %v: %v", req["path"], r)
		}
	}
	r, _ := f.engine.Authorize(googleRequest("", nil))
	if r["allow"] != true {
		t.Fatal("scoped denied")
	}
}

func TestGoogleExactGrantIsSingleUse(t *testing.T) {
	f := newGoogleFixture(t)
	f.grant(nil, "exact")
	r, _ := f.engine.Authorize(googleRequest("", nil))
	if r["allow"] != true {
		t.Fatal("denied")
	}
	r, _ = f.engine.Authorize(googleRequest("", nil))
	if statusOf(r) != 428 {
		t.Fatal("reused")
	}
}

func TestGoogleWritesUnknownHostsAndOAuthAreNotApprovable(t *testing.T) {
	f := newGoogleFixture(t)
	for _, req := range []map[string]any{googleRequest("", map[string]any{"method": "POST"}), googleRequest("/v1/documents/TestFile/comments", map[string]any{"method": "POST"}),
		googleRequest("/v1/oauth/token", nil), googleRequest("/v1/teams/team/projects", nil), googleRequest("", map[string]any{"host": "docs.google.com"}),
		googleRequest("", map[string]any{"host": "docs.googleapis.com.evil.example"}), googleRequest("", map[string]any{"scheme": "http", "port": 80}),
		googleRequest("", map[string]any{"port": 8443}), googleRequest("/v1/documents/TestFile/images", nil), googleRequest("/v1/documents/TestFile/../OtherFile", nil),
		googleRequest("/v1/documents/TestFile%2Fcomments", nil), googleRequest("/v1/documents/TestFile?plugin_data=shared", nil), googleRequest("/v1/documents/TestFile?access_token=evil", nil),
		googleRequest("/v1/documents/TestFile?includeTabsContent=true&depth=2", nil), googleRequest("", map[string]any{"headers": []any{[]any{"X-Goog-Api-Key", "guest"}}}),
		googleRequest("", map[string]any{"headers": []any{[]any{"Authorization", "Bearer guest"}}}), googleRequest("", map[string]any{"body_base64": base64.StdEncoding.EncodeToString([]byte("{}"))})} {
		r, _ := f.engine.Authorize(req)
		if statusOf(r) != 403 {
			t.Fatalf("expected 403 for %v: %v", req, r)
		}
	}
}

func TestGoogleInvalidQueriesAndBatchRoutes(t *testing.T) {
	f := newGoogleFixture(t)
	for _, path := range []string{"/batch", "/v1/documents/TestFile:batchUpdate", "/v1/documents/TestFile?key=guest", "/v1/documents/TestFile?includeTabsContent=1",
		"/v1/documents/TestFile?suggestionsViewMode=bad", "/v1/documents/TestFile?includeTabsContent=true&includeTabsContent=false", "/v1/documents/TestFile?fields=title", "/v1/documents/TestFile?alt=media"} {
		r, _ := f.engine.Authorize(googleRequest(path, nil))
		if statusOf(r) != 403 {
			t.Fatalf("expected 403 for %s: %v", path, r)
		}
	}
}

func TestGoogleFileAllowlistDoesNotReuseRepositoryPolicy(t *testing.T) {
	f := newGoogleFixture(t)
	policy := f.engine.PolicyCopy()
	policy["allowed_repositories"] = []any{}
	policy["allowed_google_documents"] = []any{"TestFile"}
	if err := f.engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	f.grant(nil, "scoped")
	r, _ := f.engine.Authorize(googleRequest("", nil))
	if r["allow"] != true {
		t.Fatal("allowlisted denied")
	}
	r, _ = f.engine.Authorize(googleRequest("/v1/documents/OtherFile", nil))
	if statusOf(r) != 403 {
		t.Fatal("other file")
	}
	r, _ = f.engine.Authorize(map[string]any{"host": "api.github.com", "method": "GET", "path": "/repos/acme/demo", "headers": []any{}})
	if statusOf(r) != 403 {
		t.Fatal("repository outside project")
	}
}

func TestGoogleMissingCredentialDoesNotConsumeGrantOrFallBack(t *testing.T) {
	f := newGoogleFixture(t)
	grant := f.grant(nil, "exact")
	f.engine.GoogleDocs.Disconnect()
	r, _ := f.engine.Authorize(googleRequest("", nil))
	if statusOf(r) != 503 || r["authorization"] != nil {
		t.Fatalf("result: %v", r)
	}
	var remaining int64
	f.engine.DB.QueryRow("SELECT remaining FROM grants WHERE id=?", grant["grant_id"]).Scan(&remaining)
	if remaining != 1 {
		t.Fatal("grant consumed")
	}
}

func TestGoogleRefreshUsesCorrectEndpointAndNeverDisclosesRefreshToken(t *testing.T) {
	f := newGoogleFixture(t)
	f.grant(nil, "scoped")
	f.engine.GoogleDocs.Expires = 0
	r, _ := f.engine.Authorize(googleRequest("", nil))
	last := f.responses[len(f.responses)-1]
	if r["allow"] != true || last["endpoint"] != "/token" || last["refresh_token"] != googleRefresh || last["grant_type"] != "refresh_token" || strings.Contains(Dumps(r), googleRefresh) {
		t.Fatalf("refresh: %v %v", r, last)
	}
	f.engine.GoogleDocs.Expires = 0
	f.fail = errors.New("private error")
	r, _ = f.engine.Authorize(googleRequest("", nil))
	if statusOf(r) != 503 || strings.Contains(Dumps(r), "private error") {
		t.Fatalf("refresh failure: %v", r)
	}
}

func TestGoogleCallbackStateReplayExpiryAndLatestAttempt(t *testing.T) {
	f := newGoogleFixture(t)
	c := f.engine.GoogleDocs
	stateOf := func() string {
		u, _ := c.Start()
		parsed, _ := url.Parse(u)
		return parsed.Query().Get("state")
	}
	old := stateOf()
	latest := stateOf()
	if err := c.Complete(old, "code"); err == nil {
		t.Fatal("old state accepted")
	}
	if err := c.Complete(latest, "code"); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(latest, "code"); err == nil {
		t.Fatal("replay accepted")
	}
	current := stateOf()
	c.pendingExpires = 0
	if err := c.Complete(current, "code"); err == nil {
		t.Fatal("expired accepted")
	}
	calls := len(f.responses)
	if err := c.Complete("bad-state", "bad-code"); err == nil || len(f.responses) != calls {
		t.Fatal("bad callback exchanged")
	}
}

func TestGoogleScopeMismatchCannotReplaceWorkingConnection(t *testing.T) {
	f := newGoogleFixture(t)
	c := f.engine.GoogleDocs
	for _, scope := range []string{"", "https://www.googleapis.com/auth/drive", strings.Join(googleDocsProvider.scopes, " ") + " https://www.googleapis.com/auth/drive"} {
		if err := c.Install(map[string]any{"access_token": "other_token_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "refresh_token": googleRefresh, "expires_in": 3600, "scope": scope}, false); err == nil {
			t.Fatalf("scope %q accepted", scope)
		}
		if c.AccessToken != googleToken {
			t.Fatal("connection replaced")
		}
	}
	if !GoogleProtectedHost("docs.googleapis.com") || !GoogleProtectedHost("www.googleapis.com") || GoogleProtectedHost("docs.googleapis.com.evil.example") {
		t.Fatal("host boundaries")
	}
	if hits := filesContain(t, f.engine.State, []byte(googleToken)); len(hits) > 0 {
		t.Fatal("token persisted")
	}
}

func TestFigmaOperationsAndConnection(t *testing.T) {
	engine := newTestEngine(t, t.TempDir(), nil)
	engine.SetToken(testToken)
	if err := engine.Figma.Configure("test-client", "figma_test_client_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "http://127.0.0.1:18765/oauth/figma/callback", false); err != nil {
		t.Fatal(err)
	}
	engine.Figma.Transport = func(clientID, secret, endpoint string, fields map[string]string) (map[string]any, error) {
		return map[string]any{"access_token": "figma_test_access_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "refresh_token": "figma_test_refresh_yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy", "expires_in": 3600, "user_id_string": "12345", "token_type": "bearer"}, nil
	}
	u, _ := engine.Figma.Start()
	parsed, _ := url.Parse(u)
	if !sameStringSet(strings.Fields(parsed.Query().Get("scope")), figmaProvider.scopes) {
		t.Fatal("scopes")
	}
	if err := engine.Figma.Complete(parsed.Query().Get("state"), "authorization-code"); err != nil {
		t.Fatal(err)
	}
	if engine.Figma.UserID != "12345" {
		t.Fatal("user id")
	}
	req := map[string]any{"host": "api.figma.com", "scheme": "https", "port": 443, "method": "GET", "path": "/v1/files/TestFile", "headers": []any{}}
	pending, _ := engine.Authorize(req)
	if statusOf(pending) != 428 {
		t.Fatalf("pending: %v", pending)
	}
	engine.Approve(pending["request_id"].(string), "scoped", 60, nil)
	decision, _ := engine.Authorize(req)
	if decision["authorization"] != "Bearer figma_test_access_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
		t.Fatalf("decision: %v", decision)
	}
	for _, path := range []string{"/v1/files/TestFile/nodes", "/v1/files/TestFile?depth=0", "/v1/files/TestFile/comments?as_md=maybe", "/v1/files/TestFile/versions", "/v1/me?x=1"} {
		altered := map[string]any{}
		for k, v := range req {
			altered[k] = v
		}
		altered["path"] = path
		r, _ := engine.Authorize(altered)
		if statusOf(r) != 403 {
			t.Fatalf("%s not 403: %v", path, r)
		}
	}
	if name, key, err := FigmaOperation("GET", "/v1/files/abc/nodes", []QueryPair{{"ids", "1:2,3:4"}, {"depth", "2"}}, nil); err != nil || name != "figma/files/nodes" || key != "abc" {
		t.Fatal("nodes op")
	}
	if !FigmaProtectedHost("www.figma.com") || FigmaProtectedHost("figma.com.evil.example") {
		t.Fatal("host boundary")
	}
}

func TestDocumentWriteOperationBoundaries(t *testing.T) {
	edit := mustJSON(map[string]any{"requests": []any{map[string]any{"insertText": map[string]any{"location": map[string]any{"index": 1}, "text": "Hi"}}}, "writeControl": map[string]any{"requiredRevisionId": "r1"}})
	access, doc, err := DocumentWriteOperation("POST", "/v1/documents/doc:batchUpdate", nil, edit)
	if err != nil || access != "write" || doc != "doc" {
		t.Fatalf("write: %s %s %v", access, doc, err)
	}
	for _, body := range [][]byte{mustJSON(map[string]any{"requests": []any{}}), mustJSON(map[string]any{"requests": []any{map[string]any{"replaceAllText": map[string]any{}}}}),
		mustJSON(map[string]any{"requests": []any{map[string]any{"insertText": map[string]any{}, "deleteContentRange": map[string]any{}}}}), mustJSON(map[string]any{"requests": []any{map[string]any{"insertText": map[string]any{}}}, "extra": 1}),
		mustJSON(map[string]any{"requests": []any{map[string]any{"insertText": map[string]any{}}}, "writeControl": map[string]any{"targetRevisionId": "x"}})} {
		if _, _, err := DocumentWriteOperation("POST", "/v1/documents/doc:batchUpdate", nil, body); err == nil {
			t.Fatalf("%s accepted", body)
		}
	}
	if _, _, err := DocumentWriteOperation("POST", "/v1/documents/doc:batchUpdate", []QueryPair{{"fields", "*"}}, edit); err == nil {
		t.Fatal("query accepted")
	}
	if access, doc, err := DocumentWriteOperation("GET", "/v1/documents/doc", nil, nil); err != nil || access != "read" || doc != "doc" {
		t.Fatal("read")
	}
}
