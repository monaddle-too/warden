package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// The release store (docs/host-dogfood-plan.md, Part C): every release on
// this machine is unpacked once, under <default state>/releases/<name>/
// (~/.warden/releases on macOS), and each instance's <state>/release link
// points into it. Before Part C every instance kept its own copies under
// <state>/releases; those legacy directories stay readable (list shows
// them, start can run them) but nothing new is written there. For the
// default instance the legacy directory and the store are one directory.

// storeDir is the shared release store.
func storeDir() (string, error) {
	def, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(def, "releases"), nil
}

// storeRelease is one unpacked release: in the store, or in an instance's
// legacy directory (Legacy names that instance).
type storeRelease struct {
	Name    string    `json:"name"` // the directory name, warden-<version>-<os>-<arch>
	Version string    `json:"version"`
	Path    string    `json:"path"`
	At      time.Time `json:"installedAt"`
	Legacy  string    `json:"legacy,omitempty"`
}

// bin is the release's launcher.
func (r storeRelease) bin() string { return filepath.Join(r.Path, "bin", "warden") }

// releaseDirOf is the release directory a launcher binary belongs to and
// its parsed name, "" when the binary is not laid out as a release
// (a checkout's dist/chat/warden).
func releaseDirOf(bin string) (dir, name string) {
	dir = filepath.Dir(filepath.Dir(bin))
	if _, _, _, ok := parseReleaseName(filepath.Base(dir)); !ok {
		return "", ""
	}
	return dir, filepath.Base(dir)
}

// listReleaseDir lists the releases for this host under dir, newest first.
func listReleaseDir(dir, legacy string) ([]storeRelease, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []storeRelease
	for _, e := range entries {
		version, goos, goarch, ok := parseReleaseName(e.Name())
		if !ok || !e.IsDir() || goos != runtime.GOOS || goarch != runtime.GOARCH {
			continue
		}
		r := storeRelease{Name: e.Name(), Version: version, Path: filepath.Join(dir, e.Name()), Legacy: legacy}
		if info, err := e.Info(); err == nil {
			r.At = info.ModTime()
		}
		out = append(out, r)
	}
	sortReleases(out)
	return out, nil
}

func sortReleases(rs []storeRelease) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].At.After(rs[j].At) })
}

// storeReleases lists the store.
func storeReleases() ([]storeRelease, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	return listReleaseDir(dir, "")
}

// availableReleases lists the store and, when state has a legacy releases
// directory of its own, that too; the store first, newest first within
// each.
func availableReleases(state string) ([]storeRelease, error) {
	out, err := storeReleases()
	if err != nil {
		return nil, err
	}
	store, _ := storeDir()
	if state != "" && filepath.Clean(releasesDir(state)) != filepath.Clean(store) {
		legacy, err := listReleaseDir(releasesDir(state), instanceName(state))
		if err != nil {
			return nil, err
		}
		out = append(out, legacy...)
	}
	return out, nil
}

// allReleases lists the store and every instance's legacy directory.
func allReleases(states []string) ([]storeRelease, error) {
	out, err := storeReleases()
	if err != nil {
		return nil, err
	}
	store, _ := storeDir()
	for _, state := range states {
		if filepath.Clean(releasesDir(state)) == filepath.Clean(store) {
			continue
		}
		legacy, err := listReleaseDir(releasesDir(state), instanceName(state))
		if err != nil {
			return nil, err
		}
		out = append(out, legacy...)
	}
	return out, nil
}

// devVersion is a build's version, v0.0.0-dev.<sha>; shaPrefix a bare
// hexadecimal commit prefix (at least four characters, so a tag's digits
// are not mistaken for one).
var (
	devVersion = regexp.MustCompile(`^v0\.0\.0-dev\.([0-9a-f]+)$`)
	shaPrefix  = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
)

// resolveRelease finds the release spec names among rs: a full directory
// name, a version (with or without its v), a dev version or a bare sha
// prefix of one, or latest (the newest). A prefix matching several dev
// releases is refused with the candidates.
func resolveRelease(rs []storeRelease, spec string) (storeRelease, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return storeRelease{}, errors.New("no version given (`warden versions` lists them)")
	}
	if len(rs) == 0 {
		return storeRelease{}, errors.New("no releases installed (`warden release install TARBALL|TAG` or `warden release build` adds one; `warden versions --remote` lists what can be installed)")
	}
	if spec == "latest" {
		newest := rs[0]
		for _, r := range rs[1:] {
			if r.At.After(newest.At) {
				newest = r
			}
		}
		return newest, nil
	}
	for _, r := range rs {
		if r.Name == spec || r.Version == spec || r.Version == "v"+spec {
			return r, nil
		}
	}
	prefix := spec
	if m := devVersion.FindStringSubmatch(spec); m != nil {
		prefix = m[1]
	}
	if shaPrefix.MatchString(prefix) {
		var found []storeRelease
		for _, r := range rs {
			if m := devVersion.FindStringSubmatch(r.Version); m != nil && strings.HasPrefix(m[1], prefix) {
				if len(found) == 0 || found[len(found)-1].Path != r.Path {
					found = append(found, r)
				}
			}
		}
		// One build unpacked in several places is one version.
		if len(found) > 0 {
			same := true
			for _, r := range found[1:] {
				if r.Version != found[0].Version {
					same = false
				}
			}
			if same {
				return found[0], nil
			}
			var names []string
			for _, r := range found {
				names = append(names, r.Version)
			}
			return storeRelease{}, fmt.Errorf("%s matches several dev releases: %s", spec, strings.Join(names, ", "))
		}
	}
	return storeRelease{}, fmt.Errorf("no release %s installed (`warden versions` lists them; a GitHub release tag installs with `warden release install %s`)", spec, spec)
}

// releaseVersionOf is the version a release directory (or a path into
// one) carries, "" when it is not a release layout.
func releaseVersionOf(path string) string {
	for p := filepath.Clean(path); p != "/" && p != "."; p = filepath.Dir(p) {
		if version, _, _, ok := parseReleaseName(filepath.Base(p)); ok {
			return version
		}
	}
	return ""
}
