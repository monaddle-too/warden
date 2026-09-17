package sandbox

import (
	"context"
	"sync"
)

// The registry lock (w.mu) is held for the whole of prepare, which on
// Kubernetes may wait minutes for a node. The reads the workspace panel
// polls (status, usage, pod) must not queue behind it, or the panel goes
// stale exactly when the owner wants to see what is happening: when the
// lock is busy they answer from this snapshot, refreshed at every save of
// the registry, with the identity checked against the control bindings.
type sandboxSnapshot struct {
	Info            SandboxInfo
	Directory, Base string
	Created         bool
}

type snapshotState struct {
	mu        sync.Mutex
	sandboxes map[string]sandboxSnapshot
	usage     map[string]SandboxUsage // the last sample served, by sandbox ID
}

func (w *Worker) snapshotStore() *snapshotState {
	w.snapshotOnce.Do(func() {
		w.snapshots = &snapshotState{sandboxes: map[string]sandboxSnapshot{}, usage: map[string]SandboxUsage{}}
	})
	return w.snapshots
}

// refreshSnapshotsLocked copies every registered sandbox's public state;
// called with w.mu held, from saveManagedLocked.
func (w *Worker) refreshSnapshotsLocked() {
	st := w.snapshotStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sandboxes = make(map[string]sandboxSnapshot, len(w.managed.Sandboxes))
	for id, s := range w.managed.Sandboxes {
		st.sandboxes[id] = sandboxSnapshot{Info: s.SandboxInfo, Directory: s.Directory, Base: s.Base, Created: s.Created || s.Creating}
	}
}

func (w *Worker) rememberUsage(sandboxID string, sample SandboxUsage) {
	st := w.snapshotStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.usage[sandboxID] = sample
}

// snapshotBinding checks the request's identity against the control
// bindings (registered at bind-chat, under their own lock) and returns the
// sandbox's snapshot.
func (w *Worker) snapshotBinding(r Request) (sandboxSnapshot, error) {
	c := w.control()
	c.mu.Lock()
	b, ok := c.bindings[r.ChatID]
	c.mu.Unlock()
	if !ok || b.ProjectID != r.ProjectID || b.SandboxID != r.SandboxID || b.PrincipalID != r.PrincipalID {
		return sandboxSnapshot{}, errBindingRequired
	}
	st := w.snapshotStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	snap, ok := st.sandboxes[r.SandboxID]
	if !ok {
		return sandboxSnapshot{}, errBindingRequired
	}
	return snap, nil
}

// snapshotOp answers status, pod and usage while the registry is busy.
func (w *Worker) snapshotOp(ctx context.Context, r Request) (Response, error) {
	snap, err := w.snapshotBinding(r)
	if err != nil {
		return Response{}, err
	}
	switch r.Operation {
	case "status":
		info := snap.Info
		return Response{Version: ProtocolVersion, Sandbox: &info, Directory: snap.Directory, Base: snap.Base, Attachments: []PreviewAttachment{}}, nil
	case "pod":
		if w.Cluster == nil {
			return Response{}, ErrClusterUnavailable
		}
		if !snap.Created {
			return Response{}, nil
		}
		pod, err := w.Cluster.Pod(ctx, snap.Info.RuntimeName)
		if err != nil {
			return Response{}, err
		}
		return Response{Pod: pod}, nil
	case "usage":
		st := w.snapshotStore()
		st.mu.Lock()
		sample, ok := st.usage[r.SandboxID]
		st.mu.Unlock()
		if !ok {
			return Response{}, ErrBusy
		}
		return Response{Usage: &sample}, nil
	}
	return Response{}, ErrBusy
}
