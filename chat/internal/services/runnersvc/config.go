package runnersvc

import (
	"flag"
	"os"
	"os/exec"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/hostinfo"
	"warden/chat/internal/sandbox"
)

// settings are the effective runner values after reconciling the legacy
// flags with the optional warden.json. Each flag maps to one field; a flag
// that disagrees with a loaded file is an error.
type settings struct {
	cfg          config.Config
	root         string        // paths.state/runner
	socket       string        // paths.state/runner/worker.sock
	wardenSocket string        // paths.state/policy/sbx-control.sock
	sbx          string        // sbx.executable
	template     string        // sbx.guestImage@sbx.guestImageDigest
	runtimeDir   string        // runtimes.codex
	claudePath   string        // runtimes.claude
	idle         time.Duration // sandboxes.stopAfterIdleMinutes
	memoryMB     int           // sandboxes.memoryMB
	residents    int           // sandboxes.maxRunning
	spares       int           // sandboxes.warmSpares
	retained     int           // sandboxes.keepStopped
}

type runnerFlags struct {
	configPath, root, socket, wardenSocket, sbx, template, runtimeDir, claudePath *string
	idle                                                                          *time.Duration
	memoryMB, residents, spares, retained                                         *int
}

func resolveSettings(fs *flag.FlagSet, f runnerFlags) (settings, error) {
	cfg, source, err := config.Resolve(*f.configPath, *f.root)
	if err != nil {
		return settings{}, err
	}
	o := config.NewOverrides(fs, source)
	s := settings{cfg: cfg}
	s.root = config.Override(o, "root", *f.root, "paths.state (runner directory)", cfg.RunnerState())
	s.socket = config.Override(o, "socket", *f.socket, "paths.state (runner socket)", cfg.RunnerSocket())
	s.wardenSocket = config.Override(o, "warden-socket", *f.wardenSocket, "paths.state (policy socket)", cfg.PolicySocket())
	s.sbx = config.Override(o, "sbx", *f.sbx, "sbx.executable", cfg.SBX.Executable)
	s.template = config.Override(o, "template", *f.template, "sbx.guestImage", cfg.GuestTemplate())
	s.runtimeDir = config.Override(o, "runtime-dir", *f.runtimeDir, "runtimes.codex", cfg.Runtimes.Codex)
	s.claudePath = config.Override(o, "claude-path", *f.claudePath, "runtimes.claude", cfg.Runtimes.Claude)
	s.idle = config.Override(o, "idle-timeout", *f.idle, "sandboxes.stopAfterIdleMinutes", time.Duration(cfg.Sandboxes.StopAfterIdleMinutes)*time.Minute)
	s.memoryMB = config.Override(o, "sandbox-memory-mb", *f.memoryMB, "sandboxes.memoryMB", cfg.Sandboxes.MemoryMB)
	s.residents = config.Override(o, "max-resident", *f.residents, "sandboxes.maxRunning", cfg.Sandboxes.MaxRunning)
	s.spares = config.Override(o, "spare-sandboxes", *f.spares, "sandboxes.warmSpares", cfg.Sandboxes.WarmSpares)
	s.retained = config.Override(o, "retained", *f.retained, "sandboxes.keepStopped", cfg.Sandboxes.KeepStopped)
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

var findSBX = hostinfo.FindSBX

// resourceLimits is the size offer for SBX: the configured default, a
// ceiling from the configuration or else from the host (75 % of its memory
// and all its cores, what SBX itself allows a sandbox), whole CPUs only
// (SBX takes no fraction, so a fractional setting rounds up), and a resize
// that restarts the sandbox.
func resourceLimits(s config.Sandboxes, defaultMemoryMB, hostMemoryMB, cores int) sandbox.ResourceLimits {
	wholeCPUs := func(cpus float64) int {
		milli := sandbox.CPUMilli(cpus)
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
