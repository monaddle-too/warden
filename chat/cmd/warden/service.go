package main

import (
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

// pidFile and logFile are where a detached `warden start` records itself.
func pidFile(cfg config.Config) string { return filepath.Join(cfg.Paths.State, "warden.pid") }
func logFile(cfg config.Config) string { return filepath.Join(cfg.Paths.State, "warden.log") }

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
	log, err := os.OpenFile(logFile(cfg), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
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
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := runningPID(cfg); !alive {
			os.Remove(pidFile(cfg))
			return fmt.Errorf("Warden exited during startup; see %s (is another Warden still running in this state directory?)", logFile(cfg))
		}
		if stamp := fileStamp(cfg.OwnerTokenFile()); stamp != "" && stamp != previous {
			fmt.Fprintf(c.stdout, "Warden started in the background (pid %d, log %s). `warden chat` or `warden open` to use it, `warden stop` to stop it.\n", cmd.Process.Pid, logFile(cfg))
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Warden did not become ready within 90 s; see %s", logFile(cfg))
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

// stopService stops a detached Warden.
func (c *cli) stopService(args []string) error {
	fs := flag.NewFlagSet("warden stop", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	pid, alive := runningPID(cfg)
	if !alive {
		os.Remove(pidFile(cfg))
		return errors.New("Warden is not running in the background (no live pid file); a foreground `warden start` stops with Ctrl+C")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := runningPID(cfg); !alive {
			os.Remove(pidFile(cfg))
			fmt.Fprintf(c.stdout, "Warden stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Warden (pid %d) did not stop within 30 s", pid)
}

// status reports whether Warden is running and where.
func (c *cli) status(args []string) error {
	fs := flag.NewFlagSet("warden status", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, path, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "state:   %s\nconfig:  %s\n", cfg.Paths.State, path)
	pid, alive := runningPID(cfg)
	switch {
	case alive:
		fmt.Fprintf(c.stdout, "warden:  running in the background (pid %d, log %s)\n", pid, logFile(cfg))
	case pid != 0:
		fmt.Fprintf(c.stdout, "warden:  not running (stale pid file for %d)\n", pid)
	default:
		fmt.Fprintln(c.stdout, "warden:  no background instance (a foreground `warden start` does not record a pid)")
	}
	if base, _, err := endpoint(cfg.OwnerTokenFile()); err == nil {
		fmt.Fprintf(c.stdout, "chat:    %s (capability in %s)\n", base, cfg.OwnerTokenFile())
	} else {
		fmt.Fprintln(c.stdout, "chat:    no endpoint file; Warden has not started")
	}
	fmt.Fprintf(c.stdout, "app:     %s\n", cfg.Auth.PublicURL)
	return nil
}
