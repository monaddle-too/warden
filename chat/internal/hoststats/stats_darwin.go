//go:build darwin

package hoststats

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Snapshot reads the Mac: cores from the runtime, load from the kernel's
// vm.loadavg, memory from hw.memsize and vm_stat, disk from statfs.
// Instantaneous CPU use has no counter reachable without cgo, so
// CPUPercent stays nil; the load average stands in for it.
func (c *Collector) Snapshot() (Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample := Sample{At: time.Now().UTC(), CPUs: runtime.NumCPU()}
	out, err := exec.Command("sysctl", "-n", "hw.memsize", "vm.loadavg", "kern.boottime").Output()
	if err != nil {
		return sample, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return sample, fmt.Errorf("sysctl counters unavailable")
	}
	if sample.MemoryTotal, err = strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64); err != nil {
		return sample, err
	}
	if sample.Load, err = parseLoadAverage(lines[1]); err != nil {
		return sample, err
	}
	if boot, err := parseBootTime(lines[2]); err == nil {
		sample.UptimeSeconds = sample.At.Sub(boot).Seconds()
	}
	out, err = exec.Command("vm_stat").Output()
	if err != nil {
		return sample, err
	}
	if sample.MemoryAvailable, err = parseVMStat(string(out)); err != nil {
		return sample, err
	}
	var disk syscall.Statfs_t
	if err = syscall.Statfs(c.Root, &disk); err != nil {
		return sample, err
	}
	sample.DiskTotal = disk.Blocks * uint64(disk.Bsize)
	sample.DiskAvailable = disk.Bavail * uint64(disk.Bsize)
	return sample, nil
}

// parseLoadAverage reads sysctl's "{ 1.52 1.80 1.91 }".
func parseLoadAverage(s string) ([]float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(fields) < 3 {
		return nil, fmt.Errorf("load counters unavailable")
	}
	var load []float64
	for _, field := range fields[:3] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, err
		}
		load = append(load, value)
	}
	return load, nil
}

// parseBootTime reads sysctl's "{ sec = 1789158352, usec = 676167 } Fri …".
func parseBootTime(s string) (time.Time, error) {
	_, rest, ok := strings.Cut(s, "sec = ")
	if !ok {
		return time.Time{}, fmt.Errorf("boot time unavailable")
	}
	digits, _, _ := strings.Cut(rest, ",")
	sec, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

// parseVMStat sums the pages the kernel can hand out without swapping —
// free, inactive, speculative and purgeable — in bytes, the counterpart of
// Linux's MemAvailable. Wired, active and compressed pages are in use.
func parseVMStat(out string) (uint64, error) {
	var pageSize uint64 = 4096
	if _, rest, ok := strings.Cut(out, "page size of "); ok {
		if n, err := strconv.ParseUint(strings.Fields(rest)[0], 10, 64); err == nil {
			pageSize = n
		}
	}
	wanted := map[string]bool{"Pages free": true, "Pages inactive": true, "Pages speculative": true, "Pages purgeable": true}
	var pages uint64
	found := 0
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !wanted[strings.TrimSpace(name)] {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			return 0, err
		}
		pages += n
		found++
	}
	if found == 0 {
		return 0, fmt.Errorf("memory counters unavailable")
	}
	return pages * pageSize, nil
}
