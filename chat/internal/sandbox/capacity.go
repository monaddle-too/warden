package sandbox

import (
	"context"
	"time"
)

// Capacity is what the execution host has and what the runner's sandboxes
// already hold of it, for the size picker: on SBX the machine the runner
// runs on, on Kubernetes the cluster's ready nodes summed. Availability
// figures are nil when the platform cannot report them (no metrics server;
// no instantaneous CPU counter on macOS, where the load average stands in).
type Capacity struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // "host" or "cluster"
	// CPUMilli and MemoryMB are the whole: the host's cores and memory, or
	// the nodes' allocatable amounts.
	CPUMilli int `json:"cpuMilli"`
	MemoryMB int `json:"memoryMB"`
	// CPUPercent is the whole's live CPU use; Load the host's load
	// averages (1, 5, 15 minutes); MemoryAvailableMB what can be handed
	// out now without swapping (Linux MemAvailable, the Mac's free +
	// inactive + speculative + purgeable pages, a node's allocatable minus
	// its use).
	CPUPercent        *float64  `json:"cpuPercent,omitempty"`
	Load              []float64 `json:"load,omitempty"`
	MemoryAvailableMB *int      `json:"memoryAvailableMB,omitempty"`
	DiskMB            int       `json:"diskMB,omitempty"`
	DiskAvailableMB   *int      `json:"diskAvailableMB,omitempty"`
	// Reserved is the sum of the running sandboxes' sizes, spares at the
	// default size included; Running and Spares count them.
	Reserved Resources      `json:"reserved"`
	Running  int            `json:"running"`
	Spares   int            `json:"spares"`
	Limits   ResourceLimits `json:"limits"`
	// Error says why the whole is unknown (its fields zero) while the
	// reservations are still reported.
	Error string `json:"error,omitempty"`
}

// capacity answers the capacity op: the reservations from the managed
// state, the whole from the host or the cluster.
func (w *Worker) capacity(ctx context.Context) Response {
	w.mu.Lock()
	w.defaultsLocked()
	c := Capacity{At: w.now(), Kind: "host", Limits: w.Limits}
	if w.managed != nil {
		for _, s := range w.managed.Sandboxes {
			if s.State != "running" && s.State != "starting" && s.State != "stopping" {
				continue
			}
			size := s.Resources.Fill(w.Limits.Default)
			c.Reserved.CPUMilli += size.CPUMilli
			c.Reserved.MemoryMB += size.MemoryMB
			c.Running++
		}
		for range w.managed.Spares {
			c.Reserved.CPUMilli += w.Limits.Default.CPUMilli
			c.Reserved.MemoryMB += w.Limits.Default.MemoryMB
			c.Spares++
		}
	}
	w.mu.Unlock()
	if w.Cluster != nil {
		c.Kind = "cluster"
		status, err := w.Cluster.Cluster(ctx)
		if err != nil {
			c.Error = err.Error()
		} else {
			c.fillFromCluster(status)
		}
		return Response{Capacity: &c}
	}
	sample, err := w.metrics.Snapshot()
	if err != nil {
		c.Error = err.Error()
		return Response{Capacity: &c}
	}
	c.CPUMilli = sample.CPUs * 1000
	c.CPUPercent = sample.CPUPercent
	c.Load = sample.Load
	c.MemoryMB = int(sample.MemoryTotal >> 20)
	available := int(sample.MemoryAvailable >> 20)
	c.MemoryAvailableMB = &available
	c.DiskMB = int(sample.DiskTotal >> 20)
	disk := int(sample.DiskAvailable >> 20)
	c.DiskAvailableMB = &disk
	return Response{Capacity: &c}
}

// fillFromCluster sums the ready, schedulable nodes: allocatable as the
// whole, allocatable minus live use as available when the metrics server
// answered for every one of them.
func (c *Capacity) fillFromCluster(status *ClusterStatus) {
	if status == nil || !status.Available {
		c.Error = "the cluster is unreachable"
		return
	}
	if len(status.Nodes) == 0 {
		c.Error = status.NodesError
		if c.Error == "" {
			c.Error = "no nodes listed"
		}
		return
	}
	var usedCPU, usedMemory int64
	metered := true
	for _, n := range status.Nodes {
		if !n.Ready || n.Unschedulable {
			continue
		}
		c.CPUMilli += int(n.Allocatable.CPUMilli)
		c.MemoryMB += int(n.Allocatable.MemoryBytes >> 20)
		if n.Usage == nil {
			metered = false
			continue
		}
		usedCPU += n.Usage.CPUMilli
		usedMemory += n.Usage.MemoryBytes
	}
	if !metered || c.CPUMilli == 0 {
		return
	}
	percent := 100 * float64(usedCPU) / float64(c.CPUMilli)
	percent = max(0, min(100, percent))
	c.CPUPercent = &percent
	available := max(0, c.MemoryMB-int(usedMemory>>20))
	c.MemoryAvailableMB = &available
}
