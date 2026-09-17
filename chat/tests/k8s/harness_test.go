//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/tui"
)

// settings come from the environment (see doc.go).
type settings struct {
	kubeconfig string
	edgeURL    string
	namespace  string
	sandboxNS  string
}

func loadSettings(t *testing.T) settings {
	t.Helper()
	s := settings{
		kubeconfig: os.Getenv("WARDEN_K8S_KUBECONFIG"),
		edgeURL:    env("WARDEN_K8S_EDGE_URL", "http://127.0.0.1:28781"),
		namespace:  env("WARDEN_K8S_NAMESPACE", "warden"),
		sandboxNS:  env("WARDEN_K8S_SANDBOX_NAMESPACE", "warden-sandboxes"),
	}
	if s.kubeconfig == "" {
		t.Skip("WARDEN_K8S_KUBECONFIG unset: the Kubernetes end-to-end suite needs a running release (scripts/k8s-dev.sh up && build-images && deploy)")
	}
	s.edgeURL = strings.TrimRight(s.edgeURL, "/")
	return s
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// The resources the kube client does not name itself.
var (
	deployments = kube.Resource{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment"}
	services    = kube.Resource{Group: "", Version: "v1", Resource: "services", Kind: "Service"}
)

// Labels the chart and the runner put on pods.
const (
	labelComponent = "warden.monaddle.com/component"
	labelSandbox   = "warden.monaddle.com/sandbox"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelEgress    = "warden.monaddle.com/egress"
	egressGateway  = "gateway"
	guestContainer = "guest"
	// managedBySuite marks the pods this suite creates itself, distinct
	// from the runner's (warden-runner) and the policy service's canaries.
	managedBySuite = "warden-k8s-suite"
)

// wardenConfig is the part of the rendered warden.json the suite reads.
type wardenConfig struct {
	Kubernetes struct {
		Namespace        string `json:"namespace"`
		RuntimeClass     string `json:"runtimeClass"`
		Tier             string `json:"tier"`
		GuestImage       string `json:"guestImage"`
		GuestImageDigest string `json:"guestImageDigest"`
		GatewayService   string `json:"gatewayService"`
		GatewayPort      int    `json:"gatewayPort"`
		TrustConfigMap   string `json:"trustConfigMap"`
	} `json:"kubernetes"`
	Sandboxes struct {
		MaxRunning int `json:"maxRunning"`
	} `json:"sandboxes"`
}

// environment mirrors the chat service's GET environments answer (the
// owner's "workspace"), as far as the suite reads it.
type environment struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Runtime *struct {
		RuntimeName string `json:"runtimeName"`
		State       string `json:"state"`
		Generation  string `json:"generation"`
	} `json:"runtime"`
	Chats []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"chats"`
	Ports    []tui.Port `json:"ports"`
	Deleted  bool       `json:"deleted"`
	Archived bool       `json:"archived"`
}

// suiteChat is one chat the suite created; its sandbox is the workspace
// (environment) the chat runs in.
type suiteChat struct {
	key       string
	provider  string
	id        string
	sandboxID string
	title     string
}

// rowResult records one adversarial row with the tier it ran under.
type rowResult struct {
	Name         string
	Tier         string
	RuntimeClass string
	Pod          string
	Passed       bool
	Detail       string
}

// harness holds the clients and everything the suite created.
type harness struct {
	t        *testing.T
	s        settings
	kube     *kube.Client
	cfg      wardenConfig
	token    string
	edgeHost string
	stamp    string
	jar      http.CookieJar
	// api reaches the chat API through the edge with the owner capability;
	// web is the browser stand-in for previews (cookies, *.localhost).
	api *http.Client
	web *http.Client

	mu    sync.Mutex
	chats map[string]*suiteChat
	pods  []string
	rows  []rowResult
	// blocked is set once a wait for capacity has failed, so later waits
	// fail at once instead of each spending the full bound.
	blocked string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := loadSettings(t)
	cfg, err := kube.LoadKubeconfig(s.kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	client, err := kube.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	edgeURL, err := url.Parse(s.edgeURL)
	if err != nil {
		t.Fatalf("WARDEN_K8S_EDGE_URL %q: %v", s.edgeURL, err)
	}
	h := &harness{t: t, s: s, kube: client, jar: jar, edgeHost: edgeURL.Hostname(), stamp: time.Now().Format("15:04:05"), chats: map[string]*suiteChat{}}
	h.api = &http.Client{Timeout: 90 * time.Second, Jar: jar, Transport: &http.Transport{Proxy: nil}}
	// The browser stand-in follows the preview sign-in redirects with the
	// preview cookies (jar) and presents the owner bearer on the edge's
	// application origin, which authorizePreview accepts as the owner
	// (SessionRef) the same as the browser's owner cookie. It never sends
	// the bearer to a preview host (a guest-served origin).
	h.web = &http.Client{Timeout: 60 * time.Second, Jar: jar, Transport: &bearerToHost{host: edgeURL.Hostname(), token: func() string { return h.token }, base: &http.Transport{Proxy: nil, DialContext: dialLocalhost}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The release's own configuration says which namespace, RuntimeClass,
	// image and gateway to expect.
	var cm kube.ConfigMap
	if err := client.Get(ctx, kube.ConfigMaps, s.namespace, "warden-config", &cm); err != nil {
		t.Fatalf("the release's warden-config ConfigMap in %s: %v", s.namespace, err)
	}
	if err := json.Unmarshal([]byte(cm.Data["warden.json"]), &h.cfg); err != nil {
		t.Fatalf("warden.json in the ConfigMap: %v", err)
	}
	if h.cfg.Kubernetes.Namespace != "" && h.cfg.Kubernetes.Namespace != s.sandboxNS {
		t.Fatalf("the release's sandbox namespace is %s, WARDEN_K8S_SANDBOX_NAMESPACE says %s", h.cfg.Kubernetes.Namespace, s.sandboxNS)
	}
	if h.cfg.Kubernetes.RuntimeClass == "" || h.cfg.Kubernetes.GuestImageDigest == "" {
		t.Fatalf("warden.json lacks kubernetes.runtimeClass or guestImageDigest: %+v", h.cfg.Kubernetes)
	}
	if h.cfg.Sandboxes.MaxRunning <= 0 {
		h.cfg.Sandboxes.MaxRunning = 2
	}
	// The owner capability: the edge mints it at every start and keeps it
	// in its endpoint file (docs/warden-kubernetes.md, "First login").
	edge := h.componentPod(t, "edge")
	res, err := h.execIn(ctx, s.namespace, edge.Metadata.Name, "edge", "cat /var/lib/warden/edge/endpoint.json")
	if err != nil || res.Code != 0 {
		t.Fatalf("reading the edge's endpoint file: %v (exit %d, %s)", err, res.Code, res.Stderr)
	}
	var endpoint struct{ URL, Token string }
	if err := json.Unmarshal([]byte(res.Stdout), &endpoint); err != nil || len(endpoint.Token) < 32 {
		t.Fatalf("the edge's endpoint file is not {url,token}: %q", res.Stdout)
	}
	h.token = endpoint.Token
	t.Logf("release %s: tier %s, RuntimeClass %s, image %s@%s, gateway %s:%d, maxRunning %d; edge %s (its endpoint says %s)",
		s.namespace, h.cfg.Kubernetes.Tier, h.cfg.Kubernetes.RuntimeClass, h.cfg.Kubernetes.GuestImage, h.cfg.Kubernetes.GuestImageDigest,
		h.cfg.Kubernetes.GatewayService, h.cfg.Kubernetes.GatewayPort, h.cfg.Sandboxes.MaxRunning, s.edgeURL, endpoint.URL)
	if _, err := h.state(ctx); err != nil {
		t.Fatalf("the chat API through the edge at %s: %v", s.edgeURL, err)
	}
	t.Cleanup(h.cleanup)
	return h
}

// bearerToHost adds the owner bearer to requests whose host is the edge's
// application origin, and nothing to preview hosts. It makes the preview
// sign-in recognise the owner without depending on a cookie session.
type bearerToHost struct {
	host  string
	token func() string
	base  http.RoundTripper
}

func (b *bearerToHost) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == b.host {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token())
	}
	return b.base.RoundTrip(r)
}

// dialLocalhost resolves every *.localhost name to the loopback address,
// as browsers do for preview hosts, and dials everything else normally.
func dialLocalhost(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err == nil && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, address)
}

// The chat API through the edge, with the owner capability as bearer.

func (h *harness) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.s.edgeURL+"/api/"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.api.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (h *harness) state(ctx context.Context) (*tui.State, error) {
	var s tui.State
	if err := h.call(ctx, "GET", "state", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (h *harness) environments(ctx context.Context) ([]environment, error) {
	var out []environment
	if err := h.call(ctx, "GET", "environments", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *harness) post(ctx context.Context, path string, body map[string]any) error {
	if body == nil {
		body = map[string]any{}
	}
	return h.call(ctx, "POST", path, body, nil)
}

// chat returns the suite's chat under key, creating it (with its own
// workspace) on first use. The sandbox itself starts at the first turn.
func (h *harness) chat(t *testing.T, key, provider string) *suiteChat {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.chats[key]; c != nil {
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	title := fmt.Sprintf("k8s suite %s %s", key, h.stamp)
	var res struct {
		ID string `json:"id"`
	}
	if err := h.call(ctx, "POST", "chats", map[string]any{"title": title, "provider": provider, "model": "", "sandboxID": ""}, &res); err != nil {
		t.Fatalf("creating chat %q: %v", title, err)
	}
	s, err := h.state(ctx)
	if err != nil {
		t.Fatal(err)
	}
	created := s.Chat(res.ID)
	if created == nil || created.SandboxID == "" {
		t.Fatalf("chat %s not in the state after creation", res.ID)
	}
	c := &suiteChat{key: key, provider: provider, id: res.ID, sandboxID: created.SandboxID, title: title}
	h.chats[key] = c
	t.Logf("created chat %q (%s) on workspace %s", title, c.id, c.sandboxID)
	return c
}

func (h *harness) ownsSandbox(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.chats {
		if c.sandboxID == id {
			return true
		}
	}
	return false
}

// awaitCapacity makes room for c's sandbox to run, failing the test when it
// cannot within the deadline. It is the fatal wrapper over tryCapacity.
func (h *harness) awaitCapacity(t *testing.T, c *suiteChat, keep ...*suiteChat) {
	t.Helper()
	if ok, reason := h.tryCapacity(t, c, 10*time.Minute, keep...); !ok {
		t.Fatalf("no sandbox capacity for %q: %s", c.title, reason)
	}
}

// tryCapacity makes room for c's sandbox: sandboxes.maxRunning bounds the
// resident sandboxes across everyone on the release, so the suite stops its
// own idle workspaces first (all but c and keep), never anyone else's. When
// other people's workspaces hold every slot it waits up to timeout and then
// returns false with a reason naming them (a published preview keeps a
// workspace resident, so such a wait can only end when that preview is
// revoked). Once any wait has run out, later waits return at once with the
// same reason, so a permanently pinned slot fails fast rather than
// timeout-per-row.
func (h *harness) tryCapacity(t *testing.T, c *suiteChat, timeout time.Duration, keep ...*suiteChat) (bool, string) {
	t.Helper()
	kept := map[string]bool{c.sandboxID: true}
	for _, k := range keep {
		kept[k.sandboxID] = true
	}
	deadline := time.Now().Add(timeout)
	warned := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		envs, err := h.environments(ctx)
		cancel()
		if err != nil {
			return false, err.Error()
		}
		running, others := 0, []string{}
		var victim *environment
		for i := range envs {
			e := &envs[i]
			if e.Runtime == nil || e.Runtime.State != "running" {
				continue
			}
			if e.ID == c.sandboxID {
				return true, "" // already running; needs no slot
			}
			running++
			switch {
			case !h.ownsSandbox(e.ID):
				others = append(others, e.Name)
			case !kept[e.ID] && victim == nil:
				victim = e
			}
		}
		if running < h.cfg.Sandboxes.MaxRunning {
			return true, ""
		}
		if victim != nil {
			t.Logf("making room for %q: stopping the suite's own idle workspace %q", c.title, victim.Name)
			if err := h.stopWorkspace(victim.ID, victim.Runtime.RuntimeName); err != nil {
				return false, fmt.Sprintf("stopping the suite's workspace %q: %v", victim.Name, err)
			}
			continue
		}
		// A slot is needed but every one is held by another party's
		// workspace. Fail fast if a previous wait already timed out on the
		// same condition (the sticky reason), so one pinned slot does not
		// cost a full timeout per row.
		h.mu.Lock()
		sticky := h.blocked
		h.mu.Unlock()
		if sticky != "" {
			return false, sticky
		}
		if time.Now().After(deadline) {
			reason := fmt.Sprintf("%d sandboxes running at sandboxes.maxRunning %d, all held by other workspaces %v; a published preview keeps a workspace resident, so revoke it, stop the workspace, or raise maxRunning", running, h.cfg.Sandboxes.MaxRunning, others)
			h.mu.Lock()
			h.blocked = reason
			h.mu.Unlock()
			return false, reason
		}
		if !warned {
			t.Logf("waiting for sandbox capacity for %q (%d running of %d; other workspaces running: %v)", c.title, running, h.cfg.Sandboxes.MaxRunning, others)
			warned = true
		}
		time.Sleep(10 * time.Second)
	}
}

// secondSandbox brings up a second suite sandbox for the cross-sandbox
// rows, keeping the workspaces in keep resident. It waits a short while for
// a slot (the single-sandbox rows already ran, so the cluster is otherwise
// as free as it gets); on failure it returns the reason instead of aborting
// so the row can be recorded, never skipped in silence.
func (h *harness) secondSandbox(t *testing.T, key, provider string, keep *suiteChat) (*suiteChat, string) {
	c := h.chat(t, key, provider)
	if ok, reason := h.tryCapacity(t, c, 90*time.Second, keep); !ok {
		return c, reason
	}
	h.turn(t, c, "Reply with the single word ready.", 4*time.Minute, keep)
	return c, ""
}

// stopWorkspace stops one of the suite's own workspaces through the API
// and waits for its pod to go. Right after a turn the worker can still see
// the resident session's run as active for a moment ("sandbox has an
// active run"), so the stop is retried, as a client would; every retry
// re-runs StopEnvironment, which ends the resident session first.
func (h *harness) stopWorkspace(sandboxID, podName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var err error
	for i := 0; i < 30; i++ {
		if err = h.post(ctx, "environments/"+sandboxID+"/stop", nil); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "active run") || ctx.Err() != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return err
	}
	if podName != "" {
		if err := h.awaitPodGone(ctx, podName); err != nil {
			return err
		}
	}
	for {
		envs, err := h.environments(ctx)
		if err != nil {
			return err
		}
		stopped := true
		for _, e := range envs {
			if e.ID == sandboxID && e.Runtime != nil && e.Runtime.State == "running" {
				stopped = false
			}
		}
		if stopped {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("workspace %s still running", sandboxID)
		}
		time.Sleep(time.Second)
	}
}

// ensureRunning brings c's sandbox up (a short turn resumes a stopped
// workspace) without stopping the workspaces in keep.
func (h *harness) ensureRunning(t *testing.T, c *suiteChat, keep ...*suiteChat) {
	t.Helper()
	if e := h.environment(t, c); e.Runtime != nil && e.Runtime.State == "running" {
		return
	}
	h.turn(t, c, "Reply with the single word ready.", 4*time.Minute, keep...)
}

// turn sends text to the chat, follows it to idle and fails the test on any
// error. Most callers want this.
func (h *harness) turn(t *testing.T, c *suiteChat, text string, timeout time.Duration, keep ...*suiteChat) string {
	t.Helper()
	answer, err := h.tryTurn(t, c, text, timeout, keep...)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// tryTurn sends text to the chat and follows the transcript until the agent
// is idle again, answering every approval the agent raises (tool
// permissions, port bindings, questions) in the owner's favour, as the
// suite is the owner. It returns the assistant's new text, or an error when
// the turn fails or does not settle (so a caller can retry, e.g. after a
// policy restart where the binding re-establishes over a few seconds).
func (h *harness) tryTurn(t *testing.T, c *suiteChat, text string, timeout time.Duration, keep ...*suiteChat) (string, error) {
	t.Helper()
	h.awaitCapacity(t, c, keep...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	seen := map[string]bool{}
	if s, err := h.state(ctx); err == nil {
		if ch := s.Chat(c.id); ch != nil {
			for _, e := range ch.Conversation.Entries {
				seen[e.ID] = true
			}
		}
	}
	started := time.Now()
	if err := h.post(ctx, "chats/"+c.id+"/message", map[string]any{"text": text, "id": tui.NewMessageID()}); err != nil {
		return "", fmt.Errorf("sending to %q: %w", c.title, err)
	}
	answered := map[string]bool{}
	settled, replies := 0, 0
	var out []string
	for {
		s, err := h.state(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("turn on %q timed out after %s: %w", c.title, timeout, err)
			}
			time.Sleep(time.Second)
			continue
		}
		ch := s.Chat(c.id)
		if ch == nil {
			return "", fmt.Errorf("chat %s vanished", c.id)
		}
		streaming := false
		for _, e := range ch.Conversation.Entries {
			if e.IsStreaming {
				streaming = true
				continue
			}
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			switch e.Role {
			case "assistant":
				replies++
				out = append(out, e.Text)
				t.Logf("  %s: %s", c.provider, clip(e.Text, 400))
			case "activity":
				t.Logf("  · %s", clip(e.Text, 200))
			case "system":
				t.Logf("  ! %s", clip(e.Text, 400))
			}
		}
		for _, a := range ch.Pending() {
			if answered[a.ID] {
				continue
			}
			answered[a.ID] = true
			answers := map[string][]string{}
			for _, q := range a.Questions() {
				answers[q.ID] = []string{"yes"}
			}
			if err := h.post(ctx, "chats/"+c.id+"/approvals/"+a.ID, map[string]any{"allow": true, "answers": answers}); err != nil {
				t.Logf("  approval %s (%s): %v", a.ID, a.Method, err)
			} else {
				t.Logf("  approved %s", a.Method)
			}
		}
		active := ch.Status == "running" || ch.Status == "queued" || ch.Status == "stopping" || streaming
		if active {
			settled = 0
		} else {
			settled++
		}
		if ch.Status == "failed" {
			return "", fmt.Errorf("turn on %q failed: %s", c.title, ch.Error)
		}
		if settled >= 3 && replies > 0 {
			t.Logf("turn on %q settled in %s (status %s)", c.title, time.Since(started).Round(time.Second), ch.Status)
			return strings.Join(out, "\n"), nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("turn on %q did not settle within %s (status %s, %d replies)", c.title, timeout, ch.Status, replies)
		}
		time.Sleep(time.Second)
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// environment returns the chat's workspace as the API reports it.
func (h *harness) environment(t *testing.T, c *suiteChat) environment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	envs, err := h.environments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range envs {
		if e.ID == c.sandboxID {
			return e
		}
	}
	t.Fatalf("workspace %s of %q not listed", c.sandboxID, c.title)
	return environment{}
}

// pod is the chat's running sandbox pod, read back from the API server.
func (h *harness) pod(t *testing.T, c *suiteChat) *kube.Pod {
	t.Helper()
	e := h.environment(t, c)
	if e.Runtime == nil || e.Runtime.RuntimeName == "" {
		t.Fatalf("workspace %q has no runtime yet", c.title)
	}
	if e.Runtime.State != "running" {
		t.Fatalf("workspace %q is %s, not running", c.title, e.Runtime.State)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pod kube.Pod
	if err := h.kube.Get(ctx, kube.Pods, h.s.sandboxNS, e.Runtime.RuntimeName, &pod); err != nil {
		t.Fatalf("pod %s of %q: %v", e.Runtime.RuntimeName, c.title, err)
	}
	if pod.Status.PodIP == "" || pod.Status.Phase != "Running" {
		t.Fatalf("pod %s is %s with IP %q", pod.Metadata.Name, pod.Status.Phase, pod.Status.PodIP)
	}
	return &pod
}

func runtimeClassOf(pod *kube.Pod) string {
	if pod.Spec.RuntimeClassName == nil {
		return ""
	}
	return *pod.Spec.RuntimeClassName
}

// componentPod finds the release's pod for one component (edge, policy,
// runner, chat).
func (h *harness) componentPod(t *testing.T, component string) *kube.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list kube.List[kube.Pod]
	if err := h.kube.List(ctx, kube.Pods, h.s.namespace, kube.ListOptions{LabelSelector: labelComponent + "=" + component}, &list); err != nil {
		t.Fatalf("listing %s pods: %v", component, err)
	}
	var running []kube.Pod
	for _, p := range list.Items {
		if p.Status.Phase == "Running" && p.Metadata.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	if len(running) != 1 {
		t.Fatalf("expected one running %s pod in %s, found %d of %d", component, h.s.namespace, len(running), len(list.Items))
	}
	return &running[0]
}

// Exec into pods: the test's own identity (the kubeconfig's), never the
// verifier's.

type execResult struct {
	Stdout, Stderr string
	Code           int
}

func (h *harness) execIn(ctx context.Context, namespace, pod, container, script string) (execResult, error) {
	session, err := h.kube.Exec(ctx, namespace, pod, container, []string{"/bin/sh", "-c", script}, kube.ExecOptions{})
	if err != nil {
		return execResult{Code: -1}, err
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&stdout, session.Stdout()); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&stderr, session.Stderr()); done <- struct{}{} }()
	code, err := session.Wait()
	<-done
	<-done
	return execResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}, err
}

// exec runs a shell script in a sandbox pod's guest container and returns
// its output and exit code; a transport failure is fatal, a non-zero exit
// is the caller's to judge.
func (h *harness) exec(t *testing.T, pod, script string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := h.execIn(ctx, h.s.sandboxNS, pod, guestContainer, script)
	if err != nil {
		t.Fatalf("exec in %s: %v (stdout %q, stderr %q)", pod, err, clip(res.Stdout, 500), clip(res.Stderr, 500))
	}
	return res
}

// proxyURL runs a fresh turn on c and returns the binding's proxy URL
// (http://bindingID:capability@host:port) the agent holds in its
// environment. The turn matters twice over: it re-establishes the resident
// session's lease, which the gateway serves against (a stale credential
// from an ended lease is refused, not honoured), and it guarantees an
// agent process to read HTTPS_PROXY from. The runner passes the URL to the
// agent process only, so the pod's own environment has none, which the
// direct-egress row relies on. The resident session then keeps the lease
// open for the probes that follow.
func (h *harness) proxyURL(t *testing.T, c *suiteChat, keep ...*suiteChat) *url.URL {
	t.Helper()
	const script = `for e in /proc/[0-9]*/environ; do tr '\0' '\n' < "$e" 2>/dev/null | grep '^HTTPS_PROXY='; done | sort -u | cut -d= -f2-`
	h.turn(t, c, "Reply with the single word ready.", 4*time.Minute, keep...)
	for attempt := 0; ; attempt++ {
		pod := h.pod(t, c)
		res := h.exec(t, pod.Metadata.Name, script)
		lines := strings.Fields(res.Stdout)
		if len(lines) == 1 {
			u, err := url.Parse(lines[0])
			if err != nil || u.User == nil || u.Host == "" {
				t.Fatalf("HTTPS_PROXY in %s is not a credentialed URL: %q", pod.Metadata.Name, lines[0])
			}
			if _, ok := u.User.Password(); !ok || u.User.Username() != c.sandboxID {
				t.Fatalf("HTTPS_PROXY in %s names binding %q, the workspace is %s", pod.Metadata.Name, u.User.Username(), c.sandboxID)
			}
			return u
		}
		if len(lines) > 1 {
			t.Fatalf("%d distinct HTTPS_PROXY values in %s", len(lines), pod.Metadata.Name)
		}
		if attempt >= 1 {
			t.Fatalf("no agent process with HTTPS_PROXY in %s after a turn", pod.Metadata.Name)
		}
		t.Logf("no resident agent in %s; running a turn to start one", pod.Metadata.Name)
		h.turn(t, c, "Reply with the single word ready.", 3*time.Minute, keep...)
	}
}

// curlResult is one curl probe from inside a guest: the proxy's answer to
// CONNECT, the final status, curl's exit code and its error text.
type curlResult struct {
	Connect int
	Code    int
	Exit    int
	Err     string
}

func (r curlResult) String() string {
	return fmt.Sprintf("connect=%d code=%d exit=%d %s", r.Connect, r.Code, r.Exit, clip(r.Err, 200))
}

// curl runs curl in the pod with args (shell-quoted by the caller where
// needed) and parses its trailer. -sS and a bounded time are always set.
func (h *harness) curl(t *testing.T, pod string, args string) curlResult {
	t.Helper()
	script := `curl -sS -o /dev/null --max-time 20 -w '\nWARDEN-CURL connect=%{http_connect} code=%{http_code}\n' ` + args + ` 2>/tmp/curl.err; code=$?; cat /tmp/curl.err >&2; exit $code`
	res := h.exec(t, pod, script)
	r := curlResult{Exit: res.Code, Err: strings.TrimSpace(res.Stderr)}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "WARDEN-CURL ") {
			fmt.Sscanf(line, "WARDEN-CURL connect=%d code=%d", &r.Connect, &r.Code)
		}
	}
	return r
}

// record keeps a row's verdict with the tier it ran under.
func (h *harness) record(t *testing.T, name string, pod *kube.Pod, passed bool, detail string) {
	t.Helper()
	tier := h.cfg.Kubernetes.Tier
	class := ""
	podName := ""
	if pod != nil {
		class = runtimeClassOf(pod)
		podName = pod.Metadata.Name
	}
	h.mu.Lock()
	h.rows = append(h.rows, rowResult{Name: name, Tier: tier, RuntimeClass: class, Pod: podName, Passed: passed, Detail: detail})
	h.mu.Unlock()
	verdict := "PASS"
	if !passed {
		verdict = "FAIL"
	}
	t.Logf("row %-28s %s under tier %s (RuntimeClass %s, pod %s): %s", name, verdict, tier, class, podName, detail)
	if !passed {
		t.Errorf("row %s: %s", name, detail)
	}
}

// expectedRows are the adversarial rows the suite must record; one that
// never ran (its subtest was skipped or died before its verdict) is
// listed as such, so a missing row is visible rather than silent.
var expectedRows = []string{"direct-egress", "proxy-credentials", "ip-literal", "ipv6", "dns", "connect-non-http", "trust-bundle", "cluster-addresses", "other-binding-credential", "other-sandbox-preview", "unlabelled-pod", "policy-restart"}

// report prints every recorded row and names the expected ones that are
// missing.
func (h *harness) report(t *testing.T) {
	h.mu.Lock()
	rows := append([]rowResult(nil), h.rows...)
	h.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	recorded := map[string]bool{}
	var b strings.Builder
	fmt.Fprintf(&b, "\nadversarial rows (%d recorded) under tier %s:\n", len(rows), h.cfg.Kubernetes.Tier)
	for _, r := range rows {
		recorded[r.Name] = true
		verdict := "pass"
		if !r.Passed {
			verdict = "FAIL"
		}
		fmt.Fprintf(&b, "  %-28s %-4s RuntimeClass=%-10s pod=%-28s %s\n", r.Name, verdict, r.RuntimeClass, r.Pod, r.Detail)
	}
	for _, name := range expectedRows {
		if !recorded[name] {
			fmt.Fprintf(&b, "  %-28s NOT RUN\n", name)
		}
	}
	t.Log(b.String())
}

// awaitPodGone waits for a pod to leave the API server.
func (h *harness) awaitPodGone(ctx context.Context, name string) error {
	for {
		var pod kube.Pod
		err := h.kube.Get(ctx, kube.Pods, h.s.sandboxNS, name, &pod)
		if kube.IsNotFound(err) {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("pod %s still present: %v", name, ctx.Err())
		}
		time.Sleep(time.Second)
	}
}

// trackPod registers a pod the suite created for deletion at the end.
func (h *harness) trackPod(name string) {
	h.mu.Lock()
	h.pods = append(h.pods, name)
	h.mu.Unlock()
}

// cleanup deletes everything the suite created, on every path: chats are
// stopped and their workspaces deleted through the API (which removes the
// pod and the claim and archives the chats), and the suite's own pods are
// deleted. Other people's chats and namespaces are never touched.
func (h *harness) cleanup() {
	t := h.t
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	h.mu.Lock()
	chats := make([]*suiteChat, 0, len(h.chats))
	for _, c := range h.chats {
		chats = append(chats, c)
	}
	pods := append([]string(nil), h.pods...)
	h.mu.Unlock()
	sort.Slice(chats, func(i, j int) bool { return chats[i].key < chats[j].key })
	for _, name := range pods {
		if err := h.kube.Delete(ctx, kube.Pods, h.s.sandboxNS, name, kube.DeleteOptions{GracePeriodSeconds: kube.Int64(0)}); err != nil && !kube.IsNotFound(err) {
			t.Logf("cleanup: deleting pod %s: %v", name, err)
		} else {
			t.Logf("cleanup: deleted pod %s", name)
		}
	}
	for _, c := range chats {
		if s, err := h.state(ctx); err == nil {
			if ch := s.Chat(c.id); ch != nil && (ch.Status == "running" || ch.Status == "queued") {
				if err := h.post(ctx, "chats/"+c.id+"/stop", nil); err != nil {
					t.Logf("cleanup: stopping %q: %v", c.title, err)
				}
				for i := 0; i < 60; i++ {
					if s, err := h.state(ctx); err == nil {
						if ch := s.Chat(c.id); ch == nil || (ch.Status != "running" && ch.Status != "queued" && ch.Status != "stopping") {
							break
						}
					}
					time.Sleep(time.Second)
				}
			}
		}
		// A resident agent session counts as an active run: stop first
		// (retried past the transient "active run" right after a turn),
		// which ends it and removes the pod, then delete the workspace.
		var err error
		for i := 0; i < 12; i++ {
			if err = h.post(ctx, "environments/"+c.sandboxID+"/stop", nil); err == nil || !strings.Contains(err.Error(), "active run") {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			t.Logf("cleanup: stopping workspace %s of %q: %v", c.sandboxID, c.title, err)
		}
		for i := 0; i < 12; i++ {
			if err = h.post(ctx, "environments/"+c.sandboxID+"/delete", nil); err == nil {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if err != nil {
			t.Logf("cleanup: deleting workspace %s of %q: %v (archive it from the Workspace panel)", c.sandboxID, c.title, err)
			continue
		}
		t.Logf("cleanup: deleted workspace %s of %q", c.sandboxID, c.title)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
