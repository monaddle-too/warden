package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// installer holds one `warden install` run. Every step is idempotent: a
// re-run finds its previous work and reports it as already done. It never
// deletes sandboxes, never replaces a provider login and never touches the
// operator's own SBX namespace.
type installer struct {
	c          *cli
	state      string
	configPath string
	sbxFlag    string
	upgrade    bool
	guestTar   string
	sbxLogin   bool
	bugReports string // --bug-reports: yes, no, or "" to ask
	client     *http.Client
	memoryMB   int
	cpus       int

	arch    string
	sbxExe  string
	sbx     *sbxCLI
	changed []string
	// phase is the step under way, for the report a failure drafts;
	// transcript is everything printed so far, the report's log.
	phase      string
	transcript bytes.Buffer
	// reporting is the bug-reporting answer once decided (decided false
	// until the question is asked, answered by the flag or found in an
	// existing warden.json).
	reporting bool
	decided   bool
}

const sbxLoginMarker = "login.json"

func (c *cli) install(args []string) error {
	fs := flag.NewFlagSet("warden install", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	in := &installer{c: c, client: &http.Client{}, memoryMB: hostMemoryMB(), cpus: runtime.NumCPU()}
	fs.StringVar(&in.state, "state", "", "state directory (default: the platform's application data directory)")
	fs.StringVar(&in.configPath, "config", "", "where to write warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	fs.StringVar(&in.sbxFlag, "sbx", "", "the sbx executable (default: found on $PATH)")
	fs.BoolVar(&in.upgrade, "upgrade", false, "accept a state directory installed by another Warden release")
	fs.StringVar(&in.guestTar, "guest-image-tar", "", "a saved Warden guest image tar to load when the release pins one for this architecture")
	fs.BoolVar(&in.sbxLogin, "sbx-login", true, "run the SBX device login when Warden's namespace is not signed in yet")
	fs.StringVar(&in.bugReports, "bug-reports", "", "yes or no: send bug reports to Monaddle (you review every report before it is sent); asked on the terminal when not given")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(c.stderr, "warden install: unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	switch in.bugReports {
	case "", "yes", "no":
	default:
		fmt.Fprintf(c.stderr, "warden install: --bug-reports must be yes or no, not %q\n", in.bugReports)
		return errUsage
	}
	// Everything printed is also kept: a failure's report carries the
	// installer's own output so far.
	in.c = &cli{stdin: c.stdin, stdout: io.MultiWriter(c.stdout, &in.transcript), stderr: io.MultiWriter(c.stderr, &in.transcript), terminal: c.terminal, openFn: c.openFn, notifyFn: c.notifyFn}
	err := in.run()
	if err != nil {
		in.reportFailure(err)
	}
	return err
}

func (in *installer) step(name, result string) {
	fmt.Fprintf(in.c.stdout, "%-16s %s\n", name+":", result)
}

func (in *installer) run() error {
	var err error
	if in.state, err = resolveState(in.state); err != nil {
		return err
	}
	if in.configPath == "" {
		in.configPath = defaultConfigPath(in.state)
	}
	if in.arch, err = guestArch(); err != nil {
		return err
	}
	fmt.Fprintf(in.c.stdout, "Warden %s installing into %s (guest architecture %s)\n", revision, in.state, in.arch)

	// 1. State root, owner-only, and the release record.
	in.phase = "state"
	if err = ensurePrivateDir(in.state); err != nil {
		return err
	}
	if err = checkRecord(in.state, currentRecord(in.arch), in.upgrade); err != nil {
		return err
	}
	for _, sub := range stateSubdirs {
		if err = ensurePrivateDir(filepath.Join(in.state, sub)); err != nil {
			return err
		}
	}
	probe := config.Defaults(in.state)
	if err = probe.Validate(); err != nil {
		return fmt.Errorf("state directory %s: %w", in.state, err)
	}
	in.step("state", in.state+" (owner-only)")

	// The one question: asked once, before the slow steps, so a failure
	// among them can be reported; a re-run keeps the earlier answer.
	in.phase = "bug reports"
	if err = in.decideBugReports(); err != nil {
		return err
	}

	// 2. The sbx executable and the private namespace wrapper.
	in.phase = "sbx namespace"
	if in.sbxExe, err = findSBX(in.sbxFlag); err != nil {
		return err
	}
	privateHome := filepath.Join(in.state, "sbx")
	changed, err := ensureNamespace(in.state, privateHome, in.sbxExe)
	if err != nil {
		return err
	}
	in.sbx = &sbxCLI{wrapper: wrapperPath(in.state), stdin: in.c.stdin, stdout: in.c.stdout, stderr: in.c.stderr}
	if changed {
		in.step("sbx namespace", "wrote "+in.sbx.wrapper+" for "+in.sbxExe)
	} else {
		in.step("sbx namespace", in.sbx.wrapper+" already selects "+privateHome)
	}

	// 3. Daemon and settings.
	in.phase = "sbx daemon"
	if err = in.ensureDaemon(); err != nil {
		return err
	}
	in.phase = "sbx settings"
	if err = in.ensureSettings(); err != nil {
		return err
	}

	// 4. SBX device login. It must precede the host checks: an unsigned-in
	// daemon answers the MCP and policy queries with 401.
	in.phase = "sbx login"
	if err = in.ensureSBXLogin(); err != nil {
		return err
	}

	// 5. The host invariants the verifier enforces.
	in.phase = "host checks"
	checks := hostChecks(in.sbx)
	printChecks(in.c.stdout, checks)
	if failed(checks) {
		return errors.New("the SBX namespace does not satisfy the policy verifier; apply the remediation above and re-run warden install")
	}

	// 6. Runtimes.
	runtimes := filepath.Join(in.state, "runtimes")
	in.phase = "codex"
	if err = in.ensureCodex(runtimes); err != nil {
		return err
	}
	in.phase = "claude"
	if err = in.ensureClaude(runtimes); err != nil {
		return err
	}

	// 7. Guest image, or the stock template.
	in.phase = "guest image"
	image, digest, err := in.ensureGuestImage()
	if err != nil {
		return err
	}

	// 8. warden.json with the detected facts.
	in.phase = "config"
	cfg, err := in.compose(privateHome, runtimes, image, digest)
	if err != nil {
		return err
	}
	if err = config.Write(in.configPath, cfg); err != nil {
		return err
	}
	in.step("config", in.configPath)
	if err = writeRecord(in.state, currentRecord(in.arch)); err != nil {
		return err
	}
	fmt.Fprintf(in.c.stdout, "\nInstalled. Bug reports: %s. Next: `warden login codex` (and `warden login claude`, `warden login github` as needed), then `warden start` and `warden open`.\n", in.bugReportsSummary())
	return nil
}

// bugReportsQuestion is asked once on the terminal.
const bugReportsQuestion = "Send bug reports to Monaddle? You review every report before it is sent. [y/N] "

// decideBugReports settles the bug-reporting answer: the flag when given;
// else an existing warden.json's setting (a re-run keeps the earlier
// answer); else the question on the terminal; else no (with the way to
// change it) when nobody can be asked.
func (in *installer) decideBugReports() error {
	existing, found := reportingIn(in.configPath)
	switch {
	case in.bugReports == "yes":
		in.reporting = true
	case in.bugReports == "no":
		in.reporting = false
	case found:
		in.reporting = existing
	case !in.c.terminal:
		in.reporting = false
		in.step("bug reports", "off (no terminal to ask; --bug-reports=yes or `warden bugs on` to enable)")
		in.decided = true
		return nil
	default:
		fmt.Fprint(in.c.stdout, "\n"+bugReportsQuestion)
		answer, err := readAnswer(in.c.stdin)
		fmt.Fprintln(in.c.stdout)
		if err != nil && answer == "" {
			// No answer at all (stdin closed): the default.
			in.reporting = false
		} else {
			in.reporting = yes(answer)
		}
	}
	in.decided = true
	if in.reporting {
		in.step("bug reports", "on — every report is shown to you before it is sent (`warden bugs off` to stop)")
	} else {
		in.step("bug reports", "off (`warden bugs on` to enable)")
	}
	return nil
}

func (in *installer) bugReportsSummary() string {
	if in.reporting {
		return "on (you review each one before it is sent; `warden bugs off` to stop)"
	}
	return "off (`warden bugs on` to enable)"
}

func yes(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// readAnswer reads one line byte by byte (unlike login's buffered
// readLine), so nothing after it is consumed from a stdin the SBX login is
// about to share.
func readAnswer(r io.Reader) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimRight(string(line), "\r"), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return string(line), err
		}
	}
}

// reportingIn reads whether path has a reporting section (the question
// was answered before) and what it says.
func reportingIn(path string) (enabled, found bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var file struct {
		Reporting *struct {
			Enabled bool `json:"enabled"`
		} `json:"reporting"`
	}
	if json.Unmarshal(raw, &file) != nil || file.Reporting == nil {
		return false, false
	}
	return file.Reporting.Enabled, true
}

// reportFailure drafts and presents the report of a failed step when bug
// reporting is on: the step, the error and the installer's output so far.
// Off, or never asked (a failure before the question), nothing happens.
func (in *installer) reportFailure(err error) {
	if !in.decided || !in.reporting || errors.Is(err, errUsage) {
		return
	}
	cfg := config.Defaults(in.state)
	if existing, loadErr := config.Load(in.configPath, in.state); loadErr == nil {
		cfg = existing
	}
	cfg.Reporting.Enabled = true
	cap := bugreport.New(cfg, "", bugreport.ComponentInstall)
	r := cap.Draft(bugreport.KindError, bugreport.TriggerInstallStep, "warden install failed at "+in.phase+": "+bugreport.Summarize(err.Error()))
	r.Error = &bugreport.Error{Message: err.Error(), Operation: in.phase}
	r.Logs = []bugreport.Log{{Name: "warden install output", Lines: bugreport.TailText(in.transcript.String())}}
	draft, written, captureErr := cap.Capture(r)
	if captureErr != nil || !written {
		return
	}
	if !in.c.terminal {
		fmt.Fprintf(in.c.stdout, "\nBug report %s drafted; `warden bugs pending` shows it for review.\n", r.ID)
		return
	}
	fmt.Fprintln(in.c.stdout)
	if presentErr := in.c.present(cfg, draft); presentErr != nil {
		fmt.Fprintf(in.c.stdout, "could not show the bug report: %v\n", presentErr)
	}
}

// ensureDaemon starts the daemon in Warden's namespace with the deny-all
// initial policy when it is not running.
func (in *installer) ensureDaemon() error {
	return ensureDaemonRunning(in.sbx, in.step)
}

// ensureDaemonRunning starts the daemon in Warden's namespace unless it is
// already running. `sbx daemon status` exits 0 whether the daemon is running
// or stopped; only its "Status:" line says which. Both `warden install` and
// `warden start` call this: the daemon is not a system service, so after a
// reboot it is gone until something starts it again.
//
// After an sbx upgrade the CLI refuses to talk to the older daemon until it
// is restarted and asks for confirmation on the terminal, which Warden never
// gives it ("cannot prompt for restart: stdin is not a terminal"). The daemon
// is Warden's own, and a stopped sandbox restarts on its next use, so the
// restart is done here without asking: either because a command answered
// with that prompt, or because `daemon inspect` reports a version other than
// the CLI's.
func ensureDaemonRunning(sbx *sbxCLI, step func(name, detail string)) error {
	out, err := sbx.command(20*time.Second, "daemon", "status")
	if daemonNeedsRestart(err) {
		return restartDaemon(sbx, step, "the running daemon predates the sbx CLI")
	}
	if err == nil && daemonRunning(out) {
		if reason := daemonStale(sbx); reason != "" {
			return restartDaemon(sbx, step, reason)
		}
		step("sbx daemon", "running")
		return nil
	}
	out, err = sbx.command(3*time.Minute, "daemon", "start", "--policy", "deny-all", "--detach")
	if err != nil {
		return fmt.Errorf("starting the SBX daemon in Warden's namespace: %w", err)
	}
	step("sbx daemon", "started with --policy deny-all"+trailer(out))
	return nil
}

// daemonNeedsRestart recognises sbx's refusal to use a daemon older than
// the CLI without an interactive confirmation.
func daemonNeedsRestart(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "cannot prompt for restart") || strings.Contains(text, "needs to restart")
}

// daemonStale compares the CLI's version with the running daemon's and
// returns a reason to restart when they differ (or when asking triggers the
// restart prompt); "" when they match or the question cannot be answered.
func daemonStale(sbx *sbxCLI) string {
	cli, daemon, err := daemonVersions(sbx)
	switch {
	case daemonNeedsRestart(err):
		return "the running daemon predates the sbx CLI"
	case err != nil || cli == "" || daemon == "" || cli == daemon:
		return ""
	default:
		return "the daemon is " + daemon + " but the sbx CLI is " + cli
	}
}

// daemonVersions reads the CLI version from `sbx version` ("sbx version:
// vX.Y.Z build") and the daemon's from `daemon inspect` (daemon_version).
func daemonVersions(sbx *sbxCLI) (cli, daemon string, err error) {
	out, err := sbx.run([]string{"version"}, false)
	if err != nil {
		return "", "", err
	}
	if fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(firstLine(out)), sbxVersionPrefix)); len(fields) > 0 {
		cli = fields[0]
	}
	info, err := sbx.jsonObject([]string{"daemon", "inspect"}, false)
	if err != nil {
		return cli, "", err
	}
	daemon, _ = info["daemon_version"].(string)
	return cli, daemon, nil
}

func restartDaemon(sbx *sbxCLI, step func(name, detail string), reason string) error {
	out, err := sbx.command(3*time.Minute, "daemon", "restart")
	if err != nil {
		return fmt.Errorf("restarting the SBX daemon in Warden's namespace (%s): %w", reason, err)
	}
	step("sbx daemon", "restarted: "+reason+"; its sandboxes start again on their next use"+trailer(out))
	return nil
}

// daemonRunning reads the "Status:" line of `sbx daemon status`.
func daemonRunning(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Status:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Status:")) == "running"
		}
	}
	return false
}

// ensureSettings sets the two verifier-required settings when they differ
// and restarts the daemon if SBX says a change needs it.
func (in *installer) ensureSettings() error {
	restart := false
	for _, setting := range hostSettings {
		result, err := in.sbx.jsonObject([]string{"settings", "get", "--json", setting.key}, false)
		if settingUndefined(err) {
			in.step("sbx setting", setting.key+" is not defined by this sbx; skipped (the feature it governs is absent)")
			continue
		}
		if err == nil && result["key"] == setting.key && result["value"] == setting.required {
			in.step("sbx setting", setting.key+"="+setting.value+" already")
			continue
		}
		out, err := in.sbx.command(30*time.Second, "settings", "set", setting.key, setting.value)
		if settingUndefined(err) {
			in.step("sbx setting", setting.key+" is not defined by this sbx; skipped (the feature it governs is absent)")
			continue
		}
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(out), "restart") {
			restart = true
		}
		in.step("sbx setting", setting.key+"="+setting.value+" set"+trailer(out))
	}
	if restart {
		if _, err := in.sbx.command(3*time.Minute, "daemon", "restart"); err != nil {
			return err
		}
		in.step("sbx daemon", "restarted for the new settings")
	}
	return nil
}

// ensureSBXLogin runs the device login once. SBX has no non-interactive
// "am I signed in" query, so the completed login is recorded in the
// namespace and never repeated unless that record is removed.
func (in *installer) ensureSBXLogin() error {
	marker := filepath.Join(in.state, "sbx", sbxLoginMarker)
	if _, err := os.Stat(marker); err == nil {
		in.step("sbx login", "already signed in (remove "+marker+" to sign in again)")
		return nil
	}
	// A daemon that is signed in answers the MCP query; one that is not
	// answers 401. This recognises a login the operator completed in their
	// own terminal (for example after a keychain prompt) without repeating it.
	if _, err := in.sbx.jsonObject([]string{"mcp", "ls", "--json"}, false); err == nil {
		b, _ := json.Marshal(map[string]string{"signedInAt": time.Now().UTC().Format(time.RFC3339), "detected": "true"})
		if err := atomicWrite(marker, append(b, '\n'), 0o600); err != nil {
			return err
		}
		in.step("sbx login", "already signed in (detected)")
		return nil
	}
	// Without a sign-in the daemon answers every policy query with 401, so
	// nothing after this step can succeed; stop here with the exact command.
	if !in.sbxLogin {
		in.step("sbx login", "skipped (--sbx-login=false)")
		return errSBXLoginRequired(in.sbx.wrapper)
	}
	if !in.c.terminal {
		in.step("sbx login", "needs a terminal")
		return errSBXLoginRequired(in.sbx.wrapper)
	}
	fmt.Fprintf(in.c.stdout, "\nSigning Warden's SBX namespace in to Docker. Follow the device-code instructions below; this is Warden's private sign-in, separate from any sbx login of your own.\n\n")
	if err := in.sbx.interactive("login"); err != nil {
		// On macOS sbx keeps its Docker session in the login keychain; saving
		// it needs a keychain prompt that only an interactive terminal in the
		// user's GUI session can show.
		return fmt.Errorf("%w\nIf the error mentions the Keychain, run this in your own terminal window and then re-run warden install:\n  %s login", err, in.sbx.wrapper)
	}
	b, _ := json.Marshal(map[string]string{"signedInAt": time.Now().UTC().Format(time.RFC3339)})
	if err := atomicWrite(marker, append(b, '\n'), 0o600); err != nil {
		return err
	}
	in.step("sbx login", "signed in")
	return nil
}

// errSBXLoginRequired tells the operator how to sign Warden's namespace in.
func errSBXLoginRequired(wrapper string) error {
	return fmt.Errorf("Warden's SBX namespace is not signed in to Docker. Run this in your own terminal window (macOS may show a keychain prompt), then re-run warden install:\n  %s login", wrapper)
}

// ensureCodex fetches and extracts the pinned bundle for the guest
// architecture unless the installed one already came from that archive.
func (in *installer) ensureCodex(runtimes string) error {
	src, _ := runtimeSources(in.arch)
	dir := codexDir(runtimes)
	if marker, err := os.ReadFile(filepath.Join(dir, shaMarker)); err == nil && strings.TrimSpace(string(marker)) == src.SHA256 && src.SHA256 != "" {
		if err := worldReadable(dir); err == nil && checkCodexBundle(dir, in.arch) == nil {
			in.step("codex", "Codex "+release.CodexVersion+" already at "+dir)
			return nil
		}
	}
	if src.SHA256 == "" || src.URL == "" {
		return fmt.Errorf("this release does not yet pin the Codex %s bundle for %s guests (its SHA-256 is empty in chat/internal/release); a later Warden release will", release.CodexVersion, in.arch)
	}
	archive := filepath.Join(runtimes, "codex-package-"+in.arch+".tar.gz")
	if sha, err := fileSHA256(archive); err != nil || sha != src.SHA256 {
		fmt.Fprintf(in.c.stdout, "fetching %s\n", src.URL)
		ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
		defer cancel()
		if err = download(ctx, in.client, src.URL, src.SHA256, archive, 0o600); err != nil {
			return err
		}
	}
	fresh := dir + ".new"
	os.RemoveAll(fresh)
	if err := extractTarGz(archive, fresh); err != nil {
		os.RemoveAll(fresh)
		return err
	}
	if err := worldReadable(fresh); err != nil {
		os.RemoveAll(fresh)
		return err
	}
	if err := checkCodexBundle(fresh, in.arch); err != nil {
		os.RemoveAll(fresh)
		return fmt.Errorf("the downloaded bundle is not what the runner expects: %w", err)
	}
	if err := os.WriteFile(filepath.Join(fresh, shaMarker), []byte(src.SHA256+"\n"), 0o644); err != nil {
		return err
	}
	old := dir + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(dir); err == nil {
		if err = os.Rename(dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(fresh, dir); err != nil {
		return err
	}
	os.RemoveAll(old)
	os.Remove(archive)
	in.step("codex", "Codex "+release.CodexVersion+" "+codexTarget(in.arch)+" installed at "+dir)
	return nil
}

// ensureClaude fetches the pinned Claude executable unless the installed one
// already has the pinned SHA-256.
func (in *installer) ensureClaude(runtimes string) error {
	_, src := runtimeSources(in.arch)
	path := claudePath(runtimes)
	if src.SHA256 != "" && checkClaude(path, src.SHA256) == nil {
		in.step("claude", "Claude Code "+release.ClaudeVersion+" already at "+path)
		return nil
	}
	if src.SHA256 == "" || src.URL == "" {
		return fmt.Errorf("this release does not yet pin the Claude Code %s executable for %s guests (its SHA-256 is empty in chat/internal/release); a later Warden release will", release.ClaudeVersion, in.arch)
	}
	fmt.Fprintf(in.c.stdout, "fetching %s\n", src.URL)
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	if err := download(ctx, in.client, src.URL, src.SHA256, path, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	in.step("claude", "Claude Code "+release.ClaudeVersion+" installed at "+path)
	return nil
}

// ensureGuestImage loads the published guest image when the release pins
// one for this architecture, otherwise records the stock template the
// runner copies runtimes into.
func (in *installer) ensureGuestImage() (image, digest string, err error) {
	pinned := release.GuestImages[in.arch]
	if pinned.Digest == "" {
		in.step("guest image", "none pinned for "+in.arch+"; new sandboxes use "+release.StockTemplate+" and the runner copies the runtimes in")
		return release.StockTemplate, release.StockTemplateDigest, nil
	}
	if out, err := in.sbx.run([]string{"template", "ls", "--json"}, false); err == nil && templateListed(out, pinned.Digest) {
		in.step("guest image", pinned.Ref+"@"+pinned.Digest+" already loaded")
		return pinned.Ref, pinned.Digest, nil
	}
	tarPath := in.guestTar
	if tarPath == "" {
		docker, lookErr := exec.LookPath("docker")
		if lookErr != nil {
			in.step("guest image", pinned.Ref+"@"+pinned.Digest+" is not loaded; pass --guest-image-tar FILE (docker save of that image) or install docker so warden install can pull it. Until then new sandboxes use "+release.StockTemplate)
			return release.StockTemplate, release.StockTemplateDigest, nil
		}
		tmp, err := os.CreateTemp("", "warden-guest-*.tar")
		if err != nil {
			return "", "", err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		ref := pinned.Ref + "@" + pinned.Digest
		for _, args := range [][]string{{"pull", "-q", ref}, {"save", "-o", tmp.Name(), ref}} {
			cmd := exec.Command(docker, args...)
			cmd.Stdout, cmd.Stderr = in.c.stdout, in.c.stderr
			if err = cmd.Run(); err != nil {
				return "", "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
			}
		}
		tarPath = tmp.Name()
	}
	if _, err = in.sbx.command(20*time.Minute, "template", "load", tarPath); err != nil {
		return "", "", err
	}
	out, err := in.sbx.run([]string{"template", "ls", "--json"}, false)
	if err != nil {
		return "", "", err
	}
	if !templateListed(out, pinned.Digest) {
		return "", "", fmt.Errorf("the loaded template does not report digest %s; the runner would refuse it", pinned.Digest)
	}
	in.step("guest image", pinned.Ref+"@"+pinned.Digest+" loaded")
	return pinned.Ref, pinned.Digest, nil
}

// compose builds warden.json: the local defaults (or the existing file, so
// operator edits survive a re-run) overlaid with what this run detected.
// Ports are chosen on the first install only; a re-run while Warden is
// running must not move them.
func (in *installer) compose(privateHome, runtimes, image, digest string) (config.Config, error) {
	cfg := config.Defaults(in.state)
	fresh := true
	if _, err := os.Stat(in.configPath); err == nil {
		fresh = false
		if cfg, err = config.Load(in.configPath, in.state); err != nil {
			return cfg, fmt.Errorf("existing %s: %w", in.configPath, err)
		}
	}
	cfg.SBX.Executable = in.sbxExe
	cfg.SBX.PrivateHome = privateHome
	cfg.SBX.GuestImage = image
	cfg.SBX.GuestImageDigest = digest
	cfg.Runtimes.Codex = codexDir(runtimes)
	cfg.Runtimes.Claude = claudePath(runtimes)
	cfg.Sandboxes = sizeSandboxes(cfg.Sandboxes, in.memoryMB, in.cpus)
	if in.decided {
		cfg.Reporting.Enabled = in.reporting
	}
	if fresh {
		ports, err := freeLoopbackPorts(portOf(cfg.Chat.Listen), portOf(cfg.Previews.EdgeListen))
		if err != nil {
			return cfg, err
		}
		cfg.Chat.Listen = listenAddr(cfg.Chat.Listen, ports[0])
		cfg.Previews.EdgeListen = listenAddr(cfg.Previews.EdgeListen, ports[1])
		if cfg.Auth.Mode == config.AuthOwner {
			cfg.Auth.PublicURL = "http://" + cfg.Previews.EdgeListen
		}
	}
	in.step("host", fmt.Sprintf("%d MiB RAM, %d cores → %d sandbox(es) of %d MiB, %d warm spare; chat %s, previews %s", in.memoryMB, in.cpus, cfg.Sandboxes.MaxRunning, cfg.Sandboxes.MemoryMB, cfg.Sandboxes.WarmSpares, cfg.Chat.Listen, cfg.Previews.EdgeListen))
	return cfg, nil
}

func trailer(out string) string {
	out = strings.TrimSpace(firstLine(strings.TrimSpace(out)))
	if out == "" {
		return ""
	}
	return " (" + out + ")"
}
