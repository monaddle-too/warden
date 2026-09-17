package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func completePaths(t *testing.T, root, query string) []string {
	t.Helper()
	raw, err := exec.Command("python3", "-c", pathsScript, root, query).Output()
	if err != nil {
		t.Fatalf("%q: %v", query, err)
	}
	var r Response
	if json.Unmarshal(raw, &r) != nil {
		t.Fatalf("%q: invalid reply %s", query, raw)
	}
	return r.Paths
}

func TestPathsCompletesShellStyle(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"src/components", "src/util", "docs", ".git/objects", "node_modules/left-pad", "Scripts"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	for _, file := range []string{"README.md", "src/components/Conversation.tsx", "src/components/CodeBlock.tsx", "src/conv.ts", "src/util/convert.ts", "docs/plan.md", ".env", "node_modules/left-pad/Conv.js", "bad\nname"} {
		os.WriteFile(filepath.Join(root, file), []byte("x"), 0644)
	}
	cases := []struct {
		query string
		want  []string
	}{
		// Directories first, then files, each alphabetical and case-insensitive;
		// hidden names, dependency directories and a control character stay out.
		{"", []string{"docs/", "node_modules/", "Scripts/", "src/", "README.md"}},
		{"S", []string{"Scripts/", "src/"}},
		{"src/", []string{"src/components/", "src/util/", "src/conv.ts"}},
		{"src/comp", []string{"src/components/"}},
		{"src/components/co", []string{"src/components/CodeBlock.tsx", "src/components/Conversation.tsx"}},
		// A hidden name is offered once the query asks for one.
		{".", []string{".git/", ".env"}},
		// Without a directory part the tree is searched for the prefix too.
		{"conv", []string{"src/components/Conversation.tsx", "src/conv.ts", "src/util/convert.ts"}},
		{"nothing-here", []string{}},
	}
	for _, c := range cases {
		got := completePaths(t, root, c.query)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.query, got, c.want)
		}
	}
}

func TestPathsStaysInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0644)
	os.Symlink(outside, filepath.Join(root, "link"))
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "file-link"))
	for _, q := range []string{"link/", "link/sec", "../", "/etc/", "./", "src//x", "src/../"} {
		if _, err := exec.Command("python3", "-c", pathsScript, root, q).Output(); err == nil {
			t.Errorf("accepted %q", q)
		}
	}
	// A symlink is listed by name, as a file, but never followed.
	if got := completePaths(t, root, "l"); !reflect.DeepEqual(got, []string{"link"}) {
		t.Fatalf("symlink listing: %v", got)
	}
	if got := completePaths(t, root, "sec"); len(got) != 0 {
		t.Fatalf("search followed a symlink: %v", got)
	}
}

func TestPathsIsBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 120; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d.txt", i)), nil, 0644)
	}
	if got := completePaths(t, root, "file"); len(got) != 50 || got[0] != "file-000.txt" {
		t.Fatalf("cap: %d %v", len(got), got[:1])
	}
	// Deep trees stop at the depth limit rather than walking everything.
	deep := root
	for i := 0; i < 9; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	os.MkdirAll(deep, 0755)
	os.WriteFile(filepath.Join(deep, "needle.txt"), nil, 0644)
	os.WriteFile(filepath.Join(root, "d0", "d1", "needle.txt"), nil, 0644)
	if got := completePaths(t, root, "needle"); !reflect.DeepEqual(got, []string{"d0/d1/needle.txt"}) {
		t.Fatalf("depth: %v", got)
	}
}
