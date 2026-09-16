// Package config is the one configuration surface shared by warden-policy,
// warden-runner, warden-chat and warden-edge. The schema is the appendix of
// docs/warden-local-deployments-plan.md. The file is optional in local mode:
// every field has a computed default and only Paths.State is required.
// Secrets never appear here; credentials are paths to owner-only files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Version is the schema version this package reads and writes.
const Version = 1

// Env names the environment variable that locates the file when no --config
// flag is given.
const Env = "WARDEN_CONFIG"

// Preview modes.
const (
	PreviewLoopback = "loopback"
	PreviewPublic   = "public"
)

// Auth modes.
const (
	AuthOwner  = "owner"
	AuthGoogle = "google"
)

// Config is the parsed file. Zero values mean "compute the default".
type Config struct {
	Version   int       `json:"version"`
	Paths     Paths     `json:"paths"`
	SBX       SBX       `json:"sbx"`
	Runtimes  Runtimes  `json:"runtimes"`
	Sandboxes Sandboxes `json:"sandboxes"`
	Chat      Chat      `json:"chat"`
	Previews  Previews  `json:"previews"`
	Auth      Auth      `json:"auth"`
	Providers Providers `json:"providers"`
}

// Paths locates Warden's data and release assets.
type Paths struct {
	// State is the one private directory holding all Warden data. Required.
	State string `json:"state"`
	// WebAssets is the built chat UI served to the browser.
	WebAssets string `json:"webAssets,omitempty"`
	// GitHubCatalog holds github-operations.json and github-meta.json.
	GitHubCatalog string `json:"githubCatalog,omitempty"`
	// SandboxPolicyTemplate is the starting policy for new sandboxes.
	SandboxPolicyTemplate string `json:"sandboxPolicyTemplate,omitempty"`
}

// SBX describes the sandbox runtime on this host.
type SBX struct {
	Executable               string `json:"executable,omitempty"`
	PrivateHome              string `json:"privateHome,omitempty"`
	GuestImage               string `json:"guestImage,omitempty"`
	GuestImageDigest         string `json:"guestImageDigest,omitempty"`
	InspectionCertMaxAgeDays int    `json:"inspectionCertMaxAgeDays,omitempty"`
}

// Runtimes are the pinned agent programs copied into guests.
type Runtimes struct {
	Codex  string `json:"codex,omitempty"`
	Claude string `json:"claude,omitempty"`
}

// Sandboxes sets capacity and lifecycle.
type Sandboxes struct {
	MemoryMB             int `json:"memoryMB,omitempty"`
	MaxRunning           int `json:"maxRunning,omitempty"`
	WarmSpares           int `json:"warmSpares,omitempty"`
	StopAfterIdleMinutes int `json:"stopAfterIdleMinutes,omitempty"`
	KeepStopped          int `json:"keepStopped,omitempty"`
}

// Chat is the web app listener.
type Chat struct {
	Listen string `json:"listen,omitempty"`
}

// Previews says how agent web previews reach the browser.
type Previews struct {
	Mode       string `json:"mode,omitempty"`
	HostSuffix string `json:"hostSuffix,omitempty"`
	EdgeListen string `json:"edgeListen,omitempty"`
}

// Auth says who may open Warden.
type Auth struct {
	Mode      string        `json:"mode,omitempty"`
	PublicURL string        `json:"publicURL,omitempty"`
	Google    *GoogleSignIn `json:"google,omitempty"`
}

// GoogleSignIn configures Google sign-in at the edge (server mode).
type GoogleSignIn struct {
	SignInClientID string   `json:"signInClientID"`
	Owners         []string `json:"owners"`
	DemoDomains    []string `json:"demoDomains,omitempty"`
	SignInLedger   string   `json:"signInLedger,omitempty"`
}

// Providers are the accounts Warden brokers for agents.
type Providers struct {
	Codex  *AuthFile `json:"codex,omitempty"`
	Claude *AuthFile `json:"claude,omitempty"`
	Google *Google   `json:"google,omitempty"`
	GitHub *GitHub   `json:"github,omitempty"`
}

// AuthFile points at one owner-only credential file.
type AuthFile struct {
	AuthFile string `json:"authFile"`
}

// Google selects the Docs OAuth client: "builtin" or a path to an operator
// client file.
type Google struct {
	DocsClient string `json:"docsClient,omitempty"`
}

// GitHub is either a local user token (AuthFile) or a server GitHub App.
type GitHub struct {
	AuthFile          string `json:"authFile,omitempty"`
	AppID             int64  `json:"appID,omitempty"`
	AppSlug           string `json:"appSlug,omitempty"`
	InstallationOwner string `json:"installationOwner,omitempty"`
	BrokerFile        string `json:"brokerFile,omitempty"`
}

// BuiltinGoogleClient names the shared Warden Docs client.
const BuiltinGoogleClient = "builtin"

// Defaults returns the local-mode configuration for a state root with every
// field computed. Callers that detect host facts overwrite fields afterwards.
func Defaults(state string) Config {
	c := Config{Version: Version}
	c.Paths.State = state
	c.SBX.PrivateHome = filepath.Join(state, "sbx")
	c.SBX.InspectionCertMaxAgeDays = 365
	c.Sandboxes = Sandboxes{MemoryMB: 1536, MaxRunning: 2, WarmSpares: 1, StopAfterIdleMinutes: 15, KeepStopped: 32}
	c.Chat.Listen = "127.0.0.1:18780"
	c.Previews = Previews{Mode: PreviewLoopback, HostSuffix: "localhost", EdgeListen: "127.0.0.1:18781"}
	c.Auth = Auth{Mode: AuthOwner, PublicURL: "http://" + c.Previews.EdgeListen}
	provider := filepath.Join(state, "provider")
	c.Providers = Providers{
		Codex:  &AuthFile{AuthFile: filepath.Join(provider, "auth.json")},
		Claude: &AuthFile{AuthFile: filepath.Join(provider, "claude.json")},
		Google: &Google{DocsClient: BuiltinGoogleClient},
		GitHub: &GitHub{AuthFile: filepath.Join(provider, "github.json")},
	}
	return c
}

// SBXSocketPath is the longest Unix socket sbx binds inside its namespace
// HOME (<privateHome>/home); macOS limits socket paths to 104 bytes.
func SBXSocketPath(privateHome string) string {
	return filepath.Join(privateHome, "home", ".sbx", "run", "d", "containerd", "containerd.sock.ttrpc")
}

// Derived paths under the state root. These are never written to the file.
func (c Config) PolicyState() string  { return filepath.Join(c.Paths.State, "policy") }
func (c Config) RunnerState() string  { return filepath.Join(c.Paths.State, "runner") }
func (c Config) AppState() string     { return filepath.Join(c.Paths.State, "app") }
func (c Config) EdgeState() string    { return filepath.Join(c.Paths.State, "edge") }
func (c Config) ProviderDir() string  { return filepath.Join(c.Paths.State, "provider") }
func (c Config) PolicySocket() string { return filepath.Join(c.PolicyState(), "sbx-control.sock") }
func (c Config) RunnerSocket() string { return filepath.Join(c.RunnerState(), "worker.sock") }
func (c Config) OwnerTokenFile() string {
	return filepath.Join(c.AppState(), "endpoint.json")
}

// PreviewScheme is the URL scheme approved bindings receive.
func (c Config) PreviewScheme() string {
	if c.Previews.Mode == PreviewPublic {
		return "https"
	}
	return "http"
}

// GitHubMode reports "user", "app" or "" for the configured GitHub provider.
func (c Config) GitHubMode() string {
	switch {
	case c.Providers.GitHub == nil:
		return ""
	case c.Providers.GitHub.AuthFile != "":
		return "user"
	case c.Providers.GitHub.AppID != 0:
		return "app"
	}
	return ""
}

// Load reads path (or $WARDEN_CONFIG when path is empty), fills defaults and
// validates. An empty path and unset variable return local defaults only when
// state is non-empty; otherwise a file is required.
func Load(path, state string) (Config, error) {
	if path == "" {
		path = os.Getenv(Env)
	}
	var c Config
	if path == "" {
		if state == "" {
			return c, errors.New("a config file or state directory is required")
		}
		c = Defaults(state)
		return c, c.Validate()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	c, err = Parse(raw)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if state != "" && c.Paths.State != "" && c.Paths.State != state {
		return c, fmt.Errorf("%s: paths.state %q disagrees with --state %q", path, c.Paths.State, state)
	}
	if c.Paths.State == "" {
		c.Paths.State = state
	}
	return c, c.Validate()
}

// Parse decodes a file, rejects unknown fields, merges defaults into unset
// fields and validates.
func Parse(raw []byte) (Config, error) {
	var file Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return file, err
	}
	if file.Version != Version {
		return file, fmt.Errorf("unsupported config version %d", file.Version)
	}
	if file.Paths.State == "" {
		return file, errors.New("paths.state is required")
	}
	c := Defaults(file.Paths.State)
	merge(&c, file)
	// A provider set to JSON null is removed: the pointer stays nil in file,
	// so explicit nulls are read separately and hide that integration.
	var nulls struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(raw, &nulls)
	for name, value := range nulls.Providers {
		if strings.TrimSpace(string(value)) != "null" {
			continue
		}
		switch name {
		case "codex":
			c.Providers.Codex = nil
		case "claude":
			c.Providers.Claude = nil
		case "google":
			c.Providers.Google = nil
		case "github":
			c.Providers.GitHub = nil
		}
	}
	return c, c.Validate()
}

// merge copies every set field of file over c.
func merge(c *Config, file Config) {
	setString(&c.Paths.WebAssets, file.Paths.WebAssets)
	setString(&c.Paths.GitHubCatalog, file.Paths.GitHubCatalog)
	setString(&c.Paths.SandboxPolicyTemplate, file.Paths.SandboxPolicyTemplate)
	setString(&c.SBX.Executable, file.SBX.Executable)
	setString(&c.SBX.PrivateHome, file.SBX.PrivateHome)
	setString(&c.SBX.GuestImage, file.SBX.GuestImage)
	setString(&c.SBX.GuestImageDigest, file.SBX.GuestImageDigest)
	setInt(&c.SBX.InspectionCertMaxAgeDays, file.SBX.InspectionCertMaxAgeDays)
	setString(&c.Runtimes.Codex, file.Runtimes.Codex)
	setString(&c.Runtimes.Claude, file.Runtimes.Claude)
	setInt(&c.Sandboxes.MemoryMB, file.Sandboxes.MemoryMB)
	setInt(&c.Sandboxes.MaxRunning, file.Sandboxes.MaxRunning)
	setInt(&c.Sandboxes.WarmSpares, file.Sandboxes.WarmSpares)
	setInt(&c.Sandboxes.StopAfterIdleMinutes, file.Sandboxes.StopAfterIdleMinutes)
	setInt(&c.Sandboxes.KeepStopped, file.Sandboxes.KeepStopped)
	setString(&c.Chat.Listen, file.Chat.Listen)
	setString(&c.Previews.Mode, file.Previews.Mode)
	setString(&c.Previews.HostSuffix, file.Previews.HostSuffix)
	setString(&c.Previews.EdgeListen, file.Previews.EdgeListen)
	setString(&c.Auth.Mode, file.Auth.Mode)
	if file.Auth.Mode == AuthGoogle || file.Previews.Mode == PreviewPublic {
		// A public deployment states its own URL; the loopback default is wrong.
		c.Auth.PublicURL = ""
	}
	setString(&c.Auth.PublicURL, file.Auth.PublicURL)
	if file.Auth.Google != nil {
		g := *file.Auth.Google
		if g.SignInLedger == "" {
			g.SignInLedger = filepath.Join(c.EdgeState(), "logins.json")
		}
		c.Auth.Google = &g
	}
	// Providers: a section present in the file replaces the default section;
	// a JSON null removes it (hides that integration).
	if file.Providers.Codex != nil {
		c.Providers.Codex = file.Providers.Codex
	}
	if file.Providers.Claude != nil {
		c.Providers.Claude = file.Providers.Claude
	}
	if file.Providers.Google != nil {
		c.Providers.Google = file.Providers.Google
	}
	if file.Providers.GitHub != nil {
		c.Providers.GitHub = file.Providers.GitHub
	}
}

func setString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
func setInt(dst *int, v int) {
	if v != 0 {
		*dst = v
	}
}

// Validate applies the mode rules from the plan appendix.
func (c Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Paths.State == "" || !filepath.IsAbs(c.Paths.State) {
		return errors.New("paths.state must be an absolute path")
	}
	if len(c.PolicySocket()) > 100 {
		return errors.New("paths.state is too long for the private Unix sockets")
	}
	if len(SBXSocketPath(c.SBX.PrivateHome)) > 103 {
		return errors.New("sbx.privateHome is too long: sbx binds Unix sockets under it (keep the state directory short, e.g. ~/.warden)")
	}
	if err := loopback(c.Chat.Listen); err != nil {
		return fmt.Errorf("chat.listen: %w", err)
	}
	switch c.Previews.Mode {
	case PreviewLoopback:
		if c.Previews.HostSuffix != "localhost" {
			return errors.New("previews.hostSuffix must be \"localhost\" in loopback mode")
		}
		if err := loopback(c.Previews.EdgeListen); err != nil {
			return fmt.Errorf("previews.edgeListen: %w", err)
		}
	case PreviewPublic:
		if !dottedHost(c.Previews.HostSuffix) {
			return errors.New("previews.hostSuffix must be a dotted hostname in public mode")
		}
		if c.Auth.Mode != AuthGoogle {
			return errors.New("previews.mode \"public\" requires auth.mode \"google\"")
		}
		if _, _, err := net.SplitHostPort(c.Previews.EdgeListen); err != nil {
			return fmt.Errorf("previews.edgeListen: %w", err)
		}
	default:
		return fmt.Errorf("previews.mode must be %q or %q", PreviewLoopback, PreviewPublic)
	}
	switch c.Auth.Mode {
	case AuthOwner:
		if c.Auth.Google != nil {
			return errors.New("auth.google is only used with auth.mode \"google\"")
		}
		if !strings.HasPrefix(c.Auth.PublicURL, "http://") {
			return errors.New("auth.publicURL must be an http:// loopback URL in owner mode")
		}
	case AuthGoogle:
		if c.Auth.Google == nil || c.Auth.Google.SignInClientID == "" || len(c.Auth.Google.Owners) == 0 {
			return errors.New("auth.mode \"google\" requires auth.google.signInClientID and auth.google.owners")
		}
		if !strings.HasPrefix(c.Auth.PublicURL, "https://") {
			return errors.New("auth.publicURL must be an https:// URL in google mode")
		}
	default:
		return fmt.Errorf("auth.mode must be %q or %q", AuthOwner, AuthGoogle)
	}
	if c.SBX.GuestImageDigest != "" && !digestShape(c.SBX.GuestImageDigest) {
		return errors.New("sbx.guestImageDigest must be sha256:<64 hex>")
	}
	s := c.Sandboxes
	if s.MemoryMB < 512 || s.MemoryMB > 16384 || s.MaxRunning < 1 || s.WarmSpares < 0 || s.StopAfterIdleMinutes < 1 || s.KeepStopped < 1 {
		return errors.New("sandboxes: memoryMB 512–16384, maxRunning ≥ 1, warmSpares ≥ 0, stopAfterIdleMinutes ≥ 1, keepStopped ≥ 1")
	}
	if g := c.Providers.GitHub; g != nil {
		user, app := g.AuthFile != "", g.AppID != 0 || g.AppSlug != "" || g.InstallationOwner != "" || g.BrokerFile != ""
		if user == app {
			return errors.New("providers.github must be either a user authFile or a GitHub App (appID, appSlug, installationOwner, brokerFile), not both or neither")
		}
		if app && (g.AppID == 0 || g.AppSlug == "" || g.InstallationOwner == "" || g.BrokerFile == "") {
			return errors.New("providers.github App mode requires appID, appSlug, installationOwner and brokerFile")
		}
	}
	if g := c.Providers.Google; g != nil && g.DocsClient == "" {
		return errors.New("providers.google.docsClient must be \"builtin\" or a file path")
	}
	return nil
}

func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || ip.To4() == nil {
		return errors.New("must be an IPv4 loopback address with port")
	}
	return nil
}

func dottedHost(s string) bool {
	return strings.Contains(s, ".") && !strings.ContainsAny(s, "/:@?#* ") && !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".")
}

func digestShape(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 7+64 {
		return false
	}
	for _, r := range s[7:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// Write stores the configuration owner-only, atomically replacing path.
func Write(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
