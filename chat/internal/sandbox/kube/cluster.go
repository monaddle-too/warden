package kube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// The driver's sandbox.ClusterInspector (docs/warden-startup-visibility-
// plan.md): what the owner sees of the cluster. Everything here is read-
// only and answered from the API server on demand, cached for
// clusterCacheTTL so the workspace panel and the admin console, which poll,
// share one round of reads.
const (
	clusterCacheTTL = 3 * time.Second
	// LogTailDefault and LogTailMax bound a log read; LogBytesMax caps what
	// is kept of it (the newest lines win).
	LogTailDefault = 200
	LogTailMax     = 2000
	LogBytesMax    = 1 << 20
	// ComponentLabel marks the Warden service pods in the runner's own
	// namespace (the chart's selector label).
	ComponentLabel = "warden.monaddle.com/component"
)

// clusterCache is the last answer of Cluster and the last pod answers,
// guarded by cacheMu.
type clusterCache struct {
	status *sandbox.ClusterStatus
	at     time.Time
	pods   map[string]podCacheEntry
}

type podCacheEntry struct {
	info *sandbox.PodInfo
	at   time.Time
}

var _ sandbox.ClusterInspector = (*Driver)(nil)

// Pod describes the sandbox pod of a runtime: nil when it has none.
func (d *Driver) Pod(ctx context.Context, name string) (*sandbox.PodInfo, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	d.cacheMu.Lock()
	if e, ok := d.cache.pods[name]; ok && time.Since(e.at) < clusterCacheTTL {
		d.cacheMu.Unlock()
		return e.info, nil
	}
	d.cacheMu.Unlock()
	var pod kube.Pod
	err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &pod)
	var info *sandbox.PodInfo
	switch {
	case kube.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("sandbox %s: pod: %w", name, err)
	default:
		var metrics kube.PodMetricsItem
		usage := map[string]*sandbox.Amounts{}
		if d.client.Get(ctx, kube.PodMetrics, d.opts.Namespace, name, &metrics) == nil {
			usage[name] = podUsage(metrics)
		}
		summary := podInfo(&pod, usage)
		// The pod's events: the autoscaler's and the kubelet's word on a
		// start. A Role without the events verb leaves the list empty.
		if events, err := d.podEvents(ctx, d.opts.Namespace, pod.Metadata.UID); err == nil {
			summary.Events = events
		}
		info = &summary
	}
	d.cacheMu.Lock()
	if d.cache.pods == nil {
		d.cache.pods = map[string]podCacheEntry{}
	}
	d.cache.pods[name] = podCacheEntry{info: info, at: time.Now()}
	d.cacheMu.Unlock()
	return info, nil
}

// Cluster is the nodes, the sandbox pods and the Warden service pods.
// What the runner may not list (nodes without rbac.runnerClusterView, its
// own namespace's pods) is reported in the status's error fields rather
// than failing the answer.
func (d *Driver) Cluster(ctx context.Context) (*sandbox.ClusterStatus, error) {
	d.cacheMu.Lock()
	if d.cache.status != nil && time.Since(d.cache.at) < clusterCacheTTL {
		status := *d.cache.status
		d.cacheMu.Unlock()
		return &status, nil
	}
	d.cacheMu.Unlock()
	status := sandbox.ClusterStatus{Available: true, At: time.Now(), SandboxNamespace: d.opts.Namespace, ServiceNamespace: d.client.Namespace(), Tier: d.opts.Tier, RuntimeClass: d.opts.RuntimeClass, Nodes: []sandbox.NodeInfo{}, SandboxPods: []sandbox.PodInfo{}, ServicePods: []sandbox.PodInfo{}, Events: []sandbox.Event{}}
	if v, err := d.client.ServerVersion(ctx); err == nil {
		status.Server = v.GitVersion
	}
	// Live usage first, so the pod and node summaries can carry it. Any
	// refusal (no metrics server, no RBAC) leaves usage nil.
	usage := map[string]*sandbox.Amounts{}
	nodeUsage := map[string]*sandbox.Amounts{}
	status.Metrics = true
	var podMetrics kube.List[kube.PodMetricsItem]
	if err := d.client.List(ctx, kube.PodMetrics, d.opts.Namespace, kube.ListOptions{}, &podMetrics); err != nil {
		status.Metrics, status.MetricsError = false, metricsError(err)
	} else {
		for _, m := range podMetrics.Items {
			usage[d.opts.Namespace+"/"+m.Metadata.Name] = podUsage(m)
		}
	}
	if status.Metrics && d.client.Namespace() != "" && d.client.Namespace() != d.opts.Namespace {
		podMetrics.Items = nil
		if err := d.client.List(ctx, kube.PodMetrics, d.client.Namespace(), kube.ListOptions{}, &podMetrics); err == nil {
			for _, m := range podMetrics.Items {
				usage[d.client.Namespace()+"/"+m.Metadata.Name] = podUsage(m)
			}
		}
	}
	var nodeMetrics kube.List[kube.NodeMetricsItem]
	if status.Metrics {
		if err := d.client.List(ctx, kube.NodeMetrics, "", kube.ListOptions{}, &nodeMetrics); err == nil {
			for _, m := range nodeMetrics.Items {
				nodeUsage[m.Metadata.Name] = resources(m.Usage)
			}
		}
	}
	// Events, one list per namespace, related to the pods by uid; the
	// cluster's list is the newest of both. A refusal is reported, not
	// fatal.
	var all []kube.CoreEvent
	byUID := map[string][]kube.CoreEvent{}
	if events, err := d.namespaceEvents(ctx, d.opts.Namespace); err != nil {
		status.EventsError = metricsError(err)
	} else {
		all = append(all, events...)
	}
	if ns := d.client.Namespace(); ns != "" && ns != d.opts.Namespace && status.EventsError == "" {
		if events, err := d.namespaceEvents(ctx, ns); err == nil {
			all = append(all, events...)
		}
	}
	for _, e := range all {
		if e.InvolvedObject.UID != "" {
			byUID[e.InvolvedObject.UID] = append(byUID[e.InvolvedObject.UID], e)
		}
	}
	status.Events = eventInfos(all, ClusterEventsMax)
	var pods kube.List[kube.Pod]
	if err := d.client.List(ctx, kube.Pods, d.opts.Namespace, kube.ListOptions{LabelSelector: selector()}, &pods); err != nil {
		return nil, fmt.Errorf("sandbox pods: %w", err)
	}
	perNode := map[string]int{}
	for i := range pods.Items {
		info := podInfo(&pods.Items[i], map[string]*sandbox.Amounts{pods.Items[i].Metadata.Name: usage[d.opts.Namespace+"/"+pods.Items[i].Metadata.Name]})
		info.Events = eventInfos(byUID[info.UID], PodEventsMax)
		status.SandboxPods = append(status.SandboxPods, info)
		if info.Node != "" {
			perNode[info.Node]++
		}
	}
	sort.Slice(status.SandboxPods, func(i, j int) bool { return status.SandboxPods[i].Name < status.SandboxPods[j].Name })
	if ns := d.client.Namespace(); ns != "" {
		var service kube.List[kube.Pod]
		if err := d.client.List(ctx, kube.Pods, ns, kube.ListOptions{LabelSelector: ComponentLabel}, &service); err != nil {
			status.ServicePodsError = metricsError(err)
		} else {
			for i := range service.Items {
				info := podInfo(&service.Items[i], map[string]*sandbox.Amounts{service.Items[i].Metadata.Name: usage[ns+"/"+service.Items[i].Metadata.Name]})
				info.Events = eventInfos(byUID[info.UID], PodEventsMax)
				status.ServicePods = append(status.ServicePods, info)
			}
			sort.Slice(status.ServicePods, func(i, j int) bool {
				if status.ServicePods[i].Component != status.ServicePods[j].Component {
					return status.ServicePods[i].Component < status.ServicePods[j].Component
				}
				return status.ServicePods[i].Name < status.ServicePods[j].Name
			})
		}
	}
	var nodes kube.List[kube.Node]
	if err := d.client.List(ctx, kube.Nodes, "", kube.ListOptions{}, &nodes); err != nil {
		status.NodesError = metricsError(err)
	} else {
		for i := range nodes.Items {
			info := nodeInfo(&nodes.Items[i], nodeUsage[nodes.Items[i].Metadata.Name])
			info.SandboxPods = perNode[info.Name]
			status.Nodes = append(status.Nodes, info)
		}
		sort.Slice(status.Nodes, func(i, j int) bool { return status.Nodes[i].Name < status.Nodes[j].Name })
	}
	d.cacheMu.Lock()
	copied := status
	d.cache.status, d.cache.at = &copied, time.Now()
	d.cacheMu.Unlock()
	return &status, nil
}

// Logs reads the tail of one container's log of a pod the owner may see:
// a sandbox pod of this driver or a Warden service pod. The pod is read
// first, so the answer names its containers and a pod outside those two
// sets is refused before any log is fetched.
func (d *Driver) Logs(ctx context.Context, q sandbox.LogQuery) (*sandbox.PodLogs, error) {
	if q.Namespace == "" {
		q.Namespace = d.opts.Namespace
	}
	if err := validObjectName(q.Pod); err != nil {
		return nil, fmt.Errorf("pod: %w", err)
	}
	if q.Container != "" {
		if err := validObjectName(q.Container); err != nil {
			return nil, fmt.Errorf("container: %w", err)
		}
	}
	var pod kube.Pod
	if err := d.client.Get(ctx, kube.Pods, q.Namespace, q.Pod, &pod); err != nil {
		if kube.IsNotFound(err) {
			return nil, errors.New("pod not found")
		}
		return nil, fmt.Errorf("pod %s: %w", q.Pod, err)
	}
	sandboxPod := q.Namespace == d.opts.Namespace && pod.Metadata.Labels[LabelManagedBy] == ManagedBy
	servicePod := q.Namespace == d.client.Namespace() && pod.Metadata.Labels[ComponentLabel] != ""
	if !sandboxPod && !servicePod {
		return nil, errors.New("only sandbox pods and Warden service pods have readable logs")
	}
	containers := containerNames(&pod)
	if q.Container == "" && len(containers) > 0 {
		q.Container = containers[0]
	}
	if q.Tail <= 0 {
		q.Tail = LogTailDefault
	}
	if q.Tail > LogTailMax {
		q.Tail = LogTailMax
	}
	body, err := d.client.LogsWith(ctx, q.Namespace, q.Pod, kube.LogOptions{Container: q.Container, Previous: q.Previous, Timestamps: true, TailLines: q.Tail})
	if err != nil {
		return nil, fmt.Errorf("pod %s logs: %w", q.Pod, err)
	}
	defer body.Close()
	lines, truncated, err := readTail(body, q.Tail, LogBytesMax)
	if err != nil {
		return nil, fmt.Errorf("pod %s logs: %w", q.Pod, err)
	}
	return &sandbox.PodLogs{Namespace: q.Namespace, Pod: q.Pod, Container: q.Container, Containers: containers, Lines: lines, Truncated: truncated, At: time.Now()}, nil
}

// validObjectName accepts a pod or container name as Kubernetes names
// them (a DNS label or subdomain), the owner's choice from a listing.
func validObjectName(name string) error {
	if len(name) == 0 || len(name) > 253 || name[0] == '-' || name[0] == '.' || name[len(name)-1] == '-' || name[len(name)-1] == '.' {
		return fmt.Errorf("invalid name %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			return fmt.Errorf("invalid name %q", name)
		}
	}
	return nil
}

// readTail keeps the last max lines of r within limit bytes; truncated
// says that older lines were dropped.
func readTail(r io.Reader, max int, limit int) ([]string, bool, error) {
	lines := []string{}
	truncated := false
	size := 0
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		size += len(line) + 1
		for len(lines) > max || (size > limit && len(lines) > 1) {
			size -= len(lines[0]) + 1
			lines = lines[1:]
			truncated = true
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			lines = append(lines, "… a line longer than 256 KiB was cut …")
			truncated = true
			return lines, truncated, nil
		}
		return nil, truncated, err
	}
	return lines, truncated, nil
}

// podInfo summarises a pod for the owner; usage is by pod name.
func podInfo(pod *kube.Pod, usage map[string]*sandbox.Amounts) sandbox.PodInfo {
	info := sandbox.PodInfo{Namespace: pod.Metadata.Namespace, Name: pod.Metadata.Name, UID: pod.Metadata.UID, Node: pod.Spec.NodeName, Phase: pod.Status.Phase, IP: pod.Status.PodIP, Started: pod.Status.StartTime, Containers: containerNames(pod), SandboxID: pod.Metadata.Annotations[AnnotationSandboxID], Spare: pod.Metadata.Labels[LabelSpare] == "true", Component: pod.Metadata.Labels[ComponentLabel], Events: []sandbox.Event{}}
	if pod.Spec.RuntimeClassName != nil {
		info.RuntimeClass = *pod.Spec.RuntimeClassName
	}
	if pod.Status.Phase == "" {
		info.Phase = "Unknown"
	}
	if pod.Metadata.DeletionTimestamp != nil {
		info.Phase = "Terminating"
	}
	ready := len(pod.Status.ContainerStatuses) > 0
	for _, c := range pod.Status.ContainerStatuses {
		info.Restarts += int(c.RestartCount)
		if !c.Ready {
			ready = false
		}
	}
	info.Ready = ready && info.Phase == "Running"
	if info.Phase != "Running" || !info.Ready {
		info.Reason = StartupDetail(pod)
		if info.Phase == "Terminating" {
			info.Reason = "terminating"
		}
	}
	for _, c := range pod.Spec.Containers {
		info.Requests = info.Requests.Add(resources(c.Resources.Requests))
		info.Limits = info.Limits.Add(resources(c.Resources.Limits))
	}
	info.Usage = usage[pod.Metadata.Name]
	return info
}

func containerNames(pod *kube.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

// nodeInfo summarises a node; usage is nil without metrics.
func nodeInfo(node *kube.Node, usage *sandbox.Amounts) sandbox.NodeInfo {
	info := sandbox.NodeInfo{Name: node.Metadata.Name, Roles: []string{}, KubeletVersion: node.Status.NodeInfo.KubeletVersion, ContainerRuntime: node.Status.NodeInfo.ContainerRuntimeVersion, OS: node.Status.NodeInfo.OSImage, Architecture: node.Status.NodeInfo.Architecture, Created: node.Metadata.CreationTimestamp, Unschedulable: node.Spec.Unschedulable, Capacity: *resources(node.Status.Capacity), Allocatable: *resources(node.Status.Allocatable), Usage: usage}
	for _, c := range node.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			info.Ready = true
		}
	}
	for label := range node.Metadata.Labels {
		if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && role != "" {
			info.Roles = append(info.Roles, role)
		}
	}
	sort.Strings(info.Roles)
	return info
}

// resources reads the cpu and memory of a resource list; a quantity that
// does not parse counts as unset.
func resources(list kube.ResourceList) *sandbox.Amounts {
	var r sandbox.Amounts
	if v, err := kube.Milli(list["cpu"]); err == nil {
		r.CPUMilli = v
	}
	if v, err := kube.Bytes(list["memory"]); err == nil {
		r.MemoryBytes = v
	}
	return &r
}

func podUsage(m kube.PodMetricsItem) *sandbox.Amounts {
	var total sandbox.Amounts
	for _, c := range m.Containers {
		total = total.Add(resources(c.Usage))
	}
	return &total
}

// metricsError is the owner-facing form of a refused or absent read.
func metricsError(err error) string {
	switch {
	case kube.IsForbidden(err):
		return "the runner's ServiceAccount may not read this (rbac.runnerClusterView)"
	case kube.IsNotFound(err):
		return "not served by this cluster (no metrics server?)"
	}
	return err.Error()
}
