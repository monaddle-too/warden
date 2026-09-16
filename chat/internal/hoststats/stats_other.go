//go:build !linux

package hoststats

import "fmt"

func (c *Collector) Snapshot() (Sample, error) {
	return Sample{}, fmt.Errorf("server metrics require a Linux execution host")
}
