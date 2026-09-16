package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "gho_" + "abcdefghijklmnopqrstuvwxyz0123456789"

// fakeGitHub serves the device code endpoint, a scripted sequence of token
// poll responses and GET /user, recording what it saw.
type fakeGitHub struct {
	t        *testing.T
	oauth    *httptest.Server
	api      *httptest.Server
	mu       sync.Mutex
	polls    []map[string]any // scripted responses, consumed in order
	seen     []string
	interval int
	expires  int
	userCode int // HTTP status for GET /user (default 200)
	sleeps   []time.Duration
}

func newFakeGitHub(t *testing.T, polls ...map[string]any) *fakeGitHub {
	f := &fakeGitHub{t: t, polls: polls, interval: 1, expires: 900}
	f.oauth = httptest.NewServer(http.HandlerFunc(f.serveOAuth))
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	t.Cleanup(f.oauth.Close)
	t.Cleanup(f.api.Close)
	oldOAuth, oldAPI, oldClient, oldSleep, oldClientID := githubOAuthBase, githubAPIBase, httpClient, sleep, githubClientID
	githubOAuthBase, githubAPIBase, httpClient = f.oauth.URL, f.api.URL, f.oauth.Client()
	githubClientID = "Iv1.warden-test-client"
	sleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.sleeps = append(f.sleeps, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() {
		githubOAuthBase, githubAPIBase, httpClient, sleep, githubClientID = oldOAuth, oldAPI, oldClient, oldSleep, oldClientID
	})
	return f
}

func (f *fakeGitHub) serveOAuth(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.URL.Path)
	if r.Method != "POST" || r.Header.Get("Accept") != "application/json" || r.PostForm.Get("client_id") != "Iv1.warden-test-client" {
		w.WriteHeader(400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/login/device/code":
		if r.PostForm.Get("scope") != "repo read:org" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "device-code-40-characters-long-fixture-000", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device", "expires_in": f.expires, "interval": f.interval})
	case "/login/oauth/access_token":
		if r.PostForm.Get("grant_type") != deviceGrant || r.PostForm.Get("device_code") != "device-code-40-characters-long-fixture-000" {
			json.NewEncoder(w).Encode(map[string]any{"error": "incorrect_device_code"})
			return
		}
		if len(f.polls) == 0 {
			json.NewEncoder(w).Encode(map[string]any{"error": "unexpected_extra_poll"})
			return
		}
		next := f.polls[0]
		f.polls = f.polls[1:]
		json.NewEncoder(w).Encode(next)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.URL.Path)
	if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	if f.userCode != 0 {
		w.WriteHeader(f.userCode)
		w.Write([]byte(`{}`))
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"login":"octocat","id":1}`))
}

func readFile(t *testing.T, path string) GitHubFile {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f GitHubFile
	if err = json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	json.Unmarshal(data, &generic)
	for _, key := range []string{"token", "login", "scopes", "obtained"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("missing %s in %s", key, data)
		}
	}
	return f
}

func TestGitHubDeviceFlowPendingSlowDownThenSuccess(t *testing.T) {
	f := newFakeGitHub(t,
		map[string]any{"error": "authorization_pending"},
		map[string]any{"error": "slow_down", "interval": 6},
		map[string]any{"error": "authorization_pending"},
		map[string]any{"access_token": testToken, "token_type": "bearer", "scope": "repo,read:org"},
	)
	var out bytes.Buffer
	authFile := filepath.Join(t.TempDir(), "provider", "github.json")
	if err := GitHub(context.Background(), &out, authFile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://github.com/login/device") || !strings.Contains(out.String(), "ABCD-1234") || !strings.Contains(out.String(), "octocat") {
		t.Fatalf("output: %s", out.String())
	}
	record := readFile(t, authFile)
	if record.Token != testToken || record.Login != "octocat" || strings.Join(record.Scopes, " ") != "repo read:org" || record.Obtained < time.Now().Unix()-60 {
		t.Fatalf("record: %+v", record)
	}
	dir, _ := os.Lstat(filepath.Dir(authFile))
	if dir.Mode().Perm() != 0o700 {
		t.Fatalf("provider directory mode %v", dir.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Dir(authFile)); len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
	// Interval 1s, then 6s after slow_down (server value), kept afterwards.
	f.mu.Lock()
	sleeps := append([]time.Duration{}, f.sleeps...)
	seen := append([]string{}, f.seen...)
	f.mu.Unlock()
	if len(sleeps) != 4 || sleeps[0] != time.Second || sleeps[1] != time.Second || sleeps[2] != 6*time.Second || sleeps[3] != 6*time.Second {
		t.Fatalf("sleeps: %v", sleeps)
	}
	if seen[len(seen)-1] != "/user" || strings.Count(strings.Join(seen, " "), "/login/oauth/access_token") != 4 {
		t.Fatalf("seen: %v", seen)
	}
}

func TestGitHubDeviceFlowSlowDownWithoutIntervalAddsFiveSeconds(t *testing.T) {
	f := newFakeGitHub(t,
		map[string]any{"error": "slow_down"},
		map[string]any{"access_token": testToken, "token_type": "bearer", "scope": ""},
	)
	f.interval = 5
	authFile := filepath.Join(t.TempDir(), "github.json")
	if err := GitHub(context.Background(), &bytes.Buffer{}, authFile); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	sleeps := append([]time.Duration{}, f.sleeps...)
	f.mu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second || sleeps[1] != 10*time.Second {
		t.Fatalf("sleeps: %v", sleeps)
	}
	// An empty token scope falls back to the scopes GitHub reports on /user.
	if record := readFile(t, authFile); strings.Join(record.Scopes, " ") != "repo read:org" {
		t.Fatalf("scopes: %v", record.Scopes)
	}
}

func TestGitHubDeviceFlowFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		polls   []map[string]any
		message string
	}{
		"expired":   {[]map[string]any{{"error": "authorization_pending"}, {"error": "expired_token"}}, "expired"},
		"denied":    {[]map[string]any{{"error": "access_denied"}}, "cancelled"},
		"other":     {[]map[string]any{{"error": "unsupported_grant_type"}}, "unsupported_grant_type"},
		"bad token": {[]map[string]any{{"access_token": "ghs_installation-token-shape-0000000000", "token_type": "bearer"}}, "unexpected token"},
	} {
		newFakeGitHub(t, tc.polls...)
		authFile := filepath.Join(t.TempDir(), "github.json")
		err := GitHub(context.Background(), &bytes.Buffer{}, authFile)
		if err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, statErr := os.Lstat(authFile); statErr == nil {
			t.Fatalf("%s: file written", name)
		}
	}
	// A device code that outlives expires_in stops polling.
	f := newFakeGitHub(t, map[string]any{"error": "authorization_pending"}, map[string]any{"error": "authorization_pending"})
	f.expires = 1
	sleep = func(ctx context.Context, d time.Duration) error { time.Sleep(1100 * time.Millisecond); return nil }
	if err := GitHub(context.Background(), &bytes.Buffer{}, filepath.Join(t.TempDir(), "github.json")); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("local expiry: %v", err)
	}
	// Cancellation stops the wait.
	newFakeGitHub(t, map[string]any{"error": "authorization_pending"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := GitHub(ctx, &bytes.Buffer{}, filepath.Join(t.TempDir(), "github.json")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestGitHubRequiresClientIDInRelease(t *testing.T) {
	f := newFakeGitHub(t)
	githubClientID = ""
	err := GitHub(context.Background(), &bytes.Buffer{}, filepath.Join(t.TempDir(), "github.json"))
	if err == nil || !strings.Contains(err.Error(), "not configured in this release") {
		t.Fatalf("empty client ID: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) != 0 {
		t.Fatal("GitHub reached without a client ID")
	}
}

func TestGitHubPasteValidatesShapeAndVerifiesToken(t *testing.T) {
	f := newFakeGitHub(t)
	authFile := filepath.Join(t.TempDir(), "github.json")
	for _, bad := range []string{"", "ghs_" + strings.Repeat("a", 40), "gho_short", "ghp_" + strings.Repeat("a", 20) + " x", "Bearer " + testToken} {
		if err := GitHubPaste(bad, authFile); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	f.mu.Lock()
	if len(f.seen) != 0 {
		t.Fatal("GitHub reached for a malformed token")
	}
	f.mu.Unlock()
	if err := GitHubPaste("gho_"+strings.Repeat("r", 36), authFile); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := os.Lstat(authFile); err == nil {
		t.Fatal("file written for a rejected token")
	}
	if err := GitHubPaste("  "+testToken+"\n", authFile); err != nil {
		t.Fatal(err)
	}
	record := readFile(t, authFile)
	if record.Token != testToken || record.Login != "octocat" || strings.Join(record.Scopes, " ") != "repo read:org" {
		t.Fatalf("record: %+v", record)
	}
	// Replacing an existing file keeps 0600 even with a permissive umask.
	os.Chmod(authFile, 0o644)
	if err := GitHubPaste(testToken, authFile); err != nil {
		t.Fatal(err)
	}
	readFile(t, authFile)
	// The client ID is not needed for a pasted token.
	githubClientID = ""
	if err := GitHubPaste(testToken, authFile); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubOpensTheBrowserAndCopiesTheCode(t *testing.T) {
	newFakeGitHub(t, map[string]any{"access_token": testToken, "token_type": "bearer", "scope": "repo"})
	var opened, copied string
	OpenBrowser = func(u string) error { opened = u; return nil }
	CopyToClipboard = func(s string) error { copied = s; return nil }
	defer func() { OpenBrowser, CopyToClipboard = nil, nil }()
	var out bytes.Buffer
	if err := GitHub(context.Background(), &out, filepath.Join(t.TempDir(), "github.json")); err != nil {
		t.Fatal(err)
	}
	if opened != "https://github.com/login/device" || copied != "ABCD-1234" {
		t.Fatalf("opened %q copied %q", opened, copied)
	}
	if !strings.Contains(out.String(), "on your clipboard") || !strings.Contains(out.String(), "ABCD-1234") {
		t.Fatalf("output: %s", out.String())
	}
	// A browser that cannot be opened falls back to the printed instructions.
	OpenBrowser = func(string) error { return errors.New("no display") }
	CopyToClipboard = nil
	newFakeGitHub(t, map[string]any{"access_token": testToken, "token_type": "bearer", "scope": "repo"})
	out.Reset()
	if err := GitHub(context.Background(), &out, filepath.Join(t.TempDir(), "github.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Open https://github.com/login/device in a browser and enter the code ABCD-1234") {
		t.Fatalf("fallback output: %s", out.String())
	}
}
