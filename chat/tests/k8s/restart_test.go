//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
)

// policyRestart rolls the policy Deployment while a sandbox holds a live
// binding, the row "restart worker, gateway, daemon with sessions" of the
// matrix. The invariants the threat-model doc claims are what this asserts:
// the gateway address is stable (a Service IP survives the pod roll), no
// binding widens across the restart, and leases are re-established by the
// next turn through the new policy pod.
//
// The live binding's run is fail-closed by design: when the policy service
// is down the runner's periodic Renew fails, so it ends the run and stops
// the sandbox (chat/internal/sandbox/managed.go finishManagedRun) rather
// than let a guest keep egress it can no longer verify. So the sandbox does
// not survive the restart with its binding intact; the row records the
// observed post-restart state (fail-closed stop is expected) and proves the
// binding re-establishes cleanly, at the same gateway address, on resume —
// never that the pre-restart credential still works (it must not).
//
// The other suite workspace is stopped first so the new policy pod's canary
// proof (two short-lived pods) fits the sandbox namespace's quota, which is
// sized for maxRunning sandboxes, the spares and the canaries.
func (h *harness) policyRestart(t *testing.T) {
	a := h.chat(t, "codex", "codex")
	oldProxy := h.proxyURL(t, a)
	before := h.pod(t, a)
	if before.Metadata.Labels[labelEgress] != egressGateway {
		t.Fatalf("pod %s has no egress label before the restart", before.Metadata.Name)
	}
	if got := h.gatewayAnswers(t, before.Metadata.Name, oldProxy.String()); got.Connect != 200 || got.Code != 200 {
		t.Fatalf("positive control before the restart failed: %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	h.mu.Lock()
	b := h.chats["claude"]
	h.mu.Unlock()
	if b != nil {
		if e := h.environment(t, b); e.Runtime != nil && e.Runtime.State == "running" {
			if err := h.stopWorkspace(b.sandboxID, e.Runtime.RuntimeName); err != nil {
				t.Logf("stopping %q before the restart: %v", b.title, err)
			}
		}
	}
	svcBefore := h.serviceIP(t, h.cfg.Kubernetes.GatewayService)
	oldPolicy := h.componentPod(t, "policy")
	t.Logf("restarting the policy Deployment (pod %s) with %s holding a live binding; gateway Service %s is %s", oldPolicy.Metadata.Name, before.Metadata.Name, h.cfg.Kubernetes.GatewayService, svcBefore)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339),
	}}}}})
	if err := h.kube.Patch(ctx, deployments, h.s.namespace, "warden-policy", patch, nil); err != nil {
		t.Fatalf("restarting warden-policy: %v", err)
	}
	started := time.Now()
	newPolicy := h.awaitRolledPolicy(t, ctx, oldPolicy, started)
	t.Logf("policy pod %s ready after %s", newPolicy.Metadata.Name, time.Since(started).Round(time.Second))
	svcAfter := h.serviceIP(t, h.cfg.Kubernetes.GatewayService)
	if svcAfter != svcBefore {
		t.Errorf("the gateway Service IP changed across the restart: %s -> %s", svcBefore, svcAfter)
	}
	// No binding widens: the pre-restart credential must not keep serving.
	// The fail-closed stop makes the pod gone (its exec fails), which is one
	// way the old credential stops working; if the pod is somehow still up,
	// the stale credential must be refused, never 200.
	e := h.environment(t, a)
	failedClosed := e.Runtime == nil || e.Runtime.State != "running"
	staleWorks := false
	staleDetail := "sandbox stopped (fail-closed), old credential unusable"
	if !failedClosed {
		stale := h.curl(t, before.Metadata.Name, "-x '"+oldProxy.String()+"' https://registry.npmjs.org/-/ping")
		staleWorks = stale.Connect == 200 && stale.Code == 200
		staleDetail = "sandbox still running; old credential [" + stale.String() + "]"
	} else {
		t.Logf("sandbox %s fail-closed on the restart (expected: the runner stops a guest whose egress it cannot re-verify)", a.sandboxID)
	}
	// Leases are re-established: a new turn resumes the sandbox (new
	// generation), re-registers the binding and begins a fresh lease through
	// the new policy pod.
	answer := h.turn(t, a, "Reply with the single word ready.", 5*time.Minute)
	if !strings.Contains(strings.ToLower(answer), "ready") {
		t.Errorf("the turn after the restart did not answer: %q", clip(answer, 200))
	}
	after := h.pod(t, a)
	if after.Metadata.Labels[labelEgress] != egressGateway {
		t.Errorf("the resumed sandbox pod has no egress label: %q", after.Metadata.Labels[labelEgress])
	}
	newProxy := h.proxyURL(t, a)
	fresh := h.gatewayAnswers(t, after.Metadata.Name, newProxy.String())
	// The pre-restart credential must not work from the resumed pod either.
	staleAfter := h.gatewayAnswers(t, after.Metadata.Name, oldProxy.String())
	ok := svcAfter == svcBefore && !staleWorks &&
		fresh.Connect == 200 && fresh.Code == 200 && newProxy.Host == oldProxy.Host &&
		after.Metadata.Labels[labelEgress] == egressGateway &&
		staleAfter.Connect != 200 && staleAfter.Code != 200
	detail := fmt.Sprintf("gateway %s stable across the roll; %s; policy pod %s -> %s in %s; leases re-established: %s answers at the same gateway %s [%s]; pre-restart credential from the resumed pod [%s]; credential rotated: %v",
		svcAfter, staleDetail, oldPolicy.Metadata.Name, newPolicy.Metadata.Name, time.Since(started).Round(time.Second), after.Metadata.Name, newProxy.Host, fresh, staleAfter, passwordOf(newProxy) != passwordOf(oldProxy))
	h.record(t, "policy-restart", after, ok, detail)
}

// awaitRolledPolicy waits for the old policy pod to go and a new one to be
// ready.
func (h *harness) awaitRolledPolicy(t *testing.T, ctx context.Context, oldPolicy *kube.Pod, started time.Time) *kube.Pod {
	for {
		var list kube.List[kube.Pod]
		if err := h.kube.List(ctx, kube.Pods, h.s.namespace, kube.ListOptions{LabelSelector: labelComponent + "=policy"}, &list); err != nil {
			t.Fatal(err)
		}
		var candidate *kube.Pod
		oldGone := true
		for i := range list.Items {
			p := &list.Items[i]
			if p.Metadata.UID == oldPolicy.Metadata.UID {
				oldGone = false
				continue
			}
			if p.Status.Phase == "Running" && p.Metadata.DeletionTimestamp == nil && ready(p) {
				candidate = p
			}
		}
		if oldGone && candidate != nil {
			return candidate
		}
		if ctx.Err() != nil {
			t.Fatalf("the policy Deployment did not roll within %s", time.Since(started).Round(time.Second))
		}
		time.Sleep(2 * time.Second)
	}
}

func ready(p *kube.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

// serviceIP reads a Service's ClusterIP in the release namespace.
func (h *harness) serviceIP(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var obj kube.Object
	if err := h.kube.Get(ctx, services, h.s.namespace, name, &obj); err != nil {
		t.Fatalf("Service %s: %v", name, err)
	}
	return serviceIP(obj)
}

// awaitAllIdle waits until no chat on the release is running or queued,
// the suite's or anyone else's.
func (h *harness) awaitAllIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	warned := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s, err := h.state(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var busy []string
		for _, c := range s.Chats {
			if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
				busy = append(busy, c.Title)
			}
		}
		if len(busy) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chats still running after %s: %v", timeout, busy)
		}
		if !warned {
			t.Logf("waiting for running chats to finish before restarting the policy service: %v", busy)
			warned = true
		}
		time.Sleep(5 * time.Second)
	}
}
