package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warden/chat/internal/config"
)

// fakeHome points the default state directory at a short temporary home,
// so instanceDir and instanceName round-trip against it.
func fakeHome(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "wh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("HOME", base)
	t.Setenv("XDG_DATA_HOME", base)
	t.Setenv(instanceEnv, "")
	return base
}

func TestInstanceDirAndNameRoundTrip(t *testing.T) {
	home := fakeHome(t)
	def, err := defaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "default"} {
		if dir, err := instanceDir(name); err != nil || dir != def {
			t.Fatalf("instanceDir(%q) = %q, %v; want %q", name, dir, err, def)
		}
	}
	dev, err := instanceDir("dogfood-a")
	if err != nil || dev != filepath.Join(filepath.Dir(def), ".warden-dogfood-a") || !strings.HasPrefix(dev, home) {
		t.Fatalf("instanceDir(dogfood-a) = %q, %v", dev, err)
	}
	if instanceName(def) != defaultInstance || instanceName(dev) != "dogfood-a" || instanceName("/tmp/wd/state") != "state" {
		t.Fatalf("instanceName: %q %q %q", instanceName(def), instanceName(dev), instanceName("/tmp/wd/state"))
	}
	for _, bad := range []string{"a/b", "a b", "spare", ".."} {
		if _, err := instanceDir(bad); err == nil {
			t.Fatalf("instanceDir(%q) accepted", bad)
		}
	}
}

func TestStateFlagsSelectTheInstance(t *testing.T) {
	fakeHome(t)
	parse := func(env string, args ...string) (string, error) {
		t.Setenv(instanceEnv, env)
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		s := addStateFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return s.dir()
	}
	dev, _ := instanceDir("dev")
	if dir, err := parse(""); err != nil || dir != "" {
		t.Fatalf("no flags: %q %v", dir, err)
	}
	if dir, err := parse("", "--instance", "dev"); err != nil || dir != dev {
		t.Fatalf("--instance dev: %q %v", dir, err)
	}
	if dir, err := parse("dev"); err != nil || dir != dev {
		t.Fatalf("$WARDEN_INSTANCE=dev: %q %v", dir, err)
	}
	if dir, err := parse("dev", "--instance", "default"); err != nil || dir != "" {
		t.Fatalf("--instance default over the env: %q %v", dir, err)
	}
	if dir, err := parse("dev", "--state", "/tmp/x"); err != nil || dir != "/tmp/x" {
		t.Fatalf("--state over the env: %q %v", dir, err)
	}
	if _, err := parse("", "--state", "/tmp/x", "--instance", "dev"); err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Fatalf("--state with --instance: %v", err)
	}
	if _, err := parse("", "--instance", "no/way"); err == nil {
		t.Fatal("bad instance name accepted")
	}
}

// A dev record is accepted with other pins without --upgrade; a plain one
// is not.
func TestCheckRecordAcceptsPinDriftOnADevInstance(t *testing.T) {
	state := t.TempDir()
	arch, _ := guestArch()
	have := currentRecord(arch)
	have.Codex, have.Warden, have.Dev = "0.1.0", "older", true
	if err := writeRecord(state, have); err != nil {
		t.Fatal(err)
	}
	if err := checkRecord(state, currentRecord(arch), false); err != nil {
		t.Fatalf("dev record refused: %v", err)
	}
	have.Dev = false
	if err := writeRecord(state, have); err != nil {
		t.Fatal(err)
	}
	if err := checkRecord(state, currentRecord(arch), false); err == nil {
		t.Fatal("plain record with other pins accepted without --upgrade")
	}
}

// warden install --dev marks the record, and the mark survives a re-run.
func TestInstallDevMarksTheRecordAndNamesTheInstance(t *testing.T) {
	f := newFixture(t)
	if code, out := f.install("--dev"); code != 0 {
		t.Fatalf("install --dev (%d):\n%s", code, out)
	}
	record, _, _ := readRecord(f.state)
	if !record.Dev || record.Name != "state" {
		t.Fatalf("record: %+v", record)
	}
	if code, out := f.install(); code != 0 {
		t.Fatalf("re-run (%d):\n%s", code, out)
	}
	if record, _, _ = readRecord(f.state); !record.Dev {
		t.Fatalf("dev mark lost on a re-run: %+v", record)
	}
	// A --state directory is its own instance; only a fresh non-default
	// instance gets a sandbox name prefix.
	cfg, err := config.Load(filepath.Join(f.state, "warden.json"), f.state)
	if err != nil || cfg.Sandboxes.NamePrefix != "state" {
		t.Fatalf("namePrefix %q (%v)", cfg.Sandboxes.NamePrefix, err)
	}
}

// installedSource installs the default instance of a fake home and returns
// the fixture; the source of every create below.
func installedSource(t *testing.T) (*fixture, string) {
	t.Helper()
	home := fakeHome(t)
	f := newFixture(t)
	f.state, _ = defaultStateDir()
	if code, out := f.install(); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	// A guest image the source runs on, which the release does not pin.
	cfg, err := config.Load(filepath.Join(f.state, "warden.json"), f.state)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SBX.GuestImage, cfg.SBX.GuestImageDigest = "warden-guest:abc-arm64", "sha256:"+strings.Repeat("ab", 32)
	if err := config.Write(filepath.Join(f.state, "warden.json"), cfg); err != nil {
		t.Fatal(err)
	}
	f.set("templates", `{"id": "abababababab", "ref": "warden-guest:abc-arm64"}`)
	for _, name := range []string{"auth.json", "claude.json", "github.json"} {
		if err := os.WriteFile(filepath.Join(f.state, "provider", name), []byte(`{"token":"`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f, home
}

func TestInstanceCreateCopiesFromTheSourceAndSharesItsNamespace(t *testing.T) {
	f, _ := installedSource(t)
	before := len(f.commands())
	code, out := f.run("", "instance", "create", "dev", "--dev", "--from", "default")
	if code != 0 {
		t.Fatalf("create (%d):\n%s", code, out)
	}
	dev, _ := instanceDir("dev")
	// The sign-ins and runtimes came from the source; no download ran.
	for _, name := range []string{"auth.json", "claude.json", "github.json"} {
		raw, err := os.ReadFile(filepath.Join(dev, "provider", name))
		if err != nil || string(raw) != `{"token":"`+name+`"}` {
			t.Fatalf("provider/%s: %q %v", name, raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dev, "runtimes", "claude", "claude")); err != nil {
		t.Fatalf("runtimes not copied: %v", err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("runtimes downloaded again (%d fetches)", f.hits.Load())
	}
	cfg, err := config.Load(filepath.Join(dev, "warden.json"), dev)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SBX.PrivateHome != filepath.Join(f.state, "sbx") {
		t.Fatalf("namespace not shared: %s", cfg.SBX.PrivateHome)
	}
	if cfg.Sandboxes.NamePrefix != "dev" {
		t.Fatalf("namePrefix %q", cfg.Sandboxes.NamePrefix)
	}
	if cfg.SBX.GuestImage != "warden-guest:abc-arm64" || cfg.SBX.GuestImageDigest != "sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("guest image pin not taken from the source: %s@%s", cfg.SBX.GuestImage, cfg.SBX.GuestImageDigest)
	}
	src, _ := config.Load(filepath.Join(f.state, "warden.json"), f.state)
	if cfg.Chat.Listen == src.Chat.Listen || cfg.Previews.EdgeListen == src.Previews.EdgeListen || cfg.Chat.Listen == cfg.Previews.EdgeListen {
		t.Fatalf("ports collide with the source's: %s %s vs %s %s", cfg.Chat.Listen, cfg.Previews.EdgeListen, src.Chat.Listen, src.Previews.EdgeListen)
	}
	wrapper, _ := os.ReadFile(wrapperPath(dev))
	if !strings.Contains(string(wrapper), shellQuote(filepath.Join(f.state, "sbx", "home"))) {
		t.Fatalf("wrapper does not select the shared namespace:\n%s", wrapper)
	}
	if _, err := os.Lstat(filepath.Join(dev, "sbx")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an own namespace directory was made: %v", err)
	}
	// One daemon: the shared one was found running, none was started.
	for _, cmd := range f.commands()[before:] {
		if strings.HasPrefix(cmd, "daemon start") || cmd == "login" {
			t.Fatalf("create ran %q against the shared namespace", cmd)
		}
	}
	record, _, _ := readRecord(dev)
	if !record.Dev || record.Name != "dev" {
		t.Fatalf("record: %+v", record)
	}
	if !strings.Contains(out, "sharing "+filepath.Join(f.state, "sbx")) || !strings.Contains(out, "Instance dev created") {
		t.Fatalf("output:\n%s", out)
	}
	if code, out := f.run("", "instance", "create", "dev"); code == 0 || !strings.Contains(out, "exists") {
		t.Fatalf("second create (%d):\n%s", code, out)
	}
	if code, out := f.run("", "instance", "create", "default"); code == 0 {
		t.Fatalf("create default (%d):\n%s", code, out)
	}
	// list shows both, the JSON form too.
	code, out = f.run("", "instance", "list")
	if code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(out, "default") || !strings.Contains(out, "dev ") || !strings.Contains(out, "yes") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	code, out = f.run("", "instance", "list", "--json")
	var infos []instanceInfo
	if code != 0 || json.Unmarshal([]byte(out), &infos) != nil || len(infos) != 2 || infos[0].Name != "default" || infos[1].Name != "dev" || !infos[1].Dev || infos[1].SharedNamespace != filepath.Join(f.state, "sbx") || infos[1].Release != "-" || infos[1].ChatPort == 0 {
		t.Fatalf("list --json (%d): %+v\n%s", code, infos, out)
	}
}

func TestInstanceRemoveTakesOnlyItsOwnSandboxes(t *testing.T) {
	f, _ := installedSource(t)
	if code, out := f.run("", "instance", "create", "dev"); code != 0 {
		t.Fatalf("create (%d):\n%s", code, out)
	}
	dev, _ := instanceDir("dev")
	f.set("sandboxes", "wc-abc123\nwc-dev-123abc\nwc-dev-spare-1a\nwc-spare-2b\n")
	if code, out := f.run("", "instance", "rm", "default", "--yes"); code == 0 || !strings.Contains(out, "warden uninstall") {
		t.Fatalf("rm default (%d):\n%s", code, out)
	}
	if code, out := f.run("", "instance", "rm", "dev"); code == 0 || !strings.Contains(out, "pass --yes") {
		t.Fatalf("rm without --yes off a terminal (%d):\n%s", code, out)
	}
	before := len(f.commands())
	code, out := f.run("", "instance", "rm", "dev", "--yes")
	if code != 0 || !strings.Contains(out, "sandboxes:   2 removed") || !strings.Contains(out, "left running") {
		t.Fatalf("rm (%d):\n%s", code, out)
	}
	if got := strings.Join(f.commands()[before:], "\n"); got != "ls --quiet\nrm --force wc-dev-123abc\nrm --force wc-dev-spare-1a" {
		t.Fatalf("commands:\n%s", got)
	}
	left, _ := os.ReadFile(filepath.Join(f.dir, "sandboxes"))
	if string(left) != "wc-abc123\nwc-spare-2b\n" {
		t.Fatalf("the default instance's sandboxes: %q", left)
	}
	if _, err := os.Stat(dev); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "daemon")); err != nil {
		t.Fatal("the shared daemon was stopped")
	}
	if code, out := f.run("", "instance", "rm", "dev", "--yes"); code == 0 {
		t.Fatalf("rm of nothing (%d):\n%s", code, out)
	}
}

func TestInstanceCollisionsFlagPortsAndLabels(t *testing.T) {
	infos := []instanceInfo{
		{Name: "default", ChatPort: 18780, EdgePort: 18781, Label: "com.monaddle.warden"},
		{Name: "a", ChatPort: 18780, EdgePort: 18783, Label: "com.monaddle.warden.a"},
		{Name: "b", ChatPort: 18790, EdgePort: 18790, Label: "com.monaddle.warden.a"},
		{Name: "c", ChatPort: 18783, EdgePort: 18795, Label: "com.monaddle.warden.c"},
	}
	got := strings.Join(instanceCollisions(infos), "\n")
	for _, want := range []string{"chat port 18780 is used by default and a", "service label com.monaddle.warden.a is used by a and b", "port 18783 is used by a (edge) and c (chat)", "port 18790 is used by b (chat) and b (edge)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(instanceCollisions(infos[:1])) != 0 {
		t.Fatal("one instance collides with itself")
	}
}

// The source instance may run a release whose warden.json has a section
// this launcher does not know; create still reads what it copies.
func TestLoadSourceConfigReadsAnUnknownReleaseRaw(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "warden.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"`+state+`"},"sbx":{"executable":"/opt/x/sbx","privateHome":"`+state+`/sbx","guestImage":"g:1","guestImageDigest":"sha256:`+strings.Repeat("cd", 32)+`"},"reporting":{"enabled":true},"vms":{"future":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path, state); err == nil {
		t.Fatal("the config package accepted the unknown section; the fallback is untested")
	}
	cfg, err := loadSourceConfig(path, state)
	if err != nil || cfg.SBX.Executable != "/opt/x/sbx" || cfg.SBX.PrivateHome != state+"/sbx" || cfg.SBX.GuestImage != "g:1" || !cfg.Reporting.Enabled {
		t.Fatalf("raw read: %+v %v", cfg.SBX, err)
	}
	if _, err := loadSourceConfig(filepath.Join(state, "missing.json"), state); err == nil {
		t.Fatal("a missing file loaded")
	}
}
