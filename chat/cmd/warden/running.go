package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runningFile records the launcher running an instance: which version
// and binary, since when, on which addresses. `warden start` writes it in
// every mode (foreground, detached, service) once the stack is up and
// removes it on a clean stop; a file whose pid is dead is stale and reads
// as not running (docs/host-dogfood-plan.md, Part C).
const runningFile = "running.json"

type runningInfo struct {
	Version   string    `json:"version"`
	Release   string    `json:"release,omitempty"` // the release directory's name, "" for a bare build
	Binary    string    `json:"binary"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
	Chat      string    `json:"chat,omitempty"` // listen addresses
	Edge      string    `json:"edge,omitempty"`
}

func runningPath(state string) string { return filepath.Join(state, runningFile) }

func writeRunning(state string, r runningInfo) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(runningPath(state), append(b, '\n'), 0o600)
}

// removeRunning removes the record when it is this process's.
func removeRunning(state string) {
	r, _, err := readRunning(state)
	if err != nil || r.PID != os.Getpid() {
		return
	}
	os.Remove(runningPath(state))
}

// readRunning reads the record and reports whether its process is alive;
// a missing file is (zero, false, nil).
func readRunning(state string) (runningInfo, bool, error) {
	raw, err := os.ReadFile(runningPath(state))
	if errors.Is(err, os.ErrNotExist) {
		return runningInfo{}, false, nil
	}
	if err != nil {
		return runningInfo{}, false, err
	}
	var r runningInfo
	if err := json.Unmarshal(raw, &r); err != nil {
		return runningInfo{}, false, err
	}
	if r.PID <= 0 || processAlive(r.PID) != nil {
		return r, false, nil
	}
	return r, true, nil
}

// processAlive is nil when pid exists (signal 0).
func processAlive(pid int) error {
	return syscall.Kill(pid, 0)
}

// processBinary is the executable of a live process, resolved to its real
// path: /proc/<pid>/exe on Linux, the first txt file lsof lists on macOS
// (ps would report the symlink the process was started through, which a
// `release use` since may have repointed). "" when it cannot be told. It
// serves an instance whose launcher predates running.json.
func processBinary(pid int) string {
	if pid <= 0 {
		return ""
	}
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return exe
	}
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "txt", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return line[1:]
		}
	}
	return ""
}
