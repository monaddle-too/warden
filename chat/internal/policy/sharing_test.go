package policy

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeGoogle struct {
	fileErr       error
	authorization string
	createResult  map[string]any
	createErr     error
	createCalls   []string
	filesResult   map[string]any
	configured    bool
	connected     bool
	canWrite      bool
	disconnected  bool
}

func (g *fakeGoogle) Configured() bool { return g.configured }
func (g *fakeGoogle) Connected() bool  { return g.connected }
func (g *fakeGoogle) CanWrite() bool   { return g.canWrite }
func (g *fakeGoogle) Start() (string, error) {
	return "https://accounts.google.com/o/oauth2/v2/auth?state=x", nil
}
func (g *fakeGoogle) Complete(string, string) error { return nil }
func (g *fakeGoogle) Disconnect() error {
	g.connected, g.authorization, g.disconnected = false, "", true
	return nil
}
func (g *fakeGoogle) Files(page string) (map[string]any, error) {
	if g.filesResult == nil {
		return map[string]any{"files": []any{}}, nil
	}
	return g.filesResult, nil
}
func (g *fakeGoogle) File(id string) (map[string]any, error) {
	if g.fileErr != nil {
		return nil, g.fileErr
	}
	return map[string]any{"id": id, "title": id, "url": "https://docs.google.com/document/d/" + id + "/edit", "api_url": "https://docs.googleapis.com/v1/documents/" + id}, nil
}
func (g *fakeGoogle) Create(title string) (map[string]any, error) {
	g.createCalls = append(g.createCalls, title)
	if g.createErr != nil {
		return nil, g.createErr
	}
	if g.createResult != nil {
		return g.createResult, nil
	}
	return map[string]any{"id": "new-doc", "title": title}, nil
}
func (g *fakeGoogle) Authorization() (string, error) {
	if g.authorization == "" {
		return "", errors.New("not connected")
	}
	return g.authorization, nil
}

type sharingFixture struct {
	t      *testing.T
	dir    string
	clock  *testClock
	google *fakeGoogle
	s      *Sharing
}

func newSharingFixture(t *testing.T) *sharingFixture {
	f := &sharingFixture{t: t, dir: t.TempDir(), clock: &testClock{now: 1000}, google: &fakeGoogle{authorization: "Bearer synthetic-google-secret", canWrite: true, connected: true, configured: true}}
	f.s = f.open()
	return f
}

func (f *sharingFixture) open() *Sharing {
	f.t.Helper()
	s, err := NewSharing(f.dir, f.google, f.clock.wall, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(s.Close)
	return s
}

func (f *sharingFixture) dispatch(op string, data map[string]any) map[string]any {
	f.t.Helper()
	result, err := f.s.Dispatch(op, data)
	if err != nil {
		f.t.Fatalf("%s: %v", op, err)
	}
	return result
}

func (f *sharingFixture) request(changes map[string]any) map[string]any {
	data := map[string]any{"chatID": "chat-a", "sandboxID": "sbx-a", "callID": "tool1", "reason": "Read the plan"}
	for k, v := range changes {
		data[k] = v
	}
	return f.dispatch("request", data)
}

func (f *sharingFixture) grant() map[string]any {
	r := f.request(nil)
	return f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc-a"}, "duration": 900})
}

func (f *sharingFixture) read(doc, chat, sandbox string, changes map[string]any) (map[string]any, string, error) {
	if doc == "" {
		doc = "doc-a"
	}
	if chat == "" {
		chat = "chat-a"
	}
	if sandbox == "" {
		sandbox = "sbx-a"
	}
	request := map[string]any{"method": "GET", "scheme": "https", "port": 443, "host": "docs.googleapis.com", "path": "/v1/documents/" + doc, "body_base64": ""}
	for k, v := range changes {
		request[k] = v
	}
	return f.s.Authorize(chat, sandbox, request)
}

func TestSharingDurableRequestIdempotencyAndCompletion(t *testing.T) {
	f := newSharingFixture(t)
	r := f.request(nil)
	if f.request(nil)["request_id"] != r["request_id"] {
		t.Fatal("duplicate request")
	}
	f.s.Close()
	f.s = f.open()
	state := f.dispatch("state", nil)["requests"].([]any)
	if state[0].(map[string]any)["status"] != "pending" {
		t.Fatal("request not durable")
	}
	g := f.grant()
	if len(f.dispatch("undelivered", nil)["requests"].([]any)) != 1 {
		t.Fatal("undelivered")
	}
	f.dispatch("ack", map[string]any{"id": g["request_id"]})
	if len(f.dispatch("undelivered", nil)["requests"].([]any)) != 0 {
		t.Fatal("ack ignored")
	}
}

func TestSharingScopeExpiryAndRevocation(t *testing.T) {
	f := newSharingFixture(t)
	g := f.grant()
	if _, auth, err := f.read("", "", "", nil); err != nil || auth != "Bearer synthetic-google-secret" {
		t.Fatalf("read: %v %v", auth, err)
	}
	// Another chat on the same environment shares the grant; another environment does not.
	if _, auth, err := f.read("", "chat-b", "", nil); err != nil || auth != "Bearer synthetic-google-secret" {
		t.Fatal("shared environment denied")
	}
	if f.dispatch("get", map[string]any{"id": g["request_id"], "chatID": "chat-b", "sandboxID": "sbx-a"})["status"] != "granted" {
		t.Fatal("get")
	}
	id := g["request_id"].(string)
	if !f.s.Active(id, "chat-b", "sbx-a") || f.s.Active(id, "chat-a", "sbx-b") {
		t.Fatal("active scope")
	}
	for _, c := range []struct {
		doc, sandbox string
		changes      map[string]any
	}{{"doc-b", "", nil}, {"", "sbx-b", nil}, {"", "", map[string]any{"method": "POST"}}, {"", "", map[string]any{"host": "www.googleapis.com"}},
		{"", "", map[string]any{"path": "/v1/documents/doc-a:batchUpdate"}}, {"", "", map[string]any{"path": "/v1/documents/doc-a?fields=*"}}, {"", "", map[string]any{"body_base64": "e30="}}} {
		if _, _, err := f.read(c.doc, "", c.sandbox, c.changes); err == nil {
			t.Fatalf("%v allowed", c)
		}
	}
	f.clock.now = 1900
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("expired grant allowed")
	}
	f.clock.now = 1001
	f.dispatch("revoke", map[string]any{"id": id})
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("revoked grant allowed")
	}
}

func TestSharingDenyDoesNotGrantAndCannotBeChanged(t *testing.T) {
	f := newSharingFixture(t)
	r := f.request(nil)
	f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": false})
	if f.grant()["status"] != "denied" {
		t.Fatal("denial changed")
	}
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("denied read allowed")
	}
}

func TestSharingOwnerSelectionIsVerifiedBeforeCommit(t *testing.T) {
	f := newSharingFixture(t)
	r := f.request(nil)
	f.google.fileErr = errors.New("no such file")
	if _, err := f.s.Dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc-a"}, "duration": 900}); err == nil {
		t.Fatal("unverified selection committed")
	}
	if f.dispatch("state", nil)["requests"].([]any)[0].(map[string]any)["status"] != "pending" {
		t.Fatal("status changed")
	}
	if len(f.dispatch("list", map[string]any{"chatID": "chat-a", "sandboxID": "sbx-a"})["grants"].([]any)) != 0 {
		t.Fatal("grant listed")
	}
}

func TestSharingUnsharableTagBlocksGrantsAndRevokesExistingAccess(t *testing.T) {
	f := newSharingFixture(t)
	g := f.grant()
	f.google.filesResult = map[string]any{"files": []any{map[string]any{"id": "doc-a", "name": "Plan"}, map[string]any{"id": "doc-b", "name": "Notes"}}, "nextPageToken": "p2"}
	blockedFlags := func() []bool {
		var out []bool
		for _, item := range f.dispatch("files", nil)["files"].([]any) {
			out = append(out, item.(map[string]any)["blocked"].(bool))
		}
		return out
	}
	if flags := blockedFlags(); flags[0] || flags[1] {
		t.Fatal("blocked before tagging")
	}
	status := f.dispatch("status", nil)
	if !jsonEqual(status["github"], map[string]any{"configured": false, "connected": false, "owner": "", "appSlug": ""}) {
		t.Fatalf("status: %v", status)
	}
	if !jsonEqual(status["google"], map[string]any{"configured": true, "connected": true}) {
		t.Fatalf("status: %v", status)
	}
	// providers.github names an App without a running broker: configured, not connected.
	f.s.GitHubConfigured, f.s.GitHubAppSlug = true, "example-app"
	status = f.dispatch("status", nil)
	if !jsonEqual(status["github"], map[string]any{"configured": true, "connected": false, "owner": "", "appSlug": "example-app"}) {
		t.Fatalf("status: %v", status)
	}
	f.s.GitHubConfigured, f.s.GitHubAppSlug = false, ""
	// A nil Google provider (providers.google absent) is hidden, not disconnected.
	hidden, err := NewSharing(t.TempDir(), nil, f.clock.wall, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hidden.Close()
	status, err = hidden.Dispatch("status", nil)
	if err != nil || !jsonEqual(status["google"], map[string]any{"configured": false, "connected": false}) || status["configured"] != false {
		t.Fatalf("status: %v %v", status, err)
	}
	r := f.dispatch("block", map[string]any{"id": "doc-a", "name": "Plan"})
	if !jsonEqual(r["revoked"], []any{g["request_id"]}) {
		t.Fatalf("revoked: %v", r)
	}
	if f.dispatch("state", nil)["requests"].([]any)[0].(map[string]any)["status"] != "revoked" {
		t.Fatal("grant not revoked")
	}
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("blocked document readable")
	}
	blocked := f.dispatch("blocked", nil)["documents"].([]any)
	if !jsonEqual(blocked, []any{map[string]any{"id": "doc-a", "name": "Plan", "blocked_at": 1000.0}}) {
		t.Fatalf("blocked listing: %v", blocked)
	}
	if flags := blockedFlags(); !flags[0] || flags[1] {
		t.Fatal("flags after tagging")
	}
	if f.dispatch("files", nil)["nextPageToken"] != "p2" {
		t.Fatal("page token")
	}
	n := f.dispatch("request", map[string]any{"chatID": "chat-a", "sandboxID": "sbx-a", "callID": "tool2", "reason": "Read it again"})
	if _, err := f.s.Dispatch("resolve", map[string]any{"id": n["request_id"], "allow": true, "documents": []any{"doc-b", "doc-a"}, "duration": 900}); err == nil {
		t.Fatal("tagged document granted")
	}
	if f.dispatch("get", map[string]any{"id": n["request_id"], "chatID": "chat-a", "sandboxID": "sbx-a"})["status"] != "pending" {
		t.Fatal("request not pending")
	}
	if f.dispatch("resolve", map[string]any{"id": n["request_id"], "allow": true, "documents": []any{"doc-b"}, "duration": 900})["status"] != "granted" {
		t.Fatal("doc-b not granted")
	}
	if _, auth, err := f.read("doc-b", "", "", nil); err != nil || auth != "Bearer synthetic-google-secret" {
		t.Fatal("doc-b read")
	}
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("doc-a read")
	}
	// A stale grant that somehow names a tagged document is still refused at dispatch.
	if _, err := f.s.DB.Exec("UPDATE requests SET documents=? WHERE id=?", `[{"id":"doc-a","title":"Plan","url":"","api_url":""}]`, n["request_id"]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.read("", "", "", nil); err == nil {
		t.Fatal("stale grant honoured")
	}
	for _, bad := range []map[string]any{{"id": "../x"}, {"id": ""}, {"id": 7}, {"id": "doc-a", "name": string(make([]byte, 201))}} {
		if _, err := f.s.Dispatch("block", bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	f.s.Close()
	f.s = f.open()
	if len(f.dispatch("blocked", nil)["documents"].([]any)) != 1 {
		t.Fatal("tag not durable")
	}
	f.dispatch("unblock", map[string]any{"id": "doc-a"})
	if len(f.dispatch("blocked", nil)["documents"].([]any)) != 0 {
		t.Fatal("unblock")
	}
	if flags := blockedFlags(); flags[0] || flags[1] {
		t.Fatal("flags after unblock")
	}
}

// Document write tests.

func (f *sharingFixture) writeRequest(access, call string) map[string]any {
	return f.dispatch("request", map[string]any{"chatID": "chat", "sandboxID": "sbx", "callID": call, "reason": "Plan", "title": "Plan", "access": access})
}

func (f *sharingFixture) writeGrant(access, call string) map[string]any {
	r := f.writeRequest(access, call)
	return f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc"}, "duration": 900})
}

func (f *sharingFixture) write(doc, chat string, edits []any, path string) error {
	if doc == "" {
		doc = "doc"
	}
	if chat == "" {
		chat = "chat"
	}
	if edits == nil {
		edits = []any{map[string]any{"insertText": map[string]any{"location": map[string]any{"index": 1}, "text": "Hello"}}}
	}
	if path == "" {
		path = "/v1/documents/" + doc + ":batchUpdate"
	}
	body := mustJSON(map[string]any{"requests": edits})
	_, _, err := f.s.Authorize(chat, "sbx", map[string]any{"scheme": "https", "port": 443, "host": "docs.googleapis.com", "method": "POST", "path": path, "body_base64": base64.StdEncoding.EncodeToString(body)})
	return err
}

func TestWriteGrantIsScopedAndRevocable(t *testing.T) {
	f := newSharingFixture(t)
	g := f.writeGrant("write", "1")
	if err := f.write("", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	// Write grants belong to the environment, not the requesting chat.
	if err := f.write("", "other", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.write("other", "", nil, ""); err == nil {
		t.Fatal("other doc")
	}
	if err := f.write("", "", nil, "/v1/documents"); err == nil {
		t.Fatal("list path")
	}
	if err := f.write("", "", nil, "/v1/documents/doc:batchUpdate?fields=*"); err == nil {
		t.Fatal("query")
	}
	if err := f.write("", "", []any{map[string]any{"insertInlineImage": map[string]any{"uri": "https://evil.test"}}}, ""); err == nil {
		t.Fatal("image edit")
	}
	f.clock.now = 1900
	if err := f.write("", "", nil, ""); err == nil {
		t.Fatal("expired")
	}
	f.clock.now = 1001
	f.dispatch("revoke", map[string]any{"id": g["request_id"]})
	if err := f.write("", "", nil, ""); err == nil {
		t.Fatal("revoked")
	}
}

func TestReadGrantNeverAuthorizesWrite(t *testing.T) {
	f := newSharingFixture(t)
	f.writeGrant("read", "1")
	if err := f.write("", "", nil, ""); err == nil {
		t.Fatal("read grant allowed write")
	}
}

func TestCreationRequiresApprovalAndIsIdempotent(t *testing.T) {
	f := newSharingFixture(t)
	r := f.writeRequest("create", "1")
	if len(f.google.createCalls) != 0 {
		t.Fatal("created before approval")
	}
	data := map[string]any{"id": r["request_id"], "allow": true, "duration": 900}
	g := f.dispatch("resolve", data)
	if g["documents"].([]any)[0].(map[string]any)["id"] != "new-doc" {
		t.Fatalf("grant: %v", g)
	}
	if err := f.write("new-doc", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	f.dispatch("resolve", data)
	if len(f.google.createCalls) != 1 || f.google.createCalls[0] != "Plan" {
		t.Fatalf("create calls: %v", f.google.createCalls)
	}
	if err := f.write("other", "", nil, ""); err == nil {
		t.Fatal("other document")
	}
}

func TestUncertainCreationIsNeverRetried(t *testing.T) {
	f := newSharingFixture(t)
	f.google.createErr = errors.New("timeout")
	r := f.writeRequest("create", "1")
	data := map[string]any{"id": r["request_id"], "allow": true, "duration": 900}
	if f.dispatch("resolve", data)["status"] != "failed" {
		t.Fatal("not failed")
	}
	f.dispatch("resolve", data)
	if len(f.google.createCalls) != 1 {
		t.Fatal("retried")
	}
}

func TestCreationRestartAndDenial(t *testing.T) {
	f := newSharingFixture(t)
	r := f.writeRequest("create", "1")
	if _, err := f.s.DB.Exec("UPDATE requests SET status='creating' WHERE id=?", r["request_id"]); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	f.s = f.open()
	if f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true})["status"] != "failed" {
		t.Fatal("creating not failed on restart")
	}
	r = f.writeRequest("create", "2")
	f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": false})
	if len(f.google.createCalls) != 0 {
		t.Fatal("denied creation executed")
	}
}

func TestOwnerSetupDoesNotLaunchAgentAndCanBeReadOnly(t *testing.T) {
	f := newSharingFixture(t)
	r := f.dispatch("select", map[string]any{"chatID": "codex", "sandboxID": "sbx", "access": "read", "documents": []any{"doc"}, "duration": 86400})
	if r["status"] != "granted" || len(f.dispatch("undelivered", nil)["requests"].([]any)) != 0 {
		t.Fatalf("select: %v", r)
	}
	if err := f.write("", "codex", nil, ""); err == nil {
		t.Fatal("read-only selection allowed write")
	}
}

// Images.

var testPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLttAAAAABJRU5ErkJggg==")

func TestImagesImmutableScopedAndPersistent(t *testing.T) {
	f := newSharingFixture(t)
	add := func() map[string]any {
		return f.dispatch("image_add", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "caption": "A screenshot", "png": base64.StdEncoding.EncodeToString(testPNG)})
	}
	r := add()
	if !jsonEqual(r, add()) {
		t.Fatal("not idempotent")
	}
	id := r["image_id"].(string)
	f.s.Close()
	f.s = f.open()
	img, err := f.s.Images.Get(id, "chat", "sandbox")
	if err != nil || string(img.PNG) != string(testPNG) {
		t.Fatal("image lost")
	}
	if _, err := f.s.Images.Get(id, "other", "sandbox"); err == nil {
		t.Fatal("other chat")
	}
	if _, err := f.s.Images.Get(id, "chat", "other"); err == nil {
		t.Fatal("other sandbox")
	}
	if _, err := f.s.DB.Exec("UPDATE images SET png=? WHERE id=?", []byte("changed"), id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Images.Get(id, "chat", "sandbox"); err == nil {
		t.Fatal("tampered image served")
	}
}

func TestImageLimits(t *testing.T) {
	f := newSharingFixture(t)
	big := append(append([]byte{}, pngMagic...), make([]byte, 4*1024*1024)...)
	for _, changes := range []map[string]any{{"png": base64.StdEncoding.EncodeToString([]byte("<svg/>"))}, {"caption": string(make([]byte, 501))}, {"png": base64.StdEncoding.EncodeToString(big)}} {
		data := map[string]any{"chatID": "chat", "sandboxID": "sandbox", "caption": "A screenshot", "png": base64.StdEncoding.EncodeToString(testPNG)}
		for k, v := range changes {
			data[k] = v
		}
		if _, err := f.s.Dispatch("image_add", data); err == nil {
			t.Fatalf("%v accepted", changes)
		}
	}
}

// Google connection scope tests.

func TestPublicChatCallbackIsHTTPSAndExact(t *testing.T) {
	g, err := NewGoogleConnection(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Configure("test.apps.googleusercontent.com", "test-secret", "https://warden.monaddle.com/oauth/google_docs/callback"); err != nil {
		t.Fatal(err)
	}
	if g.RedirectURI != "https://warden.monaddle.com/oauth/google_docs/callback" {
		t.Fatal("redirect")
	}
	for _, uri := range []string{"http://warden.monaddle.com/oauth/google_docs/callback", "https://user@warden.monaddle.com/oauth/google_docs/callback", "https://warden.monaddle.com/wrong", "https://warden.monaddle.com/oauth/google_docs/callback?x=1"} {
		if err := g.Configure("test.apps.googleusercontent.com", "test-secret", uri); err == nil {
			t.Fatalf("%s accepted", uri)
		}
	}
}

func TestLegacyReadConnectionSurvivesUpgrade(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	writePrivate(t, config, mustJSON(map[string]any{"client_id": "client.apps.googleusercontent.com", "client_secret": "synthetic-secret", "redirect_uri": "http://127.0.0.1:18781/oauth/google_docs/callback"}))
	db, err := sql.Open("sqlite", filepath.Join(root, "google.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE credentials (id INTEGER PRIMARY KEY, data TEXT)")
	db.Exec("INSERT INTO credentials VALUES (1,?)", string(mustJSON(map[string]any{"client_id": "client.apps.googleusercontent.com", "access_token": "synthetic-access-token", "refresh_token": "synthetic-refresh-token", "expires": 9999999999})))
	db.Close()
	c, err := NewGoogleConnection(root, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken == "" || c.CanWrite() {
		t.Fatal("legacy connection state")
	}
	if _, err := c.Create("Plan"); err == nil {
		t.Fatal("create without write scope")
	}
	scopes := "https://www.googleapis.com/auth/documents https://www.googleapis.com/auth/drive.metadata.readonly"
	if err := c.Install(map[string]any{"access_token": "synthetic-new-access-token", "refresh_token": "synthetic-new-refresh-token", "expires_in": 3600, "scope": scopes}, true); err != nil {
		t.Fatal(err)
	}
	if !c.CanWrite() {
		t.Fatal("write scope not recorded")
	}
	c.Close()
	c, err = NewGoogleConnection(root, config, nil)
	if err != nil || !c.CanWrite() {
		t.Fatal("write scope not durable")
	}
	c.Close()
	os.Chmod(config, 0o644)
	if _, err := NewGoogleConnection(root, config, nil); err == nil {
		t.Fatal("public config accepted")
	}
}

// Disconnecting a provider from the console forgets its credential and
// everything it backed: Google grants are revoked, GitHub repository
// selections are dropped, and status reports the provider as disconnected.
func TestDisconnectForgetsProviderAndRevokesWhatItBacked(t *testing.T) {
	f := newSharingFixture(t)
	g := f.grant()
	if _, err := f.s.Dispatch("disconnect", map[string]any{"provider": "figma"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if r := f.dispatch("disconnect", map[string]any{"provider": "google"}); r["ok"] != true || !f.google.disconnected {
		t.Fatalf("google disconnect: %v", r)
	}
	if f.dispatch("get", map[string]any{"id": g["request_id"], "chatID": "chat-a", "sandboxID": "sbx-a"})["status"] != "revoked" {
		t.Fatal("grant survived the disconnect")
	}
	if status := f.dispatch("status", nil); status["connected"] != false {
		t.Fatalf("status after disconnect: %v", status)
	}
	if _, err := f.s.Dispatch("disconnect", map[string]any{"provider": "google"}); err == nil {
		t.Fatal("second disconnect accepted")
	}
	// No GitHub provider: nothing to disconnect.
	if _, err := f.s.Dispatch("disconnect", map[string]any{"provider": "github"}); err == nil {
		t.Fatal("github disconnect without a provider accepted")
	}
}

func TestDisconnectGitHubUserTokenDeletesTheFileAndSelections(t *testing.T) {
	api := newUserAPI(t, 1)
	source, path := newUserSource(t, api)
	s, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status, _ := s.Dispatch("status", nil)
	github := status["github"].(map[string]any)
	if github["connected"] != true || github["login"] != "owner" || github["disconnectable"] != true || github["mode"] != "user" {
		t.Fatalf("status before: %v", github)
	}
	if _, ok := github["scopes"].([]any); !ok {
		t.Fatalf("scopes missing: %v", github)
	}
	if _, err = s.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo1"}}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Dispatch("disconnect", map[string]any{"provider": "github"}); err != nil || r["ok"] != true {
		t.Fatalf("disconnect: %v %v", r, err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file still present: %v", err)
	}
	status, _ = s.Dispatch("status", nil)
	if status["github"].(map[string]any)["connected"] != false {
		t.Fatalf("status after: %v", status["github"])
	}
	if list, err := s.Dispatch("github_list", map[string]any{"chatID": "c1", "sandboxID": "s1"}); err == nil {
		t.Fatalf("selection survived the disconnect: %v", list)
	}
	// The App broker cannot be disconnected from the UI.
	app, err := NewSharing(t.TempDir(), nil, nil, &GitHubAppCredentials{Owner: "org", AppID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err = app.Dispatch("disconnect", map[string]any{"provider": "github"}); err == nil {
		t.Fatal("app broker disconnect accepted")
	}
	if status, _ := app.Dispatch("status", nil); status["github"].(map[string]any)["disconnectable"] != false {
		t.Fatalf("app status: %v", status["github"])
	}
}

type fakeEgress struct{ mode, source string }

func (f *fakeEgress) EgressMode() (string, string) { return f.mode, f.source }
func (f *fakeEgress) SetEgressMode(mode string) error {
	f.mode, f.source = mode, "console"
	return nil
}

// The console speaks restricted/open; the policy document speaks
// restricted/public. The sharing operations translate both ways.
func TestEgressOperationsTranslateConsoleNames(t *testing.T) {
	f := newSharingFixture(t)
	if _, err := f.s.Dispatch("egress", nil); err == nil {
		t.Fatal("egress without a switch accepted")
	}
	f.s.Egress = &fakeEgress{mode: "restricted", source: "config"}
	if r := f.dispatch("egress", nil); r["mode"] != "restricted" || r["source"] != "config" {
		t.Fatalf("egress: %v", r)
	}
	if r := f.dispatch("egress_set", map[string]any{"mode": "open"}); r["mode"] != "open" || r["source"] != "console" {
		t.Fatalf("egress_set open: %v", r)
	}
	if f.s.Egress.(*fakeEgress).mode != "public" {
		t.Fatal("open not translated to the policy's public")
	}
	if _, err := f.s.Dispatch("egress_set", map[string]any{"mode": "public"}); err == nil {
		t.Fatal("policy vocabulary accepted from the console")
	}
	if r := f.dispatch("egress_set", map[string]any{"mode": "restricted"}); r["mode"] != "restricted" {
		t.Fatalf("egress_set restricted: %v", r)
	}
}

// The access history lists every document request for a sandbox with who
// resolved it and when, marks lapsed grants as expired, and records
// repository selections; expired grants are not dropped.
func TestAccessHistoryRecordsResolutionsAndExpiry(t *testing.T) {
	f := newSharingFixture(t)
	r := f.request(nil)
	granted := f.dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc-a"}, "duration": 900, "actor": "Ada Lovelace"})
	if granted["resolved_by"] != "Ada Lovelace" || granted["resolved_at"] != f.clock.wall() {
		t.Fatalf("resolution not recorded: %v", granted)
	}
	f.clock.now += 5
	denied := f.request(map[string]any{"callID": "tool2", "reason": "Also this one"})
	f.dispatch("resolve", map[string]any{"id": denied["request_id"], "allow": false, "actor": "bob@example.com"})
	history := f.dispatch("history", map[string]any{"sandboxID": "sbx-a"})["events"].([]any)
	if len(history) != 2 {
		t.Fatalf("history: %v", history)
	}
	newest := history[0].(map[string]any)
	if newest["status"] != "denied" || newest["resolved_by"] != "bob@example.com" || newest["kind"] != "document_request" || newest["expired"] != false {
		t.Fatalf("newest: %v", newest)
	}
	// The grant lapses: still granted, now flagged expired; a later revoke
	// by a person is recorded as such.
	f.clock.now += 1000
	history = f.dispatch("history", map[string]any{"sandboxID": "sbx-a"})["events"].([]any)
	if e := history[1].(map[string]any); e["status"] != "granted" || e["expired"] != true {
		t.Fatalf("expired grant: %v", e)
	}
	if _, err := f.s.Dispatch("history", map[string]any{"sandboxID": ""}); err == nil {
		t.Fatal("history without a sandbox accepted")
	}
	// Disconnecting Google revokes with a system actor and shows up too.
	f.clock.now -= 1000
	f.dispatch("disconnect", map[string]any{"provider": "google"})
	history = f.dispatch("history", map[string]any{"sandboxID": "sbx-a"})["events"].([]any)
	if e := history[1].(map[string]any); e["status"] != "revoked" || e["resolved_by"] != "Google disconnected" {
		t.Fatalf("revoked by disconnect: %v", e)
	}
}
