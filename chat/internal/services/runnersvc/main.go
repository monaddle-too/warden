// Package runnersvc is `warden runner`, the sandbox worker that drives sbx.
package runnersvc

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"warden/chat/internal/handshake"
	"warden/chat/internal/hostinfo"
	"warden/chat/internal/release"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/services"
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
	socket := fs.String("socket", "", "Private Unix socket (paths.state/runner/worker.sock)")
	sbx := fs.String("sbx", "", "Pinned SBX executable (sbx.executable)")
	template := fs.String("template", "", "Pinned SBX template for the verified Warden launch profile (sbx.guestImage@sbx.guestImageDigest; default the stock shell template)")
	runtimeDir := fs.String("runtime-dir", "", "Pinned Linux Codex vendor bundle directory (runtimes.codex)")
	wardenSocket := fs.String("warden-socket", "", "Private Warden enforcement socket; missing/unverified enforcement denies execution (paths.state/policy/sbx-control.sock)")
	idle := fs.Duration("idle-timeout", 15*time.Minute, "Stop environments after this much trusted user inactivity (sandboxes.stopAfterIdleMinutes)")
	memoryMB := fs.Int("sandbox-memory-mb", 1536, "Memory in MiB for newly created chat sandboxes, 512–16384 (sandboxes.memoryMB)")
	residents := fs.Int("max-resident", 2, "Maximum resident sandbox environments (sandboxes.maxRunning)")
	spares := fs.Int("spare-sandboxes", 1, "Booted spare guests kept ready for new environments, beside max-resident (sandboxes.warmSpares)")
	parallel := fs.Int("parallel", sandbox.MaxParallelSessions, "Maximum simultaneous task sandboxes")
	retained := fs.Int("retained", 32, "Maximum retained task sandboxes (sandboxes.keepStopped)")
	tlsListen := fs.String("tls-listen", "", "Optional loopback TLS address inside a remote runner VM")
	tlsCA := fs.String("tls-ca", "", "CA allowed to authenticate the production dispatcher")
	tlsCert := fs.String("tls-cert", "", "Worker certificate")
	tlsKey := fs.String("tls-key", "", "Worker private key")
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
	s, err := resolveSettings(fs, runnerFlags{configPath: configPath, root: root, socket: socket, wardenSocket: wardenSocket, sbx: sbx, template: template, runtimeDir: runtimeDir, claudePath: claudePath, idle: idle, memoryMB: memoryMB, residents: residents, spares: spares, retained: retained})
	if err != nil {
		slog.Error("configuration", "error", err)
		return services.ExitCode(1)
	}
	root, socket, wardenSocket, sbx, template, runtimeDir, claudePath = &s.root, &s.socket, &s.wardenSocket, &s.sbx, &s.template, &s.runtimeDir, &s.claudePath
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
	if *legacy && (*wardenSocket != "" || *runtimeDir != "") {
		slog.Error("legacy workers cannot accept Warden chat runtime configuration")
		return services.ExitCode(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var l net.Listener
	if *tlsListen != "" {
		host, _, splitErr := net.SplitHostPort(*tlsListen)
		if splitErr != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			slog.Error("worker TLS listener must be loopback-only")
			return services.ExitCode(1)
		}
		config, tlsErr := sandbox.ServerTLS(*tlsCA, *tlsCert, *tlsKey)
		if tlsErr != nil {
			slog.Error("worker TLS configuration failed")
			return services.ExitCode(1)
		}
		l, err = tls.Listen("tcp", *tlsListen, config)
	} else {
		if err = os.MkdirAll(filepath.Dir(*socket), 0750); err != nil {
			slog.Error("socket directory", "error", err)
			return services.ExitCode(1)
		}
		// The root lock prevents a second worker from replacing this socket.
		_ = os.Remove(*socket)
		l, err = net.Listen("unix", *socket)
		if err != nil {
			slog.Error("listen", "error", err)
			return services.ExitCode(1)
		}
		if err = os.Chmod(*socket, 0660); err != nil {
			slog.Error("socket permissions", "error", err)
			return services.ExitCode(1)
		}
	}
	if err != nil {
		slog.Error("worker listen failed", "error", err)
		return services.ExitCode(1)
	}
	w := sandbox.NewWorker(*root, *sbx, *template)
	w.Legacy = *legacy
	w.RuntimeDir = *runtimeDir
	w.ClaudePath = *claudePath
	w.Spares = *spares
	w.Gate = &sandbox.UnixEnforcement{Socket: *wardenSocket}
	w.IdleTimeout = *idle
	w.MaxResident = *residents
	w.MemoryMB = *memoryMB
	hostMemoryMB, cores := hostinfo.Capacity()
	w.Limits = resourceLimits(s.cfg.Sandboxes, *memoryMB, hostMemoryMB, cores)
	if err = w.Limits.Validate(); err != nil {
		slog.Error("invalid sandbox size limits", "error", err)
		return services.ExitCode(1)
	}
	w.Revision, w.Parallel, w.Retained = release.Revision, *parallel, *retained
	if err = w.Serve(ctx, l); err != nil {
		slog.Error("worker stopped", "error", err)
		return services.ExitCode(1)
	}
	return nil
}
