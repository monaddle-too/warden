package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileCredentialsLoadKeepsThePrivateFileRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	store := FileCredentials{Description: "Codex credential file"}
	if _, err := store.Load(context.Background(), path); err == nil {
		t.Fatal("missing file loaded")
	}
	writePrivate(t, path, []byte(`{"ok":true}`))
	raw, err := store.Load(context.Background(), path)
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("load: %s %v", raw, err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background(), path); err == nil || err.Error() != "Codex credential file must be private and owned" {
		t.Fatalf("public file: %v", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background(), link); err == nil {
		t.Fatal("symlink followed")
	}
	if _, err = (FileCredentials{Limit: 4}).Load(context.Background(), path); err == nil {
		t.Fatal("oversized file loaded")
	}
	if _, err = store.Load(context.Background(), "relative/auth.json"); err == nil {
		t.Fatal("relative name accepted")
	}
}

func TestFileCredentialsStoreIsAtomicPrivateAndRefusesPublicTargets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	store := FileCredentials{Description: "Codex credential file"}
	if err := store.Store(context.Background(), path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("stored file: %v %v", info, err)
	}
	if _, err = os.Lstat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	if err = store.Store(context.Background(), path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Load(context.Background(), path)
	if err != nil || string(raw) != "second" {
		t.Fatalf("after rewrite: %s %v", raw, err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = store.Store(context.Background(), path, []byte("third")); err == nil {
		t.Fatal("public file overwritten")
	}
	if err = (FileCredentials{Limit: 2}).Store(context.Background(), filepath.Join(dir, "small"), []byte("big")); err == nil {
		t.Fatal("oversized credential stored")
	}
}

func TestFileCredentialsWatchSignalsRotationsUntilCancelled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	store := FileCredentials{PollInterval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	changes, err := store.Watch(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(what string) {
		t.Helper()
		select {
		case _, ok := <-changes:
			if !ok {
				t.Fatal(what + ": channel closed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal(what + ": no signal")
		}
	}
	writePrivate(t, path, []byte("first"))
	wait("file appeared")
	// A rotation through Store (rename) changes the inode even when size
	// and mtime resolution would not tell the versions apart.
	if err = store.Store(context.Background(), path, []byte("secnd")); err != nil {
		t.Fatal(err)
	}
	wait("file rotated")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	wait("file removed")
	cancel()
	select {
	case _, ok := <-changes:
		for ok {
			_, ok = <-changes
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

// fakeStore is a CredentialStore held in memory, as a Secret store would.
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (f *fakeStore) Load(_ context.Context, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.data[name]
	if !ok {
		return nil, errors.New("no such credential")
	}
	return raw, nil
}
func (f *fakeStore) Store(_ context.Context, name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[name] = data
	return nil
}
func (f *fakeStore) Watch(context.Context, string) (<-chan struct{}, error) {
	return make(chan struct{}), nil
}

// The provider loaders read through any store: the same document shape
// and expiry rules, and a login that appears in the store later is used.
func TestProviderLoadersReadThroughACredentialStore(t *testing.T) {
	store := &fakeStore{data: map[string][]byte{}}
	codex := &CodexCredentials{Store: store, Name: "warden-codex-login", Clock: func() float64 { return 100 }}
	if codex.Available() {
		t.Fatal("codex available before the secret exists")
	}
	document := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": fixtureJWT(500), "account_id": "fixture-account"}}
	store.data["warden-codex-login"] = mustJSON(document)
	headers, err := codex.Headers()
	if err != nil || headers["Authorization"] != "Bearer "+fixtureJWT(500) || headers["ChatGPT-Account-ID"] != "fixture-account" {
		t.Fatalf("codex: %v %v", headers, err)
	}
	claude := &ClaudeCredentials{Store: store, Name: "warden-claude-login", Clock: func() float64 { return 100 }}
	store.data["warden-claude-login"] = mustJSON(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat01-fixture-token-value", "expiresAt": 900000}})
	if headers, err = claude.Headers(); err != nil || headers["Authorization"] != "Bearer sk-ant-oat01-fixture-token-value" {
		t.Fatalf("claude: %v %v", headers, err)
	}
	github := NewGitHubUserCredentialsFrom(store, "warden-github-login", nil, nil)
	if _, err = github.read(); err == nil || !strings.Contains(err.Error(), "Refresh the GitHub sign-in") {
		t.Fatalf("github before login: %v", err)
	}
	store.data["warden-github-login"] = mustJSON(map[string]any{"token": "gho_" + strings.Repeat("a", 36), "login": "octocat"})
	file, err := github.read()
	if err != nil || file.Login != "octocat" {
		t.Fatalf("github: %+v %v", file, err)
	}
	// The file-backed loaders behave identically through the file store.
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodex(t, path, fixtureJWT(500))
	fromFile := &CodexCredentials{Store: FileCredentials{Description: "Codex credential file"}, Name: path, Clock: func() float64 { return 100 }}
	byPath := &CodexCredentials{Path: path, Clock: func() float64 { return 100 }}
	a, errA := fromFile.Headers()
	b, errB := byPath.Headers()
	if errA != nil || errB != nil || a["Authorization"] != b["Authorization"] {
		t.Fatalf("file store and path disagree: %v %v %v %v", a, errA, b, errB)
	}
}
