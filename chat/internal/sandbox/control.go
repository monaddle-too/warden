package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type controlState struct {
	mu        sync.Mutex
	bindings  map[string]Request
	cancel    map[string]context.CancelFunc
	cancelled map[string]bool
}

func (w *Worker) control() *controlState {
	// Initialized by NewWorker, independent of the runtime/lifecycle mutex.
	return w.controls
}
func (w *Worker) registerControlBinding(r Request) {
	c := w.control()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bindings[r.ChatID] = r
}
func (w *Worker) cancelPath(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(w.Root, "cancelled-runs", hex.EncodeToString(h[:])+".json")
}
func (w *Worker) registerRunControl(parent context.Context, r Request) (context.Context, func(), error) {
	c := w.control()
	c.mu.Lock()
	defer c.mu.Unlock()
	key := runKey(r)
	if c.cancelled[key] {
		return nil, nil, errors.New("run was cancelled")
	}
	if _, err := os.Stat(w.cancelPath(key)); err == nil {
		return nil, nil, errors.New("run was cancelled")
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel[key] = cancel
	return ctx, func() { c.mu.Lock(); delete(c.cancel, key); c.mu.Unlock(); cancel() }, nil
}
func (w *Worker) wasExplicitlyCancelled(r Request) bool {
	c := w.control()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled[runKey(r)] {
		return true
	}
	_, err := os.Stat(w.cancelPath(runKey(r)))
	return err == nil
}
func (w *Worker) cancelManaged(r Request) error {
	c := w.control()
	c.mu.Lock()
	b, ok := c.bindings[r.ChatID]
	if !ok || b.ProjectID != r.ProjectID || b.SandboxID != r.SandboxID || b.PrincipalID != r.PrincipalID || !validIdentity(r.RunID) {
		c.mu.Unlock()
		return errors.New("cancel is not authorized for this registered chat")
	}
	key := runKey(r)
	// Commit cancellation before signaling. A stale run's tombstone can never
	// authorize stopping a different active run in this shared sandbox.
	if err := atomicJSON(w.cancelPath(key), true); err != nil {
		c.mu.Unlock()
		return err
	}
	c.cancelled[key] = true
	cancel := c.cancel[key]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Cover cancellation in the gap after prepare returns but before stream opens.
	// This must not wait behind an in-progress creation on the control lane.
	go w.cancelPreparedReservation(r)
	return nil
}
func (w *Worker) cancelPreparedReservation(r Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, _, err := w.bindingLocked(r)
	if err != nil || s.Active == nil || s.Active.Streaming || s.Active.ID != r.RunID || s.Active.ChatID != r.ChatID {
		return
	}
	endCtx, end := context.WithTimeout(context.Background(), 10*time.Second)
	_ = w.Gate.End(endCtx, s.Grant)
	end()
	s.Active = nil
	s.LastActivity = w.now()
	stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	_ = w.stopLocked(stopCtx, s)
}
