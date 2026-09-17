package sandbox

import (
	"context"
	"sync"
	"time"
)

// Progress is where a sandbox's start is: the stage the worker is in and
// the runtime's own detail for it ("scheduling: 0/3 nodes are available…",
// "pulling the guest image"), since when. It is never persisted; a restart
// clears it with the run. The stages are listed in the plan
// (docs/warden-startup-visibility-plan.md); the chat engine adds its own
// before and after prepare.
type Progress struct {
	Stage  string    `json:"stage"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"`
}

// Stages the runner reports during prepare, in the order a cold start
// passes through them.
const (
	StageWaiting    = "waiting"    // a capacity or policy gate before the sandbox is touched
	StageCreating   = "creating"   // a new sandbox: volume, pod, scheduling, image, container
	StageResuming   = "resuming"   // a stopped sandbox's pod or VM coming back
	StageAttesting  = "attesting"  // the policy service checks the runtime's networking
	StageProbing    = "probing"    // reading the guest report
	StageInstalling = "installing" // copying the Codex bundle or Claude into the guest
	StageCloning    = "cloning"    // fetching the workspace repository
)

// progressState is the worker's startup reports by sandbox ID, under its
// own lock so a reader never waits behind prepare, which holds w.mu.
type progressState struct {
	mu      sync.Mutex
	reports map[string]Progress
}

func (w *Worker) setProgress(sandboxID, stage, detail string) {
	p := w.progressStore()
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.reports[sandboxID]
	if ok && current.Stage == stage && current.Detail == detail {
		return
	}
	since := w.now()
	if ok && current.Stage == stage {
		since = current.Since // a detail change keeps the stage's start
	}
	p.reports[sandboxID] = Progress{Stage: stage, Detail: detail, Since: since}
}

func (w *Worker) clearProgress(sandboxID string) {
	p := w.progressStore()
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.reports, sandboxID)
}

// Progress reports the current startup stage of a sandbox, if one is in
// progress.
func (w *Worker) Progress(sandboxID string) (Progress, bool) {
	p := w.progressStore()
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.reports[sandboxID]
	return current, ok
}

func (w *Worker) progressStore() *progressState {
	w.progressOnce.Do(func() { w.progress = &progressState{reports: map[string]Progress{}} })
	return w.progress
}

// progressOp answers the progress operation: the identity check is against
// the control bindings, not the managed registry, so it never queues
// behind a creation.
func (w *Worker) progressOp(r Request) (Response, error) {
	c := w.control()
	c.mu.Lock()
	b, ok := c.bindings[r.ChatID]
	c.mu.Unlock()
	if !ok || b.ProjectID != r.ProjectID || b.SandboxID != r.SandboxID || b.PrincipalID != r.PrincipalID {
		return Response{}, errBindingRequired
	}
	if p, ok := w.Progress(r.SandboxID); ok {
		return Response{Progress: &p}, nil
	}
	return Response{}, nil
}

// reporter is the context value a driver reports sub-stage detail through.
type reporterKey struct{}

// WithProgress returns a context whose Report calls reach fn. The worker
// installs one around Create and Prepare; a driver that has nothing to say
// leaves it unused.
func WithProgress(ctx context.Context, fn func(detail string)) context.Context {
	return context.WithValue(ctx, reporterKey{}, fn)
}

// Report tells the worker what a driver is waiting for now; a context
// without a reporter ignores it.
func Report(ctx context.Context, detail string) {
	if fn, ok := ctx.Value(reporterKey{}).(func(string)); ok && fn != nil {
		fn(detail)
	}
}
