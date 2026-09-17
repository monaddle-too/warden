package chatsvc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warden/chat/internal/config"
)

func chatSettings(t *testing.T, args ...string) (settings, error) {
	t.Helper()
	fs := flag.NewFlagSet("warden-chat", flag.ContinueOnError)
	f := chatFlags{configPath: fs.String("config", "", ""), state: fs.String("state", "", ""), wardenSocket: fs.String("warden-socket", "", ""), runnerSocket: fs.String("runner-socket", "", ""), listen: fs.String("listen", "127.0.0.1:18780", ""), web: fs.String("web-dir", "chat/web/dist", ""), suffix: fs.String("preview-suffix", "", "")}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return resolveSettings(fs, f)
}

func TestOVHChatFlagsReproduceCurrentBehaviour(t *testing.T) {
	t.Setenv(config.Env, "")
	s, err := chatSettings(t, "--state", "/state", "--warden-socket", "/run/warden/sbx-control.sock", "--runner-socket", "/run/runner/worker.sock", "--listen", "127.0.0.1:18780", "--preview-suffix", "preview.monaddle.com", "--web-dir", "/app/web")
	if err != nil {
		t.Fatal(err)
	}
	if s.state != "/state" || s.policy != "unix:///run/warden/sbx-control.sock" || s.runner != "unix:///run/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.web != "/app/web" || s.suffix != "preview.monaddle.com" {
		t.Fatalf("%+v", s)
	}
	if s.listenURL != "http://127.0.0.1:18780" || s.address != "http://127.0.0.1:18780" || s.tls != nil {
		t.Fatalf("%+v", s)
	}
	if s.previewScheme != "https" || s.previewPort != "" {
		t.Fatal("public previews must keep https URLs without a port", s.previewScheme, s.previewPort)
	}
	// The launcher's empty suffix leaves external previews unconfigured.
	if s, err = chatSettings(t, "--state", "/tmp/w/app", "--preview-suffix", ""); err != nil || s.suffix != "" {
		t.Fatal(s.suffix, err)
	}
}

func TestChatStateAloneMeansLoopbackPreviews(t *testing.T) {
	t.Setenv(config.Env, "")
	s, err := chatSettings(t, "--state", "/tmp/w/app")
	if err != nil {
		t.Fatal(err)
	}
	if s.policy != "unix:///tmp/w/policy/sbx-control.sock" || s.runner != "unix:///tmp/w/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.listenURL != "http://127.0.0.1:18780" || s.address != s.listenURL || s.tls != nil || s.web != "chat/web/dist" {
		t.Fatalf("%+v", s)
	}
	// The legacy --listen flag alone moves the loopback URL with it.
	if s, err = chatSettings(t, "--state", "/tmp/w/app", "--listen", "127.0.0.1:19000"); err != nil || s.listenURL != "http://127.0.0.1:19000" || s.address != "http://127.0.0.1:19000" {
		t.Fatalf("%+v %v", s, err)
	}
	if s.suffix != "localhost" || s.previewScheme != "http" || s.previewPort != "18781" {
		t.Fatalf("%+v", s)
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/tmp/w","webAssets":"/opt/warden/web"},"previews":{"edgeListen":"127.0.0.1:20000"}}`), 0600)
	if s, err = chatSettings(t, "--config", path); err != nil || s.previewPort != "20000" || s.web != "/opt/warden/web" || s.state != "/tmp/w/app" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err = chatSettings(t, "--config", path, "--preview-suffix", ""); err == nil || !strings.Contains(err.Error(), "previews.hostSuffix") {
		t.Fatal("empty suffix against a loopback file accepted", err)
	}
	if _, err = chatSettings(t, "--config", path, "--listen", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "chat.listen") {
		t.Fatal("listen disagreement accepted", err)
	}
}

// deploy/chat/compose.yaml: the chat reads warden.example.json with the
// host paths mounted at the same container paths; the values must equal
// TestOVHChatFlagsReproduceCurrentBehaviour under those paths.
func TestOVHExampleFileMatchesTheComposeDeployment(t *testing.T) {
	t.Setenv(config.Env, "")
	s, err := chatSettings(t, "--config", filepath.Join("..", "..", "..", "..", "deploy", "chat", "warden.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.state != "/var/lib/warden/app" || s.policy != "unix:///var/lib/warden/policy/sbx-control.sock" || s.runner != "unix:///var/lib/warden/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.web != "/app/web" || s.suffix != "preview.monaddle.com" {
		t.Fatalf("%+v", s)
	}
	if s.listenURL != "http://127.0.0.1:18780" || s.address != "http://127.0.0.1:18780" || s.tls != nil {
		t.Fatalf("%+v", s)
	}
	if s.previewScheme != "https" || s.previewPort != "" {
		t.Fatal("public previews must keep https URLs without a port", s.previewScheme, s.previewPort)
	}
}

// A file with tls:// services gives the chat its mutual-TLS listener, the
// name the edge dials it by, and the runner and policy addresses.
func TestChatTransportSettings(t *testing.T) {
	t.Setenv(config.Env, "")
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/var/lib/warden"},
		"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444","previews":{"listen":"tls://0.0.0.0:7446","address":"tls://warden-runner:7446"}},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
		"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}}`), 0600)
	s, err := chatSettings(t, "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	if s.listenURL != "tls://0.0.0.0:7445" || s.address != "tls://warden-chat:7445" || s.policy != "tls://warden-policy:7443" || s.runner != "tls://warden-runner:7444" || s.listen != "127.0.0.1:18780" || s.tls == nil || s.tls.KeyFile != "/etc/warden/tls/tls.key" {
		t.Fatalf("%+v %+v", s, s.tls)
	}
	// The runner's shared preview server, dialed by the ports proxy as
	// https://warden-runner:7446/<id>; its host is what config.HostOf gives.
	if s.runnerPreviews != "tls://warden-runner:7446" || config.HostOf(s.runnerPreviews) != "warden-runner:7446" {
		t.Fatalf("%+v", s)
	}
	if _, err = chatSettings(t, "--config", path, "--listen", "127.0.0.1:18780"); err == nil || !strings.Contains(err.Error(), "services.chat.listen") {
		t.Fatal("loopback listen flag against a tls file accepted", err)
	}
	if _, err = chatSettings(t, "--config", path, "--runner-socket", "/x.sock"); err == nil || !strings.Contains(err.Error(), "services.runner.address") {
		t.Fatal("runner socket flag against a tls file accepted", err)
	}
}
