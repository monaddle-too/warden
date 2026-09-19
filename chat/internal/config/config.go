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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"warden/chat/internal/transport"
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

// Runtime kinds (runtime.kind): the sandbox runtime a deployment uses.
const (
	RuntimeSBX        = "sbx"
	RuntimeKubernetes = "kubernetes"
)

// Gateway modes, derived from the runtime kind (GatewayMode): per-binding
// loopback listeners on the sbx shapes, one credentialed listener on
// Kubernetes.
const (
	GatewayLoopback = "loopback"
	GatewayShared   = "shared"
)

// Isolation tiers of the kubernetes kind (kubernetes.tier).
const (
	TierKata   = "kata"
	TierGVisor = "gvisor"
)

// Config is the parsed file. Zero values mean "compute the default".
type Config struct {
	Version int   `json:"version"`
	Paths   Paths `json:"paths"`
	// Runtime selects the sandbox runtime; its zero value is the sbx kind,
	// so files written before the field existed read unchanged and a file
	// written for the sbx shapes does not mention it.
	Runtime Runtime `json:"runtime,omitzero"`
	SBX     SBX     `json:"sbx"`
	// Kubernetes is the kubernetes kind's section (docs/warden-kubernetes-plan.md,
	// appendix A); required with that kind and refused with any other.
	Kubernetes *Kubernetes `json:"kubernetes,omitempty"`
	Runtimes   Runtimes    `json:"runtimes"`
	Sandboxes  Sandboxes   `json:"sandboxes"`
	Chat       Chat        `json:"chat"`
	Previews   Previews    `json:"previews"`
	Auth       Auth        `json:"auth"`
	Providers  Providers   `json:"providers"`
	// Services and TLS are the transport between the four services; both
	// are omitted from a written file when they hold nothing, and an
	// existing file without them keeps today's Unix sockets and loopback
	// chat.
	Services Services `json:"services,omitzero"`
	TLS      *TLS     `json:"tls,omitempty"`
	// Edge is what the edge does beyond sign-in and previews (which it
	// derives from auth.* and previews.*): today the bug-report receiver.
	Edge Edge `json:"edge,omitzero"`
	// Reporting is bug reporting (docs/bug-reporting-plan.md): off unless
	// the person opted in at install or with `warden bugs on`; every report
	// is shown to them before it is sent to URL.
	Reporting Reporting `json:"reporting,omitzero"`
	// Dogfood holds the owner's opt-ins for developing Warden from a
	// Warden chat (docs/host-dogfood-plan.md); omitted when nothing is on.
	Dogfood Dogfood `json:"dogfood,omitzero"`
}

// Dogfood is the dogfooding section. Jailbreak lets the owner opt a
// workspace into host access (the agent's host_* tools: commands on this
// machine as the owner, files each way, a host port as a preview); only a
// local owner install may turn it on, never a server or Kubernetes.
type Dogfood struct {
	Jailbreak bool `json:"jailbreak,omitempty"`
}

// JailbreakAllowed reports whether this install may turn host access on:
// a single-owner install on a local runtime (the runner runs on the
// owner's machine, which the Kubernetes shape's pod is not).
func (c Config) JailbreakAllowed() bool {
	return c.Auth.Mode == AuthOwner && c.RuntimeKind() != RuntimeKubernetes
}

// ErrJailbreakRefused is why dogfood.jailbreak is refused elsewhere.
var ErrJailbreakRefused = errors.New("dogfood.jailbreak needs auth.mode owner and a local runtime")

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

// Runtime names the sandbox runtime kind.
type Runtime struct {
	Kind string `json:"kind,omitempty"`
}

// Kubernetes configures the kubernetes runtime kind: where sandboxes run,
// how they are isolated, what they run, and what the policy service
// publishes for them.
type Kubernetes struct {
	// Namespace holds the sandbox pods and their workspace volumes.
	Namespace string `json:"namespace"`
	// Tier is the isolation boundary, kata or gvisor; the verifier checks
	// the RuntimeClass handler against the tier's allow-list.
	Tier string `json:"tier"`
	// RuntimeClass is the runtimeClassName every sandbox pod is created
	// with and the verifier pins.
	RuntimeClass string `json:"runtimeClass"`
	// GuestImage and GuestImageDigest are the guest base image and the
	// digest the pod's imageID must report; they replace sbx.guestImage*
	// for this kind.
	GuestImage       string `json:"guestImage"`
	GuestImageDigest string `json:"guestImageDigest"`
	// StorageClass is the workspace volumes' class; empty means the
	// cluster default.
	StorageClass string `json:"storageClass,omitempty"`
	// WorkspaceSizeGi sizes each sandbox's workspace volume; 20 when unset.
	WorkspaceSizeGi int `json:"workspaceSizeGi,omitempty"`
	// GatewayService is the Service the shared gateway is advertised
	// through; warden-gateway when unset. GatewayPort is the port the
	// shared gateway listens on and the Service exposes; 7000 when unset.
	GatewayService string `json:"gatewayService,omitempty"`
	GatewayPort    int    `json:"gatewayPort,omitempty"`
	// TrustConfigMap is the guest trust bundle the policy service publishes
	// and the runner mounts; warden-guest-trust when unset.
	TrustConfigMap string `json:"trustConfigMap,omitempty"`
	// GatewayCAMaxAgeDays bounds the gateway CA's age before the policy
	// service rotates it (sbx.inspectionCertMaxAgeDays of the sbx shapes);
	// 365 when unset.
	GatewayCAMaxAgeDays int `json:"gatewayCAMaxAgeDays,omitempty"`
	// NodeSelector and Tolerations place sandbox pods (a Kata node pool, a
	// tainted gVisor pool); none when unset.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Tolerations  []Toleration      `json:"tolerations,omitempty"`
}

// Toleration is a sandbox pod toleration, the Kubernetes field names.
type Toleration struct {
	Key               string `json:"key,omitempty"`
	Operator          string `json:"operator,omitempty"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect,omitempty"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
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

// Sandboxes sets capacity and lifecycle. MemoryMB and CPUs are the size a
// fresh workspace gets unless its creator chose one; MaxMemoryMB and MaxCPUs
// are the most any one workspace may be given, whether at creation, by the
// owner or through an agent's approved request. A zero ceiling means the
// runner derives it from the host (75 % of its memory, all its cores, the
// most SBX itself allows). CPUs may be fractional only where the runtime
// takes fractions (Kubernetes); SBX rounds up to whole CPUs.
type Sandboxes struct {
	MemoryMB             int     `json:"memoryMB,omitempty"`
	CPUs                 float64 `json:"cpus,omitempty"`
	MaxMemoryMB          int     `json:"maxMemoryMB,omitempty"`
	MaxCPUs              float64 `json:"maxCPUs,omitempty"`
	MaxRunning           int     `json:"maxRunning,omitempty"`
	WarmSpares           int     `json:"warmSpares,omitempty"`
	StopAfterIdleMinutes int     `json:"stopAfterIdleMinutes,omitempty"`
	KeepStopped          int     `json:"keepStopped,omitempty"`
	// Egress is what a sandbox may reach through its gateway besides the
	// brokered providers: "restricted" (the template's destination list;
	// the default) or "open" (any public HTTP/HTTPS host). Credentials are
	// injected only for approved requests in either mode; in open mode a
	// brokered host without a grant is reached anonymously instead of
	// being refused.
	Egress string `json:"egress,omitempty"`
}

// Egress modes.
const (
	EgressRestricted = "restricted"
	EgressOpen       = "open"
)

// Chat is the web app listener: the loopback host:port of the sbx shapes.
// It stays the address the policy service's built-in Google Docs client
// redirects to; services.chat.listen may replace it as the chat's own
// listener (a tls:// URL in Kubernetes).
type Chat struct {
	Listen string `json:"listen,omitempty"`
}

// Services are the listeners of the policy service, the runner and the
// chat, and the addresses their clients dial, one URL each. The sbx shapes
// leave them unset: unix://<paths.state>/policy/sbx-control.sock and
// unix://<paths.state>/runner/worker.sock for the first two, and
// http://<chat.listen> for the chat, the same values as before this
// section existed. Kubernetes sets tls://<host>:<port> for all of them,
// with mutual TLS from the tls section (docs/warden-kubernetes-plan.md,
// decision 5 and appendix A).
type Services struct {
	Policy Service       `json:"policy,omitzero"`
	Runner RunnerService `json:"runner,omitzero"`
	Chat   Service       `json:"chat,omitzero"`
}

// Service is one service's listener and the address its clients dial.
// Address defaults to Listen when that is a unix:// (or, for the chat,
// http://) URL; a tls:// listener needs an explicit address, since the
// listener usually binds every interface.
type Service struct {
	Listen  string `json:"listen,omitempty"`
	Address string `json:"address,omitempty"`
}

// RunnerService is the runner's control listener and address plus, where
// the chat and the runner do not share a host, its preview server.
type RunnerService struct {
	Service
	// Previews is the runner's one mutual-TLS preview server (listen) and
	// the address the chat dials it at (docs/warden-kubernetes-plan.md,
	// decisions 5 and 10): every published preview is served under
	// <address>/<publication ID>, and only the chat's certificate is
	// admitted. Both are tls:// URLs, set together or not at all; the
	// tls section is required with them. Unset (the sbx shapes, where the
	// chat and the runner share a host) the runner keeps one loopback
	// listener per publication and hands the chat http://127.0.0.1:<port>/
	// URLs. Accepted with any runtime kind; the chart sets it for
	// Kubernetes.
	Previews Service `json:"previews,omitzero"`
}

// TLS is the mutual-TLS material every service uses on tls:// URLs: the
// deployment CA and this service's own certificate and key, PEM files at
// the same paths in every container (the chart mounts each service's own
// Secret there). Required, and validated, only when a listener or address
// is tls://.
type TLS struct {
	CAFile   string `json:"caFile"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
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

// Edge is the edge's own section: the bug-report receiver
// (docs/bug-reporting-plan.md).
type Edge struct {
	BugReports BugReports `json:"bugReports"`
}

// BugReports configures POST /api/bug-reports on the edge: off unless
// enabled (a local install never receives; the cloud chart values turn it
// on), reports kept under <edge state>/bug-reports for retentionDays and
// at most 10 000 files, at most maxPerHour reports per source IP and
// maxPerDay overall.
type BugReports struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retentionDays,omitempty"`
	MaxPerHour    int  `json:"maxPerHour,omitempty"`
	MaxPerDay     int  `json:"maxPerDay,omitempty"`
}

// Providers are the accounts Warden brokers for agents.
type Providers struct {
	Codex  *AuthFile `json:"codex,omitempty"`
	Claude *Claude   `json:"claude,omitempty"`
	Google *Google   `json:"google,omitempty"`
	GitHub *GitHub   `json:"github,omitempty"`
}

// AuthFile points at one owner-only credential file (the sbx shapes) or
// names the Secret holding the login (the kubernetes kind, appendix A).
type AuthFile struct {
	AuthFile string `json:"authFile,omitempty"`
	Secret   string `json:"secret,omitempty"`
	// DefaultModel is the model new chats of this provider start with
	// (chats/defaults.go has the built-in: GPT-5.6 Sol for Codex).
	DefaultModel string `json:"defaultModel,omitempty"`
}

// Claude is the Claude login (as AuthFile) plus the session features the
// operator may let chats use. Both cost more than the defaults, so both
// are off unless set: fast mode (Claude Code's faster, pricier Opus
// serving) and the 1M-context model variants (`sonnet[1m]`, `opus[1m]`).
type Claude struct {
	AuthFile         string `json:"authFile,omitempty"`
	Secret           string `json:"secret,omitempty"`
	AllowFastMode    bool   `json:"allowFastMode,omitempty"`
	AllowLongContext bool   `json:"allowLongContext,omitempty"`
	// DefaultModel is the model new Claude chats start with (the
	// built-in is Opus, chats/defaults.go).
	DefaultModel string `json:"defaultModel,omitempty"`
}

// logins are the two agent logins as one shape, for the validation that
// applies to both.
func (p Providers) logins() map[string]*AuthFile {
	m := map[string]*AuthFile{"codex": p.Codex}
	if p.Claude != nil {
		m["claude"] = &AuthFile{AuthFile: p.Claude.AuthFile, Secret: p.Claude.Secret}
	} else {
		m["claude"] = nil
	}
	return m
}

// Google selects the Docs OAuth client: "builtin" or a path to an operator
// client file.
type Google struct {
	DocsClient string `json:"docsClient,omitempty"`
}

// GitHub is either a local user token (AuthFile, or Secret on Kubernetes)
// or a server GitHub App.
type GitHub struct {
	AuthFile          string `json:"authFile,omitempty"`
	Secret            string `json:"secret,omitempty"`
	AppID             int64  `json:"appID,omitempty"`
	AppSlug           string `json:"appSlug,omitempty"`
	InstallationOwner string `json:"installationOwner,omitempty"`
	BrokerFile        string `json:"brokerFile,omitempty"`
}

// BuiltinGoogleClient names the shared Warden Docs client.
const BuiltinGoogleClient = "builtin"

// Reporting is the bug-reporting opt-in and the receiver it sends to
// (docs/bug-reporting-plan.md). Enabled false means no drafts are written
// at all; URL is https, or http for a loopback receiver only.
type Reporting struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
}

// DefaultReportingURL is the cloud Warden's receiver.
const DefaultReportingURL = "https://cloud.warden.monaddle.com/api/bug-reports"

// Defaults returns the local-mode configuration for a state root with every
// field computed. Callers that detect host facts overwrite fields afterwards.
func Defaults(state string) Config {
	c := Config{Version: Version}
	c.Paths.State = state
	c.SBX.PrivateHome = filepath.Join(state, "sbx")
	c.SBX.InspectionCertMaxAgeDays = 365
	c.Sandboxes = Sandboxes{MemoryMB: 1536, CPUs: 1, MaxRunning: 2, WarmSpares: 1, StopAfterIdleMinutes: 30, KeepStopped: 32, Egress: EgressRestricted}
	c.Chat.Listen = "127.0.0.1:18780"
	c.Previews = Previews{Mode: PreviewLoopback, HostSuffix: "localhost", EdgeListen: "127.0.0.1:18781"}
	c.Auth = Auth{Mode: AuthOwner, PublicURL: "http://" + c.Previews.EdgeListen}
	c.Edge.BugReports = BugReports{RetentionDays: 90, MaxPerHour: 30, MaxPerDay: 500}
	provider := filepath.Join(state, "provider")
	c.Providers = Providers{
		Codex:  &AuthFile{AuthFile: filepath.Join(provider, "auth.json")},
		Claude: &Claude{AuthFile: filepath.Join(provider, "claude.json")},
		Google: &Google{DocsClient: BuiltinGoogleClient},
		GitHub: &GitHub{AuthFile: filepath.Join(provider, "github.json")},
	}
	c.Reporting = Reporting{URL: DefaultReportingURL}
	return c
}

// RuntimeKind is the effective runtime kind: runtime.kind, or sbx when the
// file does not say.
func (c Config) RuntimeKind() string {
	if c.Runtime.Kind == "" {
		return RuntimeSBX
	}
	return c.Runtime.Kind
}

// GatewayMode is how bindings' gateways are listened for, derived from the
// kind rather than configured: the sbx shapes keep their per-binding
// loopback listeners (the shared gateway is not switched on there in this
// plan), and Kubernetes has no per-binding ports, so it is always shared.
func (c Config) GatewayMode() string {
	if c.RuntimeKind() == RuntimeKubernetes {
		return GatewayShared
	}
	return GatewayLoopback
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

// Transport URLs: what each service listens on and what its clients dial.
// Each is the file's value or the sbx-shape default named in Services.
func (c Config) PolicyListen() string {
	return first(c.Services.Policy.Listen, "unix://"+c.PolicySocket())
}
func (c Config) PolicyAddress() string {
	return first(c.Services.Policy.Address, unixOnly(c.Services.Policy.Listen), "unix://"+c.PolicySocket())
}
func (c Config) RunnerListen() string {
	return first(c.Services.Runner.Listen, "unix://"+c.RunnerSocket())
}
func (c Config) RunnerAddress() string {
	return first(c.Services.Runner.Address, unixOnly(c.Services.Runner.Listen), "unix://"+c.RunnerSocket())
}
func (c Config) ChatListen() string {
	return first(c.Services.Chat.Listen, "http://"+c.Chat.Listen)
}
func (c Config) ChatAddress() string {
	return first(c.Services.Chat.Address, httpOnly(c.Services.Chat.Listen), "http://"+c.Chat.Listen)
}

// RunnerPreviewListen and RunnerPreviewAddress are the runner's shared
// preview server and the address the chat dials it at
// (services.runner.previews), both "" when the file sets none: the sbx
// shapes' per-publication loopback listeners.
func (c Config) RunnerPreviewListen() string  { return c.Services.Runner.Previews.Listen }
func (c Config) RunnerPreviewAddress() string { return c.Services.Runner.Previews.Address }

// TransportTLS is the tls section as the transport package takes it, nil
// when the file has none.
func (c Config) TransportTLS() *transport.TLS {
	if c.TLS == nil {
		return nil
	}
	return &transport.TLS{CAFile: c.TLS.CAFile, CertFile: c.TLS.CertFile, KeyFile: c.TLS.KeyFile}
}

// UsesTLS reports whether any listener or address is tls://.
func (c Config) UsesTLS() bool {
	for _, u := range []string{c.PolicyListen(), c.PolicyAddress(), c.RunnerListen(), c.RunnerAddress(), c.ChatListen(), c.ChatAddress(), c.RunnerPreviewListen(), c.RunnerPreviewAddress()} {
		if transport.IsTLS(u) {
			return true
		}
	}
	return false
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func unixOnly(u string) string {
	if strings.HasPrefix(u, "unix://") {
		return u
	}
	return ""
}
func httpOnly(u string) string {
	if strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}

// HostOf is the host:port of an http:// or tls:// URL, "" for anything else.
func HostOf(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil || (u.Scheme != "http" && u.Scheme != "tls") {
		return ""
	}
	return u.Host
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
	case c.Providers.GitHub.AuthFile != "" || c.Providers.GitHub.Secret != "":
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
	if c.RuntimeKind() == RuntimeKubernetes {
		if file.SBX != (SBX{}) {
			return c, errors.New("sbx.* is only used with runtime.kind \"sbx\"")
		}
		// The file-backed provider defaults are the sbx shapes'; here a
		// provider is configured only by naming its Secret.
		if file.Providers.Codex == nil {
			c.Providers.Codex = nil
		}
		if file.Providers.Claude == nil {
			c.Providers.Claude = nil
		}
		if file.Providers.GitHub == nil {
			c.Providers.GitHub = nil
		}
	}
	// A provider set to JSON null is removed: the pointer stays nil in file,
	// so explicit nulls are read separately and hide that integration.
	var nulls struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(raw, &nulls)
	// warmSpares is the one integer whose zero is a setting (no spare
	// sandbox), which merge cannot tell from unset; an explicit value is
	// read separately so 0 survives.
	var explicit struct {
		Sandboxes struct {
			WarmSpares *int `json:"warmSpares"`
		} `json:"sandboxes"`
	}
	_ = json.Unmarshal(raw, &explicit)
	if explicit.Sandboxes.WarmSpares != nil {
		c.Sandboxes.WarmSpares = *explicit.Sandboxes.WarmSpares
	}
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
	setString(&c.Runtime.Kind, file.Runtime.Kind)
	if file.Kubernetes != nil {
		k := *file.Kubernetes
		if k.WorkspaceSizeGi == 0 {
			k.WorkspaceSizeGi = 20
		}
		if k.GatewayService == "" {
			k.GatewayService = "warden-gateway"
		}
		if k.GatewayPort == 0 {
			k.GatewayPort = 7000
		}
		if k.TrustConfigMap == "" {
			k.TrustConfigMap = "warden-guest-trust"
		}
		if k.GatewayCAMaxAgeDays == 0 {
			k.GatewayCAMaxAgeDays = 365
		}
		c.Kubernetes = &k
	}
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
	setFloat(&c.Sandboxes.CPUs, file.Sandboxes.CPUs)
	setInt(&c.Sandboxes.MaxMemoryMB, file.Sandboxes.MaxMemoryMB)
	setFloat(&c.Sandboxes.MaxCPUs, file.Sandboxes.MaxCPUs)
	setInt(&c.Sandboxes.MaxRunning, file.Sandboxes.MaxRunning)
	setInt(&c.Sandboxes.WarmSpares, file.Sandboxes.WarmSpares)
	setInt(&c.Sandboxes.StopAfterIdleMinutes, file.Sandboxes.StopAfterIdleMinutes)
	setInt(&c.Sandboxes.KeepStopped, file.Sandboxes.KeepStopped)
	setString(&c.Sandboxes.Egress, file.Sandboxes.Egress)
	setString(&c.Chat.Listen, file.Chat.Listen)
	setString(&c.Services.Policy.Listen, file.Services.Policy.Listen)
	setString(&c.Services.Policy.Address, file.Services.Policy.Address)
	setString(&c.Services.Runner.Listen, file.Services.Runner.Listen)
	setString(&c.Services.Runner.Address, file.Services.Runner.Address)
	setString(&c.Services.Runner.Previews.Listen, file.Services.Runner.Previews.Listen)
	setString(&c.Services.Runner.Previews.Address, file.Services.Runner.Previews.Address)
	setString(&c.Services.Chat.Listen, file.Services.Chat.Listen)
	setString(&c.Services.Chat.Address, file.Services.Chat.Address)
	if file.Chat.Listen == "" {
		// A loopback services.chat.listen alone also sets chat.listen, so
		// the two never disagree unless the file says both.
		if host := HostOf(httpOnly(file.Services.Chat.Listen)); host != "" {
			c.Chat.Listen = host
		}
	}
	if file.TLS != nil {
		t := *file.TLS
		c.TLS = &t
	}
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
	if file.Edge.BugReports.Enabled {
		c.Edge.BugReports.Enabled = true
	}
	setInt(&c.Edge.BugReports.RetentionDays, file.Edge.BugReports.RetentionDays)
	setInt(&c.Edge.BugReports.MaxPerHour, file.Edge.BugReports.MaxPerHour)
	setInt(&c.Edge.BugReports.MaxPerDay, file.Edge.BugReports.MaxPerDay)
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
	if file.Reporting.Enabled {
		c.Reporting.Enabled = true
	}
	setString(&c.Reporting.URL, file.Reporting.URL)
	if file.Dogfood.Jailbreak {
		c.Dogfood.Jailbreak = true
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
func setFloat(dst *float64, v float64) {
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
	if err := c.validateKind(); err != nil {
		return err
	}
	sbx := c.RuntimeKind() == RuntimeSBX
	// The Unix socket and loopback rules belong to the one-host shapes: on
	// Kubernetes the services dial each other over tls:// and the edge port
	// reaches the operator through a port-forward.
	if sbx && len(c.PolicySocket()) > 100 {
		return errors.New("paths.state is too long for the private Unix sockets")
	}
	if sbx && len(SBXSocketPath(c.SBX.PrivateHome)) > 103 {
		return errors.New("sbx.privateHome is too long: sbx binds Unix sockets under it (keep the state directory short, e.g. ~/.warden)")
	}
	if err := loopback(c.Chat.Listen); err != nil {
		return fmt.Errorf("chat.listen: %w", err)
	}
	if err := c.validateTransport(); err != nil {
		return err
	}
	switch c.Previews.Mode {
	case PreviewLoopback:
		if c.Previews.HostSuffix != "localhost" {
			return errors.New("previews.hostSuffix must be \"localhost\" in loopback mode")
		}
		if sbx {
			if err := loopback(c.Previews.EdgeListen); err != nil {
				return fmt.Errorf("previews.edgeListen: %w", err)
			}
		} else if _, _, err := net.SplitHostPort(c.Previews.EdgeListen); err != nil {
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
	if s.MemoryMB < 512 || s.MemoryMB > 65536 || s.MaxRunning < 1 || s.WarmSpares < 0 || s.StopAfterIdleMinutes < 1 || s.KeepStopped < 1 {
		return errors.New("sandboxes: memoryMB 512–65536, maxRunning ≥ 1, warmSpares ≥ 0, stopAfterIdleMinutes ≥ 1, keepStopped ≥ 1")
	}
	if s.CPUs < 0.25 || s.CPUs > 64 || s.MaxCPUs < 0 || s.MaxCPUs > 64 || s.MaxMemoryMB < 0 || s.MaxMemoryMB > 65536 {
		return errors.New("sandboxes: cpus 0.25–64, maxCPUs ≤ 64, maxMemoryMB ≤ 65536")
	}
	if (s.MaxCPUs != 0 && s.MaxCPUs < s.CPUs) || (s.MaxMemoryMB != 0 && s.MaxMemoryMB < s.MemoryMB) {
		return errors.New("sandboxes: the maximum size is below the default")
	}
	if s.Egress != EgressRestricted && s.Egress != EgressOpen {
		return fmt.Errorf("sandboxes.egress must be %q or %q", EgressRestricted, EgressOpen)
	}
	if c.Dogfood.Jailbreak && !c.JailbreakAllowed() {
		return ErrJailbreakRefused
	}
	if g := c.Providers.GitHub; g != nil {
		user, app := g.AuthFile != "" || g.Secret != "", g.AppID != 0 || g.AppSlug != "" || g.InstallationOwner != "" || g.BrokerFile != ""
		if user == app {
			return errors.New("providers.github must be either a user authFile (or secret) or a GitHub App (appID, appSlug, installationOwner, brokerFile), not both or neither")
		}
		if g.AuthFile != "" && g.Secret != "" {
			return errors.New("providers.github: authFile and secret are exclusive")
		}
		if app && (g.AppID == 0 || g.AppSlug == "" || g.InstallationOwner == "" || g.BrokerFile == "") {
			return errors.New("providers.github App mode requires appID, appSlug, installationOwner and brokerFile")
		}
	}
	if g := c.Providers.Google; g != nil && g.DocsClient == "" {
		return errors.New("providers.google.docsClient must be \"builtin\" or a file path")
	}
	if b := c.Edge.BugReports; b.RetentionDays < 1 || b.RetentionDays > 3650 || b.MaxPerHour < 1 || b.MaxPerDay < 1 {
		return errors.New("edge.bugReports: retentionDays 1–3650, maxPerHour ≥ 1, maxPerDay ≥ 1")
	}
	if err := c.Reporting.validate(); err != nil {
		return err
	}
	return nil
}

// validate admits an https receiver, or http on a loopback address (a
// local receiver while developing); anything else is refused so a report
// never travels in the clear to another host.
func (r Reporting) validate() error {
	if r.URL == "" {
		return errors.New("reporting.url is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("reporting.url %q must be an absolute https:// URL", r.URL)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("reporting.url %q: http is allowed for 127.0.0.1 only", r.URL)
	}
	return fmt.Errorf("reporting.url %q must be an absolute https:// URL", r.URL)
}

// validateKind applies the rules that depend on runtime.kind: the sbx
// section and file-backed provider logins belong to the sbx kind, the
// kubernetes section and Secret-backed logins to the kubernetes kind.
func (c Config) validateKind() error {
	switch c.RuntimeKind() {
	case RuntimeSBX:
		if c.Kubernetes != nil {
			return errors.New("kubernetes.* is only used with runtime.kind \"kubernetes\"")
		}
		for name, p := range c.Providers.logins() {
			if p != nil && p.Secret != "" {
				return fmt.Errorf("providers.%s.secret is only used with runtime.kind \"kubernetes\"; the sbx shapes use authFile", name)
			}
		}
		if c.Providers.GitHub != nil && c.Providers.GitHub.Secret != "" {
			return errors.New("providers.github.secret is only used with runtime.kind \"kubernetes\"; the sbx shapes use authFile")
		}
	case RuntimeKubernetes:
		k := c.Kubernetes
		if k == nil {
			return errors.New("runtime.kind \"kubernetes\" requires the kubernetes section (namespace, tier, runtimeClass, guestImage, guestImageDigest)")
		}
		if !dnsLabel(k.Namespace) {
			return errors.New("kubernetes.namespace must be a DNS label")
		}
		if k.Tier != TierKata && k.Tier != TierGVisor {
			return fmt.Errorf("kubernetes.tier must be %q or %q", TierKata, TierGVisor)
		}
		if !dnsLabel(k.RuntimeClass) {
			return errors.New("kubernetes.runtimeClass must be a DNS label")
		}
		if k.GuestImage == "" || strings.ContainsAny(k.GuestImage, " @\t\n") {
			return errors.New("kubernetes.guestImage must be an image reference without a digest")
		}
		if !digestShape(k.GuestImageDigest) {
			return errors.New("kubernetes.guestImageDigest must be sha256:<64 hex>")
		}
		if k.WorkspaceSizeGi < 1 {
			return errors.New("kubernetes.workspaceSizeGi must be at least 1")
		}
		if !dnsLabel(k.GatewayService) || !dnsLabel(k.TrustConfigMap) || (k.StorageClass != "" && !dnsLabel(k.StorageClass)) {
			return errors.New("kubernetes.gatewayService, trustConfigMap and storageClass must be DNS labels")
		}
		if k.GatewayPort < 1 || k.GatewayPort > 65535 {
			return errors.New("kubernetes.gatewayPort must be a TCP port")
		}
		if k.GatewayCAMaxAgeDays < 0 {
			return errors.New("kubernetes.gatewayCAMaxAgeDays must not be negative")
		}
		for i, t := range k.Tolerations {
			if t.Operator != "" && t.Operator != "Equal" && t.Operator != "Exists" {
				return fmt.Errorf("kubernetes.tolerations[%d].operator must be Equal or Exists", i)
			}
			if t.Effect != "" && t.Effect != "NoSchedule" && t.Effect != "PreferNoSchedule" && t.Effect != "NoExecute" {
				return fmt.Errorf("kubernetes.tolerations[%d].effect must be NoSchedule, PreferNoSchedule or NoExecute", i)
			}
		}
		for name, p := range c.Providers.logins() {
			if p == nil {
				continue
			}
			if p.AuthFile != "" {
				return fmt.Errorf("providers.%s.authFile is only used with runtime.kind \"sbx\"; the kubernetes kind names a secret", name)
			}
			if !dnsLabel(p.Secret) {
				return fmt.Errorf("providers.%s.secret must name a Secret", name)
			}
		}
		if g := c.Providers.GitHub; g != nil {
			if g.AuthFile != "" {
				return errors.New("providers.github.authFile is only used with runtime.kind \"sbx\"; the kubernetes kind names a secret")
			}
			if g.Secret != "" && !dnsLabel(g.Secret) {
				return errors.New("providers.github.secret must name a Secret")
			}
		}
	default:
		return fmt.Errorf("runtime.kind must be %q or %q", RuntimeSBX, RuntimeKubernetes)
	}
	return nil
}

// dnsLabel reports whether s is a Kubernetes object name: lower-case
// alphanumerics and dashes, 1–63 bytes, starting and ending alphanumeric.
func dnsLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// validateTransport applies the URL rules: unix:// or tls:// for the policy
// service and the runner, http:// (loopback) or tls:// for the chat, a
// listener and its address on the same scheme, an explicit address for a
// tls:// listener, and the tls section whenever any of them is tls://.
func (c Config) validateTransport() error {
	services := []struct {
		name, listen, address string
		schemes               []string
	}{
		{"policy", c.PolicyListen(), c.PolicyAddress(), []string{"unix", "tls"}},
		{"runner", c.RunnerListen(), c.RunnerAddress(), []string{"unix", "tls"}},
		{"chat", c.ChatListen(), c.ChatAddress(), []string{"http", "tls"}},
	}
	for _, s := range services {
		listen, err := serviceURL(s.listen, s.schemes, true)
		if err != nil {
			return fmt.Errorf("services.%s.listen: %w", s.name, err)
		}
		address, err := serviceURL(s.address, s.schemes, false)
		if err != nil {
			return fmt.Errorf("services.%s.address: %w", s.name, err)
		}
		if listen != address {
			return fmt.Errorf("services.%s: listen is %s:// but address is %s://", s.name, listen, address)
		}
		if listen == "tls" && c.services(s.name).Address == "" {
			return fmt.Errorf("services.%s.address is required with a tls:// listener", s.name)
		}
	}
	if HostOf(httpOnly(c.ChatListen())) != "" && HostOf(c.ChatListen()) != c.Chat.Listen {
		return fmt.Errorf("services.chat.listen %s disagrees with chat.listen %s", c.ChatListen(), c.Chat.Listen)
	}
	if err := c.validatePreviewListener(); err != nil {
		return err
	}
	if c.UsesTLS() {
		if c.TLS == nil {
			return errors.New("tls (caFile, certFile, keyFile) is required when a service listens on or dials a tls:// URL")
		}
		for name, path := range map[string]string{"caFile": c.TLS.CAFile, "certFile": c.TLS.CertFile, "keyFile": c.TLS.KeyFile} {
			if !filepath.IsAbs(path) {
				return fmt.Errorf("tls.%s must be an absolute path", name)
			}
		}
	}
	return nil
}

func (c Config) services(name string) Service {
	switch name {
	case "policy":
		return c.Services.Policy
	case "runner":
		return c.Services.Runner.Service
	}
	return c.Services.Chat
}

// validatePreviewListener applies the rules of services.runner.previews:
// listen and address are set together or not at all, both tls:// (the
// chat dials it as https:// with its client certificate, so there is no
// plaintext form), and the address names a host. The tls section is
// checked with the other tls:// URLs.
func (c Config) validatePreviewListener() error {
	p := c.Services.Runner.Previews
	if p.Listen == "" && p.Address == "" {
		return nil
	}
	if p.Listen == "" || p.Address == "" {
		return errors.New("services.runner.previews: listen and address are set together")
	}
	if _, err := serviceURL(p.Listen, []string{"tls"}, true); err != nil {
		return fmt.Errorf("services.runner.previews.listen: %w", err)
	}
	if _, err := serviceURL(p.Address, []string{"tls"}, false); err != nil {
		return fmt.Errorf("services.runner.previews.address: %w", err)
	}
	if _, previews, _ := net.SplitHostPort(HostOf(p.Listen)); previews != "" {
		if _, control, _ := net.SplitHostPort(HostOf(c.RunnerListen())); control == previews {
			return errors.New("services.runner.previews.listen must use a port other than services.runner.listen: the preview server is its own listener")
		}
	}
	return nil
}

// serviceURL checks one transport URL against the allowed schemes and
// returns its scheme. A listener may bind every interface (tls://:port);
// an address needs a host. http:// URLs keep the loopback rule the chat
// listener always had; unix:// paths keep the socket length limit.
func serviceURL(rawurl string, schemes []string, listener bool) (string, error) {
	scheme, rest, ok := strings.Cut(rawurl, "://")
	if !ok || !contains(schemes, scheme) {
		return "", fmt.Errorf("%q must be a %s URL", rawurl, strings.Join(schemes, ":// or ")+"://")
	}
	switch scheme {
	case "unix":
		if !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("%q: a unix:// URL needs an absolute socket path", rawurl)
		}
		if len(rest) > 100 {
			return "", fmt.Errorf("%q: socket path too long for a private Unix socket", rawurl)
		}
	case "http":
		u, err := url.Parse(rawurl)
		if err != nil || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
			return "", fmt.Errorf("%q must be http://<loopback>:<port>", rawurl)
		}
		if err := loopback(u.Host); err != nil {
			return "", fmt.Errorf("%q: %w", rawurl, err)
		}
	case "tls":
		if strings.ContainsAny(rest, "/?#@") {
			return "", fmt.Errorf("%q: a tls:// URL is host:port only", rawurl)
		}
		host, port, err := net.SplitHostPort(rest)
		if err != nil {
			return "", fmt.Errorf("%q: %w", rawurl, err)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q: port must be 1-65535", rawurl)
		}
		if host == "" && !listener {
			return "", fmt.Errorf("%q: an address needs a host", rawurl)
		}
	}
	return scheme, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
