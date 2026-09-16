package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type testGate struct {
	mu      sync.Mutex
	calls   []string
	deny    bool
	endHook func(context.Context, GrantContext) error
}

func (g *testGate) record(s string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, s)
	if g.deny {
		return errors.New("enforcement unavailable")
	}
	return nil
}
func (g *testGate) Register(_ context.Context, c GrantContext) error {
	return g.record("register:" + c.SandboxID)
}
func (g *testGate) Check(_ context.Context, _ GrantContext, phase string) error {
	return g.record("check:" + phase)
}
func (g *testGate) Begin(_ context.Context, _ GrantContext) (BrokerConfig, error) {
	return BrokerConfig{APIKeyPlaceholder: "warden-proxy-managed", ProviderBaseURL: "http://gateway/openai/v1", ProxyURL: "http://gateway"}, g.record("begin")
}
func (g *testGate) Renew(_ context.Context, _ GrantContext) error { return g.record("renew") }
func (g *testGate) End(ctx context.Context, c GrantContext) error {
	if g.endHook != nil {
		return g.endHook(ctx, c)
	}
	return g.record("end")
}

type testRuntime struct {
	mu            sync.Mutex
	calls         []string
	servers       map[int]*http.Server
	createStarted chan struct{}
	createBlock   bool
	headers       http.Header
	stopHook      func(context.Context, string) error
	execHook      func([]string) error
	execOutput    string
}

func (d *testRuntime) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, s)
}
func (d *testRuntime) Create(ctx context.Context, s RuntimeSpec) error {
	d.record("create:" + s.Name)
	if d.createStarted != nil {
		close(d.createStarted)
	}
	if d.createBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (d *testRuntime) Exec(_ context.Context, name, dir string, args ...string) (string, error) {
	d.record("exec:" + name + ":" + strings.Join(args, " "))
	out := d.execOutput
	if out == "" {
		// A plain guest that kept whatever the runner installed earlier.
		out = "ca-present\ncodex-present\nclaude-present\n"
	}
	if d.execHook != nil {
		return out, d.execHook(args)
	}
	return out, nil
}
func (d *testRuntime) Copy(_ context.Context, name, source, target string) error {
	d.record("copy:" + name + ":" + target)
	return nil
}
func (d *testRuntime) InstallCA(_ context.Context, name, _ string) error {
	d.record("ca:" + name)
	return nil
}
func (d *testRuntime) Stream(ctx context.Context, _, _ string, _ BrokerConfig) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	go func() { <-ctx.Done(); b.Close() }()
	return a, nil
}
func (d *testRuntime) Stop(ctx context.Context, name string) error {
	d.record("stop:" + name)
	if d.stopHook != nil {
		return d.stopHook(ctx, name)
	}
	return nil
}
func (d *testRuntime) Remove(_ context.Context, name string) error {
	d.record("remove:" + name)
	return nil
}
func (d *testRuntime) Publish(_ context.Context, _ string, port, host int) error {
	d.record("publish")
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmtInt(host)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.headers = r.Header.Clone()
		d.mu.Unlock()
		w.Header().Set("Set-Cookie", "bad=secret")
		_, _ = io.WriteString(w, "counter")
	})}
	d.mu.Lock()
	d.servers[host] = server
	d.mu.Unlock()
	go server.Serve(l)
	return nil
}
func (d *testRuntime) Unpublish(_ context.Context, _ string, port, host int) error {
	d.record("unpublish")
	d.mu.Lock()
	server := d.servers[host]
	delete(d.servers, host)
	d.mu.Unlock()
	if server != nil {
		server.Close()
	}
	return nil
}
func (d *testRuntime) Mappings(_ context.Context, name string) ([]PortMapping, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "mappings:"+name)
	out := []PortMapping{}
	for host := range d.servers {
		out = append(out, PortMapping{"127.0.0.1", host, 3000, "tcp4"})
	}
	return out, nil
}
func fmtInt(n int) string { b, _ := json.Marshal(n); return string(b) }
func managedFixture(t *testing.T) (*Worker, *testRuntime, *testGate, Request) {
	t.Helper()
	d := &testRuntime{servers: map[int]*http.Server{}}
	g := &testGate{}
	w := NewWorker(t.TempDir(), "/never-host-exec", "template")
	w.Runtime = d
	w.RuntimeDir = "/fake-pinned-linux-bundle"
	w.Gate = g
	r := Request{Version: 2, Operation: "bind-chat", ProjectID: "project-one", ChatID: "chat-one", SandboxID: "sandbox-one", RunID: "run-one", PrincipalID: "owner"}
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.mu.Lock()
		for _, p := range w.managed.Publications {
			if p.server != nil {
				p.server.Close()
			}
		}
		w.mu.Unlock()
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, s := range d.servers {
			s.Close()
		}
	})
	return w, d, g, r
}
func prepareFixture(t *testing.T, w *Worker, r Request) {
	t.Helper()
	r.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func attachFixture(t *testing.T, w *Worker, r Request) PreviewAttachment {
	t.Helper()
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active.Streaming = true
	w.mu.Unlock()
	r.Operation = "preview.attach"
	r.Port = 3000
	r.Path = "/"
	r.Title = "Counter"
	r.CallID = "call-one"
	res, err := w.dispatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return *res.Attachment
}

func TestManagedOwnershipPrecedesEveryRuntimeEffect(t *testing.T) {
	w, d, g, r := managedFixture(t)
	for _, op := range []string{"prepare", "stream", "status", "activity", "stop", "preview.attach", "preview.remove"} {
		bad := r
		bad.Operation = op
		bad.ProjectID = "another-project"
		if _, err := w.dispatch(context.Background(), bad); err == nil {
			t.Fatalf("accepted cross-project %s", op)
		}
	}
	if len(d.calls) != 0 || len(g.calls) != 0 {
		t.Fatal("authorization rejection had lifecycle effects", d.calls, g.calls)
	}
	unknown := r
	unknown.ChatID = "unknown-chat"
	unknown.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), unknown); err == nil {
		t.Fatal("implicit registration")
	}
}
func TestManagedRequiresVerifiedEnforcement(t *testing.T) {
	w, d, g, r := managedFixture(t)
	g.deny = true
	r.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), r); err == nil {
		t.Fatal("unverified enforcement accepted")
	}
	if len(d.calls) != 0 {
		t.Fatal("runtime touched before readiness")
	}
	w.Gate = nil
	if _, err := w.dispatch(context.Background(), r); err == nil {
		t.Fatal("missing Warden accepted")
	}
}
func TestManagedEmptyWorkspaceSharedChatsAndAdmission(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	other := r
	other.ChatID = "chat-two"
	other.Operation = "bind-chat"
	if _, err := w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	other.RunID = "run-two"
	other.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), other); err == nil {
		t.Fatal("shared streams not serialized")
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	if _, err := w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, call := range d.calls {
		if strings.HasPrefix(call, "create:") {
			creates++
		}
		if strings.Contains(call, "git") {
			t.Fatal("repository-free chat used git")
		}
	}
	if creates != 1 {
		t.Fatal("shared sandbox was recreated", d.calls)
	}
}
func TestManagedRemoveDeletesSandboxAndChatBindings(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	other := r
	other.ChatID = "chat-two"
	other.Operation = "bind-chat"
	if _, err := w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	r.Operation = "remove"
	if _, err := w.dispatch(context.Background(), r); err == nil {
		t.Fatal("removal proceeded under an active run")
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox == nil || res.Sandbox.State != "removed" {
		t.Fatal(res, err)
	}
	stopped, removed := false, false
	for _, call := range d.calls {
		if strings.HasPrefix(call, "stop:wc-") {
			stopped = true
		}
		if strings.HasPrefix(call, "remove:wc-") && stopped {
			removed = true
		}
	}
	if !removed {
		t.Fatal("runtime was not stopped then removed", d.calls)
	}
	w.mu.Lock()
	_, sandboxKept := w.managed.Sandboxes[r.SandboxID]
	_, chatKept := w.managed.Chats["chat-two"]
	w.mu.Unlock()
	if sandboxKept || chatKept {
		t.Fatal("removed sandbox or its chats remained registered")
	}
	other.Operation = "status"
	if _, err = w.dispatch(context.Background(), other); err == nil {
		t.Fatal("sibling chat still bound after removal")
	}
	// The same chat may start over on a fresh environment.
	fresh := r
	fresh.SandboxID = "sandbox-two"
	fresh.Operation = "bind-chat"
	if _, err = w.dispatch(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
}
func TestManagedCancellationBeforeAdmissionAndDuringCreation(t *testing.T) {
	w, d, _, r := managedFixture(t)
	cancel := r
	cancel.Operation = "cancel"
	if _, err := w.dispatch(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	r.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), r); err == nil {
		t.Fatal("cancelled queued run started")
	}
	if len(d.calls) != 0 {
		t.Fatal("cancelled admission touched SBX")
	}
	r.RunID = "run-two"
	d.createStarted = make(chan struct{})
	d.createBlock = true
	done := make(chan error, 1)
	go func() { _, err := w.dispatch(context.Background(), r); done <- err }()
	<-d.createStarted
	cancel.RunID = r.RunID
	if _, err := w.dispatch(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled creation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited behind lifecycle lock")
	}
}
func TestPreviewIdempotencyReferenceCountAndStableAvailability(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	again := attachFixture(t, w, r)
	if a.ID != again.ID || a.URL != again.URL {
		t.Fatal("retry changed attachment")
	}
	req, _ := http.NewRequest("GET", a.URL, nil)
	req.Header.Set("Authorization", "Bearer controller-secret")
	req.Header.Set("Cookie", "controller=secret")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "counter" || response.Header.Get("Set-Cookie") != "" {
		t.Fatal("proxy failed or leaked cookie")
	}
	d.mu.Lock()
	if d.headers.Get("Authorization") != "" || d.headers.Get("Cookie") != "" {
		t.Fatal("proxy forwarded credentials")
	}
	d.mu.Unlock()
	other := r
	other.ChatID = "chat-two"
	other.RunID = "run-two"
	other.Operation = "bind-chat"
	if _, err = w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	prepareFixture(t, w, other)
	b := attachFixture(t, w, other)
	r.Operation = "preview.remove"
	r.AttachmentID = a.ID
	if _, err = w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	response, err = http.Get(b.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("removing one reference dropped publication")
	}
	other.Operation = "preview.remove"
	other.AttachmentID = b.ID
	if _, err = w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	response, err = http.Get(b.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 410 {
		t.Fatal("removed stable endpoint did not fail closed", response.StatusCode)
	}
	publishes := 0
	for _, call := range d.calls {
		if call == "publish" {
			publishes++
		}
	}
	if publishes != 1 {
		t.Fatal("duplicate publication", d.calls)
	}
}
func TestPreviewRejectsUnsafePathsWithoutPublishing(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active.Streaming = true
	w.mu.Unlock()
	r.Operation = "preview.attach"
	r.Port = 3000
	r.Title = "Preview"
	r.CallID = "call-one"
	for _, path := range []string{"//evil.test", "/%2fexample.test", "/../other", "/a%0d%0aHost:x", "https://evil.test", "/\\evil", "/%ZZ", "/ok?q=1"} {
		r.Path = path
		if _, err := w.dispatch(context.Background(), r); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	for _, call := range d.calls {
		if call == "publish" {
			t.Fatal("invalid attachment published")
		}
	}
}
func TestManagedIdleLeaseAndStoppedPreview(t *testing.T) {
	w, d, _, r := managedFixture(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	w.Now = func() time.Time { return now }
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	now = now.Add(14 * time.Minute)
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active.Expires = now.Add(time.Minute)
	w.mu.Unlock()
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	r.Operation = "activity"
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(14 * time.Minute)
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range d.calls {
		if strings.HasPrefix(call, "stop:") {
			t.Fatal("stopped before trusted activity lease expired")
		}
	}
	now = now.Add(2 * time.Minute)
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	state := w.managed.Sandboxes[r.SandboxID].State
	w.mu.Unlock()
	if state != "running" {
		t.Fatal("published preview was stopped by idle sweep", state)
	}
	r.Operation = "stop"
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("stopped URL remained available")
	}
	r.Operation = "status"
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.State != "stopped" || res.Attachments[0].URL != "" {
		t.Fatal(res, err)
	}
}
func TestWorkerProtocolRejectsLegacyBeforeEffects(t *testing.T) {
	w, d, _, r := managedFixture(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.handle(ctx, a)
	r.Version = 1
	r.Operation = "prepare"
	if err := json.NewEncoder(b).Encode(r); err != nil {
		t.Fatal(err)
	}
	var res Response
	if err := json.NewDecoder(b).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Error == "" || res.Version != 2 || len(d.calls) != 0 {
		t.Fatal(res, d.calls)
	}
}
func TestRunnerRootLockPreventsSecondOwner(t *testing.T) {
	root := t.TempDir()
	unlock, err := LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if other, err := LockRoot(root); err == nil {
		other()
		t.Fatal("second runner acquired root")
	}
}
func TestUnixEnforcementRejectsRegistrationAsReadiness(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "w.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for i := 0; i < 2; i++ {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadBytes('\n')
			_, _ = io.WriteString(c, `{"version":1,"ok":true,"ready":false,"reason":"enforcement_unavailable"}`+"\n")
			c.Close()
		}
	}()
	gate := &UnixEnforcement{Socket: socket}
	if err = gate.Register(context.Background(), GrantContext{}); err != nil {
		t.Fatal(err)
	}
	if err = gate.Check(context.Background(), GrantContext{}, "runtime"); err == nil {
		t.Fatal("registration confused with verified readiness")
	}
}

func TestParseInstalledSBXPortInventory(t *testing.T) {
	good := `[{"host_ip":"127.0.0.1","host_port":49484,"sandbox_port":8080,"protocol":"tcp4"}]`
	mappings, err := parsePortMappings([]byte(good))
	if err != nil || len(mappings) != 1 || mappings[0].HostPort != 49484 {
		t.Fatal(mappings, err)
	}
	if empty, err := parsePortMappings([]byte(`[]`)); err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	for _, bad := range []string{`null`, `{"ports":[]}`, `[] []`, strings.Replace(good, "127.0.0.1", "0.0.0.0", 1), strings.Replace(good, "tcp4", "tcp6", 1), strings.Replace(good, "49484", "0", 1), strings.Replace(good, "8080", "65536", 1), strings.Replace(good, "}]", "},{\"host_ip\":\"127.0.0.1\",\"host_port\":49484,\"sandbox_port\":8081,\"protocol\":\"tcp4\"}]", 1)} {
		if _, err := parsePortMappings([]byte(bad)); err == nil {
			t.Fatalf("accepted unsafe inventory %s", bad)
		}
	}
}

func openManagedTestStream(t *testing.T, w *Worker, r Request) (net.Conn, <-chan struct{}) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); defer a.Close(); w.handle(context.Background(), a) }()
	r.Operation = "stream"
	if err := json.NewEncoder(b).Encode(r); err != nil {
		t.Fatal(err)
	}
	var res Response
	if err := json.NewDecoder(b).Decode(&res); err != nil || res.Error != "" {
		t.Fatal(res, err)
	}
	t.Cleanup(func() { b.Close() })
	return b, done
}
func waitManagedStream(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker stream cleanup did not finish")
	}
}
func runtimeStops(d *testRuntime) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, call := range d.calls {
		if strings.HasPrefix(call, "stop:") {
			n++
		}
	}
	return n
}
func TestExplicitCancelStopsCurrentGuestAndInvalidatesPreview(t *testing.T) {
	w, d, g, r := managedFixture(t)
	prepareFixture(t, w, r)
	_, done := openManagedTestStream(t, w, r)
	a := attachFixture(t, w, r)
	ended := false
	g.endHook = func(context.Context, GrantContext) error { ended = true; return nil }
	d.stopHook = func(ctx context.Context, _ string) error {
		if !ended {
			t.Error("guest stopped before broker revocation")
		}
		return ctx.Err()
	}
	cancel := r
	cancel.Operation = "cancel"
	if _, err := w.dispatch(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	waitManagedStream(t, done)
	if runtimeStops(d) != 1 {
		t.Fatal("explicit current cancellation must stop guest exactly once", d.calls)
	}
	r.Operation = "status"
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.State != "stopped" || res.Attachments[0].State != "stopped" || res.Attachments[0].URL != "" {
		t.Fatal(res, err)
	}
	response, err := http.Get(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("cancelled preview remained live")
	}
}
func TestNormalStreamCompletionKeepsPreview(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	client, done := openManagedTestStream(t, w, r)
	a := attachFixture(t, w, r)
	client.Close()
	waitManagedStream(t, done)
	if runtimeStops(d) != 0 {
		t.Fatal("normal completion stopped the preview sandbox")
	}
	response, err := http.Get(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("normal completion lost preview", response.StatusCode)
	}
}
func TestStaleCancellationCannotStopNewerRun(t *testing.T) {
	w, d, _, r := managedFixture(t)
	r.RunID = "new-run"
	prepareFixture(t, w, r)
	client, done := openManagedTestStream(t, w, r)
	stale := r
	stale.RunID = "old-run"
	stale.Operation = "cancel"
	if _, err := w.dispatch(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	// A normal close of the newer run must remain normal despite the old tombstone.
	client.Close()
	waitManagedStream(t, done)
	if runtimeStops(d) != 0 {
		t.Fatal("stale cancellation stopped newer run")
	}
	w.cancelPreparedReservation(stale)
	if runtimeStops(d) != 0 {
		t.Fatal("stale/idle cancellation stopped sandbox")
	}
}
func TestCancellationBetweenPrepareAndStreamStopsReservation(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	cancel := r
	cancel.Operation = "cancel"
	if _, err := w.dispatch(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	// Synchronize with the same idempotent cleanup path used by the control lane.
	w.cancelPreparedReservation(cancel)
	if runtimeStops(d) != 1 {
		t.Fatal("prepared cancellation did not stop exactly its sandbox", d.calls)
	}
	r.Operation = "status"
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.State != "stopped" {
		t.Fatal(res, err)
	}
}
func TestFailedBrokerEndGetsFreshStopDeadline(t *testing.T) {
	w, d, g, r := managedFixture(t)
	prepareFixture(t, w, r)
	client, done := openManagedTestStream(t, w, r)
	var endedContext context.Context
	g.endHook = func(ctx context.Context, _ GrantContext) error { endedContext = ctx; return context.DeadlineExceeded }
	d.stopHook = func(ctx context.Context, _ string) error {
		if endedContext == nil || endedContext == ctx || ctx.Err() != nil {
			t.Error("VM stop reused failed broker context")
		}
		deadline, _ := ctx.Deadline()
		prior, _ := endedContext.Deadline()
		if !deadline.After(prior) {
			t.Error("VM stop did not get an independent timeout")
		}
		return nil
	}
	client.Close()
	waitManagedStream(t, done)
	if runtimeStops(d) != 1 {
		t.Fatal("failed broker end did not stop guest")
	}
}

func TestRevokedMappingIdentitySurvivesRuntimeRestoration(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	p := w.managed.Publications[pubKey(r.SandboxID, 3000)]
	host := p.HostPort
	r.Operation = "preview.remove"
	r.AttachmentID = a.ID
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(w.Root, "managed-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved managedState
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Publications[pubKey(r.SandboxID, 3000)]; got.State != "removed" || got.HostPort != host {
		t.Fatal("revoked mapping identity forgotten", got)
	}
	// SBX can restore its saved loopback publication when the VM starts again.
	if err = d.Publish(context.Background(), "", 3000, host); err != nil {
		t.Fatal(err)
	}
	if err = w.reconcileRemovedLocked(context.Background(), w.managed.Sandboxes[r.SandboxID]); err != nil {
		t.Fatal(err)
	}
	mappings, err := d.Mappings(context.Background(), "")
	if err != nil || len(mappings) != 0 {
		t.Fatal("restored revoked mapping remains", mappings, err)
	}
	if p.HostPort != host || p.State != "removed" {
		t.Fatal("reconciliation discarded revocation identity")
	}
}

func TestUnpublishedPreviewAllowsIdleStop(t *testing.T) {
	w, _, _, r := managedFixture(t)
	now := time.Now()
	w.Now = func() time.Time { return now }
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	r.Operation = "preview.remove"
	r.AttachmentID = a.ID
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if state := w.managed.Sandboxes[r.SandboxID].State; state != "stopped" {
		t.Fatal("unpublished preview retained idle sandbox", state)
	}
}

type testResidency struct{ runtime *testRuntime }

func (p *testResidency) Close() error { p.runtime.record("release-residency"); return nil }
func (d *testRuntime) KeepAlive(name string) (io.Closer, error) {
	d.record("hold-residency:" + name)
	return &testResidency{runtime: d}, nil
}
func TestResidentSessionSurvivesAgentFinishAndReleasesOnStop(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	grant := s.Grant
	held := s.residency != nil
	w.mu.Unlock()
	if !held {
		t.Fatal("no independent residency session")
	}
	w.finishManagedRun(r, grant, false)
	w.mu.Lock()
	held = s.residency != nil && s.Active == nil
	w.mu.Unlock()
	if !held {
		t.Fatal("agent completion released residency")
	}
	r.Operation = "stop"
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	released := false
	for _, call := range d.calls {
		if call == "release-residency" {
			released = true
		}
		if strings.HasPrefix(call, "stop:") && !released {
			t.Fatal("stop did not release session first")
		}
	}
	if !released {
		t.Fatal("residency session leaked")
	}
}

func TestPreviewAuditsRunOutsideRequestsAndExpire(t *testing.T) {
	w, _, g, r := managedFixture(t)
	now := time.Now()
	w.Now = func() time.Time { return now }
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	get := func(want int) {
		t.Helper()
		res, err := http.Get(a.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("status %d, want %d", res.StatusCode, want)
		}
	}
	g.mu.Lock()
	before := len(g.calls)
	g.mu.Unlock()
	get(200)
	w.auditPreviews(context.Background())
	g.mu.Lock()
	after := len(g.calls)
	g.mu.Unlock()
	if before != after {
		t.Fatal("request or early sweep performed audit")
	}
	now = now.Add(61 * time.Second)
	w.auditPreviews(context.Background())
	g.mu.Lock()
	after = len(g.calls)
	g.mu.Unlock()
	if after != before+1 {
		t.Fatal("minute audit did not run")
	}
	now = now.Add(91 * time.Second)
	get(503)
	w.auditPreviews(context.Background())
	get(200)
	g.mu.Lock()
	g.deny = true
	g.mu.Unlock()
	get(200) // Requests use the last valid proof, not an inline audit.
	now = now.Add(61 * time.Second)
	w.auditPreviews(context.Background())
	get(503)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.managed.Sandboxes[r.SandboxID].State != "stopped" {
		t.Fatal("failed audit did not stop runtime")
	}
}

type blockedPreviewAudit struct {
	Enforcement
	entered chan struct{}
	release chan struct{}
}

func (g *blockedPreviewAudit) Check(ctx context.Context, grant GrantContext, phase string) error {
	close(g.entered)
	select {
	case <-g.release:
		return g.Enforcement.Check(ctx, grant, phase)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestSlowBackgroundAuditDoesNotBlockPreview(t *testing.T) {
	w, _, gate, r := managedFixture(t)
	now := time.Now()
	w.Now = func() time.Time { return now }
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	now = now.Add(61 * time.Second)
	blocked := &blockedPreviewAudit{Enforcement: gate, entered: make(chan struct{}), release: make(chan struct{})}
	w.Gate = blocked
	done := make(chan struct{})
	go func() { w.auditPreviews(context.Background()); close(done) }()
	defer func() { close(blocked.release); <-done }()
	<-blocked.entered
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(a.URL)
	if err != nil {
		t.Fatal("request blocked behind background audit", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
}

func TestManagedTwoIndependentRunsAndCapacity(t *testing.T) {
	w, _, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	second := r
	second.ChatID = "chat-two"
	second.SandboxID = "sandbox-two"
	second.RunID = "run-two"
	second.Operation = "bind-chat"
	if _, err := w.dispatch(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	second.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	// Idempotent preparation is valid even when all slots are occupied.
	if _, err := w.dispatch(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	third := second
	third.ChatID = "chat-three"
	third.SandboxID = "sandbox-three"
	third.RunID = "run-three"
	third.Operation = "bind-chat"
	if _, err := w.dispatch(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	third.Operation = "prepare"
	if _, err := w.dispatch(context.Background(), third); !errors.Is(err, ErrBusy) {
		t.Fatalf("third run should wait, got %v", err)
	}
}

func TestClaudeRuntimeCopiedOncePerGuest(t *testing.T) {
	w, d, _, r := managedFixture(t)
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("release one"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.ClaudePath = path
	r.Provider = "claude"
	copies := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		n := 0
		for _, c := range d.calls {
			if strings.HasSuffix(c, ":/tmp/warden-claude") {
				n++
			}
		}
		return n
	}
	nextRun := func(id string) {
		w.mu.Lock()
		w.managed.Sandboxes[r.SandboxID].Active = nil
		w.mu.Unlock()
		r.RunID = id
		prepareFixture(t, w, r)
	}
	prepareFixture(t, w, r)
	if copies() != 1 {
		t.Fatalf("first Claude run should copy the executable once, got %d", copies())
	}
	nextRun("run-two")
	if copies() != 1 {
		t.Fatalf("second run on the same guest copied the executable again (%d copies)", copies())
	}
	// A restarted worker keeps the record with the rest of the managed state.
	fresh := NewWorker(w.Root, "/never-host-exec", "template")
	fresh.Runtime, fresh.Gate, fresh.RuntimeDir, fresh.ClaudePath = d, w.Gate, w.RuntimeDir, path
	if err := fresh.initializeManaged(context.Background()); err != nil {
		t.Fatal(err)
	}
	w = fresh
	nextRun("run-three")
	if copies() != 1 {
		t.Fatalf("restart forgot the installed executable (%d copies)", copies())
	}
	// Replacing the host executable, or losing the guest copy, copies again.
	if err := os.WriteFile(path, []byte("release two!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	nextRun("run-four")
	if copies() != 2 {
		t.Fatalf("replaced executable was not copied (%d copies)", copies())
	}
	d.execOutput = "claude-absent"
	nextRun("run-five")
	if copies() != 3 {
		t.Fatalf("missing guest copy was not restored (%d copies)", copies())
	}
}

func TestProxyCAInstalledOncePerGuestAndAgainOnRotationOrLoss(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	installs := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		n := 0
		for _, c := range d.calls {
			if strings.HasPrefix(c, "ca:") {
				n++
			}
		}
		return n
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	ctx := context.Background()
	if err := w.ensureProxyCALocked(ctx, s, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.ensureProxyCALocked(ctx, s, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	if installs() != 1 {
		t.Fatalf("same CA installed %d times", installs())
	}
	w.mu.Lock()
	if err := w.ensureProxyCALocked(ctx, s, "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n"); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	if installs() != 2 {
		t.Fatal("rotated CA was not installed")
	}
	// The guest reports the file missing during the next prepare: reinstall.
	d.execHook = func(args []string) error { return nil }
	d.execOutput = "ca-absent"
	w.mu.Lock()
	s.Active = nil
	w.mu.Unlock()
	r.RunID = "run-two"
	prepareFixture(t, w, r)
	w.mu.Lock()
	if s.ProxyCA != "" {
		t.Fatal("prepare did not clear the fingerprint after the guest lost the CA")
	}
	if err := w.ensureProxyCALocked(ctx, s, "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n"); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	if installs() != 3 {
		t.Fatal("lost guest CA was not reinstalled")
	}
	if s.ProxyCA == "" {
		t.Fatal("fingerprint not recorded after reinstall")
	}
	if err := w.Runtime.(*testRuntime).InstallCA(ctx, "x", ""); err != nil {
		t.Fatal(err)
	}
}

func TestGuestImageManifestSkipsRuntimeAndCACopies(t *testing.T) {
	w, d, _, r := managedFixture(t)
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "codex-package.json"), []byte(`{"layoutVersion":1,"version":"0.154.0","target":"`+hostTarget()+`","entrypoint":"bin/codex","resourcesDir":"codex-resources","pathDir":"codex-path"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w.RuntimeDir = bundle
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("claude release"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.ClaudePath = claude
	sum := sha256.Sum256([]byte("claude release"))
	cert := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	certSum := sha256.Sum256([]byte(cert))
	d.execOutput = "WARDEN-GUEST-BEGIN\n" + `{"codex":{"version":"0.154.0","target":"` + hostTarget() + `"},"claude":{"version":"2.1.272","sha256":"` + hex.EncodeToString(sum[:]) + `"},"ca":{"sha256":"` + hex.EncodeToString(certSum[:]) + `"}}` + "\nWARDEN-GUEST-END\nca-present\ncodex-present\nclaude-present\n"
	r.Provider = "claude"
	prepareFixture(t, w, r)
	d.mu.Lock()
	for _, c := range d.calls {
		if strings.HasPrefix(c, "copy:") {
			t.Fatalf("preinstalled guest still received a copy: %s", c)
		}
	}
	d.mu.Unlock()
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	if !s.Installed || s.ClaudeInstalled == "" || s.GuestCA != hex.EncodeToString(certSum[:]) {
		t.Fatalf("manifest not recorded: installed=%v claude=%q guestCA=%q", s.Installed, s.ClaudeInstalled, s.GuestCA)
	}
	if err := w.ensureProxyCALocked(context.Background(), s, cert); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	d.mu.Lock()
	for _, c := range d.calls {
		if strings.HasPrefix(c, "ca:") {
			t.Fatal("CA shipped by the image was installed again")
		}
	}
	d.mu.Unlock()
	if s.ProxyCA != hex.EncodeToString(certSum[:]) {
		t.Fatal("shipped CA not recorded as trusted")
	}
	// A manifest naming a different Claude digest does not stop the copy.
	w2, d2, _, r2 := managedFixture(t)
	w2.RuntimeDir, w2.ClaudePath = bundle, claude
	d2.execOutput = strings.Replace(d.execOutput, hex.EncodeToString(sum[:]), strings.Repeat("0", 64), 1)
	r2.Provider = "claude"
	prepareFixture(t, w2, r2)
	copied := false
	d2.mu.Lock()
	for _, c := range d2.calls {
		if strings.HasSuffix(c, ":/tmp/warden-claude") {
			copied = true
		}
	}
	d2.mu.Unlock()
	if !copied {
		t.Fatal("mismatched manifest digest must not suppress the Claude copy")
	}
}
func hostTarget() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64-unknown-linux-musl"
	}
	return "aarch64-unknown-linux-musl"
}

func (d *testRuntime) created() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, "create:") {
			names = append(names, strings.TrimPrefix(c, "create:"))
		}
	}
	return names
}
func waitFor(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestSpareSandboxIsBootedAheadAndAdoptedByTheNextEnvironment(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Spares = 1
	spares := func() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.managed.Spares) }
	w.maintainSpares(context.Background())
	waitFor(t, "spare not created", func() bool { return spares() == 1 })
	w.maintainSpares(context.Background())
	if created := d.created(); len(created) != 1 || !strings.HasPrefix(created[0], "wc-spare-") {
		t.Fatalf("expected one spare creation, got %v", created)
	}
	w.mu.Lock()
	var spareName string
	for name := range w.managed.Spares {
		spareName = name
	}
	hashed := w.managed.Sandboxes[r.SandboxID].RuntimeName
	w.mu.Unlock()
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	w.mu.Unlock()
	if s.RuntimeName != spareName || !s.Created || s.residency == nil {
		t.Fatalf("spare not adopted: runtime %q (spare %q) created=%v", s.RuntimeName, spareName, s.Created)
	}
	for _, name := range d.created() {
		if name == hashed {
			t.Fatal("adopting a spare must not also create the hashed runtime")
		}
	}
	if spares() != 0 {
		t.Fatal("adopted spare still listed")
	}
	// The pool refills, and a restarted worker discards spares it cannot vouch for.
	w.maintainSpares(context.Background())
	waitFor(t, "pool not refilled", func() bool { return spares() == 1 })
	fresh := NewWorker(w.Root, "/never-host-exec", "template")
	fresh.Runtime, fresh.Gate, fresh.RuntimeDir = d, w.Gate, w.RuntimeDir
	if err := fresh.initializeManaged(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	left := len(fresh.managed.Spares)
	fresh.mu.Unlock()
	removed := false
	d.mu.Lock()
	for _, c := range d.calls {
		if strings.HasPrefix(c, "remove:wc-spare-") {
			removed = true
		}
	}
	d.mu.Unlock()
	if left != 0 || !removed {
		t.Fatal("restart must remove stale spares")
	}
}

func TestSpareIsNotUsedForRepositoryClonesOrWhenDisabled(t *testing.T) {
	w, _, _, r := managedFixture(t)
	w.Spares = 1
	w.maintainSpares(context.Background())
	waitFor(t, "spare not created", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.managed.Spares) == 1 })
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Source = "https://github.com/example/repo"
	w.mu.Unlock()
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	kept := len(w.managed.Spares)
	w.mu.Unlock()
	if strings.HasPrefix(s.RuntimeName, "wc-spare-") || kept != 1 {
		t.Fatal("a clone-mode sandbox must be created itself and leave the spare")
	}
	other, d2, _, _ := managedFixture(t)
	other.Spares = 0
	other.maintainSpares(context.Background())
	time.Sleep(20 * time.Millisecond)
	if len(d2.created()) != 0 {
		t.Fatal("spares disabled but one was created")
	}
}

func (d *testRuntime) countPrefix(prefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestFreshGuestSkipsPublicationCheckAndLaterRunsProbeConcurrently(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r) // creates the guest: nothing can be published yet
	if d.countPrefix("mappings:") != 0 {
		t.Fatal("a guest created in this prepare was asked for port mappings")
	}
	if d.countPrefix("hold-residency:") != 1 || d.countPrefix("exec:"+w.managed.Sandboxes[r.SandboxID].RuntimeName+":sh -c mkdir") != 1 {
		t.Fatalf("keep-alive and report expected once: %v", d.calls)
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].Active = nil
	w.mu.Unlock()
	r.RunID = "run-two"
	prepareFixture(t, w, r)
	if d.countPrefix("mappings:") != 1 {
		t.Fatal("a resumed guest must still have its publications verified")
	}
	if d.countPrefix("hold-residency:") != 1 {
		t.Fatal("a resident guest must not get a second keep-alive")
	}
}

func TestAdoptedSpareNeedsNoGuestRoundTrips(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Spares = 1
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "codex-package.json"), []byte(`{"layoutVersion":1,"version":"0.154.0","target":"`+hostTarget()+`","entrypoint":"bin/codex","resourcesDir":"codex-resources","pathDir":"codex-path"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w.RuntimeDir = bundle
	d.execOutput = "WARDEN-GUEST-BEGIN\n" + `{"codex":{"version":"0.154.0","target":"` + hostTarget() + `"}}` + "\nWARDEN-GUEST-END\nca-present\ncodex-present\nclaude-present\n"
	w.maintainSpares(context.Background())
	waitFor(t, "spare not created", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.managed.Spares) == 1 })
	w.mu.Lock()
	var spare *spareSandbox
	for _, sp := range w.managed.Spares {
		spare = sp
	}
	w.mu.Unlock()
	if spare.report == "" {
		t.Fatal("spare boot did not capture the guest report")
	}
	execs := d.countPrefix("exec:" + spare.Name)
	holds := d.countPrefix("hold-residency:" + spare.Name)
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	w.mu.Unlock()
	if s.RuntimeName != spare.Name {
		t.Fatal("spare not adopted")
	}
	if d.countPrefix("exec:"+spare.Name) != execs || d.countPrefix("hold-residency:"+spare.Name) != holds || d.countPrefix("mappings:"+spare.Name) != 0 {
		t.Fatalf("adoption must reuse the spare's report and keep-alive and skip the mapping check: %v", d.calls)
	}
	if s.pendingReport != "" || s.fresh || !s.Installed {
		t.Fatalf("report not consumed: pending=%q fresh=%v installed=%v", s.pendingReport, s.fresh, s.Installed)
	}
}
