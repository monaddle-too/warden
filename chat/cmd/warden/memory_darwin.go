//go:build darwin

package main

import (
	"encoding/binary"
	"syscall"
)

// hostMemoryMB reads hw.memsize. syscall.Sysctl returns the raw
// little-endian value as a string with one trailing NUL stripped, so it is
// padded back to eight bytes.
func hostMemoryMB() int {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(raw) > 8 {
		return 0
	}
	b := make([]byte, 8)
	copy(b, raw)
	return int(binary.LittleEndian.Uint64(b) / (1024 * 1024))
}
