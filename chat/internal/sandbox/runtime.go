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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/release"
)

// RuntimeSpec describes the guest to create: its Warden-derived runtime
// name, the working directory, and for a fork the runtime it is cloned
// from. SandboxID and Generation are the registry identity the worker
// creates it for, absent on a spare (Spare), which has no sandbox yet; a
// driver that labels its guests records them. Resources is its size,
// already resolved against the runner's limits (zero means the worker's
// default, which only the spare pool still relies on).
type RuntimeSpec struct {
	Name, Directory, Source string
	SandboxID, Generation   string
	Spare                   bool
	Resources               Resources
}

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
	// Instructions is the participants' standing instructions, assembled
	// by the chat service (Request.Instructions), appended after Warden's
	// own prompt; "" appends nothing.
	Instructions string
}

// ChatRenderingPrompt tells an agent what the chat makes of its replies,
// so a diagram it is asked for goes into the reply rather than into a
// file or a web page; both providers' prompts carry it.
const ChatRenderingPrompt = "Your replies are shown in Warden's chat as Markdown: fenced code with a language is highlighted, a ```diff fence is shown as a diff, a ```mermaid fence is rendered as a diagram (one that does not parse is shown as source with the error), $$ math is typeset, and ![alt](relative/path) shows an image file from the workspace. To show a diagram, put the ```mermaid fence in the reply itself; do not write a file or a web page for it unless asked."

// WardenSystemPrompt is what Warden itself tells a Claude session, the
// first part of the appended system prompt.
const WardenSystemPrompt = "You work inside a Warden-managed sandbox. Warden controls external access and tool approvals. Never request or expose host credentials. Keep files in the workspace. GitHub repositories are reached through Warden's repository sharing: list_shared_repositories shows what this workspace can clone and read; to clone or read one that is not listed, ask with request_repository_access (contents), never with request_network_access for github.com: a refused git clone means the repository is not shared, not that the network is blocked. For web previews, start a detached server on 0.0.0.0 and use the Warden MCP preview_attach or sandbox_bind_port tool; Warden chooses the URL. " + ChatRenderingPrompt

// MaxInstructions bounds the instructions text one launch appends (every
// participant's blocks together); the chat service caps one person's text
// well below it. The argument travels the exec path as data, and a guest
// argument has a hard size on Linux, so the cap keeps a launch safe.
const MaxInstructions = 96 << 10

// claudeSystemPrompt is the text `--append-system-prompt` carries: Warden's
// own prompt first, then the participants' instructions when there are
// any, a blank line between. An over-long instructions text is cut at the
// cap rather than failing the launch, since the prompt is advice.
func claudeSystemPrompt(run RunSpec) string {
	instructions := strings.TrimSpace(run.Instructions)
	if instructions == "" {
		return WardenSystemPrompt
	}
	if len(instructions) > MaxInstructions {
		instructions = instructions[:MaxInstructions] + "\n[instructions cut at the size limit]"
	}
	return WardenSystemPrompt + "\n\n" + instructions
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
	// Resize gives a created sandbox, running or stopped, a new size. It
	// reports whether the instance was replaced to do it: SBX cannot change
	// a sandbox's limits, so its driver stops the sandbox, snapshots it,
	// recreates it under the same name with the new limits, and the caller
	// treats the sandbox as stopped. A live resize reports false.
	Resize(ctx context.Context, name string, r Resources) (restarted bool, err error)
	// Prepare makes a created guest resident again after a stop (or
	// confirms it is) before the worker's first exec, and returns a handle
	// the worker closes when the guest stops. The SBX handle is the exec
	// session that keeps the VM booted, since SBX stops a VM after its last
	// exec ends and boots it on the next; a driver whose guests are
	// recreated per generation (a pod on the kept workspace) creates one
	// here from the spec and returns a no-op handle.
	Prepare(context.Context, RuntimeSpec) (io.Closer, error)
	Exec(context.Context, string, string, ...string) (string, error)
	Copy(context.Context, string, string, string) error
	// CopyOut copies a guest path to a host path (the reverse of Copy).
	CopyOut(context.Context, string, string, string) error
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

// Reconciler is implemented by a driver whose guests outlive the worker
// process (pods do, SBX VMs stop with their keep-alive session). At startup,
// after the worker has stopped every registered sandbox and removed every
// registered spare, it is handed the runtime names of the registered
// sandboxes, whose workspaces must be kept; anything else it finds under its
// labels is stale.
type Reconciler interface {
	Reconcile(ctx context.Context, registered []string) error
}

// LaunchOptions vary the agent command line per driver.
type LaunchOptions struct {
	// CodexSandboxMode overrides Codex's inner sandbox (plan decision 14
	// after the spike): the gVisor tier passes "danger-full-access" because
	// bubblewrap's network namespace is not available there; empty keeps
	// Codex's default.
	CodexSandboxMode string
}

// AgentCommand is the agent app-server launch both drivers run in the
// guest's working directory: `env` clears the variables an agent would
// otherwise take a credential or endpoint from and sets the brokered
// session's, then the agent program from the manifest paths with the
// Warden-managed provider configuration. Every value is data: the drivers
// pass the list as arguments, never through a shell.
func AgentCommand(run RunSpec, opts LaunchOptions) []string {
	broker := run.Broker
	paths := run.Paths.orDefaults()
	if broker.Provider == "claude" {
		args := append(claudeEnvironment(broker), paths.Claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", "default", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{"warden":{"type":"sdk","name":"warden"}}}`, "--setting-sources=",
			// Rendered fresh on every request: with the CLI's default (on)
			// a resumed conversation keeps the system prompt recorded at
			// its first request, so a relaunch could never change the
			// appended text (the participants' instructions).
			"--system-prompt-snapshot", "off",
			"--append-system-prompt", claudeSystemPrompt(run))
		if broker.Model != "" {
			args = append(args, "--model", broker.Model)
		}
		return append(args, claudeSessionArgs(broker)...)
	}
	args := []string{"env", "-u", "OPENAI_API_KEY", "-u", "OPENAI_BASE_URL", "-u", "CODEX_API_KEY", "HTTP_PROXY=" + broker.ProxyURL, "HTTPS_PROXY=" + broker.ProxyURL, "http_proxy=" + broker.ProxyURL, "https_proxy=" + broker.ProxyURL, "WARDEN_API_KEY=" + broker.APIKeyPlaceholder, "WORKSPACE_DOCUMENT_API_URL=" + broker.DocumentBaseURL, paths.Codex + "/bin/codex", "app-server", "--listen", "stdio://", "-c", `model_provider="warden"`, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`, "-c", `model_providers.warden.base_url=` + strconv.Quote(broker.ProviderBaseURL), "-c", `model_providers.warden.name="Warden"`, "-c", `model_providers.warden.wire_api="responses"`, "-c", `model_providers.warden.env_key="WARDEN_API_KEY"`,
		// The model's reasoning summaries stream as items, so the chat can
		// show that the model is thinking during the long first reply of a
		// reasoning model instead of nothing; "auto" leaves whole turns
		// without one.
		"-c", `model_reasoning_summary="detailed"`}
	if opts.CodexSandboxMode != "" {
		args = append(args, "-c", "sandbox_mode="+strconv.Quote(opts.CodexSandboxMode))
	}
	return args
}

// claudeEnvironment is the `env` prefix every Claude launch in a guest
// takes: the variables an agent would otherwise take a credential or
// endpoint from cleared, the brokered session's set.
func claudeEnvironment(broker BrokerConfig) []string {
	return []string{"env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN=" + broker.APIKeyPlaceholder, "ANTHROPIC_BASE_URL=" + broker.ProviderBaseURL, "HTTP_PROXY=" + broker.ProxyURL, "HTTPS_PROXY=" + broker.ProxyURL, "http_proxy=" + broker.ProxyURL, "https_proxy=" + broker.ProxyURL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1"}
}

// claudeSessionArgs are the flags that pick the session a Claude launch
// continues and how: `--resume` for the chat's recorded session, with
// `--fork-session` when the chat was forked from another (the CLI copies
// the session under a new id and reports it in system/init), and the
// output style as the settings key the CLI reads it from (there is no
// flag for it on 2.1.272; docs/claude-parity.md, item 15). The style
// name is JSON-encoded, so it is data whatever it contains.
func claudeSessionArgs(broker BrokerConfig) []string {
	var args []string
	if broker.ThreadID != "" {
		args = append(args, "--resume", broker.ThreadID)
		if broker.ForkSession {
			args = append(args, "--fork-session")
		}
	}
	if broker.OutputStyle != "" {
		settings, _ := json.Marshal(map[string]string{"outputStyle": broker.OutputStyle})
		args = append(args, "--settings", string(settings))
	}
	return args
}

type sbxRuntime struct{ worker *Worker }

// NewSBXRuntime is the RuntimeDriver of the sbx shapes: it drives the
// worker's pinned sbx executable and template.
func NewSBXRuntime(w *Worker) RuntimeDriver { return &sbxRuntime{worker: w} }

// ErrRuntimeKindUnsupported is returned for a runtime kind this build has
// no driver for.
var ErrRuntimeKindUnsupported = errors.New("runtime kind is not implemented in this build")

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
	if s.Source != "" {
		return d.createFrom(ctx, s)
	}
	args, err := d.createArgs(s.Name, s.Resources, d.worker.Template)
	if err != nil {
		return err
	}
	Report(ctx, "creating the sandbox VM from the guest template")
	return runCreate(ctx, d.worker.Executable, append(args, "shell"))
}

// createFrom creates the sandbox as a copy of Source's disk (a workspace
// copy, docs/claude-parity.md R2.13): SBX has no sandbox clone, so the
// source is saved as a template (`sbx template save`, which refuses a
// running sandbox — the worker stops the source first), the copy is
// created from that template at the requested size with the deny-all
// rule, and the template is dropped. The snapshot carries the guest's
// root filesystem without /tmp, so the runtimes the worker installed
// there are installed again at the copy's first prepare. The copy boots
// with its creation, as any created sandbox does.
func (d *sbxRuntime) createFrom(ctx context.Context, s RuntimeSpec) error {
	tag := "warden-copy-" + strings.ToLower(s.Name)
	args, err := d.createArgs(s.Name, s.Resources, tag)
	if err != nil {
		return err
	}
	sbx := func(a ...string) error {
		cmd := command(ctx, d.worker.Executable, a...)
		var stderr bytes.Buffer
		cmd.Stderr = &limitedWriter{W: &stderr, N: 4096}
		if err := cmd.Run(); err != nil {
			if detail := strings.TrimSpace(stderr.String()); detail != "" {
				return fmt.Errorf("sbx %s: %w: %s", a[0], err, detail)
			}
			return fmt.Errorf("sbx %s: %w", a[0], err)
		}
		return nil
	}
	Report(ctx, "saving a snapshot of the source sandbox "+s.Source)
	if err := sbx("template", "save", s.Source, tag); err != nil {
		return fmt.Errorf("SBX copy failed: %w", err)
	}
	// The template is only disk once the copy exists, and only a leftover
	// when the creation failed; either way it goes.
	defer func() { _ = sbx("template", "rm", tag) }()
	Report(ctx, "creating the sandbox VM from the snapshot")
	if err := runCreate(ctx, d.worker.Executable, append(args, "shell")); err != nil {
		return fmt.Errorf("SBX copy failed: %w", err)
	}
	return nil
}

// ImageDigest is the digest of the image the sandbox runs, as `sbx
// inspect` reports it: the guest image for a sandbox created from the
// template, the snapshot's own digest for one created from a saved
// template (a copy, or a resize's regeneration). The worker passes a
// snapshot's digest to the policy service, whose inspector pins the
// image (policy/sbxinspector.go allowedImage), so a sandbox the runner
// derived from a verified guest is accepted for what it is.
func (d *sbxRuntime) ImageDigest(ctx context.Context, name string) (string, error) {
	cmd := command(ctx, d.worker.Executable, "inspect", name, "--json")
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{W: &out, N: 1 << 20}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sbx inspect: %w", err)
	}
	var details struct {
		ImageDigest string `json:"image_digest"`
	}
	if err := json.Unmarshal(out.Bytes(), &details); err != nil {
		return "", errors.New("invalid sandbox inspection")
	}
	if !imageDigestShape.MatchString(details.ImageDigest) {
		return "", errors.New("sandbox inspection reports no image digest")
	}
	return details.ImageDigest, nil
}

// imageDigestShape is a container image digest as sbx reports it.
var imageDigestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageInspector is a driver that can say which image a sandbox runs; the
// worker records the digest of a sandbox derived from a snapshot (a copy,
// a regeneration) for the policy service's image pin.
type ImageInspector interface {
	ImageDigest(ctx context.Context, name string) (string, error)
}

// createArgs is the `sbx create` invocation for a sandbox of the given size
// from a template: one deny-all network rule (the gateway is reached through
// the policy service's allowances), no host skills, whole CPUs since SBX
// takes no fraction.
func (d *sbxRuntime) createArgs(name string, r Resources, template string) ([]string, error) {
	r = r.Fill(d.worker.Limits.Default)
	if r.MemoryMB == 0 {
		r.MemoryMB = d.worker.MemoryMB
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = 1536
	}
	if r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB {
		return nil, fmt.Errorf("sandbox memory must be %d–%d MiB", MinMemoryMB, MaxMemoryMB)
	}
	return []string{"create", "--name", name, "--cpus", strconv.Itoa(CPUsFromSpec(r.CPUMilli)), "--memory", fmt.Sprintf("%dm", r.MemoryMB), "--template", template, "--deny-network", "**", "--no-share-skills"}, nil
}

// runCreate runs an `sbx create`, keeping the daemon's stderr for the error.
func runCreate(ctx context.Context, executable string, args []string) error {
	// The daemon's own output (not agent-controlled) is worth the log line
	// when creation fails: it names the image or resource that was refused.
	cmd := command(ctx, executable, args...)
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

// Resize regenerates the sandbox at the new size: SBX fixes a sandbox's
// limits at creation, so the stopped sandbox is saved as a template, removed,
// and created again under the same name from that template (the whole
// filesystem carries over, the deny-all rule is set again), and the
// template is dropped. Nothing is removed before the template exists, so a
// failure up to that point leaves the old sandbox in place; a failure after
// it leaves the template, which the next Resize or Create finds by name.
func (d *sbxRuntime) Resize(ctx context.Context, name string, r Resources) (bool, error) {
	tag := "warden-resize-" + strings.ToLower(name)
	args, err := d.createArgs(name, r, tag)
	if err != nil {
		return false, err
	}
	sbx := func(a ...string) error {
		cmd := command(ctx, d.worker.Executable, a...)
		var stderr bytes.Buffer
		cmd.Stderr = &limitedWriter{W: &stderr, N: 4096}
		if err := cmd.Run(); err != nil {
			if detail := strings.TrimSpace(stderr.String()); detail != "" {
				return fmt.Errorf("sbx %s: %w: %s", a[0], err, detail)
			}
			return fmt.Errorf("sbx %s: %w", a[0], err)
		}
		return nil
	}
	if err := sbx("stop", name); err != nil {
		return false, fmt.Errorf("SBX resize failed: %w", err)
	}
	if err := sbx("template", "save", name, tag); err != nil {
		return false, fmt.Errorf("SBX resize failed: %w", err)
	}
	if err := sbx("rm", "--force", name); err != nil {
		return false, fmt.Errorf("SBX resize failed: %w", err)
	}
	if err := runCreate(ctx, d.worker.Executable, append(args, "shell")); err != nil {
		return true, fmt.Errorf("SBX resize failed after removing the old sandbox (template %s keeps its files): %w", tag, err)
	}
	// A leftover template is only disk; the resize succeeded regardless.
	_ = sbx("template", "rm", tag)
	return true, nil
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
func (d *sbxRuntime) CopyOut(ctx context.Context, name, source, target string) error {
	return command(ctx, d.worker.Executable, "cp", name+":"+source, target).Run()
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
	ctx, cancel := context.WithCancel(ctx)
	cmd := command(ctx, d.worker.Executable, append([]string{"exec", "-i", "-w", run.Directory, name}, AgentCommand(run, LaunchOptions{})...)...)
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
func (d *sbxRuntime) Prepare(parent context.Context, spec RuntimeSpec) (io.Closer, error) {
	Report(parent, "booting the sandbox VM")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := command(ctx, d.worker.Executable, "exec", "-i", spec.Name, "sh", "-c", "printf 'ready\\n'; exec cat >/dev/null")
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
