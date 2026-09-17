// Package policysvc is `warden policy`, the private sandbox control and
// credential broker: registry, per-sandbox policy engines, the runtime
// verifier over the kind's inspector (SBX, or Kubernetes with its canaries),
// inspected gateways (per-binding loopback listeners, or one shared listener
// behind the warden-gateway Service), sharing services and the GitHub App
// broker subcommand.
package policysvc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/handshake"
	"warden/chat/internal/policy"
	"warden/chat/internal/services"
	"warden/chat/internal/transport"
)

// Main runs the policy service (or one of its one-shot subcommands:
// github-broker, git-limited, rotate-gateway-ca, selfcheck) and returns
// the exit status.
func Main(args []string) int { return services.Run(run, args) }

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "github-broker":
			return services.ExitCode(policy.RunGitHubBroker(args[1:], os.Stdin, os.Stdout))
		case "git-limited":
			return policy.RunLimitedGit(args[1:])
		case "rotate-gateway-ca":
			if err := rotateGatewayCA(args[1:]); err != nil {
				return errors.New("rotate-gateway-ca: " + err.Error())
			}
			return nil
		case "selfcheck":
			// Packaging smoke check: the catalog, GitHub networks, policy
			// template and an engine must all load from the installed layout.
			if err := selfcheck(args[1:]); err != nil {
				return errors.New("selfcheck: " + err.Error())
			}
			fmt.Println("warden policy selfcheck ok")
			return nil
		}
	}
	fs := flag.NewFlagSet("warden policy", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional warden.json (default $WARDEN_CONFIG); flags override its computed defaults and must agree with its values")
	state := fs.String("state", "", "private state directory (paths.state/policy)")
	sbx := fs.String("sbx", "", "explicit trusted SBX executable; enables pinned CLI verifier")
	mitmdump := fs.String("mitmdump", "", "ignored; gateways run in-process (kept for launcher compatibility)")
	manageNetwork := fs.Bool("manage-network", false, "ignored; sandbox gateway rules are always managed (kept for launcher compatibility)")
	googleConfig := fs.String("google-config", "", "private Google OAuth client configuration for file sharing")
	claudeAuth := fs.String("claude-auth-file", "", "private host Claude credential file")
	codexAuth := fs.String("codex-auth-file", "", "private host Codex login, read without copying or refreshing")
	documentOrigin := fs.String("document-api-origin", "", "fixed local app HTTP origin, e.g. http://127.0.0.1:8080")
	documentKey := fs.String("document-api-key-file", "", "private app/Warden shared signing key; never sent to sandbox")
	vendorDir := fs.String("vendor-dir", defaultPath("vendor"), "directory holding github-operations.json and github-meta.json")
	template := fs.String("policy-template", defaultPath(filepath.Join("config", "policy.template.json")), "policy template for new sandboxes")
	caMaxAge := fs.Duration("gateway-ca-max-age", policy.DefaultGatewayCAMaxAge, "rotate the host gateway CA at startup when older than this (0 disables)")
	guestDigest := fs.String("guest-image-digest", policy.SBXShellDigest, "digest of the only guest image a verified runtime may run (sha256:…)")
	version := fs.Bool("version", false, "print the build revision and protocol number")
	githubAuthFile := fs.String("github-auth-file", "", "private user OAuth token file written by warden login github; exclusive with WARDEN_GITHUB_APP_BROKER")
	chatListen := fs.String("chat-listen", "127.0.0.1:18780", "chat listen address; its port is the loopback redirect of the built-in Google Docs client")
	egress := fs.String("egress", config.EgressRestricted, "what sandboxes may reach besides the brokered providers: restricted (the template's destination list) or open (any public HTTP/HTTPS host; credentials still only after approval) (sandboxes.egress)")
	kubeconfig := fs.String("kubeconfig", "", "runtime.kind kubernetes, development only: a kubeconfig file instead of the pod's service account (default $WARDEN_KUBECONFIG)")
	kubeNamespace := fs.String("kube-namespace", "", "runtime.kind kubernetes with --kubeconfig: the core namespace holding the provider Secrets (default: the kubeconfig context's)")
	if err := services.ParseFlags(fs, args); err != nil {
		return err
	}
	if *version {
		fmt.Println(handshake.Self("warden-policy"))
		return nil
	}
	s, err := resolveSettings(fs, policyFlags{configPath: configPath, state: state, sbx: sbx, googleConfig: googleConfig, claudeAuth: claudeAuth, codexAuth: codexAuth, vendorDir: vendorDir, template: template, guestDigest: guestDigest, caMaxAge: caMaxAge, githubAuthFile: githubAuthFile, chatListen: chatListen, egress: egress, kubeconfig: kubeconfig, kubeNamespace: kubeNamespace})
	if err != nil {
		return err
	}
	if err := supportedKind(s.kind); err != nil {
		return err
	}
	if *manageNetwork && s.sbx == "" && s.kind == config.RuntimeSBX {
		return errors.New("managed mode requires --sbx")
	}
	state, sbx, googleConfig, claudeAuth, codexAuth, vendorDir, template, guestDigest, caMaxAge = &s.state, &s.sbx, &s.googleConfig, &s.claudeAuth, &s.codexAuth, &s.vendorDir, &s.template, &s.guestDigest, &s.caMaxAge
	if (*documentOrigin == "") != (*documentKey == "") {
		return errors.New("document API origin and key file must be configured together")
	}
	_ = mitmdump
	syscall.Umask(0o077)
	if err := os.MkdirAll(*state, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(*state)
	if err != nil {
		return err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || stat == nil || int(stat.Uid) != os.Getuid() {
		return errors.New("state must be an owned private directory")
	}
	if err = os.Chmod(*state, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(*state, "service.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another Warden policy service holds this state directory")
	}
	defer lock.Close()
	listen, err := transport.Parse(s.listen)
	if err != nil {
		return err
	}
	if listen.Scheme == transport.SchemeUnix && len(listen.Path) > 100 {
		return errors.New("state path too long for private Unix socket")
	}
	operations, err := policy.LoadOperations(filepath.Join(*vendorDir, "github-operations.json"))
	if err != nil {
		return errors.New("operation catalog: " + err.Error())
	}
	networks, err := policy.LoadGitHubNetworks(filepath.Join(*vendorDir, "github-meta.json"))
	if err != nil {
		return errors.New("GitHub networks: " + err.Error())
	}
	if _, err = os.Stat(*template); err != nil {
		return errors.New("policy template: " + err.Error())
	}
	var documentAPI *policy.DocumentAPI
	if *documentOrigin != "" {
		documentAPI, err = policy.NewDocumentAPI(*documentOrigin, *documentKey)
		if err != nil {
			return err
		}
	}
	ca, rotated, err := policy.PrepareGatewayCA(*state, *caMaxAge, time.Now())
	if err != nil {
		return errors.New("gateway CA: " + err.Error())
	}
	if rotated {
		fmt.Printf("gateway CA rotated (older than %s); guests reinstall it on their next run; fingerprint %s\n", *caMaxAge, ca.Fingerprint())
	}
	// The kubernetes kind: the API client, the Secret credential store and
	// the trust publisher come first; the gateway and inspector attach to
	// the registry below.
	var k8s *kubernetesRuntime
	var store policy.CredentialStore
	if s.kind == config.RuntimeKubernetes {
		if k8s, err = newKubernetesRuntime(s); err != nil {
			return err
		}
		store = k8s.store
	}
	github, err := s.github(store)
	if err != nil {
		return err
	}
	egressMode := "restricted"
	switch s.egress {
	case config.EgressOpen:
		egressMode = "public"
	case config.EgressRestricted:
	default:
		return errors.New("--egress must be restricted or open")
	}
	// A mode chosen from the Admin console persists in the policy state and
	// wins over warden.json until it is cleared.
	if saved, err := policy.LoadEgressMode(*state); err != nil {
		return errors.New("egress setting: " + err.Error())
	} else if saved != "" {
		egressMode = saved
	}
	options := policy.RegistryOptions{Operations: operations, PolicyTemplate: *template, GitHubAppConfig: s.githubBroker, GitHubAuthFile: s.githubAuthFile, DocumentAPI: documentAPI, Networks: networks, CA: ca, EgressMode: egressMode}
	registry, err := policy.NewRegistry(*state, options)
	if err != nil {
		return errors.New("registry: " + err.Error())
	}
	google, err := s.google()
	if err != nil {
		return errors.New("Google connection: " + err.Error())
	}
	defer google.Close()
	sharing, err := policy.NewSharing(*state, s.googleSharing(google), nil, github)
	if err != nil {
		return errors.New("sharing: " + err.Error())
	}
	defer sharing.Close()
	sharing.GitHubConfigured, sharing.GitHubAppSlug = s.githubConfigured, s.githubSlug
	sharing.Egress = registry
	registry.Sharing = sharing
	if *claudeAuth != "" {
		registry.ClaudeSource = &policy.ClaudeCredentials{Path: *claudeAuth}
	}
	if *codexAuth != "" {
		registry.ProviderSource = &policy.CodexCredentials{Path: *codexAuth}
	}
	if k8s != nil {
		k8s.credentials(registry)
	}
	// Gateway rules are always managed; --manage-network is accepted and ignored.
	managed := *sbx != "" || k8s != nil
	serviceCtx, stopService := context.WithCancel(context.Background())
	defer stopService()
	if managed {
		if exe, err := os.Executable(); err == nil {
			policy.LimitedGitLauncher = []string{exe, "policy", "git-limited"}
		}
		if k8s != nil {
			// The trust bundle precedes the control listener (decision 9);
			// the gateway precedes the inspector, whose canaries dial it.
			if err := k8s.publishTrust(serviceCtx, ca); err != nil {
				return err
			}
			if err := k8s.enforce(serviceCtx, registry, networks); err != nil {
				return err
			}
			defer k8s.stop()
		} else if err := enforcement(s, registry, networks); err != nil {
			return err
		}
	}
	server, err := policy.ListenControl(s.listen, s.tls, registry)
	if err != nil {
		return errors.New("control socket: " + err.Error())
	}
	switch {
	case k8s != nil:
		fmt.Printf("policy control ready on %s; gateway %s:%d; readiness requires the cluster proof and verified pods\n", s.listen, k8s.gatewayHost, s.kubernetes.GatewayPort)
	case managed:
		fmt.Println("SBX control ready; readiness requires verified gateway policy")
	default:
		fmt.Println("SBX control ready; runtime enforcement unsupported (fail closed)")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	server.Close()
	if listen.Scheme == transport.SchemeUnix {
		_ = os.Remove(listen.Path)
	}
	registry.Close()
	return nil
}

// supportedKind refuses a runtime kind this build has no inspector, gateway
// or credential store for.
func supportedKind(kind string) error {
	switch kind {
	case config.RuntimeSBX, config.RuntimeKubernetes:
		return nil
	}
	return errors.New("unknown runtime.kind " + kind)
}

// enforcement wires the sbx shapes' gateway and verifier into the
// registry: per-binding loopback gateways and the SBX inspector over the
// pinned sbx executable. The kubernetes kind attaches through
// kubernetesRuntime.enforce.
func enforcement(s settings, registry *policy.Registry, networks []*net.IPNet) error {
	if s.cfg.GatewayMode() != config.GatewayLoopback {
		return errors.New("gateway mode " + s.cfg.GatewayMode() + " is not wired for runtime.kind " + s.cfg.RuntimeKind())
	}
	registry.Gateways = policy.NewLoopbackGateways(registry, networks)
	verifier, err := policy.NewSbxCliVerifier(registry, s.sbx, true, nil)
	if err != nil {
		return errors.New("verifier: " + err.Error())
	}
	if !policy.ValidImageDigest(s.guestDigest) {
		return errors.New("--guest-image-digest must be sha256:<64 hex>")
	}
	verifier.Inspector.(*policy.SbxInspector).ShellDigest = s.guestDigest
	registry.Verifier = verifier
	verifier.StartRefresher()
	return nil
}

// rotateGatewayCA replaces the host gateway CA on demand. It needs the state
// directory's service lock, so the policy service must be stopped first.
func rotateGatewayCA(args []string) error {
	fs := flag.NewFlagSet("rotate-gateway-ca", flag.ContinueOnError)
	state := fs.String("state", "", "private state directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *state == "" {
		return fmt.Errorf("--state is required")
	}
	syscall.Umask(0o077)
	lock, err := os.OpenFile(filepath.Join(*state, "service.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("the policy service is running; stop it before rotating the gateway CA")
	}
	ca, err := policy.RotateGatewayCA(*state, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("gateway CA rotated; fingerprint %s; start the policy service, guests reinstall the CA on their next run\n", ca.Fingerprint())
	return nil
}

func selfcheck(args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	vendorDir := fs.String("vendor-dir", defaultPath("vendor"), "")
	template := fs.String("policy-template", defaultPath(filepath.Join("config", "policy.template.json")), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	operations, err := policy.LoadOperations(filepath.Join(*vendorDir, "github-operations.json"))
	if err != nil {
		return err
	}
	if _, err = policy.LoadGitHubNetworks(filepath.Join(*vendorDir, "github-meta.json")); err != nil {
		return err
	}
	state, err := os.MkdirTemp("", "warden-selfcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	engine, err := policy.NewEngine(state, policy.EngineOptions{Operations: operations, PolicyTemplate: *template})
	if err != nil {
		return err
	}
	engine.Close()
	if _, _, err = policy.PrepareGatewayCA(state, policy.DefaultGatewayCAMaxAge, time.Now()); err != nil {
		return err
	}
	return nil
}

// defaultPath prefers the container layout (/app), then the release layout
// (vendor/ and config/ beside the bin/ directory holding the executable),
// then the repository checkout relative to the executable's grandparent
// (dist/chat/warden).
func defaultPath(relative string) string {
	if _, err := os.Stat(filepath.Join("/app", relative)); err == nil {
		return filepath.Join("/app", relative)
	}
	if exe, err := os.Executable(); err == nil {
		bin := filepath.Dir(exe)
		for _, root := range []string{filepath.Dir(bin), filepath.Dir(filepath.Dir(bin))} {
			if _, err := os.Stat(filepath.Join(root, relative)); err == nil {
				return filepath.Join(root, relative)
			}
		}
	}
	return relative
}
