package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The private SBX namespace: the five HOME/XDG directories deploy/chat/
// warden-sbx selects on OVH, here under <state>/sbx. The wrapper script at
// <state>/bin/warden-sbx exports them and execs the pinned sbx, and every
// Warden command and service runs SBX only through it, so the operator's own
// SBX namespace is never inspected or changed (plan decision 10).
var namespaceDirs = []struct{ env, dir string }{
	{"HOME", "home"},
	{"XDG_CACHE_HOME", "cache"},
	{"XDG_STATE_HOME", "state"},
	{"XDG_CONFIG_HOME", "config"},
	{"XDG_DATA_HOME", "data"},
}

// wrapperName is the wrapper's file name under <state>/bin.
const wrapperName = "warden-sbx"

func wrapperPath(state string) string { return filepath.Join(state, "bin", wrapperName) }

// wrapperScript is the wrapper's content for a private home and executable.
func wrapperScript(privateHome, executable string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Warden's private SBX namespace; written by `warden install`, do not edit.\n# Equivalent to deploy/chat/warden-sbx on OVH.\n")
	for _, d := range namespaceDirs {
		fmt.Fprintf(&b, "export %s=%s\n", d.env, shellQuote(filepath.Join(privateHome, d.dir)))
	}
	fmt.Fprintf(&b, "exec %s \"$@\"\n", shellQuote(executable))
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureNamespace creates the five directories owner-only and writes the
// wrapper when missing or different. It reports whether anything changed.
func ensureNamespace(state, privateHome, executable string) (changed bool, err error) {
	if err = ensurePrivateDir(privateHome); err != nil {
		return false, err
	}
	for _, d := range namespaceDirs {
		if err = ensurePrivateDir(filepath.Join(privateHome, d.dir)); err != nil {
			return false, err
		}
	}
	if err = ensurePrivateDir(filepath.Join(state, "bin")); err != nil {
		return false, err
	}
	if err = ensureKeychainLink(privateHome); err != nil {
		return false, err
	}
	path := wrapperPath(state)
	want := wrapperScript(privateHome, executable)
	have, readErr := os.ReadFile(path)
	if readErr == nil && string(have) == want {
		return false, os.Chmod(path, 0o700)
	}
	return true, atomicWrite(path, []byte(want), 0o700)
}

// keychainLink is where macOS's Security framework looks for the login
// keychain under a HOME: sbx keeps its Docker session there, so the private
// namespace HOME links to the operator's real keychain directory. Without
// the link the namespace daemon sees only the System keychain ("no default
// account profile set: secret not found") and `sbx login` cannot save
// ("Keychain Error. (-60008)"). The Warden namespace therefore shares the
// operator's own sbx sign-in, which is the intended single-owner semantics.
func keychainLink(privateHome string) (link, target string, err error) {
	if runtime.GOOS != "darwin" {
		return "", "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(privateHome, "home", "Library", "Keychains"), filepath.Join(home, "Library", "Keychains"), nil
}

func ensureKeychainLink(privateHome string) error {
	link, target, err := keychainLink(privateHome)
	if err != nil || link == "" {
		return err
	}
	if have, err := os.Readlink(link); err == nil && have == target {
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		return fmt.Errorf("%s exists and is not a link to %s; move it aside", link, target)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// sbxCLI runs SBX through the wrapper.
type sbxCLI struct {
	wrapper string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
}

// Limits copied from the policy verifier's runner so doctor sees exactly
// what the verifier sees.
const (
	inspectTimeout = 4 * time.Second
	inspectLimit   = 1024 * 1024
)

// run mirrors SbxCliVerifier.run: a 4 s timeout, captured stdout capped at
// 1 MiB, and exit status 1 tolerated only when denied is set (a policy check
// that is expected to deny exits 1).
func (s *sbxCLI) run(args []string, denied bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.wrapper, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, limit: inspectLimit + 1}
	cmd.Stderr = &limitedWriter{w: &stderr, limit: 4096}
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !(denied && errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
			return "", fmt.Errorf("%s %s: %s", wrapperName, strings.Join(args, " "), describeExit(err, stderr.String()))
		}
	}
	if stdout.Len() > inspectLimit {
		return "", fmt.Errorf("%s %s: output exceeds %d bytes", wrapperName, strings.Join(args, " "), inspectLimit)
	}
	return stdout.String(), nil
}

// jsonObject runs args and decodes one JSON object.
func (s *sbxCLI) jsonObject(args []string, denied bool) (map[string]any, error) {
	out, err := s.run(args, denied)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	if err = dec.Decode(&value); err != nil || value == nil {
		return nil, fmt.Errorf("%s %s: output is not a JSON object", wrapperName, strings.Join(args, " "))
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s %s: trailing data after the JSON object", wrapperName, strings.Join(args, " "))
	}
	return value, nil
}

// command runs a longer, non-inspection operation (daemon start, settings
// set, template load) with its output captured and returned for the report.
func (s *sbxCLI) command(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.wrapper, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, limit: inspectLimit}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %s", wrapperName, strings.Join(args, " "), describeExit(err, out.String()))
	}
	return out.String(), nil
}

// interactive runs a command attached to the operator's terminal (the SBX
// device login).
func (s *sbxCLI) interactive(args ...string) error {
	cmd := exec.Command(s.wrapper, args...)
	cmd.Stdin = s.stdin
	cmd.Stdout = s.stdout
	cmd.Stderr = s.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", wrapperName, strings.Join(args, " "), err)
	}
	return nil
}

func describeExit(err error, output string) string {
	text := strings.TrimSpace(output)
	if text == "" {
		return err.Error()
	}
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	return err.Error() + ": " + text
}

// limitedWriter keeps the first limit bytes and drops the rest.
type limitedWriter struct {
	w     io.Writer
	limit int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.limit <= 0 {
		return len(p), nil
	}
	n := len(p)
	if n > l.limit {
		n = l.limit
	}
	l.limit -= n
	if _, err := l.w.Write(p[:n]); err != nil {
		return 0, err
	}
	return len(p), nil
}
