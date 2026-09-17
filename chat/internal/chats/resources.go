package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// Workspace size: chosen at creation (Create), changed by the owner from
// the workspace panel (ResizeEnvironment) or by an agent's approved
// request (the warden/sandbox/resources grant). The runner's record is the
// size in force; the chats on the workspace carry the same value so a
// workspace whose sandbox does not exist yet is created at it.

// number reads an approval param that was an int before the store's JSON
// round-trip made it a float64.
func number(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// wardenActor signs messages Warden itself puts in a chat.
var wardenActor = cv.Actor{PrincipalID: "warden", Name: "Warden"}

// ResizeEnvironment gives the workspace a new size, larger or smaller,
// within the runner's limits. Where a resize restarts the sandbox (SBX) no
// chat on it may be running: the owner stops it first, so nothing is
// interrupted behind their back.
func (e *Engine) ResizeEnvironment(ctx context.Context, id string, r *sandbox.Resources) error {
	if r == nil || r.IsZero() {
		return errors.New("a size is required")
	}
	limits := e.Limits(ctx)
	if limits == nil {
		return errors.New("the runner's size limits are unavailable")
	}
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 {
		return errors.New("workspace not found")
	}
	if st.deleted(id) {
		return errors.New("workspace was deleted")
	}
	// A field left out keeps its current value, not the default.
	resolved, err := limits.Resolve(r.Fill(e.currentResources(ctx, chats[0], limits)))
	if err != nil {
		return fmt.Errorf("workspace size: %w", err)
	}
	if limits.Restart {
		if c := busy(chats); c != nil {
			return errors.New("workspace is running chat “" + c.Title + "”; stop that chat first")
		}
	}
	if ran := ranChat(chats); ran != nil {
		if limits.Restart {
			e.releaseSandbox(ctx, id, "")
		}
		if err := e.resizeSandbox(ctx, ran, resolved); err != nil {
			return err
		}
	}
	return e.recordResources(id, resolved)
}

// resizeSandbox asks the runner to resize the chat's sandbox, waiting out a
// resident session that was released moments ago.
func (e *Engine) resizeSandbox(ctx context.Context, c *Chat, r sandbox.Resources) error {
	req := request(c, "resize")
	req.Resources = &r
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		_, err := e.Worker.Call(ctx, req)
		if err == nil || !strings.Contains(err.Error(), "sandbox has an active run") {
			return err
		}
		select {
		case <-waitCtx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// recordResources sets the size on every chat of the workspace.
func (e *Engine) recordResources(sandboxID string, r sandbox.Resources) error {
	return e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			if c.SandboxID == sandboxID {
				size := r
				c.Resources = &size
			}
		}
		return nil
	})
}

// currentResources is the size the runner holds for the chat's sandbox.
func (e *Engine) currentResources(ctx context.Context, c *Chat, limits *sandbox.ResourceLimits) sandbox.Resources {
	if res, err := e.Runtime(ctx, c.ID, "status"); err == nil && res.Sandbox != nil && !res.Sandbox.Resources.IsZero() {
		return res.Sandbox.Resources
	}
	if c.Resources != nil {
		return c.Resources.Fill(limits.Default)
	}
	return limits.Default
}

// resourceRequest validates an agent's request_resources call: at least one
// of cpus and memory_mb, a reason, growth only (the owner can shrink from
// the panel), within the limits. It returns the approval's params, or a
// result to answer at once when nothing would change, or the refusal.
func (e *Engine) resourceRequest(c *Chat, cpus float64, memoryMB int, reason string) (params map[string]any, immediate map[string]any, err error) {
	if cpus == 0 && memoryMB == 0 {
		return nil, nil, errors.New("cpus or memory_mb is required")
	}
	if reason == "" {
		return nil, nil, errors.New("a reason is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	limits := e.Limits(ctx)
	if limits == nil {
		return nil, nil, errors.New("the runner's size limits are unavailable; try again later")
	}
	current := e.currentResources(ctx, c, limits)
	wanted := sandbox.Resources{CPUMilli: sandbox.CPUMilli(cpus), MemoryMB: memoryMB}
	if cpus != 0 && wanted.CPUMilli == 0 {
		return nil, nil, errors.New("cpus must be a positive number")
	}
	wanted = wanted.Fill(current)
	resolved, err := limits.Resolve(wanted)
	if err != nil {
		return nil, nil, fmt.Errorf("%v; this workspace has %s", err, current)
	}
	if resolved.CPUMilli < current.CPUMilli || resolved.MemoryMB < current.MemoryMB {
		return nil, nil, fmt.Errorf("only more can be requested; this workspace has %s and the owner can shrink it from the workspace panel", current)
	}
	if resolved == current {
		return nil, map[string]any{"cpus": float64(current.CPUMilli) / 1000, "memory_mb": current.MemoryMB, "already": true, "note": "this workspace already has " + current.String() + "; no approval was needed"}, nil
	}
	if limits.Restart {
		st := e.Store.Snapshot()
		for _, other := range st.Chats {
			if other.SandboxID == c.SandboxID && other.ID != c.ID && (other.Status == "running" || other.Status == "queued" || other.Status == "stopping") {
				return nil, nil, errors.New("another chat is running on this workspace and a resize restarts it; ask the owner to stop that chat first")
			}
		}
	}
	return map[string]any{"cpu_milli": resolved.CPUMilli, "memory_mb": resolved.MemoryMB, "current_cpu_milli": current.CPUMilli, "current_memory_mb": current.MemoryMB, "reason": reason, "restart": limits.Restart}, nil, nil
}

// resolveResources performs an approved size grant. Live (Kubernetes): the
// runner resizes the pod and the tool result says so. Restarting (SBX): the
// tool result tells the agent the sandbox restarts now, and once the
// answer is delivered the run is stopped, the sandbox regenerated at the
// new size and the chat resumed with a note from Warden; the agent asked
// from inside the instance being replaced, so its process cannot survive.
func (e *Engine) resolveResources(c *Chat, a Approval) any {
	r := sandbox.Resources{CPUMilli: number(a.Params["cpu_milli"]), MemoryMB: number(a.Params["memory_mb"])}
	restart, _ := a.Params["restart"].(bool)
	out := map[string]any{"cpus": float64(r.CPUMilli) / 1000, "memory_mb": r.MemoryMB}
	if !restart {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := e.resizeSandbox(ctx, c, r); err != nil {
			return toolResult(nil, errors.New("the owner approved, but the resize failed: "+err.Error()))
		}
		_ = e.recordResources(c.SandboxID, r)
		out["note"] = "in effect now; the guest may still report its old memory total, the limit is " + r.String()
		return toolResult(out, nil)
	}
	out["note"] = "the sandbox restarts now with " + r.String() + "; your process ends with it, your files and this conversation are kept, and Warden resumes the chat when the sandbox is back"
	go e.restartResized(c.ID, r)
	return toolResult(out, nil)
}

// restartResized is the restarting half of a size grant: it runs after the
// tool result is delivered (Stop takes the engine lock the delivery holds),
// stops the chat, resizes the sandbox and queues Warden's note, which
// resumes the run on the new instance.
func (e *Engine) restartResized(chatID string, r sandbox.Resources) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fail := func(err error) {
		log.Printf("chat %s: resize after approval failed: %v", chatID, err)
		_ = e.Store.update(func(st *State) error {
			if c := st.chat(chatID); c != nil {
				c.Error = "the approved resize to " + r.String() + " failed: " + err.Error()
			}
			return nil
		})
	}
	if err := e.Stop(ctx, chatID); err != nil {
		fail(err)
		return
	}
	st := e.Store.Snapshot()
	c := st.chat(chatID)
	if c == nil {
		return
	}
	if err := e.resizeSandbox(ctx, c, r); err != nil {
		fail(err)
		return
	}
	if err := e.recordResources(c.SandboxID, r); err != nil {
		fail(err)
		return
	}
	if err := e.MessageFrom(chatID, "The sandbox was restarted with "+r.String()+", as approved. Continue where you left off.", cv.ID(), wardenActor); err != nil {
		fail(err)
	}
}
