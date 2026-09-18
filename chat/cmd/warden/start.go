package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
	"warden/chat/internal/handshake"
)

// start runs the policy, runner, chat and edge services as owner processes.
// They are subcommands of this same executable (warden policy, warden
// runner, warden serve, warden edge), so one build is always launched with
// itself; the web UI, GitHub catalog and policy template are found beside
// it in the release layout. Ctrl+C stops them all.
func (c *cli) start(args []string) error {
	fs := flag.NewFlagSet("warden start", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	webDir := fs.String("web-dir", "", "built chat UI when warden.json has no paths.webAssets (default: found beside the binaries)")
	vendorDir := fs.String("vendor-dir", "", "GitHub catalog directory when warden.json has no paths.githubCatalog")
	template := fs.String("policy-template", "", "sandbox policy template when warden.json has no paths.sandboxPolicyTemplate")
	withoutEdge := fs.Bool("without-edge", false, "do not start the edge (no previews; the app is reachable on the chat port only)")
	detach := fs.Bool("detach", false, "run in the background; logs to <state>/warden.log, stop with `warden stop`")
	popupsMode := fs.String("popups", popupsNone, "how pending approvals are surfaced: none (default: they wait in the app and the terminal client), notify (desktop notification), browser (notification and the chat opened in the browser), auto (browser when detached, notify otherwise), silent (nothing at all). A review only the app can do (a pull request proposal, document suggestions, a document choice) opens the app under every mode but silent")
	detachedChild := fs.Bool("detached-child", false, "internal: this process was started by --detach")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, path, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w; run `warden install` first", path, err)
	}
	if *detach {
		return c.detach(cfg, args)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if !slices.Contains(popupModes, *popupsMode) {
		return fmt.Errorf("--popups must be auto, browser, notify, none or silent, not %q", *popupsMode)
	}
	l := &launcher{c: c, cfg: cfg, configPath: path, exe: exe, withoutEdge: *withoutEdge, popups: *popupsMode, detached: *detachedChild}
	if l.assets, err = locateAssets(cfg, filepath.Dir(exe), *webDir, *vendorDir, *template); err != nil {
		return err
	}
	return l.run()
}

// services in start order (shutdown is the reverse): the log name each one
// writes under <state>/ and the warden subcommand that runs it.
var services = []struct{ name, subcommand string }{{"warden-policy", "policy"}, {"warden-runner", "runner"}, {"warden-chat", "serve"}, {"warden-edge", "edge"}}

type assets struct{ web, vendor, template string }

// locateAssets finds the built UI, the GitHub catalog and the policy
// template: the config's paths, else the flags, else the release layout
// (web/, vendor/ and config/ beside the bin/ directory of an unpacked
// release tarball), else the repository layout around dist/chat (the Python
// launcher's ROOT), else beside the binaries.
func locateAssets(cfg config.Config, binDir, web, vendor, template string) (assets, error) {
	a := assets{web: cfg.Paths.WebAssets, vendor: cfg.Paths.GitHubCatalog, template: cfg.Paths.SandboxPolicyTemplate}
	if a.web == "" {
		a.web = web
	}
	if a.vendor == "" {
		a.vendor = vendor
	}
	if a.template == "" {
		a.template = template
	}
	release := filepath.Dir(binDir)
	root := filepath.Dir(release)
	candidates := []struct {
		dst  *string
		rel  []string
		what string
	}{
		{&a.web, []string{filepath.Join(release, "web"), filepath.Join(root, "chat", "web", "dist"), filepath.Join(binDir, "web")}, "built chat UI (paths.webAssets or --web-dir)"},
		{&a.vendor, []string{filepath.Join(release, "vendor"), filepath.Join(root, "vendor"), filepath.Join(binDir, "vendor")}, "GitHub catalog directory (paths.githubCatalog or --vendor-dir)"},
		{&a.template, []string{filepath.Join(release, "config", "policy.template.json"), filepath.Join(root, "config", "policy.template.json"), filepath.Join(binDir, "policy.template.json")}, "sandbox policy template (paths.sandboxPolicyTemplate or --policy-template)"},
	}
	for _, cand := range candidates {
		if *cand.dst == "" {
			for _, p := range cand.rel {
				if _, err := os.Stat(p); err == nil {
					*cand.dst = p
					break
				}
			}
		}
		if *cand.dst == "" {
			return a, fmt.Errorf("cannot find the %s near %s", cand.what, binDir)
		}
		if _, err := os.Stat(*cand.dst); err != nil {
			return a, fmt.Errorf("%s: %w", cand.what, err)
		}
	}
	return a, nil
}

type launcher struct {
	c           *cli
	cfg         config.Config
	configPath  string
	exe         string // this executable; every service is a subcommand of it
	assets      assets
	withoutEdge bool
	popups      string // --popups mode
	detached    bool   // started by --detach: nobody is watching this terminal

	procs []*service
	// bugs drafts the launcher's own reports (a service exiting); drafts
	// watches the pending directory and presents each new draft once.
	bugs   *bugreport.Capturer
	drafts *draftWatcher
}

type service struct {
	name    string
	cmd     *exec.Cmd
	log     *os.File
	done    chan error
	stopped bool // shutdown has dealt with it
}

func (l *launcher) run() error {
	cfg := l.cfg
	state := cfg.Paths.State
	for _, dir := range []string{cfg.PolicyState(), cfg.RunnerState(), cfg.AppState(), cfg.EdgeState()} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(filepath.Join(state, "launcher.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Warden is already running or shutting down in this state directory.")
	}
	fmt.Fprintf(l.c.stdout, "warden: %s\n", handshake.Self("warden"))
	wrapper := wrapperPath(state)
	if err = executableFile(wrapper); err != nil {
		return fmt.Errorf("%s: %w; run `warden install`", wrapper, err)
	}
	// The private sandbox daemon is not a system service; after a reboot it
	// is gone. Start it here so a plain `warden start` works.
	if err = ensureDaemonRunning(&sbxCLI{wrapper: wrapper, stdin: l.c.stdin, stdout: l.c.stdout, stderr: l.c.stderr}, func(name, detail string) {
		fmt.Fprintf(l.c.stdout, "warden: %s: %s\n", name, detail)
	}); err != nil {
		return err
	}
	env := append(os.Environ(), config.Env+"="+l.configPath)
	// The policy service and the runner run sbx themselves (sbx.executable,
	// not the wrapper), so they get the namespace environment the OVH
	// compose file gives its containers; without it every sandbox would be
	// created in the operator's own namespace.
	sbxEnv := namespaceEnv(env, cfg.SBX.PrivateHome)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer l.shutdown()

	if err = l.launch(ctx, "warden-policy", sbxEnv, append([]string{"policy"}, l.policyArgs()...), cfg.PolicySocket()); err != nil {
		return err
	}
	if err = l.launch(ctx, "warden-runner", sbxEnv, append([]string{"runner"}, l.runnerArgs()...), cfg.RunnerSocket()); err != nil {
		return err
	}
	if err = l.launch(ctx, "warden-chat", env, append([]string{"serve"}, l.chatArgs()...), cfg.OwnerTokenFile()); err != nil {
		return err
	}
	if l.withoutEdge {
		fmt.Fprintln(l.c.stdout, "warden: edge not started (--without-edge)")
	} else if err = l.launch(ctx, "warden-edge", env, []string{"edge", "--config", l.configPath}, ""); err != nil {
		return err
	}
	fmt.Fprintf(l.c.stdout, "Warden started with state %s. Run `warden open` to open it. Ctrl+C stops this stack.\n", state)
	// Surface approvals while the stack runs: a desktop notification, and
	// when nobody is watching a terminal, the app opened on the chat.
	go popups(ctx, cfg, l.popups, l.detached, l.c.stdout)
	// Bug reports: every draft a service (or the launcher itself) writes is
	// shown once on the review page (docs/bug-reporting-plan.md).
	l.bugs = bugreport.New(cfg, l.configPath, bugreport.ComponentLauncher)
	l.drafts = &draftWatcher{state: state, log: l.c.stdout, present: func(ctx context.Context, path string) (bugreport.Outcome, error) {
		return l.c.presenter(l.cfg, l.detached).Present(ctx, path)
	}}
	if waiting := l.drafts.start(); waiting > 0 && l.bugs.Enabled() {
		fmt.Fprintf(l.c.stdout, "warden: %d bug report draft(s) waiting for a decision; `warden bugs pending` shows them\n", waiting)
	}
	go l.drafts.run(ctx)
	select {
	case <-ctx.Done():
		fmt.Fprintln(l.c.stdout, "warden: stopping")
		return nil
	case name := <-l.anyExit():
		err := fmt.Errorf("%s stopped; inspect %s", name, filepath.Join(state, name+".log"))
		// The stack is down either way; stop the rest first, then draft
		// and show the report (the service's log tail and the launcher's).
		l.shutdown()
		l.reportExit(ctx, name)
		return err
	}
}

// reportExit drafts the service-exit report for name and presents every
// draft not yet shown (the new one, or the panic draft the service wrote
// on its way down).
func (l *launcher) reportExit(ctx context.Context, name string) {
	if !l.bugs.Enabled() {
		return
	}
	exit := "exited"
	for _, s := range l.procs {
		if s.name == name && s.cmd.ProcessState != nil {
			exit = s.cmd.ProcessState.String()
		}
	}
	if _, _, err := exitDraft(l.bugs, name, exit, time.Now()); err != nil {
		fmt.Fprintf(l.c.stdout, "warden: bug report of the %s exit not written: %v\n", name, err)
	}
	l.drafts.presentNew(ctx)
}

// launch starts one service (this executable with the service subcommand
// first in args) with its output appended to <state>/<name>.log and, when
// ready names a file, waits until that file appears or changes (inode or
// mtime), giving up after 10 s.
func (l *launcher) launch(ctx context.Context, name string, env, args []string, ready string) error {
	var previous string
	if ready != "" {
		previous = fileStamp(ready)
	}
	logPath := filepath.Join(l.cfg.Paths.State, name+".log")
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(l.exe, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Dir = l.cfg.Paths.State
	if err = cmd.Start(); err != nil {
		log.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	s := &service{name: name, cmd: cmd, log: log, done: make(chan error, 1)}
	go func() { s.done <- cmd.Wait() }()
	l.procs = append(l.procs, s)
	if ready == "" {
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return fmt.Errorf("%s failed; see %s", name, logPath)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if stamp := fileStamp(ready); stamp != "" && stamp != previous {
			return nil
		}
	}
	return fmt.Errorf("%s did not become ready; see %s", name, logPath)
}

// fileStamp identifies a file's current incarnation ("" when absent).
func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", inode(info), info.ModTime().UnixNano())
}

func inode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// anyExit yields the name of the first service to stop.
func (l *launcher) anyExit() <-chan string {
	out := make(chan string, len(l.procs))
	for _, s := range l.procs {
		go func(s *service) {
			<-s.done
			s.done <- nil
			out <- s.name
		}(s)
	}
	return out
}

// shutdown stops the services in reverse order: SIGTERM, 15 s, then SIGKILL.
// A second call finds nothing running.
func (l *launcher) shutdown() {
	for i := len(l.procs) - 1; i >= 0; i-- {
		s := l.procs[i]
		if s.stopped {
			continue
		}
		s.stopped = true
		if s.cmd.ProcessState != nil {
			s.log.Close()
			continue
		}
		s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(15 * time.Second):
			s.cmd.Process.Kill()
			<-s.done
		}
		s.log.Close()
	}
}

// Service arguments: the configuration file plus the resolved asset paths
// the file leaves empty (a flag may fill an empty field, never disagree
// with one).

func (l *launcher) policyArgs() []string {
	args := []string{"--config", l.configPath}
	if l.cfg.Paths.GitHubCatalog == "" && l.assets.vendor != "" {
		args = append(args, "--vendor-dir", l.assets.vendor)
	}
	if l.cfg.Paths.SandboxPolicyTemplate == "" && l.assets.template != "" {
		args = append(args, "--policy-template", l.assets.template)
	}
	return args
}

func (l *launcher) runnerArgs() []string {
	return []string{"--config", l.configPath}
}

func (l *launcher) chatArgs() []string {
	args := []string{"--config", l.configPath}
	if l.cfg.Paths.WebAssets == "" && l.assets.web != "" {
		args = append(args, "--web-dir", l.assets.web)
	}
	return args
}

// open reads <state>/app/endpoint.json and opens the browser on the private
// launch URL.
func (c *cli) open(args []string) error {
	fs := flag.NewFlagSet("warden open", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	print := fs.Bool("print", false, "print the URL instead of opening a browser")
	withoutEdge := fs.Bool("without-edge", false, "open the chat origin directly (when warden start ran with --without-edge)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	url, err := launchURL(cfg.OwnerTokenFile(), time.Now())
	if err != nil {
		return err
	}
	// In owner mode the edge fronts the chat: opening the app through it
	// gives the browser the owner session that preview navigations need.
	if cfg.Auth.Mode == config.AuthOwner && cfg.Auth.PublicURL != "" && !*withoutEdge {
		url, err = throughEdge(url, cfg.Auth.PublicURL)
		if err != nil {
			return err
		}
	}
	if *print {
		fmt.Fprintln(c.stdout, url)
		return nil
	}
	if err = openBrowser(url); err != nil {
		fmt.Fprintf(c.stdout, "open this URL in your browser:\n%s\n", url)
	}
	return nil
}

// throughEdge rewrites the chat launch URL onto the edge origin, keeping
// the launch query and the capability fragment.
func throughEdge(launch, publicURL string) (string, error) {
	u, err := neturl.Parse(launch)
	if err != nil {
		return "", err
	}
	base, err := neturl.Parse(publicURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("auth.publicURL %q is not an absolute URL", publicURL)
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	return u.String(), nil
}

func launchURL(endpointFile string, now time.Time) (string, error) {
	raw, err := os.ReadFile(endpointFile)
	if err != nil {
		return "", fmt.Errorf("%w; run `warden start` first (it stays in the foreground), then `warden open` in another terminal", err)
	}
	var endpoint struct{ URL, Token string }
	if err = json.Unmarshal(raw, &endpoint); err != nil || endpoint.URL == "" || endpoint.Token == "" {
		return "", errors.New(endpointFile + " is not a chat endpoint file")
	}
	return endpoint.URL + "/?launch=" + strconv.FormatInt(now.UnixNano(), 10) + "#session=" + endpoint.Token, nil
}

// copyToClipboard puts text on the system clipboard where a clipboard
// command exists (pbcopy on macOS, xclip or wl-copy on Linux).
func copyToClipboard(text string) error {
	for _, candidate := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		path, err := exec.LookPath(candidate[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, candidate[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return errors.New("no clipboard command found")
}

// openBrowser opens url in the person's browser: the command $BROWSER
// names when set (the convention xdg-open and gh follow; a script that
// records the URL serves a headless machine or a test), else /usr/bin/open
// on macOS or xdg-open.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("BROWSER") != "":
		cmd = exec.Command(os.Getenv("BROWSER"), url)
	case executableFile("/usr/bin/open") == nil:
		cmd = exec.Command("/usr/bin/open", url)
	default:
		path, err := exec.LookPath("xdg-open")
		if err != nil {
			return err
		}
		cmd = exec.Command(path, url)
	}
	return cmd.Run()
}

// namespaceEnv replaces HOME and the XDG variables in env with Warden's
// private SBX namespace, exactly as the wrapper script and the OVH compose
// x-environment do.
func namespaceEnv(env []string, privateHome string) []string {
	out := make([]string, 0, len(env)+len(namespaceDirs))
	for _, kv := range env {
		keep := true
		for _, d := range namespaceDirs {
			if strings.HasPrefix(kv, d.env+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	for _, d := range namespaceDirs {
		out = append(out, d.env+"="+filepath.Join(privateHome, d.dir))
	}
	return out
}
