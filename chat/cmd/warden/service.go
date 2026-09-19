package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"warden/chat/internal/config"
)

// Warden in the background has two shapes. The service (svc.go) is the
// normal one: `warden install` registers a launchd agent or a systemd user
// unit running `warden start --service`, and start/stop/restart/status
// drive the manager. `warden start --detach` is the fallback for a host
// without a user service manager: `warden start` re-run in its own
// session, recorded in <state>/warden.pid.

// pidFile and logFile are where a detached `warden start` records itself
// and where both shapes write the launcher's output.
func pidFile(cfg config.Config) string { return filepath.Join(cfg.Paths.State, "warden.pid") }
func logFile(cfg config.Config) string { return filepath.Join(cfg.Paths.State, "warden.log") }

// logRotateSize is the size at which a log under <state> is rotated when
// the launcher starts: the file becomes .1 (then .2, .3; older ones go).
// Neither launchd nor the launcher's O_APPEND children rotate by
// themselves, so each start bounds what the previous run left.
const logRotateSize = 10 << 20

func rotateLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < logRotateSize {
		return
	}
	os.Remove(path + ".3")
	os.Rename(path+".2", path+".3")
	os.Rename(path+".1", path+".2")
	os.Rename(path, path+".1")
}

// openLog appends to <state>/warden.log after rotating it.
func openLog(cfg config.Config) (*os.File, error) {
	rotateLog(logFile(cfg))
	return os.OpenFile(logFile(cfg), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// awaitReady waits for the chat endpoint file to change from previous
// (the chat service rewrites it at every start) while alive still reports
// the launcher up, for at most 90 s. The first seconds do not trust a
// "not running": a manager reports the unit before it has spawned it.
func awaitReady(cfg config.Config, previous string, alive func() bool) error {
	start := time.Now()
	deadline := start.Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if stamp := fileStamp(cfg.OwnerTokenFile()); stamp != "" && stamp != previous {
			return nil
		}
		if time.Since(start) > 3*time.Second && !alive() {
			return fmt.Errorf("Warden exited during startup; see %s", logFile(cfg))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Warden did not become ready within 90 s; see %s", logFile(cfg))
}

// detach re-runs `warden start` without --detach as its own session, with
// output appended to the log, and waits for the chat endpoint to appear.
func (c *cli) detach(cfg config.Config, args []string) error {
	if pid, alive := runningPID(cfg); alive {
		return fmt.Errorf("Warden is already running (pid %d); `warden stop` first", pid)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var child []string
	for _, a := range args {
		if a != "--detach" && a != "-detach" && a != "--detach=true" {
			child = append(child, a)
		}
	}
	log, err := openLog(cfg)
	if err != nil {
		return err
	}
	defer log.Close()
	previous := fileStamp(cfg.OwnerTokenFile())
	cmd := exec.Command(exe, append([]string{"start", "--detached-child"}, child...)...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	if err = os.WriteFile(pidFile(cfg), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		return err
	}
	// Release the child; it lives on after this process exits.
	go cmd.Wait()
	if err = awaitReady(cfg, previous, func() bool { _, alive := runningPID(cfg); return alive }); err != nil {
		if _, alive := runningPID(cfg); !alive {
			os.Remove(pidFile(cfg))
			return fmt.Errorf("%w (is another Warden still running in this state directory?)", err)
		}
		return err
	}
	fmt.Fprintf(c.stdout, "Warden started in the background (pid %d, log %s). `warden chat` or `warden open` to use it, `warden stop` to stop it.\n", cmd.Process.Pid, logFile(cfg))
	return nil
}

// runningPID reads the pid file and reports whether that process is alive.
func runningPID(cfg config.Config) (int, bool) {
	raw, err := os.ReadFile(pidFile(cfg))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

// registeredService is the manager with Warden's unit registered, or nil.
func (c *cli) registeredService(cfg config.Config) serviceManager {
	svc, _ := c.service(cfg.Paths.State)
	if svc == nil || !svc.registered() {
		return nil
	}
	return svc
}

// registeredMenu is the menu bar item's manager with its unit registered,
// or nil.
func (c *cli) registeredMenu(cfg config.Config) serviceManager {
	m, _ := c.menu(cfg.Paths.State)
	if m == nil || !m.registered() {
		return nil
	}
	return m
}

// registerMenu installs the menu bar item's unit for this launcher and
// waits a moment for it to run. It reports what it did, or why the item
// is not registered (no build of it beside the launcher), as one phrase.
func (c *cli) registerMenu(m serviceManager, configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if menuExecutable(exe) == "" {
		return "not registered: no warden-menu beside " + exe + " (a release built without swiftc)", nil
	}
	result, err := m.install(m.unit(exe, configPath))
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !m.status().Running && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	return m.label() + ": " + result + " (" + m.status().String() + ")", nil
}

// startService starts the registered service and waits for the chat
// endpoint.
func (c *cli) startService(cfg config.Config, svc serviceManager) error {
	if st := svc.status(); st.Running {
		fmt.Fprintf(c.stdout, "Warden is already running as a %s (%s). `warden restart` restarts it.\n", svc.kind(), st)
		return nil
	}
	previous := fileStamp(cfg.OwnerTokenFile())
	if err := svc.start(); err != nil {
		return err
	}
	if err := awaitReady(cfg, previous, func() bool { return svc.status().Running }); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Warden started as a %s (%s, log %s). `warden open` to open it, `warden stop` to stop it.\n", svc.kind(), svc.status(), logFile(cfg))
	return nil
}

// serviceFlags is the flag set start, stop, restart and status share.
func serviceFlags(name string, c *cli) (*flag.FlagSet, *string, *stateFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := addStateFlags(fs)
	return fs, configPath, state
}

// stopService stops the running Warden: the service when one is
// registered, else a detached one.
func (c *cli) stopService(args []string) error {
	fs, configPath, state := serviceFlags("warden stop", c)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	if svc := c.registeredService(cfg); svc != nil {
		st := svc.status()
		if !st.Running {
			if pid, alive := runningPID(cfg); alive {
				return c.stopDetachedAndSay(cfg, pid)
			}
			return fmt.Errorf("Warden is not running (%s %s: %s)", svc.kind(), svc.label(), st)
		}
		if err := svc.stop(); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "Warden stopped (%s %s; it starts again at the next login, or with `warden start`)\n", svc.kind(), svc.label())
		return nil
	}
	pid, alive := runningPID(cfg)
	if !alive {
		os.Remove(pidFile(cfg))
		return errors.New("Warden is not running in the background (no live pid file); a foreground `warden start` stops with Ctrl+C")
	}
	return c.stopDetachedAndSay(cfg, pid)
}

func (c *cli) stopDetachedAndSay(cfg config.Config, pid int) error {
	if err := stopDetached(cfg, pid); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Warden stopped (pid %d)\n", pid)
	return nil
}

// restartService restarts the registered service (a detached Warden is
// stopped and started again), and waits for the chat endpoint.
func (c *cli) restartService(args []string) error {
	fs, configPath, state := serviceFlags("warden restart", c)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	previous := fileStamp(cfg.OwnerTokenFile())
	if svc := c.registeredService(cfg); svc != nil {
		if err := svc.restart(); err != nil {
			return err
		}
		if err := awaitReady(cfg, previous, func() bool { return svc.status().Running }); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "Warden restarted as a %s (%s). `warden open` to open it again: the owner session rotates with the chat service.\n", svc.kind(), svc.status())
		// The menu bar item too, so a new release's runs.
		if m := c.registeredMenu(cfg); m != nil {
			if err := m.restart(); err != nil {
				fmt.Fprintf(c.stdout, "menu bar item: not restarted: %v\n", err)
			}
		}
		return nil
	}
	if pid, alive := runningPID(cfg); alive {
		if err := stopDetached(cfg, pid); err != nil {
			return err
		}
	} else {
		return errors.New("Warden is not running in the background; `warden start --detach` starts it")
	}
	return c.detach(cfg, []string{"--config", cfgPath(cfg, *configPath)})
}

// cfgPath is the --config a re-spawned launcher gets: the one given, else
// the state's default.
func cfgPath(cfg config.Config, configPath string) string {
	if configPath != "" {
		return configPath
	}
	return defaultConfigPath(cfg.Paths.State)
}

// status reports whether Warden is running and how. Without --instance or
// --state it first prints a table of every instance on the machine (what
// each runs, from running.json, against what it is pinned to), then the
// default instance's detail lines as before, so what reads `warden
// status` keeps working; --json prints the table alone.
func (c *cli) status(args []string) error {
	fs, configPath, state := serviceFlags("warden status", c)
	asJSON := fs.Bool("json", false, "print the instance table as JSON")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dir, err := state.dir()
	if err != nil {
		return err
	}
	if dir == "" && *configPath == "" {
		infos, err := c.instances()
		if err != nil {
			return err
		}
		if *asJSON {
			if infos == nil {
				infos = []instanceInfo{}
			}
			b, err := json.MarshalIndent(infos, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(c.stdout, string(b))
			return nil
		}
		if len(infos) > 0 {
			printStatusTable(c.stdout, infos, time.Now())
			fmt.Fprintln(c.stdout)
		}
	} else if *asJSON {
		cfg, _, err := loadConfigFlags(*configPath, state)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent([]instanceInfo{c.describeInstance(cfg.Paths.State)}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, string(b))
		return nil
	}
	cfg, path, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "state:   %s\nconfig:  %s\n", cfg.Paths.State, path)
	svc, reason := c.service(cfg.Paths.State)
	switch {
	case svc == nil:
		fmt.Fprintf(c.stdout, "service: none (%s); `warden start --detach` runs Warden in the background\n", reason)
	case !svc.registered():
		fmt.Fprintf(c.stdout, "service: not registered; `warden service install` registers a %s\n", svc.kind())
	default:
		fmt.Fprintf(c.stdout, "service: %s %s: %s (%s)\n", svc.kind(), svc.label(), svc.status(), svc.unitPath())
	}
	if m, reason := c.menu(cfg.Paths.State); m != nil || reason != "" {
		switch {
		case m == nil:
			fmt.Fprintf(c.stdout, "menu:    none (%s)\n", reason)
		case !m.registered():
			fmt.Fprintln(c.stdout, "menu:    not registered; `warden menu install` registers the menu bar item")
		default:
			fmt.Fprintf(c.stdout, "menu:    %s: %s (%s)\n", m.label(), m.status(), m.unitPath())
		}
	}
	pid, alive := runningPID(cfg)
	switch {
	case alive:
		fmt.Fprintf(c.stdout, "warden:  running detached (pid %d, log %s)\n", pid, logFile(cfg))
	case pid != 0:
		fmt.Fprintf(c.stdout, "warden:  no detached instance (stale pid file for %d)\n", pid)
	case svc != nil && svc.registered():
		fmt.Fprintf(c.stdout, "log:     %s\n", logFile(cfg))
	default:
		fmt.Fprintln(c.stdout, "warden:  no background instance (a foreground `warden start` does not record a pid)")
	}
	livePID := 0
	if alive {
		livePID = pid
	} else if svc != nil && svc.registered() && svc.status().Running {
		livePID = svc.status().PID
	}
	fmt.Fprintf(c.stdout, "running: %s\n", runningLine(cfg.Paths.State, time.Now(), livePID))
	if base, _, err := endpoint(cfg.OwnerTokenFile()); err == nil {
		fmt.Fprintf(c.stdout, "chat:    %s (capability in %s)\n", base, cfg.OwnerTokenFile())
	} else {
		fmt.Fprintln(c.stdout, "chat:    no endpoint file; Warden has not started")
	}
	fmt.Fprintf(c.stdout, "app:     %s\n", cfg.Auth.PublicURL)
	return nil
}

// runningLine describes running.json for the detail view: the version,
// pid, uptime and binary, whether it differs from the pinned release, or
// that nothing runs.
func runningLine(state string, now time.Time, pid int) string {
	r, alive, err := readRunning(state)
	switch {
	case err != nil:
		return "unreadable " + runningPath(state) + ": " + err.Error()
	case !alive && r.PID != 0:
		return fmt.Sprintf("nothing (stale %s for pid %d)", runningFile, r.PID)
	case !alive && pid != 0:
		// A launcher from before running.json: the process table's word.
		if bin := processBinary(pid); bin != "" {
			if v := releaseVersionOf(bin); v != "" {
				return fmt.Sprintf("%s (pid %d, %s; from the process table, a launcher from before %s)", v, pid, bin, runningFile)
			}
			return fmt.Sprintf("a version this launcher cannot tell (pid %d, %s; no %s)", pid, bin, runningFile)
		}
		return fmt.Sprintf("a version this launcher cannot tell (pid %d, no %s)", pid, runningFile)
	case !alive:
		return "nothing (no " + runningFile + ")"
	}
	line := fmt.Sprintf("%s (pid %d, up %s, %s)", r.Version, r.PID, uptime(now.Sub(r.StartedAt)), r.Binary)
	if pinned := releaseVersionOf(currentRelease(state)); pinned != "" && pinned != r.Version {
		line += fmt.Sprintf("; the pinned release is %s (a trial run: `warden start --version %s --use` makes it stick)", pinned, r.Version)
	}
	return line
}

// serviceCommand: `warden service install` registers the service for
// this launcher and starts it (what `warden install` does last);
// `warden service uninstall` stops and unregisters it, leaving the state.
func (c *cli) serviceCommand(args []string) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		fmt.Fprintln(c.stderr, "usage: warden service install|uninstall [--config PATH] [--state DIR]")
		return errUsage
	}
	fs, configPath, state := serviceFlags("warden service "+args[0], c)
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	cfg, path, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	svc, reason := c.service(cfg.Paths.State)
	if svc == nil {
		return fmt.Errorf("no user service manager here (%s); `warden start --detach` runs Warden in the background", reason)
	}
	if args[0] == "uninstall" {
		if err := c.unregisterMenu(cfg); err != nil {
			return err
		}
		if !svc.registered() {
			fmt.Fprintf(c.stdout, "no %s registered for %s\n", svc.kind(), cfg.Paths.State)
			return nil
		}
		if err := svc.uninstall(); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "Warden stopped and unregistered (%s %s removed). `warden start --detach` runs it without a service; `warden service install` registers it again.\n", svc.kind(), svc.unitPath())
		return nil
	}
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w; run `warden install` first", path, err)
	}
	if pid, alive := runningPID(cfg); alive {
		return fmt.Errorf("a detached Warden is running (pid %d); `warden stop` it first, the service takes its place", pid)
	}
	result, err := c.registerService(cfg, svc, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "service: %s\n", result)
	for _, h := range svc.hints() {
		fmt.Fprintf(c.stdout, "note:    %s\n", h)
	}
	if m, _ := c.menu(cfg.Paths.State); m != nil {
		result, err := c.registerMenu(m, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "menu:    %s\n", result)
	}
	return nil
}

// unregisterMenu stops and unregisters the menu bar item when it is
// registered, saying so.
func (c *cli) unregisterMenu(cfg config.Config) error {
	m := c.registeredMenu(cfg)
	if m == nil {
		return nil
	}
	if err := m.uninstall(); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "menu:    stopped and unregistered the menu bar item (%s removed)\n", m.unitPath())
	return nil
}

// menuSubcommand: `warden menu install` registers the menu bar item for
// this launcher and starts it (what `warden install` and `warden service
// install` do too; the way back after "Quit Menu Bar Item"); `warden
// menu uninstall` stops and unregisters it.
func (c *cli) menuSubcommand(args []string) error {
	fs, configPath, state := serviceFlags("warden menu "+args[0], c)
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	cfg, path, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	m, reason := c.menu(cfg.Paths.State)
	if m == nil {
		if reason == "" {
			reason = "the menu bar item is macOS only"
		}
		return fmt.Errorf("no menu bar item here (%s)", reason)
	}
	if args[0] == "uninstall" {
		if !m.registered() {
			fmt.Fprintf(c.stdout, "no menu bar item registered for %s\n", cfg.Paths.State)
			return nil
		}
		return c.unregisterMenu(cfg)
	}
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w; run `warden install` first", path, err)
	}
	result, err := c.registerMenu(m, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "menu:    %s\n", result)
	return nil
}

// registerService installs the unit for this launcher and waits until the
// chat endpoint answers.
func (c *cli) registerService(cfg config.Config, svc serviceManager, configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	previous := fileStamp(cfg.OwnerTokenFile())
	before := svc.status()
	result, err := svc.install(svc.unit(exe, configPath))
	if err != nil {
		return "", err
	}
	if before.Running && svc.status().PID == before.PID {
		return svc.kind() + " " + svc.label() + ": " + result, nil
	}
	if err := awaitReady(cfg, previous, func() bool { return svc.status().Running }); err != nil {
		return "", err
	}
	return svc.kind() + " " + svc.label() + ": " + result + " (" + svc.status().String() + ")", nil
}
