package sandbox

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Resources is a workspace's size: CPU in millicores (1000 = one CPU) and
// memory in MiB. The zero value means "the configured default". Millicores
// are the unit because Kubernetes takes them; SBX takes whole CPUs, so the
// SBX limits declare a step of 1000 and validation rounds nothing.
type Resources struct {
	CPUMilli int `json:"cpuMilli,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty"`
}

// ResourceLimits is what a runner offers: the default a fresh workspace
// gets, the most any one workspace may have, and the CPU granularity the
// platform accepts (1000 on SBX, 250 on Kubernetes). Restart says that a
// resize replaces the running instance (SBX) instead of applying live.
type ResourceLimits struct {
	Default      Resources `json:"default"`
	Max          Resources `json:"max"`
	CPUStepMilli int       `json:"cpuStepMilli"`
	Restart      bool      `json:"restart"`
}

// ErrResizeInfeasible is a driver's answer when the platform cannot give
// the running instance the size in place (a memory decrease the kubelet
// refuses, a cluster without in-place resize, no room on the node). The
// size is still right for the next instance: the worker replaces the
// instance when nothing is running on it.
var ErrResizeInfeasible = errors.New("the running sandbox cannot be resized in place")

// Memory bounds a single sandbox may have, whatever the ceiling says.
const (
	MinMemoryMB = 512
	MaxMemoryMB = 65536
	MaxCPUMilli = 64000
)

func (r Resources) IsZero() bool { return r.CPUMilli == 0 && r.MemoryMB == 0 }

// String is the owner-facing form, "2 CPUs · 4 GiB".
func (r Resources) String() string {
	return CPUString(r.CPUMilli) + " · " + MemoryString(r.MemoryMB)
}

// CPUString renders millicores as CPUs: 1 CPU, 0.5 CPU, 2 CPUs.
func CPUString(milli int) string {
	cpus := float64(milli) / 1000
	s := strconv.FormatFloat(cpus, 'f', -1, 64)
	if cpus == 1 {
		return "1 CPU"
	}
	return s + " CPUs"
}

// MemoryString renders MiB as GiB when whole, otherwise MiB.
func MemoryString(mb int) string {
	if mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + " GiB"
	}
	return strconv.Itoa(mb) + " MiB"
}

// Fill returns r with zero fields taken from d.
func (r Resources) Fill(d Resources) Resources {
	if r.CPUMilli == 0 {
		r.CPUMilli = d.CPUMilli
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = d.MemoryMB
	}
	return r
}

// Resolve fills r from the default and checks it against the limits; the
// error names the ceiling so a client can say why a size was refused.
func (l ResourceLimits) Resolve(r Resources) (Resources, error) {
	r = r.Fill(l.Default)
	step := l.CPUStepMilli
	if step <= 0 {
		step = 1000
	}
	if r.CPUMilli < step || r.CPUMilli%step != 0 {
		return r, fmt.Errorf("cpus must be a multiple of %s", CPUString(step))
	}
	// The configured default is accepted as it is; a chosen size moves in
	// 512 MiB steps.
	if r.MemoryMB < MinMemoryMB || (r.MemoryMB%MinMemoryMB != 0 && r.MemoryMB != l.Default.MemoryMB) {
		return r, fmt.Errorf("memory must be a multiple of %d MiB", MinMemoryMB)
	}
	if r.CPUMilli > l.Max.CPUMilli || r.MemoryMB > l.Max.MemoryMB {
		return r, fmt.Errorf("at most %s on this Warden", l.Max)
	}
	return r, nil
}

// Validate checks the limits themselves.
func (l ResourceLimits) Validate() error {
	if l.CPUStepMilli <= 0 || 1000%l.CPUStepMilli != 0 {
		return errors.New("cpu step must divide one CPU")
	}
	for _, r := range []Resources{l.Default, l.Max} {
		if r.CPUMilli < l.CPUStepMilli || r.CPUMilli > MaxCPUMilli || r.CPUMilli%l.CPUStepMilli != 0 {
			return fmt.Errorf("cpus must be a multiple of %s up to %s", CPUString(l.CPUStepMilli), CPUString(MaxCPUMilli))
		}
		if r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB {
			return fmt.Errorf("memory must be %d–%d MiB", MinMemoryMB, MaxMemoryMB)
		}
	}
	if l.Default.CPUMilli > l.Max.CPUMilli || l.Default.MemoryMB > l.Max.MemoryMB {
		return errors.New("default size exceeds the maximum")
	}
	return nil
}

// CPUMilli converts a CPU count (possibly fractional) to millicores,
// rounding to the nearest millicore; a negative or absurd value is 0.
func CPUMilli(cpus float64) int {
	if cpus <= 0 || math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus > MaxCPUMilli/1000 {
		return 0
	}
	return int(math.Round(cpus * 1000))
}

// ParseMemoryMB reads "4g", "4096m", "4096" (MiB) into MiB.
func ParseMemoryMB(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("memory is required")
	}
	unit := 1
	switch {
	case strings.HasSuffix(s, "g"), strings.HasSuffix(s, "gib"):
		unit = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "gib"), "g")
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "mib"):
		s = strings.TrimSuffix(strings.TrimSuffix(s, "mib"), "m")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, errors.New("memory must be a size like 4g or 2048m")
	}
	return n * unit, nil
}

// CPUsFromSpec turns millicores into the whole-CPU count SBX takes,
// rounding up: SBX has no fractional CPUs, so a request that got through
// validation is already whole and this only guards the default.
func CPUsFromSpec(milli int) int {
	if milli <= 0 {
		return 1
	}
	return (milli + 999) / 1000
}
