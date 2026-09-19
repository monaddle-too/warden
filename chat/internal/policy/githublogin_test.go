package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/login"
)

const freshToken = "gho_" + "0123456789abcdefghijklmnopqrstuvwxyz"

// fakeDeviceGitHub is GitHub's device-flow side for the console sign-in:
// the code endpoint, a scripted sequence of token poll answers and GET
// /user for the token they end in.
type fakeDeviceGitHub struct {
	mu     sync.Mutex
	polls  []map[string]any
	login  string
	seen   []string
	oauth  *httptest.Server
	api    *httptest.Server
	expire int
}

func newFakeDeviceGitHub(t *testing.T, login string, polls ...map[string]any) *fakeDeviceGitHub {
	f := &fakeDeviceGitHub{polls: polls, login: login, expire: 900}
	f.oauth = httptest.NewServer(http.HandlerFunc(f.serveOAuth))
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	t.Cleanup(f.oauth.Close)
	t.Cleanup(f.api.Close)
	return f
}

func (f *fakeDeviceGitHub) flow() *login.DeviceFlow {
	return &login.DeviceFlow{OAuthBase: f.oauth.URL, APIBase: f.api.URL, ClientID: "Iv1.warden-test-client", Client: f.oauth.Client(),
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
}

func (f *fakeDeviceGitHub) serveOAuth(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/login/device/code":
		json.NewEncoder(w).Encode(map[string]any{"device_code": "device-code-fixture", "user_code": "WXYZ-9876",
			"verification_uri": "https://github.com/login/device", "expires_in": f.expire, "interval": 1})
	case "/login/oauth/access_token":
		if r.PostForm.Get("device_code") != "device-code-fixture" || len(f.polls) == 0 {
			json.NewEncoder(w).Encode(map[string]any{"error": "incorrect_device_code"})
			return
		}
		next := f.polls[0]
		f.polls = f.polls[1:]
		json.NewEncoder(w).Encode(next)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeDeviceGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.URL.Path)
	if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+freshToken {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"login":"` + f.login + `"}`))
}

var granted = map[string]any{"access_token": freshToken, "token_type": "bearer", "scope": "repo,read:org"}

func signInFixture(t *testing.T, fake *fakeDeviceGitHub) (*Sharing, *GitHubUserCredentials, string) {
	t.Helper()
	api := newUserAPI(t, 2)
	source, path := newUserSource(t, api)
	s, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.SignIn.Flow = fake.flow()
	// The repository API fixture knows one token; the refreshed one counts
	// as the same credential there.
	direct := source.Transport
	source.Transport = func(method, path, token string) (int, []byte, error) {
		if token == freshToken {
			token = userToken
		}
		return direct(method, path, token)
	}
	return s, source, path
}

func waitSignIn(t *testing.T, s *Sharing, want string) map[string]any {
	t.Helper()
	if !s.SignIn.Wait(5 * time.Second) {
		t.Fatal("sign-in did not finish")
	}
	status, err := s.Dispatch("github_login_status", nil)
	if err != nil || status["status"] != want {
		t.Fatalf("status: %v %v", status, err)
	}
	return status
}

// The console signs in through the policy service: start returns the code
// to type at GitHub, the poller stores the confirmed token where the
// credential source reads, and status reports who signed in. A different
// account than before drops the repository selections; the same account
// keeps them.
func TestConsoleGitHubSignInStoresTheTokenAndReportsTheLogin(t *testing.T) {
	fake := newFakeDeviceGitHub(t, "octocat", map[string]any{"error": "authorization_pending"}, granted)
	s, source, path := signInFixture(t, fake)
	if _, err := s.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo1"}}); err != nil {
		t.Fatal(err)
	}
	if status, _ := s.Dispatch("github_login_status", nil); status["status"] != "none" {
		t.Fatalf("initial status: %v", status)
	}
	started, err := s.Dispatch("github_login_start", map[string]any{"actor": "owner"})
	if err != nil || started["status"] != "pending" || started["user_code"] != "WXYZ-9876" || started["verification_uri"] != "https://github.com/login/device" {
		t.Fatalf("start: %v %v", started, err)
	}
	if _, ok := started["expires_at"].(float64); !ok {
		t.Fatalf("expires_at missing: %v", started)
	}
	done := waitSignIn(t, s, "done")
	if done["login"] != "octocat" || done["user_code"] != nil {
		t.Fatalf("done: %v", done)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored file: %v %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	var stored map[string]any
	if json.Unmarshal(raw, &stored) != nil || stored["token"] != freshToken || stored["login"] != "octocat" {
		t.Fatalf("stored: %s", raw)
	}
	if scopes, _ := stored["scopes"].([]any); len(scopes) != 2 {
		t.Fatalf("scopes: %s", raw)
	}
	if owner, _, err := source.Identity(); err != nil || owner != "octocat" {
		t.Fatalf("identity: %q %v", owner, err)
	}
	status, _ := s.Dispatch("status", nil)
	if github := status["github"].(map[string]any); github["connected"] != true || github["login"] != "octocat" {
		t.Fatalf("sharing status: %v", github)
	}
	// "owner" signed out, "octocat" signed in: the selection is gone and
	// the history says so.
	if list, err := s.Dispatch("github_list", map[string]any{"chatID": "c1", "sandboxID": "s1"}); err != nil || len(list["repositories"].([]any)) != 0 {
		t.Fatalf("selection survived a sign-in as another account: %v %v", list, err)
	}
	history, _ := s.Dispatch("history", map[string]any{"sandboxID": "s1"})
	var signedIn map[string]any
	for _, e := range history["events"].([]any) {
		if e := e.(map[string]any); e["kind"] == "github_signed_in" {
			signedIn = e
		}
	}
	if signedIn == nil || signedIn["resolved_by"] != "owner" {
		t.Fatalf("history: %v", history)
	}
	if detail := signedIn["repositories"].(map[string]any); detail["login"] != "octocat" || detail["previous"] != "owner" {
		t.Fatalf("event detail: %v", detail)
	}
	// The token itself never appears in a status or the history.
	for _, m := range []map[string]any{started, done, status, history} {
		if strings.Contains(string(mustJSON(m)), freshToken) {
			t.Fatalf("token leaked: %v", m)
		}
	}
	// Once "octocat" is stored, GitHub identifies a refreshed token as the
	// same person, so the sign-in keeps the (new) selection.
	fake.mu.Lock()
	fake.polls = []map[string]any{granted}
	fake.mu.Unlock()
	if _, err := s.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dispatch("github_login_start", map[string]any{"actor": "owner"}); err != nil {
		t.Fatal(err)
	}
	waitSignIn(t, s, "done")
	if list, err := s.Dispatch("github_list", map[string]any{"chatID": "c1", "sandboxID": "s1"}); err != nil || len(list["repositories"].([]any)) != 1 {
		t.Fatalf("selection after a refresh: %v %v", list, err)
	}
}

// A sign-in works with no stored token at all (the first sign-in on a
// fresh install), and Save refuses a malformed record.
func TestConsoleGitHubSignInFromNothing(t *testing.T) {
	fake := newFakeDeviceGitHub(t, "octocat", granted)
	s, source, path := signInFixture(t, fake)
	os.Remove(path)
	if source.Available() {
		t.Fatal("source available without a file")
	}
	if _, err := s.Dispatch("github_login_start", nil); err != nil {
		t.Fatal(err)
	}
	waitSignIn(t, s, "done")
	if !source.Available() {
		t.Fatal("sign-in not stored")
	}
	if err := source.Save(login.GitHubFile{Token: "nope", Login: "octocat"}); err == nil {
		t.Fatal("malformed record saved")
	}
}

// Refusal, cancellation, expiry and a missing client ID all leave a
// status the console can show; a pending attempt is returned again rather
// than duplicated; the App broker cannot be signed in to.
func TestConsoleGitHubSignInFailures(t *testing.T) {
	fake := newFakeDeviceGitHub(t, "octocat", map[string]any{"error": "access_denied"})
	s, source, _ := signInFixture(t, fake)
	if _, err := s.Dispatch("github_login_start", nil); err != nil {
		t.Fatal(err)
	}
	failed := waitSignIn(t, s, "failed")
	if failed["error"] != "the GitHub sign-in was cancelled" {
		t.Fatalf("refusal: %v", failed)
	}
	if owner, _, err := source.Identity(); err != nil || owner != "owner" {
		t.Fatalf("stored sign-in changed after a refusal: %q %v", owner, err)
	}
	// Pending: a second start returns the same code; cancel forgets it.
	fake.mu.Lock()
	fake.polls = []map[string]any{{"error": "authorization_pending"}, {"error": "authorization_pending"}, {"error": "authorization_pending"}}
	fake.mu.Unlock()
	s.SignIn.Flow.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return nil
		}
	}
	first, err := s.Dispatch("github_login_start", nil)
	if err != nil || first["status"] != "pending" {
		t.Fatalf("start: %v %v", first, err)
	}
	again, err := s.Dispatch("github_login_start", nil)
	if err != nil || again["user_code"] != first["user_code"] {
		t.Fatalf("second start: %v %v", again, err)
	}
	s.SignIn.mu.Lock()
	polling := s.SignIn.done
	s.SignIn.mu.Unlock()
	if r, err := s.Dispatch("github_login_cancel", nil); err != nil || r["ok"] != true {
		t.Fatalf("cancel: %v %v", r, err)
	}
	if status, _ := s.Dispatch("github_login_status", nil); status["status"] != "none" {
		t.Fatalf("after cancel: %v", status)
	}
	<-polling // the cancelled poller is done with the flow before it changes
	// An expired code reads as failed as soon as the clock passes it, even
	// while the poller is still waiting.
	clock := &testClock{now: 1000}
	s.SignIn.Clock = clock.wall
	s.SignIn.Flow.Sleep = func(ctx context.Context, d time.Duration) error { <-ctx.Done(); return ctx.Err() }
	if _, err := s.Dispatch("github_login_start", nil); err != nil {
		t.Fatal(err)
	}
	clock.now += 901
	if status, _ := s.Dispatch("github_login_status", nil); status["status"] != "failed" || !strings.Contains(status["error"].(string), "expired") {
		t.Fatalf("expired: %v", status)
	}
	// A start after expiry begins a fresh attempt rather than returning
	// the stale code.
	if fresh, err := s.Dispatch("github_login_start", nil); err != nil || fresh["status"] != "pending" {
		t.Fatalf("restart after expiry: %v %v", fresh, err)
	}
	s.SignIn.Cancel()
	// No client ID in the release: nothing to start.
	s.SignIn.Flow = &login.DeviceFlow{OAuthBase: fake.oauth.URL, APIBase: fake.api.URL, Client: fake.oauth.Client()}
	if _, err := s.Dispatch("github_login_start", nil); !errors.Is(err, login.ErrNoClientID) {
		t.Fatalf("without a client ID: %v", err)
	}
	// The App broker is the operator's.
	app, err := NewSharing(t.TempDir(), nil, nil, &GitHubAppCredentials{Owner: "org", AppID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, op := range []string{"github_login_start", "github_login_status", "github_login_cancel"} {
		if _, err := app.Dispatch(op, nil); err == nil {
			t.Fatalf("%s accepted for the App broker", op)
		}
	}
	none, err := NewSharing(t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer none.Close()
	if _, err := none.Dispatch("github_login_start", nil); err == nil {
		t.Fatal("sign-in without a GitHub provider accepted")
	}
}

// memoryStore is a CredentialStore standing in for the Kubernetes Secret.
type memoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memoryStore) Load(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[name]; ok {
		return v, nil
	}
	return nil, errors.New("missing")
}
func (m *memoryStore) Store(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[name] = data
	return nil
}
func (m *memoryStore) Watch(context.Context, string) (<-chan struct{}, error) {
	return nil, errors.New("unused")
}

// In the kubernetes kind the sign-in lands in the Secret through the
// store, and a disconnect empties it (the store cannot delete a key), so
// every later use fails closed.
func TestConsoleGitHubSignInThroughTheCredentialStore(t *testing.T) {
	store := &memoryStore{data: map[string][]byte{}}
	source := NewGitHubUserCredentialsFrom(store, "warden-logins/github.json", NewRedactor(), nil)
	s, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SignIn.Flow = newFakeDeviceGitHub(t, "octocat", granted).flow()
	if _, err := s.Dispatch("github_login_start", nil); err != nil {
		t.Fatal(err)
	}
	waitSignIn(t, s, "done")
	if owner, _, err := source.Identity(); err != nil || owner != "octocat" {
		t.Fatalf("identity: %q %v", owner, err)
	}
	if _, err := s.Dispatch("disconnect", map[string]any{"provider": "github"}); err != nil {
		t.Fatal(err)
	}
	if source.Available() {
		t.Fatal("still available after a disconnect")
	}
	if _, _, err := source.Identity(); err == nil || err.Error() != GitHubRefreshMessage {
		t.Fatalf("after disconnect: %v", err)
	}
}
