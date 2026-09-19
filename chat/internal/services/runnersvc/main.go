// Package runnersvc is `warden runner`, the sandbox worker: it drives sbx
// on the sbx shapes and sandbox pods on Kubernetes (runtime.kind).
package runnersvc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
	"warden/chat/internal/handshake"
	"warden/chat/internal/hostinfo"
	"warden/chat/internal/kube"
	"warden/chat/internal/release"
	"warden/chat/internal/sandbox"
	sandboxkube "warden/chat/internal/sandbox/kube"
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
	root := fs.String("root", "", "Private persistent worker directory (paths.state/runner)")
	socket := fs.String("socket", "", "Private Unix socket (services.runner.listen, default paths.state/runner/worker.sock)")
	sbx := fs.String("sbx", "", "Pinned SBX executable (sbx.executable)")
	template := fs.String("template", "", "Pinned SBX template for the verified Warden launch profile (sbx.guestImage@sbx.guestImageDigest; default the stock shell template)")
	runtimeDir := fs.String("runtime-dir", "", "Pinned Linux Codex vendor bundle directory (runtimes.codex)")
	wardenSocket := fs.String("warden-socket", "", "Private Warden enforcement socket; missing/unverified enforcement denies execution (services.policy.address, default paths.state/policy/sbx-control.sock)")
	idle := fs.Duration("idle-timeout", 30*time.Minute, "Stop environments this long after the last chat activity (a turn's end, a message, a command, a preview; sandboxes.stopAfterIdleMinutes)")
	memoryMB := fs.Int("sandbox-memory-mb", 1536, "Memory in MiB for newly created chat sandboxes, 512–16384 (sandboxes.memoryMB)")
	residents := fs.Int("max-resident", 2, "Maximum resident sandbox environments (sandboxes.maxRunning)")
	spares := fs.Int("spare-sandboxes", 1, "Booted spare guests kept ready for new environments, beside max-resident (sandboxes.warmSpares)")
	parallel := fs.Int("parallel", sandbox.MaxParallelSessions, "Maximum simultaneous task sandboxes")
	retained := fs.Int("retained", 32, "Maximum retained task sandboxes (sandboxes.keepStopped)")
	tlsListen := fs.String("tls-listen", "", "Mutual-TLS host:port to listen on instead of the Unix socket (services.runner.listen as tls://)")
	tlsCA := fs.String("tls-ca", "", "CA every peer is verified against (tls.caFile)")
	tlsCert := fs.String("tls-cert", "", "This runner's certificate (tls.certFile)")
	tlsKey := fs.String("tls-key", "", "This runner's private key (tls.keyFile)")
	kubeconfig := fs.String("kubeconfig", "", "Kubernetes kind only: reach the API server through this kubeconfig instead of the pod's service account (development and tests)")
	if err := services.ParseFlags(fs, args); err != nil {
		return err
	}
	if *version {
		fmt.Println(handshake.Self("warden-runner"))
		return nil
	}
	s, err := resolveSettings(fs, runnerFlags{configPath: configPath, root: root, socket: socket, wardenSocket: wardenSocket, tlsListen: tlsListen, tlsCA: tlsCA, tlsCert: tlsCert, tlsKey: tlsKey, sbx: sbx, template: template, runtimeDir: runtimeDir, claudePath: claudePath, idle: idle, memoryMB: memoryMB, residents: residents, spares: spares, retained: retained})
	if err != nil {
		slog.Error("configuration", "error", err)
		return services.ExitCode(1)
	}
	root, sbx, template, runtimeDir, claudePath = &s.root, &s.sbx, &s.template, &s.runtimeDir, &s.claudePath
	idle, memoryMB, residents, spares, retained = &s.idle, &s.memoryMB, &s.residents, &s.spares, &s.retained
	// Bug reports (docs/bug-reporting-plan.md): a recovered panic in a
	// worker op or the preview server is drafted for the launcher to show.
	bugreport.SetDefault(bugreport.New(s.cfg, s.configPath, bugreport.ComponentRunner))
	limits := sizeLimits(s)
	driver, err := runtimeDriver(s, limits, *kubeconfig)
	if err != nil {
		slog.Error("configuration", "error", err)
		return services.ExitCode(1)
	}
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
	if s.previewListen != "" {
		// The shared preview server (services.runner.previews): the chat
		// dials it as https://<address host>/<publication ID> with its
		// client certificate, so only the chat is admitted.
		pl, err := transport.Listen(s.previewListen, transport.ListenOptions{TLS: s.tls, Peers: []string{transport.Chat}})
		if err != nil {
			slog.Error("preview listen", "error", err)
			return services.ExitCode(1)
		}
		defer pl.Close()
		w.PreviewListener, w.PreviewAddress = pl, s.previewAddress
		slog.Info("previews served over mutual TLS", "listen", s.previewListen, "address", s.previewAddress)
	}
	w.Runtime = driver(w)
	w.RuntimeDir = *runtimeDir
	w.ClaudePath = *claudePath
	w.Spares = *spares
	w.Gate = &sandbox.PolicyEnforcement{Address: s.policy, TLS: s.tls}
	w.IdleTimeout = *idle
	w.MaxResident = *residents
	w.MemoryMB = *memoryMB
	w.Limits = limits
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

// kubernetesPrepareTimeout bounds one prepare on Kubernetes: a node may
// have to be provisioned and the guest image pulled before the pod runs.
const kubernetesPrepareTimeout = 10 * time.Minute

// sizeLimits is the size offer of the configured runtime kind: what the
// host allows on SBX, what the configuration says on Kubernetes.
func sizeLimits(s settings) sandbox.ResourceLimits {
	if s.cfg.RuntimeKind() == config.RuntimeKubernetes {
		return kubernetesResourceLimits(s.cfg.Sandboxes, s.memoryMB)
	}
	hostMemoryMB, cores := hostinfo.Capacity()
	return resourceLimits(s.cfg.Sandboxes, s.memoryMB, hostMemoryMB, cores)
}

// runtimeDriver selects the RuntimeDriver of the configured runtime kind.
// The sbx shapes get the SBX driver over the worker's executable and
// template; the Kubernetes kind gets the pod driver over the in-cluster
// service account (or the kubeconfig named for development), configured
// from the kubernetes section and the runner's default sandbox size
// (docs/warden-kubernetes-plan.md, work item 4). The runner's other
// settings (spares, idle timeout, residents) apply to both.
func runtimeDriver(s settings, limits sandbox.ResourceLimits, kubeconfig string) (func(*sandbox.Worker) sandbox.RuntimeDriver, error) {
	switch kind := s.cfg.RuntimeKind(); kind {
	case config.RuntimeSBX:
		return sandbox.NewSBXRuntime, nil
	case config.RuntimeKubernetes:
		k := s.cfg.Kubernetes
		if k == nil {
			return nil, errors.New("runtime.kind \"kubernetes\" requires the kubernetes section")
		}
		var kcfg *kube.Config
		var err error
		if kubeconfig != "" {
			kcfg, err = kube.LoadKubeconfig(kubeconfig)
		} else {
			kcfg, err = kube.InClusterConfig()
		}
		if err != nil {
			return nil, fmt.Errorf("kubernetes API access: %w", err)
		}
		client, err := kube.NewClient(kcfg)
		if err != nil {
			return nil, fmt.Errorf("kubernetes API client: %w", err)
		}
		driver, err := sandboxkube.New(client, kubernetesOptions(k, limits.Default))
		if err != nil {
			return nil, err
		}
		return func(w *sandbox.Worker) sandbox.RuntimeDriver {
			// The driver is also the cluster view the owner sees, and a
			// pod may wait for a node to join before it can start.
			w.Cluster = driver
			w.PrepareTimeout = kubernetesPrepareTimeout
			return driver
		}, nil
	default:
		return nil, errors.New("unknown runtime.kind " + kind)
	}
}

// kubernetesOptions maps the kubernetes section and the default sandbox
// size (what a spare is booted at) to the driver's options.
func kubernetesOptions(k *config.Kubernetes, size sandbox.Resources) sandboxkube.Options {
	return sandboxkube.Options{
		Namespace:          k.Namespace,
		Tier:               k.Tier,
		RuntimeClass:       k.RuntimeClass,
		GuestImage:         k.GuestImage,
		GuestImageDigest:   k.GuestImageDigest,
		StorageClass:       k.StorageClass,
		WorkspaceSizeGi:    k.WorkspaceSizeGi,
		TrustConfigMap:     k.TrustConfigMap,
		MemoryMB:           size.MemoryMB,
		CPUMillis:          size.CPUMilli,
		NodeSelector:       k.NodeSelector,
		Tolerations:        k.Tolerations,
		SparePriorityClass: k.SparePriorityClass,
	}
}
