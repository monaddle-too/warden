//go:build linux

package hoststats

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cpuCounters(data string) (total, idle uint64, cpus int, err error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			if len(fields) < 5 {
				return 0, 0, 0, fmt.Errorf("incomplete CPU counters")
			}
			// guest/guest_nice are already included in user/nice.
			for i, field := range fields[1:min(len(fields), 9)] {
				value, e := strconv.ParseUint(field, 10, 64)
				if e != nil {
					return 0, 0, 0, e
				}
				total += value
				if i == 3 || i == 4 {
					idle += value
				}
			}
		} else if strings.HasPrefix(fields[0], "cpu") {
			cpus++
		}
	}
	if total == 0 || cpus == 0 {
		err = fmt.Errorf("CPU counters unavailable")
	}
	return
}

func (c *Collector) Snapshot() (Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample := Sample{At: time.Now().UTC()}
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return sample, err
	}
	total, idle, cpus, err := cpuCounters(string(data))
	if err != nil {
		return sample, err
	}
	sample.CPUs = cpus
	if c.previous.IsZero() || sample.At.Sub(c.previous) >= 250*time.Millisecond {
		if !c.previous.IsZero() && total > c.total && idle >= c.idle {
			percent := 100 * (1 - float64(idle-c.idle)/float64(total-c.total))
			percent = max(0, min(100, percent))
			c.percent = &percent
		}
		c.total, c.idle, c.previous = total, idle, sample.At
	}
	sample.CPUPercent = c.percent
	data, err = os.ReadFile("/proc/meminfo")
	if err != nil {
		return sample, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			sample.MemoryTotal = value * 1024
		case "MemAvailable:":
			sample.MemoryAvailable = value * 1024
		}
	}
	if sample.MemoryTotal == 0 {
		return sample, fmt.Errorf("memory counters unavailable")
	}
	data, err = os.ReadFile("/proc/loadavg")
	if err != nil {
		return sample, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return sample, fmt.Errorf("load counters unavailable")
	}
	for _, field := range fields[:3] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return sample, err
		}
		sample.Load = append(sample.Load, value)
	}
	data, err = os.ReadFile("/proc/uptime")
	if err != nil {
		return sample, err
	}
	fields = strings.Fields(string(data))
	if len(fields) == 0 {
		return sample, fmt.Errorf("uptime unavailable")
	}
	sample.UptimeSeconds, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
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
