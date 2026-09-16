//go:build !linux || !amd64

package imageguard

// Development platforms still use a fresh process with byte/pixel/time bounds.
// Production is Linux amd64, where the syscall allowlist is mandatory.
func isolate() error { return nil }
