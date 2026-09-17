package policysvc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/policy"
	"warden/chat/internal/transport"
)

// settings are the effective values after reconciling the legacy flags with
// the optional warden.json (--config or $WARDEN_CONFIG). Every flag maps to
// one configuration field; a flag that disagrees with a loaded file is an
// error rather than a silent precedence.
type settings struct {
	cfg              config.Config
	state            string // policy state directory (paths.state/policy)
	listen           string // services.policy.listen (unix://<state>/sbx-control.sock)
	tls              *transport.TLS
	sbx              string // sbx.executable
	codexAuth        string // providers.codex.authFile
	claudeAuth       string // providers.claude.authFile
	googleConfig     string // providers.google.docsClient when it is a file
	googleConfigured bool   // providers.google present
	vendorDir        string // paths.githubCatalog
	template         string // paths.sandboxPolicyTemplate
	caMaxAge         time.Duration
	guestDigest      string // sbx.guestImageDigest
	githubBroker     string // providers.github.brokerFile or $WARDEN_GITHUB_APP_BROKER
	githubSlug       string // providers.github.appSlug
	githubConfigured bool   // providers.github present
	githubAuthFile   string // providers.github.authFile (local user token)
	chatListen       string // chat.listen; its port is the built-in Google redirect
	publicURL        string // auth.publicURL; the Google redirect origin in the kubernetes kind
	egress           string // sandboxes.egress: restricted or open
	// The kubernetes kind (docs/warden-kubernetes-plan.md, work item 5):
	// kind is runtime.kind; kubernetes the section; the *Secret fields are
	// providers.<p>.secret; kubeconfig is --kubeconfig or
	// $WARDEN_KUBECONFIG (empty: the pod's own service account).
	kind          string
	kubernetes    *config.Kubernetes
	codexSecret   string
	claudeSecret  string
	githubSecret  string
	kubeconfig    string
	coreNamespace string // --kube-namespace; empty: the client's own
}

type policyFlags struct {
	configPath, state, sbx, googleConfig, claudeAuth, codexAuth, vendorDir, template, guestDigest, githubAuthFile, chatListen, egress *string
	caMaxAge                                                                                                                          *time.Duration
	// kubeconfig and kubeNamespace are the kubernetes kind's development
	// flags; nil when the flag set does not define them.
	kubeconfig, kubeNamespace *string
}

func resolveSettings(fs *flag.FlagSet, f policyFlags) (settings, error) {
	cfg, source, err := config.Resolve(*f.configPath, *f.state)
	if err != nil {
		return settings{}, err
	}
	o := config.NewOverrides(fs, source)
	s := settings{cfg: cfg}
	s.state = config.Override(o, "state", *f.state, "paths.state (policy directory)", cfg.PolicyState())
	// The listener: the file's services.policy.listen, or, with the legacy
	// --state flag alone, the socket inside that directory as before.
	s.listen = cfg.PolicyListen()
	if !o.FromFile() {
		s.listen = "unix://" + filepath.Join(s.state, "sbx-control.sock")
	}
	s.tls = cfg.TransportTLS()
	s.sbx = config.Override(o, "sbx", *f.sbx, "sbx.executable", cfg.SBX.Executable)
	s.vendorDir = config.Override(o, "vendor-dir", *f.vendorDir, "paths.githubCatalog", cfg.Paths.GitHubCatalog)
	if s.vendorDir == "" {
		s.vendorDir = defaultPath("vendor")
	}
	s.template = config.Override(o, "policy-template", *f.template, "paths.sandboxPolicyTemplate", cfg.Paths.SandboxPolicyTemplate)
	if s.template == "" {
		s.template = defaultPath("config/policy.template.json")
	}
	s.kind = cfg.RuntimeKind()
	if s.kind == config.RuntimeKubernetes {
		k := *cfg.Kubernetes
		s.kubernetes = &k
		s.caMaxAge = config.Override(o, "gateway-ca-max-age", *f.caMaxAge, "kubernetes.gatewayCAMaxAgeDays", time.Duration(k.GatewayCAMaxAgeDays)*24*time.Hour)
		s.guestDigest = config.Override(o, "guest-image-digest", *f.guestDigest, "kubernetes.guestImageDigest", k.GuestImageDigest)
		if f.kubeconfig != nil {
			s.kubeconfig = *f.kubeconfig
		}
		if s.kubeconfig == "" {
			s.kubeconfig = os.Getenv("WARDEN_KUBECONFIG")
		}
		if f.kubeNamespace != nil {
			s.coreNamespace = *f.kubeNamespace
		}
	} else {
		s.caMaxAge = config.Override(o, "gateway-ca-max-age", *f.caMaxAge, "sbx.inspectionCertMaxAgeDays", time.Duration(cfg.SBX.InspectionCertMaxAgeDays)*24*time.Hour)
		s.guestDigest = config.Override(o, "guest-image-digest", *f.guestDigest, "sbx.guestImageDigest", cfg.GuestDigest())
	}
	s.egress = config.Override(o, "egress", *f.egress, "sandboxes.egress", cfg.Sandboxes.Egress)
	codex := ""
	if cfg.Providers.Codex != nil {
		codex = cfg.Providers.Codex.AuthFile
		s.codexSecret = cfg.Providers.Codex.Secret
	}
	s.codexAuth = config.Override(o, "codex-auth-file", *f.codexAuth, "providers.codex.authFile", codex)
	claude := ""
	if cfg.Providers.Claude != nil {
		claude = cfg.Providers.Claude.AuthFile
		s.claudeSecret = cfg.Providers.Claude.Secret
	}
	s.claudeAuth = config.Override(o, "claude-auth-file", *f.claudeAuth, "providers.claude.authFile", claude)
	docs := ""
	if cfg.Providers.Google != nil {
		s.googleConfigured = true
		docs = cfg.Providers.Google.DocsClient
	}
	docs = config.Override(o, "google-config", *f.googleConfig, "providers.google.docsClient", docs)
	if docs != config.BuiltinGoogleClient {
		// A file path is the operator's own client; "builtin" leaves
		// googleConfig empty and settings.google uses the release client.
		s.googleConfig = docs
	}
	if o.Set("google-config") {
		s.googleConfigured = true
	}
	broker, authFile := "", ""
	if g := cfg.Providers.GitHub; g != nil {
		s.githubConfigured = true
		s.githubSlug = g.AppSlug
		broker = g.BrokerFile
		authFile = g.AuthFile
		s.githubSecret = g.Secret
	}
	s.githubAuthFile = config.Override(o, "github-auth-file", *f.githubAuthFile, "providers.github.authFile", authFile)
	if o.Set("github-auth-file") {
		s.githubConfigured = true
	}
	s.chatListen = config.Override(o, "chat-listen", *f.chatListen, "chat.listen", cfg.Chat.Listen)
	s.publicURL = strings.TrimRight(cfg.Auth.PublicURL, "/")
	if env := os.Getenv("WARDEN_GITHUB_APP_BROKER"); env != "" {
		if source != "" && broker != "" && broker != env {
			return settings{}, errNamed("$WARDEN_GITHUB_APP_BROKER " + env + " disagrees with providers.github.brokerFile " + broker + " in " + source)
		}
		broker = env
		s.githubConfigured = true
	}
	s.githubBroker = broker
	if s.githubSlug == "" && s.githubBroker != "" {
		s.githubSlug = policy.DefaultGitHubAppSlug
	}
	if err := o.Err(); err != nil {
		return settings{}, err
	}
	return s, nil
}

type errNamed string

func (e errNamed) Error() string { return string(e) }

// googleSharing hides the Google integration when providers.google is absent
// by handing the sharing service a nil interface rather than a nil pointer.
func (s settings) googleSharing(g *policy.GoogleConnection) policy.GoogleSharing {
	if !s.googleConfigured || g == nil {
		return nil
	}
	return g
}
