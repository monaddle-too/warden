// Package hoststats reads resource counters on the execution host, outside VMs.
package hoststats

import (
	"sync"
	"time"
)

type Sample struct {
	At              time.Time `json:"at"`
	CPUPercent      *float64  `json:"cpuPercent"`
	CPUs            int       `json:"cpus"`
	Load            []float64 `json:"load"`
	UptimeSeconds   float64   `json:"uptimeSeconds"`
	MemoryTotal     uint64    `json:"memoryTotal"`
	MemoryAvailable uint64    `json:"memoryAvailable"`
	DiskTotal       uint64    `json:"diskTotal"`
	DiskAvailable   uint64    `json:"diskAvailable"`
}

type Collector struct {
	Root        string
	mu          sync.Mutex
	total, idle uint64
	previous    time.Time
	percent     *float64
}
