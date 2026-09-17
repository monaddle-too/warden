// Package hostinfo detects the host facts the local installer records in
// warden.json: the SBX executable and its version, the guest architecture,
// sandbox sizing from memory and cores, and free loopback ports. The
// decisions are pure functions over injected inputs so they are testable;
// the thin wrappers at the bottom read the real host.
package hostinfo

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"warden/chat/internal/release"
)

// SBXCandidates are the fixed locations tried after PATH, in order.
var SBXCandidates = []string{"/opt/homebrew/bin/sbx", "/usr/bin/sbx"}

// FindSBX returns the first sbx executable found: PATH first (lookPath),
// then the fixed candidates (exists reports a regular executable file).
func FindSBX(lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if lookPath != nil {
		if path, err := lookPath("sbx"); err == nil && path != "" {
			return path, nil
		}
	}
	for _, candidate := range SBXCandidates {
		if exists != nil && exists(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("sbx executable not found on PATH, /opt/homebrew/bin or /usr/bin")
}

// ParseSBXVersion extracts the version from `sbx version` output, which the
// verifier expects as "sbx version: v0.42.1 <build>". It returns the version
// without the leading "v".
func ParseSBXVersion(output string) (string, error) {
	line := strings.TrimSpace(output)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	const prefix = "sbx version: "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("unrecognised sbx version output %q", line)
	}
	fields := strings.Fields(strings.TrimPrefix(line, prefix))
	if len(fields) == 0 {
		return "", fmt.Errorf("unrecognised sbx version output %q", line)
	}
	version := strings.TrimPrefix(fields[0], "v")
	if version == "" {
		return "", fmt.Errorf("unrecognised sbx version output %q", line)
	}
	return version, nil
}

// CheckSBXVersion reports nil when output is what `sbx version` prints. No
// version is required: Warden assumes the features it uses are present and
// fails at the call that needs one that is not.
func CheckSBXVersion(output string) error {
	_, err := ParseSBXVersion(output)
	return err
}

// GuestArch is the guest architecture for this host: guests run the host's
// architecture (arm64 on Apple Silicon, amd64 on x86_64).
func GuestArch() string { return GuestArchFor(runtime.GOARCH) }

// GuestArchFor maps a Go host architecture onto a supported guest one.
func GuestArchFor(goarch string) string {
	switch goarch {
	case release.AMD64, release.ARM64:
		return goarch
	}
	return ""
}

// Sizing is the sandbox capacity derived from the host.
type Sizing struct {
	MemoryMB   int
	MaxRunning int
	WarmSpares int
}

const (
	sandboxMemoryMB   = 1536
	bytesPerSandbox   = 4 << 30 // one running sandbox per 4 GiB of host memory
	maxRunningCeiling = 8
)

// Size derives capacity from host memory and cores: one slot per 4 GiB
// (each guest gets 1536 MiB, the rest is host headroom), at least one running
// sandbox, never more than cores minus one, and one warm spare when a slot is
// left over.
func Size(memoryBytes uint64, cores int) Sizing {
	slots := int(memoryBytes / bytesPerSandbox)
	if slots < 1 {
		slots = 1
	}
	byCores := cores - 1
	if byCores < 1 {
		byCores = 1
	}
	s := Sizing{MemoryMB: sandboxMemoryMB, MaxRunning: 1}
	if slots >= 2 {
		s.MaxRunning = slots - 1
		s.WarmSpares = 1
	}
	if s.MaxRunning > byCores {
		s.MaxRunning = byCores
	}
	if s.MaxRunning > maxRunningCeiling {
		s.MaxRunning = maxRunningCeiling
	}
	if s.WarmSpares > 0 && s.MaxRunning+s.WarmSpares > slots {
		s.WarmSpares = 0
	}
	return s
}

// FreePorts asks the kernel for n distinct free IPv4 loopback ports. They are
// released before returning; the caller binds them promptly.
func FreePorts(n int) ([]int, error) {
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for len(ports) < n {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// FreePort returns one free loopback port.
func FreePort() (int, error) {
	ports, err := FreePorts(1)
	if err != nil {
		return 0, err
	}
	return ports[0], nil
}

// Loopback formats a loopback listen address for a port.
func Loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Host reads the real host. Each method wraps the pure function above.
type Host struct{}

// SBX finds the executable and checks its version by running it.
func (Host) SBX() (string, error) {
	path, err := FindSBX(exec.LookPath, func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
	})
	if err != nil {
		return "", err
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s version: %w", path, err)
	}
	if err = CheckSBXVersion(string(out)); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return path, nil
}

// Capacity is this host's memory in MiB and its cores; memory is 0 when
// the platform cannot report it.
func Capacity() (memoryMB, cores int) {
	memory, err := totalMemory()
	if err != nil {
		return 0, runtime.NumCPU()
	}
	return int(memory >> 20), runtime.NumCPU()
}

// Sizing derives capacity from this host's memory and cores.
func (Host) Sizing() (Sizing, error) {
	memory, err := totalMemory()
	if err != nil {
		return Sizing{}, err
	}
	return Size(memory, runtime.NumCPU()), nil
}
