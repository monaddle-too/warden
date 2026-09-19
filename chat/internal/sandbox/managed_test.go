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
	"slices"
	"sort"
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
	// grants are every context registered or checked, in order.
	grants []GrantContext
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
	g.mu.Lock()
	g.grants = append(g.grants, c)
	g.mu.Unlock()
	return g.record("register:" + c.SandboxID)
}
func (g *testGate) Check(_ context.Context, c GrantContext, phase string) error {
	g.mu.Lock()
	g.grants = append(g.grants, c)
	g.mu.Unlock()
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
	resizeRestart bool      // Resize reports the instance replaced, as SBX does
	resizeErr     error     // Resize's answer when set
	runs          []RunSpec // every Stream launch, in order
	requestURI    string    // the last request the fake guest service saw
	// digests is what ImageDigest answers per runtime name (a snapshot's
	// digest for a copy or a regeneration); a name without one errors.
	digests map[string]string
	// survivors is what a restarted worker's Reconcile finds still
	// running: runtime name to the generation of its guest.
	survivors map[string]string
	// lost is what Resident denies: guests taken away under the worker (a
	// preempted spare pod).
	lost map[string]bool
}

func (d *testRuntime) Resident(_ context.Context, name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "resident:"+name)
	return !d.lost[name], nil
}

func (d *testRuntime) lose(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lost == nil {
		d.lost = map[string]bool{}
	}
	d.lost[name] = true
}

func (d *testRuntime) removed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, "remove:") {
			names = append(names, strings.TrimPrefix(c, "remove:"))
		}
	}
	return names
}

func (d *testRuntime) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, s)
}
func (d *testRuntime) Create(ctx context.Context, s RuntimeSpec) error {
	d.record("create:" + s.Name)
	if !s.Resources.IsZero() {
		d.record("size:" + s.Name + ":" + s.Resources.String())
	}
	if s.Source != "" {
		d.record("source:" + s.Name + ":" + s.Source)
	}
	Report(ctx, "creating the VM")
	if d.createStarted != nil {
		close(d.createStarted)
	}
	if d.createBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (d *testRuntime) ImageDigest(_ context.Context, name string) (string, error) {
	d.record("image:" + name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if digest, ok := d.digests[name]; ok {
		return digest, nil
	}
	return "", errors.New("no digest for " + name)
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
func (d *testRuntime) CopyOut(_ context.Context, name, source, target string) error {
	d.record("copyout:" + name + ":" + source + ":" + target)
	return nil
}
func (d *testRuntime) Address(context.Context, string) (string, error) { return "127.0.0.1", nil }

// Stream records the launch and, like the SBX driver, installs the broker
// CA the guest does not trust yet ("ca:<name>").
func (d *testRuntime) Stream(ctx context.Context, name string, run RunSpec) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	d.runs = append(d.runs, run)
	d.mu.Unlock()
	if run.Broker.CACertificate != "" && !run.TrustsCA {
		d.record("ca:" + name)
	}
	a, b := net.Pipe()
	go func() { <-ctx.Done(); b.Close() }()
	return a, nil
}
func (d *testRuntime) Resize(ctx context.Context, name string, r Resources) (bool, error) {
	d.record("resize:" + name + ":" + r.String())
	if d.resizeErr != nil {
		return false, d.resizeErr
	}
	return d.resizeRestart, nil
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

// Reconcile records the registered runtimes a restarted worker hands the
// driver (name, with "*" for one registered resident), sorted so tests can
// compare them, and reports the names in survivors as still running.
func (d *testRuntime) Reconcile(_ context.Context, registered []RegisteredRuntime) ([]string, error) {
	var names, kept []string
	for _, r := range registered {
		name := r.Name
		if r.Resident {
			name += "*"
		}
		names = append(names, name)
		if d.survivors[r.Name] == r.Generation && r.Resident {
			kept = append(kept, r.Name)
		}
	}
	sort.Strings(names)
	d.record("reconcile:" + strings.Join(names, ","))
	return kept, nil
}

// Publish serves a fake guest service on a fresh loopback port, the same
// contract as the SBX driver: the mapping is reserved with the caller
// before it takes effect. publishAt restores a known mapping (SBX brings
// a stopped guest's publications back on resume).
func (d *testRuntime) Publish(_ context.Context, _ string, guestPort int, reserve func(PortMapping) error) (PortMapping, error) {
	d.record("publish")
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return PortMapping{}, err
	}
	m := PortMapping{Address: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, GuestPort: guestPort}
	if reserve != nil {
		if err = reserve(m); err != nil {
			l.Close()
			return PortMapping{}, err
		}
	}
	d.serve(l, m)
	return m, nil
}
func (d *testRuntime) publishAt(m PortMapping) error {
	l, err := net.Listen("tcp4", net.JoinHostPort(m.Address, fmtInt(m.Port)))
	if err != nil {
		return err
	}
	d.serve(l, m)
	return nil
}
func (d *testRuntime) serve(l net.Listener, m PortMapping) {
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.headers = r.Header.Clone()
		d.requestURI = r.URL.RequestURI()
		d.mu.Unlock()
		w.Header().Set("Set-Cookie", "bad=secret")
		_, _ = io.WriteString(w, "counter")
	})}
	d.mu.Lock()
	d.servers[m.Port] = server
	d.mu.Unlock()
	go server.Serve(l)
}
func (d *testRuntime) Unpublish(_ context.Context, _ string, m PortMapping) error {
	d.record("unpublish")
	d.mu.Lock()
	server := d.servers[m.Port]
	delete(d.servers, m.Port)
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
		out = append(out, PortMapping{Address: "127.0.0.1", Port: host, GuestPort: 3000})
	}
	return out, nil
}
func (d *testRuntime) lastRun() RunSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runs) == 0 {
		return RunSpec{}
	}
	return d.runs[len(d.runs)-1]
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
func TestPolicyEnforcementRejectsRegistrationAsReadiness(t *testing.T) {
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
	gate := &PolicyEnforcement{Address: "unix://" + socket}
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
	if err != nil || len(mappings) != 1 || mappings[0] != (PortMapping{Address: "127.0.0.1", Port: 49484, GuestPort: 8080}) {
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
	// The registry's own answer: a status op that finds the lock busy (the
	// stream's last bookkeeping) answers from the snapshot, without
	// attachments.
	r.Operation = "status"
	w.mu.Lock()
	res := w.statusLocked(r)
	w.mu.Unlock()
	if res.Sandbox.State != "stopped" || len(res.Attachments) != 1 || res.Attachments[0].State != "stopped" || res.Attachments[0].URL != "" {
		t.Fatal(res)
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
	saved := savedManaged(t, w)
	if got := saved.Publications[pubKey(r.SandboxID, 3000)]; got.State != "removed" || got.HostPort != host {
		t.Fatal("revoked mapping identity forgotten", got)
	}
	// SBX can restore its saved loopback publication when the VM starts again.
	if err := d.publishAt(PortMapping{Address: "127.0.0.1", Port: host, GuestPort: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcileRemovedLocked(context.Background(), w.managed.Sandboxes[r.SandboxID]); err != nil {
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
	now = now.Add(31 * time.Minute) // past the default idle window
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
func (d *testRuntime) Prepare(_ context.Context, spec RuntimeSpec) (io.Closer, error) {
	name := spec.Name
	d.record("hold-residency:" + name)
	if !spec.Resources.IsZero() {
		d.record("prepare:" + name + ":" + spec.Resources.String())
	}
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

// The idle window counts from the last chat activity: an activity report
// carries the time of the turn's end it reports (never moving the clock
// back, never ahead of now), and the run's stream ending — a resident
// session released after sitting idle — is not activity, so the sweep
// stops the sandbox IdleTimeout after the last reported turn's end, not
// after the release.
func TestIdleWindowCountsFromReportedActivityNotTheStreamEnd(t *testing.T) {
	w, _, _, r := managedFixture(t)
	w.IdleTimeout = 30 * time.Minute
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	now := start
	w.Now = func() time.Time { return now }
	prepareFixture(t, w, r)
	activity := func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.managed.Sandboxes[r.SandboxID].LastActivity
	}
	if !activity().Equal(start) {
		t.Fatalf("activity after prepare: %v", activity())
	}
	now = start.Add(20 * time.Minute)
	report := func(at time.Time) {
		t.Helper()
		q := r
		q.Operation = "activity"
		q.At = at
		if _, err := w.dispatch(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	report(start.Add(5 * time.Minute)) // the turn ended at +5, reported late
	if !activity().Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("activity after a late report: %v", activity())
	}
	report(start.Add(2 * time.Minute)) // an older report never moves the clock back
	if !activity().Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("activity moved back: %v", activity())
	}
	report(start.Add(time.Hour)) // a report from the future counts as now
	if !activity().Equal(now) {
		t.Fatalf("activity ahead of now: %v", activity())
	}
	report(time.Time{}) // no time: now (the chat menu's "Keep workspace running")
	now = start.Add(21 * time.Minute)
	report(time.Time{})
	if !activity().Equal(now) {
		t.Fatalf("activity without a time: %v", activity())
	}
	// The session is released ten minutes later: the stream ends, the run
	// with it, and the clock stays at the last report.
	w.mu.Lock()
	grant := w.managed.Sandboxes[r.SandboxID].Grant
	w.mu.Unlock()
	now = start.Add(31 * time.Minute)
	w.finishManagedRun(r, grant, false)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	state, active := s.State, s.Active
	w.mu.Unlock()
	if state != "running" || active != nil || !activity().Equal(start.Add(21*time.Minute)) {
		t.Fatalf("after the stream end: state=%s active=%v activity=%v", state, active != nil, activity())
	}
	now = start.Add(50 * time.Minute)
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	state = w.managed.Sandboxes[r.SandboxID].State
	w.mu.Unlock()
	if state != "running" {
		t.Fatalf("stopped before the window from the last turn passed: %s", state)
	}
	now = start.Add(52 * time.Minute)
	if err := w.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	state = w.managed.Sandboxes[r.SandboxID].State
	w.mu.Unlock()
	if state != "stopped" {
		t.Fatalf("not stopped after the window: %s", state)
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
	launch := func(certificate string) {
		t.Helper()
		stream, err := w.launchLocked(ctx, s, BrokerConfig{CACertificate: certificate, ProxyURL: "http://gateway"})
		if err != nil {
			t.Fatal(err)
		}
		stream.Close()
	}
	launch("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	launch("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	w.mu.Unlock()
	if installs() != 1 {
		t.Fatalf("same CA installed %d times", installs())
	}
	w.mu.Lock()
	launch("-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n")
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
	launch("-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n")
	w.mu.Unlock()
	if installs() != 3 {
		t.Fatal("lost guest CA was not reinstalled")
	}
	if s.ProxyCA == "" {
		t.Fatal("fingerprint not recorded after reinstall")
	}
	// A launch without a broker CA neither installs nor forgets anything.
	w.mu.Lock()
	launch("")
	w.mu.Unlock()
	if installs() != 3 || s.ProxyCA == "" {
		t.Fatal("a CA-less launch changed the trust record")
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
	stream, err := w.launchLocked(context.Background(), s, BrokerConfig{CACertificate: cert, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
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
	// The driver is then handed the registered sandboxes (the adopted spare
	// among them, under its runtime name) and no spare, in that order.
	d.mu.Lock()
	calls := append([]string(nil), d.calls...)
	d.mu.Unlock()
	reconciled := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "reconcile:") {
			reconciled = i
			if c != "reconcile:"+spareName+"*" {
				t.Fatalf("reconcile did not name the registered sandbox: %s", c)
			}
		}
	}
	if reconciled < 0 {
		t.Fatal("restart did not reconcile the driver")
	}
	for _, c := range calls[reconciled:] {
		if strings.HasPrefix(c, "remove:wc-spare-") || strings.HasPrefix(c, "stop:") {
			t.Fatalf("reconcile ran before the registry pass: %v", calls)
		}
	}
}

// A restarted worker on a driver whose guests outlive it keeps a sandbox
// whose guest the driver finds still running at its generation: running,
// with no run on it, its grant ended, its idle window started over; the
// driver stops the guests it does not keep, and the worker stops nothing
// itself. A later prepare resumes the kept sandbox without a stop or a
// creation, and the idle sweep still stops it once the window passes.
func TestRestartKeepsTheGuestsTheDriverFindsRunning(t *testing.T) {
	w, d, g, r := managedFixture(t)
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	generation, activity := s.Generation, s.LastActivity
	w.mu.Unlock()
	if s.State != "running" || s.Active == nil || generation == "" {
		t.Fatalf("fixture not running: %s active=%v generation=%q", s.State, s.Active != nil, generation)
	}
	// A second sandbox on the same worker whose guest did not survive.
	other := r
	other.ChatID, other.SandboxID, other.RunID = "chat-two", "sandbox-two", "run-two"
	if _, err := w.dispatch(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	prepareFixture(t, w, other)
	w.mu.Lock()
	lostName := w.managed.Sandboxes[other.SandboxID].RuntimeName
	w.mu.Unlock()
	later := activity.Add(20 * time.Minute)
	d.survivors = map[string]string{s.RuntimeName: generation}
	fresh := NewWorker(w.Root, "/never-host-exec", "template")
	fresh.Runtime, fresh.Gate, fresh.RuntimeDir = d, g, w.RuntimeDir
	fresh.IdleTimeout = 30 * time.Minute
	fresh.Now = func() time.Time { return later }
	before := len(g.calls)
	if err := fresh.initializeManaged(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	kept, lost := fresh.managed.Sandboxes[r.SandboxID], fresh.managed.Sandboxes[other.SandboxID]
	fresh.mu.Unlock()
	if kept.State != "running" || kept.Active != nil || kept.Generation != generation || !kept.LastActivity.Equal(later) {
		t.Fatalf("kept sandbox after restart: state=%s active=%v generation=%q activity=%v", kept.State, kept.Active != nil, kept.Generation, kept.LastActivity)
	}
	if lost.State != "stopped" || lost.Active != nil {
		t.Fatalf("lost sandbox after restart: state=%s active=%v", lost.State, lost.Active != nil)
	}
	ended := 0
	for _, c := range g.calls[before:] {
		if c == "end" {
			ended++
		}
	}
	if ended != 2 {
		t.Fatalf("both runs' grants must end at the restart: %v", g.calls[before:])
	}
	d.mu.Lock()
	calls := append([]string(nil), d.calls...)
	d.mu.Unlock()
	reconciled := false
	for _, c := range calls {
		if strings.HasPrefix(c, "reconcile:") {
			reconciled = true
			if c != "reconcile:"+lostName+"*,"+s.RuntimeName+"*" && c != "reconcile:"+s.RuntimeName+"*,"+lostName+"*" {
				t.Fatalf("reconcile did not name both sandboxes resident: %s", c)
			}
		}
		if strings.HasPrefix(c, "stop:") && reconciled {
			t.Fatalf("the worker stopped a guest the driver settles: %v", calls)
		}
	}
	if !reconciled {
		t.Fatal("restart did not reconcile the driver")
	}
	// The next prepare resumes the kept sandbox on the same generation
	// without creating or stopping anything.
	created, stops := len(d.created()), 0
	resumed := r
	resumed.RunID = "run-three"
	prepareFixture(t, fresh, resumed)
	d.mu.Lock()
	for _, c := range d.calls {
		if strings.HasPrefix(c, "stop:") {
			stops++
		}
	}
	d.mu.Unlock()
	fresh.mu.Lock()
	again := fresh.managed.Sandboxes[r.SandboxID]
	fresh.mu.Unlock()
	if again.State != "running" || again.Active == nil || again.Generation != generation || len(d.created()) != created || stops != 0 {
		t.Fatalf("resume on the kept guest: state=%s active=%v generation=%q created=%d stops=%d", again.State, again.Active != nil, again.Generation, len(d.created()), stops)
	}
	// Once the run ends, the idle window counts from the restart.
	fresh.mu.Lock()
	again.Active = nil
	fresh.mu.Unlock()
	fresh.Now = func() time.Time { return later.Add(29 * time.Minute) }
	if err := fresh.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	state := fresh.managed.Sandboxes[r.SandboxID].State
	fresh.mu.Unlock()
	if state != "running" {
		t.Fatalf("swept inside the idle window: %s", state)
	}
	fresh.Now = func() time.Time { return later.Add(31 * time.Minute) }
	if err := fresh.SweepIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	state = fresh.managed.Sandboxes[r.SandboxID].State
	fresh.mu.Unlock()
	if state != "stopped" {
		t.Fatalf("not swept after the idle window: %s", state)
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

// A workspace created with a size keeps it in the registry, is created at
// it, and where a resize restarts (SBX) does not take a spare booted at
// the default size; a size on a later request is ignored. The health
// answer carries the limits.
func TestWorkspaceSizeIsRecordedAtCreationAndSkipsSpares(t *testing.T) {
	d := &testRuntime{servers: map[int]*http.Server{}}
	w := NewWorker(t.TempDir(), "/never-host-exec", "template")
	w.Runtime, w.Gate, w.RuntimeDir = d, &testGate{}, "/fake-pinned-linux-bundle"
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000, Restart: true}
	w.Spares = 1
	w.mu.Lock()
	w.defaultsLocked()
	w.mu.Unlock()
	w.maintainSpares(context.Background())
	waitFor(t, "spare not created", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.managed.Spares) == 1 })
	r := Request{Version: 2, Operation: "bind-chat", ProjectID: "project-one", ChatID: "chat-one", SandboxID: "sandbox-one", RunID: "run-one", PrincipalID: "owner", Resources: &Resources{MemoryMB: 4096}}
	if _, err := w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Resources = &Resources{MemoryMB: 65536}
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.Resources != (Resources{CPUMilli: 1000, MemoryMB: 4096}) || res.Limits == nil || res.Limits.Max.MemoryMB != 8192 {
		t.Fatalf("size after a second bind: %+v %v", res.Sandbox, err)
	}
	r.Resources = nil
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	spares := len(w.managed.Spares)
	w.mu.Unlock()
	if strings.HasPrefix(s.RuntimeName, "wc-spare-") || spares != 1 {
		t.Fatal("a sized workspace adopted the default-size spare")
	}
	d.mu.Lock()
	calls := strings.Join(d.calls, "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "size:"+s.RuntimeName+":1 CPU · 4 GiB") {
		t.Fatalf("runtime created without the size:\n%s", calls)
	}
	over := Request{Version: 2, Operation: "bind-chat", ProjectID: "project-one", ChatID: "chat-two", SandboxID: "sandbox-two", RunID: "run-two", PrincipalID: "owner", Resources: &Resources{CPUMilli: 500}}
	if _, err := w.dispatch(context.Background(), over); err == nil || !strings.Contains(err.Error(), "multiple of 1 CPU") {
		t.Fatalf("fractional CPU accepted on SBX: %v", err)
	}
}

// Where a resize is live (Kubernetes) a sized workspace adopts the spare
// and the spare is grown to the size before the run; a spare the driver
// cannot grow in place is stopped and a pod at the right size prepared.
func TestSizedWorkspaceAdoptsAndResizesASpareOnALivePlatform(t *testing.T) {
	for _, infeasible := range []bool{false, true} {
		d := &testRuntime{servers: map[int]*http.Server{}}
		if infeasible {
			d.resizeErr = ErrResizeInfeasible
		}
		w := NewWorker(t.TempDir(), "/never-host-exec", "template")
		w.Runtime, w.Gate, w.RuntimeDir = d, &testGate{}, "/fake-pinned-linux-bundle"
		w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 250}
		w.Spares = 1
		w.mu.Lock()
		w.defaultsLocked()
		w.mu.Unlock()
		w.maintainSpares(context.Background())
		waitFor(t, "spare not created", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.managed.Spares) == 1 })
		r := Request{Version: 2, Operation: "bind-chat", ProjectID: "project-one", ChatID: "chat-one", SandboxID: "sandbox-one", RunID: "run-one", PrincipalID: "owner", Resources: &Resources{CPUMilli: 1500, MemoryMB: 4096}}
		if _, err := w.dispatch(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		r.Resources = nil
		prepareFixture(t, w, r)
		w.mu.Lock()
		s := w.managed.Sandboxes[r.SandboxID]
		spares := len(w.managed.Spares)
		w.mu.Unlock()
		if !strings.HasPrefix(s.RuntimeName, "wc-spare-") || spares != 0 {
			t.Fatalf("infeasible=%v: the spare was not adopted: %s, %d spares", infeasible, s.RuntimeName, spares)
		}
		d.mu.Lock()
		calls := strings.Join(d.calls, "\n")
		d.mu.Unlock()
		if !strings.Contains(calls, "resize:"+s.RuntimeName+":1.5 CPUs · 4 GiB") {
			t.Fatalf("infeasible=%v: adopted spare not resized:\n%s", infeasible, calls)
		}
		replaced := strings.Contains(calls, "stop:"+s.RuntimeName+"\nhold-residency:"+s.RuntimeName+"\nprepare:"+s.RuntimeName+":1.5 CPUs · 4 GiB")
		if replaced != infeasible {
			t.Fatalf("infeasible=%v: replaced=%v:\n%s", infeasible, replaced, calls)
		}
		if s.State != "running" {
			t.Fatalf("infeasible=%v: state %s", infeasible, s.State)
		}
	}
}

// A live platform that cannot apply a size in place (a memory decrease
// the kubelet refuses, a runtime without in-place resize) has the sandbox
// stopped instead, so the next start carries the size; the record holds
// it either way. Under an active run the in-place attempt is made (that
// is what a live resize is for) and, refused, answered with
// ErrResizeRestart rather than a stop the caller did not arrange.
func TestResizeInfeasibleInPlaceStopsTheSandboxOnALivePlatform(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 250}
	prepareFixture(t, w, r)
	d.resizeErr = ErrResizeInfeasible
	r.Operation = "resize"
	r.Resources = &Resources{CPUMilli: 2000, MemoryMB: 4096}
	if _, err := w.dispatch(context.Background(), r); !errors.Is(err, ErrResizeRestart) {
		t.Fatalf("under a run: %v", err)
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	state := s.State
	w.mu.Unlock()
	if state != "running" {
		t.Fatalf("the sandbox was stopped under a run: %s", state)
	}
	d.resizeErr = nil
	if res, err := w.dispatch(context.Background(), r); err != nil || res.Sandbox.Resources != *r.Resources || res.Sandbox.State != "running" {
		t.Fatalf("live resize under a run: %+v %v", res.Sandbox, err)
	}
	w.mu.Lock()
	s.Active = nil
	w.mu.Unlock()
	d.resizeErr = ErrResizeInfeasible
	r.Resources = &Resources{CPUMilli: 500, MemoryMB: 1024}
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.Resources != *r.Resources || res.Sandbox.State != "stopped" {
		t.Fatalf("infeasible resize: %+v %v", res.Sandbox, err)
	}
	d.mu.Lock()
	calls := strings.Join(d.calls, "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "resize:"+s.RuntimeName+":0.5 CPUs · 1 GiB\nrelease-residency\nstop:"+s.RuntimeName) {
		t.Fatalf("driver calls:\n%s", calls)
	}
	d.resizeErr = errors.New("api server down")
	r.Resources = &Resources{CPUMilli: 2000, MemoryMB: 2048}
	if _, err = w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "api server down") {
		t.Fatalf("other errors must surface: %v", err)
	}
}

// Resize: refused while a run is active; a sandbox not created yet only
// records the size; a created one goes through the driver, and when the
// driver replaced the instance the sandbox is stopped and its residency
// released. The same size again is a no-op.
func TestResizeRecordsAppliesAndStopsWhenTheInstanceIsReplaced(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Limits = ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000, Restart: true}
	r.Operation = "resize"
	r.Resources = &Resources{CPUMilli: 2000, MemoryMB: 4096}
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.Resources != *r.Resources {
		t.Fatalf("size not recorded before creation: %+v %v", res.Sandbox, err)
	}
	if d.calls != nil && strings.Contains(strings.Join(d.calls, " "), "resize:") {
		t.Fatal("driver resized a sandbox that does not exist")
	}
	prepareFixture(t, w, r)
	r.Operation = "resize"
	r.Resources = &Resources{CPUMilli: 4000, MemoryMB: 8192}
	if _, err = w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "active run") {
		t.Fatalf("resized under an active run: %v", err)
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	s.Active = nil
	w.mu.Unlock()
	d.resizeRestart = true
	res, err = w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.Resources != *r.Resources || res.Sandbox.State != "stopped" {
		t.Fatalf("resize with restart: %+v %v", res.Sandbox, err)
	}
	w.mu.Lock()
	held := s.residency != nil
	w.mu.Unlock()
	if held {
		t.Fatal("residency kept across a replaced instance")
	}
	d.mu.Lock()
	calls := strings.Join(d.calls, "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "stop:"+s.RuntimeName+"\n") || !strings.Contains(calls, "resize:"+s.RuntimeName+":4 CPUs · 8 GiB") {
		t.Fatalf("driver calls:\n%s", calls)
	}
	n := strings.Count(calls, "resize:")
	if _, err = w.dispatch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	again := strings.Count(strings.Join(d.calls, "\n"), "resize:")
	d.mu.Unlock()
	if again != n {
		t.Fatal("the same size resized again")
	}
	r.Resources = &Resources{MemoryMB: 16384}
	if _, err = w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over the ceiling: %v", err)
	}
}

// A guest whose manifest names other runtime paths gets the copies there
// and is launched from there; a guest without a manifest keeps the SBX
// template's /tmp layout.
func TestGuestManifestPathsDriveCopiesAndLaunch(t *testing.T) {
	w, d, _, r := managedFixture(t)
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("claude release"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.ClaudePath = claude
	d.execOutput = "WARDEN-GUEST-BEGIN\n" + `{"codex":{"version":"0.0.0","target":"none"},"claude":{"version":"0","sha256":""},"ca":{"sha256":""},"paths":{"codex":"/opt/warden/runtime","claude":"/opt/warden/claude/claude","trust":"/opt/warden/trust/ca-certificates.crt","home":"/home/agent"},"user":{"name":"agent","uid":1000,"gid":1000}}` + "\nWARDEN-GUEST-END\nca-absent\ncodex-absent\nclaude-absent\n"
	r.Provider = "claude"
	prepareFixture(t, w, r)
	var copies []string
	d.mu.Lock()
	for _, c := range d.calls {
		if strings.HasPrefix(c, "copy:") {
			copies = append(copies, c)
		}
	}
	d.mu.Unlock()
	if len(copies) != 2 || !strings.HasSuffix(copies[0], ":/opt/warden/claude/claude") || !strings.HasSuffix(copies[1], ":/opt/warden/runtime-stage") {
		t.Fatalf("copies did not follow the manifest: %v", copies)
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	stream, err := w.launchLocked(context.Background(), s, BrokerConfig{Provider: "claude"})
	w.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if run := d.lastRun(); run.Paths.Codex != "/opt/warden/runtime" || run.Paths.Claude != "/opt/warden/claude/claude" || run.Paths.Trust != "/opt/warden/trust/ca-certificates.crt" || run.Paths.Home != "/home/agent" || run.Paths.User != "agent" || run.Directory != s.Directory {
		t.Fatalf("launch did not carry the manifest paths: %+v", run)
	}
	// No manifest: the template layout.
	w2, d2, _, r2 := managedFixture(t)
	w2.ClaudePath = claude
	d2.execOutput = "ca-absent\ncodex-absent\nclaude-absent\n"
	r2.Provider = "claude"
	prepareFixture(t, w2, r2)
	d2.mu.Lock()
	var defaults []string
	for _, c := range d2.calls {
		if strings.HasPrefix(c, "copy:") {
			defaults = append(defaults, c)
		}
	}
	d2.mu.Unlock()
	if len(defaults) != 2 || !strings.HasSuffix(defaults[0], ":/tmp/warden-claude") || !strings.HasSuffix(defaults[1], ":/tmp/warden-runtime-stage") {
		t.Fatalf("default copies: %v", defaults)
	}
	w2.mu.Lock()
	stream, err = w2.launchLocked(context.Background(), w2.managed.Sandboxes[r2.SandboxID], BrokerConfig{Provider: "claude"})
	w2.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if run := d2.lastRun(); run.Paths.Codex != defaultCodexPath || run.Paths.Claude != defaultClaudePath {
		t.Fatalf("default launch paths: %+v", run.Paths)
	}
}

// A worker state file written before publications recorded their address
// gets the driver's address on load, so the availability proxy still
// reaches the guest and the mapping check still matches.
func TestPublicationAddressBackfilledFromDriverOnLoad(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	if a.State != "available" {
		t.Fatal(a)
	}
	// A record written before publications recorded their address.
	if _, err := w.store.db.Exec(`UPDATE publications SET record = json_remove(record, '$.Address')`); err != nil {
		t.Fatal(err)
	}
	fresh := NewWorker(w.Root, "/never-host-exec", "template")
	fresh.Runtime, fresh.Gate, fresh.RuntimeDir = d, w.Gate, w.RuntimeDir
	if err := fresh.initializeManaged(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	p := fresh.managed.Publications[pubKey(r.SandboxID, 3000)]
	fresh.mu.Unlock()
	if p == nil || p.Address != "127.0.0.1" || p.HostPort == 0 {
		t.Fatalf("address not backfilled: %+v", p)
	}
}

// The owner's Start: a stopped, created sandbox is made resident again
// without a run and reported running; a sandbox never created has nothing
// to start; a running one is left alone.
func TestStartBringsAStoppedSandboxBackWithoutARun(t *testing.T) {
	w, d, _, r := managedFixture(t)
	r.Operation = "start"
	if _, err := w.dispatch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "no sandbox yet") {
		t.Fatalf("start before creation: %v", err)
	}
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	s.Active = nil
	w.mu.Unlock()
	r.Operation = "stop"
	if res, err := w.dispatch(context.Background(), r); err != nil || res.Sandbox.State != "stopped" {
		t.Fatalf("stop: %+v %v", res.Sandbox, err)
	}
	d.mu.Lock()
	before := len(d.calls)
	d.mu.Unlock()
	r.Operation = "start"
	res, err := w.dispatch(context.Background(), r)
	if err != nil || res.Sandbox.State != "running" {
		t.Fatalf("start: %+v %v", res.Sandbox, err)
	}
	d.mu.Lock()
	calls := strings.Join(d.calls[before:], "\n")
	d.mu.Unlock()
	if !strings.Contains(calls, "hold-residency:"+s.RuntimeName) || strings.Contains(calls, "exec:") {
		t.Fatalf("start must only restore residency:\n%s", calls)
	}
	w.mu.Lock()
	held := s.residency != nil
	w.mu.Unlock()
	if !held {
		t.Fatal("residency not held after start")
	}
	if _, err = w.dispatch(context.Background(), r); err != nil {
		t.Fatalf("start of a running sandbox: %v", err)
	}
}

func TestOperationTimeoutHonoursPrepareTimeout(t *testing.T) {
	w := &Worker{}
	if got := w.operationTimeout(Request{Operation: "prepare"}); got != 2*time.Minute {
		t.Fatalf("unset PrepareTimeout: prepare bounded by %v, want 2m", got)
	}
	w.PrepareTimeout = 10 * time.Minute
	for _, op := range []string{"prepare", "start", "clone", "resize"} {
		if got := w.operationTimeout(Request{Operation: op}); got != 10*time.Minute {
			t.Errorf("%s bounded by %v, want the PrepareTimeout", op, got)
		}
	}
	for _, op := range []string{"status", "stop", "remove", "exec"} {
		if got := w.operationTimeout(Request{Operation: op}); got != 2*time.Minute {
			t.Errorf("%s bounded by %v, want 2m", op, got)
		}
	}
}

// A spare whose guest was taken away (its pod preempted by a sandbox pod)
// is retired by the periodic look and replaced, and one lost between looks
// is not handed to a chat: the chat is created fresh instead.
func TestPreemptedSpareIsReplacedAndNeverAdopted(t *testing.T) {
	w, d, _, r := managedFixture(t)
	w.Spares = 1
	spares := func() map[string]bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		out := map[string]bool{}
		for name := range w.managed.Spares {
			out[name] = true
		}
		return out
	}
	w.maintainSpares(context.Background())
	waitFor(t, "spare not created", func() bool { return len(spares()) == 1 })
	var first string
	for name := range spares() {
		first = name
	}
	// Still there: the look keeps it.
	w.retireLostSpares(context.Background())
	if !spares()[first] {
		t.Fatal("a resident spare was retired")
	}
	// Preempted: the next look (10 s later) retires and removes it, and
	// the pool refills with a new name.
	d.lose(first)
	w.retireLostSpares(context.Background())
	if !spares()[first] {
		t.Fatal("the look ran again within 10 s")
	}
	w.mu.Lock()
	w.spareCheckAt = time.Time{}
	w.mu.Unlock()
	w.retireLostSpares(context.Background())
	if len(spares()) != 0 {
		t.Fatalf("lost spare still listed: %v", spares())
	}
	waitFor(t, "lost spare not removed", func() bool {
		for _, name := range d.removed() {
			if name == first {
				return true
			}
		}
		return false
	})
	w.maintainSpares(context.Background())
	waitFor(t, "pool not refilled", func() bool { return len(spares()) == 1 && !spares()[first] })
	var second string
	for name := range spares() {
		second = name
	}
	// Lost between looks: adoption checks, retires it and creates fresh.
	d.lose(second)
	w.mu.Lock()
	hashed := w.managed.Sandboxes[r.SandboxID].RuntimeName
	w.mu.Unlock()
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	w.mu.Unlock()
	if s.RuntimeName != hashed || !s.Created {
		t.Fatalf("lost spare adopted: runtime %q (spare %q, hashed %q)", s.RuntimeName, second, hashed)
	}
	if len(spares()) != 0 {
		t.Fatalf("lost spare still listed after adoption: %v", spares())
	}
	waitFor(t, "lost spare not removed after adoption", func() bool {
		for _, name := range d.removed() {
			if name == second {
				return true
			}
		}
		return false
	})
}

// A restarted worker finds a sandbox whose creation was interrupted (the
// record says creating, nothing created) and cannot stop it: sbx reports
// the name unknown, or its daemon has lost its Docker session. Once, the
// runner exited over it, taking every chat down; then the record was marked
// failed and the stop retried, and warned about, at every start. The record
// is reset to never created instead, so the next run creates the sandbox
// afresh; a sandbox that was created keeps its workspace and is marked
// failed.
func TestRestartResetsAnInterruptedCreationWhoseStopIsRefused(t *testing.T) {
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	s.Created, s.Creating, s.State = false, true, "starting"
	interrupted := s.RuntimeName
	w.managed.Sandboxes["sandbox-two"] = &managedSandbox{SandboxInfo: SandboxInfo{ID: "sandbox-two", ProjectID: r.ProjectID, RuntimeName: "wc-two", State: "running"}, PrincipalID: r.PrincipalID, Created: true}
	if err := w.saveManagedLocked(); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	d.stopHook = func(context.Context, string) error { return errors.New("sandbox not found") }
	fresh := NewWorker(w.Root, "/never-host-exec", "template")
	// A driver whose guests die with the worker (SBX), so the restart
	// stops them itself rather than handing them to a Reconciler.
	fresh.Runtime, fresh.Gate, fresh.RuntimeDir = struct{ RuntimeDriver }{d}, w.Gate, w.RuntimeDir
	if err := fresh.initializeManaged(context.Background()); err != nil {
		t.Fatalf("a refused stop must not keep the runner down: %v", err)
	}
	fresh.mu.Lock()
	s, two := fresh.managed.Sandboxes[r.SandboxID], fresh.managed.Sandboxes["sandbox-two"]
	fresh.mu.Unlock()
	if s.Creating || s.Created || s.State != "stopped" {
		t.Fatalf("interrupted creation not reset: creating=%v created=%v state=%s", s.Creating, s.Created, s.State)
	}
	if two.State != "error" || !two.Created {
		t.Fatalf("a created sandbox whose stop is refused must be marked failed, got state=%s created=%v", two.State, two.Created)
	}
	removed := false
	d.mu.Lock()
	for _, c := range d.calls {
		if c == "remove:"+interrupted {
			removed = true
		}
	}
	d.mu.Unlock()
	if !removed {
		t.Fatal("what the runtime made of the interrupted name must be removed", d.calls)
	}
	// The next run creates the sandbox afresh rather than resuming a guest
	// that is not there.
	d.stopHook = nil
	r.RunID = "run-two"
	prepareFixture(t, fresh, r)
	created := false
	for _, name := range d.created() {
		if name == interrupted {
			created = true
		}
	}
	if !created {
		t.Fatal("the reset sandbox was not created afresh", d.created())
	}
}

// savedManaged is the inventory as the database holds it, loaded by a
// worker of its own.
func savedManaged(t *testing.T, w *Worker) *managedState {
	t.Helper()
	other := NewWorker(w.Root, w.Executable, w.Template)
	other.managed = newManagedState()
	if err := other.loadManagedLocked(); err != nil {
		t.Fatal(err)
	}
	other.closeStore()
	return other.managed
}

// A full inventory (Retained sandboxes) makes room for a new environment by
// retiring the oldest stopped sandbox that no chat is bound to and no run
// holds: its runtime is removed and its rows go. Bound or running sandboxes
// are never retired; when nothing else is left the bind is refused.
func TestFullInventoryRetiresTheOldestUnboundStoppedSandbox(t *testing.T) {
	d := &testRuntime{servers: map[int]*http.Server{}}
	w := NewWorker(t.TempDir(), "/never-host-exec", "template")
	w.Runtime, w.Gate, w.RuntimeDir = d, &testGate{}, "/fake-pinned-linux-bundle"
	w.Retained = 3
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	clock := base
	w.Now = func() time.Time { return clock }
	bind := func(chat, sandbox string) error {
		_, err := w.dispatch(context.Background(), Request{Version: 2, Operation: "bind-chat", ProjectID: "project-one", ChatID: chat, SandboxID: sandbox, PrincipalID: "owner"})
		return err
	}
	for i, id := range []string{"sb-old", "sb-mid", "sb-new"} {
		clock = base.Add(time.Duration(i) * time.Hour)
		if err := bind("chat-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	for _, id := range []string{"sb-old", "sb-mid", "sb-new"} {
		w.managed.Sandboxes[id].Created = true
	}
	// sb-old and sb-mid lost their chats (deleted from the chat service);
	// sb-new keeps its binding. sb-mid is the older of the two orphans.
	w.managed.Sandboxes["sb-mid"].LastActivity = base.Add(-time.Hour)
	for _, chat := range []string{"chat-sb-old", "chat-sb-mid"} {
		delete(w.managed.Chats, chat)
	}
	if err := w.saveManagedLocked(); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()

	if err := bind("chat-four", "sb-four"); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	_, midKept := w.managed.Sandboxes["sb-mid"]
	_, oldKept := w.managed.Sandboxes["sb-old"]
	_, fourKept := w.managed.Sandboxes["sb-four"]
	count := len(w.managed.Sandboxes)
	w.mu.Unlock()
	if midKept || !oldKept || !fourKept || count != 3 {
		t.Fatalf("expected sb-mid retired for sb-four: mid=%v old=%v four=%v count=%d", midKept, oldKept, fourKept, count)
	}
	if !slices.Contains(d.calls, "remove:"+RuntimeName("", "sb-mid")) || slices.Contains(d.calls, "remove:"+RuntimeName("", "sb-old")) {
		t.Fatalf("runtime calls: %v", d.calls)
	}
	// The retirement reached the store: a restart does not bring sb-mid back.
	saved := savedManaged(t, w)
	if _, ok := saved.Sandboxes["sb-mid"]; ok {
		t.Fatal("retired sandbox still in the inventory store")
	}

	// A running orphan is not retirable: sb-old is the only candidate now;
	// under a run it stays and the bind is refused.
	w.mu.Lock()
	w.managed.Sandboxes["sb-old"].State = "running"
	w.mu.Unlock()
	err := bind("chat-five", "sb-five")
	if err == nil || !strings.Contains(err.Error(), "3 retained sandboxes") {
		t.Fatalf("bind with every sandbox bound or running: %v", err)
	}
	w.mu.Lock()
	_, fiveKept := w.managed.Sandboxes["sb-five"]
	w.mu.Unlock()
	if fiveKept {
		t.Fatal("refused bind registered a sandbox")
	}
	// Stopped again, it makes room.
	w.mu.Lock()
	w.managed.Sandboxes["sb-old"].State = "stopped"
	w.mu.Unlock()
	if err = bind("chat-five", "sb-five"); err != nil {
		t.Fatal(err)
	}
	// Rebinding an existing chat never counts against the cap.
	if err = bind("chat-five", "sb-five"); err != nil {
		t.Fatal(err)
	}
}
