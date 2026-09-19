package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// `warden start` from one build re-executes the instance's own release
// when the state's release link points at a different binary, and does
// nothing without a link, when the link is this binary, or when it has
// already re-executed.
func TestStartExecsTheInstanceRelease(t *testing.T) {
	state := t.TempDir()
	var got []string
	c := &cli{stderr: &bytes.Buffer{}, execve: func(path string, argv, env []string) error { got = append([]string{path}, argv...); return nil }}
	if err := c.execInstanceRelease(state, []string{"--instance", "inner"}); err != nil || got != nil {
		t.Fatalf("no link: %v %q", err, got)
	}
	bin := filepath.Join(state, "releases", "warden-v1-darwin-arm64", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(bin, "warden")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(bin), filepath.Join(state, "release")); err != nil {
		t.Fatal(err)
	}
	if err := c.execInstanceRelease(state, []string{"--instance", "inner", "--detach"}); err != nil {
		t.Fatal(err)
	}
	other, _ = filepath.EvalSymlinks(other) // macOS: /var → /private/var
	want := []string{other, other, "start", "--instance", "inner", "--detach"}
	if len(got) != len(want) {
		t.Fatalf("execve = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execve = %q, want %q", got, want)
		}
	}
	if !bytes.Contains(c.stderr.(*bytes.Buffer).Bytes(), []byte("running the instance's release warden-v1-darwin-arm64")) {
		t.Fatalf("stderr: %s", c.stderr.(*bytes.Buffer).String())
	}
	got = nil
	t.Setenv(releaseReexecEnv, "1")
	if err := c.execInstanceRelease(state, nil); err != nil || got != nil {
		t.Fatalf("re-exec looped: %v %q", err, got)
	}
	// The guard is consumed: what this launcher starts (services, host
	// commands) must not inherit it, or their own `warden start
	// --instance X` would run this binary against X's state.
	if os.Getenv(releaseReexecEnv) != "" {
		t.Fatal("the re-exec guard leaked into the launcher's environment")
	}
}
