package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

func runnerSettings(t *testing.T, args ...string) (settings, error) {
	t.Helper()
	fs := flag.NewFlagSet("warden-runner", flag.ContinueOnError)
	f := runnerFlags{
		configPath: fs.String("config", "", ""), root: fs.String("root", "", ""), socket: fs.String("socket", "", ""), wardenSocket: fs.String("warden-socket", "", ""),
		sbx: fs.String("sbx", "", ""), template: fs.String("template", "", ""), runtimeDir: fs.String("runtime-dir", "", ""), claudePath: fs.String("claude-path", "", ""),
		idle: fs.Duration("idle-timeout", 15*time.Minute, ""), memoryMB: fs.Int("sandbox-memory-mb", 1536, ""), residents: fs.Int("max-resident", 2, ""), spares: fs.Int("spare-sandboxes", 1, ""), retained: fs.Int("retained", 32, ""),
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
	if s.root != "/state" || s.socket != "/state/worker.sock" || s.wardenSocket != "/run/warden/sbx-control.sock" || s.sbx != "/usr/local/bin/sbx" || s.runtimeDir != "/opt/warden-runtime" || s.claudePath != "/opt/warden-claude/claude" || s.template != template {
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
	if s.socket != "/tmp/w/runner/worker.sock" || s.wardenSocket != "/tmp/w/policy/sbx-control.sock" || s.template != release.StockTemplate+"@"+release.StockTemplateDigest || s.residents != 2 || s.spares != 1 || s.idle != 15*time.Minute || s.retained != 32 {
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
	s, err := runnerSettings(t, "--config", filepath.Join("..", "..", "..", "deploy", "chat", "warden.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.root != "/var/lib/warden/runner" || s.socket != "/var/lib/warden/runner/worker.sock" || s.wardenSocket != "/var/lib/warden/policy/sbx-control.sock" || s.sbx != "/usr/bin/sbx" {
		t.Fatalf("%+v", s)
	}
	if s.runtimeDir != "/opt/warden/runtime" || s.claudePath != "/opt/warden/claude/claude" || s.template != release.StockTemplate+"@"+release.StockTemplateDigest {
		t.Fatalf("%+v", s)
	}
	if s.memoryMB != 1536 || s.residents != 2 || s.spares != 1 || s.retained != 32 || s.idle != 15*time.Minute {
		t.Fatalf("%+v", s)
	}
}
