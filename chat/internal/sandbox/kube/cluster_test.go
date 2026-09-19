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
		{&kube.Pod{Status: kube.PodStatus{Phase: "Pending", Conditions: []kube.PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu. no new claims to deallocate, preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod."}}}}, "waiting for a node: none of the 3 nodes can take the sandbox: 3 full (cpu)"},
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

// The scheduler's tally in the owner's words: the GKE Autopilot resume that
// prompted it, the dev cluster's one node, a disk in another zone, a node
// still starting, an empty cluster, a reason the verdict does not know
// (kept verbatim) and a message that is not a tally.
func TestSchedulerVerdict(t *testing.T) {
	cases := []struct{ message, want string }{
		{"0/5 nodes are available: 2 Insufficient cpu, 2 Insufficient memory, 3 node(s) didn't match Pod's node affinity/selector. no new claims to deallocate, preemption: 0/5 nodes are available: 2 No preemption victims found for incoming pod, 3 Preemption is not helpful for scheduling.",
			"none of the 5 nodes can take the sandbox: 2 full (cpu, memory), 3 not for sandboxes"},
		{"0/6 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity, 2 Insufficient cpu, 2 Insufficient memory, 3 node(s) didn't match Pod's node affinity/selector. no new claims to deallocate, preemption: 0/6 nodes are available: 2 No preemption victims found for incoming pod, 4 Preemption is not helpful for scheduling.",
			"none of the 6 nodes can take the sandbox: 2 full (cpu, memory), 1 in another zone than the workspace's disk, 3 not for sandboxes"},
		{"0/6 nodes are available: 1 node(s) had untolerated taint(s), 2 Insufficient cpu, 3 node(s) didn't match Pod's node affinity/selector. preemption: 0/6 nodes are available: 2 No preemption victims found for incoming pod, 4 Preemption is not helpful for scheduling.",
			"none of the 6 nodes can take the sandbox: 2 full (cpu), 1 still starting or reserved, 3 not for sandboxes"},
		{"0/1 nodes are available: 1 Insufficient memory. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.",
			"the cluster's only node cannot take the sandbox: 1 full (memory)"},
		{"0/2 nodes are available: 1 Too many pods, 1 node(s) were unschedulable. preemption: 0/2 nodes are available: 1 No preemption victims found for incoming pod, 1 Preemption is not helpful for scheduling.",
			"none of the 2 nodes can take the sandbox: 1 full (Too many pods), 1 cordoned"},
		{"0/0 nodes are available: no nodes available to schedule pods.", "the cluster has no nodes"},
		{"no nodes available to schedule pods", "the cluster has no nodes"},
		{"0/2 nodes are available: 2 node(s) exceed max volume count. preemption: 0/2 nodes are available: 2 No preemption victims found for incoming pod.",
			"none of the 2 nodes can take the sandbox: 2 node(s) exceed max volume count"},
		{"0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.",
			"the cluster's only node cannot take the sandbox: the workspace's disk is not ready"},
		{"skip schedule deleting pod: warden-sandboxes/wc-1", "skip schedule deleting pod: warden-sandboxes/wc-1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SchedulerVerdict(c.message); got != c.want {
			t.Errorf("SchedulerVerdict(%q)\n got %q\nwant %q", c.message, got, c.want)
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
	// One pod, by runtime name; absent once stopped.
	pod, err := d.Pod(ctx, "wc-one")
	if err != nil || pod == nil || pod.Node != "node-a" || pod.Usage == nil {
		t.Fatalf("pod: %+v, %v", pod, err)
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
