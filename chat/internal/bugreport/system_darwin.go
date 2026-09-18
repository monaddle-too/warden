//go:build darwin

package bugreport

import "syscall"

// osVersion is the kernel release (kern.osrelease, e.g. 24.6.0).
func osVersion() string {
	v, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return ""
	}
	return v
}
