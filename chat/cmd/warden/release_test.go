package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"warden/chat/internal/config"
)

// releaseTarball builds a warden-<version>-<os>-<arch>.tar.gz whose
// bin/warden is a script appending its arguments to log.
func releaseTarball(t *testing.T, dir, version, goos, goarch, log string) string {
	t.Helper()
	name := fmt.Sprintf("warden-%s-%s-%s", version, goos, goarch)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(path string, mode int64, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + path, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"", "bin/", "web/", "config/", "vendor/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + d, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	// The usage probe (`install -h`) is not logged: a stub says nothing
	// about its flags, so every flag is passed.
	add("bin/warden", 0o755, "#!/bin/sh\n[ \"$2\" = -h ] && exit 0\necho \"$*\" >> "+shellQuote(log)+"\n")
	add("web/index.html", 0o644, "<html>"+version+"</html>")
	add("config/policy.template.json", 0o644, "{}")
	add("vendor/README", 0o644, "catalog")
	tw.Close()
	gz.Close()
	path := filepath.Join(dir, name+".tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installedState is a state directory with a record and a warden.json,
// what release install needs of an instance.
func installedState(t *testing.T) string {
	t.Helper()
	fakeHome(t)
	base, err := os.MkdirTemp("/tmp", "wr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	state := filepath.Join(base, "state")
	if err := ensurePrivateDir(state); err != nil {
		t.Fatal(err)
	}
	arch, _ := guestArch()
	if err := writeRecord(state, currentRecord(arch)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults(state)
	cfg.SBX.Executable = "/opt/fake/sbx"
	if err := config.Write(filepath.Join(state, "warden.json"), cfg); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestReleaseInstallUnpacksLinksAndUpgradesThenListsAndSwitches(t *testing.T) {
	state := installedState(t)
	log := filepath.Join(filepath.Dir(state), "warden.log")
	logged := func() string { raw, _ := os.ReadFile(log); return strings.TrimSpace(string(raw)) }
	tarballs := filepath.Join(filepath.Dir(state), "dist")
	os.MkdirAll(tarballs, 0o700)
	newer := releaseTarball(t, tarballs, "v9.9.9", runtime.GOOS, runtime.GOARCH, log)
	older := releaseTarball(t, tarballs, "v9.9.8", runtime.GOOS, runtime.GOARCH, log)
	foreign := releaseTarball(t, tarballs, "v9.9.9", "plan9", "amd64", log)
	var out bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
		return c.run(args)
	}
	if code := run("release", "list", "--state", state); code != 0 || !strings.Contains(out.String(), "no releases") {
		t.Fatalf("list of nothing (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", foreign, "--state", state); code == 0 || !strings.Contains(out.String(), "is for plan9/amd64") {
		t.Fatalf("foreign tarball (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", older, "--state", state); code != 0 {
		t.Fatalf("install older (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", newer, "--state", state); code != 0 {
		t.Fatalf("install newer (%d):\n%s", code, out.String())
	}
	// Releases land in the shared store, never under the instance.
	store, _ := storeDir()
	dest := filepath.Join(store, "warden-v9.9.9-"+runtime.GOOS+"-"+runtime.GOARCH)
	if target, err := os.Readlink(filepath.Join(state, "release")); err != nil || target != dest {
		t.Fatalf("link: %q %v, want %q", target, err, dest)
	}
	if _, err := os.Stat(filepath.Join(state, "releases")); err == nil {
		t.Fatal("a legacy <state>/releases was written")
	}
	for _, rel := range []string{"bin/warden", "web/index.html", "config/policy.template.json", "vendor/README"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Fatalf("unpacked layout: %v", err)
		}
	}
	// Each release's own launcher ran the install step, with --upgrade
	// and the instance's sbx, registering nothing.
	want := "install --state " + state + " --upgrade --service=false --menu=false --sbx /opt/fake/sbx"
	if got := logged(); got != want+"\n"+want {
		t.Fatalf("release launchers ran:\n%s\nwant twice:\n%s", got, want)
	}
	if !strings.Contains(out.String(), "unpacked warden-v9.9.9") || strings.Contains(out.String(), "still runs") {
		t.Fatalf("output:\n%s", out.String())
	}
	// A re-install of a version in the store reuses it (the instance is
	// still linked and its install step run); --force unpacks it again.
	if code := run("release", "install", newer, "--state", state); code != 0 || !strings.Contains(out.String(), "already in the store") {
		t.Fatalf("re-install (%d):\n%s", code, out.String())
	}
	if !strings.HasSuffix(logged(), want) {
		t.Fatalf("the reused release's install step did not run:\n%s", logged())
	}
	if code := run("release", "install", newer, "--state", state, "--force"); code != 0 || !strings.Contains(out.String(), "unpacked warden-v9.9.9") {
		t.Fatalf("forced re-install (%d):\n%s", code, out.String())
	}
	if _, err := os.Stat(dest + ".old"); err == nil {
		t.Fatal("the replaced directory was left behind")
	}
	if code := run("release", "list", "--state", state); code != 0 || !strings.Contains(out.String(), "* v9.9.9") || !strings.Contains(out.String(), "  v9.9.8") {
		t.Fatalf("list (%d):\n%s", code, out.String())
	}
	if code := run("release", "use", "9.9.8", "--state", state); code != 0 {
		t.Fatalf("use (%d):\n%s", code, out.String())
	}
	if target, _ := os.Readlink(filepath.Join(state, "release")); !strings.HasSuffix(target, "warden-v9.9.8-"+runtime.GOOS+"-"+runtime.GOARCH) {
		t.Fatalf("use did not repoint the link: %s", target)
	}
	if code := run("release", "use", "v1.2.3", "--state", state); code == 0 || !strings.Contains(out.String(), "no release v1.2.3") {
		t.Fatalf("use of an unknown version (%d):\n%s", code, out.String())
	}
	// An unpacked directory outside releases/ is copied in; one inside is
	// used as it is.
	os.Remove(log)
	outside := filepath.Join(filepath.Dir(state), "warden-v9.9.7-"+runtime.GOOS+"-"+runtime.GOARCH)
	if _, err := copyTree(dest, outside); err != nil {
		t.Fatal(err)
	}
	if code := run("release", "install", outside, "--state", state); code != 0 || !strings.Contains(out.String(), "copied "+outside) {
		t.Fatalf("install of a directory (%d):\n%s", code, out.String())
	}
	if target, _ := os.Readlink(filepath.Join(state, "release")); target != filepath.Join(store, filepath.Base(outside)) {
		t.Fatalf("link after a directory install: %s", target)
	}
	if code := run("release", "install", dest, "--state", state); code != 0 || strings.Contains(out.String(), "copied") {
		t.Fatalf("install of an installed directory (%d):\n%s", code, out.String())
	}
	if !strings.HasPrefix(logged(), "install --state "+state+" --upgrade") {
		t.Fatalf("directory installs did not run the install step:\n%s", logged())
	}
	if code := run("release", "install", filepath.Join(filepath.Dir(state), "nothing.tar.gz"), "--state", state); code == 0 {
		t.Fatalf("install of a missing tarball (%d):\n%s", code, out.String())
	}
}

// --restart goes through the release's own launcher: restart for a
// registered service, stop + start --detach for a detached Warden, a note
// when nothing runs.
func TestReleaseInstallRestartsTheWayDeployLocalDid(t *testing.T) {
	state := installedState(t)
	log := filepath.Join(filepath.Dir(state), "warden.log")
	tarball := releaseTarball(t, filepath.Dir(state), "v9.9.9", runtime.GOOS, runtime.GOARCH, log)
	logged := func() []string {
		raw, _ := os.ReadFile(log)
		os.Remove(log)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	svc := newFakeService(t, state)
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out, serviceFn: func(string) (serviceManager, string) { return svc, "" }}
	// Nothing registered, nothing detached: a note.
	if code := c.run([]string{"release", "install", tarball, "--state", state, "--restart"}); code != 0 || !strings.Contains(out.String(), "No Warden is running") {
		t.Fatalf("nothing running (%d):\n%s", code, out.String())
	}
	if got := logged(); len(got) != 1 || !strings.HasPrefix(got[0], "install ") {
		t.Fatalf("launcher ran %v", got)
	}
	// Detached: stop, then start --detach.
	if err := os.WriteFile(filepath.Join(state, "warden.pid"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := c.run([]string{"release", "install", tarball, "--state", state, "--restart"}); code != 0 {
		t.Fatalf("detached (%d):\n%s", code, out.String())
	}
	if got := logged(); len(got) != 3 || got[1] != "stop --state "+state || got[2] != "start --state "+state+" --detach" {
		t.Fatalf("launcher ran %v", got)
	}
	os.Remove(filepath.Join(state, "warden.pid"))
	// A registered, running service: restart.
	if _, err := svc.install("unit"); err != nil {
		t.Fatal(err)
	}
	svc.st = serviceStatus{Loaded: true, Running: true, PID: 42}
	out.Reset()
	if code := c.run([]string{"release", "install", tarball, "--state", state, "--restart"}); code != 0 {
		t.Fatalf("service (%d):\n%s", code, out.String())
	}
	if got := logged(); len(got) != 2 || got[1] != "restart --state "+state {
		t.Fatalf("launcher ran %v", got)
	}
	// Without --restart the running service is left, and said so.
	out.Reset()
	if code := c.run([]string{"release", "install", tarball, "--state", state}); code != 0 || !strings.Contains(out.String(), "still runs the previous release") {
		t.Fatalf("no --restart (%d):\n%s", code, out.String())
	}
}

func TestReleaseBuildNeedsAnInstanceAndAVersionFromGit(t *testing.T) {
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if code := c.run([]string{"release", "build", "."}); code == 0 || !strings.Contains(out.String(), "needs --instance") {
		t.Fatalf("build without an instance (%d):\n%s", code, out.String())
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "HOME="+repo)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "one")
	sha := git("rev-parse", "--short=12", "HEAD")
	if v, err := buildVersion(repo); err != nil || v != "v0.0.0-dev."+sha {
		t.Fatalf("buildVersion: %q %v", v, err)
	}
	git("tag", "v1.2.3")
	if v, err := buildVersion(repo); err != nil || v != "v1.2.3" {
		t.Fatalf("buildVersion at a tag: %q %v", v, err)
	}
	// An uncommitted edit to a tracked file makes the checkout dirty (a
	// build then replaces the store entry of the same version); untracked
	// files do not.
	if checkoutDirty(repo) {
		t.Fatal("a clean checkout reads as dirty")
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if checkoutDirty(repo) {
		t.Fatal("an untracked file makes the checkout dirty")
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-q", "-m", "two")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !checkoutDirty(repo) {
		t.Fatal("an edited tracked file leaves the checkout clean")
	}
	if checkoutDirty(t.TempDir()) {
		t.Fatal("a directory that is no checkout reads as dirty")
	}
	if _, err := buildVersion(t.TempDir()); err == nil {
		t.Fatal("a directory that is no checkout has a version")
	}
	if version, goos, goarch, ok := parseReleaseName("warden-v0.0.0-dev.abc123def456-darwin-arm64.tar.gz"); !ok || version != "v0.0.0-dev.abc123def456" || goos != "darwin" || goarch != "arm64" {
		t.Fatalf("parseReleaseName: %q %q %q %v", version, goos, goarch, ok)
	}
	if _, _, _, ok := parseReleaseName("release"); ok {
		t.Fatal("parseReleaseName accepted a bare name")
	}
}

// A release from before `install --service` / `--menu` existed is given
// only the flags its own install lists; a launcher whose usage cannot be
// read gets them all.
func TestInstallWithPassesOnlyTheFlagsTheReleaseKnows(t *testing.T) {
	state := installedState(t)
	log := filepath.Join(filepath.Dir(state), "warden.log")
	old := filepath.Join(filepath.Dir(state), "warden-v0.1.0-alpha.13-"+runtime.GOOS+"-"+runtime.GOARCH)
	os.MkdirAll(filepath.Join(old, "bin"), 0o755)
	script := "#!/bin/sh\nif [ \"$1 $2\" = \"install -h\" ]; then\n  printf 'Usage of warden install:\\n  -sbx string\\n    \\tsbx\\n  -state string\\n    \\tstate\\n  -upgrade\\n' >&2\n  exit 0\nfi\necho \"$*\" >> " + shellQuote(log) + "\n"
	if err := os.WriteFile(filepath.Join(old, "bin", "warden"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if known := installFlagsOf(filepath.Join(old, "bin", "warden")); !known["state"] || !known["sbx"] || known["service"] || known["menu"] {
		t.Fatalf("installFlagsOf = %v", known)
	}
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if code := c.run([]string{"release", "install", old, "--state", state}); code != 0 {
		t.Fatalf("install (%d):\n%s", code, out.String())
	}
	if raw, _ := os.ReadFile(log); strings.TrimSpace(string(raw)) != "install --state "+state+" --upgrade --sbx /opt/fake/sbx" {
		t.Fatalf("old release ran:\n%s", raw)
	}
	if known := installFlagsOf(filepath.Join(state, "no-such-launcher")); !known["service"] || !known["menu"] {
		t.Fatalf("unreadable usage: %v", known)
	}
}
