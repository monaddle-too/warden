// Command warden installs, checks, signs in and launches a single-owner
// Warden on the operator's own machine (docs/warden-local-install.md). It
// depends only on the standard library and the config and release packages,
// so it builds and runs before the four services do.
//
//	warden install [--state DIR] [--config PATH]
//	warden doctor  [--config PATH]
//	warden login   codex|claude|github
//	warden start   [--config PATH]
//	warden open    [--config PATH]
//	warden bugs    status | on | off | send "text" | test | pending
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
	"warden/chat/internal/services/chatsvc"
	"warden/chat/internal/services/edgesvc"
	"warden/chat/internal/services/policysvc"
	"warden/chat/internal/services/runnersvc"
)

// revision is the build revision shared by every Warden binary
// (release.Revision, set at link time by the release workflow).
var revision = release.Revision

// cli carries the standard streams so subcommands are testable, and the
// two ways of reaching the person besides them (nil is the host's own).
type cli struct {
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	terminal bool // stdin is an interactive terminal
	openFn   func(url string) error
	notifyFn func(title, body string) error
	// serviceFn supplies the service manager for a state directory (nil:
	// the platform's); tests substitute a recording fake.
	serviceFn func(state string) (serviceManager, string)
}

// openURL opens the browser on url.
func (c *cli) openURL(url string) error {
	if c.openFn != nil {
		return c.openFn(url)
	}
	return openBrowser(url)
}

// notifyDesktop shows a desktop notification.
func (c *cli) notifyDesktop(title, body string) error {
	if c.notifyFn != nil {
		return c.notifyFn(title, body)
	}
	return desktopNotify(title, body)
}

func main() {
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, terminal: isTerminal(os.Stdin)}
	os.Exit(c.run(os.Args[1:]))
}

const usageText = `usage: warden COMMAND [flags]

  install   create the private state, SBX namespace and runtimes; write warden.json
  doctor    check every host and runtime invariant and print the remediation
  login     codex | claude | github: store one provider sign-in, owner-only
  start     start Warden: the registered service, or the four services here (--foreground) or detached (--detach)
  stop      stop the running Warden (the service, or a detached one)
  restart   restart the service (after a new release)
  status    show whether Warden is running and how
  service   install | uninstall: register Warden with launchd / systemd --user (install does this too)
  open      open the running Warden in the browser (--chat ID, --new)
  menu      feed: the menu bar item's model, one JSON line per change (macOS; warden-menu runs it)
  chat      terminal client: warden chat [CHAT] | list | new | send | approve
  uninstall stop Warden, unregister the service, delete its sandboxes, stop its private sbx daemon and remove the state
  bugs      bug reports: status | on | off | send "text" | test | pending (you review every report before it is sent)
  tls       bootstrap: write a deployment CA and the four service certificates for tls:// transport
  version   print the build revision and protocol number

The services themselves (started by warden start; also usable directly):
  policy | runner | serve | edge   [--config warden.json] [flags]

Run "warden COMMAND -h" for the flags of one command.
`

func (c *cli) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, usageText)
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = c.install(args[1:])
	case "doctor":
		err = c.doctor(args[1:])
	case "login":
		err = c.login(args[1:])
	case "start":
		err = c.start(args[1:])
	case "open":
		err = c.open(args[1:])
	case "chat":
		err = c.chat(args[1:])
	case "stop":
		err = c.stopService(args[1:])
	case "restart":
		err = c.restartService(args[1:])
	case "status":
		err = c.status(args[1:])
	case "service":
		err = c.serviceCommand(args[1:])
	case "menu":
		err = c.menuCommand(args[1:])
	case "uninstall":
		err = c.uninstall(args[1:])
	case "bugs":
		err = c.bugs(args[1:])
	case "tls":
		err = c.tls(args[1:])
	case "policy":
		return policysvc.Main(args[1:])
	case "runner":
		return runnersvc.Main(args[1:])
	case "serve":
		return chatsvc.Main(args[1:])
	case "edge":
		return edgesvc.Main(args[1:])
	case "version", "--version":
		fmt.Fprintln(c.stdout, handshake.Self("warden"))
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, usageText)
		return 0
	default:
		fmt.Fprintf(c.stderr, "warden: unknown command %q\n%s", args[0], usageText)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errDoctor):
		// doctor already printed every line; the exit status is the summary.
		return 1
	default:
		fmt.Fprintf(c.stderr, "warden: %v\n", err)
		return 1
	}
}

var (
	errUsage  = errors.New("usage")
	errDoctor = errors.New("doctor found failures")
)

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
