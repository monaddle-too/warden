package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// fakeSBX is a shell script standing in for sbx: it records every call with
// the HOME it saw, answers the verifier's inspection commands with canned
// JSON (the same shapes chat/internal/policy/verifier_test.go uses) and
// keeps a little state so `daemon start` and `settings set` are observable
// on a re-run. %[1]s is the fake's own state directory.
const fakeSBX = `#!/bin/sh
D=%[1]s
printf '%%s\n' "HOME=$HOME XDG_DATA_HOME=$XDG_DATA_HOME :: $*" >> "$D/calls.log"
setting() { if [ -f "$D/setting.$1" ]; then cat "$D/setting.$1"; else cat "$D/initial.$1"; fi; }
undefined() { if [ -f "$D/undefined.$1" ]; then echo "ERROR: setting \"$1\" is not defined" >&2; exit 1; fi; }
case "$*" in
  "settings get --json "*) undefined "$4" ;;
  "settings set "*) undefined "$3" ;;
esac
case "$*" in
  "version") echo "sbx version: v0.42.1 fixture-build" ;;
  "daemon status") if [ -f "$D/daemon.prompt" ]; then echo "Error: ensure daemon: cannot prompt for restart: stdin is not a terminal; run the command in an interactive terminal to confirm the restart" >&2; exit 1; fi; if [ -f "$D/daemon" ]; then echo "Status: running"; else echo "Status: stopped"; fi ;;
  "daemon inspect") if [ -f "$D/daemon.version" ]; then echo "{\"daemon_version\":\"$(cat "$D/daemon.version")\"}"; else echo "{\"daemon_version\":\"v0.42.1\"}"; fi ;;
  "daemon start --policy deny-all --detach") touch "$D/daemon"; echo "daemon started" ;;
  "daemon restart") rm -f "$D/daemon.prompt" "$D/daemon.version"; touch "$D/daemon"; echo restarted ;;
  "daemon stop") if [ -f "$D/daemon" ]; then rm -f "$D/daemon"; echo "daemon stopped"; else echo "daemon is not running" >&2; exit 1; fi ;;
  "ls --quiet") if [ -f "$D/daemon" ] && [ -f "$D/sandboxes" ]; then cat "$D/sandboxes"; fi ;;
  "rm --force "*) echo "$3" >> "$D/removed"; grep -v "^$3$" "$D/sandboxes" > "$D/sandboxes.new" 2>/dev/null; mv "$D/sandboxes.new" "$D/sandboxes"; echo "removed $3" ;;
  "settings get --json ssh.agentForwardingEnabled") echo "{\"key\":\"ssh.agentForwardingEnabled\",\"value\":$(setting ssh.agentForwardingEnabled)}" ;;
  "settings get --json proxy.sandbox") echo "{\"key\":\"proxy.sandbox\",\"value\":\"$(setting proxy.sandbox)\"}" ;;
  "settings set ssh.agentForwardingEnabled "*) echo "$4" > "$D/setting.ssh.agentForwardingEnabled"; echo "updated; restart the daemon to apply" ;;
  "settings set proxy.sandbox "*) echo "$4" > "$D/setting.proxy.sandbox"; echo updated ;;
  "mcp ls --json") if [ -f "$D/login" ]; then echo "{\"gateway\":{\"local\":true},\"servers\":[$(cat "$D/mcp")]}"; else echo "ERROR: 401 Unauthorized: user is not authenticated to Docker" >&2; exit 1; fi ;;
  "policy ls --json --include-inactive") echo "{\"rules\":[$(cat "$D/rules")]}" ;;
  "policy check network --json example.com:443") echo "{\"allowed\":false,\"deny_kind\":\"implicit\",\"governance\":{\"active\":false}}"; exit 1 ;;
  "login") touch "$D/login"; echo "device login complete" ;;
  "template ls --json") echo "{\"templates\":[]}" ;;
  *) echo "fake sbx: unexpected command: $*" >&2; exit 2 ;;
esac
`

type fixture struct {
	t       *testing.T
	dir     string // the fake's state
	sbx     string // the fake executable
	state   string // Warden's state root
	server  *httptest.Server
	hits    atomic.Int32
	codex   []byte
	claude  []byte
	badSHA  bool
	unpin   bool
	out     bytes.Buffer
	stdinRd *strings.Reader
	// serviceFn is the service manager the install sees (default: none).
	serviceFn func(state string) (serviceManager, string)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// Keep the state path short: the policy socket path must stay under 100
	// bytes and sbx's own sockets under <state>/sbx/home within 104; macOS
	// $TMPDIR and t.TempDir() names are far too long.
	base, err := os.MkdirTemp("/tmp", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	f := &fixture{t: t, dir: filepath.Join(base, "fake"), state: filepath.Join(base, "state")}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.sbx = filepath.Join(f.dir, "sbx")
	if err := os.WriteFile(f.sbx, []byte(fmt.Sprintf(fakeSBX, shellQuote(f.dir))), 0o700); err != nil {
		t.Fatal(err)
	}
	f.set("initial.ssh.agentForwardingEnabled", "true")
	f.set("initial.proxy.sandbox", "http")
	f.set("mcp", "")
	f.set("rules", "")
	arch, _ := guestArch()
	f.codex = bundleTarGz(t, codexTarget(arch))
	f.claude = []byte("#!/bin/sh\necho claude " + release.ClaudeVersion + "\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/codex.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		body := f.codex
		if f.badSHA {
			body = append([]byte("corrupt"), body...)
		}
		w.Write(body)
	})
	mux.HandleFunc("/claude", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.Write(f.claude)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	previous := runtimeSources
	runtimeSources = func(arch string) (release.Runtime, release.Runtime) {
		if f.unpin {
			return release.Runtime{URL: f.server.URL + "/codex.tar.gz"}, release.Runtime{URL: f.server.URL + "/claude"}
		}
		return release.Runtime{URL: f.server.URL + "/codex.tar.gz", SHA256: sha(f.codex)}, release.Runtime{URL: f.server.URL + "/claude", SHA256: sha(f.claude)}
	}
	t.Cleanup(func() { runtimeSources = previous })
	return f
}

func (f *fixture) set(name, content string) {
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (f *fixture) commands() []string {
	var out []string
	for _, line := range f.calls() {
		out = append(out, line[strings.Index(line, ":: ")+3:])
	}
	return out
}

func (f *fixture) run(stdin string, args ...string) (int, string) {
	f.out.Reset()
	c := &cli{stdin: strings.NewReader(stdin), stdout: &f.out, stderr: &f.out, terminal: true, serviceFn: f.serviceFn}
	code := c.run(args)
	return code, f.out.String()
}

func (f *fixture) install(extra ...string) (int, string) {
	return f.run("", append([]string{"install", "--state", f.state, "--sbx", f.sbx}, extra...)...)
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bundleTarGz builds a Codex "package" bundle with the layout the runner
// validates and the executables the Dockerfile checks.
func bundleTarGz(t *testing.T, target string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"bin/", "codex-path/", "codex-resources/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	add("codex-package.json", 0o644, `{"layoutVersion":1,"version":"`+release.CodexVersion+`","target":"`+target+`","variant":"codex","entrypoint":"bin/codex","resourcesDir":"codex-resources","pathDir":"codex-path"}`)
	for _, exe := range codexBundleExecutables {
		add(exe, 0o755, "#!/bin/sh\necho "+exe+"\n")
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestInstallCreatesNamespaceRuntimesAndConfig(t *testing.T) {
	f := newFixture(t)
	code, out := f.install()
	if code != 0 {
		t.Fatalf("install failed (%d):\n%s", code, out)
	}
	for _, sub := range append([]string{""}, stateSubdirs...) {
		if m := mode(t, filepath.Join(f.state, sub)); m != 0o700 {
			t.Errorf("%s mode %04o", sub, m)
		}
	}
	for _, d := range namespaceDirs {
		if m := mode(t, filepath.Join(f.state, "sbx", d.dir)); m != 0o700 {
			t.Errorf("namespace %s mode %04o", d.dir, m)
		}
	}
	wrapper, err := os.ReadFile(wrapperPath(f.state))
	if err != nil {
		t.Fatal(err)
	}
	want := wrapperScript(filepath.Join(f.state, "sbx"), f.sbx)
	if string(wrapper) != want || mode(t, wrapperPath(f.state)) != 0o700 {
		t.Fatalf("wrapper:\n%s", wrapper)
	}
	if !strings.Contains(want, "export HOME='"+filepath.Join(f.state, "sbx", "home")+"'") || !strings.Contains(want, "export XDG_DATA_HOME='"+filepath.Join(f.state, "sbx", "data")+"'") || !strings.HasSuffix(want, "exec '"+f.sbx+"' \"$@\"\n") {
		t.Fatalf("wrapper content:\n%s", want)
	}
	// Every call went through the wrapper, so the fake saw the private HOME.
	for _, line := range f.calls() {
		if !strings.HasPrefix(line, "HOME="+filepath.Join(f.state, "sbx", "home")+" XDG_DATA_HOME="+filepath.Join(f.state, "sbx", "data")+" :: ") {
			t.Fatalf("call outside the namespace: %s", line)
		}
	}
	cmds := f.commands()
	expect := []string{
		"daemon status", "daemon start --policy deny-all --detach",
		"settings get --json ssh.agentForwardingEnabled", "settings set ssh.agentForwardingEnabled false",
		"settings get --json proxy.sandbox", "settings set proxy.sandbox direct",
		"daemon restart",
		"mcp ls --json", "login",
		"version", "version", "daemon inspect", "settings get --json ssh.agentForwardingEnabled", "settings get --json proxy.sandbox", "mcp ls --json", "policy ls --json --include-inactive", "policy check network --json example.com:443",
	}
	if strings.Join(cmds, "\n") != strings.Join(expect, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(cmds, "\n"), strings.Join(expect, "\n"))
	}
	arch, _ := guestArch()
	runtimes := filepath.Join(f.state, "runtimes")
	if err := checkCodexBundle(codexDir(runtimes), arch); err != nil {
		t.Fatal(err)
	}
	if marker, _ := os.ReadFile(filepath.Join(codexDir(runtimes), shaMarker)); strings.TrimSpace(string(marker)) != sha(f.codex) {
		t.Fatalf("sha marker %q", marker)
	}
	if _, err := os.Stat(filepath.Join(runtimes, "codex-package-"+arch+".tar.gz")); !os.IsNotExist(err) {
		t.Fatal("archive kept after extraction")
	}
	if err := checkClaude(claudePath(runtimes), sha(f.claude)); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, claudePath(runtimes)); m != 0o755 {
		t.Fatalf("claude mode %04o", m)
	}
	configPath := filepath.Join(f.state, "warden.json")
	if m := mode(t, configPath); m != 0o600 {
		t.Fatalf("config mode %04o", m)
	}
	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paths.State != f.state || cfg.SBX.Executable != f.sbx || cfg.SBX.PrivateHome != filepath.Join(f.state, "sbx") {
		t.Fatalf("config paths: %+v", cfg)
	}
	if cfg.SBX.GuestImage != release.StockTemplate || cfg.SBX.GuestImageDigest != release.StockTemplateDigest {
		t.Fatalf("guest image: %+v", cfg.SBX)
	}
	if cfg.Runtimes.Codex != codexDir(runtimes) || cfg.Runtimes.Claude != claudePath(runtimes) {
		t.Fatalf("runtimes: %+v", cfg.Runtimes)
	}
	if cfg.Sandboxes.MaxRunning < 1 || (cfg.Sandboxes.MemoryMB != 1536 && cfg.Sandboxes.MemoryMB != 2048) {
		t.Fatalf("sizing: %+v", cfg.Sandboxes)
	}
	if cfg.Previews.Mode != config.PreviewLoopback || cfg.Auth.Mode != config.AuthOwner || cfg.Auth.PublicURL != "http://"+cfg.Previews.EdgeListen {
		t.Fatalf("modes: %+v %+v", cfg.Previews, cfg.Auth)
	}
	if cfg.Providers.Codex.AuthFile != filepath.Join(f.state, "provider", "auth.json") || cfg.Providers.Claude.AuthFile != filepath.Join(f.state, "provider", "claude.json") || cfg.Providers.GitHub.AuthFile != filepath.Join(f.state, "provider", "github.json") {
		t.Fatalf("providers: %+v", cfg.Providers)
	}
	// The file holds paths, never secrets, and its providers are paths only.
	raw, _ := os.ReadFile(configPath)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["version"] != float64(1) {
		t.Fatalf("config document: %s", raw)
	}
	record, found, err := readRecord(f.state)
	if err != nil || !found || !record.samePins(currentRecord(arch)) {
		t.Fatalf("install record: %+v %v %v", record, found, err)
	}
	if _, err := os.Stat(filepath.Join(f.state, "sbx", sbxLoginMarker)); err != nil {
		t.Fatal("login marker missing")
	}
	if !strings.Contains(out, "PASS sbx implicit denial") || !strings.Contains(out, "Installed.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("first install (%d):\n%s", code, out)
	}
	first, _ := os.ReadFile(filepath.Join(f.state, "warden.json"))
	firstCalls := len(f.commands())
	hits := f.hits.Load()
	// A stray login file must survive; install never writes provider logins.
	login := filepath.Join(f.state, "provider", "auth.json")
	if err := os.WriteFile(login, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := f.install()
	if code != 0 {
		t.Fatalf("second install (%d):\n%s", code, out)
	}
	second, _ := os.ReadFile(filepath.Join(f.state, "warden.json"))
	if string(first) != string(second) {
		t.Fatalf("config changed on re-run:\n%s\n---\n%s", first, second)
	}
	if f.hits.Load() != hits {
		t.Fatalf("re-run downloaded again (%d → %d)", hits, f.hits.Load())
	}
	cmds := f.commands()[firstCalls:]
	for _, c := range cmds {
		switch {
		case c == "login":
			t.Fatal("re-run repeated the SBX login")
		case strings.HasPrefix(c, "daemon start"), strings.HasPrefix(c, "settings set"), c == "daemon restart":
			t.Fatalf("re-run changed the daemon: %s", c)
		}
	}
	if raw, _ := os.ReadFile(login); string(raw) != `{"tokens":{}}` {
		t.Fatal("re-run touched the provider login")
	}
	for _, want := range []string{"already selects", "sbx daemon:      running", "already signed in", "already at"} {
		if !strings.Contains(out, want) {
			t.Fatalf("re-run output lacks %q:\n%s", want, out)
		}
	}
}

// An sbx build without a setting (older, or one that renamed it) answers
// `setting "…" is not defined`; the feature it governs is absent, so install
// skips it and doctor reports it as satisfied instead of failing.
func TestInstallAndDoctorSkipSettingsThisSbxDoesNotDefine(t *testing.T) {
	f := newFixture(t)
	f.set("undefined.ssh.agentForwardingEnabled", "")
	code, out := f.install()
	if code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "ssh.agentForwardingEnabled is not defined by this sbx; skipped") {
		t.Fatalf("install did not report the skipped setting:\n%s", out)
	}
	for _, call := range f.calls() {
		if strings.Contains(call, "settings set ssh.agentForwardingEnabled") {
			t.Fatalf("install tried to set an undefined setting: %s", call)
		}
	}
	code, out = f.run("", "doctor", "--config", filepath.Join(f.state, "warden.json"))
	if code != 0 || !strings.Contains(out, "PASS sbx setting ssh.agentForwardingEnabled: not defined by this sbx") || !strings.HasSuffix(out, "all checks passed\n") {
		t.Fatalf("doctor (%d):\n%s", code, out)
	}
}

func TestInstallRefusesShaMismatch(t *testing.T) {
	f := newFixture(t)
	f.badSHA = true
	code, out := f.install()
	if code == 0 || !strings.Contains(out, "SHA-256 mismatch") {
		t.Fatalf("mismatch accepted (%d):\n%s", code, out)
	}
	if _, err := os.Stat(codexDir(filepath.Join(f.state, "runtimes"))); !os.IsNotExist(err) {
		t.Fatal("bundle installed despite mismatch")
	}
	if _, err := os.Stat(filepath.Join(f.state, "warden.json")); !os.IsNotExist(err) {
		t.Fatal("config written despite failure")
	}
	entries, _ := os.ReadDir(filepath.Join(f.state, "runtimes"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".download") {
			t.Fatalf("partial download left behind: %s", e.Name())
		}
	}
}

func TestInstallRefusesUnpinnedArchitecture(t *testing.T) {
	f := newFixture(t)
	f.unpin = true
	code, out := f.install()
	if code == 0 || !strings.Contains(out, "does not yet pin the Codex") {
		t.Fatalf("unpinned architecture accepted (%d):\n%s", code, out)
	}
	if f.hits.Load() != 0 {
		t.Fatal("downloaded without a pin")
	}
}

func TestInstallStopsOnHostCheckFailure(t *testing.T) {
	f := newFixture(t)
	f.set("mcp", `{"name":"filesystem","transport":"stdio"}`)
	code, out := f.install()
	if code == 0 {
		t.Fatalf("install passed with an MCP server:\n%s", out)
	}
	if !strings.Contains(out, "FAIL sbx mcp inventory: 1 MCP server(s) registered: filesystem") || !strings.Contains(out, "mcp rm NAME") {
		t.Fatalf("output:\n%s", out)
	}
	if f.hits.Load() != 0 {
		t.Fatal("runtimes fetched although the host failed")
	}
	// The login precedes the host checks (an unsigned-in daemon answers them
	// with 401), so it runs once; the failure must stop everything after it.
	if n := strings.Count(strings.Join(f.commands(), "\n"), "login"); n != 1 {
		t.Fatalf("login ran %d times", n)
	}
}

func TestInstallRefusesForeignStateUnlessUpgrade(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	arch, _ := guestArch()
	old := currentRecord(arch)
	old.Codex, old.Warden = "0.150.0", "older"
	if err := writeRecord(f.state, old); err != nil {
		t.Fatal(err)
	}
	code, out := f.install()
	if code == 0 || !strings.Contains(out, "was installed by Warden older") || !strings.Contains(out, "--upgrade") {
		t.Fatalf("foreign state accepted (%d):\n%s", code, out)
	}
	if len(f.commands()) != 0 {
		t.Fatal("touched SBX before refusing")
	}
	if code, out = f.install("--upgrade"); code != 0 {
		t.Fatalf("upgrade failed (%d):\n%s", code, out)
	}
	record, _, _ := readRecord(f.state)
	if record.Codex != release.CodexVersion {
		t.Fatalf("record not refreshed: %+v", record)
	}
}

func TestInstallWithoutTerminalExplainsLogin(t *testing.T) {
	f := newFixture(t)
	f.out.Reset()
	c := &cli{stdin: strings.NewReader(""), stdout: &f.out, stderr: &f.out, terminal: false}
	// Without a sign-in nothing after the login step can pass, so install
	// stops there, non-zero, with the exact command to run.
	if code := c.run([]string{"install", "--state", f.state, "--sbx", f.sbx}); code == 0 {
		t.Fatalf("install passed without a sign-in:\n%s", f.out.String())
	}
	if !strings.Contains(f.out.String(), "not signed in to Docker") || !strings.Contains(f.out.String(), wrapperPath(f.state)+" login") || strings.Contains(f.out.String(), "PASS sbx version") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	for _, c := range f.commands() {
		if c == "login" {
			t.Fatal("login run without a terminal")
		}
	}
}

func TestDoctorAfterInstallPassesAndReportsFailuresWithFixes(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	configPath := filepath.Join(f.state, "warden.json")
	code, out := f.run("", "doctor", "--config", configPath)
	if code != 0 || strings.Contains(out, "FAIL") || !strings.HasSuffix(out, "all checks passed\n") {
		t.Fatalf("doctor after install (%d):\n%s", code, out)
	}
	for _, want := range []string{"PASS config: " + configPath, "PASS sbx version: sbx version: v0.42.1 fixture-build", "PASS sbx setting ssh.agentForwardingEnabled: false", "PASS sbx setting proxy.sandbox: \"direct\"", "PASS sbx mcp inventory: no servers, local gateway", "PASS sbx global network policy: no global rules", "PASS sbx implicit denial: example.com:443 denied implicitly, no governance", "PASS codex bundle: Codex " + release.CodexVersion, "PASS claude executable: Claude Code " + release.ClaudeVersion, "PASS guest image: stock template"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	// Break several things at once: a setting, a global allow rule, the
	// Claude binary and a login file's mode. Doctor lists every one with its
	// remediation and exits 1 without changing anything.
	f.set("setting.ssh.agentForwardingEnabled", "true")
	f.set("rules", `{"id":"r1","name":"allow","policy_name":"p","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["*.example.com:443"],"origin":"local","layer":"local","status":"active","editable":true}`)
	if err := os.WriteFile(claudePath(filepath.Join(f.state, "runtimes")), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	loginFile := filepath.Join(f.state, "provider", "claude.json")
	if err := os.WriteFile(loginFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(f.commands())
	code, out = f.run("", "doctor", "--config", configPath)
	if code != 1 {
		t.Fatalf("doctor exit %d:\n%s", code, out)
	}
	wrapper := wrapperPath(f.state)
	for _, want := range []string{
		"FAIL sbx setting ssh.agentForwardingEnabled: value is true, verifier requires false\n     fix: " + wrapper + " settings set ssh.agentForwardingEnabled false",
		"FAIL sbx global network policy: 1 global network rule(s): \"r1\" \"allow\" [\"*.example.com:443\"]\n     fix: remove every global network rule with " + wrapper + " policy rm network --id ID",
		"FAIL claude executable: " + claudePath(filepath.Join(f.state, "runtimes")) + " has SHA-256 ",
		"FAIL claude login: " + loginFile + " is mode 0644\n     fix: chmod 600 " + loginFile,
		"PASS sbx implicit denial",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "all checks passed") {
		t.Fatalf("summary printed despite failures:\n%s", out)
	}
	for _, c := range f.commands()[before:] {
		if strings.HasPrefix(c, "settings set") || (strings.HasPrefix(c, "daemon") && c != "daemon inspect") || strings.HasPrefix(c, "policy rm") || c == "login" {
			t.Fatalf("doctor changed the host: %s", c)
		}
	}
	if m := mode(t, loginFile); m != 0o644 {
		t.Fatal("doctor changed the login file")
	}
}

func TestDoctorRejectsGovernedPolicy(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	f.set("rules", `{"id":"g1","scope":"global","resource_type":"network","decision":"deny","resources":["**"],"layer":"remote","status":"active"}`)
	code, out := f.run("", "doctor", "--config", filepath.Join(f.state, "warden.json"))
	if code != 1 || !strings.Contains(out, "FAIL sbx global network policy: unknown or governed network policy \"g1\"") {
		t.Fatalf("governed rule accepted (%d):\n%s", code, out)
	}
	// The Linux implicit-deny sentinel is not a rule.
	sentinel, _ := json.Marshal(linuxDefaultDeny)
	f.set("rules", string(sentinel))
	if code, out = f.run("", "doctor", "--config", filepath.Join(f.state, "warden.json")); code != 0 {
		t.Fatalf("sentinel rejected (%d):\n%s", code, out)
	}
}

func TestSizeSandboxes(t *testing.T) {
	base := config.Defaults("/tmp/w").Sandboxes
	for _, tc := range []struct {
		memoryMB, cpus              int
		memory, running, warmSpares int
	}{
		{0, 8, 1536, 2, 1},        // unknown memory keeps the defaults
		{8 * 1024, 8, 1536, 2, 1}, // 8 GiB: 4 GiB for the host, two sandboxes
		{6 * 1024, 4, 1536, 1, 0}, // 6 GiB: one sandbox, no spare
		{16 * 1024, 4, 1536, 2, 1},
		{16 * 1024, 10, 1536, 4, 1},
		{48 * 1024, 14, 2048, 4, 1},
	} {
		s := sizeSandboxes(base, tc.memoryMB, tc.cpus)
		if s.MemoryMB != tc.memory || s.MaxRunning != tc.running || s.WarmSpares != tc.warmSpares {
			t.Errorf("%d MiB %d cpus: %+v", tc.memoryMB, tc.cpus, s)
		}
		if s.StopAfterIdleMinutes != base.StopAfterIdleMinutes || s.KeepStopped != base.KeepStopped {
			t.Errorf("lifecycle changed: %+v", s)
		}
	}
}

func TestDefaultStateDir(t *testing.T) {
	dir, err := defaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		if dir != filepath.Join(home, ".warden") {
			t.Fatal(dir)
		}
	case "linux":
		if !strings.HasSuffix(dir, "/warden") {
			t.Fatal(dir)
		}
	}
	if len(filepath.Join(dir, "policy", "sbx-control.sock")) > 100 {
		t.Fatalf("default socket path too long: %s", dir)
	}
}

func TestNamespaceLinksTheLoginKeychainOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS keychain layout")
	}
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	link, target, err := keychainLink(filepath.Join(f.state, "sbx"))
	if err != nil {
		t.Fatal(err)
	}
	if have, err := os.Readlink(link); err != nil || have != target {
		t.Fatalf("keychain link %q -> %q (%v), want %q", link, have, err, target)
	}
	// A second install keeps the link; a foreign entry in its place is refused.
	if code, out := f.install(); code != 0 {
		t.Fatalf("re-install (%d):\n%s", code, out)
	}
	os.Remove(link)
	os.MkdirAll(link, 0o700)
	if code, out := f.install(); code == 0 || !strings.Contains(out, "not a link") {
		t.Fatalf("foreign Keychains directory accepted (%d):\n%s", code, out)
	}
}

func TestInstalledBundleIsWorldReadable(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	dir := codexDir(filepath.Join(f.state, "runtimes"))
	for _, rel := range []string{"", "bin", "codex-package.json", "bin/codex", shaMarker} {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		perm := info.Mode().Perm()
		if info.IsDir() && perm != 0o755 {
			t.Errorf("%s is %04o, want 0755", rel, perm)
		}
		if !info.IsDir() && perm&0o444 != 0o444 {
			t.Errorf("%s is %04o, not world-readable", rel, perm)
		}
	}
	if perm := mode(t, filepath.Join(dir, "bin", "codex")); perm&0o111 != 0o111 {
		t.Errorf("bin/codex is %04o, not executable by all", perm)
	}
	// A tree that lost its modes is repaired by a re-run and flagged by doctor.
	os.Chmod(filepath.Join(dir, "bin"), 0o700)
	if err := checkWorldReadable(dir); err == nil {
		t.Fatal("owner-only bin accepted")
	}
	if code, out := f.install(); code != 0 {
		t.Fatalf("re-install (%d):\n%s", code, out)
	}
	if perm := mode(t, filepath.Join(dir, "bin")); perm != 0o755 {
		t.Errorf("bin not repaired: %04o", perm)
	}
}

func TestEnsureDaemonRunningStartsAStoppedDaemon(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	// Simulate a reboot: the fake daemon marker disappears.
	os.Remove(filepath.Join(f.dir, "daemon"))
	before := len(f.commands())
	var steps []string
	sbx := &sbxCLI{wrapper: wrapperPath(f.state), stdin: strings.NewReader(""), stdout: &f.out, stderr: &f.out}
	if err := ensureDaemonRunning(sbx, func(name, detail string) { steps = append(steps, name+": "+detail) }); err != nil {
		t.Fatal(err)
	}
	got := f.commands()[before:]
	if strings.Join(got, "\n") != "daemon status\ndaemon start --policy deny-all --detach" {
		t.Fatalf("commands: %v", got)
	}
	if len(steps) != 1 || !strings.Contains(steps[0], "started") {
		t.Fatalf("steps: %v", steps)
	}
	// Running already: only the status query.
	before = len(f.commands())
	if err := ensureDaemonRunning(sbx, func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	if got := f.commands()[before:]; strings.Join(got, "\n") != "daemon status\nversion\ndaemon inspect" {
		t.Fatalf("commands: %v", got)
	}
}

// After an sbx upgrade the CLI refuses the older daemon until it restarts.
// Warden owns the daemon and restarts it itself, whether sbx says so with
// the terminal prompt it cannot show, or `daemon inspect` reports a version
// other than the CLI's.
func TestEnsureDaemonRunningRestartsADaemonOlderThanTheCLI(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	sbx := &sbxCLI{wrapper: wrapperPath(f.state), stdin: strings.NewReader(""), stdout: &f.out, stderr: &f.out}
	for _, c := range []struct {
		name, marker, want string
	}{
		{"prompt", "daemon.prompt", "daemon status\ndaemon restart"},
		{"version", "daemon.version", "daemon status\nversion\ndaemon inspect\ndaemon restart"},
	} {
		f.set(c.marker, "v0.41.0")
		before := len(f.commands())
		var steps []string
		if err := ensureDaemonRunning(sbx, func(name, detail string) { steps = append(steps, name+": "+detail) }); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := f.commands()[before:]; strings.Join(got, "\n") != c.want {
			t.Fatalf("%s commands: %v", c.name, got)
		}
		if len(steps) != 1 || !strings.Contains(steps[0], "restarted") {
			t.Fatalf("%s steps: %v", c.name, steps)
		}
	}
	// doctor reports a mismatch with the remediation.
	f.set("daemon.version", "v0.41.0")
	code, out := f.run("", "doctor", "--config", filepath.Join(f.state, "warden.json"))
	if code == 0 || !strings.Contains(out, "FAIL sbx daemon version: daemon v0.41.0, CLI v0.42.1") {
		t.Fatalf("doctor (%d):\n%s", code, out)
	}
}

// uninstall deletes every sandbox in the namespace, stops the daemon and
// removes the state; --keep-state stops after the sbx cleanup. Without a
// terminal it needs --yes.
func TestUninstallRemovesSandboxesDaemonAndState(t *testing.T) {
	f := newFixture(t)
	svc := newFakeService(t, f.state)
	svc.onStart = endpointWriter(config.Defaults(f.state))
	f.serviceFn = func(string) (serviceManager, string) { return svc, "" }
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	f.set("sandboxes", "wc-spare-1\nwc-2\n")
	configPath := filepath.Join(f.state, "warden.json")
	if code, out := f.run("", "uninstall", "--config", configPath); code == 0 || !strings.Contains(out, "pass --yes") {
		t.Fatalf("uninstall without a terminal or --yes (%d):\n%s", code, out)
	}
	before := len(f.commands())
	code, out := f.run("", "uninstall", "--config", configPath, "--yes", "--keep-state")
	if code != 0 || !strings.Contains(out, "sandboxes:   2 removed") || !strings.Contains(out, "sbx daemon:  stopped") || !strings.Contains(out, "kept "+f.state) {
		t.Fatalf("uninstall --keep-state (%d):\n%s", code, out)
	}
	// The service went first: stopped, unregistered, its unit removed.
	if !strings.Contains(out, "service:     stopped and unregistered the fake agent ("+svc.unitPath()+" removed)") || svc.registered() || svc.calls[len(svc.calls)-1] != "uninstall" {
		t.Fatalf("service not unregistered (%v):\n%s", svc.calls, out)
	}
	if got := strings.Join(f.commands()[before:], "\n"); got != "ls --quiet\nrm --force wc-spare-1\nrm --force wc-2\ndaemon stop" {
		t.Fatalf("commands:\n%s", got)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal("state removed despite --keep-state")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "daemon")); err == nil {
		t.Fatal("daemon still running")
	}
	// Second run: nothing to list (daemon stopped), then the state goes.
	code, out = f.run("", "uninstall", "--state", f.state, "--yes")
	if code != 0 || !strings.Contains(out, "sandboxes:   0 removed") || !strings.Contains(out, "removed "+f.state) || !strings.Contains(out, "github.com/settings/applications") {
		t.Fatalf("uninstall (%d):\n%s", code, out)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state still present: %v", err)
	}
	if code, out := f.run("", "uninstall", "--state", f.state, "--yes"); code == 0 || !strings.Contains(out, "nothing to uninstall") {
		t.Fatalf("uninstall of nothing (%d):\n%s", code, out)
	}
}
