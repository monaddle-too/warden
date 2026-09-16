package imageguard

import (
	"syscall"
	"unsafe"
)

func isolate() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: 3, Max: 3}); err != nil {
		return err
	}
	// This decoder needs only runtime/memory/stdio syscalls, never filesystem opens,
	// networking, process execution, or filesystem mutation. Apply to all Go threads.
	allowed := []uint32{0, 1, 3, 5, 8, 9, 10, 11, 12, 13, 14, 15, 24, 28, 35, 39, 56, 60, 72, 96, 97, 98, 99, 102, 104, 107, 108, 110, 131, 158, 186, 202, 204, 218, 219, 228, 230, 231, 234, 273, 302, 318, 324, 334, 435}
	filters := []syscall.SockFilter{{Code: 0x20, K: 4}, {Code: 0x15, Jt: 1, K: 0xc000003e}, {Code: 0x06, K: 0x80000000}, {Code: 0x20, K: 0}}
	for _, n := range allowed {
		filters = append(filters, syscall.SockFilter{Code: 0x15, Jf: 1, K: n}, syscall.SockFilter{Code: 0x06, K: 0x7fff0000})
	}
	filters = append(filters, syscall.SockFilter{Code: 0x06, K: 0x50000 | uint32(syscall.EPERM)})
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); e != 0 {
		return e
	}
	program := syscall.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if _, _, e := syscall.Syscall(317, 1, 1, uintptr(unsafe.Pointer(&program))); e != 0 {
		return e
	}
	return nil
}
