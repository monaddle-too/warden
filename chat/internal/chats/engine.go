package chats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/transport"
)

type Worker interface {
	Call(context.Context, sandbox.Request) (sandbox.Response, error)
	Open(context.Context, sandbox.Request) (io.ReadWriteCloser, sandbox.Response, error)
}
type activeRun struct {
	sharingResults []string
	id, runID      string
	sandboxID      string
	cancel         context.CancelFunc
	client         *agent.Client
	// A resident session keeps its agent process, worker run and lease open
	// between turns. idle is set while it waits for the chat's next message;
	// release ends that wait so the run finishes cleanly.
	resident bool
	idle     atomic.Bool
	release  context.CancelFunc
	done     chan struct{}
}
type Engine struct {
	// PolicyAddress is the policy service's control endpoint (a unix:// or
	// tls:// URL; "" leaves sharing unconfigured), dialed with PolicyTLS on
	// tls://.
	PolicyAddress       string
	PolicyTLS           *transport.TLS
	PublicPreviewSuffix string
	// RunnerPreviewHost is the host:port of the runner's shared preview
	// server (services.runner.previews.address; docs/warden-kubernetes-plan.md,
	// decisions 5 and 10), the only https:// origin an attachment URL may
	// name, dialed with RunnerPreviewTLS (this service's certificate; the
	// runner admits only warden-chat). "" on the sbx shapes, where the
	// runner hands out http://127.0.0.1:<port>/ loopback URLs and nothing
	// else is accepted.
	RunnerPreviewHost string
	RunnerPreviewTLS  *transport.TLS
	previewMu         sync.Mutex
	previewTransport  *http.Transport
	// PreviewScheme and PreviewPort shape approved binding URLs:
	// <scheme>://<binding-id>.<suffix>[:port]/path. The scheme defaults to
	// https (public previews); loopback previews use http and the edge port.
	PreviewScheme string
	PreviewPort   string
	Store         *Store
	Worker        Worker
	// ResidentProviders keep their session open between turns (default: Claude and Codex).
	// ResidentIdle bounds how long an idle session stays open (default 10 minutes).
	// SteeringProviders accept a message into the running turn (default: Codex);
	// others queue it for the next turn.
	ResidentProviders []string
	ResidentIdle      time.Duration
	SteeringProviders []string
	// LocalMode: a single-owner install (auth.mode owner). Host directory
	// grants exist only there.
	LocalMode bool
	// Now is the clock (tests replace it); nil means time.Now.
	Now func() time.Time
	// typing: chat id -> principal -> indicator, see Typing.
	typingMu sync.Mutex
	typing   map[string]map[string]Typist
	// startup: chat id -> where its start is, see startup.go.
	startupMu sync.Mutex
	startup   map[string]Startup
	mu        sync.Mutex
	active    map[string]*activeRun
	wake      chan struct{}
	done      chan struct{}
}

const runSlots = 2

func (e *Engine) resident(provider string) bool {
	return listed(provider, e.ResidentProviders, "claude", "codex")
}
func (e *Engine) steers(provider string) bool {
	return listed(provider, e.SteeringProviders, "codex")
}
func listed(provider string, providers []string, fallback ...string) bool {
	if providers == nil {
		providers = fallback
	}
	for _, p := range providers {
		if p == provider {
			return true
		}
	}
	return false
}
func (e *Engine) residentIdle() time.Duration {
	if e.ResidentIdle > 0 {
		return e.ResidentIdle
	}
	return 10 * time.Minute
}

// releaseIdle asks a resident session that is waiting between turns to end.
func (e *Engine) releaseIdle(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.active[id]
	if a == nil || !a.resident || !a.idle.Load() || a.release == nil {
		return false
	}
	a.release()
	return true
}

// releaseSessions ends idle resident sessions not selected by keep and waits
// for their runs to finish, so the worker sees the sandbox free before the
// caller stops it or starts another chat on it.
func (e *Engine) releaseSessions(ctx context.Context, keep func(a *activeRun) bool) {
	e.mu.Lock()
	var waits []chan struct{}
	for _, a := range e.active {
		if keep(a) || !a.resident || !a.idle.Load() || a.release == nil {
			continue
		}
		a.release()
		waits = append(waits, a.done)
	}
	e.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
			return
		}
	}
}
func (e *Engine) releaseSandbox(ctx context.Context, sandboxID, exceptChat string) {
	e.releaseSessions(ctx, func(a *activeRun) bool { return a.sandboxID != sandboxID || a.id == exceptChat })
}
func (e *Engine) releaseChat(ctx context.Context, id string) {
	e.releaseSessions(ctx, func(a *activeRun) bool { return a.id != id })
}

func NewEngine(s *Store, w Worker) *Engine {
	return &Engine{Store: s, Worker: w, wake: make(chan struct{}, 1), done: make(chan struct{})}
}
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) Serve(ctx context.Context) {
	defer close(e.done)
	if e.PolicyAddress != "" {
		deliveryCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); e.sharingDelivery(deliveryCtx) }()
		defer func() { cancel(); <-done }()
	}
	running := map[string]string{}
	finished := make(chan string, runSlots)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		for _, c := range e.Store.Snapshot().Chats {
			if c.Status != "queued" || ctx.Err() != nil {
				if _, own := running[c.ID]; !own {
					e.clearStartup(c.ID) // stopped or failed before its run began
				}
				continue
			}
			if _, own := running[c.ID]; own {
				continue // the chat's live resident session takes the message itself
			}
			blocked := ""
			for id, sandboxID := range running {
				if sandboxID == c.SandboxID {
					blocked = "another chat on this workspace is running"
					e.releaseIdle(id) // an idle session on this sandbox yields to the waiting chat
				}
			}
			if blocked == "" && len(running) >= runSlots {
				blocked = "waiting for a free agent slot"
				for id := range running {
					if e.releaseIdle(id) {
						break
					}
				}
			}
			if blocked != "" {
				e.setStartup(c.ID, stageQueued, blocked)
				continue
			}
			id := c.ID
			running[id] = c.SandboxID
			workers.Add(1)
			go func() { defer workers.Done(); e.run(ctx, id); finished <- id }()
		}
		select {
		case <-ctx.Done():
			return
		case id := <-finished:
			delete(running, id)
		case <-e.wake:
		}
	}
}

func (e *Engine) Create(title, shared, repository string, selection ...string) (string, error) {
	provider, model := "codex", ""
	if len(selection) > 0 {
		provider = selection[0]
	}
	if len(selection) > 1 {
		model = selection[1]
	}
	if err := sandbox.ValidateAgent(provider, model); err != nil {
		return "", err
	}
	if provider == "" {
		provider = "codex"
	}
	id := cv.ID()
	err := e.Store.update(func(st *State) error {
		title = strings.TrimSpace(title)
		if title == "" {
			title = "New chat"
		}
		if len(title) > 160 || len(repository) > 300 {
			return errors.New("title or repository too long")
		}
		sbxID := cv.ID()
		if shared != "" {
			if st.deleted(shared) {
				return errors.New("workspace was deleted")
			}
			found := false
			for _, c := range st.Chats {
				if c.SandboxID == shared {
					sbxID = shared
					repository = c.Repository
					found = true
					break
				}
			}
			if !found {
				return errors.New("unknown workspace")
			}
		}
		st.Chats = append(st.Chats, &Chat{ID: id, Provider: provider, Model: model, Title: title, SandboxID: sbxID, Repository: repository, Status: "idle", Conversation: cv.Conversation{Entries: []cv.Entry{}}, Approvals: []Approval{}})
		return nil
	})
	return id, err
}
func (e *Engine) Message(id, text, messageID string) error {
	return e.MessageFrom(id, text, messageID, cv.Actor{PrincipalID: "owner"})
}

// MessageFrom appends a user message attributed to actor (the person the
// edge identified, or the owner) and clears that person's typing indicator.
func (e *Engine) MessageFrom(id, text, messageID string, actor cv.Actor) error {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	defer e.stopTyping(id, actor.PrincipalID)
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 128<<10 || len(messageID) != 32 {
		return errors.New("valid message and message ID required")
	}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		for _, v := range c.Conversation.Entries {
			if v.ID == messageID {
				if v.Role == "user" && v.Text == text {
					return nil
				}
				return errors.New("message ID conflict")
			}
		}
		if st.deleted(c.SandboxID) {
			return errors.New("this chat's workspace was deleted; start a new chat")
		}
		if c.Archived || c.Status == "stopping" {
			return errors.New("chat is archived or stopping")
		}
		v := cv.NewEntry("user", text)
		v.ID = messageID
		sender := actor
		v.Sender = &sender
		v.Delivery = "queued"
		c.Conversation.Entries = append(c.Conversation.Entries, v)
		if c.Status != "running" && c.Status != "queued" {
			// A live resident session picks the message up under its own run ID;
			// a fresh run assigns a new one when it starts.
			c.Status = "queued"
			c.Error = ""
		}
		return nil
	})
	if err == nil {
		e.Wake()
	}
	return err
}

// TypingTTL is how long a typing indicator outlives the last keystroke.
const TypingTTL = 8 * time.Second

// Typing records that actor is composing a message in chat id: the
// indicator shows to everyone else for TypingTTL after each keystroke the
// client reports. Nothing is stored; a restart forgets it.
func (e *Engine) Typing(id string, actor cv.Actor) error {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	if e.Store.Snapshot().chat(id) == nil {
		return errors.New("chat not found")
	}
	label := actor.Name
	if label == "" {
		label = actor.Email
	}
	if label == "" {
		label = "Someone"
	}
	e.typingMu.Lock()
	defer e.typingMu.Unlock()
	if e.typing == nil {
		e.typing = map[string]map[string]Typist{}
	}
	if e.typing[id] == nil {
		e.typing[id] = map[string]Typist{}
	}
	e.typing[id][actor.PrincipalID] = Typist{PrincipalID: actor.PrincipalID, Name: label, Until: float64(e.now().Add(TypingTTL).UnixNano()) / 1e9}
	return nil
}

func (e *Engine) stopTyping(id, principal string) {
	e.typingMu.Lock()
	defer e.typingMu.Unlock()
	if e.typing[id] != nil {
		delete(e.typing[id], principal)
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// View is the state clients see: the stored snapshot plus who is typing
// in each chat, expired indicators dropped.
func (e *Engine) View() State {
	st := e.Store.Snapshot()
	now := float64(e.now().UnixNano()) / 1e9
	for _, c := range st.Chats {
		if c.Status == "queued" || c.Status == "running" {
			c.Startup = e.startupOf(c.ID)
		}
	}
	e.typingMu.Lock()
	defer e.typingMu.Unlock()
	for _, c := range st.Chats {
		people := e.typing[c.ID]
		if len(people) == 0 {
			continue
		}
		var list []Typist
		for principal, t := range people {
			if t.Until <= now {
				delete(people, principal)
				continue
			}
			list = append(list, t)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		c.Typing = list
	}
	return st
}

func (e *Engine) Edit(id, title string, archived bool) error {
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if strings.TrimSpace(title) == "" || len(title) > 160 {
			return errors.New("title required (160 characters maximum)")
		}
		if archived && (c.Status == "running" || c.Status == "queued" || c.Status == "stopping") {
			return errors.New("stop the chat before archiving")
		}
		if !archived && c.Archived && st.deleted(c.SandboxID) {
			return errors.New("this chat's workspace was deleted; start a new chat")
		}
		c.Title = title
		c.Archived = archived
		return nil
	})
	if err == nil && archived {
		e.releaseChat(context.Background(), id)
	}
	return err
}
func (e *Engine) Stop(ctx context.Context, id string) error {
	var cpy Chat
	snapshot := e.Store.Snapshot()
	target := snapshot.chat(id)
	if target != nil {
		for _, other := range snapshot.Chats {
			if other.ID != id && other.SandboxID == target.SandboxID && (other.Status == "running" || other.Status == "stopping") {
				return errors.New("workspace is running another chat; stop that chat first")
			}
		}
	}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if c.Status == "stopping" {
			return errors.New("stop already pending")
		}
		c.Status = "stopping"
		cpy = *c
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.Delivery == "queued" {
				v.Delivery = "failed"
				v.Detail = "Stopped before delivery"
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Tombstone the run before disconnecting its stream, so worker cleanup stops
	// the environment instead of treating the disconnect as normal completion.
	if cpy.RunID != "" {
		_, err = e.Worker.Call(ctx, request(&cpy, "cancel"))
	}
	e.mu.Lock()
	if a := e.active[id]; a != nil {
		a.cancel()
	}
	e.mu.Unlock()
	e.releaseSandbox(ctx, cpy.SandboxID, id)
	if err == nil {
		stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for {
			_, err = e.Worker.Call(stopCtx, request(&cpy, "stop"))
			if err == nil || !strings.Contains(err.Error(), "sandbox has an active run") {
				break
			}
			select {
			case <-stopCtx.Done():
				err = stopCtx.Err()
			case <-time.After(100 * time.Millisecond):
				continue
			}
			break
		}
	}

	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c.RunID == cpy.RunID {
			c.Status = "interrupted"
			c.Conversation.ActiveTurnID = nil
			if err != nil {
				c.Error = err.Error()
			}
		}
		return nil
	})
	return err
}
func (e *Engine) Runtime(ctx context.Context, id, op string) (sandbox.Response, error) {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return sandbox.Response{}, errors.New("chat not found")
	}
	r := request(c, op)
	res, err := e.Worker.Call(ctx, r)
	if err == nil && res.Sandbox != nil && (res.Sandbox.ID != c.SandboxID || res.Sandbox.ProjectID != "warden-local") {
		return sandbox.Response{}, errors.New("worker returned a different sandbox")
	}
	for _, a := range res.Attachments {
		if a.ChatID != c.ID || a.SandboxID != c.SandboxID {
			return sandbox.Response{}, errors.New("preview binding mismatch")
		}
		if invalid := e.validateAttachment(a); invalid != nil {
			return sandbox.Response{}, invalid
		}
	}
	return res, err
}
func (e *Engine) run(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var current Chat
	e.mu.Lock()
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || c.Status != "queued" {
			return errors.New("chat no longer queued")
		}
		c.Status = "running"
		c.RunID = cv.ID()
		current = *c
		return nil
	})
	if err != nil {
		e.mu.Unlock()
		return
	}
	a := &activeRun{id: id, runID: current.RunID, sandboxID: current.SandboxID, cancel: cancel, resident: e.resident(current.Provider), done: make(chan struct{})}
	if e.active == nil {
		e.active = make(map[string]*activeRun)
	}
	e.active[id] = a
	e.mu.Unlock()
	defer close(a.done)
	e.setStartup(id, stageBinding, "")
	defer e.clearStartup(id)
	defer func() {
		if err != nil {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = e.Worker.Call(cleanup, request(&current, "cancel"))
			done()
		}
		e.mu.Lock()
		sharingResults := append([]string(nil), a.sharingResults...)
		e.mu.Unlock()
		if err == nil {
			for _, requestID := range sharingResults {
				_, _ = e.sharingCall(parent, "ack", map[string]any{"id": requestID})
			}
		}
		e.mu.Lock()
		delete(e.active, id)
		e.mu.Unlock()
		_ = e.Store.update(func(st *State) error {
			c := st.chat(id)
			if c.RunID != current.RunID {
				return nil
			}
			for i := range c.Approvals {
				if c.Approvals[i].State == "pending" || c.Approvals[i].State == "resolving" {
					c.Approvals[i].State = "expired"
				}
			}
			c.Conversation.ActiveTurnID = nil
			for i := range c.Conversation.Entries {
				v := &c.Conversation.Entries[i]
				v.IsStreaming = false
				if err != nil && v.Delivery == "queued" {
					v.Delivery = "failed"
					v.Detail = "Not delivered"
				}
			}
			if c.Status == "stopping" || c.Status == "interrupted" {
				return nil
			}
			c.Status = "idle"
			if err != nil {
				c.Status = "failed"
				c.Error = err.Error()
			} else {
				for _, v := range c.Conversation.Entries {
					if v.Delivery == "queued" {
						c.Status = "queued"
						c.RunID = cv.ID()
						e.Wake()
						break
					}
				}
			}
			return nil
		})
	}()
	r := request(&current, "bind-chat")
	r.Repository = current.Repository
	if _, err = e.Worker.Call(ctx, r); err != nil {
		return
	}
	r = request(&current, "prepare")
	if current.Conversation.ThreadID != nil {
		r.ThreadID = *current.Conversation.ThreadID
	}
	var prep sandbox.Response
	// The runner reports its stages (sandbox creation, the boot, the guest
	// provisioning) while prepare runs; a follower copies them to the chat.
	e.setStartup(id, stagePreparing, "")
	following := make(chan struct{})
	followed := make(chan struct{})
	go func() { defer close(followed); e.followProgress(ctx, &current, following) }()
	// A resident session released moments ago may still be finishing on the
	// worker; wait briefly for the sandbox instead of failing the chat.
	for start := time.Now(); ; {
		prep, err = e.Worker.Call(ctx, r)
		if err == nil || !errors.Is(err, sandbox.ErrBusy) || time.Since(start) > 15*time.Second {
			break
		}
		e.setStartup(id, stageWaiting, busyDetail(err))
		select {
		case <-ctx.Done():
			err = ctx.Err()
			close(following)
			<-followed
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	close(following)
	<-followed
	if err != nil {
		return
	}
	if prep.Directory == "" {
		err = errors.New("worker returned no sandbox workspace")
		return
	}
	e.setStartup(id, stageLaunching, "starting the agent in the sandbox")
	r = request(&current, "stream")
	r.Directory = prep.Directory
	var stream io.ReadWriteCloser
	stream, _, err = e.Worker.Open(ctx, r)
	if err != nil {
		return
	}
	if current.Provider == "claude" {
		stream = agent.ClaudeStream(ctx, stream)
	}
	frames := make(chan agent.Frame, 256)
	var client *agent.Client
	client, err = agent.StartStream(ctx, stream, func(_ *agent.Client, f agent.Frame) {
		select {
		case frames <- f:
		case <-ctx.Done():
		}
	})
	if err != nil {
		stream.Close()
		return
	}
	defer client.Close()
	e.mu.Lock()
	a.client = client
	e.mu.Unlock()
	params := map[string]any{"cwd": prep.Directory, "approvalPolicy": "on-request", "sandbox": "danger-full-access", "developerInstructions": "You are an agent in a Warden-managed sandbox. The files, shared documents and shared repositories belong to this workspace and are visible to every chat in it; preserve other chats' files. Warden controls external access. Do not request or expose host credentials. To show a web preview, start the server as a detached process on 0.0.0.0 inside this sandbox (for example subprocess.Popen with start_new_session=True and stdio redirected to files), then call preview_attach with port, path beginning /, and title. The controller chooses the URL.", "ephemeral": false, "historyMode": "legacy"}
	params["modelProvider"] = "warden"
	if current.Model != "" {
		params["model"] = current.Model
	}
	params["runtimeWorkspaceRoots"] = []string{prep.Directory}
	params["approvalsReviewer"] = "user"
	tools := append(append(previewTools(), sharingTools()...), grantTools(e.LocalMode)...)
	params["dynamicTools"] = tools
	if e.PublicPreviewSuffix != "" {
		copy := agent.Map(previewTools()[0])
		copy["name"] = "sandbox_bind_port"
		copy["description"] = "Request to bind a port inside this sandbox to an externally reachable URL. Warden asks the owner for approval and requires viewers to sign in. Start the server on 0.0.0.0 before requesting."
		params["dynamicTools"] = append(tools, copy)
	}
	method := "thread/start"
	if current.Conversation.ThreadID != nil {
		method = "thread/resume"
		params["threadId"] = *current.Conversation.ThreadID
		delete(params, "ephemeral")
		delete(params, "historyMode")
		if prep.RolloutPath != "" {
			params["path"] = prep.RolloutPath
		}
	}
	if method == "thread/resume" {
		e.setStartup(id, stageConnecting, "resuming the agent session")
	} else {
		e.setStartup(id, stageConnecting, "starting the agent session")
	}
	var response map[string]any
	response, err = client.Call(ctx, method, params)
	if err != nil {
		return
	}
	thread := agent.Map(response["thread"])
	threadID := agent.String(thread["id"])
	if threadID == "" {
		err = errors.New("agent returned no thread ID")
		return
	}
	err = e.Store.update(func(st *State) error { st.chat(id).Conversation.Hydrate(thread); return nil })
	if err != nil {
		return
	}
	var message *cv.Entry
	message, err = e.attempt(id, "")
	if err != nil {
		return
	}
	if message == nil {
		err = errors.New("no pending message")
		return
	}
	response, err = client.Call(ctx, "turn/start", map[string]any{"threadId": threadID, "clientUserMessageId": message.ID, "cwd": prep.Directory, "approvalPolicy": "on-request", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}, "input": input(*message)})
	if err != nil {
		return
	}
	turn := agent.Map(response["turn"])
	turnID := agent.String(turn["id"])
	if turnID == "" {
		err = errors.New("agent returned no turn ID")
		return
	}
	if err = e.confirm(id, message.ID, turnID); err != nil {
		return
	}
	e.clearStartup(id)
	for {
		if err = e.turn(ctx, id, &current, client, frames, threadID, turnID, turn); err != nil || !a.resident {
			return
		}
		// The turn finished but the session stays open: settle the transcript,
		// report idle, and wait for the chat's next message.
		e.settleTurn(parent, id, a)
		message = e.awaitMessage(ctx, id, &current, a, client, frames)
		if message == nil {
			return // released, timed out, stopped or ended by the worker: a clean end
		}
		response, err = client.Call(ctx, "turn/start", map[string]any{"threadId": threadID, "clientUserMessageId": message.ID, "cwd": prep.Directory, "approvalPolicy": "on-request", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}, "input": input(*message)})
		if err != nil {
			return
		}
		turn = agent.Map(response["turn"])
		turnID = agent.String(turn["id"])
		if turnID == "" {
			err = errors.New("agent returned no turn ID")
			return
		}
		if err = e.confirm(id, message.ID, turnID); err != nil {
			return
		}
	}
}

// turn drives one agent turn to completion. It returns nil once the turn
// completed and an error when it failed, the run was cancelled or the agent
// stream ended.
func (e *Engine) turn(ctx context.Context, id string, current *Chat, client *agent.Client, frames chan agent.Frame, threadID, turnID string, turn map[string]any) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if turn["status"] == "completed" {
			return nil
		}
		if turn["status"] == "failed" || turn["status"] == "interrupted" {
			return fmt.Errorf("agent turn %s", turn["status"])
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-client.Done():
			return client.Err()
		case f := <-frames:
			var err error
			if len(f.ID) > 0 {
				err = e.request(ctx, current, client, f)
			} else {
				err = e.notification(id, f)
			}
			if err != nil {
				return err
			}
			if f.Method == "turn/completed" && agent.String(agent.Map(f.Params["turn"])["id"]) == turnID {
				turn = agent.Map(f.Params["turn"])
			}
		case <-tick.C:
			if !e.steers(current.Provider) {
				continue // Claude queues a separate turn; it does not implement Codex steering.
			}
			message, err := e.attempt(id, turnID)
			if err != nil {
				return err
			}
			if message != nil {
				result, callErr := client.Call(ctx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "clientUserMessageId": message.ID, "input": input(*message)})
				if callErr == nil && agent.String(result["turnId"]) == turnID {
					if err = e.confirm(id, message.ID, turnID); err != nil {
						return err
					}
				}
			}
		}
	}
}

// settleTurn records the end of a turn on a resident session: sharing results
// are acknowledged, pending approvals expire, streaming markers clear and the
// chat reports idle (or queued when a message arrived during the turn).
func (e *Engine) settleTurn(parent context.Context, id string, a *activeRun) {
	e.mu.Lock()
	sharingResults := a.sharingResults
	a.sharingResults = nil
	e.mu.Unlock()
	for _, requestID := range sharingResults {
		_, _ = e.sharingCall(parent, "ack", map[string]any{"id": requestID})
	}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || c.RunID != a.runID {
			return nil
		}
		for i := range c.Approvals {
			if c.Approvals[i].State == "pending" || c.Approvals[i].State == "resolving" {
				c.Approvals[i].State = "expired"
			}
		}
		c.Conversation.ActiveTurnID = nil
		for i := range c.Conversation.Entries {
			c.Conversation.Entries[i].IsStreaming = false
		}
		if c.Status == "stopping" || c.Status == "interrupted" {
			return nil
		}
		c.Status = "idle"
		for _, v := range c.Conversation.Entries {
			if v.Delivery == "queued" {
				c.Status = "queued"
				break
			}
		}
		return nil
	})
}

// awaitMessage keeps a resident session open until the chat's next message
// arrives, returning it with the chat marked running. It returns nil when the
// session should end: released for another chat, idle too long, the run
// cancelled, the chat stopped, or the agent stream closed by the worker.
func (e *Engine) awaitMessage(ctx context.Context, id string, current *Chat, a *activeRun, client *agent.Client, frames chan agent.Frame) *cv.Entry {
	idleCtx, release := context.WithTimeout(ctx, e.residentIdle())
	defer release()
	e.mu.Lock()
	a.release = release
	a.idle.Store(true)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		a.idle.Store(false)
		a.release = nil
		e.mu.Unlock()
	}()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-idleCtx.Done():
			return nil
		case <-client.Done():
			return nil
		case f := <-frames:
			if len(f.ID) > 0 {
				_ = e.request(ctx, current, client, f)
			} else {
				_ = e.notification(id, f)
			}
		case <-tick.C:
			message, err := e.resume(id)
			if err != nil {
				return nil
			}
			if message != nil {
				return message
			}
		}
	}
}

// resume moves a queued chat with a live resident session back to running and
// hands over its first undelivered message. It fails when the chat is being
// stopped, was interrupted or archived, which ends the session.
func (e *Engine) resume(id string) (*cv.Entry, error) {
	var message *cv.Entry
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || c.Archived || c.Status == "stopping" || c.Status == "interrupted" {
			return errors.New("session ended")
		}
		if c.Status != "queued" {
			return nil
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.Delivery == "queued" {
				v.Delivery = "failed"
				v.Detail = "Delivery unconfirmed. Check the agent response before retrying."
				copy := *v
				message = &copy
				break
			}
		}
		if message == nil {
			c.Status = "idle"
			return nil
		}
		c.Status = "running"
		c.Error = ""
		return nil
	})
	return message, err
}
func input(m cv.Entry) []any {
	return []any{map[string]any{"type": "text", "text": m.Text, "text_elements": []any{}}}
}
func (e *Engine) attempt(id, turn string) (*cv.Entry, error) {
	var message *cv.Entry
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c.Status != "running" {
			return errors.New("run stopped")
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.Delivery == "queued" {
				v.Delivery = "failed"
				v.Detail = "Delivery unconfirmed. Check the agent response before retrying."
				if turn != "" {
					v.TurnID = cv.Ptr(turn)
				}
				copy := *v
				message = &copy
				break
			}
		}
		return nil
	})
	return message, err
}
func (e *Engine) confirm(id, message, turn string) error {
	return e.Store.update(func(st *State) error {
		c := st.chat(id)
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.ID == message {
				v.Delivery = "sent"
				v.Detail = ""
				v.TurnID = cv.Ptr(turn)
			}
		}
		return nil
	})
}
func (e *Engine) notification(id string, f agent.Frame) error {
	return e.Store.update(func(st *State) error {
		c := &st.chat(id).Conversation
		p := f.Params
		turn := agent.String(p["turnId"])
		if turn == "" {
			turn = agent.String(agent.Map(p["turn"])["id"])
		}
		switch f.Method {
		case "thread/started":
			c.ThreadID = cv.Ptr(agent.String(agent.Map(p["thread"])["id"]))
		case "turn/started":
			c.ActiveTurnID = cv.Ptr(turn)
		case "item/started", "item/completed":
			c.Upsert(agent.Map(p["item"]), turn, f.Method == "item/completed")
		case "item/agentMessage/delta", "item/commandExecution/outputDelta":
			c.Delta(agent.String(p["itemId"]), turn, agent.String(p["delta"]), strings.Contains(f.Method, "commandExecution"))
		case "turn/completed":
			for _, v := range agent.Array(agent.Map(p["turn"])["items"]) {
				c.Upsert(agent.Map(v), turn, true)
			}
			c.Finish(turn)
		case "error":
			if p["willRetry"] != true {
				c.Entries = append(c.Entries, cv.NewEntry("system", agent.String(agent.Map(p["error"])["message"])))
			}
		}
		return nil
	})
}
func (e *Engine) request(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	if f.Method == "item/tool/call" {
		return e.tool(ctx, c, client, f)
	}
	switch f.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
		return e.Store.update(func(st *State) error {
			chat := st.chat(c.ID)
			chat.Approvals = append(chat.Approvals, Approval{ID: cv.ID(), RunID: c.RunID, RPCID: append(json.RawMessage(nil), f.ID...), Method: f.Method, Params: f.Params, State: "pending"})
			return nil
		})
	default:
		return client.Send(agent.Frame{ID: f.ID, Error: &agent.RPCError{Code: -32601, Message: "Unsupported Warden agent request"}})
	}
}
func (e *Engine) Resolve(chatID, approvalID string, allow bool, answers map[string][]string) error {
	return e.ResolveAs(chatID, approvalID, allow, answers, cv.Actor{PrincipalID: "owner"})
}

// ResolveAs answers an approval on behalf of actor, the person the edge
// identified (grants record who approved them).
func (e *Engine) ResolveAs(chatID, approvalID string, allow bool, answers map[string][]string, actor cv.Actor) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.active[chatID]
	if a == nil || a.id != chatID || a.client == nil {
		return errors.New("approval run is no longer active")
	}
	var approval Approval
	err := e.Store.update(func(st *State) error {
		c := st.chat(chatID)
		if c.Status != "running" {
			return errors.New("chat stopped")
		}
		for i := range c.Approvals {
			v := &c.Approvals[i]
			if v.ID == approvalID && v.RunID == a.runID && v.State == "pending" {
				approval = *v
				v.State = "resolving"
				return nil
			}
		}
		return errors.New("approval expired or already answered")
	})
	if err != nil {
		return err
	}
	var result any
	switch approval.Method {
	case "warden/ports/bind":
		state := e.Store.Snapshot()
		result = e.resolvePort(state.chat(chatID), approval, allow)
	case methodNetworkAllow, methodRepositoryAccess, methodGitHubWrite, methodHostImport, methodHostExport:
		state := e.Store.Snapshot()
		result = e.resolveGrant(state.chat(chatID), approval, allow, actor)
	case "item/permissions/requestApproval":
		permissions := map[string]any{}
		if allow {
			permissions = agent.Map(approval.Params["permissions"])
		}
		result = map[string]any{"permissions": permissions, "scope": "turn"}
	case "item/tool/requestUserInput":
		out := map[string]any{}
		for _, q := range agent.Array(approval.Params["questions"]) {
			qid := agent.String(agent.Map(q)["id"])
			out[qid] = map[string]any{"answers": answers[qid]}
		}
		result = map[string]any{"answers": out}
	default:
		decision := "decline"
		if allow {
			decision = "accept"
		}
		result = map[string]any{"decision": decision}
	}
	err = a.client.Reply(approval.RPCID, result)
	saveErr := e.Store.update(func(st *State) error {
		for i := range st.chat(chatID).Approvals {
			v := &st.chat(chatID).Approvals[i]
			if v.ID == approvalID {
				v.State = "answered"
				if err != nil {
					v.State = "delivery-unconfirmed"
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return saveErr
}

func (e *Engine) Done() <-chan struct{} { return e.done }

func (e *Engine) ConfigureAgent(id, provider, model string) error {
	if err := sandbox.ValidateAgent(provider, model); err != nil {
		return err
	}
	if provider == "" {
		provider = "codex"
	}
	return e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
			return errors.New("wait until the conversation is idle")
		}
		old := c.Provider
		if old == "" {
			old = "codex"
		}
		if provider != old && len(c.Conversation.Entries) > 0 {
			return errors.New("start a new conversation to change providers")
		}
		c.Provider = provider
		c.Model = model
		return nil
	})
}

// ConfigureAgentAndRelease applies ConfigureAgent and ends the chat's resident
// session, so the next message starts an agent with the new selection.
func (e *Engine) ConfigureAgentAndRelease(ctx context.Context, id, provider, model string) error {
	if err := e.ConfigureAgent(id, provider, model); err != nil {
		return err
	}
	e.releaseChat(ctx, id)
	return nil
}
