// Command warden-policy is the private SBX control and credential broker:
// registry, per-sandbox policy engines, SBX verifier, inspected loopback
// gateways, sharing services and the GitHub App broker subcommand.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"warden/chat/internal/handshake"
	"warden/chat/internal/policy"
)

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "github-broker":
			os.Exit(policy.RunGitHubBroker(os.Args[2:], os.Stdin, os.Stdout))
		case "git-limited":
			if err := policy.RunLimitedGit(os.Args[2:]); err != nil {
				fail(err.Error())
			}
			return
		case "rotate-gateway-ca":
			if err := rotateGatewayCA(os.Args[2:]); err != nil {
				fail("rotate-gateway-ca: " + err.Error())
			}
			return
		case "selfcheck":
			// Packaging smoke check: the catalog, GitHub networks, policy
			// template and an engine must all load from the installed layout.
			if err := selfcheck(os.Args[2:]); err != nil {
				fail("selfcheck: " + err.Error())
			}
			fmt.Println("warden-policy selfcheck ok")
			return
		}
	}
	configPath := flag.String("config", "", "optional warden.json (default $WARDEN_CONFIG); flags override its computed defaults and must agree with its values")
	state := flag.String("state", "", "private state directory (paths.state/policy)")
	sbx := flag.String("sbx", "", "explicit trusted SBX executable; enables pinned CLI verifier")
	mitmdump := flag.String("mitmdump", "", "ignored; gateways run in-process (kept for launcher compatibility)")
	manageNetwork := flag.Bool("manage-network", false, "ignored; sandbox gateway rules are always managed (kept for launcher compatibility)")
	googleConfig := flag.String("google-config", "", "private Google OAuth client configuration for file sharing")
	claudeAuth := flag.String("claude-auth-file", "", "private host Claude credential file")
	codexAuth := flag.String("codex-auth-file", "", "private host Codex login, read without copying or refreshing")
	documentOrigin := flag.String("document-api-origin", "", "fixed local app HTTP origin, e.g. http://127.0.0.1:8080")
	documentKey := flag.String("document-api-key-file", "", "private app/Warden shared signing key; never sent to sandbox")
	vendorDir := flag.String("vendor-dir", defaultPath("vendor"), "directory holding github-operations.json and github-meta.json")
	template := flag.String("policy-template", defaultPath(filepath.Join("config", "policy.template.json")), "policy template for new sandboxes")
	caMaxAge := flag.Duration("gateway-ca-max-age", policy.DefaultGatewayCAMaxAge, "rotate the host gateway CA at startup when older than this (0 disables)")
	guestDigest := flag.String("guest-image-digest", policy.SBXShellDigest, "digest of the only guest image a verified runtime may run (sha256:…)")
	version := flag.Bool("version", false, "print the build revision and protocol number")
	githubAuthFile := flag.String("github-auth-file", "", "private user OAuth token file written by warden login github; exclusive with WARDEN_GITHUB_APP_BROKER")
	chatListen := flag.String("chat-listen", "127.0.0.1:18780", "chat listen address; its port is the loopback redirect of the built-in Google Docs client")
	flag.Parse()
	if *version {
		fmt.Println(handshake.Self("warden-policy"))
		return
	}
	s, err := resolveSettings(flag.CommandLine, policyFlags{configPath: configPath, state: state, sbx: sbx, googleConfig: googleConfig, claudeAuth: claudeAuth, codexAuth: codexAuth, vendorDir: vendorDir, template: template, guestDigest: guestDigest, caMaxAge: caMaxAge, githubAuthFile: githubAuthFile, chatListen: chatListen})
	if err != nil {
		fail(err.Error())
	}
	if *manageNetwork && s.sbx == "" {
		fail("managed mode requires --sbx")
	}
	state, sbx, googleConfig, claudeAuth, codexAuth, vendorDir, template, guestDigest, caMaxAge = &s.state, &s.sbx, &s.googleConfig, &s.claudeAuth, &s.codexAuth, &s.vendorDir, &s.template, &s.guestDigest, &s.caMaxAge
	if (*documentOrigin == "") != (*documentKey == "") {
		fail("document API origin and key file must be configured together")
	}
	_ = mitmdump
	syscall.Umask(0o077)
	if err := os.MkdirAll(*state, 0o700); err != nil {
		fail(err.Error())
	}
	info, err := os.Lstat(*state)
	if err != nil {
		fail(err.Error())
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || stat == nil || int(stat.Uid) != os.Getuid() {
		fail("state must be an owned private directory")
	}
	if err = os.Chmod(*state, 0o700); err != nil {
		fail(err.Error())
	}
	lock, err := os.OpenFile(filepath.Join(*state, "service.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		fail(err.Error())
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fail("another Warden policy service holds this state directory")
	}
	defer lock.Close()
	socketPath := filepath.Join(*state, "sbx-control.sock")
	if len(socketPath) > 100 {
		fail("state path too long for private Unix socket")
	}
	operations, err := policy.LoadOperations(filepath.Join(*vendorDir, "github-operations.json"))
	if err != nil {
		fail("operation catalog: " + err.Error())
	}
	networks, err := policy.LoadGitHubNetworks(filepath.Join(*vendorDir, "github-meta.json"))
	if err != nil {
		fail("GitHub networks: " + err.Error())
	}
	if _, err = os.Stat(*template); err != nil {
		fail("policy template: " + err.Error())
	}
	var documentAPI *policy.DocumentAPI
	if *documentOrigin != "" {
		documentAPI, err = policy.NewDocumentAPI(*documentOrigin, *documentKey)
		if err != nil {
			fail(err.Error())
		}
	}
	ca, rotated, err := policy.PrepareGatewayCA(*state, *caMaxAge, time.Now())
	if err != nil {
		fail("gateway CA: " + err.Error())
	}
	if rotated {
		fmt.Printf("gateway CA rotated (older than %s); guests reinstall it on their next run; fingerprint %s\n", *caMaxAge, ca.Fingerprint())
	}
	github, err := s.github()
	if err != nil {
		fail(err.Error())
	}
	options := policy.RegistryOptions{Operations: operations, PolicyTemplate: *template, GitHubAppConfig: s.githubBroker, GitHubAuthFile: s.githubAuthFile, DocumentAPI: documentAPI, Networks: networks, CA: ca}
	registry, err := policy.NewRegistry(*state, options)
	if err != nil {
		fail("registry: " + err.Error())
	}
	google, err := s.google()
	if err != nil {
		fail("Google connection: " + err.Error())
	}
	defer google.Close()
	sharing, err := policy.NewSharing(*state, s.googleSharing(google), nil, github)
	if err != nil {
		fail("sharing: " + err.Error())
	}
	defer sharing.Close()
	sharing.GitHubConfigured, sharing.GitHubAppSlug = s.githubConfigured, s.githubSlug
	registry.Sharing = sharing
	if *claudeAuth != "" {
		registry.ClaudeSource = &policy.ClaudeCredentials{Path: *claudeAuth}
	}
	if *codexAuth != "" {
		registry.ProviderSource = &policy.CodexCredentials{Path: *codexAuth}
	}
	// Gateway rules are always managed; --manage-network is accepted and ignored.
	managed := *sbx != ""
	if managed {
		if exe, err := os.Executable(); err == nil {
			policy.LimitedGitLauncher = []string{exe, "git-limited"}
		}
		registry.GatewayPool = policy.NewGatewayPool(registry, networks)
		verifier, err := policy.NewSbxCliVerifier(registry, *sbx, true, nil)
		if err != nil {
			fail("verifier: " + err.Error())
		}
		if !policy.ValidImageDigest(*guestDigest) {
			fail("--guest-image-digest must be sha256:<64 hex>")
		}
		verifier.ShellDigest = *guestDigest
		registry.Verifier = verifier
		verifier.StartRefresher()
	}
	server, err := policy.ListenControl(socketPath, registry)
	if err != nil {
		fail("control socket: " + err.Error())
	}
	if managed {
		fmt.Println("SBX control ready; readiness requires verified gateway policy")
	} else {
		fmt.Println("SBX control ready; runtime enforcement unsupported (fail closed)")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	server.Close()
	_ = os.Remove(socketPath)
	registry.Close()
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
// (dist/chat/warden-policy).
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
