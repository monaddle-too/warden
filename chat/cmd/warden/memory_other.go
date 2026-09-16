//go:build !darwin && !linux

package main

// hostMemoryMB is unknown on other hosts; sizing keeps the defaults.
func hostMemoryMB() int { return 0 }
