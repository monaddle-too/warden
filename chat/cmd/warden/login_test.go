package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"warden/chat/internal/config"
)

// loginFixture is a state root with a written warden.json and no SBX.
func loginFixture(t *testing.T) (state, configPath string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	state = filepath.Join(base, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(state, "warden.json")
	if err := config.Write(configPath, config.Defaults(state)); err != nil {
		t.Fatal(err)
	}
	return state, configPath
}

func runCLI(stdin string, terminal bool, args ...string) (int, string) {
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(stdin), stdout: &out, stderr: &out, terminal: terminal}
	return c.run(args), out.String()
}

func TestLoginClaudeWritesOnePrivateFile(t *testing.T) {
	state, configPath := loginFixture(t)
	token := "sk-ant-oat01-" + strings.Repeat("a", 40)
	code, out := runCLI(token+"\n", false, "login", "claude", "--config", configPath)
	if code != 0 {
		t.Fatalf("login (%d):\n%s", code, out)
	}
	path := filepath.Join(state, "provider", "claude.json")
	if m := mode(t, path); m != 0o600 {
		t.Fatalf("mode %04o", m)
	}
	raw, _ := os.ReadFile(path)
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.ClaudeAiOauth.AccessToken != token {
		t.Fatalf("document: %s (%v)", raw, err)
	}
	expires := time.UnixMilli(doc.ClaudeAiOauth.ExpiresAt)
	if d := time.Until(expires); d < 363*24*time.Hour || d > 365*24*time.Hour {
		t.Fatalf("expiry %s", expires)
	}
	entries, _ := os.ReadDir(filepath.Join(state, "provider"))
	if len(entries) != 1 {
		t.Fatalf("provider directory holds %d entries", len(entries))
	}
	if m := mode(t, filepath.Join(state, "provider")); m != 0o700 {
		t.Fatalf("provider dir mode %04o", m)
	}
	// An existing login is kept unless --replace is given.
	if code, out = runCLI(token+"\n", false, "login", "claude", "--config", configPath); code == 0 || !strings.Contains(out, "already holds a sign-in") {
		t.Fatalf("overwrote the login (%d):\n%s", code, out)
	}
	if code, out = runCLI(token+"\n", false, "login", "claude", "--config", configPath, "--replace"); code != 0 {
		t.Fatalf("replace failed (%d):\n%s", code, out)
	}
}

func TestClaudeCredentialRejectsWrongTokens(t *testing.T) {
	for _, bad := range []string{"", "short", "sk-ant-api03-" + strings.Repeat("b", 40), "sk-ant-oat01-proxy-" + strings.Repeat("c", 30), "sk-ant-oat01-placeholder" + strings.Repeat("d", 30), "sk-ant-oat01-has space" + strings.Repeat("e", 30), "sk-ant-oat01-" + strings.Repeat("f", 5000)} {
		if _, _, err := claudeCredential(bad, time.Now()); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	doc, expires, err := claudeCredential("sk-ant-oat01-"+strings.Repeat("g", 40), time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))
	if err != nil || expires != time.Date(2027, 9, 14, 0, 0, 0, 0, time.UTC) || !strings.HasSuffix(string(doc), "}\n") {
		t.Fatalf("%s %s %v", doc, expires, err)
	}
}

func TestLoginCodexImportsPastedAuthFile(t *testing.T) {
	state, configPath := loginFixture(t)
	source := filepath.Join(state, "pasted-auth.json")
	if err := os.WriteFile(source, []byte(`{"tokens":{"access_token":"x","refresh_token":"y"},"last_refresh":"2026-09-15T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(source+"\n", false, "login", "codex", "--config", configPath)
	if code != 0 {
		t.Fatalf("login (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "CODEX_HOME="+filepath.Join(state, "provider", ".codex-login")+" codex login --device-auth") {
		t.Fatalf("manual instructions missing:\n%s", out)
	}
	path := filepath.Join(state, "provider", "auth.json")
	if m := mode(t, path); m != 0o600 {
		t.Fatalf("mode %04o", m)
	}
	entries, _ := os.ReadDir(filepath.Join(state, "provider"))
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("provider directory: %v", entries)
	}
	// Not an auth.json: refused, nothing written.
	os.Remove(path)
	if err := os.WriteFile(source, []byte(`{"unrelated":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out = runCLI(source+"\n", false, "login", "codex", "--config", configPath); code == 0 || !strings.Contains(out, "neither ChatGPT tokens nor an API key") {
		t.Fatalf("accepted a non-login (%d):\n%s", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("wrote a rejected file")
	}
	// A missing --codex-cli is reported, not silently ignored.
	if code, out = runCLI("", false, "login", "codex", "--config", configPath, "--codex-cli", filepath.Join(state, "nope")); code == 0 || !strings.Contains(out, "--codex-cli") {
		t.Fatalf("missing codex accepted (%d):\n%s", code, out)
	}
}

func TestLoginCodexRunsHostCLIWithPrivateCodexHome(t *testing.T) {
	state, configPath := loginFixture(t)
	fake := filepath.Join(state, "codex")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"login --device-auth\" ] || exit 3\nprintf '{\"tokens\":{\"access_token\":\"t\"}}' > \"$CODEX_HOME/auth.json\"\necho \"$CODEX_HOME\" > " + shellQuote(filepath.Join(state, "seen-home")) + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI("", true, "login", "codex", "--config", configPath, "--codex-cli", fake)
	if code != 0 {
		t.Fatalf("login (%d):\n%s", code, out)
	}
	seen, _ := os.ReadFile(filepath.Join(state, "seen-home"))
	scratch := filepath.Join(state, "provider", ".codex-login")
	if strings.TrimSpace(string(seen)) != scratch {
		t.Fatalf("CODEX_HOME was %q", seen)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("scratch CODEX_HOME left behind")
	}
	if m := mode(t, filepath.Join(state, "provider", "auth.json")); m != 0o600 {
		t.Fatalf("mode %04o", m)
	}
}

func TestLoginGitHubUsesTheLoginPackageSurface(t *testing.T) {
	_, configPath := loginFixture(t)
	// A pasted value reaches chat/internal/login, whose shape check rejects
	// it before any network call.
	code, out := runCLI("", false, "login", "github", "--config", configPath, "--paste", "gho_x")
	if code == 0 || !strings.Contains(out, "expected a GitHub OAuth token") {
		t.Fatalf("login package not used (%d):\n%s", code, out)
	}
	// Without --paste the device flow runs through the same surface. Stub it:
	// the release ships a real client ID, and a test must never poll GitHub.
	saved := githubLogin
	githubLogin = fakeGitHubLogin{}
	defer func() { githubLogin = saved }()
	code, out = runCLI("", false, "login", "github", "--config", configPath)
	if code == 0 || !strings.Contains(out, "fake device flow reached") {
		t.Fatalf("device flow not dispatched to the login package (%d):\n%s", code, out)
	}
}

type fakeGitHubLogin struct{}

func (fakeGitHubLogin) Device(context.Context, io.Writer, string) error {
	return errors.New("fake device flow reached")
}
func (fakeGitHubLogin) Paste(string, string) error { return errors.New("fake paste reached") }

func TestLoginRejectsUnknownProviderAndMissingConfigDirectory(t *testing.T) {
	if code, _ := runCLI("", false, "login", "figma"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code, _ := runCLI("", false, "login"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestLaunchURLAndServiceArgs(t *testing.T) {
	_, configPath := loginFixture(t)
	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := cfg.OwnerTokenFile()
	os.MkdirAll(filepath.Dir(endpoint), 0o700)
	if _, err := launchURL(endpoint, time.Unix(0, 5)); err == nil {
		t.Fatal("missing endpoint accepted")
	}
	os.WriteFile(endpoint, []byte(`{"url":"http://127.0.0.1:18780","token":"abc"}`), 0o600)
	url, err := launchURL(endpoint, time.Unix(0, 5))
	if err != nil || url != "http://127.0.0.1:18780/?launch=5#session=abc" {
		t.Fatalf("%q %v", url, err)
	}
	// Owner mode opens through the edge (auth.publicURL, 127.0.0.1:18781 by
	// default) so preview navigations find the owner session; --without-edge
	// opens the chat origin itself.
	code, out := runCLI("", false, "open", "--config", configPath, "--print")
	if code != 0 || !strings.HasPrefix(out, "http://127.0.0.1:18781/?launch=") || !strings.HasSuffix(strings.TrimSpace(out), "#session=abc") {
		t.Fatalf("open --print (%d): %s", code, out)
	}
	code, out = runCLI("", false, "open", "--config", configPath, "--print", "--without-edge")
	if code != 0 || !strings.HasPrefix(out, "http://127.0.0.1:18780/?launch=") {
		t.Fatalf("open --print --without-edge (%d): %s", code, out)
	}

	l := &launcher{cfg: cfg, configPath: configPath, assets: assets{web: "/w", vendor: "/v", template: "/t.json"}}
	// Every service gets the file plus the resolved asset paths the file
	// leaves empty (a flag may fill an empty field, never disagree with one).
	if got := strings.Join(l.policyArgs(), " "); got != "--config "+configPath+" --vendor-dir /v --policy-template /t.json" {
		t.Errorf("policy args: %s", got)
	}
	if got := strings.Join(l.runnerArgs(), " "); got != "--config "+configPath {
		t.Errorf("runner args: %s", got)
	}
	if got := strings.Join(l.chatArgs(), " "); got != "--config "+configPath+" --web-dir /w" {
		t.Errorf("chat args: %s", got)
	}
}

func TestStartRefusesWithoutInstall(t *testing.T) {
	base, _ := os.MkdirTemp("/tmp", "ws")
	defer os.RemoveAll(base)
	code, out := runCLI("", false, "start", "--state", filepath.Join(base, "state"))
	if code == 0 || !strings.Contains(out, "run `warden install` first") {
		t.Fatalf("start without install (%d):\n%s", code, out)
	}
}

func TestExtractTarGzRefusesEscapes(t *testing.T) {
	base, _ := os.MkdirTemp("", "wx")
	defer os.RemoveAll(base)
	for i, entry := range []tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "/abs", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
		{Name: "link2", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "dev", Typeflag: tar.TypeChar},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&entry)
		tw.Close()
		gz.Close()
		archive := filepath.Join(base, "a.tgz")
		os.WriteFile(archive, buf.Bytes(), 0o600)
		dir := filepath.Join(base, "out")
		os.RemoveAll(dir)
		if err := extractTarGz(archive, dir); err == nil {
			t.Errorf("entry %d (%s) accepted", i, entry.Name)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "evil")); !os.IsNotExist(err) {
		t.Fatal("escaped file written")
	}
}

func TestWrapperScriptQuotesPaths(t *testing.T) {
	s := wrapperScript("/Users/o'neil/Library/Application Support/Warden/sbx", "/opt/homebrew/bin/sbx")
	if !strings.Contains(s, `export HOME='/Users/o'\''neil/Library/Application Support/Warden/sbx/home'`) || !strings.HasSuffix(s, "exec '/opt/homebrew/bin/sbx' \"$@\"\n") {
		t.Fatalf("%s", s)
	}
	for _, d := range namespaceDirs {
		if !strings.Contains(s, "export "+d.env+"=") {
			t.Fatalf("missing %s:\n%s", d.env, s)
		}
	}
}

func TestNamespaceEnvSelectsThePrivateNamespace(t *testing.T) {
	env := namespaceEnv([]string{"PATH=/bin", "HOME=/Users/x", "XDG_DATA_HOME=/elsewhere", "WARDEN_CONFIG=/c"}, "/tmp/w/sbx")
	got := strings.Join(env, " ")
	for _, want := range []string{"PATH=/bin", "WARDEN_CONFIG=/c", "HOME=/tmp/w/sbx/home", "XDG_CACHE_HOME=/tmp/w/sbx/cache", "XDG_STATE_HOME=/tmp/w/sbx/state", "XDG_CONFIG_HOME=/tmp/w/sbx/config", "XDG_DATA_HOME=/tmp/w/sbx/data"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "HOME=/Users/x") || strings.Contains(got, "/elsewhere") {
		t.Errorf("operator namespace leaked: %s", got)
	}
}

// `warden start` reads every service binary's --version line and refuses a
// set whose protocol numbers differ from its own; revisions may differ and
// a binary that prints no protocol (an older build) is only reported.
// An unpacked release tarball is warden-<version>-<os>-<arch>/{bin,web,
// vendor,config}; the launcher next to the binaries in bin/ must find the
// assets one level up without any configuration.
func TestLocateAssetsFindsTheReleaseLayout(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(root, "web"), filepath.Join(root, "vendor"), filepath.Join(root, "config")} {
		os.MkdirAll(dir, 0o755)
	}
	os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("<html>"), 0o644)
	os.WriteFile(filepath.Join(root, "vendor", "github-operations.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, "config", "policy.template.json"), []byte("{}"), 0o644)
	a, err := locateAssets(config.Defaults("/tmp/w"), bin, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.web != filepath.Join(root, "web") || a.vendor != filepath.Join(root, "vendor") || a.template != filepath.Join(root, "config", "policy.template.json") {
		t.Fatalf("%+v", a)
	}
	if _, err = locateAssets(config.Defaults("/tmp/w"), t.TempDir(), "", "", ""); err == nil || !strings.Contains(err.Error(), "built chat UI") {
		t.Fatal("missing assets accepted", err)
	}
}
