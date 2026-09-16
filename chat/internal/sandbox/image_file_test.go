package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageFileDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "private"), []byte("outside"), 0600)
	os.WriteFile(filepath.Join(root, "good"), []byte("image bytes"), 0600)
	os.Symlink(filepath.Join(outside, "private"), filepath.Join(root, "link"))
	os.Symlink(outside, filepath.Join(root, "dir"))
	for _, p := range []string{"../private", "/etc/passwd", "link", "dir/private", "./good"} {
		if _, err := exec.Command("python3", "-c", imageFileScript, root, p, "image-file").Output(); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	raw, err := exec.Command("python3", "-c", imageFileScript, root, "good", "image-file").Output()
	if err != nil {
		t.Fatal(err)
	}
	var r Response
	if json.Unmarshal(raw, &r) != nil || string(r.Bytes) != "image bytes" {
		t.Fatal("failed bounded read")
	}
}

func TestProposalFileBoundedRead(t *testing.T) {
	root := t.TempDir()
	script := strings.ReplaceAll(imageFileScript, "8*1024*1024", "2*1024*1024")
	os.WriteFile(filepath.Join(root, "large.json"), make([]byte, (2<<20)+1), 0600)
	os.Symlink("large.json", filepath.Join(root, "link.json"))
	for _, path := range []string{"large.json", "link.json", "../outside", "/etc/passwd"} {
		if _, err := exec.Command("python3", "-c", script, root, path, "proposal-file").Output(); err == nil {
			t.Fatalf("accepted invalid proposal path %s", path)
		}
	}
}
