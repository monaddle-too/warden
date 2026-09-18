package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// StartupDetail names what a pod that is not yet running is waiting for.
func TestStartupDetail(t *testing.T) {
	waiting := func(reason, message string) *kube.Pod {
		return &kube.Pod{Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "True"}}, ContainerStatuses: []kube.ContainerStatus{{Name: ContainerName, State: kube.ContainerState{Waiting: &kube.ContainerStateWaiting{Reason: reason, Message: message}}}}}}
	}
	cases := []struct {
		pod  *kube.Pod
		want string
	}{
		{&kube.Pod{Status: kube.PodStatus{Phase: "Pending"}}, "waiting for a node"},
		{&kube.Pod{Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu. no new claims to deallocate, preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod."}}}}, "waiting for a node: 0/3 nodes are available: 3 Insufficient cpu"},
		{&kube.Pod{Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "True"}}}}, "waiting for the kubelet to start the container"},
		{waiting("ContainerCreating", ""), "starting the container"},
		{waiting("ImagePullBackOff", "Back-off pulling image"), "waiting for the container image: Back-off pulling image"},
		{waiting("ErrImagePull", ""), "pulling the container image"},
		{waiting("CreateContainerError", "runsc failed"), "CreateContainerError: runsc failed"},
		{&kube.Pod{Status: kube.PodStatus{Phase: "Running"}}, "waiting for the pod's address"},
		{&kube.Pod{Status: kube.PodStatus{Phase: "Running", PodIP: "10.0.0.1"}}, ""},
		{&kube.Pod{Metadata: kube.ObjectMeta{DeletionTimestamp: new(time.Time)}}, "the previous pod is still terminating"},
		{&kube.Pod{Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "True"}}, ContainerStatuses: []kube.ContainerStatus{{State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}}}}, "container exited (1 Error)"},
	}
	for _, c := range cases {
		if got := StartupDetail(c.pod); got != c.want {
			t.Errorf("StartupDetail = %q, want %q", got, c.want)
		}
	}
}

// The cluster view lists the nodes, the sandbox pods with their workspace
// identity and the service pods, with usage when metrics are served, and
// explains what it may not read instead of failing.
func TestClusterViewAndLogs(t *testing.T) {
	api := newFakeAPI(t)
	api.publishTrust("bundle")
	d := newTestDriver(t, api, testOptions())
	ctx := context.Background()
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-one", Directory: "/home/agent/workspace", SandboxID: "sandbox-one", Generation: "gen-1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(ctx, sandbox.RuntimeSpec{Name: "wc-spare-two", Directory: "/home/agent/workspace", Spare: true}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.nodes = []map[string]any{{"metadata": map[string]any{"name": "node-a", "labels": map[string]any{"node-role.kubernetes.io/control-plane": "true"}}, "status": map[string]any{"capacity": map[string]any{"cpu": "8", "memory": "16Gi"}, "allocatable": map[string]any{"cpu": "7500m", "memory": "15Gi"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "nodeInfo": map[string]any{"kubeletVersion": "v1.36.4+k3s1", "containerRuntimeVersion": "containerd://2.3.4"}}}}
	// A service pod in the same namespace (the fake serves one namespace).
	api.objects["pods/warden-chat-1"] = map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "warden-chat-1", "namespace": testNamespace, "labels": map[string]any{ComponentLabel: "chat"}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "chat", "resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}}}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "chat", "ready": true, "restartCount": 2, "state": map[string]any{"running": map[string]any{}}}}}}
	for _, pod := range []string{"wc-one", "wc-spare-two"} {
		spec := api.objects["pods/"+pod]["spec"].(map[string]any)
		spec["nodeName"] = "node-a"
	}
	api.logs = map[string][]string{"wc-one": {"one", "two", "three"}, "warden-chat-1": {"chat started"}}
	// Events about the bound pod: the scheduler's refusal and, later,
	// the autoscaler's answer; one about the service pod; one about
	// nothing the view lists.
	one := metaString(api.objects["pods/wc-one"], "uid")
	api.objects["events/wc-one.a"] = coreEvent("wc-one.a", "Pod", "wc-one", one, "Warning", "FailedScheduling", "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 2 No preemption victims found for incoming pod.", "default-scheduler", "2026-09-17T10:00:00Z", 3)
	api.objects["events/wc-one.b"] = coreEvent("wc-one.b", "Pod", "wc-one", one, "Normal", "TriggeredScaleUp", "pod triggered scale-up: [{https://www.googleapis.com/compute/v1/projects/p/zones/z/instanceGroups/gk3-pool 0->1 (max: 1000)}]", "cluster-autoscaler", "2026-09-17T10:00:05Z", 1)
	api.objects["events/chat.a"] = coreEvent("chat.a", "Pod", "warden-chat-1", "uid-chat", "Normal", "Pulled", "Container image \"warden/chat:1\" already present on machine", "kubelet", "2026-09-17T09:00:00Z", 1)
	api.objects["events/pvc.a"] = coreEvent("pvc.a", "PersistentVolumeClaim", "wc-one", "uid-pvc", "Normal", "ProvisioningSucceeded", "Successfully provisioned volume", "pd.csi.storage.gke.io", "2026-09-17T09:30:00Z", 1)
	api.mu.Unlock()

	status, err := d.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.Server != "v1.36.4-fake" || status.Metrics || !strings.Contains(status.MetricsError, "metrics server") {
		t.Fatalf("status without metrics: %+v", status)
	}
	if len(status.Nodes) != 1 || status.Nodes[0].Name != "node-a" || !status.Nodes[0].Ready || status.Nodes[0].Roles[0] != "control-plane" || status.Nodes[0].Allocatable.CPUMilli != 7500 || status.Nodes[0].Allocatable.MemoryBytes != 15<<30 || status.Nodes[0].SandboxPods != 2 || status.Nodes[0].Usage != nil {
		t.Fatalf("nodes: %+v", status.Nodes)
	}
	if len(status.SandboxPods) != 2 || status.SandboxPods[0].Name != "wc-one" || status.SandboxPods[0].SandboxID != "sandbox-one" || status.SandboxPods[0].Spare || !status.SandboxPods[0].Ready || status.SandboxPods[0].RuntimeClass != "gvisor" || status.SandboxPods[0].Limits.MemoryBytes != 1024<<20 {
		t.Fatalf("sandbox pods: %+v", status.SandboxPods)
	}
	if !status.SandboxPods[1].Spare || status.SandboxPods[1].SandboxID != "" {
		t.Fatalf("spare: %+v", status.SandboxPods[1])
	}
	if len(status.ServicePods) != 1 || status.ServicePods[0].Component != "chat" || status.ServicePods[0].Restarts != 2 || status.ServicePods[0].Requests.CPUMilli != 100 {
		t.Fatalf("service pods: %+v", status.ServicePods)
	}
	// Events: the cluster's newest first across objects; each pod's own,
	// newest first, with the owner's-words hint. The service pod's uid is
	// not the seeded one, so it has none.
	if len(status.Events) != 4 || status.Events[0].Reason != "TriggeredScaleUp" || status.Events[0].Hint != "a node is being added" || status.Events[3].Kind != "Pod" || status.Events[3].Name != "warden-chat-1" || status.Events[3].Source != "kubelet" || status.EventsError != "" {
		t.Fatalf("cluster events: %+v", status.Events)
	}
	if ev := status.SandboxPods[0].Events; len(ev) != 2 || ev[0].Reason != "TriggeredScaleUp" || ev[1].Reason != "FailedScheduling" || ev[1].Count != 3 || ev[1].Type != "Warning" || ev[1].Hint != "no node fits yet: 0/2 nodes are available: 2 Insufficient cpu" {
		t.Fatalf("pod events: %+v", ev)
	}
	if ev := status.SandboxPods[1].Events; len(ev) != 0 {
		t.Fatalf("spare pod events: %+v", ev)
	}
	if ev := status.ServicePods[0].Events; len(ev) != 0 {
		t.Fatalf("service pod events: %+v", ev)
	}
	// With a metrics server the usage columns fill in; the answer is cached
	// briefly, so the change shows after the cache lapses.
	api.mu.Lock()
	api.metrics = true
	api.nodeMetrics = map[string]map[string]string{"node-a": {"cpu": "250m", "memory": "2Gi"}}
	api.podMetrics = map[string]map[string]string{"wc-one": {"cpu": "12m", "memory": "300Mi"}}
	api.mu.Unlock()
	d.cacheMu.Lock()
	d.cache = clusterCache{}
	d.cacheMu.Unlock()
	if status, err = d.Cluster(ctx); err != nil {
		t.Fatal(err)
	}
	if !status.Metrics || status.Nodes[0].Usage == nil || status.Nodes[0].Usage.CPUMilli != 250 || status.SandboxPods[0].Usage == nil || status.SandboxPods[0].Usage.MemoryBytes != 300<<20 || status.SandboxPods[1].Usage != nil {
		t.Fatalf("usage: nodes %+v pods %+v", status.Nodes[0].Usage, status.SandboxPods[0].Usage)
	}
	// One pod, by runtime name, with its events; absent once stopped.
	pod, err := d.Pod(ctx, "wc-one")
	if err != nil || pod == nil || pod.Node != "node-a" || pod.Usage == nil {
		t.Fatalf("pod: %+v, %v", pod, err)
	}
	if len(pod.Events) != 2 || pod.Events[0].Reason != "TriggeredScaleUp" {
		t.Fatalf("pod events: %+v", pod.Events)
	}
	if err = d.Stop(ctx, "wc-spare-two"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * api.deleteDelay)
	d.cacheMu.Lock()
	d.cache = clusterCache{}
	d.cacheMu.Unlock()
	if pod, err = d.Pod(ctx, "wc-spare-two"); err != nil || pod != nil {
		t.Fatalf("stopped pod: %+v, %v", pod, err)
	}
	// Nodes the runner may not list are explained, not fatal.
	api.mu.Lock()
	api.forbidden["list nodes"] = true
	api.mu.Unlock()
	d.cacheMu.Lock()
	d.cache = clusterCache{}
	d.cacheMu.Unlock()
	if status, err = d.Cluster(ctx); err != nil || len(status.Nodes) != 0 || !strings.Contains(status.NodesError, "rbac.runnerClusterView") {
		t.Fatalf("forbidden nodes: %+v, %v", status.NodesError, err)
	}
	// So are events: the cluster's list is empty and explained, the pods'
	// lists empty, the rest of the view intact.
	api.mu.Lock()
	api.forbidden["list events"] = true
	api.mu.Unlock()
	d.cacheMu.Lock()
	d.cache = clusterCache{}
	d.cacheMu.Unlock()
	if status, err = d.Cluster(ctx); err != nil || len(status.Events) != 0 || !strings.Contains(status.EventsError, "ServiceAccount") || len(status.SandboxPods) != 1 || len(status.SandboxPods[0].Events) != 0 {
		t.Fatalf("forbidden events: %+v, %v", status, err)
	}
	if pod, err = d.Pod(ctx, "wc-one"); err != nil || pod == nil || len(pod.Events) != 0 {
		t.Fatalf("pod without events: %+v, %v", pod, err)
	}
	api.mu.Lock()
	delete(api.forbidden, "list events")
	api.mu.Unlock()
	// Logs: the tail, with the container list; a service pod too; a pod
	// outside the two sets is refused.
	logs, err := d.Logs(ctx, sandbox.LogQuery{Pod: "wc-one", Tail: 2})
	if err != nil || len(logs.Lines) != 2 || !strings.HasSuffix(logs.Lines[1], " three") || logs.Container != ContainerName || len(logs.Containers) != 1 {
		t.Fatalf("logs: %+v, %v", logs, err)
	}
	if logs, err = d.Logs(ctx, sandbox.LogQuery{Pod: "warden-chat-1", Previous: true}); err != nil || len(logs.Lines) != 1 || !strings.HasSuffix(logs.Lines[0], "previous instance") {
		t.Fatalf("service logs: %+v, %v", logs, err)
	}
	if _, err = d.Logs(ctx, sandbox.LogQuery{Pod: "wc-one", Namespace: "kube-system"}); err == nil {
		t.Fatal("logs read outside the sandbox and service namespaces")
	}
	if _, err = d.Logs(ctx, sandbox.LogQuery{Pod: "no-such-pod"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing pod: %v", err)
	}
}

// readTail keeps the newest lines within the line and byte limits.
func TestReadTail(t *testing.T) {
	lines, truncated, err := readTail(strings.NewReader("a\nb\nc\nd\n"), 2, 1<<20)
	if err != nil || truncated != true || len(lines) != 2 || lines[0] != "c" {
		t.Fatalf("tail: %v %v %v", lines, truncated, err)
	}
	lines, truncated, err = readTail(strings.NewReader("aaaa\nbbbb\ncccc\n"), 10, 9)
	if err != nil || !truncated || len(lines) != 1 || lines[0] != "cccc" {
		t.Fatalf("byte cap: %v %v %v", lines, truncated, err)
	}
	if lines, truncated, err = readTail(strings.NewReader(""), 10, 10); err != nil || truncated || len(lines) != 0 {
		t.Fatalf("empty: %v %v %v", lines, truncated, err)
	}
}

// coreEvent is one core event object as the API lists it.
func coreEvent(name, kind, object, uid, typ, reason, message, source, at string, count int) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"name": name, "namespace": testNamespace, "uid": "uid-event-" + name, "creationTimestamp": at}, "involvedObject": map[string]any{"kind": kind, "namespace": testNamespace, "name": object, "uid": uid}, "type": typ, "reason": reason, "message": message, "count": count, "lastTimestamp": at, "source": map[string]any{"component": source}}
}

// EventHint says, in the owner's words, what the events that decide a
// start mean; the rest have none.
func TestEventHint(t *testing.T) {
	cases := []struct{ reason, message, want string }{
		{"TriggeredScaleUp", "pod triggered scale-up: [{gk3-pool 0->1 (max: 1000)}]", "a node is being added"},
		{"NotTriggerScaleUp", "pod didn't trigger scale-up: 1 max node group size reached", "no node can be added: 1 max node group size reached"},
		{"FailedScheduling", "0/2 nodes are available: 2 Insufficient cpu. preemption: not eligible.", "no node fits yet: 0/2 nodes are available: 2 Insufficient cpu"},
		{"Scheduled", "Successfully assigned warden-sandboxes/wc-one to gk3-node-1", "placed on node gk3-node-1"},
		{"Pulling", "Pulling image \"ghcr.io/x/guest:1\"", "pulling the container image"},
		{"Pulled", "Successfully pulled image \"ghcr.io/x/guest:1\" in 42.1s (42.1s including waiting). Image size: 1.2GB.", "the container image is pulled (42.1s)"},
		{"Pulled", "Container image \"ghcr.io/x/guest:1\" already present on machine", "the container image was already on the node"},
		{"Failed", "Failed to pull image \"ghcr.io/x/guest:1\": not found", "the container image cannot be pulled: Failed to pull image \"ghcr.io/x/guest:1\": not found"},
		{"BackOff", "Back-off pulling image \"ghcr.io/x/guest:1\"", "retrying the image pull after failures"},
		{"BackOff", "Back-off restarting failed container guest in pod wc-one", "the container keeps exiting; restarts are backing off"},
		{"FailedMount", "MountVolume.SetUp failed for volume \"workspace\": timed out", "the workspace volume is not attached yet: MountVolume.SetUp failed for volume \"workspace\": timed out"},
		{"FailedCreatePodSandBox", "Failed to create pod sandbox: rpc error: gvisor", "the sandbox runtime could not start the pod: Failed to create pod sandbox: rpc error: gvisor"},
		{"Started", "Started container guest", "the container is started"},
		{"SandboxChanged", "Pod sandbox changed, it will be killed and re-created.", ""},
	}
	for _, c := range cases {
		if got := EventHint(c.reason, c.message); got != c.want {
			t.Errorf("EventHint(%s, %q) = %q, want %q", c.reason, c.message, got, c.want)
		}
	}
}

// The startup detail carries the newest event's word when it adds
// something: not a repeat of the detail, not a plain Normal event with no
// hint, not an event from before the pod existed.
func TestStartupDetailWithEvents(t *testing.T) {
	created := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	pending := &kube.Pod{Metadata: kube.ObjectMeta{CreationTimestamp: &created}, Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "False", Message: "0/2 nodes are available: 2 Insufficient cpu. preemption: none."}}}}
	scaleUp := sandbox.Event{At: created.Add(5 * time.Second), Type: "Normal", Reason: "TriggeredScaleUp", Hint: "a node is being added"}
	old := sandbox.Event{At: created.Add(-time.Minute), Type: "Warning", Reason: "FailedScheduling", Message: "an earlier pod's"}
	plain := sandbox.Event{At: created.Add(time.Second), Type: "Normal", Reason: "SandboxChanged", Message: "Pod sandbox changed"}
	if got := startupDetailWithEvents(pending, nil); got != "waiting for a node: 0/2 nodes are available: 2 Insufficient cpu" {
		t.Fatalf("no events: %q", got)
	}
	if got := startupDetailWithEvents(pending, []sandbox.Event{scaleUp, old}); got != "waiting for a node: 0/2 nodes are available: 2 Insufficient cpu · a node is being added" {
		t.Fatalf("scale-up: %q", got)
	}
	if got := startupDetailWithEvents(pending, []sandbox.Event{plain, old}); got != "waiting for a node: 0/2 nodes are available: 2 Insufficient cpu" {
		t.Fatalf("nothing to add: %q", got)
	}
	warning := sandbox.Event{At: created.Add(time.Second), Type: "Warning", Reason: "FailedMount", Message: "MountVolume.SetUp failed for volume \"workspace\": timed out. Retrying.", Hint: EventHint("FailedMount", "MountVolume.SetUp failed for volume \"workspace\": timed out. Retrying.")}
	pulling := &kube.Pod{Metadata: kube.ObjectMeta{CreationTimestamp: &created}, Spec: kube.PodSpec{NodeName: "node-a"}, Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "True"}}, ContainerStatuses: []kube.ContainerStatus{{Name: "guest", State: kube.ContainerState{Waiting: &kube.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}
	if got := startupDetailWithEvents(pulling, []sandbox.Event{warning}); got != "starting the container · the workspace volume is not attached yet: MountVolume.SetUp failed for volume \"workspace\": timed out" {
		t.Fatalf("warning: %q", got)
	}
	// A Warning without a hint still says its reason and first sentence.
	odd := sandbox.Event{At: created.Add(time.Second), Type: "Warning", Reason: "FailedSomething", Message: "the first sentence. The second."}
	if got := startupDetailWithEvents(pulling, []sandbox.Event{odd}); got != "starting the container · FailedSomething: the first sentence" {
		t.Fatalf("odd warning: %q", got)
	}
}
