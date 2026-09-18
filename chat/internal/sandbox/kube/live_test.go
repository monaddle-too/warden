package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// TestLiveDriverAgainstTheDevCluster drives a real cluster when
// WARDEN_KUBE_LIVE_KUBECONFIG names a kubeconfig (docs/warden-kubernetes-plan.md,
// appendix B: the Lima dev VM). It needs the namespace, RuntimeClass, image
// digest and trust ConfigMap below (WARDEN_KUBE_LIVE_* override them) and
// deletes what it creates. It is skipped otherwise, so the unit suite never
// needs a cluster.
func TestLiveDriverAgainstTheDevCluster(t *testing.T) {
	kubeconfig := os.Getenv("WARDEN_KUBE_LIVE_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("WARDEN_KUBE_LIVE_KUBECONFIG unset")
	}
	env := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	opts := Options{
		Namespace:        env("WARDEN_KUBE_LIVE_NAMESPACE", "warden-spike-sandboxes"),
		Tier:             env("WARDEN_KUBE_LIVE_TIER", "gvisor"),
		RuntimeClass:     env("WARDEN_KUBE_LIVE_RUNTIME_CLASS", "gvisor"),
		GuestImage:       env("WARDEN_KUBE_LIVE_IMAGE", "warden-guest-base"),
		GuestImageDigest: env("WARDEN_KUBE_LIVE_DIGEST", "sha256:cd77d0ff2af115f3cb700414c7c54840c2c1c87c3322ea0a98c17880ef07b63e"),
		StorageClass:     env("WARDEN_KUBE_LIVE_STORAGE_CLASS", "local-path"),
		WorkspaceSizeGi:  1,
		TrustConfigMap:   env("WARDEN_KUBE_LIVE_TRUST", "spike-trust"),
		MemoryMB:         1024,
		ImagePullPolicy:  env("WARDEN_KUBE_LIVE_PULL_POLICY", ""),
	}
	cfg, err := kube.LoadKubeconfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kube.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	name := "wc-live-" + strings.ToLower(time.Now().Format("150405"))
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_ = d.Remove(cleanup, name)
		_ = d.Remove(cleanup, name+"-fork")
	})
	spec := sandbox.RuntimeSpec{Name: name, Directory: "/home/agent/workspace", SandboxID: "live-sandbox", Generation: "gen-1"}
	started := time.Now()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	rt, _ := d.Runtime(name)
	t.Logf("created in %s: claim %s pod %s at %s", time.Since(started).Round(time.Millisecond), rt.ClaimUID, rt.PodUID, rt.PodIP)
	// The pod the cluster holds is the golden shape and reports the pinned
	// image.
	var pod kube.Pod
	if err := client.Get(ctx, kube.Pods, opts.Namespace, name, &pod); err != nil {
		t.Fatal(err)
	}
	if got := pod.Status.ContainerStatuses[0].ImageID; !strings.HasSuffix(got, opts.GuestImageDigest) {
		t.Errorf("imageID %q does not end with the pinned digest", got)
	}
	// The guest report the worker takes, in the directory it asks for.
	report, err := d.Exec(ctx, name, "/tmp", "sh", "-c", "mkdir -p \"$0\" && echo WARDEN-GUEST-BEGIN; cat /opt/warden/guest-manifest.json; echo; echo WARDEN-GUEST-END; id -u; pwd; test -x /opt/warden/runtime/bin/codex && echo codex-present", spec.Directory)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(report, `"paths"`) || !strings.Contains(report, "\n1000\n/tmp\n") || !strings.Contains(report, "codex-present") {
		t.Fatalf("report: %q", report)
	}
	// sudo works (allow-suid on the gVisor node), the trust mount is the
	// ConfigMap's bundle, /tmp is writable, exit codes come through.
	if out, err := d.Exec(ctx, name, spec.Directory, "sudo", "-n", "id", "-u"); err != nil || strings.TrimSpace(out) != "0" {
		t.Fatalf("sudo: %q %v", out, err)
	}
	if out, err := d.Exec(ctx, name, "/", "sh", "-c", "readlink -f /etc/ssl/certs/ca-certificates.crt; wc -c < /opt/warden/trust/ca-certificates.crt"); err != nil || !strings.Contains(out, TrustMountPath) {
		t.Fatalf("trust: %q %v", out, err)
	}
	if _, err := d.Exec(ctx, name, "/", "sh", "-c", "exit 7"); err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("exit code: %v", err)
	}
	// Copy a file and a directory, as the worker copies a git bundle and
	// the runtime bundle.
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "bundle.git"), []byte("bundle bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Copy(ctx, name, filepath.Join(host, "bundle.git"), "/tmp/warden-live.bundle"); err != nil {
		t.Fatalf("copy file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(host, "tree", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "tree", "bin", "tool"), []byte("#!/bin/sh\necho tool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Copy(ctx, name, filepath.Join(host, "tree"), "/opt/warden/live-stage"); err != nil {
		t.Fatalf("copy directory: %v", err)
	}
	out, err := d.Exec(ctx, name, "/", "sh", "-c", "stat -c '%U %a' /tmp/warden-live.bundle /opt/warden/live-stage /opt/warden/live-stage/bin/tool; cat /tmp/warden-live.bundle; /opt/warden/live-stage/bin/tool")
	if err != nil || !strings.Contains(out, "root 644\n") || !strings.Contains(out, "root 755\n") || !strings.Contains(out, "bundle bytes") || !strings.Contains(out, "tool\n") {
		t.Fatalf("copied content: %q %v", out, err)
	}
	// A stream through the launch wrapper: the command runs in the
	// directory and both directions work (an echo stands in for the agent).
	if out, err := d.Exec(ctx, name, spec.Directory, "pwd"); err != nil || strings.TrimSpace(out) != spec.Directory {
		t.Fatalf("directory: %q %v", out, err)
	}
	// Previews: a server in the guest is reachable at the pod IP from the
	// runner's side only through the cluster network, so here we check the
	// mapping and that the pod UID guard holds.
	m, err := d.Publish(ctx, name, 8080, func(sandbox.PortMapping) error { return nil })
	if err != nil || m.Address != rt.PodIP || m.Port != 8080 {
		t.Fatalf("publish: %+v %v", m, err)
	}
	if got, err := d.Mappings(ctx, name); err != nil || len(got) != 1 {
		t.Fatalf("mappings: %v %v", got, err)
	}
	// Stop keeps the claim; resume through Prepare makes a new generation
	// on the same claim and the workspace persists.
	if _, err := d.Exec(ctx, name, spec.Directory, "sh", "-c", "echo persisted > note.txt"); err != nil {
		t.Fatal(err)
	}
	stopped := time.Now()
	if err := d.Stop(ctx, name); err != nil {
		t.Fatalf("stop: %v", err)
	}
	t.Logf("stopped in %s", time.Since(stopped).Round(time.Millisecond))
	if err := client.Get(ctx, kube.Pods, opts.Namespace, name, &pod); !kube.IsNotFound(err) {
		t.Fatalf("pod after stop: %v", err)
	}
	resumed := time.Now()
	spec.Generation = "gen-2"
	if _, err := d.Prepare(ctx, spec); err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Logf("resumed in %s", time.Since(resumed).Round(time.Millisecond))
	again, _ := d.Runtime(name)
	if again.ClaimUID != rt.ClaimUID || again.PodUID == rt.PodUID || again.Generation != "gen-2" {
		t.Fatalf("resume record %+v (was %+v)", again, rt)
	}
	if out, err := d.Exec(ctx, name, spec.Directory, "cat", "note.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("workspace did not persist: %q %v", out, err)
	}
	if _, err := d.Mappings(ctx, name); err != nil || len(mustMappings(t, d, ctx, name)) != 0 {
		t.Fatalf("publications survived the stop: %v", err)
	}
	// A fork on local-path: the dataSource is ignored (an empty volume), so
	// the driver copies the home through exec and records it.
	forked := time.Now()
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: name + "-fork", Directory: spec.Directory, SandboxID: "live-fork", Generation: "gen-1", Source: name}); err != nil {
		t.Fatalf("fork: %v", err)
	}
	fork, _ := d.Runtime(name + "-fork")
	t.Logf("forked in %s: workspace %s", time.Since(forked).Round(time.Millisecond), fork.Workspace)
	if out, err := d.Exec(ctx, name+"-fork", spec.Directory, "cat", "note.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("fork did not carry the workspace: %q %v", out, err)
	}
	// Reconcile from a fresh driver keeps the resident pod at its
	// generation, rebuilding the record, deletes the other pod and keeps
	// both claims.
	fresh, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	resident, err := fresh.Reconcile(ctx, []sandbox.RegisteredRuntime{{Name: name, Generation: "gen-2", Resident: true}, {Name: name + "-fork", Generation: "gen-1"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if strings.Join(resident, ",") != name {
		t.Fatalf("resident after reconcile: %v", resident)
	}
	if kept, ok := fresh.Runtime(name); !ok || kept.PodUID != again.PodUID || kept.ClaimUID != again.ClaimUID || kept.Generation != "gen-2" {
		t.Fatalf("kept record %+v (was %+v)", kept, again)
	}
	if out, err := fresh.Exec(ctx, name, spec.Directory, "cat", "note.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("kept pod not usable: %q %v", out, err)
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if err := client.Get(ctx, kube.Pods, opts.Namespace, name+"-fork", &pod); kube.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}
	if err := client.Get(ctx, kube.Pods, opts.Namespace, name+"-fork", &pod); !kube.IsNotFound(err) {
		t.Fatalf("fork pod after reconcile: %v", err)
	}
	var claim kube.PersistentVolumeClaim
	for _, n := range []string{name, name + "-fork"} {
		if err := client.Get(ctx, kube.PersistentVolumeClaims, opts.Namespace, n, &claim); err != nil {
			t.Fatalf("claim %s after reconcile: %v", n, err)
		}
	}
	if err := d.Remove(ctx, name+"-fork"); err != nil {
		t.Fatalf("remove fork: %v", err)
	}
	if err := d.Remove(ctx, name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := client.Get(ctx, kube.PersistentVolumeClaims, opts.Namespace, name, &claim); err == nil && claim.Metadata.DeletionTimestamp == nil {
		t.Fatal("claim survived remove")
	}
}

func mustMappings(t *testing.T, d *Driver, ctx context.Context, name string) []sandbox.PortMapping {
	t.Helper()
	got, err := d.Mappings(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestLiveResize proves the resize on a real cluster
// (WARDEN_KUBE_LIVE_KUBECONFIG, as TestLiveDriverAgainstTheDevCluster):
// a pod created at 0.5 CPU and 1 GiB cannot hold a 1.4 GiB allocation;
// after the resize to 1.5 CPUs and 2 GiB it can, and a busy loop gets
// more done. The resize is in place where the cluster's runtime does it
// (upstream runsc on the dev cluster: same pod) and otherwise what the
// worker falls back to (GKE Sandbox's gVisor shim answers Unimplemented):
// stop and a new generation at the size, which the test performs as the
// worker would and reports. A memory decrease is then attempted the same
// way. The pod is removed at the end.
func TestLiveResize(t *testing.T) {
	kubeconfig := os.Getenv("WARDEN_KUBE_LIVE_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("WARDEN_KUBE_LIVE_KUBECONFIG unset")
	}
	env := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	opts := Options{
		Namespace:        env("WARDEN_KUBE_LIVE_NAMESPACE", "warden-spike-sandboxes"),
		Tier:             env("WARDEN_KUBE_LIVE_TIER", "gvisor"),
		RuntimeClass:     env("WARDEN_KUBE_LIVE_RUNTIME_CLASS", "gvisor"),
		GuestImage:       env("WARDEN_KUBE_LIVE_IMAGE", "warden-guest-base"),
		GuestImageDigest: env("WARDEN_KUBE_LIVE_DIGEST", "sha256:cd77d0ff2af115f3cb700414c7c54840c2c1c87c3322ea0a98c17880ef07b63e"),
		StorageClass:     env("WARDEN_KUBE_LIVE_STORAGE_CLASS", "local-path"),
		WorkspaceSizeGi:  1,
		TrustConfigMap:   env("WARDEN_KUBE_LIVE_TRUST", "spike-trust"),
		MemoryMB:         1024,
		ImagePullPolicy:  env("WARDEN_KUBE_LIVE_PULL_POLICY", ""),
	}
	cfg, err := kube.LoadKubeconfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kube.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	name := "wc-live-" + strings.ToLower(time.Now().Format("150405"))
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_ = d.Remove(cleanup, name)
	})
	small := sandbox.Resources{CPUMilli: 500, MemoryMB: 1024}
	large := sandbox.Resources{CPUMilli: 1500, MemoryMB: 2048}
	spec := sandbox.RuntimeSpec{Name: name, Directory: "/home/agent/workspace", SandboxID: "live-resize", Generation: "gen-1", Resources: small}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, _ := d.Runtime(name)
	// 1.4 GiB, touched page by page so the limit is really met.
	const allocate = `import sys; n=1400; bs=[bytearray(1<<20) for _ in range(n)]; print("held %d MiB" % n)`
	alloc := func() (string, error) { return d.Exec(ctx, name, "/tmp", "python3", "-c", allocate) }
	// A busy loop in as many processes as the larger size has CPUs; the
	// count is what the limit lets through in the window.
	const spin = `import time, multiprocessing as mp
def work(q):
    end = time.monotonic() + 3.0; n = 0
    while time.monotonic() < end:
        for _ in range(10000): pass
        n += 1
    q.put(n)
if __name__ == "__main__":
    q = mp.Queue(); ps = [mp.Process(target=work, args=(q,)) for _ in range(2)]
    [p.start() for p in ps]; [p.join() for p in ps]
    print(sum(q.get() for _ in ps))`
	spinCount := func() int {
		out, err := d.Exec(ctx, name, "/tmp", "python3", "-c", spin)
		if err != nil {
			t.Fatalf("spin: %v", err)
		}
		n := 0
		for _, c := range strings.TrimSpace(out) {
			n = n*10 + int(c-'0')
		}
		return n
	}
	slow := spinCount()
	if out, err := alloc(); err == nil {
		t.Fatalf("1.4 GiB fit in a 1 GiB pod: %q", out)
	} else {
		t.Logf("at %s the allocation failed as it should: %v", small, err)
	}
	// Under gVisor the OOM takes the container down and the kubelet
	// restarts it on the same pod; wait until exec works and keeps
	// working (the restart settles in two steps).
	recovered := time.Now()
	for good := 0; good < 3; {
		if _, err := d.Exec(ctx, name, "/", "true"); err == nil {
			good++
		} else if time.Since(recovered) > 3*time.Minute {
			t.Fatalf("container did not come back after the OOM: %v", err)
		} else {
			good = 0
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("container back %s after the OOM", time.Since(recovered).Round(time.Millisecond))
	// resize is the worker's resizeLocked: in place, else stop and the
	// next generation at the size.
	generation := 1
	resize := func(r sandbox.Resources) (replaced bool) {
		t.Helper()
		started := time.Now()
		restarted, err := d.Resize(ctx, name, r)
		switch {
		case err == nil && !restarted:
			t.Logf("resized to %s in place in %s", r, time.Since(started).Round(time.Millisecond))
			return false
		case errors.Is(err, sandbox.ErrResizeInfeasible):
			t.Logf("resize to %s not applied in place (%v); replacing the pod", r, err)
		default:
			t.Fatalf("resize to %s: restarted=%v %v", r, restarted, err)
		}
		if err := d.Stop(ctx, name); err != nil {
			t.Fatalf("stop: %v", err)
		}
		generation++
		next := spec
		next.Generation = fmt.Sprintf("gen-%d", generation)
		next.Resources = r
		if _, err := d.Prepare(ctx, next); err != nil {
			t.Fatalf("prepare at %s: %v", r, err)
		}
		t.Logf("replaced at %s in %s", r, time.Since(started).Round(time.Millisecond))
		return true
	}
	replaced := resize(large)
	after, _ := d.Runtime(name)
	if (after.PodUID != before.PodUID) != replaced {
		t.Fatalf("pod %s (was %s), replaced=%v", after.PodUID, before.PodUID, replaced)
	}
	var pod kube.Pod
	if err := client.Get(ctx, kube.Pods, opts.Namespace, name, &pod); err != nil {
		t.Fatal(err)
	}
	if applied := pod.Status.ContainerStatuses[0].Resources; applied == nil || applied.Limits["memory"] != "2048Mi" && applied.Limits["memory"] != "2Gi" {
		t.Fatalf("status resources after resize: %+v", applied)
	}
	if out, err := alloc(); err != nil {
		t.Fatalf("1.4 GiB did not fit after the resize to %s: %v", large, err)
	} else {
		t.Logf("after the resize: %s", strings.TrimSpace(out))
	}
	fast := spinCount()
	t.Logf("busy loop: %d iterations at 0.5 CPU, %d at 1.5 CPUs (%.1fx)", slow, fast, float64(fast)/float64(slow))
	if fast < slow*3/2 {
		t.Errorf("CPU limit did not grow: %d → %d", slow, fast)
	}
	if out, err := d.Exec(ctx, name, "/", "sh", "-c", "nproc; grep MemTotal /proc/meminfo"); err == nil {
		t.Logf("guest reports after the resize: %s", strings.ReplaceAll(strings.TrimSpace(out), "\n", "; "))
	}
	// A decrease: the kubelet may apply it in place or refuse it while
	// the container uses more than the new limit; either way the pod ends
	// at the small size and the big allocation fails again.
	resize(small)
	if err := client.Get(ctx, kube.Pods, opts.Namespace, name, &pod); err != nil {
		t.Fatal(err)
	}
	if got := pod.Spec.Containers[0].Resources.Limits["memory"]; got != "1024Mi" && got != "1Gi" {
		t.Fatalf("pod after the decrease: %v", got)
	}
	if out, err := alloc(); err == nil {
		t.Fatalf("1.4 GiB fit after the decrease to %s: %q", small, out)
	}
	if err := d.Remove(ctx, name); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
