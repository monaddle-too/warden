package kube

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

const runtimeName = "wc-0123456789abcdef01234567"

func readyFake(t *testing.T) (*fakeAPI, *Driver) {
	t.Helper()
	api := newFakeAPI(t)
	api.publishTrust("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	return api, newTestDriver(t, api, testOptions())
}

// Create makes the claim then the pod, waits for it to run, reads the
// manifest and records the identities; a second Create adopts what exists.
func TestCreateIsClaimThenPodAndIdempotent(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	spec := sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", SandboxID: "sandbox-one", Generation: "gen-1"}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	claim, ok := api.claim(runtimeName)
	if !ok || claim.Metadata.Labels[LabelSandbox] != runtimeName || claim.Metadata.Annotations[AnnotationSandboxID] != "sandbox-one" {
		t.Fatalf("claim %+v", claim.Metadata)
	}
	pod, ok := api.pod(runtimeName)
	if !ok || pod.Status.Phase != "Running" || pod.Metadata.Annotations[AnnotationGeneration] != "gen-1" || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != runtimeName {
		t.Fatalf("pod %+v", pod)
	}
	rt, ok := d.Runtime(runtimeName)
	if !ok || rt.ClaimUID != claim.Metadata.UID || rt.PodUID != pod.Metadata.UID || rt.PodIP != pod.Status.PodIP || rt.Generation != "gen-1" || rt.Workspace != WorkspaceFresh {
		t.Fatalf("record %+v (claim %s pod %s)", rt, claim.Metadata.UID, pod.Metadata.UID)
	}
	// The claim was created before the pod, and the manifest read after
	// the pod ran.
	var order []string
	for _, r := range api.recorded() {
		if r.Method == "POST" {
			order = append(order, r.Path[strings.LastIndex(r.Path, "/")+1:])
		}
		if strings.HasSuffix(r.Path, "/exec") {
			order = append(order, "exec")
		}
	}
	if strings.Join(order, " ") != "persistentvolumeclaims pods exec" {
		t.Fatalf("order %v", order)
	}
	calls := api.guest.recorded()
	if len(calls) != 1 || strings.Join(calls[0].Command, " ") != "cat "+GuestManifestPath {
		t.Fatalf("guest calls %+v", calls)
	}
	if addr, err := d.Address(ctx, runtimeName); err != nil || addr != pod.Status.PodIP {
		t.Fatalf("address %q %v", addr, err)
	}
	// Again: nothing new is created and the record is the same.
	before := len(api.recorded())
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded()[before:] {
		if r.Method == "POST" && !strings.HasSuffix(r.Path, "/exec") {
			t.Fatalf("second Create posted %s", r.Path)
		}
	}
	if again, _ := d.Runtime(runtimeName); again != rt {
		t.Fatalf("record changed on the idempotent Create: %+v vs %+v", again, rt)
	}
	// A pod or claim with the name but not this driver's labels is refused.
	api.mu.Lock()
	api.objects["pods/wc-foreign"] = map[string]any{"metadata": map[string]any{"name": "wc-foreign", "uid": "x", "labels": map[string]any{LabelSandbox: "wc-foreign"}}, "status": map[string]any{"phase": "Running", "podIP": "10.42.0.99"}}
	api.objects["persistentvolumeclaims/wc-foreign"] = map[string]any{"metadata": map[string]any{"name": "wc-foreign", "uid": "y", "labels": map[string]any{LabelSandbox: "wc-foreign", LabelManagedBy: ManagedBy}}}
	api.mu.Unlock()
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-foreign", Directory: "/home/agent/workspace"}); err == nil || !strings.Contains(err.Error(), "unmanaged pod") {
		t.Fatalf("foreign pod adopted: %v", err)
	}
}

// Prepare makes a stopped runtime resident again: the claim is there, the
// pod of the new generation is created, and the handle is a no-op. On a
// running runtime it confirms the pod and creates nothing.
func TestPrepareRecreatesThePodAfterAStop(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", SandboxID: "s1", Generation: "gen-1"}); err != nil {
		t.Fatal(err)
	}
	first, _ := d.Runtime(runtimeName)
	before := len(api.recorded())
	h, err := d.Prepare(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", SandboxID: "s1", Generation: "gen-1"})
	if err != nil || h == nil {
		t.Fatal(err)
	}
	if _, ok := h.(sandbox.NoResidency); !ok {
		t.Fatalf("%T", h)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded()[before:] {
		if r.Method == "POST" {
			t.Fatalf("Prepare of a running guest posted %s", r.Path)
		}
	}
	if err := d.Stop(ctx, runtimeName); err != nil {
		t.Fatal(err)
	}
	// The worker resumes: a new generation, the fork source still named in
	// the spec (ignored: the workspace was made at creation).
	if _, err = d.Prepare(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", Source: "wc-gone", SandboxID: "s1", Generation: "gen-2"}); err != nil {
		t.Fatal(err)
	}
	pod, ok := api.pod(runtimeName)
	if !ok || pod.Status.Phase != "Running" || pod.Metadata.UID == first.PodUID || pod.Metadata.Annotations[AnnotationGeneration] != "gen-2" {
		t.Fatalf("resumed pod %+v", pod.Metadata)
	}
	claim, _ := api.claim(runtimeName)
	if rt, _ := d.Runtime(runtimeName); rt.ClaimUID != first.ClaimUID || claim.Metadata.UID != first.ClaimUID || rt.PodUID != pod.Metadata.UID || rt.Generation != "gen-2" || rt.PodIP != pod.Status.PodIP {
		t.Fatalf("resumed record %+v", rt)
	}
	// A Prepare with no claim at all (never created) still works: Prepare
	// is Create without the fork, so a lost claim is a fresh workspace.
	if _, err = d.Prepare(ctx, sandbox.RuntimeSpec{Name: "wc-never", Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
}

// The driver refuses an image it cannot run: no manifest, no paths, a
// home other than the mount, a different account.
func TestCreateRefusesImagesWithoutTheContract(t *testing.T) {
	cases := map[string]struct{ manifest, want string }{
		"no manifest":     {"", "has no manifest"},
		"sbx layout":      {`{"platform":"linux/arm64","variant":"sbx","codex":{"version":"0.154.0"}}`, "lacks the paths object"},
		"other home":      {strings.Replace(guestManifestJSON, `"home":"/home/agent"`, `"home":"/root"`, 1), "is not the mounted workspace"},
		"other account":   {strings.Replace(guestManifestJSON, `"uid":1000`, `"uid":1001`, 1), "the pod runs as 1000"},
		"trust elsewhere": {strings.Replace(guestManifestJSON, `"trust":"/opt/warden/trust/ca-certificates.crt"`, `"trust":"/etc/ssl/certs/ca-certificates.crt"`, 1), "outside the trust mount"},
		"not json":        {"{", "not JSON"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api, d := readyFake(t)
			api.guest.manifest = c.manifest
			err := d.Create(testContext(t), sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// A container the kubelet cannot start fails Create at once with the
// reason; a pod that never schedules fails when the context ends, naming
// the last condition.
func TestCreateReportsStartFailures(t *testing.T) {
	api, d := readyFake(t)
	api.failStart = "ErrImageNeverPull"
	err := d.Create(testContext(t), sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"})
	if err == nil || !strings.Contains(err.Error(), "ErrImageNeverPull") {
		t.Fatalf("image failure not reported: %v", err)
	}
	api2, d2 := readyFake(t)
	api2.holdStart = true
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = d2.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending pod did not time out: %v", err)
	}
}

// Stop deletes the pod with the grace period and waits it out, keeping the
// claim; Remove deletes the claim too. Both are fine on what is gone.
func TestStopKeepsTheClaimAndRemoveDeletesIt(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, runtimeName); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.pod(runtimeName); ok {
		t.Fatal("pod still present after Stop")
	}
	if _, ok := api.claim(runtimeName); !ok {
		t.Fatal("Stop deleted the claim")
	}
	var deleted *fakeRequest
	for _, r := range api.recorded() {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/pods/"+runtimeName) {
			r := r
			deleted = &r
		}
	}
	if deleted == nil || !strings.Contains(string(deleted.Body), `"gracePeriodSeconds":10`) {
		t.Fatalf("delete request %+v", deleted)
	}
	rt, _ := d.Runtime(runtimeName)
	if rt.PodUID != "" || rt.PodIP != "" || rt.ClaimUID == "" {
		t.Fatalf("record after stop %+v", rt)
	}
	if err := d.Stop(ctx, runtimeName); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	// Resume: a new pod on the same claim, a new generation.
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", Generation: "gen-2"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := d.Runtime(runtimeName); again.ClaimUID != rt.ClaimUID || again.PodUID == "" || again.Generation != "gen-2" {
		t.Fatalf("resume %+v", again)
	}
	if err := d.Remove(ctx, runtimeName); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.claim(runtimeName); ok {
		t.Fatal("Remove kept the claim")
	}
	if _, ok := d.Runtime(runtimeName); ok {
		t.Fatal("record kept after Remove")
	}
	if err := d.Remove(ctx, runtimeName); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// A Create right after a Stop whose pod is still terminating waits for the
// old pod to go and creates a new one.
func TestCreateWaitsOutATerminatingPod(t *testing.T) {
	api, d := readyFake(t)
	api.deleteDelay = 200 * time.Millisecond
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	first, _ := d.Runtime(runtimeName)
	// Delete behind the driver's back, as Reconcile of an earlier runner
	// would, and create at once.
	if err := d.client.Delete(ctx, kube.Pods, testNamespace, runtimeName, kube.DeleteOptions{GracePeriodSeconds: kube.Int64(StopGraceSeconds)}); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	second, _ := d.Runtime(runtimeName)
	if second.PodUID == first.PodUID || second.PodUID == "" {
		t.Fatalf("pod not replaced: %+v %+v", first, second)
	}
}

// The first Create waits for the trust bundle and says so; a bundle that
// arrives releases it; a runner that may not read the ConfigMap proceeds.
func TestReadinessWaitsForTheTrustBundle(t *testing.T) {
	api := newFakeAPI(t)
	d := newTestDriver(t, api, testOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"})
	if err == nil || !strings.Contains(err.Error(), "waiting for the guest trust bundle") {
		t.Fatalf("missing ConfigMap: %v", err)
	}
	if names := api.names("pods"); len(names) != 0 {
		t.Fatalf("pods created before the bundle: %v", names)
	}
	api.publishTrust("") // the chart-created empty ConfigMap
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	err = d.Create(ctx2, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"})
	if err == nil || !strings.Contains(err.Error(), "waiting for the guest trust bundle") {
		t.Fatalf("empty ConfigMap: %v", err)
	}
	// Published while the driver waits: the watch releases it.
	go func() {
		time.Sleep(100 * time.Millisecond)
		api.publishTrust("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	}()
	if err = d.Create(testContext(t), sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	// Once ready, the ConfigMap is not consulted again.
	before := len(api.recorded())
	if err = d.Create(testContext(t), sandbox.RuntimeSpec{Name: "wc-second", Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded()[before:] {
		if strings.Contains(r.Path, "configmaps") {
			t.Fatal("trust ConfigMap read after readiness")
		}
	}
	// No RBAC on the ConfigMap: proceed (the policy service publishes
	// before it serves), never block.
	api3 := newFakeAPI(t)
	api3.forbidden["get configmaps"] = true
	d3 := newTestDriver(t, api3, testOptions())
	if err = d3.Create(testContext(t), sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatalf("forbidden ConfigMap blocked creation: %v", err)
	}
}

// Reconcile at startup deletes every pod under the driver's labels (the
// worker has stopped everything it knows), deletes unregistered spare
// claims and keeps every other claim; the policy service's pods in the
// namespace are not touched.
func TestReconcileRetiresStalePodsAndSpareClaims(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	for _, spec := range []sandbox.RuntimeSpec{
		{Name: "wc-registered", Directory: "/home/agent/workspace", SandboxID: "s1"},
		{Name: "wc-orphan", Directory: "/home/agent/workspace", SandboxID: "s2"},
		{Name: "wc-spare-adopted", Directory: "/home/agent/workspace", Spare: true},
		{Name: "wc-spare-lost", Directory: "/home/agent/workspace", Spare: true},
	} {
		if err := d.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	api.mu.Lock()
	api.objects["pods/canary"] = map[string]any{"metadata": map[string]any{"name": "canary", "uid": "canary", "labels": map[string]any{LabelSandbox: "canary", LabelManagedBy: "warden-policy"}}, "status": map[string]any{"phase": "Running"}}
	api.mu.Unlock()
	fresh := newTestDriver(t, api, testOptions())
	if err := fresh.Reconcile(ctx, []string{"wc-registered", "wc-spare-adopted"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * api.deleteDelay)
	if names := api.names("pods"); strings.Join(names, ",") != "canary" {
		t.Fatalf("pods after reconcile: %v", names)
	}
	if names := api.names("persistentvolumeclaims"); strings.Join(names, ",") != "wc-orphan,wc-registered,wc-spare-adopted" {
		t.Fatalf("claims after reconcile: %v", names)
	}
}

// Exec runs in the requested directory as the guest account, returns
// stdout, maps a non-zero exit to an error and caps the output.
func TestExecDirectoryExitCodesAndLimit(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	api.guest.hook = func(pod string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		if len(command) > 0 && command[0] == "printf" {
			io.WriteString(stdout, strings.Join(command[1:], " "))
			io.WriteString(stderr, "noise the worker must never see")
			return 0, true
		}
		return 0, false
	}
	out, err := d.Exec(ctx, runtimeName, "/home/agent/workspace", "printf", "hello", "world")
	if err != nil || out != "hello world" {
		t.Fatalf("%q %v", out, err)
	}
	calls := api.guest.recorded()
	last := calls[len(calls)-1]
	if last.Dir != "/home/agent/workspace" || strings.Join(last.Command, " ") != "printf hello world" {
		t.Fatalf("exec call %+v", last)
	}
	_, err = d.Exec(ctx, runtimeName, "/tmp", "sh", "-c", "exit 3")
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 3 || strings.Contains(err.Error(), "noise") {
		t.Fatalf("exit code: %v", err)
	}
	if _, err = d.run(ctx, runtimeName, nil, 1024, "yes"); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("limit: %v", err)
	}
	if _, err = d.Exec(ctx, "wc-nowhere", "/tmp", "true"); err == nil {
		t.Fatal("exec in a missing pod succeeded")
	}
	// Cancellation ends a session.
	cctx, cancel := context.WithCancel(ctx)
	api.guest.hook = func(pod string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		if len(command) > 0 && command[0] == "sleep" {
			time.Sleep(400 * time.Millisecond) // outlives the caller's context
			return 0, true
		}
		return 0, false
	}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err = d.Exec(cctx, runtimeName, "/tmp", "sleep", "infinity"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exec: %v", err)
	}
}

// Copy streams a host file or directory as a root-owned tar into the
// guest at the target path, the sbx cp contract.
func TestCopyIsTarOverExec(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "claude"), []byte("claude release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Copy(ctx, runtimeName, filepath.Join(host, "claude"), "/opt/warden/claude/claude"); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(host, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "bin", "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "codex-package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/codex", filepath.Join(bundle, "codex-link")); err != nil {
		t.Fatal(err)
	}
	if err := d.Copy(ctx, runtimeName, bundle, "/opt/warden/runtime-stage"); err != nil {
		t.Fatal(err)
	}
	api.guest.mu.Lock()
	received := api.guest.received[runtimeName]
	api.guest.mu.Unlock()
	got := map[string]tarEntry{}
	for _, e := range received {
		got[e.Name] = e
	}
	if e := got["/opt/warden/claude/claude"]; e.Content != "claude release" || e.UID != 0 || e.Mode&0o111 == 0 {
		t.Fatalf("file copy %+v", e)
	}
	if e := got["/opt/warden/runtime-stage/"]; e.Type != '5' {
		t.Fatalf("directory entry %+v (all %v)", e, received)
	}
	if e := got["/opt/warden/runtime-stage/bin/codex"]; e.Content != "#!/bin/sh\n" || e.Mode&0o111 == 0 {
		t.Fatalf("nested file %+v", e)
	}
	if e := got["/opt/warden/runtime-stage/codex-link"]; e.Type != '2' {
		t.Fatalf("symlink %+v", e)
	}
	for _, bad := range []string{"relative", "/", "/a/../b"} {
		if err := d.Copy(ctx, runtimeName, filepath.Join(host, "claude"), bad); err == nil {
			t.Fatalf("target %q accepted", bad)
		}
	}
	if err := d.Copy(ctx, runtimeName, filepath.Join(host, "missing"), "/tmp/x"); err == nil {
		t.Fatal("missing source accepted")
	}
}

// Stream launches the shared agent command in the run's directory with
// the tier's option, pipes both ways, and ends with EOF when the agent
// exits or the context is cancelled.
func TestStreamLaunchesTheAgentAndEndsWithEOF(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	run := sandbox.RunSpec{Directory: "/home/agent/workspace", Broker: sandbox.BrokerConfig{Provider: "codex", ProxyURL: "http://b:c@10.43.0.5:7000", APIKeyPlaceholder: "b.c", ProviderBaseURL: "http://10.43.0.5:7000/openai/v1", CACertificate: "unused"}, Paths: sandbox.GuestPaths{Codex: "/opt/warden/runtime", Claude: "/opt/warden/claude/claude"}}
	stream, err := d.Stream(ctx, runtimeName, run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(stream, "hello\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	var got string
	for !strings.Contains(got, "agent:hello\n") {
		n, err := stream.Read(buf)
		if err != nil {
			t.Fatalf("read: %v (%q)", err, got)
		}
		got += string(buf[:n])
	}
	calls := api.guest.recorded()
	launch := calls[len(calls)-1]
	joined := strings.Join(launch.Command, "\n")
	if launch.Dir != "/home/agent/workspace" || launch.Command[0] != "env" || !strings.Contains(joined, "\n/opt/warden/runtime/bin/codex\n") || !strings.Contains(joined, "\nHTTPS_PROXY=http://b:c@10.43.0.5:7000\n") || !strings.Contains(joined, "\nWARDEN_API_KEY=b.c\n") || !strings.HasSuffix(joined, "\n-c\nsandbox_mode=\"danger-full-access\"") {
		t.Fatalf("launch %+v", launch)
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c.Command, " "), "update-ca-certificates") {
			t.Fatal("the Kubernetes driver must not install a CA by exec")
		}
	}
	// Closing the stream ends the session; the reader sees EOF.
	stream.Close()
	if _, err = stream.Read(buf); err != io.EOF {
		t.Fatalf("after close: %v", err)
	}
	// The agent exiting (its input ends) ends the reader with EOF too.
	stream, err = d.Stream(ctx, runtimeName, run)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	claude := run
	claude.Broker.Provider = "claude"
	cstream, err := d.Stream(cctx, runtimeName, claude)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = io.ReadAll(cstream); err != nil {
		t.Fatalf("cancelled stream: %v", err)
	}
	cstream.Close()
	stream.Close()
	if _, err = d.Stream(ctx, "wc-nowhere", run); err == nil {
		t.Fatal("stream into a missing pod started")
	}
}

// Previews: Publish records the guest port at the pod IP after reserve,
// Mappings returns them while the pod is the one they were made on, and
// Stop forgets them.
func TestPublishRecordsPodIPMappingsCheckThePod(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	if _, err := d.Mappings(ctx, runtimeName); err == nil {
		t.Fatal("mappings of a guest that is not running")
	}
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	rt, _ := d.Runtime(runtimeName)
	var reserved sandbox.PortMapping
	m, err := d.Publish(ctx, runtimeName, 3000, func(m sandbox.PortMapping) error { reserved = m; return nil })
	if err != nil || m != reserved || m.Address != rt.PodIP || m.Port != 3000 || m.GuestPort != 3000 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err = d.Publish(ctx, runtimeName, 3001, func(sandbox.PortMapping) error { return errors.New("no room") }); err == nil {
		t.Fatal("a refused reservation was published")
	}
	if got, err := d.Mappings(ctx, runtimeName); err != nil || len(got) != 1 || got[0] != m {
		t.Fatalf("%v %v", got, err)
	}
	if _, err = d.Publish(ctx, runtimeName, 0, nil); err == nil {
		t.Fatal("port 0 accepted")
	}
	if err = d.Unpublish(ctx, runtimeName, m); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Mappings(ctx, runtimeName); err != nil || len(got) != 0 {
		t.Fatalf("after unpublish %v %v", got, err)
	}
	if _, err = d.Publish(ctx, runtimeName, 3000, nil); err != nil {
		t.Fatal(err)
	}
	// The pod replaced behind the driver's back: the publications are not
	// this pod's.
	api.mu.Lock()
	api.objects["pods/"+runtimeName]["metadata"].(map[string]any)["uid"] = "someone-else"
	api.mu.Unlock()
	if _, err = d.Mappings(ctx, runtimeName); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replaced pod: %v", err)
	}
	api.mu.Lock()
	api.objects["pods/"+runtimeName]["metadata"].(map[string]any)["uid"] = rt.PodUID
	api.mu.Unlock()
	if err = d.Stop(ctx, runtimeName); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Mappings(ctx, runtimeName); err == nil {
		t.Fatal("mappings of a stopped guest")
	}
	if _, err = d.Publish(ctx, runtimeName, 3000, nil); err == nil {
		t.Fatal("publish on a stopped guest")
	}
}

// A fork clones the source claim; when the storage refuses the clone or
// yields an empty volume, the home is copied through exec from the source
// pod, and the driver records which it used.
func TestForkClonesOrCopiesTheHome(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	source := sandbox.RuntimeSpec{Name: "wc-source", Directory: "/home/agent/workspace", SandboxID: "s1"}
	if err := d.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	api.guest.mu.Lock()
	api.guest.homes["wc-source"] = map[string]string{"workspace/README.md": "hello", ".bashrc": "x"}
	api.guest.mu.Unlock()
	// The storage clones: the workspace directory is there.
	api.guest.hook = func(pod string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		if len(command) == 3 && command[0] == "test" && pod == "wc-clone" {
			return 0, true
		}
		return 0, false
	}
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-clone", Directory: "/home/agent/workspace", SandboxID: "s2", Source: "wc-source"}); err != nil {
		t.Fatal(err)
	}
	claim, _ := api.claim("wc-clone")
	if claim.Spec.DataSource == nil || claim.Spec.DataSource.Name != "wc-source" {
		t.Fatalf("clone claim %+v", claim.Spec)
	}
	if rt, _ := d.Runtime("wc-clone"); rt.Workspace != WorkspaceClone {
		t.Fatalf("clone recorded as %q", rt.Workspace)
	}
	api.guest.mu.Lock()
	copied := api.guest.copied["wc-clone"]
	api.guest.mu.Unlock()
	if copied != nil {
		t.Fatal("a successful clone was also copied")
	}
	// The provisioner ignored the dataSource: an empty volume is filled by
	// the copy.
	api.guest.hook = nil
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-empty", Directory: "/home/agent/workspace", SandboxID: "s3", Source: "wc-source"}); err != nil {
		t.Fatal(err)
	}
	api.guest.mu.Lock()
	copied = api.guest.copied["wc-empty"]
	api.guest.mu.Unlock()
	if copied["workspace/README.md"] != "hello" || copied[".bashrc"] != "x" {
		t.Fatalf("copied home %v", copied)
	}
	if rt, _ := d.Runtime("wc-empty"); rt.Workspace != WorkspaceCopy {
		t.Fatalf("copy recorded as %q", rt.Workspace)
	}
	// The server refuses the dataSource: a plain claim, then the copy.
	api.refuseClone = true
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-refused", Directory: "/home/agent/workspace", SandboxID: "s4", Source: "wc-source"}); err != nil {
		t.Fatal(err)
	}
	claim, _ = api.claim("wc-refused")
	if claim.Spec.DataSource != nil {
		t.Fatal("refused clone claim kept its dataSource")
	}
	if rt, _ := d.Runtime("wc-refused"); rt.Workspace != WorkspaceCopy {
		t.Fatalf("refused clone recorded as %q", rt.Workspace)
	}
	// No running source pod: the copy cannot happen.
	if err := d.Stop(ctx, "wc-source"); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-late", Directory: "/home/agent/workspace", SandboxID: "s5", Source: "wc-source"}); err == nil || !strings.Contains(err.Error(), "source pod is not running") {
		t.Fatalf("fork of a stopped source: %v", err)
	}
}

// A pod is created at the spec's size; Resize patches pods/resize with a
// strategic patch naming only the guest container's resources, waits for
// the kubelet to report the container at the new size, and never
// replaces the pod. A stopped runtime has no pod and nothing to do; the
// same size again is a no-op.
func TestResizePatchesThePodInPlace(t *testing.T) {
	api, d := readyFake(t)
	ctx := testContext(t)
	spec := sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", SandboxID: "sandbox-one", Generation: "gen-1", Resources: sandbox.Resources{CPUMilli: 500, MemoryMB: 2048}}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pod, _ := api.pod(runtimeName)
	if got := pod.Spec.Containers[0].Resources.Limits; got["cpu"] != "500m" || got["memory"] != "2048Mi" {
		t.Fatalf("created at %v", got)
	}
	before, _ := d.Runtime(runtimeName)
	restarted, err := d.Resize(ctx, runtimeName, sandbox.Resources{CPUMilli: 1500, MemoryMB: 4096})
	if err != nil || restarted {
		t.Fatalf("resize: restarted=%v %v", restarted, err)
	}
	pod, _ = api.pod(runtimeName)
	if got := pod.Spec.Containers[0].Resources; got.Limits["cpu"] != "1500m" || got.Limits["memory"] != "4096Mi" || got.Requests["cpu"] != "1500m" || got.Requests["memory"] != "4096Mi" {
		t.Fatalf("pod after resize %+v", got)
	}
	if applied := pod.Status.ContainerStatuses[0].Resources; applied == nil || applied.Limits["cpu"] != "1500m" {
		t.Fatalf("status after resize %+v", applied)
	}
	if after, _ := d.Runtime(runtimeName); after.PodUID != before.PodUID {
		t.Fatal("the pod was replaced")
	}
	var patches []fakeRequest
	for _, r := range api.recorded() {
		if r.Method == "PATCH" {
			patches = append(patches, r)
		}
	}
	if len(patches) != 1 || !strings.HasSuffix(patches[0].Path, "/pods/"+runtimeName+"/resize") || !strings.Contains(string(patches[0].Body), `"name":"guest"`) || strings.Contains(string(patches[0].Body), `"image"`) {
		t.Fatalf("patches %+v", patches)
	}
	seen := len(api.recorded())
	if _, err = d.Resize(ctx, runtimeName, sandbox.Resources{CPUMilli: 1500, MemoryMB: 4096}); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded()[seen:] {
		if r.Method == "PATCH" {
			t.Fatal("the same size was patched again")
		}
	}
	if err = d.Stop(ctx, runtimeName); err != nil {
		t.Fatal(err)
	}
	if restarted, err = d.Resize(ctx, runtimeName, sandbox.Resources{CPUMilli: 250, MemoryMB: 1024}); err != nil || restarted {
		t.Fatalf("stopped resize: restarted=%v %v", restarted, err)
	}
	if _, err = d.Prepare(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace", Generation: "gen-2", Resources: sandbox.Resources{CPUMilli: 250, MemoryMB: 1024}}); err != nil {
		t.Fatal(err)
	}
	pod, _ = api.pod(runtimeName)
	if got := pod.Spec.Containers[0].Resources.Limits; got["cpu"] != "250m" || got["memory"] != "1024Mi" {
		t.Fatalf("next generation at %v", got)
	}
}

// A resize the node cannot fit, and a server without the subresource, are
// ErrResizeInfeasible: the worker replaces the pod at the size instead.
// A pod the runner may not resize (no Role verb) is the same answer.
func TestResizeReportsInfeasible(t *testing.T) {
	for _, mode := range []string{"infeasible", "absent", "forbidden"} {
		api, d := readyFake(t)
		ctx := testContext(t)
		if err := d.Create(ctx, sandbox.RuntimeSpec{Name: runtimeName, Directory: "/home/agent/workspace"}); err != nil {
			t.Fatal(err)
		}
		if mode == "forbidden" {
			api.mu.Lock()
			api.forbidden = map[string]bool{"resize pods": true}
			api.mu.Unlock()
		} else {
			api.mu.Lock()
			api.resizeMode = mode
			api.mu.Unlock()
		}
		restarted, err := d.Resize(ctx, runtimeName, sandbox.Resources{CPUMilli: 4000, MemoryMB: 8192})
		if !errors.Is(err, sandbox.ErrResizeInfeasible) || restarted {
			t.Fatalf("%s: restarted=%v %v", mode, restarted, err)
		}
		if _, ok := api.pod(runtimeName); !ok {
			t.Fatalf("%s: the driver removed the pod", mode)
		}
	}
}
