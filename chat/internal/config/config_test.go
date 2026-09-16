package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValidLocalMode(t *testing.T) {
	c := Defaults("/tmp/w")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Previews.Mode != PreviewLoopback || c.Auth.Mode != AuthOwner || c.PreviewScheme() != "http" {
		t.Fatalf("unexpected local defaults: %+v", c)
	}
	if c.GitHubMode() != "user" || c.Providers.Google.DocsClient != BuiltinGoogleClient {
		t.Fatalf("unexpected provider defaults: %+v", c.Providers)
	}
	if c.PolicySocket() != filepath.Join("/tmp/w", "policy", "sbx-control.sock") {
		t.Fatal(c.PolicySocket())
	}
}

func TestParseMergesAndRejectsUnknownFields(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"maxRunning":4}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandboxes.MaxRunning != 4 || c.Sandboxes.MemoryMB != 1536 {
		t.Fatalf("merge failed: %+v", c.Sandboxes)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"bogus":1}`)); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if _, err = Parse([]byte(`{"version":2,"paths":{"state":"/tmp/w"}}`)); err == nil {
		t.Fatal("wrong version accepted")
	}
}

func TestServerModeRules(t *testing.T) {
	server := `{"version":1,"paths":{"state":"/var/lib/warden"},
	 "previews":{"mode":"public","hostSuffix":"preview.example.com","edgeListen":"172.18.0.1:19081"},
	 "auth":{"mode":"google","publicURL":"https://warden.example.com","google":{"signInClientID":"x.apps.googleusercontent.com","owners":["o@example.com"]}},
	 "providers":{"github":{"appID":5,"appSlug":"s","installationOwner":"org","brokerFile":"/var/lib/warden/github/broker.json"}}}`
	c, err := Parse([]byte(server))
	if err != nil {
		t.Fatal(err)
	}
	if c.PreviewScheme() != "https" || c.GitHubMode() != "app" || c.Auth.Google.SignInLedger == "" {
		t.Fatalf("server parse: %+v", c)
	}
	bad := []string{
		// public previews without google auth
		`{"version":1,"paths":{"state":"/tmp/w"},"previews":{"mode":"public","hostSuffix":"p.example.com","edgeListen":"127.0.0.1:1"}}`,
		// loopback previews with a real suffix
		`{"version":1,"paths":{"state":"/tmp/w"},"previews":{"hostSuffix":"p.example.com"}}`,
		// google auth without the block
		`{"version":1,"paths":{"state":"/tmp/w"},"auth":{"mode":"google","publicURL":"https://w.example.com"}}`,
		// github both user and app
		`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"github":{"authFile":"/tmp/t","appID":1}}}`,
		// relative state
		`{"version":1,"paths":{"state":"w"}}`,
		// a private home whose sbx sockets exceed sun_path
		`{"version":1,"paths":{"state":"/tmp/w"},"sbx":{"privateHome":"/Users/danielporter/Library/Application Support/Warden/sbx"}}`,
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config: %s", raw)
		}
	}
}

func TestProvidersCanBeRemoved(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"github":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	// JSON null removes the provider so the UI hides it; an omitted section
	// keeps the local default.
	if c.GitHubMode() != "" || c.Providers.GitHub != nil || c.Providers.Google == nil || c.Providers.Codex == nil {
		t.Fatalf("null did not remove the provider: %+v", c.Providers)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"google":null,"codex":{"authFile":"/x/auth.json"}}}`))
	if err != nil || c.Providers.Google != nil || c.Providers.Codex.AuthFile != "/x/auth.json" || c.GitHubMode() != "user" {
		t.Fatalf("%+v %v", c.Providers, err)
	}
}

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// macOS temp dirs exceed the Unix socket path limit; the state root is
	// only recorded, never created, by this test.
	c := Defaults("/tmp/w")
	path := filepath.Join(dir, "warden.json")
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Paths.State != "/tmp/w" || loaded.Chat.Listen != c.Chat.Listen {
		t.Fatalf("round trip: %+v", loaded)
	}
	if _, err = Load(path, "/elsewhere"); err == nil {
		t.Fatal("disagreeing --state accepted")
	}
}
