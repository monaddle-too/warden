package kube

import (
	"net/http"
	"strings"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

// A compliant cluster passes; the proof runs both canaries once, deletes
// them, and is cached for the next call.
func TestClusterFactsPassAndCache(t *testing.T) {
	f := cluster(t)
	i := newInspector(t, f, nil)
	ctx := ctxT(t)
	if err := i.ClusterFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST", "/pods"); n != 2 {
		t.Fatalf("canary pods created: %d", n)
	}
	var pods api.List[api.Pod]
	if err := f.client().List(ctx, api.Pods, testNamespace, api.ListOptions{}, &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("canaries left behind: %d %v", len(pods.Items), err)
	}
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 2 {
		t.Fatalf("cached pass re-proved: %v %d", err, f.count("POST", "/pods"))
	}
	i.Invalidate()
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 4 {
		t.Fatalf("invalidated pass not re-proved: %v %d", err, f.count("POST", "/pods"))
	}
}

// The canary pods satisfy the namespace's admission policy and carry the
// role labels; only the gateway canary holds the egress label.
func TestCanarySpecIsAdmissible(t *testing.T) {
	f := cluster(t)
	i := newInspector(t, f, nil)
	for _, role := range []string{CanaryDeny, CanaryGateway} {
		pod := i.canarySpec(role, "abcd1234", nil)
		if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != testClass || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Fatalf("%s: not admissible: %+v", role, pod.Spec)
		}
		if pod.Metadata.Labels[LabelSandbox] == "" || pod.Metadata.Labels[LabelCanary] != role || len(pod.Spec.Volumes) != 0 || pod.Spec.RestartPolicy != "Never" {
			t.Fatalf("%s: labels or volumes: %+v", role, pod.Metadata)
		}
		if _, labelled := pod.Metadata.Labels[LabelEgress]; labelled != (role == CanaryGateway) {
			t.Fatalf("%s: egress label %v", role, labelled)
		}
		if pod.Spec.Containers[0].Image != DefaultCanaryImage || !strings.Contains(pod.Spec.Containers[0].Command[2], "nc -z -w 3") {
			t.Fatalf("%s: command: %v", role, pod.Spec.Containers[0])
		}
	}
	i = newInspector(t, f, func(o *Options) { o.Canary.Image = "registry.example/busybox:1.37" })
	if i.canarySpec(CanaryDeny, "x", nil).Spec.Containers[0].Image != "registry.example/busybox:1.37" {
		t.Fatal("canary image not configurable")
	}
}

// Every static check fails with a message naming it.
func TestClusterFactsFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fakeAPI)
		want   string
	}{
		{"namespace missing", func(f *fakeAPI) { f.remove(api.Namespaces, "", testNamespace) }, "sandbox namespace: namespace warden-sandboxes does not exist"},
		{"namespace without PSA", func(f *fakeAPI) {
			var ns api.Namespace
			f.object(api.Namespaces, "", testNamespace, &ns)
			delete(ns.Metadata.Labels, PSAEnforceLabel)
			f.put(api.Namespaces, "", &ns)
		}, "does not enforce Pod Security baseline"},
		{"namespace PSA privileged", func(f *fakeAPI) {
			var ns api.Namespace
			f.object(api.Namespaces, "", testNamespace, &ns)
			ns.Metadata.Labels[PSAEnforceLabel] = "privileged"
			f.put(api.Namespaces, "", &ns)
		}, "does not enforce Pod Security baseline"},
		{"namespace without role", func(f *fakeAPI) {
			var ns api.Namespace
			f.object(api.Namespaces, "", testNamespace, &ns)
			ns.Metadata.Labels[LabelRole] = "other"
			f.put(api.Namespaces, "", &ns)
		}, "lacks warden.monaddle.com/role=sandboxes"},
		{"default-deny missing", func(f *fakeAPI) { f.remove(api.NetworkPolicies, testNamespace, PolicyDefaultDeny) }, "network policies: default-deny is missing"},
		{"default-deny egress only", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyDefaultDeny, &np)
			np.Spec.PolicyTypes = []string{"Egress"}
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "default-deny must deny ingress and egress for every pod"},
		{"default-deny selecting some pods", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyDefaultDeny, &np)
			np.Spec.PodSelector = api.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "default-deny must deny ingress and egress for every pod"},
		{"gateway-egress missing", func(f *fakeAPI) { f.remove(api.NetworkPolicies, testNamespace, PolicyGatewayEgress) }, "gateway-egress is missing"},
		{"gateway-egress wrong port", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			p := api.FromInt(7001)
			np.Spec.Egress[0].Ports[0].Port = &p
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "gateway-egress must allow TCP 7000 only"},
		{"gateway-egress wrong selector", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			np.Spec.PodSelector = api.LabelSelector{}
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "must select exactly warden.monaddle.com/egress=gateway"},
		{"gateway-egress wrong namespace", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			np.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels[NamespaceNameLabel] = "elsewhere"
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "peer must select the core namespace warden"},
		{"gateway-egress to the runner", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			np.Spec.Egress[0].To[0].PodSelector.MatchLabels[LabelComponent] = ComponentRunner
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "peer must select the policy pods"},
		{"gateway-egress with an ipBlock", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			np.Spec.Egress[0].To = append(np.Spec.Egress[0].To, api.NetworkPolicyPeer{IPBlock: &api.IPBlock{CIDR: "0.0.0.0/0"}})
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "peer must select pods of the core namespace"},
		{"gateway-egress extra rule", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
			np.Spec.Egress = append(np.Spec.Egress, api.NetworkPolicyEgressRule{})
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "must hold one egress rule"},
		{"runner-ingress missing", func(f *fakeAPI) { f.remove(api.NetworkPolicies, testNamespace, PolicyRunnerIngress) }, "runner-ingress is missing"},
		{"runner-ingress from anywhere", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyRunnerIngress, &np)
			np.Spec.Ingress[0].From = []api.NetworkPolicyPeer{{IPBlock: &api.IPBlock{CIDR: "0.0.0.0/0"}}}
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "runner-ingress peer must select pods of the core namespace"},
		{"runner-ingress from the chat", func(f *fakeAPI) {
			var np api.NetworkPolicy
			f.object(api.NetworkPolicies, testNamespace, PolicyRunnerIngress, &np)
			np.Spec.Ingress[0].From[0].PodSelector.MatchLabels[LabelComponent] = "chat"
			f.put(api.NetworkPolicies, testNamespace, &np)
		}, "peer must select the runner pods"},
		{"binding missing", func(f *fakeAPI) {
			f.remove(api.ValidatingAdmissionPolicyBindings, "", "warden-"+testNamespace+"-sandbox-pods")
		}, "no ValidatingAdmissionPolicyBinding matches namespace"},
		{"binding for another namespace", func(f *fakeAPI) {
			var b api.ValidatingAdmissionPolicyBinding
			f.object(api.ValidatingAdmissionPolicyBindings, "", "warden-"+testNamespace+"-sandbox-pods", &b)
			b.Spec.MatchResources.NamespaceSelector.MatchLabels[NamespaceNameLabel] = "other"
			f.put(api.ValidatingAdmissionPolicyBindings, "", &b)
		}, "no ValidatingAdmissionPolicyBinding matches namespace"},
		{"binding that warns", func(f *fakeAPI) {
			var b api.ValidatingAdmissionPolicyBinding
			f.object(api.ValidatingAdmissionPolicyBindings, "", "warden-"+testNamespace+"-sandbox-pods", &b)
			b.Spec.ValidationActions = []string{"Warn"}
			f.put(api.ValidatingAdmissionPolicyBindings, "", &b)
		}, "does not deny"},
		{"policy missing", func(f *fakeAPI) {
			f.remove(api.ValidatingAdmissionPolicies, "", "warden-"+testNamespace+"-sandbox-pods")
		}, "binds a missing policy"},
		{"policy failing open", func(f *fakeAPI) {
			var p api.ValidatingAdmissionPolicy
			f.object(api.ValidatingAdmissionPolicies, "", "warden-"+testNamespace+"-sandbox-pods", &p)
			ignore := "Ignore"
			p.Spec.FailurePolicy = &ignore
			f.put(api.ValidatingAdmissionPolicies, "", &p)
		}, "must fail closed"},
		{"policy pinning another class", func(f *fakeAPI) {
			var p api.ValidatingAdmissionPolicy
			f.object(api.ValidatingAdmissionPolicies, "", "warden-"+testNamespace+"-sandbox-pods", &p)
			p.Spec.Validations[0].Expression = "object.spec.?runtimeClassName.orValue('') == 'runc'"
			f.put(api.ValidatingAdmissionPolicies, "", &p)
		}, "does not pin runtimeClassName gvisor"},
		{"policy without the token rule", func(f *fakeAPI) {
			var p api.ValidatingAdmissionPolicy
			f.object(api.ValidatingAdmissionPolicies, "", "warden-"+testNamespace+"-sandbox-pods", &p)
			p.Spec.Validations = append(p.Spec.Validations[:1], p.Spec.Validations[2:]...)
			f.put(api.ValidatingAdmissionPolicies, "", &p)
		}, "does not validate automountServiceAccountToken"},
		{"policy not matching pods", func(f *fakeAPI) {
			var p api.ValidatingAdmissionPolicy
			f.object(api.ValidatingAdmissionPolicies, "", "warden-"+testNamespace+"-sandbox-pods", &p)
			p.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"deployments"}
			f.put(api.ValidatingAdmissionPolicies, "", &p)
		}, "must match pod creation"},
		{"runtime class missing", func(f *fakeAPI) { f.remove(api.RuntimeClasses, "", testClass) }, "runtime class: RuntimeClass gvisor does not exist"},
		{"runtime class with a runc handler", func(f *fakeAPI) {
			f.put(api.RuntimeClasses, "", &api.RuntimeClass{Metadata: api.ObjectMeta{Name: testClass}, Handler: "runc"})
		}, `handler "runc" is not a gvisor handler (runsc, gvisor)`},
		{"runtime class of the other tier", func(f *fakeAPI) {
			f.put(api.RuntimeClasses, "", &api.RuntimeClass{Metadata: api.ObjectMeta{Name: testClass}, Handler: "kata-qemu"})
		}, `handler "kata-qemu" is not a gvisor handler`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cluster(t)
			tc.mutate(f)
			i := newInspector(t, f, nil)
			err := i.ClusterFacts(ctxT(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			// A static failure never reaches the canaries.
			if f.count("POST", "/pods") != 0 {
				t.Fatal("canaries ran after a static check failed")
			}
		})
	}
}

// The kata tier accepts its handlers, and an operator's allow-list wins.
func TestRuntimeClassHandlersPerTier(t *testing.T) {
	for _, tc := range []struct {
		tier, handler string
		handlers      []string
		ok            bool
	}{
		{TierKata, "kata-qemu", nil, true}, {TierKata, "kata-clh", nil, true}, {TierKata, "kata-qemu-runtime-rs", nil, true}, {TierKata, "runsc", nil, false},
		{TierGVisor, "gvisor", nil, true}, {TierGVisor, "kata", nil, false},
		{TierGVisor, "runsc-kvm", []string{"runsc-kvm"}, true}, {TierGVisor, "runsc", []string{"runsc-kvm"}, false},
	} {
		f := cluster(t)
		f.put(api.RuntimeClasses, "", &api.RuntimeClass{Metadata: api.ObjectMeta{Name: testClass}, Handler: tc.handler})
		i := newInspector(t, f, func(o *Options) { o.Tier, o.Handlers = tc.tier, tc.handlers })
		if err := i.checkRuntimeClass(ctxT(t)); (err == nil) != tc.ok {
			t.Errorf("%s/%s (%v): %v", tc.tier, tc.handler, tc.handlers, err)
		}
	}
	if _, err := New(Options{Client: cluster(t).client(), Namespace: testNamespace, RuntimeClass: testClass, GuestImageDigest: testDigest, GatewayPort: 7000, State: t.TempDir(), Tier: "runc"}); err == nil {
		t.Fatal("unknown tier accepted")
	}
}

// A failed proof is cached briefly (no canary storm on the chat path) and
// re-proved after FailureTTL; a pass lasts PassTTL.
func TestClusterFactsTTLs(t *testing.T) {
	f := cluster(t)
	scriptCanaries(f, map[string]string{"gateway": "closed", "apiserver": "closed", "dns": "closed", "external": "open"}, enforcedVerdicts(CanaryGateway))
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	i := newInspector(t, f, func(o *Options) { o.Now = func() time.Time { return now } })
	ctx := ctxT(t)
	err := i.ClusterFacts(ctx)
	if err == nil || !strings.Contains(err.Error(), "an unlabelled pod reached the external") {
		t.Fatalf("enforcement failure not reported: %v", err)
	}
	if err2 := i.ClusterFacts(ctx); err2 != err || f.count("POST", "/pods") != 2 {
		t.Fatalf("failure not cached: %v %d", err2, f.count("POST", "/pods"))
	}
	scriptCanaries(f, enforcedVerdicts(CanaryDeny), enforcedVerdicts(CanaryGateway))
	now = now.Add(61 * time.Second)
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 4 {
		t.Fatalf("failure not re-proved after its TTL: %v %d", err, f.count("POST", "/pods"))
	}
	now = now.Add(59 * time.Minute)
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 4 {
		t.Fatalf("pass re-proved before its TTL: %v %d", err, f.count("POST", "/pods"))
	}
	now = now.Add(2 * time.Minute)
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 6 {
		t.Fatalf("pass not re-proved after an hour: %v %d", err, f.count("POST", "/pods"))
	}
}

// A watch event on any of the proof's objects invalidates a cached pass.
func TestWatchEventsInvalidateTheProof(t *testing.T) {
	f := cluster(t)
	i := newInspector(t, f, nil)
	ctx := ctxT(t)
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := i.ClusterFacts(ctx); err != nil {
		t.Fatal(err)
	}
	// Nothing changed: the initial list is not a change.
	time.Sleep(100 * time.Millisecond)
	if err := i.ClusterFacts(ctx); err != nil || f.count("POST", "/pods") != 2 {
		t.Fatalf("initial list invalidated the proof: %v %d", err, f.count("POST", "/pods"))
	}
	var np api.NetworkPolicy
	f.object(api.NetworkPolicies, testNamespace, PolicyGatewayEgress, &np)
	np.Metadata.Annotations = map[string]string{"touched": "yes"}
	f.put(api.NetworkPolicies, testNamespace, &np)
	deadline := time.Now().Add(5 * time.Second)
	for f.count("POST", "/pods") < 4 {
		if time.Now().After(deadline) {
			t.Fatal("network policy change did not trigger a new proof")
		}
		if err := i.ClusterFacts(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The other watched kinds too.
	before := f.count("POST", "/pods")
	f.put(api.RuntimeClasses, "", &api.RuntimeClass{Metadata: api.ObjectMeta{Name: testClass}, Handler: "runsc", Overhead: &api.Overhead{PodFixed: api.ResourceList{"memory": "64Mi"}}})
	deadline = time.Now().Add(5 * time.Second)
	for f.count("POST", "/pods") < before+2 {
		if time.Now().After(deadline) {
			t.Fatal("runtime class change did not trigger a new proof")
		}
		if err := i.ClusterFacts(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Start fails, and watches nothing, when a watch cannot be opened.
func TestStartRefusesWithoutWatchPermission(t *testing.T) {
	f := cluster(t)
	f.mu.Lock()
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "runtimeclasses") {
			writeStatus(w, 403, "Forbidden", "runtimeclasses is forbidden")
			return true
		}
		return false
	}
	f.mu.Unlock()
	i := newInspector(t, f, nil)
	if err := i.Start(ctxT(t)); err == nil || !strings.Contains(err.Error(), "runtime class") {
		t.Fatalf("start: %v", err)
	}
}
