//go:build linux

package hoststats

import (
	"testing"
	"time"
)

func TestCPUAccountingDoesNotDoubleCountGuestTime(t *testing.T) {
	total, idle, cpus, err := cpuCounters("cpu 100 20 30 400 50 6 7 8 40 10\ncpu0 1 2 3 4\ncpu1 1 2 3 4\n")
	if err != nil || total != 621 || idle != 450 || cpus != 2 {
		t.Fatal(total, idle, cpus, err)
	}
	if _, _, _, err := cpuCounters("cpu 1 2"); err == nil {
		t.Fatal("accepted incomplete counters")
	}
}
func TestLiveHostSample(t *testing.T) {
	collector := &Collector{Root: t.TempDir()}
	first, err := collector.Snapshot()
	if err != nil || first.CPUs < 1 || first.MemoryTotal == 0 || first.MemoryAvailable > first.MemoryTotal || first.DiskTotal == 0 {
		t.Fatal(first, err)
	}
	time.Sleep(260 * time.Millisecond)
	second, err := collector.Snapshot()
	if err != nil || second.CPUPercent == nil || *second.CPUPercent < 0 || *second.CPUPercent > 100 {
		t.Fatal(second, err)
	}
}
