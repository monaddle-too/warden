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
	"fmt"
	"warden/chat/internal/config"
	"warden/chat/internal/handshake"
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

func TestLaunchURLAndLegacyArgs(t *testing.T) {
	state, configPath := loginFixture(t)
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

	cfg.Runtimes.Codex = filepath.Join(state, "runtimes", "codex")
	cfg.Runtimes.Claude = filepath.Join(state, "runtimes", "claude", "claude")
	cfg.SBX.GuestImage, cfg.SBX.GuestImageDigest = "docker/sandbox-templates:shell-docker", "sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919"
	l := &launcher{cfg: cfg, configPath: configPath, assets: assets{web: "/w", vendor: "/v", template: "/t.json"}}
	wrapper := wrapperPath(state)
	policy := strings.Join(l.policyArgs(true, wrapper), " ")
	for _, want := range []string{"--state " + cfg.PolicyState(), "--sbx " + wrapper, "--manage-network", "--vendor-dir /v", "--policy-template /t.json", "--codex-auth-file " + cfg.Providers.Codex.AuthFile, "--claude-auth-file " + cfg.Providers.Claude.AuthFile, "--guest-image-digest sha256:5fc81", "--gateway-ca-max-age 8760h0m0s"} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy args lack %q: %s", want, policy)
		}
	}
	runner := strings.Join(l.runnerArgs(true, wrapper), " ")
	for _, want := range []string{"--root " + cfg.RunnerState(), "--socket " + cfg.RunnerSocket(), "--warden-socket " + cfg.PolicySocket(), "--runtime-dir " + cfg.Runtimes.Codex, "--sbx " + wrapper, "--template docker/sandbox-templates:shell-docker@sha256:5fc81", "--sandbox-memory-mb 1536", "--max-resident 2", "--spare-sandboxes 1", "--idle-timeout 15m0s", "--retained 32"} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner args lack %q: %s", want, runner)
		}
	}
	if strings.Contains(runner, "--claude-path") {
		t.Errorf("claude path passed although the executable is absent: %s", runner)
	}
	chat := strings.Join(l.chatArgs(true), " ")
	for _, want := range []string{"--state " + cfg.AppState(), "--listen 127.0.0.1:18780", "--web-dir /w", "--preview-suffix "} {
		if !strings.Contains(chat, want) {
			t.Errorf("chat args lack %q: %s", want, chat)
		}
	}
	// Config mode passes the file plus the resolved asset paths the file
	// leaves empty (a flag may fill an empty field, never disagree with one).
	if got := strings.Join(l.policyArgs(false, wrapper), " "); got != "--config "+configPath+" --vendor-dir /v --policy-template /t.json" {
		t.Errorf("policy config-mode args: %s", got)
	}
	if got := strings.Join(l.runnerArgs(false, wrapper), " "); got != "--config "+configPath {
		t.Errorf("runner config-mode args: %s", got)
	}
	if got := strings.Join(l.chatArgs(false), " "); got != "--config "+configPath+" --web-dir /w" {
		t.Errorf("chat config-mode args: %s", got)
	}
}

func TestUsageHasConfigFlag(t *testing.T) {
	legacy := "Usage of warden-policy:\n  -claude-auth-file string\n    \tprivate host Claude credential file\n  -google-config string\n    \tprivate Google OAuth client configuration\n  -state string\n    \tprivate state directory (required)\n"
	if usageHasConfigFlag(legacy) {
		t.Fatal("-google-config mistaken for -config")
	}
	if !usageHasConfigFlag(legacy+"  -config string\n    \twarden.json (default $WARDEN_CONFIG)\n") || !usageHasConfigFlag("  -config\tstring\n") {
		t.Fatal("-config not detected")
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
func TestStartRefusesMismatchedProtocols(t *testing.T) {
	state, configPath := loginFixture(t)
	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	previous := versionOutput
	defer func() { versionOutput = previous }()
	outputs := map[string]string{}
	versionOutput = func(binary string) (string, error) {
		out, ok := outputs[filepath.Base(binary)]
		if !ok {
			return "", errors.New("exit status 2")
		}
		return out, nil
	}
	self := handshake.Self("warden")
	line := func(name, revision string, protocol int) string {
		return handshake.Peer{Name: name, Revision: revision, Protocol: protocol}.String() + "\n"
	}
	set := map[string]string{"warden-policy": "/b/warden-policy", "warden-runner": "/b/warden-runner", "warden-chat": "/b/warden-chat", "warden-edge": "/b/warden-edge"}
	check := func() (string, error) {
		var out bytes.Buffer
		l := &launcher{c: &cli{stdout: &out, stderr: &out}, cfg: cfg, configPath: configPath, binDir: "/b"}
		err := l.checkVersions(set)
		return out.String(), err
	}
	outputs = map[string]string{"warden-policy": line("warden-policy", "other", self.Protocol), "warden-runner": line("warden-runner", self.Revision, self.Protocol), "warden-chat": line("warden-chat", self.Revision, self.Protocol), "warden-edge": line("warden-edge", self.Revision, self.Protocol)}
	out, err := check()
	if err != nil || !strings.Contains(out, "warden-policy other protocol=") || !strings.Contains(out, self.String()) {
		t.Fatalf("mixed revisions refused: %v\n%s", err, out)
	}
	outputs["warden-runner"] = line("warden-runner", "old", self.Protocol+1)
	out, err = check()
	if err == nil || !strings.Contains(err.Error(), "warden-runner old protocol=") || !strings.Contains(err.Error(), fmt.Sprintf("requires protocol %d", self.Protocol)) {
		t.Fatalf("mismatched protocol accepted: %v\n%s", err, out)
	}
	outputs["warden-runner"] = "warden-runner ancient\n"
	delete(outputs, "warden-chat")
	out, err = check()
	if err != nil || !strings.Contains(out, "warden-runner reports no protocol number") || !strings.Contains(out, "warden-chat: no version information") {
		t.Fatalf("older builds not tolerated: %v\n%s", err, out)
	}
	_ = state
}

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
