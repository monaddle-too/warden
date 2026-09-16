package policy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const documentKey = "synthetic-document-signing-key-32-chars-minimum"

type recordedRequest struct {
	method, path string
	headers      http.Header
	body         []byte
}

type documentFixture struct {
	t         *testing.T
	dir       string
	keyFile   string
	mu        sync.Mutex
	requests  []recordedRequest
	status    int
	body      []byte
	headers   map[string]string
	onRequest func()
	server    *httptest.Server
	api       *DocumentAPI
	registry  *Registry
	verifier  *fixtureVerifier
	clock     *testClock
	value     map[string]any
	gateway   map[string]any
	begin     map[string]any
}

func newDocumentFixture(t *testing.T) *documentFixture {
	f := &documentFixture{t: t, dir: t.TempDir(), status: 200, body: []byte(`{"documents":[]}`), headers: map[string]string{"Content-Type": "application/json"}, clock: &testClock{now: 1000, mono: 1000}}
	f.keyFile = filepath.Join(f.dir, "key")
	writePrivate(t, f.keyFile, []byte(documentKey+"\n"))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
		hook := f.onRequest
		status, payload, headers := f.status, f.body, f.headers
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Length", itoa(len(payload)))
		w.WriteHeader(status)
		w.Write(payload)
	}))
	t.Cleanup(f.server.Close)
	api, err := NewDocumentAPI(f.server.URL, f.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	f.api = api
	f.verifier = &fixtureVerifier{enabled: true}
	options := RegistryOptions{Operations: testOperations(t), PolicyTemplate: templatePath(t), Verifier: f.verifier, Clock: f.clock.monotonic, DocumentAPI: api}
	f.registry, err = NewRegistry(filepath.Join(f.dir, "state"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.registry.Close() })
	f.verifier.registry = f.registry
	f.value = runContext(nil)
	f.registry.Register(f.value)
	f.registry.BindGateway(f.value, 19443)
	f.registry.ConfigureProvider(f.value, "synthetic-provider-fixture-secret")
	f.gateway, _ = f.registry.Gateway(f.value)
	f.begin, _ = f.registry.Begin(f.value, false)
	return f
}

func (f *documentFixture) request(method, path string, body []byte, extra map[string]any) map[string]any {
	f.t.Helper()
	if method == "" {
		method = "GET"
	}
	if path == "" {
		path = "/workspace/v1/documents"
	}
	message := map[string]any{"action": "document", "method": method, "path": path, "body": base64.StdEncoding.EncodeToString(body)}
	for k, v := range extra {
		message[k] = v
	}
	result, err := f.registry.Proxy("s1", f.gateway["capability"], message)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *documentFixture) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest{}, f.requests...)
}

func TestDocumentExactSignatureContextAndNoGuestCredentials(t *testing.T) {
	f := newDocumentFixture(t)
	result := f.request("POST", "/workspace/v1/documents/doc1/proposals", []byte(`{"content":"change","baseRevision":1}`), nil)
	if result["allow"] != true {
		t.Fatalf("denied: %v", result)
	}
	r := f.recorded()[0]
	if r.path != "/agent/v1/documents/doc1/proposals" {
		t.Fatalf("path %s", r.path)
	}
	encoded := r.headers.Get("X-Warden-Context")
	decoded, _ := base64.RawURLEncoding.DecodeString(encoded)
	var ctx map[string]any
	json.Unmarshal(decoded, &ctx)
	if !jsonEqual(ctx, f.value) {
		t.Fatalf("context: %s", decoded)
	}
	digest := sha256.Sum256(r.body)
	canonical := strings.Join([]string{r.method, r.path, hex.EncodeToString(digest[:]), r.headers.Get("X-Warden-Timestamp"), r.headers.Get("X-Warden-Nonce"), encoded}, "\n")
	mac := hmac.New(sha256.New, []byte(documentKey))
	mac.Write([]byte(canonical))
	if r.headers.Get("X-Warden-Signature") != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature mismatch")
	}
	if strings.Contains(Dumps(result), documentKey) || strings.Contains(Dumps(result), r.headers.Get("X-Warden-Signature")) || len(r.headers.Get("X-Warden-Nonce")) != 64 {
		t.Fatal("secrets in result")
	}
	if r.headers.Get("Authorization") != "" || r.headers.Get("Cookie") != "" {
		t.Fatal("guest headers forwarded")
	}
	if f.begin["documentBaseURL"] != "http://host.docker.internal:19443/workspace/v1/documents" {
		t.Fatalf("documentBaseURL: %v", f.begin["documentBaseURL"])
	}
	for _, needle := range []string{documentKey, r.headers.Get("X-Warden-Signature"), `"content":"change"`} {
		if hits := filesContain(t, filepath.Join(f.dir, "state"), []byte(needle)); len(hits) > 0 {
			t.Fatalf("%s persisted in %v", needle, hits)
		}
	}
}

func TestDocumentAllowedRoutesAndProjectQuery(t *testing.T) {
	f := newDocumentFixture(t)
	for _, path := range []string{"/workspace/v1/documents?projectID=p1", "/workspace/v1/documents/doc1", "/workspace/v1/documents/doc1/revisions",
		"/workspace/v1/documents/doc1/proposals", "/workspace/v1/documents?projectID=p1&offset=100", "/workspace/v1/documents/doc1/revisions?offset=8"} {
		if f.request("", path, nil, nil)["allow"] != true {
			t.Fatalf("%s denied", path)
		}
		recorded := f.recorded()
		if recorded[len(recorded)-1].path != strings.Replace(path, "/workspace/v1/", "/agent/v1/", 1) {
			t.Fatalf("upstream path for %s: %s", path, recorded[len(recorded)-1].path)
		}
	}
}

func TestDocumentPathMethodBodyAndContextForgeryDeniedBeforeUpstream(t *testing.T) {
	f := newDocumentFixture(t)
	for _, path := range []string{"/workspace/v1/documents?projectID=p2", "/workspace/v1/documents?url=http://evil", "/workspace/v1/documents/../auth",
		"/workspace/v1/documents/%2e%2e", "/workspace/v1/documents/doc1/proposals/accept", "/workspace/v1/documents/doc1#fragment", "/workspace/v1/documents/",
		"/workspace/v1/documents/doc1?projectID=p1", "/workspace/v1/documents?offset=-1", "/workspace/v1/documents?offset=1&offset=2",
		"/workspace/v1/documents?offset=1&target=evil", "/workspace/v1/documents/doc1?offset=0"} {
		if f.request("", path, nil, nil)["allow"] == true {
			t.Fatalf("%s allowed", path)
		}
	}
	if f.request("DELETE", "", nil, nil)["allow"] == true || f.request("", "", []byte("{}"), nil)["allow"] == true ||
		f.request("POST", "/workspace/v1/documents/doc1/proposals", []byte("[]"), nil)["allow"] == true ||
		f.request("POST", "/workspace/v1/documents/doc1/proposals", make([]byte, DocumentMaxBody+1), nil)["allow"] == true ||
		f.request("", "", nil, map[string]any{"context": runContext(map[string]any{"projectID": "p2"})})["allow"] == true {
		t.Fatal("forgery allowed")
	}
	if len(f.recorded()) != 0 {
		t.Fatal("upstream contacted")
	}
}

func TestDocumentSavedCommentRepliesAllowedButEditorDraftsDenied(t *testing.T) {
	f := newDocumentFixture(t)
	path := "/workspace/v1/documents/doc1/comments/replies?offset=8"
	if f.request("", path, nil, nil)["allow"] != true {
		t.Fatal("replies listing denied")
	}
	body := []byte(`{"id":"reply1","baseRevision":2,"text":"Explanation"}`)
	if f.request("POST", "/workspace/v1/documents/doc1/comments/thread1/replies", body, nil)["allow"] != true {
		t.Fatal("reply denied")
	}
	r := f.recorded()[1]
	if string(r.body) != string(body) {
		t.Fatal("body altered")
	}
	count := len(f.recorded())
	for _, suffix := range []string{"collaboration", "snapshot", "comments", "comments/thread1", "comments/%2e%2e/replies", "comments/thread1/replies?offset=1"} {
		for _, method := range []string{"GET", "POST"} {
			payload := []byte{}
			if method == "POST" {
				payload = []byte("{}")
			}
			if f.request(method, "/workspace/v1/documents/doc1/"+suffix, payload, nil)["allow"] == true {
				t.Fatalf("%s %s allowed", method, suffix)
			}
		}
	}
	if len(f.recorded()) != count {
		t.Fatal("upstream contacted")
	}
}

func TestDocumentGatewayCannotUseOtherSandboxCapability(t *testing.T) {
	f := newDocumentFixture(t)
	f.registry.Register(runContext(map[string]any{"sandboxID": "s2", "runtimeName": "sbx-two"}))
	if _, err := f.registry.Proxy("s2", f.gateway["capability"], map[string]any{"action": "document"}); err == nil {
		t.Fatal("capability crossed")
	}
	if len(f.recorded()) != 0 {
		t.Fatal("upstream contacted")
	}
}

func TestDocumentMissingConfigInactiveOrFailedProofFailClosed(t *testing.T) {
	f := newDocumentFixture(t)
	f.registry.DocumentAPI = nil
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("no config allowed")
	}
	renewed, _ := f.registry.Begin(f.value, true)
	if _, present := renewed["documentBaseURL"]; present {
		t.Fatal("documentBaseURL without config")
	}
	f.registry.DocumentAPI = f.api
	f.verifier.enabled = false
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("failed proof allowed")
	}
	f.verifier.enabled = true
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("revoked lease allowed")
	}
	if len(f.recorded()) != 0 {
		t.Fatal("upstream contacted")
	}
}

func TestDocumentEndDuringUpstreamResponseDoesNotDeadlockOrDeliver(t *testing.T) {
	f := newDocumentFixture(t)
	f.onRequest = func() { f.registry.End(f.value) }
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("delivered after end")
	}
	if len(f.recorded()) != 1 {
		t.Fatal("upstream count")
	}
}

func TestDocumentLeaseExpiringDuringFinalProofBlocksDelivery(t *testing.T) {
	f := newDocumentFixture(t)
	expiry := f.registry.Bindings["s1"].Lease.ExpiresAt
	f.onRequest = func() {
		f.registry.Clock = func() float64 { return expiry - 1 }
		f.verifier.hook = func(map[string]string, string) {
			f.verifier.hook = nil
			f.registry.Clock = func() float64 { return expiry + 1 }
		}
	}
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("delivered after expiry")
	}
	if len(f.recorded()) != 1 {
		t.Fatal("upstream count")
	}
}

func TestDocumentErrorStatusPreservedButRedirectNeverFollowed(t *testing.T) {
	f := newDocumentFixture(t)
	f.status, f.body = 409, []byte(`{"error":"revision_conflict"}`)
	if status, _ := asInt(f.request("", "", nil, nil)["status"]); status != 409 {
		t.Fatal("409 not preserved")
	}
	f.status = 302
	f.headers = map[string]string{"Location": f.server.URL + "/auth", "Content-Type": "application/json"}
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("redirect allowed")
	}
	recorded := f.recorded()
	if len(recorded) != 2 {
		t.Fatalf("requests: %d", len(recorded))
	}
	for _, r := range recorded {
		if r.path == "/auth" {
			t.Fatal("redirect followed")
		}
	}
}

func TestDocumentNonJSONCompressedOversizeAndAuthEchoDenied(t *testing.T) {
	f := newDocumentFixture(t)
	for _, c := range []struct {
		body    []byte
		headers map[string]string
	}{{[]byte("plain"), map[string]string{"Content-Type": "text/plain"}}, {[]byte("{}"), map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip"}},
		{[]byte(strings.Repeat(" ", DocumentMaxResponse+1)), map[string]string{"Content-Type": "application/json"}},
		{mustJSON(map[string]any{"echo": documentKey}), map[string]string{"Content-Type": "application/json"}}} {
		f.body, f.headers = c.body, c.headers
		if f.request("", "", nil, nil)["allow"] == true {
			t.Fatalf("%v allowed", c.headers)
		}
	}
	f.headers = map[string]string{"Content-Type": "application/json"}
	f.onRequest = func() {
		f.mu.Lock()
		f.body = mustJSON(map[string]any{"echo": f.requests[len(f.requests)-1].headers.Get("X-Warden-Signature")})
		f.mu.Unlock()
	}
	if f.request("", "", nil, nil)["allow"] == true {
		t.Fatal("signature echo allowed")
	}
}

func TestDocumentOriginAndKeyValidation(t *testing.T) {
	f := newDocumentFixture(t)
	for _, origin := range []string{"https://127.0.0.1:8080", "http://evil.test:8080", "http://127.1:8080", "http://127.0.0.1:8080/", "http://127.0.0.1", "http://user@localhost:8080", "http://localhost:8080?target=evil", "http://localhost:8080#fragment"} {
		if _, err := NewDocumentAPI(origin, f.keyFile); err == nil {
			t.Fatalf("%s accepted", origin)
		}
	}
	os.Chmod(f.keyFile, 0o644)
	if _, err := NewDocumentAPI(f.server.URL, f.keyFile); err == nil {
		t.Fatal("public key accepted")
	}
	os.Chmod(f.keyFile, 0o600)
	link := filepath.Join(f.dir, "link")
	os.Symlink(f.keyFile, link)
	if _, err := NewDocumentAPI(f.server.URL, link); err == nil {
		t.Fatal("symlink accepted")
	}
	for _, key := range [][]byte{[]byte("short"), []byte(strings.Repeat(" ", 64)), append([]byte("nonascii-"), make([]byte, 64)...)} {
		writePrivate(t, f.keyFile, key)
		if _, err := NewDocumentAPI(f.server.URL, f.keyFile); err == nil {
			t.Fatalf("key %q accepted", key)
		}
	}
}
