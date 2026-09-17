package runnersvc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
	sandboxkube "warden/chat/internal/sandbox/kube"
)

func runnerSettings(t *testing.T, args ...string) (settings, error) {
	t.Helper()
	fs := flag.NewFlagSet("warden-runner", flag.ContinueOnError)
	f := runnerFlags{
		configPath: fs.String("config", "", ""), root: fs.String("root", "", ""), socket: fs.String("socket", "", ""), wardenSocket: fs.String("warden-socket", "", ""),
		sbx: fs.String("sbx", "", ""), template: fs.String("template", "", ""), runtimeDir: fs.String("runtime-dir", "", ""), claudePath: fs.String("claude-path", "", ""),
		idle: fs.Duration("idle-timeout", 15*time.Minute, ""), memoryMB: fs.Int("sandbox-memory-mb", 1536, ""), residents: fs.Int("max-resident", 2, ""), spares: fs.Int("spare-sandboxes", 1, ""), retained: fs.Int("retained", 32, ""),
		tlsListen: fs.String("tls-listen", "", ""), tlsCA: fs.String("tls-ca", "", ""), tlsCert: fs.String("tls-cert", "", ""), tlsKey: fs.String("tls-key", "", ""),
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return resolveSettings(fs, f)
}

func TestOVHRunnerFlagsReproduceCurrentBehaviour(t *testing.T) {
	t.Setenv(config.Env, "")
	template := release.StockTemplate + "@" + release.StockTemplateDigest
	s, err := runnerSettings(t, "--root", "/state", "--socket", "/state/worker.sock", "--warden-socket", "/run/warden/sbx-control.sock", "--sbx", "/usr/local/bin/sbx", "--runtime-dir", "/opt/warden-runtime", "--claude-path", "/opt/warden-claude/claude", "--template", template, "--sandbox-memory-mb", "1536", "--max-resident", "2", "--spare-sandboxes", "1")
	if err != nil {
		t.Fatal(err)
	}
	if s.root != "/state" || s.listen != "unix:///state/worker.sock" || s.policy != "unix:///run/warden/sbx-control.sock" || s.tls != nil || s.sbx != "/usr/local/bin/sbx" || s.runtimeDir != "/opt/warden-runtime" || s.claudePath != "/opt/warden-claude/claude" || s.template != template {
		t.Fatalf("%+v", s)
	}
	if s.memoryMB != 1536 || s.residents != 2 || s.spares != 1 || s.retained != 32 || s.idle != 15*time.Minute {
		t.Fatalf("%+v", s)
	}
}

func TestRunnerRootAloneDerivesEverything(t *testing.T) {
	t.Setenv(config.Env, "")
	previous := findSBX
	findSBX = func(func(string) (string, error), func(string) bool) (string, error) { return "/detected/sbx", nil }
	defer func() { findSBX = previous }()
	s, err := runnerSettings(t, "--root", "/tmp/w/runner")
	if err != nil {
		t.Fatal(err)
	}
	if s.sbx != "/detected/sbx" {
		t.Fatal("unset sbx.executable should fall back to detection", s.sbx)
	}
	if s.listen != "unix:///tmp/w/runner/worker.sock" || s.policy != "unix:///tmp/w/policy/sbx-control.sock" || s.tls != nil || s.template != release.StockTemplate+"@"+release.StockTemplateDigest || s.residents != 2 || s.spares != 1 || s.idle != 15*time.Minute || s.retained != 32 {
		t.Fatalf("%+v", s)
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/tmp/w"},"sbx":{"guestImage":"ghcr.io/x/guest:1","guestImageDigest":"sha256:`+strings.Repeat("a", 64)+`"},"runtimes":{"codex":"/tmp/w/runtime/codex","claude":"/tmp/w/runtime/claude"},"sandboxes":{"maxRunning":3,"stopAfterIdleMinutes":30}}`), 0600)
	if s, err = runnerSettings(t, "--config", path); err != nil || s.root != "/tmp/w/runner" || s.template != "ghcr.io/x/guest:1@sha256:"+strings.Repeat("a", 64) || s.runtimeDir != "/tmp/w/runtime/codex" || s.claudePath != "/tmp/w/runtime/claude" || s.residents != 3 || s.idle != 30*time.Minute {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err = runnerSettings(t, "--config", path, "--max-resident", "4"); err == nil || !strings.Contains(err.Error(), "sandboxes.maxRunning") {
		t.Fatal("disagreement accepted", err)
	}
	if _, err = runnerSettings(t, "--config", path, "--idle-timeout", "10m"); err == nil || !strings.Contains(err.Error(), "stopAfterIdleMinutes") {
		t.Fatal("disagreement accepted", err)
	}
}

// deploy/chat/compose.yaml: the runner reads warden.example.json with the
// host paths mounted at the same container paths; the values must equal
// TestOVHRunnerFlagsReproduceCurrentBehaviour under those paths.
func TestOVHExampleFileMatchesTheComposeDeployment(t *testing.T) {
	t.Setenv(config.Env, "")
	s, err := runnerSettings(t, "--config", filepath.Join("..", "..", "..", "..", "deploy", "chat", "warden.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.root != "/var/lib/warden/runner" || s.listen != "unix:///var/lib/warden/runner/worker.sock" || s.policy != "unix:///var/lib/warden/policy/sbx-control.sock" || s.tls != nil || s.sbx != "/usr/bin/sbx" {
		t.Fatalf("%+v", s)
	}
	if s.runtimeDir != "/opt/warden/runtime" || s.claudePath != "/opt/warden/claude/claude" || s.template != release.StockTemplate+"@"+release.StockTemplateDigest {
		t.Fatalf("%+v", s)
	}
	if s.memoryMB != 1536 || s.residents != 2 || s.spares != 1 || s.retained != 32 || s.idle != 15*time.Minute {
		t.Fatalf("%+v", s)
	}
}

// The transport URLs come from services.* and tls.*; the legacy socket and
// TLS flags map onto them and must agree with a loaded file.
func TestRunnerTransportSettings(t *testing.T) {
	t.Setenv(config.Env, "")
	previous := findSBX
	findSBX = func(func(string) (string, error), func(string) bool) (string, error) { return "/detected/sbx", nil }
	defer func() { findSBX = previous }()
	s, err := runnerSettings(t, "--root", "/tmp/w/runner", "--tls-listen", "127.0.0.1:7444", "--tls-ca", "/tls/ca.crt", "--tls-cert", "/tls/tls.crt", "--tls-key", "/tls/tls.key")
	if err != nil {
		t.Fatal(err)
	}
	if s.listen != "tls://127.0.0.1:7444" || s.policy != "unix:///tmp/w/policy/sbx-control.sock" || s.tls == nil || s.tls.CAFile != "/tls/ca.crt" || s.tls.CertFile != "/tls/tls.crt" || s.tls.KeyFile != "/tls/tls.key" {
		t.Fatalf("%+v %+v", s, s.tls)
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/var/lib/warden"},
		"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444"},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
		"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}}`), 0600)
	if s, err = runnerSettings(t, "--config", path); err != nil || s.listen != "tls://0.0.0.0:7444" || s.policy != "tls://warden-policy:7443" || s.tls == nil || s.tls.CAFile != "/etc/warden/tls/ca.crt" || s.root != "/var/lib/warden/runner" {
		t.Fatalf("%+v %+v %v", s, s.tls, err)
	}
	if _, err = runnerSettings(t, "--config", path, "--socket", "/var/lib/warden/runner/worker.sock"); err == nil || !strings.Contains(err.Error(), "services.runner.listen") {
		t.Fatal("socket flag against a tls file accepted", err)
	}
	if _, err = runnerSettings(t, "--config", path, "--tls-listen", "0.0.0.0:7444", "--tls-ca", "/elsewhere/ca.crt"); err == nil || !strings.Contains(err.Error(), "tls.caFile") {
		t.Fatal("CA disagreement accepted", err)
	}
	if _, err = runnerSettings(t, "--config", path, "--warden-socket", "/x.sock"); err == nil || !strings.Contains(err.Error(), "services.policy.address") {
		t.Fatal("policy address disagreement accepted", err)
	}
}

// The runner selects its driver by runtime.kind: the SBX driver for the
// sbx shapes, the Kubernetes pod driver (over --kubeconfig here; the
// service account in a pod) for the kubernetes kind, configured from the
// kubernetes section and the sandbox memory.
func TestRunnerSelectsDriverByRuntimeKind(t *testing.T) {
	t.Setenv(config.Env, "")
	s, err := runnerSettings(t, "--root", "/state", "--sbx", "/usr/local/bin/sbx")
	if err != nil || s.cfg.RuntimeKind() != config.RuntimeSBX {
		t.Fatalf("%+v %v", s.cfg.Runtime, err)
	}
	driver, err := runtimeDriver(s, "")
	if err != nil || driver == nil {
		t.Fatal(err)
	}
	unknown := s
	unknown.cfg.Runtime.Kind = "firecracker"
	if _, err = runtimeDriver(unknown, ""); err == nil {
		t.Fatal("unknown kind accepted")
	}
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err = os.WriteFile(kubeconfig, []byte(`{"apiVersion":"v1","kind":"Config","current-context":"dev","clusters":[{"name":"dev","cluster":{"server":"https://127.0.0.1:6443","insecure-skip-tls-verify":true}}],"contexts":[{"name":"dev","context":{"cluster":"dev","user":"dev"}}],"users":[{"name":"dev","user":{"token":"t"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "warden.json")
	if err = os.WriteFile(path, []byte(`{"version":1,"runtime":{"kind":"kubernetes"},"paths":{"state":"/var/lib/warden"},
		"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444"},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
		"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"},
		"kubernetes":{"namespace":"warden-sandboxes","tier":"gvisor","runtimeClass":"gvisor","guestImage":"warden-guest-base","guestImageDigest":"sha256:`+strings.Repeat("ab", 32)+`","storageClass":"local-path","workspaceSizeGi":4,"nodeSelector":{"pool":"sandboxes"}},
		"sandboxes":{"memoryMB":1024,"warmSpares":1},"previews":{"edgeListen":"0.0.0.0:18781"},"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = runnerSettings(t, "--config", path)
	if err != nil || s.cfg.RuntimeKind() != config.RuntimeKubernetes || s.memoryMB != 1024 || s.spares != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err = runtimeDriver(s, filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "kubernetes API access") {
		t.Fatalf("missing kubeconfig: %v", err)
	}
	driver, err = runtimeDriver(s, kubeconfig)
	if err != nil || driver == nil {
		t.Fatal(err)
	}
	if _, ok := driver(nil).(*sandboxkube.Driver); !ok {
		t.Fatalf("%T", driver(nil))
	}
	opts := kubernetesOptions(s.cfg.Kubernetes, s.memoryMB)
	if opts.Namespace != "warden-sandboxes" || opts.Tier != config.TierGVisor || opts.RuntimeClass != "gvisor" || opts.Image() != "warden-guest-base@sha256:"+strings.Repeat("ab", 32) || opts.StorageClass != "local-path" || opts.WorkspaceSizeGi != 4 || opts.TrustConfigMap != "warden-guest-trust" || opts.MemoryMB != 1024 || opts.NodeSelector["pool"] != "sandboxes" {
		t.Fatalf("options %+v", opts)
	}
	// Outside a pod without --kubeconfig the service account is missing.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err = runtimeDriver(s, ""); err == nil || !strings.Contains(err.Error(), "kubernetes API access") {
		t.Fatalf("in-cluster outside a cluster: %v", err)
	}
}
