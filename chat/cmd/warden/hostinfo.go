package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// Host detection. chat/internal/hostinfo (Track A) will hold the shared
// versions of these; until that merge the launcher carries minimal ones.

// sbxCandidates are tried after $PATH when no --sbx is given.
var sbxCandidates = []string{"/opt/homebrew/bin/sbx", "/usr/local/bin/sbx", "/usr/bin/sbx"}

// findSBX returns the sbx executable: the explicit value when given, else
// the first of $PATH and the usual install locations.
func findSBX(explicit string) (string, error) {
	if explicit != "" {
		path, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if err = executableFile(path); err != nil {
			return "", fmt.Errorf("--sbx %s: %w", explicit, err)
		}
		return path, nil
	}
	if path, err := exec.LookPath("sbx"); err == nil {
		if path, err = filepath.Abs(path); err == nil {
			return path, nil
		}
	}
	for _, path := range sbxCandidates {
		if executableFile(path) == nil {
			return path, nil
		}
	}
	return "", errors.New("sbx (Docker Sandboxes) was not found on $PATH or in " + fmt.Sprint(sbxCandidates) + "; install it or pass --sbx PATH")
}

func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("not an executable file")
	}
	return nil
}

// guestArch is the guest architecture for this host: guests run the host's
// CPU architecture (Apple Silicon → arm64 Linux guests, x86_64 → amd64).
func guestArch() (string, error) {
	switch runtime.GOARCH {
	case release.AMD64, release.ARM64:
		return runtime.GOARCH, nil
	}
	return "", fmt.Errorf("unsupported host architecture %s; Warden runs on Apple Silicon Macs and x86_64 Linux hosts", runtime.GOARCH)
}

// codexTarget is the Codex bundle target triple for a guest architecture,
// as chat/internal/sandbox/runtime.go expects it.
func codexTarget(arch string) string {
	if arch == release.AMD64 {
		return "x86_64-unknown-linux-musl"
	}
	return "aarch64-unknown-linux-musl"
}

// sizeSandboxes derives the sandbox capacity from host memory and cores:
// 1536 MiB per sandbox (2048 MiB on hosts with 32 GiB or more), at most one
// sandbox per two cores, and 4 GiB kept for the host. Zero memory (unknown)
// keeps the config defaults.
func sizeSandboxes(base config.Sandboxes, memoryMB, cpus int) config.Sandboxes {
	s := base
	if memoryMB <= 0 {
		return s
	}
	if memoryMB >= 32*1024 {
		s.MemoryMB = 2048
	} else {
		s.MemoryMB = 1536
	}
	running := (memoryMB - 4096) / s.MemoryMB
	if cpus > 0 && running > cpus/2 {
		running = cpus / 2
	}
	if running > 4 {
		running = 4
	}
	if running < 1 {
		running = 1
	}
	s.MaxRunning = running
	if running >= 2 {
		s.WarmSpares = 1
	} else {
		s.WarmSpares = 0
	}
	return s
}

// freeLoopbackPorts returns n distinct free TCP ports on 127.0.0.1, keeping
// each preferred port that is free and otherwise scanning upward from it.
// avoid are ports to treat as taken although nothing listens on them
// right now: the ports of the other instances on this machine, which may
// be stopped.
func freeLoopbackPorts(avoid []int, preferred ...int) ([]int, error) {
	var out []int
	taken := map[int]bool{}
	for _, p := range avoid {
		taken[p] = true
	}
	for _, want := range preferred {
		port := want
		for tries := 0; tries < 200; tries++ {
			if !taken[port] && portFree(port) {
				break
			}
			port++
		}
		if taken[port] || !portFree(port) {
			return nil, fmt.Errorf("no free loopback port near %d", want)
		}
		taken[port] = true
		out = append(out, port)
	}
	return out, nil
}

func portFree(port int) bool {
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// listenAddr replaces the port of a loopback host:port.
func listenAddr(addr string, port int) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
