//go:build darwin

package hoststats

import "testing"

func TestParseMacCounters(t *testing.T) {
	load, err := parseLoadAverage("{ 14.84 9.84 7.22 }")
	if err != nil || len(load) != 3 || load[0] != 14.84 || load[2] != 7.22 {
		t.Fatal(load, err)
	}
	if _, err := parseLoadAverage("{ }"); err == nil {
		t.Fatal("accepted an empty load average")
	}
	boot, err := parseBootTime("{ sec = 1789158352, usec = 676167 } Fri Sep 11 13:25:52 2026")
	if err != nil || boot.Unix() != 1789158352 {
		t.Fatal(boot, err)
	}
	available, err := parseVMStat("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                               93604.\nPages active:                            562595.\nPages inactive:                          555267.\nPages speculative:                         6109.\nPages wired down:                        256008.\nPages purgeable:                            217.\n\"Translation faults\":                5029377058.\n")
	if err != nil || available != (93604+555267+6109+217)*16384 {
		t.Fatal(available, err)
	}
	if _, err := parseVMStat("nothing"); err == nil {
		t.Fatal("accepted missing counters")
	}
}

func TestLiveMacSample(t *testing.T) {
	collector := &Collector{Root: t.TempDir()}
	sample, err := collector.Snapshot()
	if err != nil || sample.CPUs < 1 || sample.MemoryTotal == 0 || sample.MemoryAvailable == 0 || sample.MemoryAvailable > sample.MemoryTotal || len(sample.Load) != 3 || sample.DiskTotal == 0 || sample.UptimeSeconds <= 0 {
		t.Fatalf("%+v %v", sample, err)
	}
	if sample.CPUPercent != nil {
		t.Fatal("no instantaneous CPU counter is read on macOS")
	}
}
