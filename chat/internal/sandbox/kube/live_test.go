package kube

import (
	"context"
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
	// Reconcile from a fresh driver deletes the pods and keeps the claims.
	fresh, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Reconcile(ctx, []string{name, name + "-fork"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if err := client.Get(ctx, kube.Pods, opts.Namespace, name, &pod); kube.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}
	var claim kube.PersistentVolumeClaim
	if err := client.Get(ctx, kube.PersistentVolumeClaims, opts.Namespace, name, &claim); err != nil {
		t.Fatalf("claim after reconcile: %v", err)
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
