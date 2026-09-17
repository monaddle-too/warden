package sandbox

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SandboxUsage is a sandbox's resources as the guest itself reports them:
// what the VM was given (its CPUs, MemTotal and the workspace filesystem) and
// what it is using. Provisioned figures are read inside the guest rather than
// echoed from the create flags because the guest is what the agent gets; a
// stopped sandbox falls back to the runner's sizing and reports no usage.
type SandboxUsage struct {
	At          time.Time `json:"at"`
	Running     bool      `json:"running"`
	CPUs        int       `json:"cpus"`
	MemoryTotal uint64    `json:"memoryTotal"`
	DiskTotal   uint64    `json:"diskTotal"`
	// Usage; meaningful only while Running. CPUPercent needs two samples of
	// the guest's counters and is nil until the second one.
	CPUPercent *float64 `json:"cpuPercent"`
	MemoryUsed uint64   `json:"memoryUsed"`
	DiskUsed   uint64   `json:"diskUsed"`
}

// sandboxCPUs is the CPU count every sandbox is created with (runtime.go).
const sandboxCPUs = 1

// usageScript prints, one per line: the CPU count, the aggregate line of
// /proc/stat, MemTotal and MemAvailable, and the df line of the workspace.
// Only /proc and df are read; nothing here depends on guest-installed tools.
const usageScript = `grep -c '^cpu[0-9]' /proc/stat; head -n1 /proc/stat; grep -E '^Mem(Total|Available):' /proc/meminfo; df -kP -- "$1" | tail -n1`

// usageState is the previous counter pair per sandbox for CPU percentages,
// plus a short-lived cached sample so several callers share one guest exec.
type usageState struct {
	generation  string
	total, idle uint64
	sample      SandboxUsage
}

const usageCacheTTL = 3 * time.Second

func (w *Worker) usage(ctx context.Context, r Request) (Response, error) {
	w.mu.Lock()
	w.defaultsLocked()
	s, _, err := w.bindingLocked(r)
	if err != nil {
		w.mu.Unlock()
		return Response{}, err
	}
	if w.usages == nil {
		w.usages = map[string]*usageState{}
	}
	state := w.usages[s.ID]
	if state == nil || state.generation != s.Generation {
		state = &usageState{generation: s.Generation}
		w.usages[s.ID] = state
	}
	now := w.now()
	if !state.sample.At.IsZero() && now.Sub(state.sample.At) < usageCacheTTL {
		sample := state.sample
		w.mu.Unlock()
		return Response{Usage: &sample}, nil
	}
	if s.State != "running" || !s.Created {
		memoryMB := w.MemoryMB
		if memoryMB == 0 {
			memoryMB = defaultMemoryMB
		}
		sample := SandboxUsage{At: now, CPUs: sandboxCPUs, MemoryTotal: uint64(memoryMB) << 20}
		state.total, state.idle = 0, 0
		state.sample = sample
		w.mu.Unlock()
		return Response{Usage: &sample}, nil
	}
	name, dir := s.RuntimeName, s.Directory
	previousTotal, previousIdle, previousAt := state.total, state.idle, state.sample.At
	// The guest exec runs without the worker lock; a busy sandbox must not
	// stall every other operation while it answers.
	w.mu.Unlock()
	raw, err := w.Runtime.Exec(ctx, name, dir, "sh", "-c", usageScript, "sh", dir)
	if err != nil {
		return Response{}, errors.New("sandbox usage unavailable")
	}
	sample, total, idle, err := parseUsage(raw)
	if err != nil {
		return Response{}, err
	}
	sample.At = w.now()
	if !previousAt.IsZero() && previousTotal > 0 && total > previousTotal && idle >= previousIdle {
		percent := 100 * (1 - float64(idle-previousIdle)/float64(total-previousTotal))
		sample.CPUPercent = &percent
	}
	w.mu.Lock()
	// The state may have been replaced by a newer generation meanwhile; only
	// record counters for the generation they came from.
	if current := w.usages[s.ID]; current != nil && current.generation == state.generation {
		current.total, current.idle, current.sample = total, idle, sample
	}
	w.mu.Unlock()
	return Response{Usage: &sample}, nil
}

// parseUsage reads the guest's answer strictly: the output is agent-reachable,
// so anything malformed is an error rather than a partial sample.
func parseUsage(raw string) (sample SandboxUsage, total, idle uint64, err error) {
	if len(raw) > 4096 {
		return sample, 0, 0, errors.New("sandbox usage report too large")
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 5 {
		return sample, 0, 0, errors.New("invalid sandbox usage report")
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || cpus < 1 || cpus > 1024 {
		return sample, 0, 0, errors.New("invalid sandbox CPU count")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 || fields[0] != "cpu" {
		return sample, 0, 0, errors.New("invalid sandbox CPU counters")
	}
	// user nice system idle iowait irq softirq steal; guest columns are
	// already included in user/nice.
	for i, field := range fields[1:min(len(fields), 9)] {
		value, e := strconv.ParseUint(field, 10, 64)
		if e != nil {
			return sample, 0, 0, errors.New("invalid sandbox CPU counters")
		}
		total += value
		if i == 3 || i == 4 {
			idle += value
		}
	}
	memory := map[string]uint64{}
	for _, line := range lines[2:4] {
		key, rest, ok := strings.Cut(line, ":")
		f := strings.Fields(rest)
		if !ok || len(f) != 2 || f[1] != "kB" {
			return sample, 0, 0, errors.New("invalid sandbox memory report")
		}
		value, e := strconv.ParseUint(f[0], 10, 64)
		if e != nil {
			return sample, 0, 0, errors.New("invalid sandbox memory report")
		}
		memory[key] = value << 10
	}
	memoryTotal, memoryAvailable := memory["MemTotal"], memory["MemAvailable"]
	if memoryTotal == 0 || memoryAvailable > memoryTotal {
		return sample, 0, 0, errors.New("invalid sandbox memory report")
	}
	disk := strings.Fields(lines[4])
	if len(disk) < 6 {
		return sample, 0, 0, errors.New("invalid sandbox disk report")
	}
	diskTotal, e1 := strconv.ParseUint(disk[1], 10, 64)
	diskUsed, e2 := strconv.ParseUint(disk[2], 10, 64)
	if e1 != nil || e2 != nil || diskUsed > diskTotal {
		return sample, 0, 0, errors.New("invalid sandbox disk report")
	}
	sample = SandboxUsage{Running: true, CPUs: cpus, MemoryTotal: memoryTotal, MemoryUsed: memoryTotal - memoryAvailable, DiskTotal: diskTotal << 10, DiskUsed: diskUsed << 10}
	return sample, total, idle, nil
}
