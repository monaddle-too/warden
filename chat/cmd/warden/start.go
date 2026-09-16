package main

import (
	"bytes"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
)

// start replaces `scripts/warden-chat start`: it runs warden-policy,
// warden-runner, warden-chat and warden-edge as owner processes from the
// binaries next to this executable (or --bin-dir), with the private state,
// no Docker, the same readiness waits and single-writer locks as the Python
// launcher, and stops them all on Ctrl+C.
func (c *cli) start(args []string) error {
	fs := flag.NewFlagSet("warden start", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	binDir := fs.String("bin-dir", "", "directory holding warden-policy, warden-runner, warden-chat and warden-edge (default: next to warden)")
	webDir := fs.String("web-dir", "", "built chat UI when warden.json has no paths.webAssets (default: found beside the binaries)")
	vendorDir := fs.String("vendor-dir", "", "GitHub catalog directory when warden.json has no paths.githubCatalog")
	template := fs.String("policy-template", "", "sandbox policy template when warden.json has no paths.sandboxPolicyTemplate")
	googleConfig := fs.String("google-config", "", "operator Google OAuth client file passed to warden-policy (legacy flag mode only)")
	withoutEdge := fs.Bool("without-edge", false, "do not start warden-edge")
	detach := fs.Bool("detach", false, "run in the background; logs to <state>/warden.log, stop with `warden stop`")
	popupsMode := fs.String("popups", popupsAuto, "how pending approvals are surfaced: auto (browser when detached, notify otherwise), browser, notify, none")
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
	if *binDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		*binDir = filepath.Dir(exe)
	}
	switch *popupsMode {
	case popupsAuto, popupsBrowser, popupsNotify, popupsNone:
	default:
		return fmt.Errorf("--popups must be auto, browser, notify or none, not %q", *popupsMode)
	}
	l := &launcher{c: c, cfg: cfg, configPath: path, binDir: *binDir, googleConfig: *googleConfig, withoutEdge: *withoutEdge, popups: *popupsMode, detached: *detachedChild}
	if l.assets, err = locateAssets(cfg, *binDir, *webDir, *vendorDir, *template); err != nil {
		return err
	}
	return l.run()
}

// serviceNames in start order; shutdown is the reverse.
var serviceNames = []string{"warden-policy", "warden-runner", "warden-chat", "warden-edge"}

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
	c            *cli
	cfg          config.Config
	configPath   string
	binDir       string
	assets       assets
	googleConfig string
	withoutEdge  bool
	popups       string // --popups mode
	detached     bool   // started by --detach: nobody is watching this terminal

	procs []*service
}

type service struct {
	name string
	cmd  *exec.Cmd
	log  *os.File
	done chan error
}

func (l *launcher) binary(name string) (string, error) {
	path := filepath.Join(l.binDir, name)
	if err := executableFile(path); err != nil {
		return "", fmt.Errorf("%s: %w (build it with `go -C chat build -o %s ./cmd/%s`)", path, err, path, name)
	}
	return path, nil
}

// versionOutput runs one binary's --version; tests replace it.
var versionOutput = func(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	return string(out), err
}

// checkVersions prints the revision and protocol of every binary about to
// be launched, beside this launcher's own, and refuses a set whose protocol
// numbers differ (the services would refuse each other anyway; better one
// message here than four logs). Revisions may differ. A binary that prints
// no protocol (an older build) is reported and left out of the comparison
// so the legacy flag path below keeps working.
func (l *launcher) checkVersions(binaries map[string]string) error {
	self := handshake.Self("warden")
	fmt.Fprintf(l.c.stdout, "warden: %s\n", self)
	peers := []handshake.Peer{self}
	for _, name := range serviceNames {
		binary, ok := binaries[name]
		if !ok {
			continue
		}
		out, err := versionOutput(binary)
		if err != nil {
			fmt.Fprintf(l.c.stdout, "warden: %s: no version information (%v)\n", name, err)
			continue
		}
		peer, err := handshake.Parse(out)
		if err != nil {
			fmt.Fprintf(l.c.stdout, "warden: %s: no version information (%v)\n", name, err)
			continue
		}
		fmt.Fprintf(l.c.stdout, "warden: %s\n", peer)
		if peer.Protocol == 0 {
			fmt.Fprintf(l.c.stdout, "warden: %s reports no protocol number (older build)\n", name)
			continue
		}
		peers = append(peers, peer)
	}
	var lines []string
	for _, p := range peers[1:] {
		if p.Protocol != self.Protocol {
			lines = append(lines, p.String())
		}
	}
	if len(lines) > 0 {
		return fmt.Errorf("mismatched Warden binaries in %s: %s requires protocol %d but %s; install one release's binaries together", l.binDir, self, self.Protocol, strings.Join(lines, ", "))
	}
	return nil
}

// supportsConfigFlag probes a service binary's usage for the --config flag
// Track A adds; before that merge the equivalent legacy flags are passed.
func supportsConfigFlag(binary string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-h")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	return usageHasConfigFlag(out.String())
}

// usageHasConfigFlag looks for a "-config" flag line in flag-package usage
// output ("  -config string"); "-google-config" must not match.
func usageHasConfigFlag(usage string) bool {
	for _, line := range strings.Split(usage, "\n") {
		flagName := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(flagName, "-config") && (len(flagName) == len("-config") || flagName[len("-config")] == ' ' || flagName[len("-config")] == '\t') {
			return true
		}
	}
	return false
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
	policy, err := l.binary("warden-policy")
	if err != nil {
		return err
	}
	runner, err := l.binary("warden-runner")
	if err != nil {
		return err
	}
	chat, err := l.binary("warden-chat")
	if err != nil {
		return err
	}
	edge, edgeErr := l.binary("warden-edge")
	set := map[string]string{"warden-policy": policy, "warden-runner": runner, "warden-chat": chat}
	if edgeErr == nil && !l.withoutEdge {
		set["warden-edge"] = edge
	}
	if err = l.checkVersions(set); err != nil {
		return err
	}
	legacy := !supportsConfigFlag(policy)
	if legacy {
		fmt.Fprintln(l.c.stdout, "warden: the service binaries predate --config; passing the equivalent flags")
	}
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

	if err = l.launch(ctx, "warden-policy", policy, sbxEnv, l.policyArgs(legacy, wrapper), cfg.PolicySocket()); err != nil {
		return err
	}
	if err = l.launch(ctx, "warden-runner", runner, sbxEnv, l.runnerArgs(legacy, wrapper), cfg.RunnerSocket()); err != nil {
		return err
	}
	if err = l.launch(ctx, "warden-chat", chat, env, l.chatArgs(legacy), cfg.OwnerTokenFile()); err != nil {
		return err
	}
	switch {
	case l.withoutEdge:
		fmt.Fprintln(l.c.stdout, "warden: edge not started (--without-edge)")
	case edgeErr != nil:
		fmt.Fprintf(l.c.stdout, "warden: edge not started: %v\n", edgeErr)
	case legacy:
		fmt.Fprintln(l.c.stdout, "warden: edge not started: this warden-edge build has no local (owner, loopback) mode; previews need the loopback-preview release")
	default:
		if err = l.launch(ctx, "warden-edge", edge, env, []string{"--config", l.configPath}, ""); err != nil {
			return err
		}
	}
	fmt.Fprintf(l.c.stdout, "Warden started with state %s. Run `warden open` to open it. Ctrl+C stops this stack.\n", state)
	// Surface approvals while the stack runs: a desktop notification, and
	// when nobody is watching a terminal, the app opened on the chat.
	go popups(ctx, cfg, l.popups, l.detached, l.c.stdout)
	select {
	case <-ctx.Done():
		fmt.Fprintln(l.c.stdout, "warden: stopping")
		return nil
	case name := <-l.anyExit():
		return fmt.Errorf("%s stopped; inspect %s", name, filepath.Join(state, name+".log"))
	}
}

// launch starts one service with its output appended to <state>/<name>.log
// and, when ready names a file, waits until that file appears or changes
// (inode or mtime), as the Python launcher did, giving up after 10 s.
func (l *launcher) launch(ctx context.Context, name, binary string, env, args []string, ready string) error {
	var previous string
	if ready != "" {
		previous = fileStamp(ready)
	}
	logPath := filepath.Join(l.cfg.Paths.State, name+".log")
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(binary, args...)
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
func (l *launcher) shutdown() {
	for i := len(l.procs) - 1; i >= 0; i-- {
		s := l.procs[i]
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
	l.procs = nil
}

// Legacy flag sets: what scripts/warden-chat passed, derived from warden.json.

func (l *launcher) policyArgs(legacy bool, wrapper string) []string {
	if !legacy {
		args := []string{"--config", l.configPath}
		if l.cfg.Paths.GitHubCatalog == "" && l.assets.vendor != "" {
			args = append(args, "--vendor-dir", l.assets.vendor)
		}
		if l.cfg.Paths.SandboxPolicyTemplate == "" && l.assets.template != "" {
			args = append(args, "--policy-template", l.assets.template)
		}
		return args
	}
	cfg := l.cfg
	args := []string{"--state", cfg.PolicyState(), "--sbx", wrapper, "--manage-network", "--vendor-dir", l.assets.vendor, "--policy-template", l.assets.template,
		"--guest-image-digest", cfg.SBX.GuestImageDigest, "--gateway-ca-max-age", (time.Duration(cfg.SBX.InspectionCertMaxAgeDays) * 24 * time.Hour).String()}
	if cfg.Providers.Codex != nil {
		args = append(args, "--codex-auth-file", cfg.Providers.Codex.AuthFile)
	}
	if cfg.Providers.Claude != nil {
		args = append(args, "--claude-auth-file", cfg.Providers.Claude.AuthFile)
	}
	if l.googleConfig != "" {
		args = append(args, "--google-config", l.googleConfig)
	}
	return args
}

func (l *launcher) runnerArgs(legacy bool, wrapper string) []string {
	if !legacy {
		return []string{"--config", l.configPath}
	}
	cfg := l.cfg
	template := cfg.SBX.GuestImage
	if cfg.SBX.GuestImageDigest == release.StockTemplateDigest {
		template = cfg.SBX.GuestImage + "@" + cfg.SBX.GuestImageDigest
	}
	args := []string{"--root", cfg.RunnerState(), "--socket", cfg.RunnerSocket(), "--warden-socket", cfg.PolicySocket(), "--runtime-dir", cfg.Runtimes.Codex, "--sbx", wrapper,
		"--template", template, "--sandbox-memory-mb", strconv.Itoa(cfg.Sandboxes.MemoryMB), "--max-resident", strconv.Itoa(cfg.Sandboxes.MaxRunning), "--spare-sandboxes", strconv.Itoa(cfg.Sandboxes.WarmSpares),
		"--idle-timeout", (time.Duration(cfg.Sandboxes.StopAfterIdleMinutes) * time.Minute).String(), "--retained", strconv.Itoa(cfg.Sandboxes.KeepStopped)}
	if cfg.Runtimes.Claude != "" {
		if _, err := os.Stat(cfg.Runtimes.Claude); err == nil {
			args = append(args, "--claude-path", cfg.Runtimes.Claude)
		}
	}
	return args
}

func (l *launcher) chatArgs(legacy bool) []string {
	if !legacy {
		// A flag may fill a field the file leaves empty; it may not disagree.
		args := []string{"--config", l.configPath}
		if l.cfg.Paths.WebAssets == "" && l.assets.web != "" {
			args = append(args, "--web-dir", l.assets.web)
		}
		return args
	}
	cfg := l.cfg
	// The pre-loopback chat rejects "localhost" as a suffix; without the
	// edge there are no external previews, as with the Python launcher.
	return []string{"--state", cfg.AppState(), "--warden-socket", cfg.PolicySocket(), "--runner-socket", cfg.RunnerSocket(), "--listen", cfg.Chat.Listen, "--preview-suffix", "", "--web-dir", l.assets.web}
}

// open reads <state>/app/endpoint.json and opens the browser on the private
// launch URL, as `scripts/warden-chat open` did.
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

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch {
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
