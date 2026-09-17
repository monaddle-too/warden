package chats

import (
	"context"
	"net/http"
	"strconv"
	"warden/chat/internal/sandbox"
)

// Cluster is what the owner sees of the Kubernetes cluster
// (docs/warden-startup-visibility-plan.md): the runner's view, with each
// sandbox pod related to the workspace it belongs to. On the sbx shapes
// Available is false and the lists are empty.
type Cluster struct {
	sandbox.ClusterStatus
	// Workspaces names the workspace behind each sandbox pod's sandbox ID
	// (the first chat's title), so the page can say which is which.
	Workspaces map[string]string `json:"workspaces"`
}

func (e *Engine) Cluster(ctx context.Context) (*Cluster, error) {
	res, err := e.Worker.Call(ctx, sandbox.Request{Operation: "cluster.status", ProjectID: "warden-local", PrincipalID: "owner"})
	if err != nil {
		return nil, err
	}
	out := &Cluster{Workspaces: map[string]string{}}
	if res.Cluster != nil {
		out.ClusterStatus = *res.Cluster
	}
	if out.Nodes == nil {
		out.Nodes = []sandbox.NodeInfo{}
	}
	if out.SandboxPods == nil {
		out.SandboxPods = []sandbox.PodInfo{}
	}
	if out.ServicePods == nil {
		out.ServicePods = []sandbox.PodInfo{}
	}
	st := e.Store.Snapshot()
	for _, p := range out.SandboxPods {
		if p.SandboxID == "" {
			continue
		}
		if chats := st.environmentChats(p.SandboxID); len(chats) > 0 {
			out.Workspaces[p.SandboxID] = chats[0].Title
		}
	}
	return out, nil
}

// ClusterLogs reads the tail of one pod's log through the runner.
func (e *Engine) ClusterLogs(ctx context.Context, q sandbox.LogQuery) (*sandbox.PodLogs, error) {
	res, err := e.Worker.Call(ctx, sandbox.Request{Operation: "cluster.logs", ProjectID: "warden-local", PrincipalID: "owner", Namespace: q.Namespace, Pod: q.Pod, Container: q.Container, Tail: q.Tail, Previous: q.Previous})
	if err != nil {
		return nil, err
	}
	return res.Logs, nil
}

func (h *HTTP) clusterHTTP(w http.ResponseWriter, r *http.Request, path string) {
	if path == "cluster" {
		result, err := h.Engine.Cluster(r.Context())
		respond(w, result, err)
		return
	}
	query := r.URL.Query()
	q := sandbox.LogQuery{Namespace: query.Get("namespace"), Pod: query.Get("pod"), Container: query.Get("container"), Previous: query.Get("previous") == "true"}
	if len(q.Namespace) > 253 || len(q.Pod) > 253 || len(q.Container) > 253 || q.Pod == "" {
		http.Error(w, "pod required", 400)
		return
	}
	if tail := query.Get("tail"); tail != "" {
		n, err := strconv.Atoi(tail)
		if err != nil || n < 1 || n > 100000 {
			http.Error(w, "invalid tail", 400)
			return
		}
		q.Tail = n
	}
	result, err := h.Engine.ClusterLogs(r.Context(), q)
	respond(w, result, err)
}
