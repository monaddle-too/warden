package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// serviceManager is the platform's per-user service manager, the thing
// that keeps Warden running like any other service on the machine: a
// launchd agent on macOS (starts at login, restarted after a crash), a
// systemd user unit on Linux (the same, from boot with lingering). The
// launcher itself is unchanged; the manager runs `warden start --service`
// and the commands here register, start, stop and inspect that unit
// (docs/background-service-plan.md).
type serviceManager interface {
	kind() string     // "launchd agent" or "systemd user unit"
	label() string    // the unit's name at the manager
	unitPath() string // the unit file
	// unit renders the unit file for the launcher at exe and the config at
	// configPath.
	unit(exe, configPath string) string
	registered() bool
	// install writes the unit (when it differs), loads it and makes sure
	// it is running; it reports what it did in one short phrase.
	install(unit string) (string, error)
	// uninstall stops and unloads the unit and removes the file.
	uninstall() error
	start() error
	stop() error // returns once the service has stopped
	restart() error
	status() serviceStatus
	// hints are one-line notes the platform wants the operator to know
	// after registering (lingering on Linux).
	hints() []string
}

// serviceStatus is what the manager knows about the unit.
type serviceStatus struct {
	Loaded  bool // the manager knows the unit
	Running bool
	PID     int
}

func (s serviceStatus) String() string {
	switch {
	case s.Running && s.PID > 0:
		return fmt.Sprintf("running (pid %d)", s.PID)
	case s.Running:
		return "running"
	case s.Loaded:
		return "loaded, not running"
	default:
		return "not loaded"
	}
}

// commandRunner runs a manager command and returns its combined output;
// tests substitute one that records the calls.
type commandRunner func(name string, args ...string) (string, error)

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return out.String(), nil
	case <-time.After(2 * time.Minute):
		cmd.Process.Kill()
		<-done
		return out.String(), fmt.Errorf("%s %s: timed out", name, strings.Join(args, " "))
	}
}

// serviceNames derives the launchd label and the systemd unit name from
// the state directory: the default one is com.monaddle.warden / warden,
// any other carries its basename so several Warden homes coexist.
func serviceNames(state string) (label, unitName string) {
	def, _ := defaultStateDir()
	if def != "" && filepath.Clean(def) == filepath.Clean(state) {
		return "com.monaddle.warden", "warden"
	}
	base := unsafeNameChars.ReplaceAllString(strings.TrimPrefix(filepath.Base(state), "."), "-")
	if base == "" {
		base = "state"
	}
	return "com.monaddle.warden." + base, "warden-" + base
}

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// platformService is the manager for this host, or nil with the reason
// there is none (then `warden start --detach` is the background option).
func platformService(state string) (serviceManager, string) {
	label, unitName := serviceNames(state)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "no home directory: " + err.Error()
	}
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return nil, "launchctl not found"
		}
		return &launchdAgent{name: label, path: filepath.Join(home, "Library", "LaunchAgents", label+".plist"), domain: "gui/" + strconv.Itoa(os.Getuid()), run: runCommand}, ""
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return nil, "systemctl not found"
		}
		if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
			return nil, "no user session bus (XDG_RUNTIME_DIR unset); systemd --user is not reachable from here"
		}
		dir := os.Getenv("XDG_CONFIG_HOME")
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(home, ".config")
		}
		return &systemdUnit{name: unitName, path: filepath.Join(dir, "systemd", "user", unitName+".service"), run: runCommand}, ""
	default:
		return nil, "no service manager on " + runtime.GOOS
	}
}

// service returns the manager for a state directory, or nil and the reason.
func (c *cli) service(state string) (serviceManager, string) {
	if c.serviceFn != nil {
		return c.serviceFn(state)
	}
	return defaultServiceFn(state)
}

// defaultServiceFn is platformService; the tests replace it so no test
// reaches launchctl or systemctl by accident.
var defaultServiceFn = platformService

// platformMenu is the manager for the menu bar item (docs/menu-bar-plan.md):
// a second launchd agent, `<label>.menu`, running `warden-menu` from
// beside the launcher. nil with no reason where the item does not apply
// (not macOS), nil with the reason where it cannot be registered.
func platformMenu(state string) (serviceManager, string) {
	if runtime.GOOS != "darwin" {
		return nil, ""
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return nil, "launchctl not found"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "no home directory: " + err.Error()
	}
	label, _ := serviceNames(state)
	label += ".menu"
	return &launchdAgent{name: label, path: filepath.Join(home, "Library", "LaunchAgents", label+".plist"), domain: "gui/" + strconv.Itoa(os.Getuid()), run: runCommand, menu: true}, ""
}

// menu returns the menu bar item's manager, as service does the service's.
func (c *cli) menu(state string) (serviceManager, string) {
	if c.menuFn != nil {
		return c.menuFn(state)
	}
	return defaultMenuFn(state)
}

var defaultMenuFn = platformMenu

// menuExecutable is where the menu bar item is beside the launcher, as
// the tarball lays them out (bin/warden, bin/warden-menu); "" when this
// build has none (a release built without swiftc). A test points it at
// a file of its own.
var menuExecutable = func(exe string) string {
	path := filepath.Join(filepath.Dir(exe), "warden-menu")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

// servicePath is a PATH the unit runs with: the launcher finds sbx through
// its pinned wrapper, but sbx and the browser opener want the usual
// places, which a service manager's environment lacks.
const servicePath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// --- launchd ---------------------------------------------------------

type launchdAgent struct {
	name   string
	path   string
	domain string // gui/<uid>
	run    commandRunner
	// menu: the agent runs the menu bar item beside the launcher instead
	// of the launcher (platformMenu).
	menu bool
}

func (a *launchdAgent) kind() string {
	if a.menu {
		return "menu bar item"
	}
	return "launchd agent"
}
func (a *launchdAgent) label() string    { return a.name }
func (a *launchdAgent) unitPath() string { return a.path }
func (a *launchdAgent) target() string   { return a.domain + "/" + a.name }
func (a *launchdAgent) registered() bool { _, err := os.Stat(a.path); return err == nil }
func (a *launchdAgent) hints() []string  { return nil }

// unit is the agent's plist: run at login, restarted after an unsuccessful
// exit (a crash; `warden stop` exits 0 and stays stopped until the next
// login or `warden start`), ten seconds between restarts. Output is not
// captured by launchd: the launcher writes and rotates <state>/warden.log
// itself under --service.
func (a *launchdAgent) unit(exe, configPath string) string {
	var b strings.Builder
	esc := func(s string) string {
		var buf bytes.Buffer
		xml.EscapeText(&buf, []byte(s))
		return buf.String()
	}
	args := []string{exe, "start", "--service", "--config", configPath}
	if a.menu {
		// The item runs `warden menu feed` and the actions through the
		// launcher it is told; the GUI session only (Aqua).
		args = []string{filepath.Join(filepath.Dir(exe), "warden-menu"), "--warden", exe, "--config", configPath}
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(a.name) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, arg := range args {
		b.WriteString("\t\t<string>" + esc(arg) + "</string>\n")
	}
	b.WriteString(`	</array>
`)
	if a.menu {
		b.WriteString(`	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
`)
	}
	b.WriteString(`	<key>WorkingDirectory</key>
	<string>` + esc(filepath.Dir(configPath)) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + servicePath + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>90</integer>
</dict>
</plist>
`)
	return b.String()
}

func (a *launchdAgent) status() serviceStatus {
	out, err := a.run("launchctl", "print", a.target())
	if err != nil {
		return serviceStatus{}
	}
	st := serviceStatus{Loaded: true}
	// Only the service's own "state = ..." line counts: launchctl nests
	// further "state = active" lines under the endpoints, and the last
	// one of those used to mask a running service as stopped.
	sawState := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") && !sawState {
			sawState = true
			st.Running = strings.TrimPrefix(line, "state = ") == "running"
		}
		if strings.HasPrefix(line, "pid = ") && st.PID == 0 {
			st.PID, _ = strconv.Atoi(strings.TrimPrefix(line, "pid = "))
		}
	}
	return st
}

func (a *launchdAgent) install(unit string) (string, error) {
	previous, _ := os.ReadFile(a.path)
	changed := string(previous) != unit
	before := a.status()
	if changed {
		if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(a.path, []byte(unit), 0o644); err != nil {
			return "", err
		}
	}
	switch {
	case before.Loaded && !changed && before.Running:
		return "already registered and " + before.String(), nil
	case before.Loaded && !changed:
		if _, err := a.run("launchctl", "kickstart", a.target()); err != nil {
			return "", err
		}
		return "already registered; started", nil
	case before.Loaded:
		// A changed plist has to be unloaded and loaded again for launchd
		// to read it; bootout stops the running instance.
		if _, err := a.run("launchctl", "bootout", a.target()); err != nil {
			return "", err
		}
		if err := a.awaitStopped(); err != nil {
			return "", err
		}
		if _, err := a.run("launchctl", "bootstrap", a.domain, a.path); err != nil {
			return "", err
		}
		return "re-registered (the plist changed) and started", nil
	default:
		if _, err := a.run("launchctl", "bootstrap", a.domain, a.path); err != nil {
			return "", err
		}
		return "registered " + a.path + " and started", nil
	}
}

func (a *launchdAgent) start() error {
	st := a.status()
	if !st.Loaded {
		if !a.registered() {
			return errors.New("the service is not registered; run `warden service install`")
		}
		_, err := a.run("launchctl", "bootstrap", a.domain, a.path)
		return err
	}
	if st.Running {
		return nil
	}
	_, err := a.run("launchctl", "kickstart", a.target())
	return err
}

func (a *launchdAgent) stop() error {
	st := a.status()
	if !st.Running {
		return nil
	}
	if _, err := a.run("launchctl", "kill", "TERM", a.target()); err != nil {
		return err
	}
	return a.awaitStopped()
}

func (a *launchdAgent) restart() error {
	st := a.status()
	if !st.Loaded {
		return a.start()
	}
	_, err := a.run("launchctl", "kickstart", "-k", a.target())
	return err
}

func (a *launchdAgent) uninstall() error {
	if a.status().Loaded {
		if err := a.stop(); err != nil {
			return err
		}
		if _, err := a.run("launchctl", "bootout", a.target()); err != nil {
			return err
		}
	}
	if err := os.Remove(a.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (a *launchdAgent) awaitStopped() error {
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		if !a.status().Running {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("the service did not stop within 100 s")
}

// --- systemd ---------------------------------------------------------

type systemdUnit struct {
	name string
	path string
	run  commandRunner
}

func (u *systemdUnit) kind() string     { return "systemd user unit" }
func (u *systemdUnit) label() string    { return u.name + ".service" }
func (u *systemdUnit) unitPath() string { return u.path }
func (u *systemdUnit) registered() bool { _, err := os.Stat(u.path); return err == nil }

// unit is the user unit: restarted after a failure, stopped by SIGTERM
// with time for the launcher's own shutdown, wanted by the user session.
func (u *systemdUnit) unit(exe, configPath string) string {
	return `[Unit]
Description=Warden (sandboxed AI coding agents)
Documentation=https://github.com/monaddle-too/warden
After=network-online.target

[Service]
ExecStart=` + systemdQuote(exe) + ` start --service --config ` + systemdQuote(configPath) + `
WorkingDirectory=` + systemdQuote(filepath.Dir(configPath)) + `
Environment=PATH=` + servicePath + `
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=90

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes a path for a unit file's command line.
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`).Replace(s) + `"`
}

func (u *systemdUnit) status() serviceStatus {
	out, err := u.run("systemctl", "--user", "show", u.name+".service", "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID")
	if err != nil {
		return serviceStatus{}
	}
	st := serviceStatus{}
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "LoadState":
			st.Loaded = value == "loaded"
		case "ActiveState":
			st.Running = value == "active" || value == "activating" || value == "reloading"
		case "MainPID":
			st.PID, _ = strconv.Atoi(value)
		}
	}
	return st
}

func (u *systemdUnit) install(unit string) (string, error) {
	previous, _ := os.ReadFile(u.path)
	changed := string(previous) != unit
	before := u.status()
	if changed {
		if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(u.path, []byte(unit), 0o644); err != nil {
			return "", err
		}
		if _, err := u.run("systemctl", "--user", "daemon-reload"); err != nil {
			return "", err
		}
	}
	if _, err := u.run("systemctl", "--user", "enable", "--now", u.name+".service"); err != nil {
		return "", err
	}
	switch {
	case before.Running && changed:
		if _, err := u.run("systemctl", "--user", "restart", u.name+".service"); err != nil {
			return "", err
		}
		return "re-registered (the unit changed) and restarted", nil
	case before.Running:
		return "already registered and " + before.String(), nil
	case before.Loaded && !changed:
		return "already registered; started", nil
	default:
		return "registered " + u.path + " and started", nil
	}
}

func (u *systemdUnit) start() error {
	if !u.registered() {
		return errors.New("the service is not registered; run `warden service install`")
	}
	_, err := u.run("systemctl", "--user", "start", u.name+".service")
	return err
}

func (u *systemdUnit) stop() error {
	_, err := u.run("systemctl", "--user", "stop", u.name+".service")
	return err
}

func (u *systemdUnit) restart() error {
	if !u.registered() {
		return errors.New("the service is not registered; run `warden service install`")
	}
	_, err := u.run("systemctl", "--user", "restart", u.name+".service")
	return err
}

func (u *systemdUnit) uninstall() error {
	if u.registered() {
		if _, err := u.run("systemctl", "--user", "disable", "--now", u.name+".service"); err != nil {
			return err
		}
	}
	if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := u.run("systemctl", "--user", "daemon-reload")
	return err
}

// hints: a user unit runs while the user has a session; lingering keeps
// it running from boot and across logouts, which is what a service is.
func (u *systemdUnit) hints() []string {
	name := ""
	if me, err := user.Current(); err == nil {
		name = me.Username
	}
	out, err := u.run("loginctl", "show-user", name, "-p", "Linger")
	if err != nil || strings.Contains(out, "Linger=yes") {
		return nil
	}
	return []string{"the service runs while you are logged in; `loginctl enable-linger " + name + "` starts it at boot and keeps it running after you log out"}
}
