package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValidLocalMode(t *testing.T) {
	c := Defaults("/tmp/w")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Previews.Mode != PreviewLoopback || c.Auth.Mode != AuthOwner || c.PreviewScheme() != "http" {
		t.Fatalf("unexpected local defaults: %+v", c)
	}
	if c.GitHubMode() != "user" || c.Providers.Google.DocsClient != BuiltinGoogleClient {
		t.Fatalf("unexpected provider defaults: %+v", c.Providers)
	}
	if c.PolicySocket() != filepath.Join("/tmp/w", "policy", "sbx-control.sock") {
		t.Fatal(c.PolicySocket())
	}
}

func TestParseMergesAndRejectsUnknownFields(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"maxRunning":4}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandboxes.MaxRunning != 4 || c.Sandboxes.MemoryMB != 1536 {
		t.Fatalf("merge failed: %+v", c.Sandboxes)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"bogus":1}`)); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if _, err = Parse([]byte(`{"version":2,"paths":{"state":"/tmp/w"}}`)); err == nil {
		t.Fatal("wrong version accepted")
	}
}

func TestServerModeRules(t *testing.T) {
	server := `{"version":1,"paths":{"state":"/var/lib/warden"},
	 "previews":{"mode":"public","hostSuffix":"preview.example.com","edgeListen":"172.18.0.1:19081"},
	 "auth":{"mode":"google","publicURL":"https://warden.example.com","google":{"signInClientID":"x.apps.googleusercontent.com","owners":["o@example.com"]}},
	 "providers":{"github":{"appID":5,"appSlug":"s","installationOwner":"org","brokerFile":"/var/lib/warden/github/broker.json"}}}`
	c, err := Parse([]byte(server))
	if err != nil {
		t.Fatal(err)
	}
	if c.PreviewScheme() != "https" || c.GitHubMode() != "app" || c.Auth.Google.SignInLedger == "" {
		t.Fatalf("server parse: %+v", c)
	}
	bad := []string{
		// public previews without google auth
		`{"version":1,"paths":{"state":"/tmp/w"},"previews":{"mode":"public","hostSuffix":"p.example.com","edgeListen":"127.0.0.1:1"}}`,
		// loopback previews with a real suffix
		`{"version":1,"paths":{"state":"/tmp/w"},"previews":{"hostSuffix":"p.example.com"}}`,
		// google auth without the block
		`{"version":1,"paths":{"state":"/tmp/w"},"auth":{"mode":"google","publicURL":"https://w.example.com"}}`,
		// github both user and app
		`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"github":{"authFile":"/tmp/t","appID":1}}}`,
		// relative state
		`{"version":1,"paths":{"state":"w"}}`,
		// a private home whose sbx sockets exceed sun_path
		`{"version":1,"paths":{"state":"/tmp/w"},"sbx":{"privateHome":"/Users/danielporter/Library/Application Support/Warden/sbx"}}`,
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config: %s", raw)
		}
	}
}

func TestProvidersCanBeRemoved(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"github":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	// JSON null removes the provider so the UI hides it; an omitted section
	// keeps the local default.
	if c.GitHubMode() != "" || c.Providers.GitHub != nil || c.Providers.Google == nil || c.Providers.Codex == nil {
		t.Fatalf("null did not remove the provider: %+v", c.Providers)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"google":null,"codex":{"authFile":"/x/auth.json"}}}`))
	if err != nil || c.Providers.Google != nil || c.Providers.Codex.AuthFile != "/x/auth.json" || c.GitHubMode() != "user" {
		t.Fatalf("%+v %v", c.Providers, err)
	}
}

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// macOS temp dirs exceed the Unix socket path limit; the state root is
	// only recorded, never created, by this test.
	c := Defaults("/tmp/w")
	path := filepath.Join(dir, "warden.json")
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Paths.State != "/tmp/w" || loaded.Chat.Listen != c.Chat.Listen {
		t.Fatalf("round trip: %+v", loaded)
	}
	if _, err = Load(path, "/elsewhere"); err == nil {
		t.Fatal("disagreeing --state accepted")
	}
}

func TestSandboxEgressModes(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"}}`))
	if err != nil || c.Sandboxes.Egress != EgressRestricted {
		t.Fatalf("default egress: %q %v", c.Sandboxes.Egress, err)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"egress":"open"}}`))
	if err != nil || c.Sandboxes.Egress != EgressOpen {
		t.Fatalf("open egress: %q %v", c.Sandboxes.Egress, err)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"egress":"everything"}}`)); err == nil || !strings.Contains(err.Error(), "sandboxes.egress") {
		t.Fatalf("bad egress accepted: %v", err)
	}
}

// A file without services or tls resolves to the Unix sockets and the
// loopback chat the services used before those sections existed, and a
// written file does not gain them.
func TestTransportDefaultsAreTodaysSocketsAndLoopbackChat(t *testing.T) {
	for _, raw := range []string{`{"version":1,"paths":{"state":"/tmp/w"}}`, `{"version":1,"paths":{"state":"/tmp/w"},"chat":{"listen":"127.0.0.1:19000"}}`} {
		c, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if c.PolicyListen() != "unix:///tmp/w/policy/sbx-control.sock" || c.PolicyAddress() != c.PolicyListen() || c.RunnerListen() != "unix:///tmp/w/runner/worker.sock" || c.RunnerAddress() != c.RunnerListen() {
			t.Fatalf("%s: %s %s %s %s", raw, c.PolicyListen(), c.PolicyAddress(), c.RunnerListen(), c.RunnerAddress())
		}
		if c.ChatListen() != "http://"+c.Chat.Listen || c.ChatAddress() != c.ChatListen() || c.UsesTLS() || c.TransportTLS() != nil {
			t.Fatalf("%s: %s %s", raw, c.ChatListen(), c.ChatAddress())
		}
		if HostOf(c.ChatAddress()) != c.Chat.Listen {
			t.Fatal(HostOf(c.ChatAddress()))
		}
	}
	c := Defaults("/tmp/w")
	if c.PolicyListen() != "unix://"+c.PolicySocket() || c.RunnerAddress() != "unix://"+c.RunnerSocket() || c.ChatListen() != "http://127.0.0.1:18780" {
		t.Fatalf("%s %s %s", c.PolicyListen(), c.RunnerAddress(), c.ChatListen())
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "services") || strings.Contains(string(b), "\"tls\"") {
		t.Fatalf("defaults written explicitly:\n%s", b)
	}
}

func TestTransportURLsAndTLSValidation(t *testing.T) {
	k8s := `{"version":1,"paths":{"state":"/var/lib/warden"},
	 "services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},
	             "runner":{"listen":"tls://:7444","address":"tls://warden-runner:7444"},
	             "chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
	 "tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}}`
	c, err := Parse([]byte(k8s))
	if err != nil {
		t.Fatal(err)
	}
	if !c.UsesTLS() || c.PolicyListen() != "tls://0.0.0.0:7443" || c.PolicyAddress() != "tls://warden-policy:7443" || c.RunnerListen() != "tls://:7444" || c.ChatAddress() != "tls://warden-chat:7445" {
		t.Fatalf("%+v", c.Services)
	}
	if tl := c.TransportTLS(); tl == nil || tl.CAFile != "/etc/warden/tls/ca.crt" || tl.CertFile != "/etc/warden/tls/tls.crt" || tl.KeyFile != "/etc/warden/tls/tls.key" || tl.ServerName != "" {
		t.Fatalf("%+v", tl)
	}
	// chat.listen keeps its loopback default beside a tls:// chat listener.
	if c.Chat.Listen != "127.0.0.1:18780" || HostOf(c.ChatAddress()) != "warden-chat:7445" {
		t.Fatal(c.Chat.Listen, HostOf(c.ChatAddress()))
	}
	// Explicit unix:// URLs and a loopback http:// chat listener that sets
	// chat.listen for the policy service's redirect.
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"unix:///run/warden/policy.sock"},"runner":{"listen":"unix:///run/warden/runner.sock","address":"unix:///mnt/runner/runner.sock"},"chat":{"listen":"http://127.0.0.1:19000"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.PolicyAddress() != "unix:///run/warden/policy.sock" || c.RunnerAddress() != "unix:///mnt/runner/runner.sock" || c.Chat.Listen != "127.0.0.1:19000" || c.ChatAddress() != "http://127.0.0.1:19000" || c.UsesTLS() {
		t.Fatalf("%+v %s", c.Services, c.Chat.Listen)
	}
	// A tls section beside Unix sockets is allowed and unused.
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"tls without the section":        `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://p:7443"}}}`,
		"tls with a relative key":        `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://p:7443"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"c"}}`,
		"tls listener without address":   `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"tls://0.0.0.0:7444"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`,
		"address without a host":         `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://:7444"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`,
		"mixed schemes":                  `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"unix:///tmp/p.sock","address":"tls://p:7443"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`,
		"http for the policy service":    `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"http://127.0.0.1:1"}}}`,
		"unix for the chat":              `{"version":1,"paths":{"state":"/tmp/w"},"services":{"chat":{"listen":"unix:///tmp/c.sock"}}}`,
		"non-loopback chat":              `{"version":1,"paths":{"state":"/tmp/w"},"services":{"chat":{"listen":"http://10.0.0.1:18780"}}}`,
		"chat listen and chat.listen":    `{"version":1,"paths":{"state":"/tmp/w"},"chat":{"listen":"127.0.0.1:18780"},"services":{"chat":{"listen":"http://127.0.0.1:19000"}}}`,
		"relative socket":                `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"unix://worker.sock"}}}`,
		"socket path too long":           `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"unix:///` + strings.Repeat("d/", 60) + `w.sock"}}}`,
		"port out of range":              `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"tls://0.0.0.0:70000","address":"tls://p:70000"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`,
		"path in a tls URL":              `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"listen":"tls://0.0.0.0:7443/x","address":"tls://p:7443"}},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c"}}`,
		"unknown service field":          `{"version":1,"paths":{"state":"/tmp/w"},"services":{"policy":{"socket":"/x"}}}`,
		"unknown tls field (serverName)": `{"version":1,"paths":{"state":"/tmp/w"},"tls":{"caFile":"/a","certFile":"/b","keyFile":"/c","serverName":"x"}}`,
	}
	for name, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

func TestRuntimeKindDefaultsToSBXAndIsNotWritten(t *testing.T) {
	c := Defaults("/tmp/w")
	if c.RuntimeKind() != RuntimeSBX || c.GatewayMode() != GatewayLoopback || c.Kubernetes != nil {
		t.Fatalf("defaults: kind=%q gateway=%q", c.RuntimeKind(), c.GatewayMode())
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"runtime"`) || strings.Contains(string(raw), `"kubernetes"`) || strings.Contains(string(raw), `"secret"`) {
		t.Fatalf("sbx file mentions the other kind: %s", raw)
	}
	parsed, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"runtime":{"kind":"sbx"}}`))
	if err != nil || parsed.RuntimeKind() != RuntimeSBX {
		t.Fatalf("explicit sbx: %v", err)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"runtime":{"kind":"docker"}}`)); err == nil || !strings.Contains(err.Error(), "runtime.kind") {
		t.Fatalf("unknown kind accepted: %v", err)
	}
}

const kubernetesExample = `{
  "version": 1,
  "runtime": { "kind": "kubernetes" },
  "paths": { "state": "/var/lib/warden" },
  "services": {
    "policy": { "listen": "tls://0.0.0.0:7443", "address": "tls://warden-policy:7443" },
    "runner": { "listen": "tls://0.0.0.0:7444", "address": "tls://warden-runner:7444" },
    "chat":   { "listen": "tls://0.0.0.0:7445", "address": "tls://warden-chat:7445" }
  },
  "tls": { "caFile": "/etc/warden/tls/ca.crt", "certFile": "/etc/warden/tls/tls.crt", "keyFile": "/etc/warden/tls/tls.key" },
  "kubernetes": {
    "namespace": "warden-sandboxes",
    "tier": "gvisor",
    "runtimeClass": "gvisor",
    "guestImage": "ghcr.io/monaddle-too/warden-guest-base",
    "guestImageDigest": "sha256:` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `"
  },
  "previews": { "edgeListen": "0.0.0.0:19081" },
  "providers": {
    "codex":  { "secret": "warden-codex-login" },
    "claude": { "secret": "warden-claude-login" },
    "github": { "secret": "warden-github-login" }
  }
}`

func TestKubernetesKindParsesWithDefaults(t *testing.T) {
	c, err := Parse([]byte(kubernetesExample))
	if err != nil {
		t.Fatal(err)
	}
	k := c.Kubernetes
	if c.RuntimeKind() != RuntimeKubernetes || c.GatewayMode() != GatewayShared || k == nil {
		t.Fatalf("kind: %+v", c)
	}
	if k.WorkspaceSizeGi != 20 || k.GatewayService != "warden-gateway" || k.TrustConfigMap != "warden-guest-trust" || k.StorageClass != "" || k.Tier != TierGVisor {
		t.Fatalf("kubernetes defaults: %+v", k)
	}
	if c.Providers.Codex.Secret != "warden-codex-login" || c.Providers.Codex.AuthFile != "" || c.GitHubMode() != "user" || c.Providers.GitHub.Secret != "warden-github-login" {
		t.Fatalf("providers: %+v %+v", c.Providers.Codex, c.Providers.GitHub)
	}
	if !c.UsesTLS() || c.PolicyAddress() != "tls://warden-policy:7443" {
		t.Fatalf("transport: %s", c.PolicyAddress())
	}
	// Providers the file does not name are absent, not the sbx file defaults.
	minimal := strings.Replace(kubernetesExample, `"providers": {
    "codex":  { "secret": "warden-codex-login" },
    "claude": { "secret": "warden-claude-login" },
    "github": { "secret": "warden-github-login" }
  }`, `"providers": {}`, 1)
	c, err = Parse([]byte(minimal))
	if err != nil || c.Providers.Codex != nil || c.Providers.Claude != nil || c.Providers.GitHub != nil || c.Providers.Google == nil {
		t.Fatalf("minimal kubernetes providers: %+v %v", c.Providers, err)
	}
}

func TestValidationByRuntimeKind(t *testing.T) {
	sbx := `{"version":1,"paths":{"state":"/tmp/w"}`
	bad := map[string]string{
		"kubernetes section with kind sbx":   sbx + `,"kubernetes":{"namespace":"n","tier":"kata","runtimeClass":"kata","guestImage":"i","guestImageDigest":"sha256:` + strings.Repeat("a", 64) + `"}}`,
		"secret with kind sbx":               sbx + `,"providers":{"codex":{"secret":"warden-codex-login"}}}`,
		"github secret with kind sbx":        sbx + `,"providers":{"github":{"secret":"warden-github-login"}}}`,
		"kubernetes without its section":     strings.Replace(kubernetesExample, `"kubernetes": {`, `"kubernetesX": {`, 1),
		"sbx section with kind kubernetes":   strings.Replace(kubernetesExample, `"previews"`, `"sbx": {"executable": "/usr/local/bin/sbx"}, "previews"`, 1),
		"authFile with kind kubernetes":      strings.Replace(kubernetesExample, `{ "secret": "warden-codex-login" }`, `{ "authFile": "/var/lib/warden/provider/auth.json" }`, 1),
		"unknown tier":                       strings.Replace(kubernetesExample, `"tier": "gvisor"`, `"tier": "runc"`, 1),
		"bad namespace":                      strings.Replace(kubernetesExample, `"warden-sandboxes"`, `"Warden Sandboxes"`, 1),
		"digest in guestImage":               strings.Replace(kubernetesExample, `"ghcr.io/monaddle-too/warden-guest-base"`, `"ghcr.io/monaddle-too/warden-guest-base@sha256:x"`, 1),
		"bad digest":                         strings.Replace(kubernetesExample, `"guestImageDigest": "sha256:`, `"guestImageDigest": "sha512:`, 1),
		"zero workspace":                     strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "workspaceSizeGi": -1,`, 1),
		"github secret and authFile":         strings.Replace(kubernetesExample, `{ "secret": "warden-github-login" }`, `{ "secret": "warden-github-login", "authFile": "/x" }`, 1),
		"loopback edge listen with kind sbx": sbx + `,"previews":{"edgeListen":"0.0.0.0:19081"}}`,
	}
	for name, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The kubernetes section's own unknown fields are refused like any other.
	if _, err := Parse([]byte(strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "nodeSelector": "x",`, 1))); err == nil || !strings.Contains(err.Error(), "nodeSelector") {
		t.Fatalf("unknown kubernetes field accepted: %v", err)
	}
	// Loopback-only rules apply to the sbx kind only: a long state path is
	// fine when no Unix socket is bound there, and the edge may bind every
	// interface behind a port-forward.
	long := "/var/lib/" + strings.Repeat("warden-state-directory/", 5) + "warden"
	c, err := Parse([]byte(strings.Replace(kubernetesExample, `"/var/lib/warden"`, `"`+long+`"`, 1)))
	if err != nil || c.Paths.State != long {
		t.Fatalf("long state on kubernetes: %v", err)
	}
	if _, err = Parse([]byte(sbx + `}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"` + long + `"}}`)); err == nil {
		t.Fatal("long state accepted for the sbx kind")
	}
}
