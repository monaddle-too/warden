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
	// The Claude session features are off unless the file turns them on;
	// the login is validated like Codex's.
	if c.Providers.Claude == nil || c.Providers.Claude.AllowFastMode || c.Providers.Claude.AllowLongContext {
		t.Fatalf("%+v", c.Providers.Claude)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"claude":{"authFile":"/x/claude.json","allowFastMode":true,"allowLongContext":true}}}`))
	if err != nil || c.Providers.Claude.AuthFile != "/x/claude.json" || !c.Providers.Claude.AllowFastMode || !c.Providers.Claude.AllowLongContext {
		t.Fatalf("%+v %v", c.Providers.Claude, err)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"providers":{"claude":{"secret":"warden-claude-login"}}}`)); err == nil || !strings.Contains(err.Error(), "providers.claude.secret") {
		t.Fatal(err)
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

// warmSpares 0 is a setting (no spare sandbox), not an unset field; the
// other integers keep their defaults when omitted.
func TestWarmSparesZeroIsKept(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"}}`))
	if err != nil || c.Sandboxes.WarmSpares != 1 {
		t.Fatalf("default warmSpares: %d %v", c.Sandboxes.WarmSpares, err)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"warmSpares":0,"maxRunning":3}}`))
	if err != nil || c.Sandboxes.WarmSpares != 0 || c.Sandboxes.MaxRunning != 3 || c.Sandboxes.KeepStopped != 32 {
		t.Fatalf("explicit warmSpares 0: spares %d running %d kept %d %v", c.Sandboxes.WarmSpares, c.Sandboxes.MaxRunning, c.Sandboxes.KeepStopped, err)
	}
	if _, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"warmSpares":-1}}`)); err == nil {
		t.Fatal("negative warmSpares accepted")
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

// services.runner.previews is the runner's shared mutual-TLS preview
// server (docs/warden-kubernetes-plan.md, decisions 5 and 10): two tls://
// URLs set together, needing the tls section, on their own port; unset
// on the sbx shapes and omitted from a written file.
func TestRunnerPreviewListener(t *testing.T) {
	tls := `"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}`
	k8s := `{"version":1,"paths":{"state":"/var/lib/warden"},
	 "services":{"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444","previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-runner:7446"}}},` + tls + `}`
	c, err := Parse([]byte(k8s))
	if err != nil {
		t.Fatal(err)
	}
	if c.RunnerPreviewListen() != "tls://0.0.0.0:7446" || c.RunnerPreviewAddress() != "tls://warden-runner:7446" || c.RunnerListen() != "tls://0.0.0.0:7444" {
		t.Fatalf("%+v", c.Services.Runner)
	}
	// The preview server alone is a tls:// URL: the tls section is required
	// with it, whatever the control listeners use.
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://runner.internal:7446"}}},` + tls + `}`))
	if err != nil || !c.UsesTLS() || c.RunnerListen() != "unix:///tmp/w/runner/worker.sock" {
		t.Fatalf("%v %+v", err, c.Services.Runner)
	}
	// Unset, nothing is written and the sbx defaults hold.
	c = Defaults("/tmp/w")
	if c.RunnerPreviewListen() != "" || c.RunnerPreviewAddress() != "" {
		t.Fatalf("%+v", c.Services.Runner)
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	if err = Write(path, c); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "\"services\"") {
		t.Fatalf("unset preview listener written:\n%s", raw)
	}
	// A file with only the control listeners writes no previews entry.
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"unix:///tmp/w/runner/worker.sock"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(path, c); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "\"runner\": {\n      \"listen\": \"unix:///tmp/w/runner/worker.sock\"\n    }") {
		t.Fatalf("services.runner written with more than its listener:\n%s", raw)
	}
	bad := map[string]string{
		"listen alone":            `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446"}}},` + tls + `}`,
		"address alone":           `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"address":"tls://warden-runner:7446"}}},` + tls + `}`,
		"without the tls section": `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-runner:7446"}}}}`,
		"http listener":           `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"http://127.0.0.1:7446","address":"http://127.0.0.1:7446"}}},` + tls + `}`,
		"https address":           `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"https://warden-runner:7446"}}},` + tls + `}`,
		"unix listener":           `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"unix:///tmp/p.sock","address":"unix:///tmp/p.sock"}}},` + tls + `}`,
		"address without a host":  `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://:7446"}}},` + tls + `}`,
		"path in the address":     `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-runner:7446/previews"}}},` + tls + `}`,
		"the control port":        `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444","previews":{"listen":"tls://:7444","address":"tls://warden-runner:7444"}}},` + tls + `}`,
		"unknown previews field":  `{"version":1,"paths":{"state":"/tmp/w"},"services":{"runner":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-runner:7446","port":7446}}},` + tls + `}`,
		"previews on the chat":    `{"version":1,"paths":{"state":"/tmp/w"},"services":{"chat":{"previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-chat:7446"}}},` + tls + `}`,
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
	if k.WorkspaceSizeGi != 20 || k.GatewayService != "warden-gateway" || k.GatewayPort != 7000 || k.TrustConfigMap != "warden-guest-trust" || k.GatewayCAMaxAgeDays != 365 || k.StorageClass != "" || k.Tier != TierGVisor || k.NodeSelector != nil || k.Tolerations != nil {
		t.Fatalf("kubernetes defaults: %+v", k)
	}
	// The placement fields the chart renders (appendix A) are typed.
	placed, err := Parse([]byte(strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "gatewayPort": 7100, "gatewayCAMaxAgeDays": 30, "nodeSelector": {"warden.monaddle.com/pool": "sandboxes"}, "tolerations": [{"key": "sandbox.gke.io/runtime", "operator": "Equal", "value": "gvisor", "effect": "NoSchedule"}],`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if pk := placed.Kubernetes; pk.GatewayPort != 7100 || pk.GatewayCAMaxAgeDays != 30 || pk.NodeSelector["warden.monaddle.com/pool"] != "sandboxes" || len(pk.Tolerations) != 1 || pk.Tolerations[0].Key != "sandbox.gke.io/runtime" || pk.Tolerations[0].Effect != "NoSchedule" {
		t.Fatalf("placement: %+v", pk)
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
		"gateway port out of range":          strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "gatewayPort": 70000,`, 1),
		"negative CA max age":                strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "gatewayCAMaxAgeDays": -1,`, 1),
		"loopback edge listen with kind sbx": sbx + `,"previews":{"edgeListen":"0.0.0.0:19081"}}`,
		"bad gateway port":                   strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "gatewayPort": 70000,`, 1),
		"bad toleration operator":            strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "tolerations": [{"operator": "Sometimes"}],`, 1),
		"bad toleration effect":              strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "tolerations": [{"key": "k", "effect": "Never"}],`, 1),
		"unknown toleration field":           strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "tolerations": [{"key": "k", "colour": "blue"}],`, 1),
	}
	for name, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The kubernetes section's own unknown fields are refused like any other.
	if _, err := Parse([]byte(strings.Replace(kubernetesExample, `"runtimeClass": "gvisor",`, `"runtimeClass": "gvisor", "nodeSelectors": "x",`, 1))); err == nil || !strings.Contains(err.Error(), "nodeSelectors") {
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

// edge.bugReports (docs/bug-reporting-plan.md): off with the defaults, on
// with the cloud values, its numbers defaulted and bounded.
func TestEdgeBugReports(t *testing.T) {
	c := Defaults("/tmp/w")
	if b := c.Edge.BugReports; b.Enabled || b.RetentionDays != 90 || b.MaxPerHour != 30 || b.MaxPerDay != 500 {
		t.Fatalf("defaults: %+v", b)
	}
	c, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"enabled":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Edge.BugReports; !b.Enabled || b.RetentionDays != 90 || b.MaxPerHour != 30 || b.MaxPerDay != 500 {
		t.Fatalf("enabled with defaults: %+v", b)
	}
	c, err = Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"enabled":true,"retentionDays":7,"maxPerHour":5,"maxPerDay":50}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Edge.BugReports; !b.Enabled || b.RetentionDays != 7 || b.MaxPerHour != 5 || b.MaxPerDay != 50 {
		t.Fatalf("overrides: %+v", b)
	}
	for _, bad := range []string{
		`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"retentionDays":-1}}}`,
		`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"retentionDays":4000}}}`,
		`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"maxPerHour":-3}}}`,
		`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bugReports":{"maxPerDay":-3}}}`,
		`{"version":1,"paths":{"state":"/tmp/w"},"edge":{"bogus":true}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	if err := Write(path, Defaults("/tmp/w")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"bugReports": {`) || !strings.Contains(string(raw), `"enabled": false`) {
		t.Fatalf("written file lacks the edge section: %s", raw)
	}
}

// Bug reporting (docs/bug-reporting-plan.md): off by default with the
// cloud receiver as the URL, an opt-in that survives a round trip, and a
// URL that is https or a loopback http.
func TestReportingDefaultsOptInAndURL(t *testing.T) {
	c := Defaults("/tmp/w")
	if c.Reporting.Enabled || c.Reporting.URL != DefaultReportingURL {
		t.Fatalf("%+v", c.Reporting)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "warden.json")
	c.Reporting.Enabled = true
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, "")
	if err != nil || !loaded.Reporting.Enabled || loaded.Reporting.URL != DefaultReportingURL {
		t.Fatalf("%+v %v", loaded.Reporting, err)
	}
	// A file without the section reads as off with the default URL.
	old, err := Parse([]byte(`{"version":1,"paths":{"state":"/tmp/w"}}`))
	if err != nil || old.Reporting.Enabled || old.Reporting.URL != DefaultReportingURL {
		t.Fatalf("%+v %v", old.Reporting, err)
	}
	for url, ok := range map[string]bool{
		"https://cloud.warden.monaddle.com/api/bug-reports": true,
		"http://127.0.0.1:9999/api/bug-reports":             true,
		"http://[::1]:9999/api/bug-reports":                 true,
		"http://example.com/api/bug-reports":                false,
		"http://localhost:9999/x":                           false,
		"ftp://127.0.0.1/x":                                 false,
		"cloud.warden.monaddle.com/api/bug-reports":         false,
		"": false,
	} {
		c := Defaults("/tmp/w")
		c.Reporting.URL = url
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("reporting.url %q: %v", url, err)
		}
	}
}
