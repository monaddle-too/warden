package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/release"
	"warden/chat/internal/sandbox"
)

// The worker scenarios of sandbox/managed_test.go that do not depend on
// SBX, run through the worker's protocol against this driver and the fake
// API: create on prepare, the manifest-driven skip of the runtime copies,
// previews at the pod IP, stop keeping the workspace, resume as a new
// generation, remove, a warm spare adopted, and reconciliation at restart.

type workerGate struct {
	mu    sync.Mutex
	calls []string
}

func (g *workerGate) record(s string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, s)
	return nil
}
func (g *workerGate) Register(_ context.Context, c sandbox.GrantContext) error {
	return g.record("register:" + c.RuntimeName)
}
func (g *workerGate) Check(_ context.Context, _ sandbox.GrantContext, phase string) error {
	return g.record("check:" + phase)
}
func (g *workerGate) Begin(_ context.Context, _ sandbox.GrantContext) (sandbox.BrokerConfig, error) {
	return sandbox.BrokerConfig{APIKeyPlaceholder: "b.cap", ProviderBaseURL: "http://10.43.0.5:7000/openai/v1", ProxyURL: "http://b:cap@10.43.0.5:7000", CACertificate: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"}, g.record("begin")
}
func (g *workerGate) Renew(_ context.Context, _ sandbox.GrantContext) error { return g.record("renew") }
func (g *workerGate) End(_ context.Context, _ sandbox.GrantContext) error   { return g.record("end") }

type workerFixture struct {
	t      *testing.T
	api    *fakeAPI
	driver *Driver
	root   string
	bundle string
	gate   *workerGate
	socket string
	cancel context.CancelFunc
	done   chan struct{}
}

func hostTarget() string {
	if goruntime.GOARCH == "amd64" {
		return "x86_64-unknown-linux-musl"
	}
	return "aarch64-unknown-linux-musl"
}

// newWorkerFixture starts a worker on a Unix socket with the driver over
// the fake; the host bundle matches the image's manifest, so nothing is
// copied into guests.
func newWorkerFixture(t *testing.T, api *fakeAPI, root string, spares int) *workerFixture {
	t.Helper()
	api.guest.mu.Lock()
	api.guest.manifest = strings.Replace(guestManifestJSON, "aarch64-unknown-linux-musl", hostTarget(), 1)
	api.guest.mu.Unlock()
	bundle := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "codex-package.json"), []byte(`{"layoutVersion":1,"version":"`+release.CodexVersion+`","target":"`+hostTarget()+`","entrypoint":"bin/codex","resourcesDir":"codex-resources","pathDir":"codex-path"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &workerFixture{t: t, api: api, root: root, bundle: bundle, gate: &workerGate{}}
	f.driver = newTestDriver(t, api, testOptions())
	f.start(spares)
	return f
}

func (f *workerFixture) start(spares int) {
	f.t.Helper()
	w := sandbox.NewWorker(f.root, "/never-host-exec", "template")
	w.Runtime = f.driver
	w.Gate = f.gate
	w.RuntimeDir = f.bundle
	w.Spares = spares
	w.IdleTimeout = time.Hour
	dir, err := os.MkdirTemp("/tmp", "wk8s")
	if err != nil {
		f.t.Fatal(err)
	}
	f.socket = filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", f.socket)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		if err := w.Serve(ctx, l); err != nil {
			f.t.Errorf("serve: %v", err)
		}
	}()
	f.t.Cleanup(func() { f.stop(); os.RemoveAll(dir) })
	// The worker is ready once it answers health.
	f.call(sandbox.Request{Operation: "health"})
}

func (f *workerFixture) stop() {
	if f.cancel == nil {
		return
	}
	f.cancel()
	f.cancel = nil
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		f.t.Fatal("worker did not stop")
	}
}

// restart stops the worker and starts a new one on the same root with a
// fresh driver, as a runner restart does.
func (f *workerFixture) restart(spares int) {
	f.t.Helper()
	f.stop()
	f.driver = newTestDriver(f.t, f.api, testOptions())
	f.start(spares)
}

func (f *workerFixture) dial() net.Conn {
	f.t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = net.Dial("unix", f.socket); err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal(err)
	return nil
}

// call sends one request and returns the answer, failing on a protocol
// error; try returns the error string instead.
func (f *workerFixture) call(r sandbox.Request) sandbox.Response {
	f.t.Helper()
	res, errText := f.try(r)
	if errText != "" {
		f.t.Fatalf("%s: %s", r.Operation, errText)
	}
	return res
}

func (f *workerFixture) try(r sandbox.Request) (sandbox.Response, string) {
	f.t.Helper()
	conn := f.dial()
	defer conn.Close()
	r.Version = sandbox.ProtocolVersion
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		f.t.Fatal(err)
	}
	var res sandbox.Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&res); err != nil {
		f.t.Fatalf("%s: %v", r.Operation, err)
	}
	return res, res.Error
}

// stream opens the agent stream and returns the connection once the
// worker admitted it.
func (f *workerFixture) stream(r sandbox.Request) net.Conn {
	f.t.Helper()
	conn := f.dial()
	r.Version = sandbox.ProtocolVersion
	r.Operation = "stream"
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		f.t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		f.t.Fatal(err)
	}
	var res sandbox.Response
	if err := json.Unmarshal(line, &res); err != nil || res.Error != "" {
		f.t.Fatalf("stream: %s %v", res.Error, err)
	}
	f.t.Cleanup(func() { conn.Close() })
	return conn
}

func baseRequest() sandbox.Request {
	return sandbox.Request{ProjectID: "project-one", ChatID: "chat-one", SandboxID: "sandbox-one", RunID: "run-one", PrincipalID: "owner", Provider: "codex"}
}

func TestWorkerLifecycleOverTheKubernetesDriver(t *testing.T) {
	api := newFakeAPI(t)
	api.publishTrust("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	f := newWorkerFixture(t, api, t.TempDir(), 0)
	r := baseRequest()
	r.Operation = "bind-chat"
	bound := f.call(r)
	name := bound.Sandbox.RuntimeName
	if !strings.HasPrefix(name, "wc-") || bound.Sandbox.State != "stopped" {
		t.Fatalf("bound %+v", bound.Sandbox)
	}
	if names := api.names("pods"); len(names) != 0 {
		t.Fatal("binding created a pod")
	}
	// Prepare: the claim and the pod appear, the report comes through exec,
	// and the matching manifest means no runtime copy.
	r.Operation = "prepare"
	prepared := f.call(r)
	if prepared.Sandbox.State != "running" {
		t.Fatalf("prepared %+v", prepared.Sandbox)
	}
	pod, ok := api.pod(name)
	if !ok || pod.Status.Phase != "Running" || pod.Metadata.Annotations[AnnotationSandboxID] != "sandbox-one" || pod.Metadata.Annotations[AnnotationGeneration] != prepared.Sandbox.Generation {
		t.Fatalf("pod %+v", pod.Metadata)
	}
	claim, ok := api.claim(name)
	if !ok || claim.Metadata.Labels[LabelSandbox] != name {
		t.Fatalf("claim %+v", claim.Metadata)
	}
	for _, c := range api.guest.recorded() {
		joined := strings.Join(c.Command, " ")
		if strings.Contains(joined, "tar -xf") || strings.Contains(joined, "runtime-stage") {
			t.Fatalf("a matching image still received a copy: %s", joined)
		}
	}
	// The agent stream carries the launch line to the pod and echoes.
	conn := f.stream(r)
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	var lines []string
	for len(lines) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v %v", err, lines)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	if lines[0] != "agent-ready" || lines[1] != "agent:ping" {
		t.Fatalf("agent lines %v", lines)
	}
	calls := api.guest.recorded()
	launch := calls[len(calls)-1]
	if launch.Pod != name || launch.Dir != "/home/agent/workspace" || !strings.Contains(strings.Join(launch.Command, "\n"), "\n/opt/warden/runtime/bin/codex\n") || !strings.Contains(strings.Join(launch.Command, "\n"), "\nHTTPS_PROXY=http://b:cap@10.43.0.5:7000\n") {
		t.Fatalf("launch %+v", launch)
	}
	// A preview: the mapping is the pod IP and the guest port; the
	// attachment's URL is the runner's loopback proxy.
	r.Operation = "preview.attach"
	r.Port = 3000
	r.Path = "/"
	r.Title = "Counter"
	r.CallID = "call-one"
	attached := f.call(r)
	if attached.Attachment == nil || attached.Attachment.State != "available" || !strings.HasPrefix(attached.Attachment.URL, "http://127.0.0.1:") {
		t.Fatalf("attachment %+v", attached.Attachment)
	}
	mappings, err := f.driver.Mappings(context.Background(), name)
	if err != nil || len(mappings) != 1 || mappings[0].Address != pod.Status.PodIP || mappings[0].Port != 3000 || mappings[0].GuestPort != 3000 {
		t.Fatalf("mappings %v %v", mappings, err)
	}
	conn.Close()
	r.Operation = "status"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res := f.call(r); res.Sandbox.State == "running" {
			if _, errText := f.try(sandbox.Request{ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, PrincipalID: r.PrincipalID, Operation: "stop"}); errText == "" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Stop: the pod is gone, the claim stays, the state is stopped.
	if res := f.call(sandbox.Request{ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, PrincipalID: r.PrincipalID, Operation: "status"}); res.Sandbox.State != "stopped" {
		t.Fatalf("after stop %+v", res.Sandbox)
	}
	if _, ok := api.pod(name); ok {
		t.Fatal("pod still present after stop")
	}
	if _, ok := api.claim(name); !ok {
		t.Fatal("claim deleted by stop")
	}
	// Resume: a new pod on the same claim and a new generation.
	r = baseRequest()
	r.RunID = "run-two"
	r.Operation = "prepare"
	resumed := f.call(r)
	pod2, ok := api.pod(name)
	if !ok || resumed.Sandbox.Generation == prepared.Sandbox.Generation || pod2.Metadata.UID == pod.Metadata.UID || pod2.Metadata.Annotations[AnnotationGeneration] != resumed.Sandbox.Generation {
		t.Fatalf("resume %+v %+v", resumed.Sandbox, pod2.Metadata)
	}
	claim2, _ := api.claim(name)
	if claim2.Metadata.UID != claim.Metadata.UID {
		t.Fatal("resume changed the claim")
	}
	// Remove: everything is deleted.
	f.call(sandbox.Request{ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, PrincipalID: r.PrincipalID, RunID: r.RunID, Operation: "cancel"})
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, errText := f.try(sandbox.Request{ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, PrincipalID: r.PrincipalID, Operation: "remove"}); errText == "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := api.claim(name); ok {
		t.Fatal("claim kept after remove")
	}
	if _, ok := api.pod(name); ok {
		t.Fatal("pod kept after remove")
	}
}

func TestWorkerSparesAndRestartOverTheKubernetesDriver(t *testing.T) {
	api := newFakeAPI(t)
	api.publishTrust("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	f := newWorkerFixture(t, api, t.TempDir(), 1)
	// The lifecycle loop boots one spare with the spare label.
	var spare string
	deadline := time.Now().Add(10 * time.Second)
	for spare == "" && time.Now().Before(deadline) {
		for _, name := range api.names("pods") {
			if pod, ok := api.pod(name); ok && strings.HasPrefix(name, "wc-spare-") && pod.Status.Phase == "Running" {
				if pod.Metadata.Labels[LabelSpare] != "true" || pod.Metadata.Annotations[AnnotationSandboxID] != "" {
					t.Fatalf("spare pod %+v", pod.Metadata)
				}
				spare = name
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if spare == "" {
		t.Fatal("no spare booted")
	}
	// Wait for the spare's report to be taken so adoption is possible.
	time.Sleep(200 * time.Millisecond)
	r := baseRequest()
	r.Operation = "bind-chat"
	f.call(r)
	r.Operation = "prepare"
	prepared := f.call(r)
	if prepared.Sandbox.RuntimeName != spare {
		t.Fatalf("spare %s not adopted: runtime %s", spare, prepared.Sandbox.RuntimeName)
	}
	// Restart: the worker stops what it knows, removes its registered spare,
	// and the driver's reconciliation deletes any pod left; the adopted
	// spare's claim is registered and kept.
	var second string
	deadline = time.Now().Add(10 * time.Second)
	for second == "" && time.Now().Before(deadline) {
		for _, name := range api.names("pods") {
			if name != spare && strings.HasPrefix(name, "wc-spare-") {
				if pod, ok := api.pod(name); ok && pod.Status.Phase == "Running" {
					second = name
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second == "" {
		t.Fatal("pool not refilled")
	}
	// An orphan pod from an earlier runner, unknown to the registry.
	if err := f.driver.Create(context.Background(), sandbox.RuntimeSpec{Name: "wc-orphan", Directory: "/home/agent/workspace"}); err != nil {
		t.Fatal(err)
	}
	f.restart(0)
	time.Sleep(5 * api.deleteDelay)
	if names := api.names("pods"); len(names) != 0 {
		t.Fatalf("pods after restart: %v", names)
	}
	claims := api.names("persistentvolumeclaims")
	if strings.Join(claims, ",") != "wc-orphan,"+spare {
		t.Fatalf("claims after restart: %v", claims)
	}
	if res := f.call(sandbox.Request{ProjectID: r.ProjectID, ChatID: r.ChatID, SandboxID: r.SandboxID, PrincipalID: r.PrincipalID, Operation: "status"}); res.Sandbox.State != "stopped" || res.Sandbox.RuntimeName != spare {
		t.Fatalf("after restart %+v", res.Sandbox)
	}
	// The sandbox resumes on its kept workspace.
	r.RunID = "run-two"
	if resumed := f.call(r); resumed.Sandbox.State != "running" || resumed.Sandbox.RuntimeName != spare {
		t.Fatalf("resume after restart %+v", resumed.Sandbox)
	}
	if _, ok := api.pod(spare); !ok {
		t.Fatal("resumed pod missing")
	}
}
