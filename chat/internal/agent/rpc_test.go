package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStartUsesExecutionDirectory(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "cwd")
	t.Setenv("WORKSPACE_TEST_CWD", marker)
	executable := filepath.Join(root, "app-server")
	// Exercise the actual child process boundary: protocol parameters alone do
	// not control Codex's startup config and sandbox-root resolution.
	script := `#!/bin/sh
printf '%s' "$PWD" > "$WORKSPACE_TEST_CWD"
IFS= read -r initialize
printf '{"id":1,"result":{}}\n'
while IFS= read -r frame; do :; done
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client, err := Start(context.Background(), executable, filepath.Join(root, "logs"), repository, func(*Client, Frame) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	actual, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves /var to /private/var in a child's PWD.
	want, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(string(actual))
	if err != nil || got != want {
		t.Fatalf("child cwd = %q, want %q (error %v)", actual, want, err)
	}
}
