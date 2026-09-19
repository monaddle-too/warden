package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/sandbox"
)

// uninstall reverses `warden install` on this machine: a background Warden
// is stopped, every sandbox in Warden's private sbx namespace is deleted,
// the namespace daemon is stopped and, unless --keep-state, the state
// directory (config, chats, provider sign-ins, the namespace with its
// template and VM images, runtimes, logs) is removed. Nothing outside the
// state directory was created by install, so nothing else is touched: the
// operator's own sbx, its Docker sign-in and the unpacked release stay.
// Sign-ins at the providers outlive the local files; the command prints
// where to revoke them.
func (c *cli) uninstall(args []string) error {
	fs := flag.NewFlagSet("warden uninstall", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := addStateFlags(fs)
	keepState := fs.Bool("keep-state", false, "delete the sandboxes and stop the private sbx daemon, but keep the state directory (config, chats, sign-ins)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfigFlags(*configPath, state)
	if err != nil {
		return err
	}
	root := cfg.Paths.State
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("%s: %w; nothing to uninstall", root, err)
	}
	if err := c.refuseForeground(cfg); err != nil {
		return err
	}
	if !*yes {
		if err := c.confirmRemoval(cfg, *keepState); err != nil {
			return err
		}
	}
	return c.removeInstall(cfg, *keepState)
}

// refuseForeground refuses to remove an install while a foreground `warden
// start` holds the launcher lock: it must be stopped by its own terminal,
// a detached one or the service is stopped by removeInstall.
func (c *cli) refuseForeground(cfg config.Config) error {
	lock, err := os.OpenFile(filepath.Join(cfg.Paths.State, "launcher.lock"), os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	held := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
	lock.Close()
	_, detached := runningPID(cfg)
	service := c.registeredService(cfg) != nil && c.registeredService(cfg).status().Running
	if held && !detached && !service {
		return errors.New("Warden is running in the foreground (warden start); stop it with Ctrl+C first")
	}
	return nil
}

// confirmRemoval says what removeInstall is about to do and asks for a
// typed yes on the terminal.
func (c *cli) confirmRemoval(cfg config.Config, keepState bool) error {
	root := cfg.Paths.State
	if sharedNamespace(cfg) {
		fmt.Fprintf(c.stdout, "This deletes the sandboxes of the %s instance in the shared sbx namespace %s (the daemon keeps running)", instanceName(root), cfg.SBX.PrivateHome)
	} else {
		fmt.Fprintf(c.stdout, "This deletes every sandbox in Warden's private sbx namespace and stops its daemon")
	}
	if keepState {
		fmt.Fprintf(c.stdout, "; %s is kept.\n", root)
	} else {
		fmt.Fprintf(c.stdout, ", then removes %s (config, chats, sign-ins, runtimes, logs).\n", root)
	}
	if !isTerminal(c.stdin) {
		return errors.New("not a terminal; pass --yes to confirm")
	}
	fmt.Fprint(c.stdout, "Type yes to continue: ")
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("cancelled")
	}
	return nil
}

// sharedNamespace reports whether the install's SBX namespace is another
// instance's (docs/host-dogfood-plan.md, decision 3): then only its own
// sandboxes (sandbox.Owned) are its to remove and the daemon is not.
func sharedNamespace(cfg config.Config) bool {
	return cfg.SBX.PrivateHome != "" && !within(cfg.SBX.PrivateHome, cfg.Paths.State)
}

// removeInstall is uninstall after the confirmation: the menu bar item
// and the service unregistered, a detached Warden stopped, the sandboxes
// removed (every one in an own namespace, only the instance's own in a
// shared one), the daemon stopped (own namespace only) and, unless
// keepState, the state directory deleted.
func (c *cli) removeInstall(cfg config.Config, keepState bool) error {
	root := cfg.Paths.State
	if m := c.registeredMenu(cfg); m != nil {
		if err := m.uninstall(); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "menu bar:    stopped and unregistered the menu bar item (%s removed)\n", m.unitPath())
	}
	if svc := c.registeredService(cfg); svc != nil {
		if err := svc.uninstall(); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "service:     stopped and unregistered the %s (%s removed)\n", svc.kind(), svc.unitPath())
	}
	if pid, alive := runningPID(cfg); alive {
		if err := stopDetached(cfg, pid); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "warden:      stopped the background Warden (pid %d)\n", pid)
	}
	os.Remove(pidFile(cfg))
	wrapper := wrapperPath(root)
	if err := executableFile(wrapper); err == nil {
		sbx := &sbxCLI{wrapper: wrapper, stdin: c.stdin, stdout: c.stdout, stderr: c.stderr}
		out, err := sbx.command(time.Minute, "ls", "--quiet")
		if err != nil && !daemonNeedsRestart(err) {
			// A stopped daemon has no sandboxes to list; sbx says so in
			// several ways, all of which mean "nothing to remove".
			fmt.Fprintf(c.stdout, "sandboxes:   not listed (%v)\n", err)
		}
		if err != nil && daemonNeedsRestart(err) {
			// The daemon predates the CLI: restart it so the listing works,
			// exactly as install and start do.
			if _, err := sbx.command(3*time.Minute, "daemon", "restart"); err == nil {
				out, _ = sbx.command(time.Minute, "ls", "--quiet")
			}
		}
		names := strings.Fields(out)
		shared := sharedNamespace(cfg)
		if shared {
			names = sandbox.Owned(cfg.Sandboxes.NamePrefix, names)
		}
		removed := 0
		for _, name := range names {
			if _, err := sbx.command(3*time.Minute, "rm", "--force", name); err != nil {
				return fmt.Errorf("removing sandbox %s: %w", name, err)
			}
			removed++
		}
		fmt.Fprintf(c.stdout, "sandboxes:   %d removed\n", removed)
		if shared {
			fmt.Fprintf(c.stdout, "sbx daemon:  left running (the namespace %s is shared)\n", cfg.SBX.PrivateHome)
		} else {
			if out, err := sbx.command(time.Minute, "daemon", "stop"); err != nil && !strings.Contains(strings.ToLower(out), "not running") {
				return fmt.Errorf("stopping Warden's sbx daemon: %w", err)
			}
			fmt.Fprintln(c.stdout, "sbx daemon:  stopped")
		}
	} else {
		fmt.Fprintf(c.stdout, "sbx:         no namespace wrapper at %s; no sandboxes or daemon to remove\n", wrapper)
	}
	if keepState {
		fmt.Fprintf(c.stdout, "state:       kept %s (delete it yourself to finish)\n", root)
	} else {
		if err := os.RemoveAll(root); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "state:       removed %s\n", root)
	}
	fmt.Fprint(c.stdout, `
Still yours to remove: the unpacked release directory and any PATH entry for
it. Sign-ins at the providers outlive the local files: revoke Warden's GitHub
token under https://github.com/settings/applications (Authorized OAuth Apps)
and its Google access under https://myaccount.google.com/permissions. Codex
and Claude sign-ins are your subscriptions' own and need nothing.
`)
	return nil
}

// stopDetached sends SIGTERM to a background Warden and waits for it.
func stopDetached(cfg config.Config, pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := runningPID(cfg); !alive {
			os.Remove(pidFile(cfg))
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Warden (pid %d) did not stop within 30 s", pid)
}
