package policysvc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/policy"
	"warden/chat/internal/release"
)

func policySettings(t *testing.T, args ...string) (settings, error) {
	t.Helper()
	fs := flag.NewFlagSet("warden-policy", flag.ContinueOnError)
	f := policyFlags{
		configPath: fs.String("config", "", ""), state: fs.String("state", "", ""), sbx: fs.String("sbx", "", ""),
		googleConfig: fs.String("google-config", "", ""), claudeAuth: fs.String("claude-auth-file", "", ""), codexAuth: fs.String("codex-auth-file", "", ""),
		vendorDir: fs.String("vendor-dir", "vendor", ""), template: fs.String("policy-template", "config/policy.template.json", ""),
		guestDigest: fs.String("guest-image-digest", policy.SBXShellDigest, ""), caMaxAge: fs.Duration("gateway-ca-max-age", policy.DefaultGatewayCAMaxAge, ""),
		githubAuthFile: fs.String("github-auth-file", "", ""), chatListen: fs.String("chat-listen", "127.0.0.1:18780", ""), egress: fs.String("egress", "restricted", ""),
	}
	fs.Bool("manage-network", false, "")
	fs.String("mitmdump", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return resolveSettings(fs, f)
}

// The OVH compose command line, without a file, must produce exactly today's values.
func TestOVHFlagsReproduceCurrentBehaviour(t *testing.T) {
	t.Setenv(config.Env, "")
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "/run/github/broker.json")
	s, err := policySettings(t, "--state", "/state", "--sbx", "/usr/local/bin/sbx", "--manage-network", "--codex-auth-file", "/run/provider/auth.json", "--claude-auth-file", "/run/provider/claude.json", "--google-config", "/run/provider/google.json", "--guest-image-digest", release.StockTemplateDigest)
	if err != nil {
		t.Fatal(err)
	}
	if s.state != "/state" || s.listen != "unix:///state/sbx-control.sock" || s.tls != nil || s.sbx != "/usr/local/bin/sbx" || s.codexAuth != "/run/provider/auth.json" || s.claudeAuth != "/run/provider/claude.json" || s.googleConfig != "/run/provider/google.json" || s.guestDigest != release.StockTemplateDigest {
		t.Fatalf("%+v", s)
	}
	if s.caMaxAge != policy.DefaultGatewayCAMaxAge || s.vendorDir != defaultPath("vendor") || s.template != defaultPath("config/policy.template.json") {
		t.Fatalf("%+v", s)
	}
	if s.githubBroker != "/run/github/broker.json" || s.githubSlug != policy.DefaultGitHubAppSlug || !s.githubConfigured || !s.googleConfigured {
		t.Fatalf("%+v", s)
	}
}

func TestStateAloneRunsWithComputedDefaults(t *testing.T) {
	t.Setenv(config.Env, "")
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "")
	s, err := policySettings(t, "--state", "/tmp/w/policy")
	if err != nil {
		t.Fatal(err)
	}
	if s.state != "/tmp/w/policy" || s.listen != "unix:///tmp/w/policy/sbx-control.sock" || s.tls != nil || s.cfg.Paths.State != "/tmp/w" || s.codexAuth != "/tmp/w/provider/auth.json" || s.claudeAuth != "/tmp/w/provider/claude.json" {
		t.Fatalf("%+v", s)
	}
	if s.googleConfig != "" || !s.googleConfigured || s.githubBroker != "" || !s.githubConfigured || s.githubSlug != "" || s.sbx != "" {
		t.Fatalf("%+v", s)
	}
	if s.guestDigest != release.StockTemplateDigest || s.caMaxAge != 365*24*time.Hour {
		t.Fatalf("%+v", s)
	}
	if _, err = policySettings(t); err == nil {
		t.Fatal("no state and no file accepted")
	}
}

func TestFileAndFlagsMustAgree(t *testing.T) {
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "")
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/tmp/w"},"sbx":{"executable":"/opt/homebrew/bin/sbx"},
		"providers":{"google":null,"github":{"appID":123,"appSlug":"example-app","installationOwner":"owner","brokerFile":"/tmp/w/github/broker.json"}}}`), 0600)
	s, err := policySettings(t, "--config", path, "--sbx", "/opt/homebrew/bin/sbx", "--state", "/tmp/w/policy")
	if err != nil {
		t.Fatal(err)
	}
	if s.googleConfigured || s.googleConfig != "" || !s.githubConfigured || s.githubSlug != "example-app" || s.githubBroker != "/tmp/w/github/broker.json" || s.sbx != "/opt/homebrew/bin/sbx" {
		t.Fatalf("%+v", s)
	}
	_, err = policySettings(t, "--config", path, "--sbx", "/usr/bin/sbx")
	if err == nil || !strings.Contains(err.Error(), "--sbx") || !strings.Contains(err.Error(), "sbx.executable") {
		t.Fatal("disagreement accepted", err)
	}
	if _, err = policySettings(t, "--config", path, "--state", "/elsewhere"); err == nil || !strings.Contains(err.Error(), "paths.state") {
		t.Fatal("state disagreement accepted", err)
	}
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "/run/github/other.json")
	if _, err = policySettings(t, "--config", path); err == nil || !strings.Contains(err.Error(), "brokerFile") {
		t.Fatal("broker disagreement accepted", err)
	}
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "")
	t.Setenv(config.Env, path)
	if s, err = policySettings(t, "--codex-auth-file", "/tmp/w/provider/auth.json"); err != nil || s.githubSlug != "example-app" {
		t.Fatal("$WARDEN_CONFIG ignored", err)
	}
}

// deploy/chat/compose.yaml mounts the host directories at the same paths
// inside the containers and passes --config warden.example.json; the
// resolved values must equal the flag-configured deployment's
// (TestOVHFlagsReproduceCurrentBehaviour) under those paths.
func TestOVHExampleFileMatchesTheComposeDeployment(t *testing.T) {
	t.Setenv(config.Env, "")
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "/var/lib/warden/github/broker.json")
	s, err := policySettings(t, "--config", filepath.Join("..", "..", "..", "..", "deploy", "chat", "warden.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.state != "/var/lib/warden/policy" || s.listen != "unix:///var/lib/warden/policy/sbx-control.sock" || s.tls != nil || s.sbx != "/usr/bin/sbx" || s.codexAuth != "/var/lib/warden/provider/auth.json" || s.claudeAuth != "/var/lib/warden/provider/claude.json" || s.googleConfig != "/var/lib/warden/provider/google.json" || s.guestDigest != release.StockTemplateDigest {
		t.Fatalf("%+v", s)
	}
	if s.caMaxAge != policy.DefaultGatewayCAMaxAge || s.vendorDir != "/app/vendor" || s.template != "/app/config/policy.template.json" || s.chatListen != "127.0.0.1:18780" {
		t.Fatalf("%+v", s)
	}
	if s.githubBroker != "/var/lib/warden/github/broker.json" || s.githubSlug != policy.DefaultGitHubAppSlug || s.githubAuthFile != "" || !s.githubConfigured || !s.googleConfigured {
		t.Fatalf("%+v", s)
	}
	// The compose file's WARDEN_GITHUB_APP_BROKER must name the same file.
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "/run/github/broker.json")
	if _, err = policySettings(t, "--config", filepath.Join("..", "..", "..", "..", "deploy", "chat", "warden.example.json")); err == nil || !strings.Contains(err.Error(), "brokerFile") {
		t.Fatal("broker path disagreement accepted", err)
	}
}

// A file with tls:// services gives the policy service its mutual-TLS
// listener and material; the legacy --state flag still names the
// directory beside it.
func TestPolicyTransportSettings(t *testing.T) {
	t.Setenv(config.Env, "")
	t.Setenv("WARDEN_GITHUB_APP_BROKER", "")
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/var/lib/warden"},
		"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444"},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
		"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}}`), 0600)
	s, err := policySettings(t, "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	if s.listen != "tls://0.0.0.0:7443" || s.state != "/var/lib/warden/policy" || s.tls == nil || s.tls.CAFile != "/etc/warden/tls/ca.crt" || s.tls.CertFile != "/etc/warden/tls/tls.crt" || s.tls.KeyFile != "/etc/warden/tls/tls.key" {
		t.Fatalf("%+v %+v", s, s.tls)
	}
	if s, err = policySettings(t, "--config", path, "--state", "/var/lib/warden/policy"); err != nil || s.listen != "tls://0.0.0.0:7443" {
		t.Fatalf("%+v %v", s, err)
	}
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"unix:///run/warden/policy.sock"}}}`), 0600)
	if s, err = policySettings(t, "--config", path); err != nil || s.listen != "unix:///run/warden/policy.sock" || s.state != "/tmp/w/policy" || s.tls != nil {
		t.Fatalf("%+v %v", s, err)
	}
}
