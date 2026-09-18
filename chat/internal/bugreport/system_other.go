//go:build !darwin && !linux

package bugreport

// osVersion is unknown on other hosts.
func osVersion() string { return "" }
