package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/release"
)

// TestLiveSBXResize drives the real sbx daemon: a throwaway sandbox on the
// stock template is created at 1 CPU / 512 MiB, given a marker file,
// resized to 2 CPUs / 1 GiB by regeneration, and checked from inside. It
// runs only with WARDEN_SBX_LIVE=1 (it needs sandboxd, outside Claude's
// command sandbox) and cleans the sandbox and its template up.
func TestLiveSBXResize(t *testing.T) {
	if os.Getenv("WARDEN_SBX_LIVE") == "" {
		t.Skip("set WARDEN_SBX_LIVE=1 to drive the real sbx daemon")
	}
	sbx, err := exec.LookPath("sbx")
	if err != nil {
		t.Skip("no sbx on PATH")
	}
	w := NewWorker(t.TempDir(), sbx, release.StockTemplate)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 512}, Max: Resources{CPUMilli: 4000, MemoryMB: 4096}, CPUStepMilli: 1000, Restart: true}
	d := &sbxRuntime{worker: w}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "wc-live-resize-" + randomID()[:8]
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_ = d.Stop(cleanup, name)
		_ = d.Remove(cleanup, name)
		_ = command(cleanup, sbx, "template", "rm", "warden-resize-"+name).Run()
	})
	if err = d.Create(ctx, RuntimeSpec{Name: name, Directory: "/home/agent", Resources: Resources{CPUMilli: 1000, MemoryMB: 512}}); err != nil {
		t.Fatal(err)
	}
	probe := "echo marker=$(cat /home/agent/marker 2>/dev/null); echo nproc=$(nproc); grep MemTotal /proc/meminfo"
	if _, err = d.Exec(ctx, name, "/home/agent", "sh", "-c", "echo kept > /home/agent/marker"); err != nil {
		t.Fatal(err)
	}
	before, err := d.Exec(ctx, name, "/home/agent", "sh", "-c", probe)
	if err != nil || !strings.Contains(before, "nproc=1") {
		t.Fatalf("before: %q %v", before, err)
	}
	restarted, err := d.Resize(ctx, name, Resources{CPUMilli: 2000, MemoryMB: 1024})
	if err != nil || !restarted {
		t.Fatalf("resize: restarted=%v %v", restarted, err)
	}
	after, err := d.Exec(ctx, name, "/home/agent", "sh", "-c", probe)
	if err != nil || !strings.Contains(after, "marker=kept") || !strings.Contains(after, "nproc=2") {
		t.Fatalf("after: %q %v", after, err)
	}
	var memKB int
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			memKB, _ = parseKB(line)
		}
	}
	if memKB < 900*1024 || memKB > 1100*1024 {
		t.Fatalf("memory after resize: %d kB (%q)", memKB, after)
	}
	if out, _ := command(ctx, sbx, "template", "ls").Output(); strings.Contains(string(out), "warden-resize-"+name) {
		t.Fatal("resize left its template behind")
	}
	t.Logf("before:\n%s\nafter:\n%s", before, after)
}

func parseKB(line string) (int, error) {
	fields := strings.Fields(line)
	n := 0
	for _, r := range fields[1] {
		n = n*10 + int(r-'0')
	}
	return n, nil
}
