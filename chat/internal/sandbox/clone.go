package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A workspace copy (docs/claude-parity.md, R2.13): a chat forked with a
// copy of its workspace gets a new sandbox created now as a copy of the
// source sandbox's disk, through the driver's clone path — on SBX a
// template saved from the stopped source (runtime.go createFrom), on
// Kubernetes a volume clone with a tar copy from the source pod as the
// fallback (kube/driver.go Create). The copy is registered like a
// sandbox bound by bind-chat, at the source's size, with the source's
// repository binding and checkpoint records (both are on the disk), and
// left stopped: its first prepare resumes it like any stopped sandbox and
// installs whatever runtime the snapshot lacks.

// cloneLocked handles the `clone` operation: r.SandboxID and r.ChatID are
// the new sandbox and its first chat, r.Source the sandbox to copy, all in
// r.ProjectID for r.PrincipalID. A source with an active run is refused
// (ErrBusy: the chat service releases the workspace's idle sessions
// first); one the driver needs stopped for the copy is stopped and
// started again after; one it needs resident is started and stopped
// again. The response is the copy's status.
func (w *Worker) cloneLocked(ctx context.Context, r Request) (Response, error) {
	if !validIdentity(r.ProjectID) || !validIdentity(r.ChatID) || !validIdentity(r.SandboxID) || !validIdentity(r.PrincipalID) || !validIdentity(r.Source) {
		return Response{}, errors.New("valid project, chat, sandbox, source and principal IDs required")
	}
	if r.Source == r.SandboxID {
		return Response{}, errors.New("a sandbox cannot be copied onto itself")
	}
	if w.managed.Sandboxes[r.SandboxID] != nil || w.managed.Chats[r.ChatID] != nil {
		return Response{}, errors.New("the copy's sandbox or chat is already registered")
	}
	src := w.managed.Sandboxes[r.Source]
	if src == nil || src.ProjectID != r.ProjectID || src.PrincipalID != r.PrincipalID {
		return Response{}, errors.New("source sandbox is not registered for this project and principal")
	}
	if !src.Created || src.Creating {
		return Response{}, errors.New("the workspace has no sandbox to copy yet; its first message creates one")
	}
	if src.Active != nil {
		return Response{}, fmt.Errorf("%w: the source workspace has a running chat", ErrBusy)
	}
	if src.Reviewing {
		return Response{}, ErrBusy
	}
	if len(w.managed.Sandboxes) >= 32 {
		return Response{}, errors.New("worker has 32 retained sandboxes")
	}
	s := &managedSandbox{SandboxInfo: SandboxInfo{ID: r.SandboxID, ProjectID: r.ProjectID, RuntimeName: RuntimeName(w.Instance, r.SandboxID), Directory: src.Directory, State: "stopped", Resources: w.resourcesOf(src)}, PrincipalID: r.PrincipalID, Source: src.RuntimeName, Repository: src.Repository, RepositoryCheckout: src.RepositoryCheckout, RepositoryReady: src.RepositoryReady, Checkpoints: append([]Checkpoint(nil), src.Checkpoints...), LastActivity: w.now()}
	c := &chatBinding{ID: r.ChatID, ProjectID: r.ProjectID, SandboxID: r.SandboxID}
	// The source as the driver needs it: SBX snapshots a stopped sandbox
	// (its residency would be cut by the stop, so it goes first); a pod
	// driver clones the volume and copies from the source pod when the
	// storage refuses, so the pod must be there.
	restore := func() {}
	if w.Limits.Restart {
		if src.State == "running" || src.State == "starting" || src.residency != nil {
			if err := w.stopLocked(ctx, src); err != nil {
				return Response{}, fmt.Errorf("stopping the source workspace for the copy: %w", err)
			}
			restore = func() {
				if err := w.startLocked(ctx, src); err != nil {
					src.State = "stopped"
					_ = w.saveManagedLocked()
				}
			}
		}
	} else if src.State != "running" {
		if err := w.startLocked(ctx, src); err != nil {
			return Response{}, fmt.Errorf("starting the source workspace for the copy: %w", err)
		}
		restore = func() { _ = w.stopLocked(ctx, src) }
	}
	w.setProgress(s.ID, StageCreating, "copying the workspace")
	defer w.clearProgress(s.ID)
	s.Creating = true
	err := w.Runtime.Create(WithProgress(ctx, func(detail string) { w.setProgress(s.ID, StageCreating, detail) }), w.specOf(s))
	restore()
	if err != nil {
		// Whatever the driver left under the name goes with the failure.
		removeCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
		_ = w.Runtime.Remove(removeCtx, s.RuntimeName)
		done()
		return Response{}, fmt.Errorf("copying the workspace: %w", err)
	}
	s.Created, s.Creating, s.fresh = true, false, true
	// The copy runs the snapshot's image (SBX): the policy service's pin
	// takes the digest from the runner.
	w.recordImageLocked(ctx, s)
	// A created sandbox boots; the copy waits stopped for its first chat.
	if err := w.Runtime.Stop(ctx, s.RuntimeName); err != nil {
		s.State = "error"
	}
	w.managed.Sandboxes[s.ID] = s
	w.managed.Chats[c.ID] = c
	w.registerControlBinding(r)
	if err := w.saveManagedLocked(); err != nil {
		return Response{}, err
	}
	return w.statusLocked(r), nil
}
