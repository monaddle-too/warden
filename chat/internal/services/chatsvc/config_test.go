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
	if s.state != "/state" || s.wardenSocket != "/run/warden/sbx-control.sock" || s.runnerSocket != "/run/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.web != "/app/web" || s.suffix != "preview.monaddle.com" {
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
	if s.wardenSocket != "/tmp/w/policy/sbx-control.sock" || s.runnerSocket != "/tmp/w/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.web != "chat/web/dist" {
		t.Fatalf("%+v", s)
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
	if s.state != "/var/lib/warden/app" || s.wardenSocket != "/var/lib/warden/policy/sbx-control.sock" || s.runnerSocket != "/var/lib/warden/runner/worker.sock" || s.listen != "127.0.0.1:18780" || s.web != "/app/web" || s.suffix != "preview.monaddle.com" {
		t.Fatalf("%+v", s)
	}
	if s.previewScheme != "https" || s.previewPort != "" {
		t.Fatal("public previews must keep https URLs without a port", s.previewScheme, s.previewPort)
	}
}
