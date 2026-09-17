package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "warden/chat/internal/kube"
	"warden/chat/internal/policy"
)

// A compliant pod: present, identified by its workspace claim's UID, the
// pinned image, the tier, no violations, denied until labelled.
func TestFactsOfACompliantPod(t *testing.T) {
	f := cluster(t)
	seedSandbox(f, "wc-one")
	i := newInspector(t, f, nil)
	ctx := ctxT(t)
	facts, err := i.Facts(ctx, identity("wc-one"))
	if err != nil {
		t.Fatal(err)
	}
	var pvc api.PersistentVolumeClaim
	f.object(api.PersistentVolumeClaims, testNamespace, "wc-one-home", &pvc)
	if !facts.Present || facts.Identity != pvc.Metadata.UID || facts.ImageDigest != testDigest || facts.Tier != TierGVisor || len(facts.Violations) != 0 || facts.Egress != policy.EgressDenied {
		t.Fatalf("facts: %+v", facts)
	}
	// Absent runtime: not present, denied, no error.
	facts, err = i.Facts(ctx, identity("wc-none"))
	if err != nil || facts.Present || facts.Egress != policy.EgressDenied {
		t.Fatalf("absent: %+v %v", facts, err)
	}
	// The pins file records the volume and the generation's pod.
	raw, err := os.ReadFile(filepath.Join(i.o.State, pinsFile))
	if err != nil || !strings.Contains(string(raw), pvc.Metadata.UID) || !strings.Contains(string(raw), `"1"`) {
		t.Fatalf("pins: %s %v", raw, err)
	}
}

// Every structural finding of decision 2 is a violation.
func TestFactsViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *api.Pod)
		want   string
	}{
		{"other RuntimeClass", func(p *api.Pod) { p.Spec.RuntimeClassName = api.String("runc") }, "pod runs an unpinned RuntimeClass"},
		{"no RuntimeClass", func(p *api.Pod) { p.Spec.RuntimeClassName = nil }, "pod runs an unpinned RuntimeClass"},
		{"host network", func(p *api.Pod) { p.Spec.HostNetwork = true }, "pod shares a host namespace"},
		{"host PID", func(p *api.Pod) { p.Spec.HostPID = true }, "pod shares a host namespace"},
		{"privileged", func(p *api.Pod) {
			p.Spec.Containers[0].SecurityContext = &api.SecurityContext{Privileged: api.Bool(true)}
		}, "pod runs a privileged container"},
		{"hostPath", func(p *api.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, api.Volume{Name: "h", HostPath: &api.HostPathVolumeSource{Path: "/"}})
		}, "pod mounts a hostPath volume"},
		{"projected token", func(p *api.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, api.Volume{Name: "t", Projected: &api.ProjectedVolumeSource{Sources: []api.VolumeProjection{{ServiceAccountToken: &api.ServiceAccountTokenProjection{Path: "token"}}}}})
		}, "pod mounts a projected volume"},
		{"secret volume", func(p *api.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, api.Volume{Name: "s", Secret: &api.SecretVolumeSource{SecretName: "x"}})
		}, "pod mounts a Secret volume"},
		{"unknown volume type", func(p *api.Pod) { p.Spec.Volumes = append(p.Spec.Volumes, api.Volume{Name: "u"}) }, "pod mounts an unknown volume type"},
		{"other ConfigMap", func(p *api.Pod) { p.Spec.Volumes[1].ConfigMap.Name = "other" }, "pod mounts an unexpected ConfigMap"},
		{"second claim", func(p *api.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, api.Volume{Name: "x", PersistentVolumeClaim: &api.PersistentVolumeClaimVolumeSource{ClaimName: "other"}})
		}, "pod mounts extra volumes"},
		{"extra container", func(p *api.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, api.Container{Name: "side", Image: "busybox"})
		}, "pod runs extra containers"},
		{"init container", func(p *api.Pod) { p.Spec.InitContainers = []api.Container{{Name: "init", Image: "busybox"}} }, "pod runs extra containers"},
		{"token automounted", func(p *api.Pod) { p.Spec.AutomountServiceAccountToken = nil }, "pod automounts a service account token"},
		{"spec image by another digest", func(p *api.Pod) {
			p.Spec.Containers[0].Image = "warden-guest-base@sha256:" + strings.Repeat("b", 64)
		}, "pod runs an unpinned image"},
		{"running image by another digest", func(p *api.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "docker.io/library/warden-guest-base@sha256:" + strings.Repeat("b", 64)
		}, "pod runs an unpinned image"},
		{"running image unreported", func(p *api.Pod) { p.Status.ContainerStatuses = nil }, "pod image digest not reported"},
		{"tag of another repository", func(p *api.Pod) { p.Spec.Containers[0].Image = "docker.io/library/busybox:1.37" }, "pod runs an unpinned image"},
		{"labelled for another binding", func(p *api.Pod) { p.Metadata.Labels[LabelBinding] = "other" }, "pod is labelled for another binding"},
		{"unexpected egress value", func(p *api.Pod) { p.Metadata.Labels[LabelEgress] = "all" }, "pod carries an unexpected egress label"},
		{"canary label", func(p *api.Pod) { p.Metadata.Labels[LabelCanary] = CanaryDeny }, "pod is a canary"},
		{"other generation", func(p *api.Pod) { p.Metadata.Annotations[AnnotationGeneration] = "2" }, "pod belongs to another generation"},
		{"no workspace volume", func(p *api.Pod) { p.Spec.Volumes = p.Spec.Volumes[1:] }, "pod has no workspace volume"},
		{"workspace volume missing", func(p *api.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "gone" }, "pod's workspace volume does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cluster(t)
			f.seed(api.PersistentVolumeClaims, testNamespace, &api.PersistentVolumeClaim{Metadata: api.ObjectMeta{Name: "wc-one-home"}})
			pod := sandboxPod("wc-one", "wc-one-home")
			tc.mutate(pod)
			f.seed(api.Pods, testNamespace, pod)
			i := newInspector(t, f, func(o *Options) { o.GuestImage = "warden-guest-base" })
			facts, err := i.Facts(ctxT(t), identity("wc-one"))
			if err != nil {
				t.Fatal(err)
			}
			if !facts.Present || !contains(facts.Violations, tc.want) {
				t.Fatalf("got %+v, want %q", facts, tc.want)
			}
		})
	}
	// Accepted variations: a tag reference with the digest reported by the
	// running container (an imported image), the binding label matching,
	// a Pending pod without a status yet, and the trust ConfigMap.
	f := cluster(t)
	f.seed(api.PersistentVolumeClaims, testNamespace, &api.PersistentVolumeClaim{Metadata: api.ObjectMeta{Name: "wc-one-home"}})
	pod := sandboxPod("wc-one", "wc-one-home")
	pod.Spec.Containers[0].Image = "docker.io/library/warden-guest-base:dev"
	pod.Metadata.Labels[LabelBinding] = policy.BindingDigest(identity("wc-one"))
	f.seed(api.Pods, testNamespace, pod)
	i := newInspector(t, f, func(o *Options) { o.RequireBindingLabel, o.GuestImage = true, "warden-guest-base" })
	facts, err := i.Facts(ctxT(t), identity("wc-one"))
	if err != nil || len(facts.Violations) != 0 || facts.ImageDigest != testDigest {
		t.Fatalf("accepted variations: %+v %v", facts, err)
	}
	pending := sandboxPod("wc-two", "wc-two-home")
	pending.Status = api.PodStatus{Phase: "Pending"}
	f.seed(api.PersistentVolumeClaims, testNamespace, &api.PersistentVolumeClaim{Metadata: api.ObjectMeta{Name: "wc-two-home"}})
	f.seed(api.Pods, testNamespace, pending)
	facts, err = i.Facts(ctxT(t), identity("wc-two"))
	if err != nil || len(facts.Violations) != 1 || facts.Violations[0] != "pod lacks the binding label" || facts.ImageDigest != "" {
		t.Fatalf("pending pod: %+v %v", facts, err)
	}
}

// Identity: the workspace claim's UID is pinned once; a different claim
// UID under the same name is fatal; a new pod for a new generation is a
// new pin; a different pod within one generation is fatal; pins survive a
// restart.
func TestIdentityPins(t *testing.T) {
	f := cluster(t)
	seedSandbox(f, "wc-one")
	state := t.TempDir()
	i := newInspector(t, f, func(o *Options) { o.State = state })
	ctx := ctxT(t)
	first, err := i.Facts(ctx, identity("wc-one"))
	if err != nil {
		t.Fatal(err)
	}
	// Stop and resume: a new pod, generation 2, same claim.
	f.remove(api.Pods, testNamespace, "wc-one")
	pod := sandboxPod("wc-one", "wc-one-home")
	pod.Metadata.Annotations[AnnotationGeneration] = "2"
	f.seed(api.Pods, testNamespace, pod)
	gen2 := identity("wc-one")
	gen2["generation"] = "2"
	second, err := i.Facts(ctx, gen2)
	if err != nil || second.Identity != first.Identity || len(second.Violations) != 0 {
		t.Fatalf("resume: %+v %v", second, err)
	}
	// The same generation with a replaced pod is refused, also after a
	// restart of the inspector.
	f.remove(api.Pods, testNamespace, "wc-one")
	f.seed(api.Pods, testNamespace, pod)
	if _, err := i.Facts(ctx, gen2); err == nil || err.Error() != "runtime replaced within its generation" {
		t.Fatalf("replaced pod: %v", err)
	}
	restarted := newInspector(t, f, func(o *Options) { o.State = state })
	if _, err := restarted.Facts(ctx, gen2); err == nil || err.Error() != "runtime replaced within its generation" {
		t.Fatalf("replaced pod after restart: %v", err)
	}
	// A replaced workspace claim is fatal.
	f.remove(api.Pods, testNamespace, "wc-one")
	f.remove(api.PersistentVolumeClaims, testNamespace, "wc-one-home")
	seedSandbox(f, "wc-one")
	gen3 := identity("wc-one")
	gen3["generation"] = "3"
	if _, err := restarted.Facts(ctx, gen3); err == nil || !strings.Contains(err.Error(), "workspace volume replaced") {
		t.Fatalf("replaced volume: %v", err)
	}
	// A storage failure is reported and the pin is not kept.
	failed := false
	i2 := newInspector(t, f, func(o *Options) {
		o.State = filepath.Join(t.TempDir(), "missing")
		o.StorageFailed = func() { failed = true }
	})
	if _, err := i2.Facts(ctx, identity("wc-one")); err == nil || !failed {
		t.Fatalf("storage failure: %v %v", err, failed)
	}
}

// Egress: the label and the policies that select the pod decide the state.
func TestFactsEgressStates(t *testing.T) {
	f := cluster(t)
	seedSandbox(f, "wc-one")
	i := newInspector(t, f, nil)
	ctx := ctxT(t)
	state := func() string {
		facts, err := i.Facts(ctx, identity("wc-one"))
		if err != nil {
			t.Fatal(err)
		}
		return facts.Egress
	}
	if state() != policy.EgressDenied {
		t.Fatal("unlabelled pod not denied")
	}
	if err := i.GrantEgress(ctx, identity("wc-one"), policy.GatewayEndpoint{Host: testGateway, Port: 7000, BindingID: "s1", Capability: "c"}); err != nil {
		t.Fatal(err)
	}
	if state() != policy.EgressGateway {
		t.Fatal("labelled pod not gateway")
	}
	// Another policy selecting the pod widens it: other, whether labelled
	// or not.
	f.seed(api.NetworkPolicies, testNamespace, &api.NetworkPolicy{Metadata: api.ObjectMeta{Name: "allow-all"}, Spec: api.NetworkPolicySpec{PolicyTypes: []string{"Egress"}, Egress: []api.NetworkPolicyEgressRule{{}}}})
	if state() != policy.EgressOther {
		t.Fatal("extra policy not reported")
	}
	if err := i.DenyEgress(ctx, identity("wc-one")); err != nil {
		t.Fatal(err)
	}
	if state() != policy.EgressOther {
		t.Fatal("extra policy not reported on a denied pod")
	}
	f.remove(api.NetworkPolicies, testNamespace, "allow-all")
	if state() != policy.EgressDenied {
		t.Fatal("denied pod not denied")
	}
	// A policy that selects other pods is irrelevant.
	f.seed(api.NetworkPolicies, testNamespace, &api.NetworkPolicy{Metadata: api.ObjectMeta{Name: "elsewhere"}, Spec: api.NetworkPolicySpec{PodSelector: api.LabelSelector{MatchLabels: map[string]string{"x": "y"}}, PolicyTypes: []string{"Egress"}}})
	if state() != policy.EgressDenied {
		t.Fatal("unrelated policy reported")
	}
	// Without default-deny nothing is denied.
	f.remove(api.NetworkPolicies, testNamespace, PolicyDefaultDeny)
	if state() != policy.EgressOther {
		t.Fatal("missing default-deny not reported")
	}
}

// Grant and deny patch the label and read it back; a wrong endpoint or an
// absent pod is refused; a deny on an absent pod is nothing.
func TestGrantAndDenyPatchTheLabel(t *testing.T) {
	f := cluster(t)
	seedSandbox(f, "wc-one")
	i := newInspector(t, f, nil)
	ctx := ctxT(t)
	endpoint := policy.GatewayEndpoint{Host: testGateway, Port: 7000, BindingID: "s1", Capability: "c"}
	if err := i.GrantEgress(ctx, identity("wc-one"), endpoint); err != nil {
		t.Fatal(err)
	}
	var pod api.Pod
	f.object(api.Pods, testNamespace, "wc-one", &pod)
	if pod.Metadata.Labels[LabelEgress] != EgressGateway || pod.Metadata.Labels[LabelSandbox] != "wc-one" {
		t.Fatalf("labels after grant: %v", pod.Metadata.Labels)
	}
	patches := f.count("PATCH", "/pods/wc-one")
	if patches != 1 {
		t.Fatalf("patches: %d", patches)
	}
	last := f.recorded()[len(f.recorded())-1]
	if last.ContentType != "application/merge-patch+json" || string(last.Body) != `{"metadata":{"labels":{"warden.monaddle.com/egress":"gateway"}}}` {
		t.Fatalf("patch: %s %s", last.ContentType, last.Body)
	}
	// Granting again confirms without a patch.
	if err := i.GrantEgress(ctx, identity("wc-one"), endpoint); err != nil || f.count("PATCH", "/pods/wc-one") != 1 {
		t.Fatalf("second grant: %v %d", err, f.count("PATCH", "/pods/wc-one"))
	}
	if err := i.GrantEgress(ctx, identity("wc-one"), policy.GatewayEndpoint{Host: testGateway, Port: 7001}); err == nil {
		t.Fatal("grant to another port accepted")
	}
	if err := i.GrantEgress(ctx, identity("wc-none"), endpoint); err == nil || err.Error() != "runtime absent" {
		t.Fatalf("grant to an absent pod: %v", err)
	}
	if err := i.DenyEgress(ctx, identity("wc-one")); err != nil {
		t.Fatal(err)
	}
	var denied api.Pod
	f.object(api.Pods, testNamespace, "wc-one", &denied)
	if _, ok := denied.Metadata.Labels[LabelEgress]; ok {
		t.Fatalf("label after deny: %v", denied.Metadata.Labels)
	}
	last = f.recorded()[len(f.recorded())-1]
	if string(last.Body) != `{"metadata":{"labels":{"warden.monaddle.com/egress":null}}}` {
		t.Fatalf("deny patch: %s", last.Body)
	}
	before := len(f.recorded())
	if err := i.DenyEgress(ctx, identity("wc-one")); err != nil || f.count("PATCH", "/pods/wc-one") != 2 {
		t.Fatalf("second deny patched again: %v", err)
	}
	if err := i.DenyEgress(ctx, identity("wc-none")); err != nil || len(f.recorded()) != before+2 {
		t.Fatalf("deny of an absent pod: %v", err)
	}
}

// Two live pods under one runtime name are ambiguous; a terminating
// predecessor beside its successor is not.
func TestFindPodAmbiguity(t *testing.T) {
	f := cluster(t)
	seedSandbox(f, "wc-one")
	other := sandboxPod("wc-one-b", "wc-one-home")
	other.Metadata.Labels[LabelSandbox] = "wc-one"
	f.seed(api.Pods, testNamespace, other)
	i := newInspector(t, f, nil)
	if _, err := i.Facts(ctxT(t), identity("wc-one")); err == nil || err.Error() != "ambiguous runtime identity" {
		t.Fatalf("two live pods: %v", err)
	}
	var pod api.Pod
	f.object(api.Pods, testNamespace, "wc-one-b", &pod)
	now := pod.Metadata.CreationTimestamp
	pod.Metadata.DeletionTimestamp = now
	f.put(api.Pods, testNamespace, &pod)
	facts, err := i.Facts(ctxT(t), identity("wc-one"))
	if err != nil || !facts.Present {
		t.Fatalf("terminating predecessor: %+v %v", facts, err)
	}
}
