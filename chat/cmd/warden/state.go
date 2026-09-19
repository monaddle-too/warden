package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// stateLayout is the on-disk layout version of the state root. A directory
// written by another layout is refused rather than migrated.
const stateLayout = 1

// installFile records which Warden release owns a state directory.
const installFile = "install.json"

// stateSubdirs are created owner-only under the state root. policy, runner,
// app and edge are the four services' private state; provider holds the
// 0600 login files; sbx is the private SBX namespace; runtimes holds the
// pinned agent programs; bin holds the SBX wrapper.
var stateSubdirs = []string{"policy", "runner", "app", "edge", "provider", "sbx", "runtimes", "bin"}

// defaultStateDir is ~/.warden on macOS and $XDG_DATA_HOME/warden (default
// ~/.local/share/warden) elsewhere. macOS deliberately does not use
// ~/Library/Application Support: sbx binds Unix sockets under the
// namespace's $HOME (…/.sbx/run/d/containerd/containerd.sock.ttrpc) and that
// prefix pushes them past the 104-byte sun_path limit.
func defaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, ".warden"), nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "warden"), nil
	}
	return filepath.Join(home, ".local", "share", "warden"), nil
}

// defaultConfigPath is where install writes and the other commands read
// warden.json when --config is not given: $WARDEN_CONFIG, else
// <state>/warden.json.
func defaultConfigPath(state string) string {
	if env := os.Getenv(config.Env); env != "" {
		return env
	}
	return filepath.Join(state, "warden.json")
}

// An instance is a state directory with a name (docs/host-dogfood-plan.md,
// Part A): the default instance is the default state directory, a named
// one is its sibling ~/.warden-<name> (the layout serviceNames and the
// clone script already assumed). Every command that takes --state also
// takes --instance NAME, and $WARDEN_INSTANCE is the default for it.

// instanceEnv names the instance a command uses when neither --state nor
// --instance is given.
const instanceEnv = "WARDEN_INSTANCE"

// defaultInstance is the default instance's name.
const defaultInstance = config.DefaultInstance

// instanceDir is the state directory of the named instance: the default
// one for "" or "default", else <dirname of the default>/.warden-<name>.
func instanceDir(name string) (string, error) {
	def, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	if name == "" || name == defaultInstance {
		return def, nil
	}
	if !config.ValidInstanceName(name) {
		return "", fmt.Errorf("instance name %q: letters, digits and dashes only (and not \"spare\")", name)
	}
	return filepath.Join(filepath.Dir(def), ".warden-"+name), nil
}

// instanceName is the inverse of instanceDir: "default" for the default
// state directory, the name of a ~/.warden-<name> sibling, else the
// directory's basename.
func instanceName(state string) string {
	if def, err := defaultStateDir(); err == nil && filepath.Clean(def) == filepath.Clean(state) {
		return defaultInstance
	}
	return config.InstanceName(state)
}

// stateFlags are the two ways a command names its state directory:
// --state DIR (any directory) or --instance NAME; one or the other.
type stateFlags struct {
	state    string
	instance string
}

// addStateFlags registers --state and --instance on fs.
func addStateFlags(fs *flag.FlagSet) *stateFlags {
	s := &stateFlags{}
	fs.StringVar(&s.state, "state", "", "state directory when no warden.json exists yet (default: the instance's)")
	fs.StringVar(&s.instance, "instance", "", "the Warden instance to use: default, or NAME for ~/.warden-NAME (default: $"+instanceEnv+", else default)")
	return s
}

// dir is the state directory the flags select, "" for the default state
// directory (so callers keep their "" = default handling): --state as
// given, else the instance named by --instance or $WARDEN_INSTANCE.
func (s *stateFlags) dir() (string, error) {
	if s.state != "" && s.instance != "" {
		return "", errors.New("--state and --instance exclude each other")
	}
	if s.state != "" {
		return s.state, nil
	}
	name := s.instance
	if name == "" {
		name = strings.TrimSpace(os.Getenv(instanceEnv))
	}
	if name == "" || name == defaultInstance {
		return "", nil
	}
	return instanceDir(name)
}

// resolveState returns the absolute state root from --state, or the default.
func resolveState(flagValue string) (string, error) {
	state := flagValue
	if state == "" {
		var err error
		if state, err = defaultStateDir(); err != nil {
			return "", err
		}
	}
	state, err := filepath.Abs(state)
	if err != nil {
		return "", err
	}
	return state, nil
}

// ensurePrivateDir creates path (and parents) and forces owner-only mode on
// the leaf, which MkdirAll alone does not do when the directory exists.
func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// installRecord is the content of <state>/install.json.
type installRecord struct {
	Layout int    `json:"layout"`
	Warden string `json:"warden"`
	// Name is the instance this state directory is (instanceName); Dev
	// marks a development instance, whose pins are expected to move: a
	// build with other pins is installed into it without --upgrade.
	Name        string `json:"name,omitempty"`
	Dev         bool   `json:"dev,omitempty"`
	SBX         string `json:"sbx,omitempty"` // no longer pinned; kept so older records still parse
	Codex       string `json:"codex"`
	Claude      string `json:"claude"`
	GuestArch   string `json:"guestArch"`
	InstalledAt string `json:"installedAt"`
}

func currentRecord(arch string) installRecord {
	return installRecord{Layout: stateLayout, Warden: revision, Codex: release.CodexVersion, Claude: release.ClaudeVersion, GuestArch: arch, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
}

// samePins reports whether two records belong to the same Warden release.
func (r installRecord) samePins(o installRecord) bool {
	return r.Layout == o.Layout && r.Codex == o.Codex && r.Claude == o.Claude && r.GuestArch == o.GuestArch
}

func readRecord(state string) (installRecord, bool, error) {
	raw, err := os.ReadFile(filepath.Join(state, installFile))
	if errors.Is(err, os.ErrNotExist) {
		return installRecord{}, false, nil
	}
	if err != nil {
		return installRecord{}, false, err
	}
	var r installRecord
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, true, fmt.Errorf("%s: %w", filepath.Join(state, installFile), err)
	}
	return r, true, nil
}

func writeRecord(state string, r installRecord) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(state, installFile), append(b, '\n'), 0o600)
}

// checkRecord refuses a state directory that a different Warden release
// installed unless upgrade is set or the record marks a dev instance. A
// directory without a record is new.
func checkRecord(state string, want installRecord, upgrade bool) error {
	have, found, err := readRecord(state)
	if err != nil {
		return err
	}
	if !found || have.samePins(want) {
		return nil
	}
	if upgrade || have.Dev {
		return nil
	}
	return fmt.Errorf("%s was installed by Warden %s (layout %d, Codex %s, Claude %s, guest %s); this warden is %s (layout %d, Codex %s, Claude %s, guest %s). Re-run with --upgrade to move the state directory to this release, or use --state for a separate one",
		state, have.Warden, have.Layout, have.Codex, have.Claude, have.GuestArch, want.Warden, want.Layout, want.Codex, want.Claude, want.GuestArch)
}

// atomicWrite writes data to path through a same-directory temporary file
// created exclusively with mode, so a reader never sees a partial file.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loadConfigFlags is loadConfig for a command's --config and its state
// flags (--state, --instance, $WARDEN_INSTANCE).
func loadConfigFlags(path string, s *stateFlags) (config.Config, string, error) {
	state, err := s.dir()
	if err != nil {
		return config.Config{}, "", err
	}
	return loadConfig(path, state)
}

// loadConfig reads warden.json for doctor, login, start and open. With
// neither --config nor --state the default state's file is used; when that
// file does not exist the local defaults for the state root apply, so the
// commands work before install has written anything (doctor then reports
// what is missing).
func loadConfig(path, state string) (config.Config, string, error) {
	if path == "" && state == "" {
		var err error
		if state, err = resolveState(""); err != nil {
			return config.Config{}, "", err
		}
	}
	if state != "" {
		var err error
		if state, err = resolveState(state); err != nil {
			return config.Config{}, "", err
		}
	}
	if path == "" {
		path = defaultConfigPath(state)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			c := config.Defaults(state)
			return c, path, c.Validate()
		}
	}
	c, err := config.Load(path, state)
	return c, path, err
}
