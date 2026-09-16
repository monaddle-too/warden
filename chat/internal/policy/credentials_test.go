package policy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixtureJWT(exp int) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp})
	return "fixture." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeCodex(t *testing.T, path, token string) {
	t.Helper()
	document := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": token, "account_id": "fixture-account",
		"refresh_token": "never-export-this-refresh-token", "id_token": "never-export-id-token"}}
	writePrivate(t, path, mustJSON(document))
}

func TestCodexFixedRouteAndOnlyRequiredHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodex(t, path, fixtureJWT(500))
	provider := &CodexCredentials{Path: path, Clock: func() float64 { return 100 }}
	if !provider.Available() {
		t.Fatal("unavailable")
	}
	host, route, err := provider.Route("/v1/responses")
	if err != nil || host != "chatgpt.com" || route != "/backend-api/codex/responses" {
		t.Fatalf("route: %s %s %v", host, route, err)
	}
	headers, err := provider.Headers()
	if err != nil || headers["Authorization"] != "Bearer "+fixtureJWT(500) || headers["ChatGPT-Account-ID"] != "fixture-account" || len(headers) != 2 {
		t.Fatalf("headers: %v %v", headers, err)
	}
	for _, p := range []string{"/v1/chat/completions", "/v1/responses?target=other", "//evil/responses"} {
		if _, _, err := provider.Route(p); err == nil {
			t.Fatalf("%s accepted", p)
		}
	}
}

func TestCodexRefreshIsReadWithoutModifyingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodex(t, path, fixtureJWT(900))
	before, _ := os.ReadFile(path)
	provider := &CodexCredentials{Path: path, Clock: func() float64 { return 100 }}
	headers, _ := provider.Headers()
	after, _ := os.ReadFile(path)
	if headers["Authorization"] != "Bearer "+fixtureJWT(900) || string(before) != string(after) {
		t.Fatal("source modified or wrong token")
	}
}

func TestCodexExpiredMalformedPublicAndSymlinkFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	provider := &CodexCredentials{Path: path, Clock: func() float64 { return 100 }}
	for _, token := range []string{fixtureJWT(110), "not-a-token", "secret\r\nInjected:yes"} {
		writeCodex(t, path, token)
		if provider.Available() {
			t.Fatalf("%q accepted", token)
		}
	}
	writeCodex(t, path, fixtureJWT(500))
	os.Chmod(path, 0o644)
	if provider.Available() {
		t.Fatal("public file accepted")
	}
	target := filepath.Join(dir, "actual.json")
	os.Rename(path, target)
	os.Chmod(target, 0o600)
	os.Symlink(target, path)
	if provider.Available() {
		t.Fatal("symlink accepted")
	}
}

func TestCodexAccountHeaderCannotBeInjected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	document := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": fixtureJWT(500), "account_id": "fixture\r\nInjected: yes"}}
	writePrivate(t, path, mustJSON(document))
	if (&CodexCredentials{Path: path, Clock: func() float64 { return 100 }}).Available() {
		t.Fatal("injected account accepted")
	}
	writePrivate(t, path, []byte("[]"))
	if (&CodexCredentials{Path: path, Clock: func() float64 { return 100 }}).Available() {
		t.Fatal("array accepted")
	}
	writeCodex(t, path, "fixture.W10.signature")
	if (&CodexCredentials{Path: path, Clock: func() float64 { return 100 }}).Available() {
		t.Fatal("non-object claims accepted")
	}
}

func TestClaudePrivateExpiringAccessTokenOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	document := map[string]any{"claudeAiOauth": map[string]any{"accessToken": "fixture-access-token-long", "refreshToken": "never-export-refresh", "expiresAt": 500000}}
	writePrivate(t, path, mustJSON(document))
	source := &ClaudeCredentials{Path: path, Clock: func() float64 { return 100 }}
	if !source.Available() {
		t.Fatal("unavailable")
	}
	headers, _ := source.Headers()
	if headers["Authorization"] != "Bearer fixture-access-token-long" || len(headers) != 1 {
		t.Fatalf("headers: %v", headers)
	}
	host, _, err := source.Route("/v1/messages")
	if err != nil || host != "api.anthropic.com" {
		t.Fatal("route")
	}
	if _, _, err := source.Route("/v1/responses"); err == nil {
		t.Fatal("codex route accepted")
	}
	for _, data := range []any{[]any{}, map[string]any{"claudeAiOauth": []any{}}, map[string]any{"claudeAiOauth": map[string]any{"accessToken": "fixture-access-token-long", "expiresAt": 100001}}} {
		writePrivate(t, path, mustJSON(data))
		if source.Available() {
			t.Fatalf("%v accepted", data)
		}
	}
	writePrivate(t, path, mustJSON(document))
	os.Chmod(path, 0o644)
	if source.Available() {
		t.Fatal("public accepted")
	}
}
