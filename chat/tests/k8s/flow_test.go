//go:build k8s

package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
)

// TestKubernetes is the suite: the owner-facing flows first (a Codex turn,
// a Claude turn, a preview, stop and resume), then the adversarial rows
// from inside the sandbox pods, then the policy restart. Subtests share
// the harness and create what they need on first use, so any of them can
// run alone (-run TestKubernetes/Adversarial); everything created is
// deleted when the suite ends.
func TestKubernetes(t *testing.T) {
	h := newHarness(t)
	t.Run("CodexTurn", func(t *testing.T) { h.agentTurn(t, "codex") })
	t.Run("ClaudeTurn", func(t *testing.T) { h.agentTurn(t, "claude") })
	t.Run("Preview", h.preview)
	t.Run("StopResume", h.stopResume)
	t.Run("Adversarial", h.adversarial)
	t.Run("PolicyRestart", h.policyRestart)
	h.report(t)
}

// agentTurn runs one turn on a fresh chat of the provider and checks that
// the answer came from the guest account in a pod of the configured tier.
func (h *harness) agentTurn(t *testing.T, provider string) {
	c := h.chat(t, provider, provider)
	answer := h.turn(t, c, "Run the shell command `id -u; uname -r` and reply with its exact output, nothing else.", 4*time.Minute)
	pod := h.pod(t, c)
	class := runtimeClassOf(pod)
	if class != h.cfg.Kubernetes.RuntimeClass {
		t.Errorf("pod %s runs under RuntimeClass %q, the release configures %q", pod.Metadata.Name, class, h.cfg.Kubernetes.RuntimeClass)
	}
	if got := pod.Status.ContainerStatuses; len(got) != 1 || !strings.HasSuffix(got[0].ImageID, h.cfg.Kubernetes.GuestImageDigest) {
		t.Errorf("pod %s image is not the pinned digest: %+v", pod.Metadata.Name, got)
	}
	if !strings.Contains(answer, "1000") {
		t.Errorf("%s did not report uid 1000: %q", provider, clip(answer, 300))
	}
	switch h.cfg.Kubernetes.Tier {
	case "gvisor":
		if !strings.Contains(strings.ToLower(answer), "gvisor") {
			t.Errorf("%s did not report a gVisor kernel: %q", provider, clip(answer, 300))
		}
	default:
		if strings.Contains(strings.ToLower(answer), "gvisor") {
			t.Errorf("%s reported a gVisor kernel under tier %s: %q", provider, h.cfg.Kubernetes.Tier, clip(answer, 300))
		}
	}
	t.Logf("%s answered from pod %s (RuntimeClass %s, tier %s, %s)", provider, pod.Metadata.Name, class, h.cfg.Kubernetes.Tier, pod.Status.PodIP)
}

func marker(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// preview has the Codex agent serve a page from the workspace and publish
// it with preview_attach; the suite approves the binding as the owner
// would and fetches the page through the edge's preview host with the
// owner's browser session.
func (h *harness) preview(t *testing.T) {
	c := h.chat(t, "codex", "codex")
	m := marker("k8s-suite-preview")
	h.turn(t, c, "Create the directory /home/agent/workspace/preview containing an index.html whose entire content is the text `"+m+"`. "+
		"Then start a detached web server on 0.0.0.0 port 8080 serving that directory, for example: `cd /home/agent/workspace/preview && nohup python3 -m http.server 8080 --bind 0.0.0.0 >/tmp/http.log 2>&1 &`. "+
		"Check with curl that http://127.0.0.1:8080/ answers. Then call the preview_attach tool with port 8080, path \"/\" and title \"k8s suite preview\". "+
		"Reply with the preview URL the tool returned, or the tool's error.", 5*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The binding the approval created, as the web UI lists it.
	var port *struct {
		URL, State string
		Port       int
	}
	deadline := time.Now().Add(30 * time.Second)
	for port == nil {
		s, err := h.state(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range s.Ports {
			if p.ChatID == c.id && p.SandboxID == c.sandboxID && p.Port == 8080 && p.State == "approved" {
				port = &struct {
					URL, State string
					Port       int
				}{p.URL, p.State, p.Port}
			}
		}
		if port == nil && time.Now().After(deadline) {
			t.Fatalf("no approved binding for port 8080 of %q; the agent did not call preview_attach or the approval failed (ports: %+v)", c.title, s.Ports)
		}
		if port == nil {
			time.Sleep(time.Second)
		}
	}
	u, err := url.Parse(port.URL)
	if err != nil {
		t.Fatal(err)
	}
	edge, _ := url.Parse(h.s.edgeURL)
	if !strings.HasSuffix(u.Hostname(), ".localhost") || u.Port() != edge.Port() {
		t.Errorf("preview URL %s is not a *.localhost host on the edge port %s", port.URL, edge.Port())
	}
	t.Logf("binding approved: %s", port.URL)
	// The runner's availability check may lag the approval by a moment, and
	// the edge only serves a binding it has seen in its 1 s port refresh.
	var status int
	var body, landed string
	for i := 0; i < 20; i++ {
		status, body, landed = h.fetchPreview(t, port.URL)
		if status == 200 && strings.Contains(body, m) {
			break
		}
		t.Logf("preview fetch %d: status %d, landed on %s (marker present: %v)", i+1, status, landed, strings.Contains(body, m))
		time.Sleep(2 * time.Second)
	}
	if status != 200 || !strings.Contains(body, m) {
		t.Fatalf("preview %s answered %d (final URL %s) without the marker %s: %q", port.URL, status, landed, m, clip(body, 300))
	}
	t.Logf("preview %s served the page from pod %s through the runner and the edge (%d bytes)", port.URL, h.pod(t, c).Metadata.Name, len(body))
	// The preview hostname is only reachable with the owner's session.
	anon := &http.Client{Timeout: 30 * time.Second, Transport: h.web.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := anon.Get(port.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), h.s.edgeURL+"/auth/preview?") {
		t.Errorf("an anonymous request to %s answered %d %s, expected the sign-in redirect", port.URL, res.StatusCode, res.Header.Get("Location"))
	}
}

// fetchPreview opens a preview URL as the owner's browser does: the API
// calls established the owner cookie on the edge origin, the preview host
// redirects through /auth/preview and back with its own session cookie. It
// returns the status, the body and the URL the redirect chain landed on
// (the preview host on success, the app origin when the owner session was
// not honoured).
func (h *harness) fetchPreview(t *testing.T, raw string) (int, string, string) {
	t.Helper()
	res, err := h.web.Get(raw)
	if err != nil {
		t.Fatalf("GET %s: %v", raw, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	landed := raw
	if res.Request != nil && res.Request.URL != nil {
		landed = res.Request.URL.String()
	}
	return res.StatusCode, string(body), landed
}

// stopResume stops the workspace through the API (the pod goes, the claim
// stays) and resumes it with the next turn, which must find the file the
// previous generation wrote.
func (h *harness) stopResume(t *testing.T) {
	c := h.chat(t, "codex", "codex")
	m := marker("k8s-suite-resume")
	h.turn(t, c, "Write exactly the text `"+m+"` to the file /home/agent/workspace/k8s-suite-marker.txt and reply done.", 4*time.Minute)
	before := h.pod(t, c)
	if got := h.exec(t, before.Metadata.Name, "cat /home/agent/workspace/k8s-suite-marker.txt"); !strings.Contains(got.Stdout, m) {
		t.Fatalf("the marker file was not written: %q", got.Stdout)
	}
	if err := h.stopWorkspace(c.sandboxID, before.Metadata.Name); err != nil {
		t.Fatalf("stopping workspace %q: %v", c.title, err)
	}
	if e := h.environment(t, c); e.Runtime == nil || e.Runtime.State != "stopped" {
		state := "<none>"
		if e.Runtime != nil {
			state = e.Runtime.State
		}
		t.Fatalf("workspace %q is %s after the stop, not stopped", c.title, state)
	}
	t.Logf("workspace %q stopped: pod %s (uid %s) is gone", c.title, before.Metadata.Name, before.Metadata.UID)
	answer := h.turn(t, c, "Reply with the exact content of the file /home/agent/workspace/k8s-suite-marker.txt and nothing else.", 4*time.Minute)
	if !strings.Contains(answer, m) {
		t.Errorf("after the resume the agent did not read back %s: %q", m, clip(answer, 300))
	}
	after := h.pod(t, c)
	if after.Metadata.Name != before.Metadata.Name {
		t.Errorf("the resumed pod is %s, the stopped one was %s (the runtime name should be stable)", after.Metadata.Name, before.Metadata.Name)
	}
	if after.Metadata.UID == before.Metadata.UID {
		t.Errorf("the resumed pod has the same UID %s as the stopped one; the stop did not delete the pod", after.Metadata.UID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var claim kube.PersistentVolumeClaim
	if err := h.kube.Get(ctx, kube.PersistentVolumeClaims, h.s.sandboxNS, after.Metadata.Name, &claim); err != nil {
		t.Errorf("workspace claim %s: %v", after.Metadata.Name, err)
	}
	t.Logf("workspace %q resumed as pod %s (uid %s, generation %s) on claim %s; the file survived", c.title, after.Metadata.Name, after.Metadata.UID, after.Metadata.Annotations["warden.monaddle.com/generation"], claim.Metadata.UID)
}

func describePod(p *kube.Pod) string {
	return fmt.Sprintf("%s (uid %s, ip %s, RuntimeClass %s, egress=%q)", p.Metadata.Name, p.Metadata.UID, p.Status.PodIP, runtimeClassOf(p), p.Metadata.Labels[labelEgress])
}
