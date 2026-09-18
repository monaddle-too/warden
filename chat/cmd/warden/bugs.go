package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
	"warden/chat/internal/tui"
)

// Bug reporting on the owner's machine (docs/bug-reporting-plan.md): the
// `warden bugs` subcommand, the presenter every process here uses, the
// launcher's watch over pending drafts and its service-exit trigger.

const bugsUsage = `usage: warden bugs COMMAND [--state DIR | --config PATH]

  status         whether bug reports are on, where they go, how many drafts wait
  on | off       turn bug reporting on or off (warden.json reporting.enabled)
  send "TEXT"    write a bug report yourself; it opens for review before it is sent
  test           raise a test exception in the running chat service (in this
                 process when Warden is not running); the report opens for review
  pending        list the drafts nobody decided on yet and offer each again
                 (--list only lists)

Every report is shown to you in full before it is sent, with "Send report" and
"Don't send". Nothing is sent otherwise.
`

func (c *cli) bugs(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("warden bugs", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	listOnly := fs.Bool("list", false, "pending: list the drafts without offering them again")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	switch sub {
	case "", "help", "-h", "--help":
		fmt.Fprint(c.stdout, bugsUsage)
		if sub == "" {
			return errUsage
		}
		return nil
	case "status", "on", "off", "send", "test", "pending":
	default:
		fmt.Fprintf(c.stderr, "warden bugs: unknown command %q\n%s", sub, bugsUsage)
		return errUsage
	}
	cfg, path, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		return c.bugsStatus(cfg, path)
	case "on", "off":
		return c.bugsSet(cfg, path, sub == "on")
	case "send":
		text := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if text == "" {
			fmt.Fprintln(c.stderr, "warden bugs send: the report's text is required: warden bugs send \"what went wrong\"")
			return errUsage
		}
		return c.bugsSend(cfg, path, text)
	case "test":
		return c.bugsTest(cfg, path)
	default:
		return c.bugsPending(cfg, path, *listOnly)
	}
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (c *cli) bugsStatus(cfg config.Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(c.stdout, "bug reports: off (no %s yet; `warden install` asks)\n", path)
		return nil
	}
	fmt.Fprintf(c.stdout, "bug reports: %s (%s)\n", onOff(cfg.Reporting.Enabled), cfg.Reporting.URL)
	drafts, err := bugreport.Pending(cfg.Paths.State)
	if err != nil {
		return err
	}
	switch len(drafts) {
	case 0:
		fmt.Fprintln(c.stdout, "pending:     none")
	case 1:
		fmt.Fprintln(c.stdout, "pending:     1 draft (`warden bugs pending` reviews it)")
	default:
		fmt.Fprintf(c.stdout, "pending:     %d drafts (`warden bugs pending` reviews them)\n", len(drafts))
	}
	return nil
}

// bugsSet writes reporting.enabled back the way install composes the
// file: the existing file re-read, one field changed, validated, written.
func (c *cli) bugsSet(cfg config.Config, path string, enabled bool) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w; run `warden install` first", path, err)
	}
	cfg.Reporting.Enabled = enabled
	if err := config.Write(path, cfg); err != nil {
		return err
	}
	if enabled {
		fmt.Fprintf(c.stdout, "bug reports: on — every report is shown to you before it is sent (%s)\n", cfg.Reporting.URL)
	} else {
		fmt.Fprintln(c.stdout, "bug reports: off — nothing is drafted or sent (`warden bugs on` to enable)")
	}
	return nil
}

// capturer is this command's capturer, reporting as the CLI.
func capturer(cfg config.Config, path string) *bugreport.Capturer {
	return bugreport.New(cfg, path, bugreport.ComponentCLI)
}

// presenter is how a process here shows a draft: the page URL on the
// terminal, the browser opened, the desktop told when nobody watches the
// terminal.
func (c *cli) presenter(cfg config.Config, notify bool) bugreport.Presenter {
	p := bugreport.Presenter{URL: cfg.Reporting.URL, Open: c.openURL, Out: c.stdout, Redactor: bugreport.NewRedactor(cfg), Timeout: presentTimeout}
	if notify {
		p.Notify = c.notifyDesktop
	}
	return p
}

// presentTimeout is how long a review page waits for a decision; a test
// shortens it.
var presentTimeout = bugreport.DefaultTimeout

// bugsSend drafts a user report from the terminal. A running launcher
// presents it (it watches the pending directory); otherwise this process
// does, before returning.
func (c *cli) bugsSend(cfg config.Config, path, text string) error {
	cap := capturer(cfg, path)
	if !cap.Enabled() {
		return errors.New(bugreport.OffNotice)
	}
	r := cap.Draft(bugreport.KindUser, bugreport.TriggerUser, text)
	r.Description = text
	cap.AddServiceLog(&r, "warden")
	cap.AddServiceLog(&r, "warden-chat")
	draft, written, err := cap.Capture(r)
	if err != nil {
		return err
	}
	if !written {
		return errors.New(bugreport.OffNotice)
	}
	if launcherRunning(cfg.Paths.State) {
		fmt.Fprintf(c.stdout, "Bug report %s drafted; the running Warden opens it for review (or `warden bugs pending`).\n", r.ID)
		return nil
	}
	return c.present(cfg, draft)
}

// bugsTest raises the test exception in the running chat service through
// its route, so the service's own recovery drafts the report and the
// launcher presents it; without a running chat the exception is raised
// and recovered here and the draft presented in-process.
func (c *cli) bugsTest(cfg config.Config, path string) error {
	cap := capturer(cfg, path)
	if !cap.Enabled() {
		return errors.New(bugreport.OffNotice)
	}
	if base, token, err := endpoint(cfg.OwnerTokenFile()); err == nil {
		client := &tui.Client{Base: base, Token: token}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, err := client.BugTest(ctx)
		if err == nil {
			fmt.Fprintln(c.stdout, result.Notice)
			if !result.Drafted {
				return errors.New("no draft was written")
			}
			return nil
		}
		if !connectionRefused(err) {
			return err
		}
	}
	// No chat service to ask: the same exception in this process.
	func() {
		defer cap.Trap(bugreport.TriggerTest, "warden bugs test")
		panic("test exception from /test bugreporting")
	}()
	drafts, err := bugreport.Pending(cfg.Paths.State)
	if err != nil {
		return err
	}
	var latest *bugreport.Draft
	for i := range drafts {
		if drafts[i].Report.Trigger == bugreport.TriggerTest && drafts[i].Report.Component == bugreport.ComponentCLI {
			latest = &drafts[i]
		}
	}
	if latest == nil {
		return errors.New("the test exception was not drafted")
	}
	fmt.Fprintln(c.stdout, "Warden is not running; the test exception was raised in this process instead.")
	return c.present(cfg, latest.Path)
}

// connectionRefused recognises a chat service that is not there (as
// opposed to one that answered with a refusal).
func connectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "connection refused") || strings.Contains(text, "no such file") || strings.Contains(text, "connect: ")
}

func (c *cli) bugsPending(cfg config.Config, path string, listOnly bool) error {
	drafts, err := bugreport.Pending(cfg.Paths.State)
	if err != nil {
		return err
	}
	if len(drafts) == 0 {
		fmt.Fprintln(c.stdout, "no pending bug reports")
		return nil
	}
	for i, d := range drafts {
		r := d.Report
		fmt.Fprintf(c.stdout, "%2d  %s  %-5s %-12s %-9s %s\n", i+1, r.CreatedAt, r.Kind, r.Trigger, r.Component, r.Summary)
	}
	if listOnly {
		return nil
	}
	if !cfg.Reporting.Enabled {
		fmt.Fprintln(c.stdout, bugreport.OffNotice+"; the drafts stay until then")
		return nil
	}
	for _, d := range drafts {
		fmt.Fprintln(c.stdout)
		if err := c.present(cfg, d.Path); err != nil {
			return err
		}
	}
	return nil
}

// present shows one draft in this process and prints the decision.
func (c *cli) present(cfg config.Config, draft string) error {
	ctx, cancel := signalContext()
	defer cancel()
	outcome, err := c.presenter(cfg, false).Present(ctx, draft)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, outcome.String())
	return nil
}

// signalContext ends on Ctrl+C or SIGTERM, so a waiting review page gives
// the terminal back.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// launcherRunning reports whether a `warden start` (foreground or
// detached) holds the state's launcher lock.
func launcherRunning(state string) bool {
	lock, err := os.OpenFile(filepath.Join(state, "launcher.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return false
}

// draftWatcher presents each draft that appears under the pending
// directory once, polling like the approval popups do. Drafts already
// there when the watch starts are only counted (they were offered before;
// `warden bugs pending` offers them again); every later one is presented,
// one at a time.
type draftWatcher struct {
	state    string
	interval time.Duration
	present  func(ctx context.Context, path string) (bugreport.Outcome, error)
	log      io.Writer

	mu    sync.Mutex
	known map[string]bool
}

// claim marks a draft as this watcher's to present; false when it already
// was.
func (w *draftWatcher) claim(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.known == nil {
		w.known = map[string]bool{}
	}
	if w.known[id] {
		return false
	}
	w.known[id] = true
	return true
}

// start counts the drafts already pending and claims them.
func (w *draftWatcher) start() int {
	drafts, _ := bugreport.Pending(w.state)
	for _, d := range drafts {
		w.claim(d.Report.ID)
	}
	return len(drafts)
}

// run polls until ctx ends.
func (w *draftWatcher) run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.presentNew(ctx)
		}
	}
}

// presentNew presents every pending draft not yet claimed, in order.
func (w *draftWatcher) presentNew(ctx context.Context) {
	drafts, err := bugreport.Pending(w.state)
	if err != nil {
		return
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			return
		}
		if !w.claim(d.Report.ID) {
			continue
		}
		outcome, err := w.present(ctx, d.Path)
		if w.log == nil {
			continue
		}
		if err != nil {
			fmt.Fprintf(w.log, "warden: bug report %s could not be shown: %v\n", d.Report.ID, err)
			continue
		}
		fmt.Fprintf(w.log, "warden: bug report %s: %s\n", d.Report.ID, outcome.String())
	}
}

// serviceComponent maps a launcher service name (warden-chat) to the
// report component (chat).
func serviceComponent(name string) string { return strings.TrimPrefix(name, "warden-") }

// exitDraft writes the service-exit draft for name: what the process ended
// with, the tail of its log and of the launcher's. Skipped when the service
// itself just drafted the panic that took it down (that draft has the
// stack; this one would only repeat it).
func exitDraft(cap *bugreport.Capturer, name, exit string, now time.Time) (string, bool, error) {
	if !cap.Enabled() {
		return "", false, nil
	}
	if bugreport.Recent(cap.State, serviceComponent(name), bugreport.TriggerPanic, now, 30*time.Second) {
		return "", false, nil
	}
	r := cap.Draft(bugreport.KindError, bugreport.TriggerServiceExit, name+" exited unexpectedly ("+exit+")")
	r.Error = &bugreport.Error{Message: name + " exited unexpectedly: " + exit, Operation: name}
	cap.AddServiceLog(&r, name)
	cap.AddServiceLog(&r, "warden")
	return cap.Capture(r)
}
