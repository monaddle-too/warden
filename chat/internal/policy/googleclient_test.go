package policy

import (
	"strings"
	"testing"
)

const builtinGoogleID = "123456-warden.apps.googleusercontent.com"

func TestBuiltinGoogleClientUsesChatPortLoopbackRedirect(t *testing.T) {
	g, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{ChatListen: "127.0.0.1:18780", BuiltinClientID: builtinGoogleID, BuiltinClientSecret: "GOCSPX-desktop-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !g.Configured() || g.Connected() || g.ClientID != builtinGoogleID || g.RedirectURI != "http://127.0.0.1:18780/oauth/google_docs/callback" {
		t.Fatalf("configured %v connected %v client %q redirect %q", g.Configured(), g.Connected(), g.ClientID, g.RedirectURI)
	}
	if !contains(g.Redactor.Secrets(), "GOCSPX-desktop-secret") {
		t.Fatal("built-in secret not redacted")
	}
	u, err := g.Start()
	if err != nil || !strings.Contains(u, "redirect_uri=http%3A%2F%2F127.0.0.1%3A18780%2Foauth%2Fgoogle_docs%2Fcallback") || !strings.Contains(u, "client_id="+strings.ReplaceAll(builtinGoogleID, ".", "%2E")) && !strings.Contains(u, "client_id="+builtinGoogleID) {
		t.Fatalf("start: %s %v", u, err)
	}
	// Any loopback-only listen spelling still yields the 127.0.0.1 callback
	// that warden-chat's Host check accepts; a hostless port is refused.
	g2, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{ChatListen: "localhost:9000", BuiltinClientID: builtinGoogleID, BuiltinClientSecret: "GOCSPX-desktop-secret"}, nil)
	if err != nil || g2.RedirectURI != "http://127.0.0.1:9000/oauth/google_docs/callback" {
		t.Fatalf("localhost listen: %v %v", err, g2)
	}
	g2.Close()
	for _, listen := range []string{"18780", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:x"} {
		if _, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{ChatListen: listen, BuiltinClientID: builtinGoogleID, BuiltinClientSecret: "GOCSPX-desktop-secret"}, nil); err == nil {
			t.Fatalf("%q accepted", listen)
		}
	}
}

func TestGoogleUnconfiguredWithoutBuiltinClientOrFile(t *testing.T) {
	// The release constant is empty until the Desktop client is registered:
	// the connection reports not configured and the UI hides it.
	g, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{ChatListen: "127.0.0.1:18780"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Configured() || g.Connected() {
		t.Fatal("configured without a client")
	}
	if _, err := g.Start(); err == nil {
		t.Fatal("start without a client")
	}
	s, err := NewSharing(t.TempDir(), g, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status, _ := s.Dispatch("status", nil)
	if status["configured"] != false || status["connected"] != false {
		t.Fatalf("status: %v", status)
	}
	// Without a chat listener the built-in client is not used either.
	g3, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{BuiltinClientID: builtinGoogleID, BuiltinClientSecret: "GOCSPX-desktop-secret"}, nil)
	if err != nil || g3.Configured() {
		t.Fatalf("configured without a listener: %v", err)
	}
	g3.Close()
}

func TestOperatorGoogleFileOverridesBuiltinClient(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/google.json"
	writePrivate(t, file, mustJSON(map[string]any{"client_id": "operator.apps.googleusercontent.com", "client_secret": "operator-secret", "redirect_uri": "https://warden.example.com/oauth/google_docs/callback"}))
	g, err := NewGoogleConnectionWithClient(dir, GoogleClientOptions{ConfigFile: file, ChatListen: "127.0.0.1:18780", BuiltinClientID: builtinGoogleID, BuiltinClientSecret: "GOCSPX-desktop-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.ClientID != "operator.apps.googleusercontent.com" || g.RedirectURI != "https://warden.example.com/oauth/google_docs/callback" {
		t.Fatalf("file did not win: %q %q", g.ClientID, g.RedirectURI)
	}
}
