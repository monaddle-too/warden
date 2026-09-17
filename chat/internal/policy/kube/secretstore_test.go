package kube

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	api "warden/chat/internal/kube"
	"warden/chat/internal/policy"
)

func seedSecret(f *fakeAPI, name string, data map[string][]byte) {
	f.seed(api.Secrets, testCore, &api.Secret{Metadata: api.ObjectMeta{Name: name}, Type: "Opaque", Data: data})
}

func TestSecretLoad(t *testing.T) {
	f := newFakeAPI(t)
	seedSecret(f, "warden-codex-login", map[string][]byte{CodexAuthKey: []byte(`{"auth_mode":"chatgpt"}`)})
	s := &SecretCredentials{Client: f.client(), Namespace: testCore}
	ctx := ctxT(t)
	data, err := s.Load(ctx, CredentialName("warden-codex-login", CodexAuthKey))
	if err != nil || string(data) != `{"auth_mode":"chatgpt"}` {
		t.Fatalf("%s %v", data, err)
	}
	for name, want := range map[string]string{
		"warden-codex-login/claude.json":  "has no key claude.json",
		"warden-claude-login/claude.json": "does not exist",
		"nokey":                           "must be <secret>/<key>",
		"a/b/c":                           "must be <secret>/<key>",
	} {
		if _, err := s.Load(ctx, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.Limit = 8
	if _, err := s.Load(ctx, "warden-codex-login/auth.json"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("limit: %v", err)
	}
	// The store serves the existing loaders unchanged.
	var _ policy.CredentialStore = s
}

func TestSecretStoreWritesBackAndRetriesConflicts(t *testing.T) {
	f := newFakeAPI(t)
	seedSecret(f, "warden-codex-login", map[string][]byte{CodexAuthKey: []byte("old"), "other": []byte("kept")})
	s := &SecretCredentials{Client: f.client(), Namespace: testCore}
	ctx := ctxT(t)
	if err := s.Store(ctx, "warden-codex-login/auth.json", []byte("new")); err != nil {
		t.Fatal(err)
	}
	var sec api.Secret
	f.object(api.Secrets, testCore, "warden-codex-login", &sec)
	if string(sec.Data[CodexAuthKey]) != "new" || string(sec.Data["other"]) != "kept" {
		t.Fatalf("stored: %v", sec.Data)
	}
	puts := f.recorded()
	last := puts[len(puts)-1]
	if last.Method != http.MethodPut || !strings.Contains(string(last.Body), `"resourceVersion"`) {
		t.Fatalf("store did not update conditionally: %s %s", last.Method, last.Body)
	}
	// Never creates.
	if err := s.Store(ctx, "warden-new-login/auth.json", []byte("x")); err == nil || !strings.Contains(err.Error(), "does not exist") || f.count("POST", "/secrets") != 0 {
		t.Fatalf("store created a Secret: %v", err)
	}
	// A conflict (another writer) is retried from a fresh read.
	conflicts := 0
	f.mu.Lock()
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && conflicts < 1 {
			conflicts++
			writeStatus(w, http.StatusConflict, "Conflict", "the object has been modified")
			return true
		}
		return false
	}
	f.mu.Unlock()
	if err := s.Store(ctx, "warden-codex-login/auth.json", []byte("newer")); err != nil || conflicts != 1 {
		t.Fatalf("%v %d", err, conflicts)
	}
	var after api.Secret
	f.object(api.Secrets, testCore, "warden-codex-login", &after)
	if string(after.Data[CodexAuthKey]) != "newer" {
		t.Fatal("not stored after the retry")
	}
	if err := s.Store(ctx, "warden-codex-login/auth.json", make([]byte, 2*1024*1024)); err == nil {
		t.Fatal("oversized credential stored")
	}
}

func TestSecretWatchSignalsChanges(t *testing.T) {
	f := newFakeAPI(t)
	seedSecret(f, "warden-codex-login", map[string][]byte{CodexAuthKey: []byte("v1")})
	seedSecret(f, "warden-claude-login", map[string][]byte{ClaudeAuthKey: []byte("c1")})
	s := &SecretCredentials{Client: f.client(), Namespace: testCore}
	ctx, cancel := context.WithCancel(ctxT(t))
	changes, err := s.Watch(ctx, "warden-codex-login/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	expectNone := func(what string) {
		t.Helper()
		select {
		case <-changes:
			t.Fatalf("%s signalled", what)
		case <-time.After(150 * time.Millisecond):
		}
	}
	expectOne := func(what string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s not signalled", what)
		}
	}
	expectNone("initial state")
	// The watch is on the named Secret only.
	if reqs := f.recorded(); !strings.Contains(reqs[len(reqs)-1].Query.Get("fieldSelector"), "metadata.name=warden-codex-login") {
		t.Fatalf("watch not by name: %v", reqs[len(reqs)-1].Query)
	}
	// Another key, another Secret: nothing.
	var codex api.Secret
	f.object(api.Secrets, testCore, "warden-codex-login", &codex)
	codex.Data["unrelated"] = []byte("x")
	f.put(api.Secrets, testCore, &codex)
	var claude api.Secret
	f.object(api.Secrets, testCore, "warden-claude-login", &claude)
	claude.Data[ClaudeAuthKey] = []byte("c2")
	f.put(api.Secrets, testCore, &claude)
	expectNone("unrelated changes")
	// A rotation of the key: one signal, coalesced.
	if err := s.Store(ctx, "warden-codex-login/auth.json", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	expectOne("rotation")
	expectNone("second signal for one change")
	// Removal and re-creation signal too.
	f.remove(api.Secrets, testCore, "warden-codex-login")
	expectOne("deletion")
	seedSecret(f, "warden-codex-login", map[string][]byte{CodexAuthKey: []byte("v3")})
	var back api.Secret
	f.object(api.Secrets, testCore, "warden-codex-login", &back)
	f.put(api.Secrets, testCore, &back)
	expectOne("re-creation")
	cancel()
	select {
	case _, open := <-changes:
		if open {
			// A signal may still be buffered; the next receive must close.
			if _, open = <-changes; open {
				t.Fatal("channel not closed")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

// The store plugs into the existing loaders: a Codex login in a Secret is
// read on every use and a refreshed login written back through Store.
func TestSecretStoreServesTheLoaders(t *testing.T) {
	f := newFakeAPI(t)
	seedSecret(f, "warden-claude-login", map[string][]byte{ClaudeAuthKey: []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-testtokenvalue","expiresAt":` + "4102444800000" + `}}`)})
	s := &SecretCredentials{Client: f.client(), Namespace: testCore}
	claude := &policy.ClaudeCredentials{Store: s, Name: CredentialName("warden-claude-login", ClaudeAuthKey)}
	if !claude.Available() {
		t.Fatal("Claude login in a Secret not available")
	}
	headers, err := claude.Headers()
	if err != nil || headers["Authorization"] != "Bearer sk-ant-oat01-testtokenvalue" {
		t.Fatalf("%v %v", headers, err)
	}
	f.remove(api.Secrets, testCore, "warden-claude-login")
	if claude.Available() {
		t.Fatal("removed Secret still available")
	}
}
