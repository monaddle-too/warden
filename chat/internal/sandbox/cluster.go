package sandbox

import (
	"context"
	"errors"
	"time"
)

// ClusterInspector is what a driver whose sandboxes are pods can show the
// owner about the cluster (docs/warden-startup-visibility-plan.md): the
// pod behind one sandbox, the whole picture, and pod logs. The Kubernetes
// driver implements it; the SBX driver does not, and the runner answers
// the cluster operations with Available false.
type ClusterInspector interface {
	// Pod describes the pod of the named runtime; nil, nil when it has none
	// (stopped).
	Pod(ctx context.Context, runtimeName string) (*PodInfo, error)
	// Cluster is the nodes, the sandbox pods and the Warden service pods.
	Cluster(ctx context.Context) (*ClusterStatus, error)
	// Logs reads the last lines of one container of one pod the owner may
	// see (a sandbox pod or a Warden service pod).
	Logs(ctx context.Context, q LogQuery) (*PodLogs, error)
}

// PodInfo is one pod as the owner sees it. Usage figures come from
// metrics.k8s.io and are nil without a metrics server.
type PodInfo struct {
	Namespace string     `json:"namespace"`
	Name      string     `json:"name"`
	UID       string     `json:"uid,omitempty"`
	Node      string     `json:"node,omitempty"`
	Phase     string     `json:"phase"`  // Pending, Running, Succeeded, Failed, Unknown
	Reason    string     `json:"reason"` // a waiting or termination reason when not simply running
	Ready     bool       `json:"ready"`
	IP        string     `json:"ip,omitempty"`
	Started   *time.Time `json:"started,omitempty"`
	Restarts  int        `json:"restarts"`
	// RuntimeClass is the pod's RuntimeClass (the tier's handler); empty
	// for the service pods.
	RuntimeClass string   `json:"runtimeClass,omitempty"`
	Containers   []string `json:"containers"`
	// SandboxID and Spare relate a sandbox pod to its workspace: the
	// registry identity the runner created it for, or a spare not yet
	// bound to one.
	SandboxID string `json:"sandboxID,omitempty"`
	Spare     bool   `json:"spare,omitempty"`
	// Component is a Warden service pod's role (policy, runner, chat, edge).
	Component string `json:"component,omitempty"`
	// Requests and Limits are the summed container requests and limits;
	// Usage is the summed live usage from metrics.k8s.io.
	Requests Amounts  `json:"requests"`
	Limits   Amounts  `json:"limits"`
	Usage    *Amounts `json:"usage"`
	// Events are the pod's recent events, newest first: what the
	// scheduler, the autoscaler and the kubelet said about it.
	Events []Event `json:"events"`
}

// Event is one Kubernetes event as the owner sees it: what happened to
// which object, in the reporter's words and, when Hint is set, in the
// owner's. Count is how many times the same event repeated; At is the
// latest.
type Event struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"` // Normal or Warning
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	// Hint is the owner's-words reading of the event, for the reasons
	// that matter to a start (a node being added, none available, the
	// image pulling, a volume that will not mount); empty otherwise.
	Hint  string `json:"hint,omitempty"`
	Count int    `json:"count"`
	// Kind, Namespace and Name are the object the event is about.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Source is the reporting component (kubelet, default-scheduler,
	// cluster-autoscaler).
	Source string `json:"source,omitempty"`
}

// Amounts is a CPU and memory pair as the cluster reports them: millicores
// and bytes. Zero means unset (no request or limit declared). A workspace's
// size is Resources (resources.go).
type Amounts struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// NodeInfo is one node of the cluster.
type NodeInfo struct {
	Name             string     `json:"name"`
	Ready            bool       `json:"ready"`
	Roles            []string   `json:"roles"`
	KubeletVersion   string     `json:"kubeletVersion,omitempty"`
	ContainerRuntime string     `json:"containerRuntime,omitempty"`
	OS               string     `json:"os,omitempty"`
	Architecture     string     `json:"architecture,omitempty"`
	Created          *time.Time `json:"created,omitempty"`
	// Unschedulable is a cordoned node.
	Unschedulable bool    `json:"unschedulable,omitempty"`
	Capacity      Amounts `json:"capacity"`
	Allocatable   Amounts `json:"allocatable"`
	// Usage is the node's live usage from metrics.k8s.io, nil without it.
	Usage *Amounts `json:"usage"`
	// SandboxPods counts the sandbox pods placed on the node.
	SandboxPods int `json:"sandboxPods"`
}

// ClusterStatus is the whole picture: read at At, with Metrics saying
// whether metrics.k8s.io answered (usage fields are nil when it did not,
// and MetricsError says why).
type ClusterStatus struct {
	Available        bool       `json:"available"`
	At               time.Time  `json:"at"`
	Server           string     `json:"server,omitempty"` // the API server's version
	SandboxNamespace string     `json:"sandboxNamespace,omitempty"`
	ServiceNamespace string     `json:"serviceNamespace,omitempty"`
	Tier             string     `json:"tier,omitempty"`
	RuntimeClass     string     `json:"runtimeClass,omitempty"`
	Metrics          bool       `json:"metrics"`
	MetricsError     string     `json:"metricsError,omitempty"`
	Nodes            []NodeInfo `json:"nodes"`
	// NodesError explains empty Nodes when the runner may not list them
	// (rbac.runnerClusterView off).
	NodesError  string    `json:"nodesError,omitempty"`
	SandboxPods []PodInfo `json:"sandboxPods"`
	ServicePods []PodInfo `json:"servicePods"`
	// ServicePodsError explains empty ServicePods when the runner may not
	// list its own namespace.
	ServicePodsError string `json:"servicePodsError,omitempty"`
	// Events are the newest events of the sandbox and service namespaces,
	// newest first; EventsError explains an empty list the runner may not
	// read.
	Events      []Event `json:"events"`
	EventsError string  `json:"eventsError,omitempty"`
}

// Add sums two resource pairs.
func (r Amounts) Add(o *Amounts) Amounts {
	if o == nil {
		return r
	}
	return Amounts{CPUMilli: r.CPUMilli + o.CPUMilli, MemoryBytes: r.MemoryBytes + o.MemoryBytes}
}

// LogQuery selects pod logs. Tail 0 means the default (200); the inspector
// caps it.
type LogQuery struct {
	Namespace, Pod, Container string
	Tail                      int
	Previous                  bool
}

// PodLogs is the tail of one container's log.
type PodLogs struct {
	Namespace  string    `json:"namespace"`
	Pod        string    `json:"pod"`
	Container  string    `json:"container"`
	Containers []string  `json:"containers"`
	Lines      []string  `json:"lines"`
	Truncated  bool      `json:"truncated"`
	At         time.Time `json:"at"`
}

// ErrClusterUnavailable is the answer to a cluster operation on a shape
// whose sandboxes are not pods.
var ErrClusterUnavailable = errors.New("cluster visibility is available on the Kubernetes shape only")

// errBindingRequired is the progress operation's refusal for a chat the
// runner has not bound.
var errBindingRequired = errors.New("chat is not authorized for this project sandbox")

func (w *Worker) clusterOp(ctx context.Context, r Request) (Response, error) {
	if w.Cluster == nil {
		if r.Operation == "cluster.status" {
			return Response{Cluster: &ClusterStatus{At: w.now(), Nodes: []NodeInfo{}, SandboxPods: []PodInfo{}, ServicePods: []PodInfo{}, Events: []Event{}}}, nil
		}
		return Response{}, ErrClusterUnavailable
	}
	switch r.Operation {
	case "cluster.status":
		status, err := w.Cluster.Cluster(ctx)
		if err != nil {
			return Response{}, err
		}
		return Response{Cluster: status}, nil
	case "cluster.logs":
		logs, err := w.Cluster.Logs(ctx, LogQuery{Namespace: r.Namespace, Pod: r.Pod, Container: r.Container, Tail: r.Tail, Previous: r.Previous})
		if err != nil {
			return Response{}, err
		}
		return Response{Logs: logs}, nil
	}
	return Response{}, errors.New("unsupported cluster operation")
}
