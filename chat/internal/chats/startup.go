package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"warden/chat/internal/sandbox"
)

// Startup is where a chat's start is while its message waits for the
// agent (docs/warden-startup-visibility-plan.md): the stage, the runtime's
// detail for it, and when the stage began (unix seconds). It is filled in
// for clients by Engine.View and never stored.
type Startup struct {
	Stage  string  `json:"stage"`
	Detail string  `json:"detail,omitempty"`
	Since  float64 `json:"since"`
}

// The engine's own stages; the runner's (sandbox.Stage*) are copied over
// "preparing" while prepare runs.
const (
	stageQueued        = "queued"        // waiting for a run slot or for the workspace
	stageBinding       = "binding"       // registering the chat with the runner
	stagePreparing     = "preparing"     // prepare in flight, no runner report yet
	stageWaiting       = "waiting"       // the runner answered busy; retrying
	stageLaunching     = "launching"     // the agent process starting in the guest
	stageInitializing  = "initializing"  // the agent's app server answering its first request
	stageConnecting    = "connecting"    // the agent thread starting or resuming
	stageSending       = "sending"       // the message handed to the agent
	stageFirstResponse = "firstResponse" // the turn accepted, nothing back from the model yet
)

// setStartup records the stage a chat's start is at, and how long each
// stage took, for the one log line clearStartup writes.
func (e *Engine) setStartup(id, stage, detail string) {
	e.startupMu.Lock()
	defer e.startupMu.Unlock()
	if e.startup == nil {
		e.startup = map[string]Startup{}
		e.startupTrace = map[string][]string{}
	}
	current, ok := e.startup[id]
	if ok && current.Stage == stage && current.Detail == detail {
		return
	}
	now := float64(e.now().UnixNano()) / 1e9
	since := now
	if ok && current.Stage == stage {
		since = current.Since
	} else if ok {
		e.startupTrace[id] = append(e.startupTrace[id], fmt.Sprintf("%s %.1fs", current.Stage, now-current.Since))
	}
	e.startup[id] = Startup{Stage: stage, Detail: detail, Since: since}
}

// clearStartup ends the chat's start (the agent has answered, or the run
// ended first) and logs where the time went when it took a while, so a
// slow start names its slow step in the chat service's log.
func (e *Engine) clearStartup(id string) {
	e.startupMu.Lock()
	defer e.startupMu.Unlock()
	current, ok := e.startup[id]
	if !ok {
		return
	}
	now := float64(e.now().UnixNano()) / 1e9
	trace := append(e.startupTrace[id], fmt.Sprintf("%s %.1fs", current.Stage, now-current.Since))
	delete(e.startup, id)
	delete(e.startupTrace, id)
	total := 0.0
	for _, step := range trace {
		var stage string
		var seconds float64
		if _, err := fmt.Sscanf(step, "%s %fs", &stage, &seconds); err == nil {
			total += seconds
		}
	}
	if total >= slowStart.Seconds() {
		log.Printf("chat %s: the agent answered after %.1fs: %s", id, total, strings.Join(trace, ", "))
	}
}

// slowStart is the start worth a log line with its stages.
const slowStart = 5 * time.Second

// startupOf returns the chat's startup report, if it is starting.
func (e *Engine) startupOf(id string) *Startup {
	e.startupMu.Lock()
	defer e.startupMu.Unlock()
	if s, ok := e.startup[id]; ok {
		return &s
	}
	return nil
}

// followProgress copies the runner's startup reports for the chat into the
// engine's until stop is closed or ctx ends: two progress calls a second
// on the runner's control lane, which does not wait behind the creation.
func (e *Engine) followProgress(ctx context.Context, c *Chat, stop <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res, err := e.Worker.Call(callCtx, request(c, "progress"))
		cancel()
		if err != nil || res.Progress == nil {
			continue
		}
		e.setStartup(c.ID, res.Progress.Stage, res.Progress.Detail)
	}
}

// busyDetail turns the runner's busy answer into the waiting stage's
// detail.
func busyDetail(err error) string {
	if !errors.Is(err, sandbox.ErrBusy) {
		return ""
	}
	text := err.Error()
	if _, rest, ok := strings.Cut(text, sandbox.ErrBusy.Error()+": "); ok {
		text = rest
	}
	return "waiting for the runner: " + text
}
