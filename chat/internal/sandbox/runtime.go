package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/release"
)

type RuntimeSpec struct{ Name, Directory, Source string }

// RuntimeDriver is the narrow trusted SBX adapter. Tests never need a daemon.
type RuntimeDriver interface {
	Create(context.Context, RuntimeSpec) error
	KeepAlive(string) (io.Closer, error)
	Exec(context.Context, string, string, ...string) (string, error)
	Copy(context.Context, string, string, string) error
	// InstallCA makes the guest trust the broker's public gateway CA.
	InstallCA(context.Context, string, string) error
	Stream(context.Context, string, string, BrokerConfig) (io.ReadWriteCloser, error)
	Stop(context.Context, string) error
	Remove(context.Context, string) error
	Publish(context.Context, string, int, int) error
	Unpublish(context.Context, string, int, int) error
	Mappings(context.Context, string) ([]PortMapping, error)
}
type sbxRuntime struct{ worker *Worker }

func (d *sbxRuntime) Create(ctx context.Context, s RuntimeSpec) error {
	inventory, err := command(ctx, d.worker.Executable, "ls", "--quiet").Output()
	if err != nil {
		return errors.New("could not reconcile registered sandbox inventory")
	}
	for _, name := range strings.Fields(string(inventory)) {
		if name == s.Name {
			return nil
		}
	}
	memoryMB := d.worker.MemoryMB
	if memoryMB == 0 {
		memoryMB = 1536
	}
	if memoryMB < 512 || memoryMB > 16384 {
		return errors.New("sandbox memory must be 512–16384 MiB")
	}
	args := []string{"create", "--name", s.Name, "--cpus", "1", "--memory", fmt.Sprintf("%dm", memoryMB), "--template", d.worker.Template, "--deny-network", "**", "--no-share-skills"}
	if s.Source != "" {
		args = append(args, "--clone", "shell", s.Source)
	} else {
		args = append(args, "shell")
	}
	// The daemon's own output (not agent-controlled) is worth the log line
	// when creation fails: it names the image or resource that was refused.
	cmd := command(ctx, d.worker.Executable, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{W: &stderr, N: 4096}
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("SBX creation failed: %w: %s", err, detail)
		}
		return fmt.Errorf("SBX creation failed: %w", err)
	}
	return nil
}
func (d *sbxRuntime) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	a := append([]string{"exec", "-i", "-w", dir, name}, args...)
	cmd := command(ctx, d.worker.Executable, a...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{W: &out, N: 96 << 20}
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("SBX operation failed: %w", err)
	}
	return out.String(), nil
}

// codexBundle describes the pinned Linux Codex runtime bundle on the host.
type codexBundle struct {
	Version, Target, Entrypoint, ResourcesDir, PathDir string
	LayoutVersion                                      int
}

// codexBundleMetadata validates the host bundle for the local guest architecture.
func codexBundleMetadata(dir string) (codexBundle, error) {
	var metadata codexBundle
	raw, err := os.ReadFile(filepath.Join(dir, "codex-package.json"))
	if err != nil {
		return metadata, errors.New("pinned Linux runtime bundle metadata is missing")
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return metadata, errors.New("invalid runtime bundle metadata")
	}
	targetArch := "aarch64-unknown-linux-musl"
	if runtime.GOARCH == "amd64" {
		targetArch = "x86_64-unknown-linux-musl"
	}
	if metadata.Version != release.CodexVersion || metadata.Target != targetArch || metadata.LayoutVersion != 1 || metadata.Entrypoint != "bin/codex" || metadata.ResourcesDir != "codex-resources" || metadata.PathDir != "codex-path" {
		return metadata, errors.New("runtime bundle must be Codex " + release.CodexVersion + " for the local Linux guest architecture")
	}
	return metadata, nil
}
func (d *sbxRuntime) Copy(ctx context.Context, name, source, target string) error {
	if source == d.worker.RuntimeDir {
		if _, err := codexBundleMetadata(source); err != nil {
			return err
		}
	}
	return command(ctx, d.worker.Executable, "cp", source, name+":"+target).Run()
}
func (d *sbxRuntime) Stop(ctx context.Context, name string) error {
	return command(ctx, d.worker.Executable, "stop", name).Run()
}

// Remove deletes the sandbox, its container state and scoped secrets. The
// runtime name is Warden-derived, never guest-supplied.
func (d *sbxRuntime) Remove(ctx context.Context, name string) error {
	return command(ctx, d.worker.Executable, "rm", "--force", name).Run()
}
func (d *sbxRuntime) Publish(ctx context.Context, name string, port, host int) error {
	return command(ctx, d.worker.Executable, "ports", name, "--publish", net.JoinHostPort("127.0.0.1", strconv.Itoa(host))+":"+strconv.Itoa(port)+"/tcp4").Run()
}
func (d *sbxRuntime) Unpublish(ctx context.Context, name string, port, host int) error {
	return command(ctx, d.worker.Executable, "ports", name, "--unpublish", net.JoinHostPort("127.0.0.1", strconv.Itoa(host))+":"+strconv.Itoa(port)+"/tcp4").Run()
}

type processStream struct {
	input  io.WriteCloser
	output io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
}

func (p *processStream) Read(b []byte) (int, error)  { return p.output.Read(b) }
func (p *processStream) Write(b []byte) (int, error) { return p.input.Write(b) }
func (p *processStream) Close() error {
	p.once.Do(func() { p.cancel(); p.input.Close(); p.output.Close(); _ = p.cmd.Wait() })
	return nil
}

// guestCAPath is where the guest keeps the broker's public gateway CA.
const guestCAPath = "/usr/local/share/ca-certificates/warden-proxy.crt"

func (d *sbxRuntime) InstallCA(ctx context.Context, name, certificate string) error {
	block, rest := pem.Decode([]byte(certificate))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("invalid Warden proxy certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return errors.New("Warden proxy certificate is not a CA")
	}
	file, err := os.CreateTemp("", "warden-public-ca-*.crt")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(certificate)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = command(ctx, d.worker.Executable, "cp", file.Name(), name+":/tmp/warden-public-ca.crt").Run(); err != nil {
		return err
	}
	// Trust only the broker's public CA. Never copy its signing key and
	// never disable upstream or guest TLS verification.
	if err = command(ctx, d.worker.Executable, "exec", name, "sudo", "sh", "-c", "install -m 0644 /tmp/warden-public-ca.crt "+guestCAPath+" && update-ca-certificates >/dev/null").Run(); err != nil {
		return errors.New("could not install Warden proxy CA")
	}
	return nil
}
func (d *sbxRuntime) Stream(ctx context.Context, name, dir string, broker BrokerConfig) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := command(ctx, d.worker.Executable, "exec", "-i", "-w", dir, name, "env", "-u", "OPENAI_API_KEY", "-u", "OPENAI_BASE_URL", "-u", "CODEX_API_KEY", "HTTP_PROXY="+broker.ProxyURL, "HTTPS_PROXY="+broker.ProxyURL, "http_proxy="+broker.ProxyURL, "https_proxy="+broker.ProxyURL, "WARDEN_API_KEY="+broker.APIKeyPlaceholder, "WORKSPACE_DOCUMENT_API_URL="+broker.DocumentBaseURL, "/tmp/warden-runtime/bin/codex", "app-server", "--listen", "stdio://", "-c", `model_provider="warden"`, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`, "-c", `model_providers.warden.base_url=`+strconv.Quote(broker.ProviderBaseURL), "-c", `model_providers.warden.name="Warden"`, "-c", `model_providers.warden.wire_api="responses"`, "-c", `model_providers.warden.env_key="WARDEN_API_KEY"`)
	if broker.Provider == "claude" {
		args := []string{"exec", "-i", "-w", dir, name, "env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN=warden-proxy-managed", "ANTHROPIC_BASE_URL=" + broker.ProviderBaseURL, "HTTP_PROXY=" + broker.ProxyURL, "HTTPS_PROXY=" + broker.ProxyURL, "http_proxy=" + broker.ProxyURL, "https_proxy=" + broker.ProxyURL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "/tmp/warden-claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", "default", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{"warden":{"type":"sdk","name":"warden"}}}`, "--setting-sources=", "--append-system-prompt", "You work inside a Warden-managed sandbox. Warden controls external access and tool approvals. Never request or expose host credentials. Keep files in the workspace. For web previews, start a detached server on 0.0.0.0 and use the Warden MCP preview_attach or sandbox_bind_port tool; Warden chooses the URL."}
		if broker.Model != "" {
			args = append(args, "--model", broker.Model)
		}
		if broker.ThreadID != "" {
			args = append(args, "--resume", broker.ThreadID)
		}
		cmd = command(ctx, d.worker.Executable, args...)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	// Agent-controlled stderr must not enter controller service logs.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		cancel()
		in.Close()
		out.Close()
		return nil, errors.New("could not start sandbox app server")
	}
	return &processStream{input: in, output: out, cmd: cmd, cancel: cancel}, nil
}

// KeepAlive owns a credential-free session independently of the agent stream.
// SBX auto-stops a VM after its last exec/SSH session disconnects, even when
// detached guest processes and published ports still exist.
func (d *sbxRuntime) KeepAlive(name string) (io.Closer, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := command(ctx, d.worker.Executable, "exec", "-i", name, "sh", "-c", "printf 'ready\\n'; exec cat >/dev/null")
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		cancel()
		in.Close()
		out.Close()
		return nil, err
	}
	p := &processStream{input: in, output: out, cmd: cmd, cancel: cancel}
	ready := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		ready <- err == nil && line == "ready\n"
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case ok := <-ready:
		if ok {
			return p, nil
		}
	case <-timer.C:
	}
	p.Close()
	return nil, errors.New("sandbox residency session did not become ready")
}
