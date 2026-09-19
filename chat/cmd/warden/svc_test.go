package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/config"
)

// The tests never reach launchctl or systemctl: the default manager is
// none, and the tests that want one install a fake. Nor do they reach the
// person's own home: the default state directory (and with it the
// release store, ~/.warden/releases) is under a temporary home for the
// whole run (a release test once unpacked its stub into the live store).
func TestMain(m *testing.M) {
	defaultServiceFn = func(string) (serviceManager, string) { return nil, "no service manager in tests" }
	defaultMenuFn = func(string) (serviceManager, string) { return nil, "" }
	home, err := os.MkdirTemp("/tmp", "wtest")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_DATA_HOME", home)
	os.Setenv(instanceEnv, "")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// newFakeMenu is a fake manager for the menu bar item, with its own unit
// file and label, and a fake warden-menu beside the launcher for
// menuExecutable to find.
func newFakeMenu(t *testing.T, state string) *fakeService {
	t.Helper()
	f := newFakeService(t, state)
	f.kindName, f.labelName = "fake menu", "com.example.warden.menu"
	fake := filepath.Join(state, "fake-warden-menu")
	os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755)
	previous := menuExecutable
	menuExecutable = func(string) string { return fake }
	t.Cleanup(func() { menuExecutable = previous })
	return f
}

// fakeService records what the launcher asks of the manager and plays
// the unit's state; onStart runs when the service starts (the fixture
// uses it to write the endpoint file the way the chat service does).
type fakeService struct {
	mu      sync.Mutex
	dir     string
	calls   []string
	st      serviceStatus
	pids    int
	onStart func()
	fail    map[string]error
	note    []string
	// kindName and labelName override the fake's kind and label (the
	// menu bar item's fake).
	kindName, labelName string
}

func newFakeService(t *testing.T, state string) *fakeService {
	t.Helper()
	f := &fakeService{dir: filepath.Join(state, "fake-launchagents")}
	os.MkdirAll(f.dir, 0o700)
	return f
}

func (f *fakeService) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if err := f.fail[call]; err != nil {
		return err
	}
	return nil
}
func (f *fakeService) kind() string {
	if f.kindName != "" {
		return f.kindName
	}
	return "fake agent"
}
func (f *fakeService) label() string {
	if f.labelName != "" {
		return f.labelName
	}
	return "com.example.warden"
}
func (f *fakeService) unitPath() string { return filepath.Join(f.dir, f.label()+".plist") }
func (f *fakeService) unit(exe, configPath string) string {
	return "unit " + exe + " " + configPath + "\n"
}
func (f *fakeService) registered() bool { _, err := os.Stat(f.unitPath()); return err == nil }
func (f *fakeService) hints() []string  { return f.note }
func (f *fakeService) status() serviceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *fakeService) run() {
	f.mu.Lock()
	f.pids++
	f.st = serviceStatus{Loaded: true, Running: true, PID: f.pids}
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart()
	}
}
func (f *fakeService) install(unit string) (string, error) {
	previous, _ := os.ReadFile(f.unitPath())
	changed := string(previous) != unit
	if err := f.record("install"); err != nil {
		return "", err
	}
	if changed {
		os.WriteFile(f.unitPath(), []byte(unit), 0o644)
	}
	if f.status().Running && !changed {
		return "already registered and " + f.status().String(), nil
	}
	f.run()
	return "registered and started", nil
}
func (f *fakeService) uninstall() error {
	if err := f.record("uninstall"); err != nil {
		return err
	}
	f.mu.Lock()
	f.st = serviceStatus{}
	f.mu.Unlock()
	return os.Remove(f.unitPath())
}
func (f *fakeService) start() error {
	if err := f.record("start"); err != nil {
		return err
	}
	f.run()
	return nil
}
func (f *fakeService) stop() error {
	if err := f.record("stop"); err != nil {
		return err
	}
	f.mu.Lock()
	f.st = serviceStatus{Loaded: true}
	f.mu.Unlock()
	return nil
}
func (f *fakeService) restart() error {
	if err := f.record("restart"); err != nil {
		return err
	}
	f.run()
	return nil
}

// endpointWriter makes the fake's start write the chat endpoint file, so
// awaitReady sees the stack come up.
func endpointWriter(cfg config.Config) func() {
	return func() {
		os.MkdirAll(filepath.Dir(cfg.OwnerTokenFile()), 0o700)
		os.WriteFile(cfg.OwnerTokenFile(), []byte(fmt.Sprintf(`{"url":"http://127.0.0.1:18780","token":"%d"}`, time.Now().UnixNano())), 0o600)
	}
}

// The launchd plist and the systemd unit: this launcher, this config, run
// at login / wanted by the session, restarted after a failure only.
func TestUnitFilesRunTheLauncherAsAService(t *testing.T) {
	a := &launchdAgent{name: "com.monaddle.warden", path: "/Users/o/Library/LaunchAgents/com.monaddle.warden.plist", domain: "gui/501"}
	plist := a.unit("/Users/o/.warden/release/bin/warden", "/Users/o/.warden/warden.json")
	for _, want := range []string{
		"<string>com.monaddle.warden</string>",
		"<string>/Users/o/.warden/release/bin/warden</string>\n\t\t<string>start</string>\n\t\t<string>--service</string>\n\t\t<string>--config</string>\n\t\t<string>/Users/o/.warden/warden.json</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>",
		"<key>WorkingDirectory</key>\n\t<string>/Users/o/.warden</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "StandardOutPath") {
		t.Fatal("launchd must not hold the log: the launcher rotates it")
	}
	escaped := a.unit("/Users/o/My <Apps>/warden", "/Users/o/.warden/warden.json")
	if !strings.Contains(escaped, "<string>/Users/o/My &lt;Apps&gt;/warden</string>") {
		t.Fatalf("path not escaped:\n%s", escaped)
	}
	u := &systemdUnit{name: "warden", path: "/home/o/.config/systemd/user/warden.service"}
	unit := u.unit("/home/o/.local/share/warden/release/bin/warden", "/home/o/.local/share/warden/warden.json")
	for _, want := range []string{
		"ExecStart=/home/o/.local/share/warden/release/bin/warden start --service --config /home/o/.local/share/warden/warden.json",
		"Restart=on-failure",
		"WantedBy=default.target",
		"TimeoutStopSec=90",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	if quoted := u.unit("/home/o/my apps/warden", "/home/o/w.json"); !strings.Contains(quoted, `ExecStart="/home/o/my apps/warden" start`) {
		t.Fatalf("path not quoted:\n%s", quoted)
	}
}

// Labels: the default state is com.monaddle.warden / warden; any other
// carries its basename so cloned homes register beside it.
func TestServiceNamesFollowTheStateDirectory(t *testing.T) {
	def, _ := defaultStateDir()
	if label, unit := serviceNames(def); label != "com.monaddle.warden" || unit != "warden" {
		t.Fatalf("%s %s", label, unit)
	}
	if label, unit := serviceNames("/Users/o/.warden-p1"); label != "com.monaddle.warden.warden-p1" || unit != "warden-warden-p1" {
		t.Fatalf("%s %s", label, unit)
	}
	if label, _ := serviceNames("/tmp/wd 2/state"); label != "com.monaddle.warden.state" {
		t.Fatalf("%s", label)
	}
}

// The launchd manager drives launchctl: bootstrap on a first install,
// print for the status, kill TERM to stop, kickstart -k to restart,
// bootout and the file's removal to uninstall; a changed plist is
// re-bootstrapped.
func TestLaunchdAgentCommands(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	running := false
	a := &launchdAgent{name: "com.monaddle.warden.t", path: filepath.Join(dir, "com.monaddle.warden.t.plist"), domain: "gui/501"}
	a.run = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.HasPrefix(call, "launchctl print"):
			if !a.registered() {
				return "", errors.New("Could not find service")
			}
			if running {
				// launchctl nests endpoint blocks with their own "state = active" lines after the service's.
				return "com.monaddle.warden.t = {\n\tstate = running\n\tpid = 4242\n\tpid-local endpoints = {\n\t\t\"x\" = {\n\t\t\tstate = active\n\t\t}\n\t}\n}\n", nil
			}
			return "com.monaddle.warden.t = {\n\tstate = not running\n}\n", nil
		case strings.HasPrefix(call, "launchctl bootstrap"), strings.HasPrefix(call, "launchctl kickstart"):
			running = true
		case strings.HasPrefix(call, "launchctl kill"), strings.HasPrefix(call, "launchctl bootout"):
			running = false
		}
		return "", nil
	}
	unit := a.unit("/x/bin/warden", "/x/warden.json")
	if result, err := a.install(unit); err != nil || !strings.HasPrefix(result, "registered ") {
		t.Fatalf("%q %v", result, err)
	}
	if st := a.status(); !st.Running || st.PID != 4242 {
		t.Fatalf("%+v", st)
	}
	if result, err := a.install(unit); err != nil || result != "already registered and running (pid 4242)" {
		t.Fatalf("%q %v", result, err)
	}
	if err := a.stop(); err != nil || a.status().Running {
		t.Fatal(err)
	}
	if err := a.start(); err != nil || !a.status().Running {
		t.Fatal(err)
	}
	if err := a.restart(); err != nil {
		t.Fatal(err)
	}
	if result, err := a.install(a.unit("/y/bin/warden", "/x/warden.json")); err != nil || !strings.HasPrefix(result, "re-registered") {
		t.Fatalf("%q %v", result, err)
	}
	if err := a.uninstall(); err != nil || a.registered() {
		t.Fatal(err)
	}
	want := []string{
		"launchctl print gui/501/com.monaddle.warden.t", // install: status before
		"launchctl bootstrap gui/501 " + a.path,
		"launchctl print gui/501/com.monaddle.warden.t", // status
		"launchctl print gui/501/com.monaddle.warden.t", // second install
		"launchctl print gui/501/com.monaddle.warden.t", // stop: status
		"launchctl kill TERM gui/501/com.monaddle.warden.t",
		"launchctl print gui/501/com.monaddle.warden.t", // awaitStopped
		"launchctl print gui/501/com.monaddle.warden.t", // status
		"launchctl print gui/501/com.monaddle.warden.t", // start: status
		"launchctl kickstart gui/501/com.monaddle.warden.t",
		"launchctl print gui/501/com.monaddle.warden.t", // status
		"launchctl print gui/501/com.monaddle.warden.t", // restart: status
		"launchctl kickstart -k gui/501/com.monaddle.warden.t",
		"launchctl print gui/501/com.monaddle.warden.t", // changed install: status
		"launchctl bootout gui/501/com.monaddle.warden.t",
		"launchctl print gui/501/com.monaddle.warden.t", // awaitStopped
		"launchctl bootstrap gui/501 " + a.path,
		"launchctl print gui/501/com.monaddle.warden.t", // uninstall: status
		"launchctl print gui/501/com.monaddle.warden.t", // stop: status
		"launchctl kill TERM gui/501/com.monaddle.warden.t",
		"launchctl print gui/501/com.monaddle.warden.t", // awaitStopped
		"launchctl bootout gui/501/com.monaddle.warden.t",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// The systemd manager: the unit is written, daemon-reload, enable --now;
// a changed unit on a running service restarts it; show answers the
// status; uninstall disables and removes.
func TestSystemdUnitCommands(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	active := false
	u := &systemdUnit{name: "warden-t", path: filepath.Join(dir, "warden-t.service")}
	u.run = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.HasPrefix(call, "systemctl --user show"):
			state := "inactive"
			pid := 0
			if active {
				state, pid = "active", 777
			}
			return fmt.Sprintf("LoadState=loaded\nActiveState=%s\nMainPID=%d\n", state, pid), nil
		case strings.Contains(call, "enable --now"), strings.HasSuffix(call, " start warden-t.service"), strings.HasSuffix(call, " restart warden-t.service"):
			active = true
		case strings.HasSuffix(call, " stop warden-t.service"), strings.Contains(call, "disable --now"):
			active = false
		case strings.HasPrefix(call, "loginctl"):
			return "Linger=no\n", nil
		}
		return "", nil
	}
	unit := u.unit("/x/bin/warden", "/x/warden.json")
	if result, err := u.install(unit); err != nil || !strings.HasPrefix(result, "registered ") {
		t.Fatalf("%q %v", result, err)
	}
	if st := u.status(); !st.Running || st.PID != 777 {
		t.Fatalf("%+v", st)
	}
	if result, err := u.install(u.unit("/y/bin/warden", "/x/warden.json")); err != nil || result != "re-registered (the unit changed) and restarted" {
		t.Fatalf("%q %v", result, err)
	}
	if h := u.hints(); len(h) != 1 || !strings.Contains(h[0], "loginctl enable-linger") {
		t.Fatalf("%v", h)
	}
	if err := u.stop(); err != nil || u.status().Running {
		t.Fatal(err)
	}
	if err := u.uninstall(); err != nil || u.registered() {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"systemctl --user daemon-reload\nsystemctl --user enable --now warden-t.service", "systemctl --user restart warden-t.service", "systemctl --user stop warden-t.service", "systemctl --user disable --now warden-t.service"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls lack %q:\n%s", want, joined)
		}
	}
}

// warden install registers the service and starts it; a re-run leaves a
// running one alone; --service=false skips; without a manager the step
// says so and points at --detach.
func TestInstallRegistersAndStartsTheService(t *testing.T) {
	f := newFixture(t)
	svc := newFakeService(t, f.state)
	svc.onStart = endpointWriter(config.Defaults(f.state))
	f.serviceFn = func(string) (serviceManager, string) { return svc, "" }
	code, out := f.install()
	if code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "service:         fake agent com.example.warden: registered and started (running (pid 1))") || !strings.Contains(out, "Next: `warden login codex` (and `warden login claude`, `warden login github` as needed), then `warden open`.") {
		t.Fatalf("output:\n%s", out)
	}
	exe, _ := os.Executable()
	if unit, _ := os.ReadFile(svc.unitPath()); string(unit) != "unit "+exe+" "+filepath.Join(f.state, "warden.json")+"\n" {
		t.Fatalf("unit: %s", unit)
	}
	if code, out = f.install(); code != 0 || !strings.Contains(out, "service:         fake agent com.example.warden: already registered and running (pid 1)") {
		t.Fatalf("re-run (%d):\n%s", code, out)
	}
	if strings.Join(svc.calls, " ") != "install install" {
		t.Fatalf("calls %v", svc.calls)
	}
	// A new release in the same unit: restarted so the new one runs.
	arch, _ := guestArch()
	older := currentRecord(arch)
	older.Warden = "v0.0.0-older"
	if err := writeRecord(f.state, older); err != nil {
		t.Fatal(err)
	}
	if code, out = f.install(); code != 0 || !strings.Contains(out, "restarted on "+revision) {
		t.Fatalf("upgrade (%d):\n%s", code, out)
	}
	if svc.calls[len(svc.calls)-1] != "restart" {
		t.Fatalf("calls %v", svc.calls)
	}

	g := newFixture(t)
	g.serviceFn = func(string) (serviceManager, string) { return newFakeService(t, g.state), "" }
	if code, out = g.install("--service=false"); code != 0 || !strings.Contains(out, "service:         not registered (--service=false)") || !strings.Contains(out, "then `warden start` and `warden open`") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	h := newFixture(t)
	if code, out = h.install(); code != 0 || !strings.Contains(out, "service:         not registered (no service manager in tests); `warden start --detach` runs Warden in the background") {
		t.Fatalf("(%d):\n%s", code, out)
	}
}

// The menu bar item's plist: warden-menu beside the launcher, told the
// launcher and the config, in the GUI session only, run at login and
// restarted after a crash.
func TestMenuUnitRunsTheItemBesideTheLauncher(t *testing.T) {
	a := &launchdAgent{name: "com.monaddle.warden.menu", path: "/Users/o/Library/LaunchAgents/com.monaddle.warden.menu.plist", domain: "gui/501", menu: true}
	plist := a.unit("/Users/o/.warden/release/bin/warden", "/Users/o/.warden/warden.json")
	for _, want := range []string{
		"<string>com.monaddle.warden.menu</string>",
		"<string>/Users/o/.warden/release/bin/warden-menu</string>\n\t\t<string>--warden</string>\n\t\t<string>/Users/o/.warden/release/bin/warden</string>\n\t\t<string>--config</string>\n\t\t<string>/Users/o/.warden/warden.json</string>",
		"<key>LimitLoadToSessionType</key>\n\t<string>Aqua</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<string>start</string>") || a.kind() != "menu bar item" {
		t.Fatalf("the menu plist runs the launcher, or the kind is off (%q):\n%s", a.kind(), plist)
	}
	if m, reason := platformMenu("/x"); runtime.GOOS != "darwin" && (m != nil || reason != "") {
		t.Fatalf("the item applies off macOS: %v %q", m, reason)
	}
}

// warden install registers the menu bar item beside the service, leaves
// a running one alone, restarts it on an upgrade, skips it with
// --menu=false, and says so when the build has no warden-menu.
func TestInstallRegistersTheMenuBarItem(t *testing.T) {
	f := newFixture(t)
	svc := newFakeService(t, f.state)
	svc.onStart = endpointWriter(config.Defaults(f.state))
	f.serviceFn = func(string) (serviceManager, string) { return svc, "" }
	menu := newFakeMenu(t, f.state)
	f.menuFn = func(string) (serviceManager, string) { return menu, "" }
	code, out := f.install()
	if code != 0 || !strings.Contains(out, "menu bar:        com.example.warden.menu: registered and started (running (pid 1))") {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	exe, _ := os.Executable()
	if unit, _ := os.ReadFile(menu.unitPath()); string(unit) != "unit "+exe+" "+filepath.Join(f.state, "warden.json")+"\n" {
		t.Fatalf("unit: %s", unit)
	}
	if code, out = f.install(); code != 0 || !strings.Contains(out, "menu bar:        com.example.warden.menu: already registered and running (pid 1)") {
		t.Fatalf("re-run (%d):\n%s", code, out)
	}
	arch, _ := guestArch()
	older := currentRecord(arch)
	older.Warden = "v0.0.0-older"
	if err := writeRecord(f.state, older); err != nil {
		t.Fatal(err)
	}
	if code, out = f.install(); code != 0 || !strings.Contains(out, "menu bar:        com.example.warden.menu: restarted on "+revision) {
		t.Fatalf("upgrade (%d):\n%s", code, out)
	}
	if strings.Join(menu.calls, " ") != "install install install restart" {
		t.Fatalf("calls %v", menu.calls)
	}
	if code, out = f.install("--menu=false"); code != 0 || !strings.Contains(out, "menu bar:        not registered (--menu=false); `warden menu install` registers it later") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	// A build without the item.
	menuExecutable = func(string) string { return "" }
	if code, out = f.install(); code != 0 || !strings.Contains(out, "menu bar:        not registered: no warden-menu beside "+exe+" (a release built without swiftc)") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	// Not on this host: no row.
	g := newFixture(t)
	if code, out = g.install(); code != 0 || strings.Contains(out, "menu bar:") {
		t.Fatalf("(%d):\n%s", code, out)
	}
}

// warden menu install|uninstall drive the item's manager; service install
// registers it too and service uninstall / warden uninstall remove it;
// restart restarts it; status and doctor report it.
func TestMenuInstallUninstallAndTheServiceCommands(t *testing.T) {
	state, configPath := loginFixture(t)
	cfg := config.Defaults(state)
	svc := newFakeService(t, state)
	svc.onStart = endpointWriter(cfg)
	menu := newFakeMenu(t, state)
	run := func(args ...string) (int, string) {
		var out strings.Builder
		c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
		c.serviceFn = func(string) (serviceManager, string) { return svc, "" }
		c.menuFn = func(string) (serviceManager, string) { return menu, "" }
		return c.run(append(args, "--config", configPath)), out.String()
	}
	if code, out := run("status"); code != 0 || !strings.Contains(out, "menu:    not registered; `warden menu install` registers the menu bar item") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("menu", "install"); code != 0 || !strings.Contains(out, "menu:    com.example.warden.menu: registered and started (running (pid 1))") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("status"); code != 0 || !strings.Contains(out, "menu:    com.example.warden.menu: running (pid 1) ("+menu.unitPath()+")") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("menu", "uninstall"); code != 0 || !strings.Contains(out, "menu:    stopped and unregistered the menu bar item") || menu.registered() {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("menu", "uninstall"); code != 0 || !strings.Contains(out, "no menu bar item registered") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	// service install registers both; restart restarts both; service
	// uninstall removes both.
	if code, out := run("service", "install"); code != 0 || !strings.Contains(out, "service: fake agent com.example.warden: registered and started") || !strings.Contains(out, "menu:    com.example.warden.menu: registered and started (running (pid 2))") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("restart"); code != 0 || !strings.Contains(out, "Warden restarted") || menu.calls[len(menu.calls)-1] != "restart" {
		t.Fatalf("(%d):\n%s\n%v", code, out, menu.calls)
	}
	if code, out := run("stop"); code != 0 || !menu.status().Running {
		t.Fatalf("stop must leave the item (%d):\n%s", code, out)
	}
	if code, out := run("service", "uninstall"); code != 0 || !strings.Contains(out, "menu:    stopped and unregistered the menu bar item") || menu.registered() || svc.registered() {
		t.Fatalf("(%d):\n%s", code, out)
	}
	// Off macOS: menu install says so.
	var out strings.Builder
	none := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	none.serviceFn = func(string) (serviceManager, string) { return svc, "" }
	if code := none.run([]string{"menu", "install", "--config", configPath}); code != 1 || !strings.Contains(out.String(), "no menu bar item here (the menu bar item is macOS only)") {
		t.Fatalf("(%d):\n%s", code, out.String())
	}
}

// doctor: the menu bar item is a pass when not registered or not in the
// build, a failure when its unit is stale or not loaded; no row off macOS.
func TestDoctorChecksTheMenuBarItem(t *testing.T) {
	state, configPath := loginFixture(t)
	menu := newFakeMenu(t, state)
	var out strings.Builder
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	c.menuFn = func(string) (serviceManager, string) { return menu, "" }
	cfg := config.Defaults(state)
	if ck, ok := c.menuCheck(cfg, configPath); !ok || !ck.OK || !strings.Contains(ck.Detail, "not registered; `warden menu install`") {
		t.Fatalf("%+v %v", ck, ok)
	}
	os.WriteFile(menu.unitPath(), []byte("unit /somewhere/else/warden "+configPath+"\n"), 0o644)
	if ck, _ := c.menuCheck(cfg, configPath); ck.OK || !strings.Contains(ck.Detail, "does not run this launcher's item") {
		t.Fatalf("%+v", ck)
	}
	exe, _ := os.Executable()
	os.WriteFile(menu.unitPath(), []byte(menu.unit(exe, configPath)), 0o644)
	if ck, _ := c.menuCheck(cfg, configPath); ck.OK || !strings.Contains(ck.Detail, "registered but not loaded") {
		t.Fatalf("%+v", ck)
	}
	menu.st = serviceStatus{Loaded: true, Running: true, PID: 4}
	if ck, _ := c.menuCheck(cfg, configPath); !ck.OK || ck.Detail != "com.example.warden.menu: running (pid 4)" {
		t.Fatalf("%+v", ck)
	}
	os.Remove(menu.unitPath())
	menuExecutable = func(string) string { return "" }
	if ck, _ := c.menuCheck(cfg, configPath); !ck.OK || !strings.Contains(ck.Detail, "not in this build") {
		t.Fatalf("%+v", ck)
	}
	none := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if _, ok := none.menuCheck(cfg, configPath); ok {
		t.Fatal("a row where the item does not apply")
	}
}

// With a service registered, start/stop/restart/status drive the manager
// and wait for the endpoint; the detached path stays for hosts without
// one; `warden service uninstall` unregisters and `service install`
// registers again.
func TestStartStopRestartStatusDriveTheService(t *testing.T) {
	state, configPath := loginFixture(t)
	cfg := config.Defaults(state)
	svc := newFakeService(t, state)
	svc.onStart = endpointWriter(cfg)
	run := func(stdin string, args ...string) (int, string) {
		var out strings.Builder
		c := &cli{stdin: strings.NewReader(stdin), stdout: &out, stderr: &out}
		c.serviceFn = func(string) (serviceManager, string) { return svc, "" }
		return c.run(append(args, "--config", configPath)), out.String()
	}
	// Not registered: status says how to register; stop finds nothing.
	if code, out := run("", "status"); code != 0 || !strings.Contains(out, "service: not registered; `warden service install` registers a fake agent") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "stop"); code != 1 || !strings.Contains(out, "not running in the background") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "service", "install"); code != 0 || !strings.Contains(out, "service: fake agent com.example.warden: registered and started (running (pid 1))") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "status"); code != 0 || !strings.Contains(out, "service: fake agent com.example.warden: running (pid 1) ("+svc.unitPath()+")") || !strings.Contains(out, "log:     "+filepath.Join(state, "warden.log")) {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "start"); code != 0 || !strings.Contains(out, "already running as a fake agent (running (pid 1))") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "stop"); code != 0 || !strings.Contains(out, "Warden stopped (fake agent com.example.warden; it starts again at the next login, or with `warden start`)") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "status"); code != 0 || !strings.Contains(out, "com.example.warden: loaded, not running") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "start", "--detach"); code != 0 || !strings.Contains(out, "a service is registered; starting it") || !strings.Contains(out, "Warden started as a fake agent (running (pid 2)") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "restart"); code != 0 || !strings.Contains(out, "Warden restarted as a fake agent (running (pid 3))") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if code, out := run("", "service", "uninstall"); code != 0 || !strings.Contains(out, "stopped and unregistered") || svc.registered() {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if strings.Join(svc.calls, " ") != "install stop start restart uninstall" {
		t.Fatalf("calls %v", svc.calls)
	}
	// A manager that fails to start reports the error.
	svc.fail = map[string]error{"install": errors.New("Bootstrap failed: 5: Input/output error")}
	if code, out := run("", "service", "install"); code != 1 || !strings.Contains(out, "Bootstrap failed") {
		t.Fatalf("(%d):\n%s", code, out)
	}
}

// doctor: no service is a pass with the hint; a registered one must be
// this launcher's unit and loaded.
func TestDoctorChecksTheService(t *testing.T) {
	state, configPath := loginFixture(t)
	svc := newFakeService(t, state)
	var out strings.Builder
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	c.serviceFn = func(string) (serviceManager, string) { return svc, "" }
	cfg := config.Defaults(state)
	if ck := c.serviceCheck(cfg, configPath); !ck.OK || !strings.Contains(ck.Detail, "not registered; `warden service install`") {
		t.Fatalf("%+v", ck)
	}
	os.WriteFile(svc.unitPath(), []byte("unit /somewhere/else/warden "+configPath+"\n"), 0o644)
	if ck := c.serviceCheck(cfg, configPath); ck.OK || !strings.Contains(ck.Detail, "does not run this launcher") || ck.Fix != "run `warden service install` from the launcher the service should run" {
		t.Fatalf("%+v", ck)
	}
	exe, _ := os.Executable()
	os.WriteFile(svc.unitPath(), []byte(svc.unit(exe, configPath)), 0o644)
	if ck := c.serviceCheck(cfg, configPath); ck.OK || !strings.Contains(ck.Detail, "registered but not loaded") {
		t.Fatalf("%+v", ck)
	}
	svc.st = serviceStatus{Loaded: true}
	if ck := c.serviceCheck(cfg, configPath); !ck.OK || ck.Detail != "fake agent com.example.warden: loaded, not running" {
		t.Fatalf("%+v", ck)
	}
	none := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if ck := none.serviceCheck(cfg, configPath); !ck.OK || !strings.Contains(ck.Detail, "none on this host (no service manager in tests)") {
		t.Fatalf("%+v", ck)
	}
}

// Logs rotate at the size limit when the launcher starts: .1, .2, .3 kept.
func TestRotateLogKeepsThreeGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warden.log")
	big := strings.Repeat("x", logRotateSize)
	os.WriteFile(path, []byte("small"), 0o600)
	rotateLog(path)
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("a small log rotated")
	}
	for i := 1; i <= 4; i++ {
		os.WriteFile(path, []byte(big+fmt.Sprint(i)), 0o600)
		rotateLog(path)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the rotated log is still there")
	}
	for i, want := range []string{"1", "2", "3"} {
		raw, err := os.ReadFile(fmt.Sprintf("%s.%d", path, i+1))
		if err != nil || !strings.HasSuffix(string(raw), fmt.Sprint(4-i)) {
			t.Fatalf(".%d: %v %q", i+1, err, want)
		}
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Fatal("a fourth generation was kept")
	}
}
