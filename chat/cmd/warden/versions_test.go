package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/config"
)

// fakeInstance installs a stub instance (install.json + warden.json) as
// the named instance under the fake home and returns its state directory.
func fakeInstance(t *testing.T, name string) string {
	t.Helper()
	dir, err := instanceDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	arch, _ := guestArch()
	r := currentRecord(arch)
	r.Name = name
	if err := writeRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults(dir)
	cfg.SBX.Executable = "/opt/fake/sbx"
	if err := config.Write(filepath.Join(dir, "warden.json"), cfg); err != nil {
		t.Fatal(err)
	}
	return dir
}

// storeStub unpacks a stub release of version into the store (through
// placeRelease, as install does) and returns its directory.
func storeStub(t *testing.T, version, log string) string {
	t.Helper()
	store, _ := storeDir()
	tarball := releaseTarball(t, t.TempDir(), version, runtime.GOOS, runtime.GOARCH, log)
	c := &cli{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	path, err := c.placeRelease(tarball, false)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != store {
		t.Fatalf("placed at %s, not in the store %s", path, store)
	}
	return path
}

func TestResolveReleaseAcceptsTagsDevVersionsShaPrefixesLatestAndNames(t *testing.T) {
	now := time.Now()
	rs := []storeRelease{
		{Name: "warden-v0.1.0-alpha.13-darwin-arm64", Version: "v0.1.0-alpha.13", Path: "/s/a13", At: now.Add(-3 * time.Hour)},
		{Name: "warden-v0.0.0-dev.0368f857c63c-darwin-arm64", Version: "v0.0.0-dev.0368f857c63c", Path: "/s/dev1", At: now.Add(-time.Hour)},
		{Name: "warden-v0.0.0-dev.0368aaaaaaaa-darwin-arm64", Version: "v0.0.0-dev.0368aaaaaaaa", Path: "/s/dev2", At: now.Add(-2 * time.Hour)},
		{Name: "warden-v0.0.0-dev.0368f857c63c-darwin-arm64", Version: "v0.0.0-dev.0368f857c63c", Path: "/legacy/dev1", At: now.Add(-4 * time.Hour), Legacy: "old"},
	}
	cases := map[string]string{
		"v0.1.0-alpha.13":                     "/s/a13",
		"0.1.0-alpha.13":                      "/s/a13",
		"warden-v0.1.0-alpha.13-darwin-arm64": "/s/a13",
		"v0.0.0-dev.0368f857c63c":             "/s/dev1",
		"0368f857c63c":                        "/s/dev1",
		"0368f8":                              "/s/dev1",
		"v0.0.0-dev.0368f":                    "/s/dev1",
		"latest":                              "/s/dev1",
		"warden-v0.0.0-dev.0368f857c63c-darwin-arm64": "/s/dev1",
		"0368a": "/s/dev2",
	}
	for spec, want := range cases {
		r, err := resolveRelease(rs, spec)
		if err != nil || r.Path != want {
			t.Errorf("resolveRelease(%q) = %q, %v; want %q", spec, r.Path, err, want)
		}
	}
	if _, err := resolveRelease(rs, "0368"); err == nil || !strings.Contains(err.Error(), "several dev releases") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if _, err := resolveRelease(rs, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "no release v9.9.9") {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := resolveRelease(rs, ""); err == nil {
		t.Fatal("empty spec accepted")
	}
	if _, err := resolveRelease(nil, "latest"); err == nil || !strings.Contains(err.Error(), "no releases installed") {
		t.Fatalf("empty store: %v", err)
	}
	// The legacy copy of a store release is the same version, not an
	// ambiguity, and resolves to the store's copy first.
	if r, _ := resolveRelease(rs, "0368f857"); r.Path != "/s/dev1" {
		t.Fatalf("store first: %s", r.Path)
	}
}

func TestAvailableReleasesIncludesAnInstancesLegacyCopies(t *testing.T) {
	fakeHome(t)
	def := fakeInstance(t, defaultInstance)
	other := fakeInstance(t, "old")
	log := filepath.Join(t.TempDir(), "log")
	inStore := storeStub(t, "v9.9.9", log)
	// A legacy release under the other instance, as Part A wrote them.
	legacy := filepath.Join(other, "releases", "warden-v9.9.8-"+runtime.GOOS+"-"+runtime.GOARCH)
	if _, err := copyTree(inStore, legacy); err != nil {
		t.Fatal(err)
	}
	rs, err := availableReleases(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Path != inStore || rs[0].Legacy != "" || rs[1].Path != legacy || rs[1].Legacy != "old" {
		t.Fatalf("availableReleases(old) = %+v", rs)
	}
	// The default instance's legacy directory is the store itself: listed once.
	if rs, _ := availableReleases(def); len(rs) != 1 {
		t.Fatalf("availableReleases(default) = %+v", rs)
	}
	if rs, _ := allReleases([]string{def, other}); len(rs) != 2 {
		t.Fatalf("allReleases = %+v", rs)
	}
	// use of the legacy version links the other instance to its own copy.
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if code := c.run([]string{"release", "use", "9.9.8", "--instance", "old"}); code != 0 {
		t.Fatalf("use (%d):\n%s", code, out.String())
	}
	if target, _ := os.Readlink(filepath.Join(other, "release")); target != legacy {
		t.Fatalf("link: %s", target)
	}
	out.Reset()
	if code := c.run([]string{"release", "list", "--instance", "old"}); code != 0 || !strings.Contains(out.String(), "* v9.9.8") || !strings.Contains(out.String(), "older copy of old; pinned by old") {
		t.Fatalf("list (%d):\n%s", code, out.String())
	}
}

func TestRunningRecordIsStaleWhenItsProcessIsGone(t *testing.T) {
	state := t.TempDir()
	if r, alive, err := readRunning(state); err != nil || alive || r.PID != 0 {
		t.Fatalf("no file: %+v %v %v", r, alive, err)
	}
	me := runningInfo{Version: "v1", Release: "warden-v1-x-y", Binary: "/r/bin/warden", PID: os.Getpid(), StartedAt: time.Now().UTC(), Chat: "127.0.0.1:1", Edge: "127.0.0.1:2"}
	if err := writeRunning(state, me); err != nil {
		t.Fatal(err)
	}
	if r, alive, err := readRunning(state); err != nil || !alive || r.Version != "v1" || r.Edge != "127.0.0.1:2" {
		t.Fatalf("alive: %+v %v %v", r, alive, err)
	}
	dead := me
	dead.PID = 1 << 30
	if err := writeRunning(state, dead); err != nil {
		t.Fatal(err)
	}
	if r, alive, _ := readRunning(state); alive || r.PID != dead.PID {
		t.Fatalf("stale: %+v %v", r, alive)
	}
	// removeRunning only removes this process's own record.
	removeRunning(state)
	if _, err := os.Stat(runningPath(state)); err != nil {
		t.Fatal("another process's record was removed")
	}
	writeRunning(state, me)
	removeRunning(state)
	if _, err := os.Stat(runningPath(state)); err == nil {
		t.Fatal("own record not removed")
	}
	l := &launcher{exe: "/store/warden-v1.2.3-darwin-arm64/bin/warden", cfg: config.Defaults(state)}
	if r := l.runningInfo(time.Unix(0, 0)); r.Release != "warden-v1.2.3-darwin-arm64" || r.Version != revision || r.PID != os.Getpid() || r.Chat != l.cfg.Chat.Listen || r.Edge != l.cfg.Previews.EdgeListen {
		t.Fatalf("runningInfo: %+v", r)
	}
	l.exe, l.withoutEdge = "/checkout/dist/chat/warden", true
	if r := l.runningInfo(time.Unix(0, 0)); r.Release != "" || r.Edge != "" {
		t.Fatalf("runningInfo of a bare build: %+v", r)
	}
}

// warden status without an instance prints the table of every instance
// (running version from running.json against the pinned one), then the
// default instance's detail; with --json the rows; with --instance the
// detail plus a running: line.
func TestStatusTablesEveryInstanceAndDetailsOne(t *testing.T) {
	fakeHome(t)
	def := fakeInstance(t, defaultInstance)
	dev := fakeInstance(t, "dev")
	log := filepath.Join(t.TempDir(), "log")
	a := storeStub(t, "v9.9.8", log)
	b := storeStub(t, "v9.9.9", log)
	relink(def, a)
	relink(dev, a)
	// The default instance runs its pinned release; dev runs b, a trial.
	writeRunning(def, runningInfo{Version: "v9.9.8", Binary: filepath.Join(a, "bin", "warden"), PID: os.Getpid(), StartedAt: time.Now().Add(-90 * time.Minute)})
	writeRunning(dev, runningInfo{Version: "v9.9.9", Binary: filepath.Join(b, "bin", "warden"), PID: os.Getpid(), StartedAt: time.Now().Add(-30 * time.Second)})
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if code := c.run([]string{"status"}); code != 0 {
		t.Fatalf("status (%d):\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{"NAME     RUNNING  PINNED  PID", "default  v9.9.8   v9.9.8  " + fmt.Sprint(os.Getpid()), "dev      v9.9.9   v9.9.8  ", "  1h30m", "  30s", "state:   " + def, "running: v9.9.8 (pid"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
	out.Reset()
	if code := c.run([]string{"status", "--instance", "dev"}); code != 0 || strings.Contains(out.String(), "NAME  ") || !strings.Contains(out.String(), "running: v9.9.9 (pid") || !strings.Contains(out.String(), "the pinned release is v9.9.8 (a trial run") {
		t.Fatalf("status --instance dev (%d):\n%s", code, out.String())
	}
	out.Reset()
	if code := c.run([]string{"status", "--json"}); code != 0 {
		t.Fatalf("status --json (%d):\n%s", code, out.String())
	}
	var rows []instanceInfo
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 2 || rows[1].Name != "dev" || rows[1].Running != "v9.9.9" || rows[1].Release != "v9.9.8" || rows[1].Shape != "foreground" || rows[1].PID != os.Getpid() {
		t.Fatalf("status --json: %v\n%s", err, out.String())
	}
	// A dead pid reads as not running.
	writeRunning(dev, runningInfo{Version: "v9.9.9", PID: 1 << 30, StartedAt: time.Now()})
	out.Reset()
	c.run([]string{"instance", "list"})
	if !strings.Contains(out.String(), "PINNED  RUNNING") || !strings.Contains(out.String(), "dev      "+dev+"  v9.9.8  -  ") {
		t.Fatalf("instance list:\n%s", out.String())
	}
	if uptime(26*time.Hour+3*time.Minute) != "1d2h" || uptime(59*time.Second) != "59s" || uptime(-time.Second) != "0s" {
		t.Fatal("uptime")
	}
}

// fakeGitHub serves a releases listing and the assets it names: a
// release with a tarball for this host and SHA256SUMS, one with a
// tarball but no checksums, one for another platform.
func fakeGitHub(t *testing.T, log string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	good := releaseTarball(t, dir, "v9.9.5", runtime.GOOS, runtime.GOARCH, log)
	unsigned := releaseTarball(t, dir, "v9.9.4", runtime.GOOS, runtime.GOARCH, log)
	raw, _ := os.ReadFile(good)
	sum := sha256.Sum256(raw)
	sums := hex.EncodeToString(sum[:]) + "  " + filepath.Base(good) + "\n" + strings.Repeat("0", 64) + "  warden-v9.9.5-plan9-amd64.tar.gz\n"
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		asset := func(name string) githubAsset {
			return githubAsset{Name: name, URL: srv.URL + "/dl/" + name, Size: 1234}
		}
		json.NewEncoder(w).Encode([]githubRelease{
			{Tag: "v9.9.5", PublishedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Assets: []githubAsset{asset(filepath.Base(good)), asset("SHA256SUMS")}},
			{Tag: "v9.9.4", PublishedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Prerelease: true, Assets: []githubAsset{asset(filepath.Base(unsigned))}},
			{Tag: "v9.9.3", PublishedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Assets: []githubAsset{asset("warden-v9.9.3-plan9-amd64.tar.gz"), asset("SHA256SUMS")}},
			{Tag: "v9.9.2", Draft: true},
		})
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(r.URL.Path)))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	previous := githubReleasesURL
	githubReleasesURL = srv.URL + "/releases"
	t.Cleanup(func() { githubReleasesURL = previous })
	return srv, sums
}

func TestVersionsListsTheStoreAndGitHub(t *testing.T) {
	fakeHome(t)
	def := fakeInstance(t, defaultInstance)
	dev := fakeInstance(t, "dev")
	log := filepath.Join(t.TempDir(), "log")
	a := storeStub(t, "v9.9.8", log)
	relink(def, a)
	relink(dev, a)
	writeRunning(dev, runningInfo{Version: "v9.9.8", Binary: filepath.Join(a, "bin", "warden"), PID: os.Getpid(), StartedAt: time.Now()})
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if code := c.run([]string{"versions"}); code != 0 {
		t.Fatalf("versions (%d):\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "VERSION  INSTALLED") || !strings.Contains(out.String(), "v9.9.8   ") || !strings.Contains(out.String(), "default,dev  dev         store") {
		t.Fatalf("versions:\n%s", out.String())
	}
	fakeGitHub(t, log)
	out.Reset()
	if code := c.run([]string{"versions", "--remote"}); code != 0 {
		t.Fatalf("versions --remote (%d):\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{"GitHub releases (monaddle-too/warden):", "v9.9.5                2026-09-01  yes", "v9.9.4 (pre-release)  2026-08-01  yes (no SHA256SUMS: not installable)", "v9.9.3                2026-07-01  no tarball for " + runtime.GOOS} {
		if !strings.Contains(text, want) {
			t.Fatalf("versions --remote lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "v9.9.2") {
		t.Fatalf("a draft was listed:\n%s", text)
	}
	out.Reset()
	if code := c.run([]string{"versions", "--json", "--remote"}); code != 0 {
		t.Fatalf("versions --json (%d):\n%s", code, out.String())
	}
	var got struct {
		Installed []versionRow
		Remote    []remoteRow
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Installed) != 1 || len(got.Remote) != 3 || !got.Remote[0].Checksums || got.Remote[1].Checksums || got.Installed[0].RunningOn[0] != "dev" {
		t.Fatalf("versions --json: %v\n%s", err, out.String())
	}
	// GitHub unreachable: one line, the local listing intact.
	githubReleasesURL = "http://127.0.0.1:1/releases"
	out.Reset()
	if code := c.run([]string{"versions", "--remote"}); code != 0 || !strings.Contains(out.String(), "v9.9.8") || !strings.Contains(out.String(), "GitHub releases (monaddle-too/warden): unreachable:") {
		t.Fatalf("offline (%d):\n%s", code, out.String())
	}
}

func TestReleaseInstallDownloadsAGitHubTagAndChecksIt(t *testing.T) {
	state := installedState(t)
	log := filepath.Join(filepath.Dir(state), "warden.log")
	srv, _ := fakeGitHub(t, log)
	var out bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
		return c.run(args)
	}
	if code := run("release", "install", "v9.9.5", "--state", state); code != 0 || !strings.Contains(out.String(), "SHA256 verified") {
		t.Fatalf("install a tag (%d):\n%s", code, out.String())
	}
	store, _ := storeDir()
	dest := filepath.Join(store, "warden-v9.9.5-"+runtime.GOOS+"-"+runtime.GOARCH)
	if target, _ := os.Readlink(filepath.Join(state, "release")); target != dest {
		t.Fatalf("link: %s", target)
	}
	if raw, _ := os.ReadFile(log); !strings.Contains(string(raw), "install --state "+state+" --upgrade") {
		t.Fatalf("install step: %s", raw)
	}
	if entries, _ := os.ReadDir(store); len(entries) != 1 {
		t.Fatalf("store holds %v", entries)
	}
	if code := run("release", "install", "9.9.5", "--state", state); code != 0 || !strings.Contains(out.String(), "already in the store") {
		t.Fatalf("second install (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", "v9.9.4", "--state", state); code == 0 || !strings.Contains(out.String(), "has no SHA256SUMS") {
		t.Fatalf("unverifiable release (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", "v9.9.3", "--state", state); code == 0 || !strings.Contains(out.String(), "no tarball for") {
		t.Fatalf("other platform (%d):\n%s", code, out.String())
	}
	if code := run("release", "install", "v1.0.0", "--state", state); code == 0 || !strings.Contains(out.String(), "neither a file nor a GitHub release") {
		t.Fatalf("unknown tag (%d):\n%s", code, out.String())
	}
	// A mismatching digest is refused and nothing is left behind.
	if want, err := checksumFor("abc  x.tar.gz\n", "x.tar.gz"); err != nil || want != "abc" {
		t.Fatal(want, err)
	}
	if _, err := checksumFor("abc  x.tar.gz\n", "y.tar.gz"); err == nil {
		t.Fatal("missing line")
	}
	path := filepath.Join(t.TempDir(), "dl")
	if err := downloadTo(t.Context(), srv.URL+"/dl/SHA256SUMS", path, "00"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the mismatching download was kept")
	}
}
