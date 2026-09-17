package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A real guest's answer to usageScript (1 CPU, 2 GiB, 20 GiB overlay).
const usageReport = `1
cpu  210 0 153 399101 6 0 0 0 0 0
MemTotal:        2066112 kB
MemAvailable:    1956576 kB
overlay           20466256   560  19400736   1% /home/agent/workspace
`

func TestParseUsageReadsProvisionedAndUsed(t *testing.T) {
	sample, total, idle, err := parseUsage(usageReport)
	if err != nil {
		t.Fatal(err)
	}
	if !sample.Running || sample.CPUs != 1 || sample.CPUPercent != nil {
		t.Fatalf("unexpected sample %+v", sample)
	}
	if sample.MemoryTotal != 2066112<<10 || sample.MemoryUsed != (2066112-1956576)<<10 {
		t.Fatalf("memory %d used %d", sample.MemoryTotal, sample.MemoryUsed)
	}
	if sample.DiskTotal != 20466256<<10 || sample.DiskUsed != 560<<10 {
		t.Fatalf("disk %d used %d", sample.DiskTotal, sample.DiskUsed)
	}
	if total != 210+153+399101+6 || idle != 399101+6 {
		t.Fatalf("counters %d %d", total, idle)
	}
	for _, bad := range []string{"", "1\ncpu x\n", strings.Repeat("1\n", 3000), strings.Replace(usageReport, "MemAvailable:    1956576", "MemAvailable:    9956576", 1), strings.Replace(usageReport, "overlay           20466256   560", "overlay           20466256   99999999", 1)} {
		if _, _, _, err := parseUsage(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestUsageReportsSizingWhenStoppedAndSamplesTheGuestWhenRunning(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.MemoryMB = 2048
	clock := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	w.Now = func() time.Time { return clock }
	r.Operation = "usage"
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil || res.Usage.Running || res.Usage.CPUs != 1 || res.Usage.MemoryTotal != 2048<<20 || res.Usage.DiskTotal != 0 {
		t.Fatalf("stopped sandbox reported %+v", res.Usage)
	}
	execs := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		n := 0
		for _, c := range d.calls {
			if strings.HasPrefix(c, "exec:") && strings.Contains(c, "/proc/stat") {
				n++
			}
		}
		return n
	}
	if execs() != 0 {
		t.Fatal("a stopped sandbox must not be exec'd")
	}
	prepareFixture(t, w, r)
	clock = clock.Add(10 * time.Second)
	d.execOutput = usageReport
	res, err = w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Usage.Running || res.Usage.CPUPercent != nil || res.Usage.MemoryUsed == 0 || res.Usage.DiskTotal == 0 {
		t.Fatalf("first running sample %+v", res.Usage)
	}
	// Inside the cache window the guest is not asked again.
	clock = clock.Add(time.Second)
	if _, err = w.dispatch(context.Background(), r); err != nil || execs() != 1 {
		t.Fatalf("cached sample: err=%v execs=%d", err, execs())
	}
	// Half of the elapsed jiffies busy since the previous sample.
	d.execOutput = strings.Replace(usageReport, "cpu  210 0 153 399101 6", "cpu  260 0 203 399201 6", 1)
	clock = clock.Add(10 * time.Second)
	res, err = w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.CPUPercent == nil || *res.Usage.CPUPercent < 49.9 || *res.Usage.CPUPercent > 50.1 {
		t.Fatalf("cpu percent %v", res.Usage.CPUPercent)
	}
	// A guest that answers nonsense yields an error, never a partial sample.
	d.execOutput = "nope"
	clock = clock.Add(10 * time.Second)
	if _, err = w.dispatch(context.Background(), r); err == nil {
		t.Fatal("malformed report accepted")
	}
}
