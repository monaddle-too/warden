package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSBX is an sbx stand-in that prints its arguments, one per line.
func fakeSBX(t *testing.T) string {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "sbx")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return fake
}

func launchArgs(t *testing.T, fake string, run RunSpec) string {
	t.Helper()
	d := &sbxRuntime{worker: &Worker{Executable: fake}}
	stream, err := d.Stream(context.Background(), "sandbox", run)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAgentToolsOverrideNativeProxyInBothCasings(t *testing.T) {
	fake := fakeSBX(t)
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			raw := launchArgs(t, fake, RunSpec{Directory: "/workspace", Broker: BrokerConfig{Provider: provider, ProxyURL: "http://host.docker.internal:19000", APIKeyPlaceholder: "warden-proxy-managed"}, TrustsCA: true})
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
				if !strings.Contains(raw, key+"=http://host.docker.internal:19000\n") {
					t.Fatalf("%s tools would inherit SBX proxy: missing %s", provider, key)
				}
			}
			if !strings.Contains(raw, "-w\n/workspace\n") {
				t.Fatalf("workspace not passed: %s", raw)
			}
		})
	}
}

// The launch commands take the runtime paths from the guest manifest and
// fall back to the SBX template's /tmp layout; the broker's placeholder is
// the credential the agent presents, whatever value the gateway minted.
func TestLaunchPathsFollowTheGuestManifest(t *testing.T) {
	fake := fakeSBX(t)
	codex := launchArgs(t, fake, RunSpec{Broker: BrokerConfig{Provider: "codex", APIKeyPlaceholder: "s1.cap"}, TrustsCA: true})
	if !strings.Contains(codex, "\n/tmp/warden-runtime/bin/codex\n") || !strings.Contains(codex, "WARDEN_API_KEY=s1.cap\n") {
		t.Fatalf("codex default launch: %s", codex)
	}
	claude := launchArgs(t, fake, RunSpec{Broker: BrokerConfig{Provider: "claude", APIKeyPlaceholder: "s1.cap"}, TrustsCA: true})
	if !strings.Contains(claude, "\n/tmp/warden-claude\n") || !strings.Contains(claude, "CLAUDE_CODE_OAUTH_TOKEN=s1.cap\n") {
		t.Fatalf("claude default launch: %s", claude)
	}
	manifest := GuestPaths{Codex: "/opt/warden/runtime", Claude: "/opt/warden/claude/claude"}
	if got := launchArgs(t, fake, RunSpec{Broker: BrokerConfig{Provider: "codex"}, Paths: manifest, TrustsCA: true}); !strings.Contains(got, "\n/opt/warden/runtime/bin/codex\n") {
		t.Fatalf("codex manifest launch: %s", got)
	}
	if got := launchArgs(t, fake, RunSpec{Broker: BrokerConfig{Provider: "claude"}, Paths: manifest, TrustsCA: true}); !strings.Contains(got, "\n/opt/warden/claude/claude\n") {
		t.Fatalf("claude manifest launch: %s", got)
	}
	// A manifest path that could not be passed verbatim falls back.
	hostile := GuestPaths{Codex: "/tmp/x; rm -rf /", Claude: "relative/claude", Home: "/home/agent/../root", User: "root shell"}.orDefaults()
	if hostile != (GuestPaths{Codex: defaultCodexPath, Claude: defaultClaudePath, Trust: defaultTrustPath, Home: defaultHomePath, User: defaultGuestUser}) {
		t.Fatalf("unsafe manifest paths accepted: %+v", hostile)
	}
	if got := (GuestPaths{Codex: "/opt/warden/runtime/", User: "agent"}).orDefaults(); got.Codex != "/opt/warden/runtime" || got.Claude != defaultClaudePath || got.User != "agent" {
		t.Fatalf("partial manifest: %+v", got)
	}
}

// The SBX driver delivers trust by exec: a guest that does not trust the
// broker CA gets it installed before the launch, one that does gets no exec.
func TestSBXStreamInstallsCAOnlyWhenUntrusted(t *testing.T) {
	fake := fakeSBX(t)
	trusted := launchArgs(t, fake, RunSpec{Broker: BrokerConfig{Provider: "codex", CACertificate: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"}, TrustsCA: true})
	if strings.Contains(trusted, "update-ca-certificates") {
		t.Fatal("trusted guest received a CA install")
	}
	d := &sbxRuntime{worker: &Worker{Executable: fake}}
	if _, err := d.Stream(context.Background(), "sandbox", RunSpec{Broker: BrokerConfig{Provider: "codex", CACertificate: "not a certificate"}}); err == nil {
		t.Fatal("an invalid CA must not be installed or launched past")
	}
}
