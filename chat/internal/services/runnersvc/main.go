// Package runnersvc is `warden runner`, the sandbox worker that drives sbx.
package runnersvc

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"warden/chat/internal/handshake"
	"warden/chat/internal/release"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/services"
	"warden/chat/internal/transport"
)

// Main runs the runner service and returns the exit status.
func Main(args []string) int { return services.Run(run, args) }

func run(args []string) error {
	fs := flag.NewFlagSet("warden runner", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional warden.json (default $WARDEN_CONFIG); flags override its computed defaults and must agree with its values")
	claudePath := fs.String("claude-path", "", "Pinned Linux Claude executable (runtimes.claude)")
	version := fs.Bool("version", false, "Print worker revision and protocol version")
	legacy := fs.Bool("legacy", false, "Use the separate protocol 1 task worker (does not provide Warden chat execution)")
	root := fs.String("root", "", "Private persistent worker directory (paths.state/runner)")
	socket := fs.String("socket", "", "Private Unix socket (services.runner.listen, default paths.state/runner/worker.sock)")
	sbx := fs.String("sbx", "", "Pinned SBX executable (sbx.executable)")
	template := fs.String("template", "", "Pinned SBX template for the verified Warden launch profile (sbx.guestImage@sbx.guestImageDigest; default the stock shell template)")
	runtimeDir := fs.String("runtime-dir", "", "Pinned Linux Codex vendor bundle directory (runtimes.codex)")
	wardenSocket := fs.String("warden-socket", "", "Private Warden enforcement socket; missing/unverified enforcement denies execution (services.policy.address, default paths.state/policy/sbx-control.sock)")
	idle := fs.Duration("idle-timeout", 15*time.Minute, "Stop environments after this much trusted user inactivity (sandboxes.stopAfterIdleMinutes)")
	memoryMB := fs.Int("sandbox-memory-mb", 1536, "Memory in MiB for newly created chat sandboxes, 512–16384 (sandboxes.memoryMB)")
	residents := fs.Int("max-resident", 2, "Maximum resident sandbox environments (sandboxes.maxRunning)")
	spares := fs.Int("spare-sandboxes", 1, "Booted spare guests kept ready for new environments, beside max-resident (sandboxes.warmSpares)")
	parallel := fs.Int("parallel", sandbox.MaxParallelSessions, "Maximum simultaneous task sandboxes")
	retained := fs.Int("retained", 32, "Maximum retained task sandboxes (sandboxes.keepStopped)")
	tlsListen := fs.String("tls-listen", "", "Mutual-TLS host:port to listen on instead of the Unix socket (services.runner.listen as tls://)")
	tlsCA := fs.String("tls-ca", "", "CA every peer is verified against (tls.caFile)")
	tlsCert := fs.String("tls-cert", "", "This runner's certificate (tls.certFile)")
	tlsKey := fs.String("tls-key", "", "This runner's private key (tls.keyFile)")
	if err := services.ParseFlags(fs, args); err != nil {
		return err
	}
	if *version {
		self := handshake.Self("warden-runner")
		if *legacy {
			self.Protocol = 1
		}
		fmt.Println(self)
		return nil
	}
	s, err := resolveSettings(fs, runnerFlags{configPath: configPath, root: root, socket: socket, wardenSocket: wardenSocket, tlsListen: tlsListen, tlsCA: tlsCA, tlsCert: tlsCert, tlsKey: tlsKey, sbx: sbx, template: template, runtimeDir: runtimeDir, claudePath: claudePath, idle: idle, memoryMB: memoryMB, residents: residents, spares: spares, retained: retained})
	if err != nil {
		slog.Error("configuration", "error", err)
		return services.ExitCode(1)
	}
	root, sbx, template, runtimeDir, claudePath = &s.root, &s.sbx, &s.template, &s.runtimeDir, &s.claudePath
	idle, memoryMB, residents, spares, retained = &s.idle, &s.memoryMB, &s.residents, &s.spares, &s.retained
	unlock, lockErr := sandbox.LockRoot(*root)
	if lockErr != nil {
		slog.Error("worker root lock", "error", lockErr)
		return services.ExitCode(1)
	}
	defer unlock()
	if *memoryMB < 512 || *memoryMB > 16384 || *parallel < 1 || *parallel > 8 || *retained < *parallel || *retained > 32 {
		slog.Error("invalid worker limits")
		return services.ExitCode(1)
	}
	if *legacy && (s.policy != "" || *runtimeDir != "") {
		slog.Error("legacy workers cannot accept Warden chat runtime configuration")
		return services.ExitCode(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	address, err := transport.Parse(s.listen)
	if err != nil {
		slog.Error("listen", "error", err)
		return services.ExitCode(1)
	}
	if address.Scheme == transport.SchemeUnix {
		if err = os.MkdirAll(filepath.Dir(address.Path), 0750); err != nil {
			slog.Error("socket directory", "error", err)
			return services.ExitCode(1)
		}
	}
	// The root lock prevents a second worker from replacing this socket.
	// Over tls:// only the chat is admitted.
	l, err := transport.Listen(s.listen, transport.ListenOptions{Mode: 0660, TLS: s.tls, Peers: []string{transport.Chat}})
	if err != nil {
		slog.Error("listen", "error", err)
		return services.ExitCode(1)
	}
	w := sandbox.NewWorker(*root, *sbx, *template)
	w.Legacy = *legacy
	w.RuntimeDir = *runtimeDir
	w.ClaudePath = *claudePath
	w.Spares = *spares
	w.Gate = &sandbox.PolicyEnforcement{Address: s.policy, TLS: s.tls}
	w.IdleTimeout = *idle
	w.MaxResident = *residents
	w.MemoryMB = *memoryMB
	w.Revision, w.Parallel, w.Retained = release.Revision, *parallel, *retained
	if err = w.Serve(ctx, l); err != nil {
		slog.Error("worker stopped", "error", err)
		return services.ExitCode(1)
	}
	return nil
}
