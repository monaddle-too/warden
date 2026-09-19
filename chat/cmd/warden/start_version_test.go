package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// `warden start --version V` executes the release V's own launcher for
// the instance with the other flags passed through: refused while the
// instance runs, a trial unless --use repoints the link, --as NAME runs
// it as another instance.
func TestStartVersionExecsTheReleaseForTheInstance(t *testing.T) {
	fakeHome(t)
	dev := fakeInstance(t, "dev")
	beside := fakeInstance(t, "beside")
	log := filepath.Join(t.TempDir(), "log")
	a := storeStub(t, "v9.9.8", log)
	b := storeStub(t, "v9.9.9", log)
	relink(dev, a)
	var out bytes.Buffer
	var execs [][]string
	var envs []string
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out, execve: func(path string, argv, env []string) error {
		execs = append(execs, append([]string{path}, argv...))
		envs = env
		return nil
	}}
	run := func(args ...string) int {
		out.Reset()
		execs, envs = nil, nil
		return c.run(args)
	}
	bin := func(rel string) string { return filepath.Join(rel, "bin", "warden") }

	// No value: a usage error that points at `warden versions`.
	if code := run("start", "--instance", "dev", "--version"); code != 2 || !strings.Contains(out.String(), "`warden versions` lists them") {
		t.Fatalf("bare --version (%d):\n%s", code, out.String())
	}
	if code := run("start", "--instance", "dev", "--version", "--detach"); code != 2 || !strings.Contains(out.String(), "needs a version") {
		t.Fatalf("--version before a flag (%d):\n%s", code, out.String())
	}
	if code := run("start", "--instance", "dev", "--use"); code != 2 || !strings.Contains(out.String(), "go with --version") {
		t.Fatalf("--use alone (%d):\n%s", code, out.String())
	}
	if code := run("start", "--instance", "dev", "--version", "v1.2.3"); code != 1 || !strings.Contains(out.String(), "no release v1.2.3 installed") {
		t.Fatalf("unknown version (%d):\n%s", code, out.String())
	}
	// A trial run: the link stays on a, b's launcher is executed with the
	// flags passed through and the re-exec guard set.
	if code := run("start", "--instance", "dev", "--version", "9.9.9", "--detach", "--popups", "notify"); code != 0 {
		t.Fatalf("trial (%d):\n%s", code, out.String())
	}
	want := []string{bin(b), bin(b), "start", "--instance", "dev", "--detach", "--popups", "notify"}
	if len(execs) != 1 || !slices.Equal(execs[0], want) {
		t.Fatalf("execve = %q, want %q", execs, want)
	}
	if !slices.Contains(envs, releaseReexecEnv+"=1") {
		t.Fatal("the child is not guarded against re-executing")
	}
	if target, _ := os.Readlink(filepath.Join(dev, "release")); target != a {
		t.Fatalf("a trial moved the link to %s", target)
	}
	if !strings.Contains(out.String(), "starting v9.9.9 as instance dev") {
		t.Fatalf("output:\n%s", out.String())
	}
	// --use repoints the link (and runs the release's install) first.
	if code := run("start", "--instance", "dev", "--version", "latest", "--use"); code != 0 {
		t.Fatalf("--use (%d):\n%s", code, out.String())
	}
	if target, _ := os.Readlink(filepath.Join(dev, "release")); target != b {
		t.Fatalf("--use left the link on %s", target)
	}
	if raw, _ := os.ReadFile(log); !strings.Contains(string(raw), "install --state "+dev+" --upgrade") {
		t.Fatalf("--use did not run the release's install:\n%s", raw)
	}
	if len(execs) != 1 || !slices.Equal(execs[0], []string{bin(b), bin(b), "start", "--instance", "dev"}) {
		t.Fatalf("execve = %q", execs)
	}
	// Already running: refused, with the way out.
	writeRunning(dev, runningInfo{Version: "v9.9.9", Binary: bin(b), PID: os.Getpid(), StartedAt: time.Now()})
	if code := run("start", "--instance", "dev", "--version", "v9.9.8"); code != 1 || !strings.Contains(out.String(), "instance dev is running v9.9.9; stop it, or add --as NAME to run v9.9.8 beside it") || execs != nil {
		t.Fatalf("running (%d):\n%s", code, out.String())
	}
	// A stale record does not count.
	writeRunning(dev, runningInfo{Version: "v9.9.9", PID: 1 << 30})
	if code := run("start", "--instance", "dev", "--version", "v9.9.8"); code != 0 || len(execs) != 1 {
		t.Fatalf("stale record (%d):\n%s", code, out.String())
	}
	// --as NAME: the existing instance NAME gets the link and is started
	// detached; this instance's flags are replaced by its own.
	writeRunning(dev, runningInfo{Version: "v9.9.9", Binary: bin(b), PID: os.Getpid(), StartedAt: time.Now()})
	if code := run("start", "--instance", "dev", "--version", "9.9.8", "--as", "beside", "--popups", "notify"); code != 0 {
		t.Fatalf("--as (%d):\n%s", code, out.String())
	}
	want = []string{bin(a), bin(a), "start", "--instance", "beside", "--popups", "notify", "--detach"}
	if len(execs) != 1 || !slices.Equal(execs[0], want) {
		t.Fatalf("execve = %q, want %q", execs, want)
	}
	if target, _ := os.Readlink(filepath.Join(beside, "release")); target != a {
		t.Fatalf("--as did not link beside to %s: %s", a, target)
	}
	if code := run("start", "--instance", "dev", "--version", "9.9.8", "--as", "beside", "--foreground"); code != 0 || slices.Contains(execs[0], "--detach") {
		t.Fatalf("--as --foreground (%d): %q\n%s", code, execs, out.String())
	}
	if code := run("start", "--instance", "dev", "--version", "9.9.8", "--as", "dev"); code != 1 || !strings.Contains(out.String(), "names this instance") {
		t.Fatalf("--as itself (%d):\n%s", code, out.String())
	}
	if code := run("start", "--instance", "dev", "--version", "9.9.8", "--as", "bad name"); code != 1 {
		t.Fatalf("--as with a bad name (%d):\n%s", code, out.String())
	}
	// Under a registered service the trial is refused (the service would
	// run the pinned release); --use switches the link and starts it.
	os.Remove(runningPath(beside))
	svc := newFakeService(t, beside)
	if _, err := svc.install("unit"); err != nil {
		t.Fatal(err)
	}
	svc.st = serviceStatus{Loaded: true}
	c.serviceFn = func(state string) (serviceManager, string) {
		if state == beside {
			return svc, ""
		}
		return nil, "none"
	}
	if code := run("start", "--instance", "beside", "--version", "9.9.9"); code != 1 || !strings.Contains(out.String(), "has a registered fake agent that runs its pinned release") {
		t.Fatalf("service trial (%d):\n%s", code, out.String())
	}
	if code := run("start", "--instance", "beside", "--version", "9.9.9", "--use"); code != 0 || len(execs) != 1 {
		t.Fatalf("service --use (%d):\n%s", code, out.String())
	}
	if target, _ := os.Readlink(filepath.Join(beside, "release")); target != b {
		t.Fatalf("service --use left the link on %s", target)
	}
	svc.st = serviceStatus{Loaded: true, Running: true, PID: 42}
	if code := run("start", "--instance", "beside", "--version", "9.9.8"); code != 1 || !strings.Contains(out.String(), "instance beside is running an unknown version (as a fake agent)") {
		t.Fatalf("service running (%d):\n%s", code, out.String())
	}
}

func TestWithoutFlagsDropsValuedAndBooleanForms(t *testing.T) {
	args := []string{"--version=1", "--instance", "x", "--use", "-as", "y", "--detach", "--popups=none", "--", "--version"}
	got := withoutFlags(args, map[string]bool{"version": true, "use": true, "as": true})
	if !slices.Equal(got, []string{"--instance", "x", "--detach", "--popups=none", "--", "--version"}) {
		t.Fatalf("withoutFlags = %q", got)
	}
	if bareVersionFlag([]string{"--version", "v1"}) != nil || bareVersionFlag([]string{"--version=v1"}) != nil || bareVersionFlag([]string{"--", "--version"}) != nil {
		t.Fatal("a valued --version was refused")
	}
	if bareVersionFlag([]string{"--foo", "--version"}) == nil {
		t.Fatal("a bare --version was accepted")
	}
}
