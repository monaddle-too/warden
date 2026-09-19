package runnersvc

import (
	"flag"
	"os"
	"os/exec"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/hostinfo"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/transport"
)

// settings are the effective runner values after reconciling the legacy
// flags with the optional warden.json. Each flag maps to one field; a flag
// that disagrees with a loaded file is an error.
type settings struct {
	cfg        config.Config
	configPath string         // the warden.json the settings came from ("" for flags only)
	root       string         // paths.state/runner
	listen     string         // services.runner.listen (unix://paths.state/runner/worker.sock)
	policy     string         // services.policy.address (unix://paths.state/policy/sbx-control.sock)
	tls        *transport.TLS // tls.*, present when any transport URL is tls://
	sbx        string         // sbx.executable
	template   string         // sbx.guestImage@sbx.guestImageDigest
	runtimeDir string         // runtimes.codex
	claudePath string         // runtimes.claude
	idle       time.Duration  // sandboxes.stopAfterIdleMinutes
	memoryMB   int            // sandboxes.memoryMB
	residents  int            // sandboxes.maxRunning
	spares     int            // sandboxes.warmSpares
	retained   int            // sandboxes.keepStopped
	namePrefix string         // sandboxes.namePrefix
	// previewListen and previewAddress are services.runner.previews, the
	// shared mutual-TLS preview server and its advertised address; both ""
	// on the sbx shapes (per-publication loopback listeners). File only.
	previewListen, previewAddress string
}

type runnerFlags struct {
	configPath, root, socket, wardenSocket, sbx, template, runtimeDir, claudePath *string
	namePrefix                                                                    *string
	tlsListen, tlsCA, tlsCert, tlsKey                                             *string
	idle                                                                          *time.Duration
	memoryMB, residents, spares, retained                                         *int
}

func resolveSettings(fs *flag.FlagSet, f runnerFlags) (settings, error) {
	cfg, source, err := config.Resolve(*f.configPath, *f.root)
	if err != nil {
		return settings{}, err
	}
	o := config.NewOverrides(fs, source)
	s := settings{cfg: cfg, configPath: source}
	s.root = config.Override(o, "root", *f.root, "paths.state (runner directory)", cfg.RunnerState())
	// The listener is the Unix socket of --socket or the mutual-TLS port of
	// --tls-listen, both against services.runner.listen.
	s.listen = config.Override(o, "socket", "unix://"+str(f.socket), "services.runner.listen", cfg.RunnerListen())
	if o.Set("tls-listen") {
		s.listen = config.Override(o, "tls-listen", "tls://"+str(f.tlsListen), "services.runner.listen", cfg.RunnerListen())
	}
	s.policy = config.Override(o, "warden-socket", "unix://"+str(f.wardenSocket), "services.policy.address", cfg.PolicyAddress())
	s.previewListen, s.previewAddress = cfg.RunnerPreviewListen(), cfg.RunnerPreviewAddress()
	s.tls = cfg.TransportTLS()
	if o.Set("tls-ca") || o.Set("tls-cert") || o.Set("tls-key") {
		var current transport.TLS
		if s.tls != nil {
			current = *s.tls
		}
		s.tls = &transport.TLS{
			CAFile:   config.Override(o, "tls-ca", str(f.tlsCA), "tls.caFile", current.CAFile),
			CertFile: config.Override(o, "tls-cert", str(f.tlsCert), "tls.certFile", current.CertFile),
			KeyFile:  config.Override(o, "tls-key", str(f.tlsKey), "tls.keyFile", current.KeyFile),
		}
	}
	s.sbx = config.Override(o, "sbx", *f.sbx, "sbx.executable", cfg.SBX.Executable)
	s.template = config.Override(o, "template", *f.template, "sbx.guestImage", cfg.GuestTemplate())
	s.runtimeDir = config.Override(o, "runtime-dir", *f.runtimeDir, "runtimes.codex", cfg.Runtimes.Codex)
	s.claudePath = config.Override(o, "claude-path", *f.claudePath, "runtimes.claude", cfg.Runtimes.Claude)
	s.idle = config.Override(o, "idle-timeout", *f.idle, "sandboxes.stopAfterIdleMinutes", time.Duration(cfg.Sandboxes.StopAfterIdleMinutes)*time.Minute)
	s.memoryMB = config.Override(o, "sandbox-memory-mb", *f.memoryMB, "sandboxes.memoryMB", cfg.Sandboxes.MemoryMB)
	s.residents = config.Override(o, "max-resident", *f.residents, "sandboxes.maxRunning", cfg.Sandboxes.MaxRunning)
	s.spares = config.Override(o, "spare-sandboxes", *f.spares, "sandboxes.warmSpares", cfg.Sandboxes.WarmSpares)
	s.retained = config.Override(o, "retained", *f.retained, "sandboxes.keepStopped", cfg.Sandboxes.KeepStopped)
	s.namePrefix = config.Override(o, "sandbox-name-prefix", str(f.namePrefix), "sandboxes.namePrefix", cfg.Sandboxes.NamePrefix)
	if err := o.Err(); err != nil {
		return settings{}, err
	}
	if s.sbx == "" {
		// Neither flag nor file named the executable (the installer records
		// it as sbx.executable); look where the old default and Homebrew put it.
		s.sbx, _ = findSBX(exec.LookPath, func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
		})
	}
	return s, nil
}

// str reads an optional flag pointer (tests leave the TLS flags unset).
func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var findSBX = hostinfo.FindSBX

// resourceLimits is the size offer for SBX: the configured default, a
// ceiling from the configuration or else from the host (75 % of its memory
// and all its cores, what SBX itself allows a sandbox), whole CPUs only
// (SBX takes no fraction, so a fractional setting rounds up), and a resize
// that restarts the sandbox.
func resourceLimits(s config.Sandboxes, defaultMemoryMB, hostMemoryMB, cores int) sandbox.ResourceLimits {
	// An unset value stays 0 so the fallbacks below apply: CPUsFromSpec
	// would turn it into one CPU and pin the ceiling there.
	wholeCPUs := func(cpus float64) int {
		milli := sandbox.CPUMilli(cpus)
		if milli == 0 {
			return 0
		}
		return sandbox.CPUsFromSpec(milli) * 1000
	}
	l := sandbox.ResourceLimits{CPUStepMilli: 1000, Restart: true}
	l.Default = sandbox.Resources{CPUMilli: wholeCPUs(s.CPUs), MemoryMB: defaultMemoryMB}
	if l.Default.CPUMilli == 0 {
		l.Default.CPUMilli = 1000
	}
	l.Max = sandbox.Resources{CPUMilli: wholeCPUs(s.MaxCPUs), MemoryMB: s.MaxMemoryMB}
	if l.Max.MemoryMB == 0 {
		l.Max.MemoryMB = hostMemoryMB * 3 / 4 / sandbox.MinMemoryMB * sandbox.MinMemoryMB
	}
	if l.Max.CPUMilli == 0 && cores > 0 {
		l.Max.CPUMilli = cores * 1000
	}
	return clampLimits(l)
}

// kubernetesResourceLimits is the size offer on Kubernetes: quarter CPUs
// (the platform takes fractions), the configured default and ceiling, and
// a resize applied live to the pod. Here the runner is a pod itself, so
// the host says nothing about the ceiling: an unset one is the default,
// which offers no growth; the chart sets both (sandboxes.maxCPUs,
// sandboxes.maxMemoryMB) and sizes the namespace quota from them.
func kubernetesResourceLimits(s config.Sandboxes, defaultMemoryMB int) sandbox.ResourceLimits {
	quarterCPUs := func(cpus float64) int {
		milli := sandbox.CPUMilli(cpus)
		return (milli + 249) / 250 * 250
	}
	l := sandbox.ResourceLimits{CPUStepMilli: 250}
	l.Default = sandbox.Resources{CPUMilli: quarterCPUs(s.CPUs), MemoryMB: defaultMemoryMB}
	if l.Default.CPUMilli == 0 {
		l.Default.CPUMilli = 1000
	}
	l.Max = sandbox.Resources{CPUMilli: quarterCPUs(s.MaxCPUs), MemoryMB: s.MaxMemoryMB}
	return clampLimits(l)
}

// clampLimits keeps the ceiling at or above the default and within what a
// single sandbox may ever have.
func clampLimits(l sandbox.ResourceLimits) sandbox.ResourceLimits {
	if l.Max.MemoryMB < l.Default.MemoryMB {
		l.Max.MemoryMB = l.Default.MemoryMB
	}
	if l.Max.CPUMilli < l.Default.CPUMilli {
		l.Max.CPUMilli = l.Default.CPUMilli
	}
	if l.Max.MemoryMB > sandbox.MaxMemoryMB {
		l.Max.MemoryMB = sandbox.MaxMemoryMB
	}
	if l.Max.CPUMilli > sandbox.MaxCPUMilli {
		l.Max.CPUMilli = sandbox.MaxCPUMilli
	}
	return l
}
