package edge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"warden/chat/internal/browserauth"
)

type fakeAuth struct {
	mu     sync.Mutex
	active bool
}

func (a *fakeAuth) Role(r *http.Request) string {
	_, ok := a.SessionRef(r)
	if ok {
		return "admin"
	}
	return ""
}
func (a *fakeAuth) Handler() http.Handler { return http.NotFoundHandler() }
func (a *fakeAuth) Identity(r *http.Request) (string, string, string, bool) {
	if _, ok := a.SessionRef(r); !ok {
		return "", "", "", false
	}
	return "google-subject", "owner@gmail.com", "Owner Person", true
}
func (a *fakeAuth) SessionRef(r *http.Request) (string, bool) {
	c, err := r.Cookie("main")
	return "parent", err == nil && c.Value == "valid" && a.ActiveSession("parent")
}
func (a *fakeAuth) ActiveSession(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active && id == "parent"
}
func testServer(t *testing.T, handler http.Handler) (*Server, *fakeAuth) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	path := filepath.Join(t.TempDir(), "owner")
	os.WriteFile(path, []byte(strings.Repeat("s", 64)), 0600)
	s, err := New(Config{Origin: "https://warden.example.com", PreviewSuffix: "preview.example.com", ClientID: "test-client", OwnerEmails: "owner@gmail.com", Upstream: up.URL, UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAuth{active: true}
	s.Auth = a
	s.bindings[strings.Repeat("a", 32)] = true
	s.bindings[strings.Repeat("b", 32)] = true
	s.lastRefresh = time.Now()
	return s, a
}
func invoke(s *Server, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	return out
}
func cookie(response *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range response.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
func loginPreview(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	id := strings.Repeat("a", 32)
	first := invoke(s, "https://"+id+".preview.example.com/assets/")
	if first.Code != 303 {
		t.Fatal(first.Code)
	}
	challenge := cookie(first, challengeCookie)
	if challenge == nil || !challenge.HttpOnly || !challenge.Secure || challenge.Domain != "" {
		t.Fatal("preview challenge is not host scoped")
	}
	auth := invoke(s, first.Header().Get("Location"), &http.Cookie{Name: "main", Value: "valid"})
	if auth.Code != 303 {
		t.Fatal(auth.Body.String())
	}
	callback := auth.Header().Get("Location")
	done := invoke(s, callback, challenge)
	if done.Code != 303 || done.Header().Get("Location") != "/assets/" {
		t.Fatal(done.Code, done.Header())
	}
	if replay := invoke(s, callback, challenge); replay.Code != 401 {
		t.Fatal("ticket replay accepted")
	}
	return cookie(done, previewCookie)
}
func TestSignedOutAndUnknownHostsNeverReachSandbox(t *testing.T) {
	count := 0
	s, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++ }))
	id := strings.Repeat("a", 32)
	if r := invoke(s, "https://"+id+".preview.example.com/"); r.Code != 303 {
		t.Fatal(r.Code)
	}
	if r := invoke(s, "https://warden.example.com/api/state"); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := invoke(s, "https://evil.example.com/"); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if count != 0 {
		t.Fatal("unauthenticated request reached sandbox")
	}
}
func TestSessionExchangeIsolationLogoutAndHeaderStripping(t *testing.T) {
	seen := make(chan *http.Request, 4)
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		w.Header().Set("Set-Cookie", "untrusted=yes")
		io.WriteString(w, "counter")
	}))
	session := loginPreview(t, s)
	if session == nil || !session.HttpOnly || !session.Secure || session.Domain != "" {
		t.Fatal("preview session is not host scoped")
	}
	id := strings.Repeat("a", 32)
	r := httptest.NewRequest("GET", "https://"+id+".preview.example.com/assets/app.js?x=1", nil)
	r.AddCookie(session)
	r.Header.Set("Authorization", "attacker")
	r.Header.Set("Cookie", r.Header.Get("Cookie")+"; secret=browser")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 || out.Body.String() != "counter" || out.Header().Get("Set-Cookie") != "" {
		t.Fatal(out.Code, out.Header())
	}
	up := <-seen
	if up.Header.Get("Cookie") != "" || up.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 64) || up.URL.Path != "/api/ports/"+id+"/proxy/assets/app.js" || up.URL.RawQuery != "x=1" {
		t.Fatal("incorrect or leaking proxy request")
	}
	if r := invoke(s, "https://"+strings.Repeat("b", 32)+".preview.example.com/", session); r.Code != 303 {
		t.Fatal("cross-binding session accepted")
	}
	a.mu.Lock()
	a.active = false
	a.mu.Unlock()
	if r := invoke(s, "https://"+id+".preview.example.com/", session); r.Code != 303 {
		t.Fatal("logged-out session accepted")
	}
}
func TestPreviewCSRFAndTicketBinding(t *testing.T) {
	s, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("invalid request reached sandbox") }))
	session := loginPreview(t, s)
	id := strings.Repeat("a", 32)
	r := httptest.NewRequest("POST", "https://"+id+".preview.example.com/change", strings.NewReader("x"))
	r.AddCookie(session)
	r.Header.Set("Origin", "https://evil.example.com")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("cross-origin write accepted")
	}
	first := invoke(s, "https://"+id+".preview.example.com/")
	auth := invoke(s, first.Header().Get("Location"), &http.Cookie{Name: "main", Value: "valid"})
	if r := invoke(s, auth.Header().Get("Location"), &http.Cookie{Name: challengeCookie, Value: "wrong"}); r.Code != 401 {
		t.Fatal("unbound login ticket accepted")
	}
}
func TestTLSAllowlistAndStaleInventory(t *testing.T) {
	s, _ := testServer(t, http.NotFoundHandler())
	id := strings.Repeat("a", 32)
	path := "http://127.0.0.1/_tls/allow?domain=" + url.QueryEscape(id+".preview.example.com")
	if r := invoke(s, path); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if r := invoke(s, "http://127.0.0.1/_tls/allow?domain=evil.example.com"); r.Code != 403 {
		t.Fatal("unknown certificate authorized")
	}
	s.lastRefresh = time.Now().Add(-time.Minute)
	if r := invoke(s, path); r.Code != 403 {
		t.Fatal("stale inventory authorized a certificate")
	}
}
func TestLogoutCancelsActivePreviewStream(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	session := loginPreview(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		invoke(s, "https://"+strings.Repeat("a", 32)+".preview.example.com/", session)
	}()
	<-started
	a.mu.Lock()
	a.active = false
	a.mu.Unlock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("logout did not close preview stream")
	}
	<-done
}

func TestEndpointFileRotation(t *testing.T) {
	s, _ := testServer(t, http.NotFoundHandler())
	for _, token := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if err := os.WriteFile(s.Config.OwnerTokenFile, []byte(`{"url":"http://127.0.0.1:18780","token":"`+token+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := s.token()
		if err != nil || got != token {
			t.Fatal("chat restart credential was not read", err)
		}
	}
	os.WriteFile(s.Config.OwnerTokenFile, []byte(`{"token":"short"}`), 0600)
	if _, err := s.token(); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestGuestContentCannotUseMainOrigin(t *testing.T) {
	reached := false
	s, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	out := invoke(s, "https://warden.example.com/api/ports/"+strings.Repeat("a", 32)+"/proxy/", &http.Cookie{Name: "main", Value: "valid"})
	if out.Code != 403 || reached {
		t.Fatal("guest content reachable on privileged origin")
	}
}

type demoAuth struct{ *fakeAuth }

func (a demoAuth) Role(r *http.Request) string {
	if a.fakeAuth.Role(r) != "" {
		return "demo"
	}
	return ""
}
func TestDemoSharesWorkspaceAndGrantsButCannotConnectGoogle(t *testing.T) {
	count := 0
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; w.WriteHeader(204) }))
	s.Auth = demoAuth{a}
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/state", 204}, {"POST", "/api/chats", 204},
		{"GET", "/api/sharing/files", 204}, {"POST", "/api/sharing/select", 204},
		{"POST", "/api/sharing/resolve", 204}, {"POST", "/api/sharing/revoke", 204},
		{"POST", "/api/sharing/connect", 403}, {"POST", "/api/sharing/connect/", 403}, {"POST", "/api/sharing/disconnect", 403},
		{"POST", "/api/sharing/github_login_start", 403}, {"GET", "/api/sharing/github_login_status", 403}, {"POST", "/api/sharing/github_login_cancel", 403},
		{"GET", "/oauth/google_docs/callback?state=test&code=test", 403},
	} {
		before := count
		r := httptest.NewRequest(tc.method, "https://warden.example.com"+tc.path, nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if tc.code == 403 && count != before {
			t.Fatal("forbidden connection reached upstream")
		}
	}
	s.Auth = a
	for _, p := range []string{"/api/sharing/connect", "/oauth/google_docs/callback?state=test&code=test"} {
		method := "POST"
		if strings.HasPrefix(p, "/oauth/") {
			method = "GET"
		}
		r := httptest.NewRequest(method, "https://warden.example.com"+p, nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("owner blocked: %s %d", p, w.Code)
		}
	}
}
func TestLoginLedgerPersistsAndAdminConsoleIsOwnerOnly(t *testing.T) {
	count := 0
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; w.WriteHeader(204) }))
	path := filepath.Join(t.TempDir(), "logins.json")
	logins, err := newLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	s.logins = logins
	first := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	logins.record(browserauth.Login{User: browserauth.User{Subject: "owner-sub", Email: "Owner@gmail.com", Role: "admin"}, At: first})
	logins.record(browserauth.Login{User: browserauth.User{Subject: "emp-sub", Email: "employee@digitalasset.com", Role: "demo"}, HostedDomain: "digitalasset.com", At: first.Add(time.Hour)})
	logins.record(browserauth.Login{User: browserauth.User{Subject: "owner-sub", Email: "owner@gmail.com", Role: "admin"}, At: first.Add(2 * time.Hour)})
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ledger must be a private file", err)
	}
	reloaded, err := newLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	users := reloaded.snapshot()
	if len(users) != 2 || users[0].Email != "owner@gmail.com" || users[0].Logins != 2 || !users[0].FirstLogin.Equal(first) || !users[0].LastLogin.Equal(first.Add(2*time.Hour)) || users[0].Role != "admin" {
		t.Fatalf("unexpected owner record: %+v", users)
	}
	if users[1].Email != "employee@digitalasset.com" || users[1].Logins != 1 || users[1].HostedDomain != "digitalasset.com" || users[1].Role != "demo" {
		t.Fatalf("unexpected demo record: %+v", users[1])
	}
	for _, tc := range []struct {
		auth   Authenticator
		method string
		path   string
		code   int
	}{
		{demoAuth{a}, "GET", "/api/admin/users", 403}, {demoAuth{a}, "POST", "/api/sharing/block", 403}, {demoAuth{a}, "POST", "/api/sharing/unblock/", 403},
		{demoAuth{a}, "GET", "/api/sharing/blocked", 204}, {demoAuth{a}, "GET", "/api/sharing/files", 204},
		{a, "GET", "/api/admin/users", 200}, {a, "POST", "/api/admin/users", 404}, {a, "GET", "/api/admin/other", 404},
		{a, "POST", "/api/sharing/block", 204}, {a, "POST", "/api/sharing/unblock", 204},
		{demoAuth{a}, "POST", "/api/sharing/disconnect", 403}, {a, "POST", "/api/sharing/disconnect", 204},
		{demoAuth{a}, "POST", "/api/sharing/egress_set", 403}, {a, "POST", "/api/sharing/egress_set", 204}, {demoAuth{a}, "GET", "/api/sharing/egress", 204},
		{demoAuth{a}, "GET", "/api/cluster", 403}, {demoAuth{a}, "GET", "/api/cluster/logs?pod=x", 403}, {a, "GET", "/api/cluster", 204},
		{demoAuth{a}, "POST", "/api/environments/ws1/network", 403}, {a, "POST", "/api/environments/ws1/network", 204}, {demoAuth{a}, "POST", "/api/environments/ws1/resize", 204},
		{demoAuth{a}, "GET", "/api/spend", 403}, {a, "GET", "/api/spend", 204},
	} {
		s.Auth = tc.auth
		before := count
		r := httptest.NewRequest(tc.method, "https://warden.example.com"+tc.path, nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if strings.HasPrefix(tc.path, "/api/admin/") && count != before {
			t.Fatal("admin console request reached upstream")
		}
		if tc.code == 403 && count != before {
			t.Fatal("forbidden owner operation reached upstream")
		}
		if tc.code == 200 && !strings.Contains(w.Body.String(), `"employee@digitalasset.com"`) {
			t.Fatalf("admin listing missing users: %s", w.Body.String())
		}
	}
	if signedOut := invoke(s, "https://warden.example.com/api/admin/users"); signedOut.Code != 403 {
		t.Fatal(signedOut.Code)
	}
}

// loopbackServer is the auth.mode "owner", previews.mode "loopback" shape:
// plain HTTP on 127.0.0.1:18781, previews on <binding>.localhost:18781, the
// owner identified by the chat's launcher capability.
func loopbackServer(t *testing.T, handler http.Handler) (*Server, string) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	path := filepath.Join(t.TempDir(), "endpoint.json")
	token := strings.Repeat("t", 64)
	os.WriteFile(path, []byte(`{"url":"http://127.0.0.1:18780","token":"`+token+`"}`), 0600)
	s, err := New(Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: up.URL, UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path, Listen: "127.0.0.1:18781"})
	if err != nil {
		t.Fatal(err)
	}
	s.bindings[strings.Repeat("a", 32)] = true
	s.bindings[strings.Repeat("b", 32)] = true
	s.lastRefresh = time.Now()
	return s, token
}

// bearer is a capability-authenticated call as the web app's fetch makes
// it: browsers add fetch metadata, which is what earns a cookie session.
func bearer(s *Server, method, target, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	return out
}

// ownerCookieFor mints the owner's cookie session the way the web app does:
// its first capability-authenticated API call.
func ownerCookieFor(t *testing.T, s *Server, token string) *http.Cookie {
	t.Helper()
	out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", token)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	c := cookie(out, s.Auth.(*ownerAuth).cookie)
	if c == nil || !c.HttpOnly || c.Secure || c.Domain != "" {
		t.Fatal("owner session cookie missing or not host scoped over loopback HTTP")
	}
	return c
}
func loginLoopbackPreview(t *testing.T, s *Server, owner *http.Cookie) *http.Cookie {
	t.Helper()
	id := strings.Repeat("a", 32)
	first := invoke(s, "http://"+id+".localhost:18781/assets/")
	if first.Code != 303 || !strings.HasPrefix(first.Header().Get("Location"), "http://127.0.0.1:18781/auth/preview?binding="+id) {
		t.Fatal(first.Code, first.Header())
	}
	challenge := cookie(first, "warden-preview-challenge")
	if challenge == nil || !challenge.HttpOnly || challenge.Secure || challenge.Domain != "" {
		t.Fatal("preview challenge is not host scoped")
	}
	auth := invoke(s, first.Header().Get("Location"), owner)
	if auth.Code != 303 || !strings.HasPrefix(auth.Header().Get("Location"), "http://"+id+".localhost:18781/_warden/login?code=") {
		t.Fatal(auth.Code, auth.Body.String(), auth.Header())
	}
	callback := auth.Header().Get("Location")
	done := invoke(s, callback, challenge)
	if done.Code != 303 || done.Header().Get("Location") != "/assets/" {
		t.Fatal(done.Code, done.Header())
	}
	if replay := invoke(s, callback, challenge); replay.Code != 401 {
		t.Fatal("ticket replay accepted")
	}
	session := cookie(done, "warden-preview")
	if session == nil || !session.HttpOnly || session.Secure || session.Domain != "" {
		t.Fatal("preview session is not host scoped")
	}
	return session
}

func TestLoopbackModeConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoint.json")
	os.WriteFile(path, []byte(strings.Repeat("t", 64)), 0600)
	base := Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: "http://127.0.0.1:18780", UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path, Listen: "127.0.0.1:18781"}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"public suffix in loopback mode": func(c *Config) { c.PreviewSuffix = "preview.example.com" },
		"non-loopback origin":            func(c *Config) { c.Origin = "http://192.168.1.2:18781" },
		"origin without port":            func(c *Config) { c.Origin = "http://127.0.0.1" },
		"listen on another port":         func(c *Config) { c.Listen = "127.0.0.1:19081" },
		"google client in owner mode":    func(c *Config) { c.ClientID = "x" },
		"http origin with google mode":   func(c *Config) { c.Mode = ModeGoogle; c.ClientID = "x"; c.OwnerEmails = "o@gmail.com" },
		"https origin with owner mode":   func(c *Config) { c.Origin = "https://warden.example.com" },
	} {
		c := base
		change(&c)
		if _, err := New(c); err == nil {
			t.Fatal("accepted:", name)
		}
	}
}

// The signed-out reply names which Warden this is (docs/host-dogfood-plan.md
// Part A): a loopback owner install reports its instance and build so a
// browser can tell `inner` from the default install, especially when the
// page is opened through another Warden's preview proxy. The same settings
// in public mode reveal nothing: the Google authenticator is never given
// them, so an anonymous visitor cannot read the version off a sign-in page.
func TestSignedOutSessionNamesTheInstanceInOwnerModeOnly(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	path := filepath.Join(t.TempDir(), "endpoint.json")
	os.WriteFile(path, []byte(`{"url":"http://127.0.0.1:18780","token":"`+strings.Repeat("t", 64)+`"}`), 0600)
	owner, err := New(Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: up.URL, UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path, Listen: "127.0.0.1:18781", InstanceName: "inner", InstanceVersion: "v0.0.0-dev.abc123"})
	if err != nil {
		t.Fatal(err)
	}
	r := invoke(owner, "http://127.0.0.1:18781/auth/session")
	var body struct {
		Enabled  bool `json:"enabled"`
		Instance *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"instance"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &body) != nil {
		t.Fatal("owner session reply unreadable", r.Code, r.Body.String())
	}
	if body.Enabled || body.Instance == nil || body.Instance.Name != "inner" || body.Instance.Version != "v0.0.0-dev.abc123" {
		t.Fatal("owner mode must name the instance and its build:", r.Body.String())
	}
	// An install that names neither says only that sign-in is disabled.
	plain, _ := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if got := invoke(plain, "http://127.0.0.1:18781/auth/session").Body.String(); strings.Contains(got, "instance") {
		t.Fatal("nothing configured, nothing reported:", got)
	}
	public, err := New(Config{Origin: "https://warden.example.com", PreviewSuffix: "preview.example.com", ClientID: "test-client", OwnerEmails: "owner@gmail.com", Upstream: up.URL, UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path, InstanceName: "inner", InstanceVersion: "v0.0.0-dev.abc123"})
	if err != nil {
		t.Fatal(err)
	}
	got := invoke(public, "https://warden.example.com/auth/session").Body.String()
	if strings.Contains(got, "inner") || strings.Contains(got, "v0.0.0-dev.abc123") || strings.Contains(got, "instance") {
		t.Fatal("a public install must not reveal its build to an anonymous visitor:", got)
	}
}

func TestLoopbackSignedOutAndUnknownHostsNeverReachSandbox(t *testing.T) {
	count := 0
	s, _ := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++ }))
	id := strings.Repeat("a", 32)
	if r := invoke(s, "http://"+id+".localhost:18781/"); r.Code != 303 {
		t.Fatal(r.Code)
	}
	if r := invoke(s, "http://127.0.0.1:18781/api/state"); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := bearer(s, "GET", "http://127.0.0.1:18781/api/state", strings.Repeat("x", 64)); r.Code != 401 {
		t.Fatal("wrong capability accepted", r.Code)
	}
	for _, host := range []string{id + ".localhost:19999", id + ".localhost", "evil.localhost:18781", id + ".localhost.example.com:18781", "localhost:18781"} {
		if r := invoke(s, "http://"+host+"/"); r.Code != 403 {
			t.Fatal(host, r.Code)
		}
	}
	if count != 0 {
		t.Fatal("unauthenticated request reached sandbox")
	}
}

func TestLoopbackOwnerSessionTicketIsolationAndRevocation(t *testing.T) {
	seen := make(chan *http.Request, 8)
	s, token := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		w.Header().Set("Set-Cookie", "untrusted=yes")
		io.WriteString(w, "counter")
	}))
	if r := invoke(s, "http://127.0.0.1:18781/auth/session"); r.Code != 200 || !strings.Contains(r.Body.String(), `"enabled":false`) {
		t.Fatal("owner mode must report browser sign-in disabled", r.Code, r.Body.String())
	}
	owner := ownerCookieFor(t, s, token)
	<-seen
	if csp := bearer(s, "GET", "http://127.0.0.1:18781/", token).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src http://*.localhost:18781;") || strings.Contains(csp, "google") {
		t.Fatal(csp)
	}
	<-seen
	// The owner cookie authenticates reads only; writes need the capability.
	r := httptest.NewRequest("POST", "http://127.0.0.1:18781/api/chats", strings.NewReader("{}"))
	r.AddCookie(owner)
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 401 {
		t.Fatal("cookie-only write accepted", out.Code)
	}
	session := loginLoopbackPreview(t, s, owner)
	id := strings.Repeat("a", 32)
	r = httptest.NewRequest("GET", "http://"+id+".localhost:18781/assets/app.js?x=1", nil)
	r.AddCookie(session)
	r.Header.Set("Authorization", "attacker")
	r.Header.Set("Cookie", r.Header.Get("Cookie")+"; secret=browser")
	out = httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 || out.Body.String() != "counter" || out.Header().Get("Set-Cookie") != "" || out.Header().Get("Content-Security-Policy") != "frame-ancestors http://127.0.0.1:18781" {
		t.Fatal(out.Code, out.Header())
	}
	up := <-seen
	if up.Header.Get("Cookie") != "" || up.Header.Get("Authorization") != "Bearer "+token || up.URL.Path != "/api/ports/"+id+"/proxy/assets/app.js" || up.URL.RawQuery != "x=1" || up.Host != "127.0.0.1:18780" {
		t.Fatal("incorrect or leaking proxy request")
	}
	// Writes must come from the preview's own origin.
	r = httptest.NewRequest("POST", "http://"+id+".localhost:18781/change", strings.NewReader("x"))
	r.AddCookie(session)
	r.Header.Set("Origin", "http://127.0.0.1:18781")
	out = httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("cross-origin write accepted", out.Code)
	}
	if r := invoke(s, "http://"+strings.Repeat("b", 32)+".localhost:18781/", session); r.Code != 303 {
		t.Fatal("cross-binding session accepted")
	}
	// Revocation: the binding leaves the approved inventory.
	s.mu.Lock()
	delete(s.bindings, id)
	s.mu.Unlock()
	if r := invoke(s, "http://"+id+".localhost:18781/", session); r.Code != 410 {
		t.Fatal("revoked binding served", r.Code)
	}
	s.mu.Lock()
	s.bindings[id] = true
	s.mu.Unlock()
	// A chat restart rotates the capability; every session minted from the
	// old one ends, for the app and for previews.
	os.WriteFile(s.Config.OwnerTokenFile, []byte(`{"url":"http://127.0.0.1:18780","token":"`+strings.Repeat("u", 64)+`"}`), 0600)
	if r := invoke(s, "http://"+id+".localhost:18781/", session); r.Code != 303 {
		t.Fatal("preview session outlived the capability", r.Code)
	}
	if r := invoke(s, "http://127.0.0.1:18781/api/state", owner); r.Code != 401 {
		t.Fatal("owner session outlived the capability", r.Code)
	}
	if r := bearer(s, "GET", "http://127.0.0.1:18781/api/state", token); r.Code != 401 {
		t.Fatal("old capability accepted", r.Code)
	}
}

func TestLoopbackRevocationCancelsActivePreviewStream(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	s, token := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/ports/") {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	session := loginLoopbackPreview(t, s, ownerCookieFor(t, s, token))
	done := make(chan struct{})
	go func() {
		defer close(done)
		invoke(s, "http://"+strings.Repeat("a", 32)+".localhost:18781/events", session)
	}()
	<-started
	s.mu.Lock()
	delete(s.bindings, strings.Repeat("a", 32))
	s.mu.Unlock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not close preview stream")
	}
	<-done
}

// The edge tells the chat who is behind each request with headers of its
// own, after discarding any the client sent.
func TestProxyForwardsIdentityHeadersItOwns(t *testing.T) {
	var seen http.Header
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone(); w.WriteHeader(204) }))
	r := httptest.NewRequest("GET", "https://warden.example.com/api/state", nil)
	r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
	r.Header.Set(HeaderPrincipal, "forged")
	r.Header.Set(HeaderName, "Forged Name")
	r.Header.Set(HeaderRole, "admin")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
	if seen.Get(HeaderPrincipal) != "google-subject" || seen.Get(HeaderEmail) != "owner@gmail.com" || seen.Get(HeaderName) != "Owner Person" {
		t.Fatalf("identity headers: %v", seen)
	}
	// The owner's role travels too; an admitted person's forged copy does
	// not.
	if seen.Get(HeaderRole) != "admin" {
		t.Fatalf("owner role: %v", seen)
	}
	s.Auth = demoAuth{a}
	r = httptest.NewRequest("GET", "https://warden.example.com/api/state", nil)
	r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
	r.Header.Set(HeaderRole, "admin")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 204 || seen.Get(HeaderRole) != "" {
		t.Fatalf("admitted person: %d %v", w.Code, seen)
	}
}

// The owner cookie exists for the browser alone (the preview ticket flow
// starts with a navigation that carries no bearer), so clients that ignore
// Set-Cookie must not consume sessions: a curl loop, the TUI or the e2e
// suite once filled the 64-session bound within its 8-hour life and the
// web app then got no cookie at all, which made every preview link land on
// a signed-out tab.
func TestLoopbackOwnerSessionIsMintedForBrowsersOnlyAndNeverRefused(t *testing.T) {
	s, token := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	r := httptest.NewRequest("GET", "http://127.0.0.1:18781/api/state", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 || cookie(out, s.Auth.(*ownerAuth).cookie) != nil {
		t.Fatal("a request without fetch metadata (curl, the TUI) minted a cookie session", out.Code, out.Header())
	}
	var first *http.Cookie
	for i := 0; i < 200; i++ {
		c := ownerCookieFor(t, s, token)
		if i == 0 {
			first = c
		}
	}
	if r := invoke(s, "http://127.0.0.1:18781/api/state", first); r.Code != 401 {
		t.Fatal("the bound no longer evicts the oldest session", r.Code)
	}
	if r := invoke(s, "http://127.0.0.1:18781/api/state", ownerCookieFor(t, s, token)); r.Code != 200 {
		t.Fatal("a browser was refused a session", r.Code)
	}
}

// Browsers scope host-only cookies by host, never by port, so a local
// `warden start` on 127.0.0.1:18781 and a port-forwarded cluster edge on
// 127.0.0.1:28781 would otherwise overwrite each other's owner cookie at
// every API call and take turns signing the other out of previews.
func TestLoopbackOwnerCookieNameCarriesThePort(t *testing.T) {
	s, token := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	if c := ownerCookieFor(t, s, token); c.Name != "warden-owner-18781" {
		t.Fatal(c.Name)
	}
	other := &http.Cookie{Name: "warden-owner", Value: "from-the-other-warden"}
	if r := bearer(s, "GET", "http://127.0.0.1:18781/api/state", token, other); r.Code != 200 || cookie(r, "warden-owner-18781") == nil || cookie(r, "warden-owner") != nil {
		t.Fatal("another Warden's cookie on the same address must be ignored, not overwritten", r.Code, r.Header())
	}
}

// Proxied requests share one upstream transport: their keep-alive
// connections to the chat service are reused, not left one per request
// in a pool nothing reads again.
func TestProxyReusesUpstreamConnections(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]bool{}
	s, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr] = true
		mu.Unlock()
		w.WriteHeader(204)
	}))
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "https://warden.example.com/api/state", nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(remotes) > 2 {
		t.Fatalf("20 sequential requests used %d upstream connections", len(remotes))
	}
}
