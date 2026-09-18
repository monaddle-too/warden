package sandbox

import (
	"context"
	"testing"
)

// The capacity op sums what the running sandboxes and the spares hold and
// reads the host (or, on Kubernetes, the cluster's ready nodes).
func TestCapacityReportsReservationsAndTheHost(t *testing.T) {
	w, _, _, r := managedFixture(t)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 2048}, Max: Resources{CPUMilli: 8000, MemoryMB: 16384}, CPUStepMilli: 1000, Restart: true}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].State = "running"
	w.managed.Sandboxes[r.SandboxID].Resources = Resources{CPUMilli: 2000, MemoryMB: 4096}
	w.managed.Sandboxes["old"] = &managedSandbox{SandboxInfo: SandboxInfo{ID: "old", State: "running"}} // registered before sizes: the default
	w.managed.Sandboxes["off"] = &managedSandbox{SandboxInfo: SandboxInfo{ID: "off", State: "stopped", Resources: Resources{CPUMilli: 4000, MemoryMB: 8192}}}
	w.managed.Spares["spare-1"] = &spareSandbox{Name: "spare-1"}
	w.mu.Unlock()
	res := w.capacity(context.Background())
	c := res.Capacity
	if c == nil || c.Kind != "host" || c.Running != 2 || c.Spares != 1 || c.Reserved != (Resources{CPUMilli: 4000, MemoryMB: 8192}) || c.Limits.Max.CPUMilli != 8000 {
		t.Fatalf("%+v", c)
	}
	if c.Error != "" && (c.CPUMilli != 0 || c.MemoryMB != 0) {
		t.Fatalf("an unreadable host reports nothing of it: %+v", c)
	}
	if c.Error == "" && (c.CPUMilli < 1000 || c.MemoryMB == 0 || c.MemoryAvailableMB == nil || *c.MemoryAvailableMB > c.MemoryMB || len(c.Load) != 3) {
		t.Fatalf("host sample %+v", c)
	}
}

func TestCapacitySumsTheClusterNodes(t *testing.T) {
	gib := int64(1 << 30)
	usage := func(cpu, mem int64) *Amounts { return &Amounts{CPUMilli: cpu, MemoryBytes: mem} }
	c := Capacity{}
	c.fillFromCluster(&ClusterStatus{Available: true, Nodes: []NodeInfo{
		{Name: "a", Ready: true, Allocatable: Amounts{CPUMilli: 4000, MemoryBytes: 16 * gib}, Usage: usage(1000, 4*gib)},
		{Name: "b", Ready: true, Allocatable: Amounts{CPUMilli: 4000, MemoryBytes: 16 * gib}, Usage: usage(3000, 12*gib)},
		{Name: "cordoned", Ready: true, Unschedulable: true, Allocatable: Amounts{CPUMilli: 64000, MemoryBytes: 256 * gib}},
		{Name: "down", Ready: false, Allocatable: Amounts{CPUMilli: 64000, MemoryBytes: 256 * gib}},
	}})
	if c.Error != "" || c.CPUMilli != 8000 || c.MemoryMB != 32*1024 || c.CPUPercent == nil || *c.CPUPercent != 50 || c.MemoryAvailableMB == nil || *c.MemoryAvailableMB != 16*1024 {
		t.Fatalf("%+v", c)
	}
	// Without a metrics server the whole is known, the use is not.
	unmetered := Capacity{}
	unmetered.fillFromCluster(&ClusterStatus{Available: true, Nodes: []NodeInfo{{Name: "a", Ready: true, Allocatable: Amounts{CPUMilli: 4000, MemoryBytes: 16 * gib}}}})
	if unmetered.Error != "" || unmetered.CPUMilli != 4000 || unmetered.CPUPercent != nil || unmetered.MemoryAvailableMB != nil {
		t.Fatalf("%+v", unmetered)
	}
	none := Capacity{}
	none.fillFromCluster(&ClusterStatus{Available: true, NodesError: "nodes are forbidden"})
	if none.Error != "nodes are forbidden" || none.CPUMilli != 0 {
		t.Fatalf("%+v", none)
	}
}
