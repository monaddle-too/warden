package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"warden/chat/internal/config"
)

// `warden release` is the Go version of scripts/deploy-local.sh
// (docs/host-dogfood-plan.md, Parts A and C): releases are unpacked once
// into the shared store (store.go) and an instance runs from the one
// <state>/release links to (the service unit and the shell alias name
// <state>/release/bin/warden, so switching is repointing the link).
// install unpacks a tarball into the store (or downloads a GitHub release
// by its tag), repoints the link, runs the new release's own `warden
// install --upgrade` into the instance and, with --restart, restarts
// whatever runs; use switches to an installed version; build builds a
// checkout with scripts/release.sh and installs the result (decision 5: a
// non-release build, versioned v0.0.0-dev.<sha>).

const releaseUsage = `usage: warden release COMMAND [--instance NAME | --state DIR] [flags]

  list                             the releases in the store (and the instance's own older ones),
                                   with the instances pinned to and running each; * marks the instance's
  install TARBALL|DIR|TAG [--restart] [--force]
                                   unpack warden-<version>-<os>-<arch>.tar.gz (or take an unpacked
                                   release directory, or download the GitHub release TAG for this host,
                                   checked against its SHA256SUMS) into the store, point <state>/release
                                   at it, run its own warden install --upgrade into the instance;
                                   a release already in the store is reused unless --force;
                                   --restart restarts the running Warden (service or detached)
  use VERSION [--restart]          point <state>/release at an installed release again (a tag, a dev
                                   version or a sha prefix of one, latest, or a release directory name)
  build [CHECKOUT] [--restart] [--test] [--force]
                                   build CHECKOUT (default: .) with scripts/release.sh --skip-tests
                                   (--test runs the tests first) and install the tarball for this host;
                                   --instance or --state is required (never the default by accident)
`

// releaseName is what scripts/release.sh names a tarball and its top
// directory: warden-<version>-<os>-<arch>.
var releaseName = regexp.MustCompile(`^warden-(v.+)-([a-z0-9]+)-([a-z0-9]+)$`)

// parseReleaseName splits a release directory's basename.
func parseReleaseName(base string) (version, goos, goarch string, ok bool) {
	m := releaseName.FindStringSubmatch(strings.TrimSuffix(base, ".tar.gz"))
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// releasesDir is an instance's legacy releases directory (the store, for
// the default instance).
func releasesDir(state string) string { return filepath.Join(state, "releases") }
func releaseLink(state string) string { return filepath.Join(state, "release") }

// currentRelease is where <state>/release points, "" without a link.
func currentRelease(state string) string {
	target, err := os.Readlink(releaseLink(state))
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(state, target)
	}
	return filepath.Clean(target)
}

func (c *cli) releaseCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, releaseUsage)
		return errUsage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("warden release "+sub, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, releaseUsage) }
	state := addStateFlags(fs)
	restart := fs.Bool("restart", false, "restart the running Warden of the instance afterwards (the service, or stop + start --detach)")
	test := fs.Bool("test", false, "build: run the full test suite first (release.sh without --skip-tests)")
	force := fs.Bool("force", false, "unpack again over a release already in the store")
	switch sub {
	case "list", "install", "use", "build":
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, releaseUsage)
		return nil
	default:
		fmt.Fprintf(c.stderr, "warden release: unknown command %q\n%s", sub, releaseUsage)
		return errUsage
	}
	if err := fs.Parse(interleaved(rest, map[string]bool{"state": true, "instance": true})); err != nil {
		return errUsage
	}
	if sub == "build" && state.state == "" && state.instance == "" {
		return errors.New("warden release build needs --instance NAME or --state DIR: it will not deploy over the default instance by accident (say --instance default)")
	}
	cfg, _, err := loadConfigFlags("", state)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		if fs.NArg() != 0 {
			fmt.Fprint(c.stderr, releaseUsage)
			return errUsage
		}
		return c.releaseList(cfg.Paths.State)
	case "install":
		if fs.NArg() != 1 {
			fmt.Fprint(c.stderr, releaseUsage)
			return errUsage
		}
		return c.releaseInstall(cfg, fs.Arg(0), *restart, *force)
	case "use":
		if fs.NArg() != 1 {
			fmt.Fprint(c.stderr, releaseUsage)
			return errUsage
		}
		return c.releaseUse(cfg, fs.Arg(0), *restart)
	default:
		checkout := "."
		if fs.NArg() == 1 {
			checkout = fs.Arg(0)
		} else if fs.NArg() > 1 {
			fmt.Fprint(c.stderr, releaseUsage)
			return errUsage
		}
		return c.releaseBuild(cfg, checkout, *restart, *test, *force)
	}
}

// releaseUsers says, per release path, which instances are pinned to it
// (their release link) and which run it (running.json alive).
type releaseUsers struct{ pinned, running map[string][]string }

func (c *cli) releaseUsersOf(infos []instanceInfo) releaseUsers {
	u := releaseUsers{pinned: map[string][]string{}, running: map[string][]string{}}
	for _, i := range infos {
		if p := currentRelease(i.State); p != "" {
			u.pinned[p] = append(u.pinned[p], i.Name)
		}
		if i.RunningBinary != "" {
			if dir, _ := releaseDirOf(i.RunningBinary); dir != "" {
				u.running[filepath.Clean(dir)] = append(u.running[filepath.Clean(dir)], i.Name)
			}
		}
	}
	return u
}

func (c *cli) releaseList(state string) error {
	releases, err := availableReleases(state)
	if err != nil {
		return err
	}
	store, _ := storeDir()
	if len(releases) == 0 {
		fmt.Fprintf(c.stdout, "no releases under %s (`warden release install TARBALL|TAG` unpacks one)\n", store)
		return nil
	}
	infos, err := c.instances()
	if err != nil {
		return err
	}
	users := c.releaseUsersOf(infos)
	current := currentRelease(state)
	for _, r := range releases {
		mark := " "
		if filepath.Clean(r.Path) == current {
			mark = "*"
		}
		line := fmt.Sprintf("%s %-40s %s  %s", mark, r.Version, r.At.Local().Format("2006-01-02 15:04"), r.Path)
		var notes []string
		if r.Legacy != "" {
			notes = append(notes, "older copy of "+r.Legacy)
		}
		if p := users.pinned[filepath.Clean(r.Path)]; len(p) > 0 {
			notes = append(notes, "pinned by "+strings.Join(p, ", "))
		}
		if p := users.running[filepath.Clean(r.Path)]; len(p) > 0 {
			notes = append(notes, "running on "+strings.Join(p, ", "))
		}
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintln(c.stdout, line)
	}
	if current == "" {
		fmt.Fprintf(c.stdout, "(%s does not link to any of them)\n", releaseLink(state))
	}
	return nil
}

// releaseInstall unpacks (adopts, or downloads) source into the store,
// points the link at it, runs the release's own install into the
// instance and restarts when asked.
func (c *cli) releaseInstall(cfg config.Config, source string, restart, force bool) error {
	state := cfg.Paths.State
	if _, err := os.Stat(filepath.Join(state, installFile)); err != nil {
		return fmt.Errorf("%s is not an installed instance: %w", state, err)
	}
	path, err := c.placeRelease(source, force)
	if err != nil {
		return err
	}
	return c.linkAndInstall(cfg, path, restart)
}

// linkAndInstall points the instance at the release at path, runs its
// install into the instance and restarts when asked.
func (c *cli) linkAndInstall(cfg config.Config, path string, restart bool) error {
	state := cfg.Paths.State
	if err := relink(state, path); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "release:         %s -> %s\n", releaseLink(state), path)
	if err := c.installWith(cfg, filepath.Join(path, "bin", "warden")); err != nil {
		return err
	}
	if restart {
		return c.restartWith(cfg, filepath.Join(path, "bin", "warden"))
	}
	if running, how := c.runningShape(cfg); running {
		fmt.Fprintf(c.stdout, "The %s still runs the previous release; `warden restart --state %s` (or --restart here) switches it.\n", how, state)
	}
	return nil
}

// placeRelease puts source in the store and returns its path: a tarball
// is unpacked, a directory outside the store is copied in, one inside is
// used as is, a name that is no file but a release tag is downloaded from
// GitHub. A release already in the store is reused unless force. The name
// must be a release for this host.
func (c *cli) placeRelease(source string, force bool) (string, error) {
	store, err := storeDir()
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(source); statErr != nil && looksLikeTag(source) {
		return c.placeRemoteRelease(store, source, force)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	base := filepath.Base(source)
	version, goos, goarch, ok := parseReleaseName(base)
	if !ok {
		return "", fmt.Errorf("%s is not a warden-<version>-<os>-<arch> release tarball or directory", base)
	}
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		return "", fmt.Errorf("%s is for %s/%s; this host is %s/%s", base, goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	if err := ensurePrivateDir(store); err != nil {
		return "", err
	}
	dest := filepath.Join(store, strings.TrimSuffix(base, ".tar.gz"))
	if info.IsDir() && within(source, store) {
		if err := executableFile(filepath.Join(source, "bin", "warden")); err != nil {
			return "", fmt.Errorf("%s: %w", source, err)
		}
		return source, nil
	}
	if !force {
		if err := executableFile(filepath.Join(dest, "bin", "warden")); err == nil {
			fmt.Fprintf(c.stdout, "release:         %s is already in the store (--force unpacks it again)\n", filepath.Base(dest))
			return dest, nil
		}
	}
	if info.IsDir() {
		if err := executableFile(filepath.Join(source, "bin", "warden")); err != nil {
			return "", fmt.Errorf("%s: %w", source, err)
		}
		fresh := dest + ".new"
		os.RemoveAll(fresh)
		if _, err := copyTree(source, fresh); err != nil {
			os.RemoveAll(fresh)
			return "", err
		}
		if err := replaceDir(fresh, dest); err != nil {
			return "", err
		}
		fmt.Fprintf(c.stdout, "release:         copied %s to %s\n", source, dest)
		return dest, nil
	}
	if err := unpackRelease(source, dest); err != nil {
		return "", err
	}
	fmt.Fprintf(c.stdout, "release:         unpacked %s (%s) to %s\n", base, version, dest)
	return dest, nil
}

// looksLikeTag says whether a source that is no file could be a GitHub
// release tag: no path separator, not a tarball name.
func looksLikeTag(source string) bool {
	return !strings.ContainsRune(source, '/') && !strings.HasSuffix(source, ".tar.gz") && source != "" && source != "." && source != ".."
}

// unpackRelease extracts a tarball to dest (its top directory must be
// dest's basename), replacing what is there.
func unpackRelease(tarball, dest string) error {
	tmp := dest + ".unpack"
	os.RemoveAll(tmp)
	if err := extractTarGz(tarball, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	unpacked := filepath.Join(tmp, filepath.Base(dest))
	if err := executableFile(filepath.Join(unpacked, "bin", "warden")); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("%s does not unpack to %s/bin/warden: %w", filepath.Base(tarball), filepath.Base(dest), err)
	}
	if err := replaceDir(unpacked, dest); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(tmp)
	return nil
}

// replaceDir moves fresh to dest, putting an existing dest aside first
// (a running release's files stay open to their processes) and removing
// it afterwards.
func replaceDir(fresh, dest string) error {
	old := dest + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(fresh, dest); err != nil {
		os.Rename(old, dest)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// relink points <state>/release at path, atomically.
func relink(state, path string) error {
	link := releaseLink(state)
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(path, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// installWith runs a release's own `warden install --upgrade` into the
// instance: its pins may differ from this launcher's, and it registers
// nothing (the service is restarted separately, by --restart or the
// person). The instance's sbx and its bug-reporting answer are reused.
func (c *cli) installWith(cfg config.Config, bin string) error {
	args := []string{"install", "--state", cfg.Paths.State, "--upgrade", "--service=false", "--menu=false"}
	if cfg.SBX.Executable != "" {
		args = append(args, "--sbx", cfg.SBX.Executable)
	}
	return c.runRelease(bin, args...)
}

// runRelease runs the release's launcher with the person's terminal.
func (c *cli) runRelease(bin string, args ...string) error {
	fmt.Fprintf(c.stdout, "$ %s %s\n", bin, strings.Join(args, " "))
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", bin, args[0], err)
	}
	return nil
}

// runningShape says whether the instance runs and how: as its service or
// detached.
func (c *cli) runningShape(cfg config.Config) (running bool, how string) {
	if svc := c.registeredService(cfg); svc != nil && svc.status().Running {
		return true, svc.kind()
	}
	if _, alive := runningPID(cfg); alive {
		return true, "detached Warden"
	}
	return false, ""
}

// restartWith restarts the instance the way scripts/deploy-local.sh did,
// through the release's own launcher: the service is restarted, a
// detached Warden stopped and started again, nothing running is said so.
func (c *cli) restartWith(cfg config.Config, bin string) error {
	state := cfg.Paths.State
	if svc := c.registeredService(cfg); svc != nil {
		if svc.status().Running {
			return c.runRelease(bin, "restart", "--state", state)
		}
		fmt.Fprintf(c.stdout, "The %s %s is registered but stopped; `warden start --state %s` starts it on the new release.\n", svc.kind(), svc.label(), state)
		return nil
	}
	if _, alive := runningPID(cfg); alive {
		if err := c.runRelease(bin, "stop", "--state", state); err != nil {
			return err
		}
		return c.runRelease(bin, "start", "--state", state, "--detach")
	}
	fmt.Fprintf(c.stdout, "No Warden is running for %s; `warden start --state %s --detach` starts it on the new release.\n", state, state)
	return nil
}

// releaseUse repoints the link at an installed version and installs it
// into the instance (its pins may differ).
func (c *cli) releaseUse(cfg config.Config, version string, restart bool) error {
	releases, err := availableReleases(cfg.Paths.State)
	if err != nil {
		return err
	}
	r, err := resolveRelease(releases, version)
	if err != nil {
		return err
	}
	return c.linkAndInstall(cfg, r.Path, restart)
}

// releaseBuild builds a checkout as scripts/deploy-local.sh did and
// installs the tarball for this host.
func (c *cli) releaseBuild(cfg config.Config, checkout string, restart, test, force bool) error {
	checkout, err := filepath.Abs(checkout)
	if err != nil {
		return err
	}
	script := filepath.Join(checkout, "scripts", "release.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("%s is not a Warden checkout: %w", checkout, err)
	}
	version, err := buildVersion(checkout)
	if err != nil {
		return err
	}
	args := []string{script}
	if !test {
		args = append(args, "--skip-tests")
	}
	fmt.Fprintf(c.stdout, "building %s (%s) in %s\n", version, strings.Join(args, " "), checkout)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Dir = checkout
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, c.stdout, c.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scripts/release.sh: %w", err)
	}
	tarball := filepath.Join(checkout, "dist", "release", fmt.Sprintf("warden-%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(tarball); err != nil {
		return fmt.Errorf("no tarball for this host: %w", err)
	}
	return c.releaseInstall(cfg, tarball, restart, force)
}

// buildVersion is the version scripts/release.sh gives a checkout: the
// tag HEAD sits on, else v0.0.0-dev.<12-character sha>.
func buildVersion(checkout string) (string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if tag, err := git("describe", "--tags", "--exact-match"); err == nil && tag != "" {
		return tag, nil
	}
	sha, err := git("rev-parse", "--short=12", "HEAD")
	if err != nil || sha == "" {
		return "", fmt.Errorf("%s: not a git checkout (git rev-parse failed: %v)", checkout, err)
	}
	return "v0.0.0-dev." + sha, nil
}
