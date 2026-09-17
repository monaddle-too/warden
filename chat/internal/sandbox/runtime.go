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

// GuestPaths are where a guest keeps the agent runtimes, its trust bundle
// and the agent home, as the guest manifest's paths object names them
// (deploy/guest/README.md), plus the account exec sessions run as (the
// manifest's user.name). A guest without a manifest, or with one that
// predates the object, has the SBX template's /tmp layout, which the zero
// value resolves to through orDefaults.
type GuestPaths struct {
	Codex  string `json:"codex,omitempty"`  // the unpacked Codex bundle (bin/codex, ...)
	Claude string `json:"claude,omitempty"` // the Claude executable
	Trust  string `json:"trust,omitempty"`  // the CA bundle TLS clients verify against
	Home   string `json:"home,omitempty"`   // the agent home
	User   string `json:"-"`                // the account exec sessions run as
}

// The SBX guest template's layout, kept as the fallback.
const (
	defaultCodexPath  = "/tmp/warden-runtime"
	defaultClaudePath = "/tmp/warden-claude"
	defaultTrustPath  = "/etc/ssl/certs/ca-certificates.crt"
	defaultHomePath   = "/home/agent"
	defaultGuestUser  = "agent"
)

// orDefaults fills every unset or unusable path with the SBX template's
// value, so callers never launch or copy against an empty string.
func (p GuestPaths) orDefaults() GuestPaths {
	p.Codex = guestPath(p.Codex, defaultCodexPath)
	p.Claude = guestPath(p.Claude, defaultClaudePath)
	p.Trust = guestPath(p.Trust, defaultTrustPath)
	p.Home = guestPath(p.Home, defaultHomePath)
	if p.User == "" || !identifier.MatchString(p.User) {
		p.User = defaultGuestUser
	}
	return p
}

// guestPath accepts only an absolute, quote-free path the launch commands
// and copy targets can carry verbatim; anything else falls back. The
// manifest is guest-writable, so a path is data, never shell.
func guestPath(value, fallback string) string {
	if strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\\\"'` \t\r\n$*?[]{}();&|<>") && !strings.Contains(value, "/../") && !strings.HasSuffix(value, "/..") && len(value) <= 256 {
		if trimmed := strings.TrimRight(value, "/"); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

// RunSpec is what a driver needs to launch the agent in a prepared guest.
type RunSpec struct {
	Directory string
	Broker    BrokerConfig
	Paths     GuestPaths
	// TrustsCA reports that the guest already trusts Broker.CACertificate
	// (its image shipped it, or an earlier run installed it and the guest
	// still has the file). A driver that delivers trust by exec installs it
	// when false; one whose guests mount the trust bundle ignores it.
	TrustsCA bool
}

// PortMapping is one published guest port: the address and port the core
// reaches it at, and the guest port behind them.
type PortMapping struct {
	Address   string
	Port      int
	GuestPort int
}

// RuntimeDriver is the narrow trusted sandbox adapter. Tests never need a
// daemon. The SBX implementation is sbxRuntime; runtime names are
// Warden-derived, never guest-supplied.
type RuntimeDriver interface {
	Create(context.Context, RuntimeSpec) error
	// Prepare readies a guest that just became resident (created, adopted
	// or resumed) and returns a handle the worker closes when the guest
	// stops. The SBX handle is the exec session that keeps the VM booted,
	// since SBX stops a VM after its last exec ends; a driver whose guests
	// stay up on their own returns a no-op handle.
	Prepare(context.Context, string) (io.Closer, error)
	Exec(context.Context, string, string, ...string) (string, error)
	Copy(context.Context, string, string, string) error
	// Address is where the core reaches the guest's published ports and
	// its own services: the loopback address for SBX, a pod IP later.
	Address(context.Context, string) (string, error)
	// Stream launches the agent app server; the SBX driver first installs
	// the broker CA the guest does not trust yet.
	Stream(context.Context, string, RunSpec) (io.ReadWriteCloser, error)
	Stop(context.Context, string) error
	Remove(context.Context, string) error
	// Publish exposes guestPort to the core and returns where. The driver
	// chooses the endpoint and calls reserve with it before the publication
	// takes effect, so the caller can record it durably first: a crash
	// between the record and the publication is reconciled on resume, the
	// reverse would leave an unregistered mapping.
	Publish(ctx context.Context, name string, guestPort int, reserve func(PortMapping) error) (PortMapping, error)
	Unpublish(context.Context, string, PortMapping) error
	Mappings(context.Context, string) ([]PortMapping, error)
}

// NoResidency is the Prepare handle of a driver whose guests stay resident
// on their own.
type NoResidency struct{}

func (NoResidency) Close() error { return nil }

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

// Address is the loopback address: SBX publishes guest ports on the host's
// 127.0.0.1 and the agent's servers are reached through them.
func (d *sbxRuntime) Address(context.Context, string) (string, error) { return loopbackAddress, nil }

const loopbackAddress = "127.0.0.1"

func (d *sbxRuntime) Stop(ctx context.Context, name string) error {
	return command(ctx, d.worker.Executable, "stop", name).Run()
}

// Remove deletes the sandbox, its container state and scoped secrets. The
// runtime name is Warden-derived, never guest-supplied.
func (d *sbxRuntime) Remove(ctx context.Context, name string) error {
	return command(ctx, d.worker.Executable, "rm", "--force", name).Run()
}

// Publish maps guestPort to a free loopback port, retrying the allocation
// when sbx refuses the port it was offered.
func (d *sbxRuntime) Publish(ctx context.Context, name string, guestPort int, reserve func(PortMapping) error) (PortMapping, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var listener net.Listener
		listener, err = net.Listen("tcp4", loopbackAddress+":0")
		if err != nil {
			return PortMapping{}, err
		}
		m := PortMapping{Address: loopbackAddress, Port: listener.Addr().(*net.TCPAddr).Port, GuestPort: guestPort}
		listener.Close()
		if err = reserve(m); err != nil {
			return PortMapping{}, err
		}
		if err = command(ctx, d.worker.Executable, "ports", name, "--publish", sbxPortSpec(m)).Run(); err == nil {
			return m, nil
		}
	}
	return PortMapping{}, err
}
func (d *sbxRuntime) Unpublish(ctx context.Context, name string, m PortMapping) error {
	if m.Address != loopbackAddress {
		return errors.New("SBX publications are loopback only")
	}
	return command(ctx, d.worker.Executable, "ports", name, "--unpublish", sbxPortSpec(m)).Run()
}
func sbxPortSpec(m PortMapping) string {
	return net.JoinHostPort(m.Address, strconv.Itoa(m.Port)) + ":" + strconv.Itoa(m.GuestPort) + "/tcp4"
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

// installCA makes the guest trust the broker's public gateway CA. It is the
// SBX way of delivering trust (copy plus update-ca-certificates as root);
// the guest base image mounts its bundle instead.
func (d *sbxRuntime) installCA(ctx context.Context, name, certificate string) error {
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
func (d *sbxRuntime) Stream(ctx context.Context, name string, run RunSpec) (io.ReadWriteCloser, error) {
	broker := run.Broker
	if broker.CACertificate != "" && !run.TrustsCA {
		if err := d.installCA(ctx, name, broker.CACertificate); err != nil {
			return nil, err
		}
	}
	paths := run.Paths.orDefaults()
	dir := run.Directory
	ctx, cancel := context.WithCancel(ctx)
	cmd := command(ctx, d.worker.Executable, "exec", "-i", "-w", dir, name, "env", "-u", "OPENAI_API_KEY", "-u", "OPENAI_BASE_URL", "-u", "CODEX_API_KEY", "HTTP_PROXY="+broker.ProxyURL, "HTTPS_PROXY="+broker.ProxyURL, "http_proxy="+broker.ProxyURL, "https_proxy="+broker.ProxyURL, "WARDEN_API_KEY="+broker.APIKeyPlaceholder, "WORKSPACE_DOCUMENT_API_URL="+broker.DocumentBaseURL, paths.Codex+"/bin/codex", "app-server", "--listen", "stdio://", "-c", `model_provider="warden"`, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`, "-c", `model_providers.warden.base_url=`+strconv.Quote(broker.ProviderBaseURL), "-c", `model_providers.warden.name="Warden"`, "-c", `model_providers.warden.wire_api="responses"`, "-c", `model_providers.warden.env_key="WARDEN_API_KEY"`)
	if broker.Provider == "claude" {
		args := []string{"exec", "-i", "-w", dir, name, "env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN=" + broker.APIKeyPlaceholder, "ANTHROPIC_BASE_URL=" + broker.ProviderBaseURL, "HTTP_PROXY=" + broker.ProxyURL, "HTTPS_PROXY=" + broker.ProxyURL, "http_proxy=" + broker.ProxyURL, "https_proxy=" + broker.ProxyURL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", paths.Claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", "default", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{"warden":{"type":"sdk","name":"warden"}}}`, "--setting-sources=", "--append-system-prompt", "You work inside a Warden-managed sandbox. Warden controls external access and tool approvals. Never request or expose host credentials. Keep files in the workspace. For web previews, start a detached server on 0.0.0.0 and use the Warden MCP preview_attach or sandbox_bind_port tool; Warden chooses the URL."}
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

// Prepare holds a credential-free session independently of the agent stream.
// SBX auto-stops a VM after its last exec/SSH session disconnects, even when
// detached guest processes and published ports still exist, so the handle
// lives as long as the guest is resident, not as long as the caller's ctx.
func (d *sbxRuntime) Prepare(_ context.Context, name string) (io.Closer, error) {
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
