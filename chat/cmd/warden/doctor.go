package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// doctor runs every host check the policy verifier performs and every
// bundle check the runner performs, without starting any service, and
// prints one line per check with the exact remediation. Exit status 1 when
// anything fails.
func (c *cli) doctor(args []string) error {
	fs := flag.NewFlagSet("warden doctor", flag.ContinueOnError)
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
	checks := doctorChecks(cfg, path)
	checks = append(checks, c.serviceCheck(cfg, path))
	if row, ok := c.menuCheck(cfg, path); ok {
		checks = append(checks, row)
	}
	printChecks(c.stdout, checks)
	if failed(checks) {
		return errDoctor
	}
	fmt.Fprintln(c.stdout, "all checks passed")
	return nil
}

// serviceCheck: a registered service must be this launcher's unit and
// known to the manager; whether it runs right now is a detail (`warden
// stop` is legitimate). No service is a pass with the way to get one.
func (c *cli) serviceCheck(cfg config.Config, configPath string) check {
	svc, reason := c.service(cfg.Paths.State)
	if svc == nil {
		return pass("service", "none on this host ("+reason+"); `warden start --detach` runs Warden in the background")
	}
	if !svc.registered() {
		return pass("service", "not registered; `warden service install` registers a "+svc.kind())
	}
	exe, err := os.Executable()
	if err != nil {
		return fail("service", err.Error(), "run `warden service install`")
	}
	unit, _ := os.ReadFile(svc.unitPath())
	if string(unit) != svc.unit(exe, configPath) {
		return fail("service", svc.unitPath()+" does not run this launcher ("+exe+") with this config", "run `warden service install` from the launcher the service should run")
	}
	st := svc.status()
	if !st.Loaded {
		return fail("service", svc.kind()+" "+svc.label()+" is registered but not loaded", "run `warden service install`")
	}
	return pass("service", svc.kind()+" "+svc.label()+": "+st.String())
}

// menuCheck (macOS): a registered menu bar item must be this launcher's
// unit and loaded; not registered, or a build without the item, is a
// pass with the way to get one. Nothing where the item does not apply.
func (c *cli) menuCheck(cfg config.Config, configPath string) (check, bool) {
	m, reason := c.menu(cfg.Paths.State)
	if m == nil {
		if reason == "" {
			return check{}, false
		}
		return pass("menu bar", "none ("+reason+")"), true
	}
	exe, err := os.Executable()
	if err != nil {
		return fail("menu bar", err.Error(), "run `warden menu install`"), true
	}
	if !m.registered() {
		if menuExecutable(exe) == "" {
			return pass("menu bar", "not in this build (no warden-menu beside "+exe+")"), true
		}
		return pass("menu bar", "not registered; `warden menu install` registers it"), true
	}
	unit, _ := os.ReadFile(m.unitPath())
	if string(unit) != m.unit(exe, configPath) {
		return fail("menu bar", m.unitPath()+" does not run this launcher's item ("+exe+") with this config", "run `warden menu install` from the launcher the item should run"), true
	}
	st := m.status()
	if !st.Loaded {
		return fail("menu bar", m.label()+" is registered but not loaded", "run `warden menu install`"), true
	}
	return pass("menu bar", m.label()+": "+st.String()), true
}

// doctorChecks is the ordered check list for one configuration.
func doctorChecks(cfg config.Config, configPath string) []check {
	var out []check
	state := cfg.Paths.State
	if info, err := os.Stat(configPath); err != nil {
		out = append(out, fail("config", configPath+" is missing (checking the defaults for "+state+")", "run `warden install`"))
	} else if info.Mode().Perm()&0o077 != 0 {
		out = append(out, fail("config", fmt.Sprintf("%s is mode %04o", configPath, info.Mode().Perm()), "chmod 600 "+configPath))
	} else {
		out = append(out, pass("config", configPath))
	}
	out = append(out, dirCheck("state directory", state))
	for _, sub := range stateSubdirs {
		out = append(out, dirCheck("state/"+sub, filepath.Join(state, sub)))
	}
	arch, err := guestArch()
	if err != nil {
		out = append(out, fail("host architecture", err.Error(), "use a supported host"))
	} else {
		out = append(out, pass("host architecture", arch+" guests"))
	}
	executable := cfg.SBX.Executable
	if executable == "" {
		if executable, err = findSBX(""); err != nil {
			out = append(out, fail("sbx executable", err.Error(), "install sbx (Docker Sandboxes) and run `warden install`"))
		}
	}
	if executable != "" {
		if err = executableFile(executable); err != nil {
			out = append(out, fail("sbx executable", executable+": "+err.Error(), "install sbx there or run `warden install --sbx PATH`"))
		} else {
			out = append(out, pass("sbx executable", executable))
		}
	}
	privateHome := cfg.SBX.PrivateHome
	for _, d := range namespaceDirs {
		out = append(out, dirCheck("sbx namespace "+d.env, filepath.Join(privateHome, d.dir)))
	}
	if link, target, err := keychainLink(privateHome); err == nil && link != "" {
		if have, err := os.Readlink(link); err != nil || have != target {
			out = append(out, fail("sbx keychain", link+" does not link to "+target, "run `warden install`; on macOS sbx keeps its Docker session in the login keychain, which the namespace HOME must be able to find"))
		} else {
			out = append(out, pass("sbx keychain", link+" -> "+target))
		}
	}
	wrapper := wrapperPath(state)
	wrapperOK := false
	if have, err := os.ReadFile(wrapper); err != nil {
		out = append(out, fail("sbx wrapper", wrapper+" is missing", "run `warden install`"))
	} else if executable != "" && string(have) != wrapperScript(privateHome, executable) {
		out = append(out, fail("sbx wrapper", wrapper+" does not select "+privateHome+" with "+executable, "run `warden install` to rewrite it"))
	} else if err = executableFile(wrapper); err != nil {
		out = append(out, fail("sbx wrapper", wrapper+": "+err.Error(), "chmod 700 "+wrapper))
	} else {
		wrapperOK = true
		out = append(out, pass("sbx wrapper", wrapper))
	}
	if wrapperOK {
		s := &sbxCLI{wrapper: wrapper, stdout: io.Discard, stderr: io.Discard}
		out = append(out, hostChecks(s)...)
		out = append(out, guestImageCheck(s, cfg)...)
	}
	if arch != "" {
		out = append(out, runtimeChecks(cfg.Runtimes.Codex, cfg.Runtimes.Claude, arch)...)
	}
	out = append(out, loginChecks(cfg)...)
	return out
}

func dirCheck(name, path string) check {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return fail(name, path+" is missing", "run `warden install`")
	case !info.IsDir():
		return fail(name, path+" is not a directory", "move it aside and run `warden install`")
	case info.Mode().Perm()&0o077 != 0:
		return fail(name, fmt.Sprintf("%s is mode %04o, not owner-only", path, info.Mode().Perm()), "chmod 700 "+path)
	}
	return pass(name, path)
}

// guestImageCheck confirms a pinned guest image is loaded in Warden's
// namespace; the stock template needs no check because sbx pulls it.
func guestImageCheck(s *sbxCLI, cfg config.Config) []check {
	if cfg.SBX.GuestImageDigest == "" || cfg.SBX.GuestImageDigest == release.StockTemplateDigest {
		return []check{pass("guest image", "stock template "+release.StockTemplate+"; the runner copies the runtimes into each new sandbox")}
	}
	out, err := s.run([]string{"template", "ls", "--json"}, false)
	if err != nil {
		return []check{fail("guest image", err.Error(), "make sure the daemon in Warden's namespace is running")}
	}
	if !templateListed(out, cfg.SBX.GuestImageDigest) {
		return []check{fail("guest image", cfg.SBX.GuestImage+"@"+cfg.SBX.GuestImageDigest+" is not loaded", "run `warden install --guest-image-tar FILE`, `warden install` with docker available, or scripts/build-guest-image-in-sbx.sh")}
	}
	return []check{pass("guest image", cfg.SBX.GuestImage+"@"+cfg.SBX.GuestImageDigest)}
}

// loginChecks report each provider file. A missing login is information,
// not a failure: Warden runs without it and that provider answers "Refresh
// the … sign-in". A login readable by others is a failure.
func loginChecks(cfg config.Config) []check {
	var out []check
	files := []struct{ name, path, command string }{}
	if cfg.Providers.Codex != nil {
		files = append(files, struct{ name, path, command string }{"codex login", cfg.Providers.Codex.AuthFile, "warden login codex"})
	}
	if cfg.Providers.Claude != nil {
		files = append(files, struct{ name, path, command string }{"claude login", cfg.Providers.Claude.AuthFile, "warden login claude"})
	}
	if cfg.Providers.GitHub != nil && cfg.Providers.GitHub.AuthFile != "" {
		files = append(files, struct{ name, path, command string }{"github login", cfg.Providers.GitHub.AuthFile, "warden login github"})
	}
	for _, f := range files {
		info, err := os.Stat(f.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out = append(out, check{Name: f.name, OK: true, Detail: "not signed in (" + f.path + " absent); run `" + f.command + "` when you want this provider"})
		case err != nil:
			out = append(out, fail(f.name, err.Error(), "run `"+f.command+"`"))
		case info.Mode().Perm() != 0o600:
			out = append(out, fail(f.name, fmt.Sprintf("%s is mode %04o", f.path, info.Mode().Perm()), "chmod 600 "+f.path))
		default:
			out = append(out, pass(f.name, f.path))
		}
	}
	return out
}

// printChecks writes one line per check: PASS name: detail, or FAIL name:
// detail, then the remediation indented on the next line.
func printChecks(w io.Writer, checks []check) {
	for _, ch := range checks {
		if ch.OK {
			fmt.Fprintf(w, "PASS %s: %s\n", ch.Name, ch.Detail)
			continue
		}
		fmt.Fprintf(w, "FAIL %s: %s\n     fix: %s\n", ch.Name, ch.Detail, ch.Fix)
	}
}

func failed(checks []check) bool {
	for _, ch := range checks {
		if !ch.OK {
			return true
		}
	}
	return false
}

// templateListed reports whether `sbx template ls --json` names the image
// with this digest. The listing carries the 12-hex-character image id, not
// the full digest, so both spellings count.
func templateListed(listing, digest string) bool {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) < 12 {
		return false
	}
	return strings.Contains(listing, `"id": "`+hex[:12]+`"`) || strings.Contains(listing, `"id":"`+hex[:12]+`"`) || strings.Contains(listing, hex)
}
