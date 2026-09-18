package chats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"warden/chat/internal/agent"
	"warden/chat/internal/bugreport"
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
	// The agent's thread and the turn in flight ("" between turns), set
	// under the engine lock, so Stop can name them to `turn/interrupt`;
	// interrupting is set once it has, so the turn's end as interrupted is
	// the requested outcome and its steering tick stands down.
	threadID, turnID string
	interrupting     atomic.Bool
	// ending is set by Stop when the agent is live but no turn is in
	// flight (starting up, or between turns): the run's cancellation is
	// then a clean end, not a failure to tombstone.
	ending atomic.Bool
	// The agent process reports token usage as a running total for its
	// lifetime (a resident session spans turns), so a turn's usage is how
	// much the total grew: `usage` is the last total seen and `usageBase`
	// the total when `usageTurn` began. Only the run's goroutine touches
	// them.
	usage, usageBase cv.Usage
	usageTurn        string
	// instructed is, by principal, the instructions text this session's
	// launch put in the agent's system prompt (instructions.go); a queued
	// message whose sender's current text differs relaunches the session.
	// Only the run's goroutine touches it.
	instructed map[string]string
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
	// AllowFastMode and AllowLongContext are the operator's leave for the
	// costlier Claude session features (config providers.claude.*): fast
	// mode as a chat setting, the 1M-context model variants as choices.
	AllowFastMode    bool
	AllowLongContext bool
	// DefaultModels is the operator's default model by provider
	// (providers.<p>.defaultModel); DefaultModel fills the built-ins in.
	DefaultModels map[string]string
	// Bugs drafts this service's bug reports (bugs.go); nil reports nothing.
	Bugs *bugreport.Capturer
	// Now is the clock (tests replace it); nil means time.Now.
	Now func() time.Time
	// NormalizeImage replaces the imageguard subprocess (images.go) in
	// tests; nil runs it.
	NormalizeImage func(context.Context, []byte) ([]byte, error)
	// limits is the runner's size offer, asked for on demand and kept for
	// limitsTTL: the form and validation need it before any chat exists.
	limitsMu sync.Mutex
	limits   *sandbox.ResourceLimits
	limitsAt time.Time
	// resizing: workspace id -> the resize in flight or its outcome
	// (resources.go).
	resizingMu sync.Mutex
	resizing   map[string]*Resizing
	// typing: chat id -> principal -> indicator, see Typing.
	typingMu sync.Mutex
	typing   map[string]map[string]Typist
	// startup: chat id -> where its start is, see startup.go.
	startupMu sync.Mutex
	startup   map[string]Startup
	// startupTrace is each starting chat's stages so far with their
	// durations, for the log line at the end of a slow start.
	startupTrace map[string][]string
	mu           sync.Mutex
	active       map[string]*activeRun
	// asides: chat id -> a side question being answered (aside.go);
	// titling: chat id -> a title being made (title.go).
	asides  map[string]bool
	titling map[string]bool
	// background counts the goroutines a run leaves behind it (a naming
	// beside the idle session); Serve returns once they are done.
	background sync.WaitGroup
	wake       chan struct{}
	done       chan struct{}
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
	defer e.background.Wait() // after the runs, which spawn them
	defer e.Bugs.Recover("engine serve loop")
	e.fillDefaultModels()
	if e.PolicyAddress != "" {
		deliveryCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); defer e.Bugs.Recover("sharing delivery"); e.sharingDelivery(deliveryCtx) }()
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
			go func() { defer workers.Done(); defer e.Bugs.Recover("chat run " + id); e.run(ctx, id); finished <- id }()
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

// limitsTTL bounds how long a runner's size offer is reused; the offer
// changes only with the runner's configuration, so a short cache just
// spares the form a worker round-trip per poll.
const limitsTTL = 30 * time.Second

// Limits is the runner's size offer (default, ceiling, CPU step, whether
// a resize restarts), or nil when the runner cannot be reached.
func (e *Engine) Limits(ctx context.Context) *sandbox.ResourceLimits {
	e.limitsMu.Lock()
	defer e.limitsMu.Unlock()
	if e.Worker == nil || (e.limits != nil && e.now().Before(e.limitsAt.Add(limitsTTL))) {
		return e.limits
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := e.Worker.Call(ctx, sandbox.Request{Operation: "health"})
	if err != nil || res.Limits == nil {
		return e.limits
	}
	e.limits, e.limitsAt = res.Limits, e.now()
	return e.limits
}

// Create starts a chat: on a fresh workspace of the given size (nil is the
// runner's default), or sharing an existing one, whose size is settled.
func (e *Engine) Create(title, shared, repository string, resources *sandbox.Resources, selection ...string) (string, error) {
	return e.CreateFrom(cv.Actor{PrincipalID: "owner"}, title, shared, repository, resources, selection...)
}

// CreateFrom creates a chat by actor (the requester the edge identified,
// or the owner), recorded as its creator.
func (e *Engine) CreateFrom(actor cv.Actor, title, shared, repository string, resources *sandbox.Resources, selection ...string) (string, error) {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	provider, model := "codex", ""
	if len(selection) > 0 {
		provider = selection[0]
	}
	if len(selection) > 1 {
		model = selection[1]
	}
	if err := e.validateAgent(provider, model); err != nil {
		return "", err
	}
	if provider == "" {
		provider = "codex"
	}
	if model == "" {
		model = e.DefaultModel(provider)
	}
	if resources != nil && resources.IsZero() {
		resources = nil
	}
	if resources != nil {
		if shared != "" {
			return "", errors.New("a shared workspace already has its size")
		}
		limits := e.Limits(context.Background())
		if limits == nil {
			return "", errors.New("the runner's size limits are unavailable; try again or leave the size at its default")
		}
		resolved, err := limits.Resolve(*resources)
		if err != nil {
			return "", fmt.Errorf("workspace size: %w", err)
		}
		resources = &resolved
	}
	id := cv.ID()
	err := e.Store.update(func(st *State) error {
		title = strings.TrimSpace(title)
		// A chat named by its creator keeps that name; one left at the
		// default is named from its first exchange (title.go).
		titled := "manual"
		if title == "" {
			title, titled = DefaultTitle, ""
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
					resources = c.Resources
					found = true
					break
				}
			}
			if !found {
				return errors.New("unknown workspace")
			}
		}
		creator := actor
		st.Chats = append(st.Chats, &Chat{ID: id, Provider: provider, Model: model, Title: title, Titled: titled, SandboxID: sbxID, Repository: repository, Resources: resources, Creator: &creator, Status: "idle", Conversation: cv.Conversation{Entries: []cv.Entry{}}, Approvals: []Approval{}})
		return nil
	})
	return id, err
}
func (e *Engine) Message(id, text, messageID string) error {
	return e.MessageFrom(id, text, messageID, cv.Actor{PrincipalID: "owner"})
}

// MessageFrom appends a user message attributed to actor (the person the
// edge identified, or the owner) and clears that person's typing indicator.
// attachments names uploads (storeAttachment) the message sends along; a
// message may be attachments alone.
func (e *Engine) MessageFrom(id, text, messageID string, actor cv.Actor, attachments ...string) error {
	if actor.PrincipalID == "" {
		actor.PrincipalID = "owner"
	}
	defer e.stopTyping(id, actor.PrincipalID)
	text = strings.TrimSpace(text)
	if text == "" && len(attachments) == 0 || len(text) > 128<<10 || len(messageID) != 32 {
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
		files, err := e.claimAttachments(c, attachments)
		if err != nil {
			return err
		}
		v := cv.NewEntry("user", text)
		v.ID = messageID
		sender := actor
		v.Sender = &sender
		v.Delivery = "queued"
		v.Attachments = files
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

// at is the time as entries record it: unix seconds.
func (e *Engine) at() float64 { return float64(e.now().UnixMilli()) / 1000 }

// View is the state clients see: the stored snapshot plus who is typing
// in each chat, expired indicators dropped.
// View is the state as clients see it: the chats with their live typing
// indicators, plus what the form needs and the store does not hold.
type View struct {
	State
	// Sandboxes is the runner's size offer; nil while the runner is
	// unreachable, when clients offer no size choice.
	Sandboxes *sandbox.ResourceLimits `json:"sandboxes,omitempty"`
	// AgentOptions are the Claude session features this Warden allows,
	// so clients offer them only then.
	AgentOptions AgentOptions `json:"agentOptions"`
}

// AgentOptions are the optional, costlier Claude features an operator
// enables (config providers.claude.allowFastMode, allowLongContext), and
// each provider's model catalog as its CLI reported it (catalog.go), by
// provider; a provider without one is absent and clients fall back to
// their own rows.
type AgentOptions struct {
	FastMode    bool                   `json:"fastMode"`
	LongContext bool                   `json:"longContext"`
	Models      map[string][]ModelInfo `json:"models,omitempty"`
	// Defaults is the model a chat of each provider starts with
	// (defaults.go), the row the pickers show first.
	Defaults map[string]string `json:"defaults"`
}

func (e *Engine) View() View {
	st := e.state()
	models := catalogRows(st.Catalog)
	st.Catalog = nil // clients get it as agentOptions.models
	return View{State: st, Sandboxes: e.Limits(context.Background()), AgentOptions: AgentOptions{FastMode: e.AllowFastMode, LongContext: e.AllowLongContext, Models: models, Defaults: e.defaultModels()}}
}

// state is the store with typing indicators and each chat's spend filled
// in, and the people's instructions left out (each person reads their
// own through me/instructions); a kept rewind tail becomes the marker it
// can undo (rewind.go).
func (e *Engine) state() State {
	st := e.Store.Snapshot()
	st.Instructions = nil
	now := float64(e.now().UnixNano()) / 1e9
	for _, c := range st.Chats {
		c.Permissions = nil // chats/{id}/permissions serves the history
		if c.Status == "queued" || c.Status == "running" {
			c.Startup = e.startupOf(c.ID)
		}
		if c.RewoundTail != nil {
			c.UndoRewind = c.RewoundTail.MarkerID
			c.RewoundTail = nil
		}
		spend := spendOf(c)
		c.Spend = &spend
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
		if title != c.Title {
			// A person's name for the chat wins over, and ends, the
			// automatic naming (title.go).
			c.Titled = "manual"
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

// Stop ends what the agent is doing in the chat, the way Escape does in
// its own CLI: a turn in flight is interrupted at the protocol level
// (`turn/interrupt`; the model stops mid-thought, a running tool is
// aborted) and the chat is handed back as `interrupted`, with the agent's
// session resident and the sandbox up, so the next message is answered at
// once. An idle session is released, and a chat still queued is taken off
// the queue. Messages queued behind the turn are held, not failed: they
// stay in the transcript until SendQueued or the next message lets them
// go (queue.go). Only a run with no live agent yet (still booting), or
// an agent that ignores the interrupt, falls back to cancelling the run,
// which stops the sandbox (the runner cannot otherwise prove the guest's
// processes died). Stopping the sandbox itself is StopEnvironment.
func (e *Engine) Stop(ctx context.Context, id string) error {
	snapshot := e.Store.Snapshot()
	target := snapshot.chat(id)
	if target != nil {
		for _, other := range snapshot.Chats {
			if other.ID != id && other.SandboxID == target.SandboxID && (other.Status == "running" || other.Status == "stopping") {
				return errors.New("workspace is running another chat; stop that chat first")
			}
		}
	}
	// Which way this stop goes depends on the run (a turn in flight, an
	// agent without one, a session idle between turns, no run at all) and
	// must agree with the chat's status; the two are read apart, so a run
	// ending or resuming in between is retried. The run's flags are set
	// before the chat is marked, so its steering tick does not read the
	// mark as a failed run.
	var a *activeRun
	var client *agent.Client
	var threadID, turnID string
	var live bool
	var cpy Chat
	for attempt := 0; ; attempt++ {
		e.mu.Lock()
		a = e.active[id]
		idle := a != nil && a.idle.Load()
		live = a != nil && a.client != nil && !idle
		client, threadID, turnID = nil, "", ""
		if live && a.turnID != "" {
			client, threadID, turnID = a.client, a.threadID, a.turnID
			a.interrupting.Store(true)
		} else if live {
			a.ending.Store(true) // the agent is up but has no turn: end the session
		}
		e.mu.Unlock()
		mode, again := "", false
		err := e.Store.update(func(st *State) error {
			c := st.chat(id)
			if c == nil {
				return errors.New("chat not found")
			}
			// Messages still queued are held, not failed: they stay in the
			// transcript to be edited, withdrawn or sent (SendQueued, or
			// the next message) once the person is ready (queue.go).
			switch {
			case c.Status == "stopping":
				return errors.New("stop already pending")
			case c.Status != "running" && c.Status != "queued":
				// Nothing runs: the turn ended by itself, or the chat is idle
				// with its session resident, which is worth keeping.
			case a == nil || (c.Status == "queued" && idle):
				// Waiting for a slot or the sandbox (or the run just ended, its
				// own cleanup keeps the mark): off the queue, nothing to cancel.
				c.Status = "interrupted"
				c.Conversation.ActiveTurnID = nil
				mode = "unqueued"
			case idle || c.Status == "queued":
				again = true // a session resuming, or settling with a message waiting
			default:
				c.Status = "stopping"
				cpy = *c
				mode = "stop"
			}
			return nil
		})
		if mode != "stop" && a != nil {
			a.interrupting.Store(false)
			a.ending.Store(false)
		}
		if err != nil {
			return err
		}
		if again && attempt < 50 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		if mode != "stop" {
			if mode == "unqueued" {
				e.Wake()
			}
			return nil
		}
		break
	}
	if client != nil {
		callCtx, done := context.WithTimeout(ctx, interruptGrace)
		_, callErr := client.Call(callCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
		done()
		if callErr == nil && e.awaitInterrupted(ctx, id) {
			e.Wake() // the sandbox is free: a chat queued on it re-evaluates now
			return nil
		}
	} else if live {
		// The session ends the way an idle release ends it: the stream
		// closes, the runner sees a run complete, the sandbox stays.
		a.cancel()
		select {
		case <-a.done:
			_ = e.Store.update(func(st *State) error {
				c := st.chat(id)
				if c.Status == "stopping" {
					c.Status = "interrupted"
					c.Conversation.ActiveTurnID = nil
				}
				return nil
			})
			e.Wake()
			return nil
		case <-ctx.Done():
		case <-time.After(interruptGrace):
		}
	}
	// Tombstone the run before disconnecting its stream, so worker cleanup stops
	// the environment instead of treating the disconnect as normal completion.
	var err error
	if cpy.RunID != "" {
		_, err = e.Worker.Call(ctx, request(&cpy, "cancel"))
	}
	e.mu.Lock()
	if a := e.active[id]; a != nil {
		a.ending.Store(false) // the cancel is the real thing now
		a.cancel()
	}
	e.mu.Unlock()
	e.releaseSandbox(ctx, cpy.SandboxID, id)
	if err == nil {
		err = e.stopSandbox(ctx, &cpy)
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
	e.Wake() // the sandbox is free: a chat queued on it re-evaluates now
	return err
}

// interruptGrace is how long an agent has to end its turn after
// `turn/interrupt` before the run is cancelled instead: the model's current
// request is aborted at once, but a tool call in flight may take a moment.
var interruptGrace = 10 * time.Second

// stopSandbox asks the runner to stop the chat's sandbox, waiting out a run
// the runner is still clearing (its stream closed moments ago).
func (e *Engine) stopSandbox(ctx context.Context, c *Chat) error {
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		_, err := e.Worker.Call(stopCtx, request(c, "stop"))
		if err == nil || !strings.Contains(err.Error(), "sandbox has an active run") {
			return err
		}
		select {
		case <-stopCtx.Done():
			return stopCtx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// endSession ends the chat's agent session once its turn is over: a
// resident session is released as soon as it is idle, a run that is
// finishing is waited for. The owner asked for the sandbox to stop or
// change, so the runner must see its run gone.
func (e *Engine) endSession(ctx context.Context, id string) {
	deadline := time.Now().Add(interruptGrace)
	for {
		e.mu.Lock()
		a := e.active[id]
		if a == nil {
			e.mu.Unlock()
			return
		}
		done := a.done
		if a.idle.Load() && a.release != nil {
			a.release()
		}
		e.mu.Unlock()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// awaitInterrupted waits for the run to record the requested interruption
// (the chat leaves `stopping`; turn marks it `interrupted`), false when the
// agent has not ended the turn within the grace.
func (e *Engine) awaitInterrupted(ctx context.Context, id string) bool {
	deadline := time.Now().Add(interruptGrace)
	for {
		if c := e.Store.Snapshot().chat(id); c == nil || c.Status != "stopping" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
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
		if err != nil && a.ending.Load() {
			err = nil // Stop ended a run that had no turn in flight
		}
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
			c.Conversation.EndTurns(e.at())
			// A run Stop ended (the agent ignored the interrupt, or had no
			// turn yet) holds its queued messages like any stop; a run
			// that failed on its own fails them, retry being the fix.
			held := c.Status == "stopping" || c.Status == "interrupted"
			for i := range c.Conversation.Entries {
				v := &c.Conversation.Entries[i]
				v.IsStreaming = false
				if err != nil && !held && v.Delivery == "queued" {
					v.Delivery = "failed"
					v.Detail = "Not delivered"
				}
				if v.Delivery == "sending" {
					// Handed over but never confirmed by the agent: the run
					// ended in between, so whether it was read is unknown.
					v.Delivery = "failed"
					v.Detail = deliveryUnconfirmed
				}
			}
			if held {
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
	r.Resources = current.Resources
	if _, err = e.Worker.Call(ctx, r); err != nil {
		return
	}
	r = request(&current, "prepare")
	if current.Conversation.ThreadID != nil {
		r.ThreadID = *current.Conversation.ThreadID
	}
	r.NewSession = current.NewSession
	r.ForkSession = current.ForkSession
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
	// The participants' standing instructions go with the launch (Claude:
	// the appended system prompt; Codex: developerInstructions below).
	var instructions string
	if snapshot := e.Store.Snapshot(); snapshot.chat(id) != nil {
		instructions, a.instructed = sessionInstructions(&snapshot, snapshot.chat(id))
	}
	r = request(&current, "stream")
	r.Directory = prep.Directory
	r.Instructions = instructions
	r.OutputStyle = current.OutputStyle
	if current.ForkSession && current.Conversation.ThreadID != nil {
		// A forked chat's first run resumes the source chat's session as
		// a copy (fork.go); the runner records nothing until the agent
		// reports the copy's own id.
		r.ThreadID, r.ForkSession = *current.Conversation.ThreadID, true
	}
	var stream io.ReadWriteCloser
	stream, _, err = e.Worker.Open(ctx, r)
	if err != nil {
		return
	}
	if current.Provider == "claude" {
		stream = agent.ClaudeStream(ctx, stream)
	}
	// The process is launched; its first answer is the app server up (on
	// a small sandbox, the slow part of a cold start).
	e.setStartup(id, stageInitializing, "waiting for the agent to answer")
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
	params := map[string]any{"cwd": prep.Directory, "approvalPolicy": "on-request", "sandbox": "danger-full-access", "developerInstructions": "You are an agent in a Warden-managed sandbox. The files, shared documents and shared repositories belong to this workspace and are visible to every chat in it; preserve other chats' files. Warden controls external access. Do not request or expose host credentials. GitHub repositories are reached through Warden's repository sharing: list_shared_repositories shows what this workspace can clone and read; to clone or read one that is not listed, ask with request_repository_access (contents), never with request_network_access for github.com: a refused git clone means the repository is not shared, not that the network is blocked. To show a web preview, start the server as a detached process on 0.0.0.0 inside this sandbox (for example subprocess.Popen with start_new_session=True and stdio redirected to files), then call preview_attach with port, path beginning /, and title. The controller chooses the URL.", "ephemeral": false, "historyMode": "legacy"}
	params["modelProvider"] = "warden"
	if instructions != "" {
		params["developerInstructions"] = params["developerInstructions"].(string) + "\n\n" + instructions
	}
	params["model"] = e.modelOf(&current)
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
	e.mu.Lock()
	a.threadID = threadID
	e.mu.Unlock()
	err = e.Store.update(func(st *State) error { st.chat(id).Conversation.Hydrate(thread); return nil })
	if err != nil {
		return
	}
	// The provider's model catalog, from the process just started
	// (catalog.go); a refusal or a slow answer leaves the cached one.
	e.loadCatalog(ctx, current.Provider, client)
	if current.Rewind != nil && !e.applyPendingRewind(ctx, id, client, threadID) {
		// The resumed session could not rewind: this run ends cleanly and
		// the message stays queued for a fresh session (rewind.go).
		return
	}
	var message *cv.Entry
	message, err = e.attempt(id, "")
	if err != nil {
		return
	}
	if message == nil && !a.resident {
		err = errors.New("no pending message")
		return
	}
	var items []any
	var turn map[string]any
	turnID := ""
	if message != nil {
		e.setStartup(id, stageSending, "handing your message to the agent")
		if items, err = e.input(ctx, &current, prep.Directory, *message); err != nil {
			return
		}
		items = e.beforeTurn(ctx, id, &current, message.ID, items)
		e.applySession(ctx, id, &current, client)
		response, err = client.Call(ctx, "turn/start", map[string]any{"threadId": threadID, "clientUserMessageId": message.ID, "cwd": prep.Directory, "approvalPolicy": "on-request", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}, "input": items})
		if err != nil {
			return
		}
		turn = agent.Map(response["turn"])
		turnID = agent.String(turn["id"])
		if turnID == "" {
			err = errors.New("agent returned no turn ID")
			return
		}
		e.mu.Lock()
		a.turnID = turnID
		e.mu.Unlock()
		if err = e.confirm(id, message.ID, turnID); err != nil {
			return
		}
		// The turn is accepted; the model has not said anything yet. The
		// first item of the turn ends the start (turn below).
		e.setStartup(id, stageFirstResponse, "waiting for the model's first reply")
	} else {
		// Nothing to send: the run was started for a side question
		// (aside.go), or its message was withdrawn meanwhile. The session
		// is up; it waits for the chat's next message like one that just
		// finished a turn.
		e.clearStartup(id)
	}
	for {
		if turnID != "" {
			if err = e.turn(ctx, id, &current, a, client, frames, threadID, turnID, prep.Directory, turn); err != nil {
				return
			}
			if !a.resident {
				// The run ends with the turn; a chat still at the default
				// title is named from the transcript alone (title.go).
				e.autoTitle(parent, id, nil)
				return
			}
		}
		// The turn finished but the session stays open: settle the transcript,
		// report idle, and wait for the chat's next message.
		e.settleTurn(parent, id, a)
		// A chat still at the default title is named from its first
		// exchange, beside the idle session (title.go).
		e.background.Add(1)
		go func() { defer e.background.Done(); e.autoTitle(parent, id, a) }()
		var agentTurn string
		message, agentTurn = e.awaitMessage(ctx, id, &current, a, client, frames)
		if agentTurn != "" {
			// The agent began a turn of its own (Claude Code resumes the
			// model when a background task it started reports back): drive
			// it like one asked for, with nothing to send.
			turnID = agentTurn
			turn = map[string]any{"id": turnID, "status": "inProgress"}
			e.mu.Lock()
			a.turnID = turnID
			e.mu.Unlock()
			continue
		}
		if message == nil {
			return // released, timed out, stopped or ended by the worker: a clean end
		}
		e.setStartup(id, stageSending, "handing your message to the agent")
		if items, err = e.input(ctx, &current, prep.Directory, *message); err != nil {
			return
		}
		items = e.beforeTurn(ctx, id, &current, message.ID, items)
		e.applySession(ctx, id, &current, client)
		response, err = client.Call(ctx, "turn/start", map[string]any{"threadId": threadID, "clientUserMessageId": message.ID, "cwd": prep.Directory, "approvalPolicy": "on-request", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}, "input": items})
		if err != nil {
			return
		}
		turn = agent.Map(response["turn"])
		turnID = agent.String(turn["id"])
		if turnID == "" {
			err = errors.New("agent returned no turn ID")
			return
		}
		e.mu.Lock()
		a.turnID = turnID
		e.mu.Unlock()
		if err = e.confirm(id, message.ID, turnID); err != nil {
			return
		}
		e.setStartup(id, stageFirstResponse, "waiting for the model's first reply")
	}
}

// turn drives one agent turn to completion. It returns nil once the turn
// completed, or ended after Stop asked for its interruption (the chat is
// then marked interrupted), and an error when it failed, was interrupted
// by the agent itself, the run was cancelled or the agent stream ended.
func (e *Engine) turn(ctx context.Context, id string, current *Chat, a *activeRun, client *agent.Client, frames chan agent.Frame, threadID, turnID, cwd string, turn map[string]any) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if status := agent.String(turn["status"]); status != "" && status != "inProgress" && a.interrupting.Swap(false) {
			// However the agent reports the end of a turn it was asked to
			// interrupt (interrupted; completed when the turn was ending
			// anyway), the stop is done.
			_ = e.Store.update(func(st *State) error {
				c := st.chat(id)
				if c.Status == "stopping" {
					c.Status = "interrupted"
					c.Conversation.ActiveTurnID = nil
				}
				return nil
			})
			return nil
		}
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
				err = e.notification(ctx, id, f)
			}
			if err != nil {
				return err
			}
			if (strings.HasPrefix(f.Method, "item/") && agent.String(agent.Map(f.Params["item"])["type"]) != "userMessage") || f.Method == "turn/completed" {
				e.clearStartup(id) // the model has answered (its own echo of the message is not that): the start is over
			}
			if f.Method == "turn/completed" && agent.String(agent.Map(f.Params["turn"])["id"]) == turnID {
				turn = agent.Map(f.Params["turn"])
				e.mu.Lock()
				a.turnID = "" // nothing left to interrupt
				e.mu.Unlock()
			}
		case <-tick.C:
			if !e.steers(current.Provider) || a.interrupting.Load() {
				continue // Claude queues a separate turn; it does not implement Codex steering.
			}
			if e.instructionsOwed(id, a) {
				continue // left queued: the session is relaunched for the sender's instructions after this turn
			}
			message, err := e.attempt(id, turnID)
			if err != nil {
				if a.interrupting.Load() {
					continue // the chat was marked stopping under the tick
				}
				return err
			}
			if message != nil {
				items, err := e.input(ctx, current, cwd, *message)
				if err != nil {
					return err
				}
				result, callErr := client.Call(ctx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "clientUserMessageId": message.ID, "input": items})
				if callErr == nil && agent.String(result["turnId"]) == turnID {
					if err = e.confirm(id, message.ID, turnID); err != nil {
						return err
					}
				} else {
					e.unconfirmed(id, message.ID)
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
		c.Conversation.EndTurns(e.at())
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
// arrives, returning it with the chat marked running, or until the agent
// starts a turn by itself (`turn/started` with no turn asked for), returning
// that turn's id with the chat marked running and the turn begun. It
// returns neither when the session should end: released for another chat,
// idle too long, the run cancelled, the chat stopped, or the agent stream
// closed by the worker.
func (e *Engine) awaitMessage(ctx context.Context, id string, current *Chat, a *activeRun, client *agent.Client, frames chan agent.Frame) (*cv.Entry, string) {
	idleCtx, release := context.WithTimeout(ctx, e.residentIdle())
	defer release()
	e.mu.Lock()
	a.release = release
	a.idle.Store(true)
	e.mu.Unlock()
	// A chat queued on this sandbox, or waiting for a run slot, was blocked
	// while the turn ran; the loop only re-evaluates when woken.
	e.Wake()
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
			return nil, ""
		case <-client.Done():
			return nil, ""
		case f := <-frames:
			if len(f.ID) > 0 {
				_ = e.request(ctx, current, client, f)
				continue
			}
			_ = e.notification(ctx, id, f)
			if f.Method == "turn/started" {
				if turn := agent.String(agent.Map(f.Params["turn"])["id"]); turn != "" {
					if err := e.beginAgentTurn(id, turn); err != nil {
						return nil, ""
					}
					return nil, turn
				}
			}
		case <-tick.C:
			if e.instructionsOwed(id, a) {
				// The message stays queued; the run ends cleanly and the
				// chat's next run launches with the sender's instructions.
				log.Printf("chat %s: relaunching the agent session for a sender's standing instructions", id)
				return nil, ""
			}
			message, err := e.resume(id)
			if err != nil {
				return nil, ""
			}
			if message != nil {
				return message, ""
			}
		}
	}
}

// beginAgentTurn marks the chat running for a turn the agent started by
// itself and records the turn's start. It fails when the chat is being
// stopped or archived, which ends the session.
func (e *Engine) beginAgentTurn(id, turn string) error {
	return e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || c.Archived || c.Status == "stopping" {
			return errors.New("session ended")
		}
		c.Status = "running"
		c.Error = ""
		c.Conversation.Begin(turn, e.at())
		dropRewoundTail(c) // a turn started: the last rewind is final (rewind.go)
		return nil
	})
}

// resume moves a queued chat with a live resident session back to running and
// hands over its first undelivered message, which is "sending" until the
// agent's turn confirms it (confirm) or the run ends without (unconfirmed). It fails when the chat is being
// stopped or archived, which ends the session. A chat whose turn Stop
// interrupted keeps its session: the next message resumes it.
func (e *Engine) resume(id string) (*cv.Entry, error) {
	var message *cv.Entry
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil || c.Archived || c.Status == "stopping" {
			return errors.New("session ended")
		}
		if c.Status != "queued" {
			return nil
		}
		for i := range c.Conversation.Entries {
			v := &c.Conversation.Entries[i]
			if v.Delivery == "queued" {
				v.Delivery = "sending"
				v.Detail = ""
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
		dropRewoundTail(c) // a turn starts: the last rewind is final (rewind.go)
		return nil
	})
	return message, err
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
				v.Delivery = "sending"
				v.Detail = ""
				if turn != "" {
					v.TurnID = cv.Ptr(turn)
				}
				copy := *v
				message = &copy
				break
			}
		}
		if message != nil {
			dropRewoundTail(c) // a turn starts: the last rewind is final (rewind.go)
		}
		return nil
	})
	return message, err
}

// deliveryUnconfirmed is the detail of a message the agent may or may not
// have read: it was handed over (Delivery "sending") but the turn that
// would confirm it never started.
const deliveryUnconfirmed = "Delivery unconfirmed. Check the agent response before retrying."

// unconfirmed fails message `message` of chat `id` as unconfirmed if it is
// still in flight: for a hand-over the agent did not acknowledge while the
// run goes on (a steer it refused), where the run's end will not do it.
func (e *Engine) unconfirmed(id, message string) {
	_ = e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return nil
		}
		for i := range c.Conversation.Entries {
			if v := &c.Conversation.Entries[i]; v.ID == message && v.Delivery == "sending" {
				v.Delivery = "failed"
				v.Detail = deliveryUnconfirmed
			}
		}
		return nil
	})
}
func (e *Engine) confirm(id, message, turn string) error {
	return e.Store.update(func(st *State) error {
		c := st.chat(id)
		c.Conversation.Begin(turn, e.at())
		c.Recap, c.NewSession = "", false // delivered with this turn (rewind.go)
		entries := c.Conversation.Entries
		for i := range entries {
			v := &entries[i]
			if v.ID != message {
				continue
			}
			v.Delivery = "sent"
			v.Detail = ""
			v.TurnID = cv.Ptr(turn)
			if i < len(entries)-1 {
				// The message opens its turn now: it moves past what the
				// earlier turn appended while it waited in the queue, so
				// the transcript reads in the order things happened.
				sent := *v
				entries = append(entries[:i:i], entries[i+1:]...)
				c.Conversation.Entries = append(entries, sent)
			}
			break
		}
		return nil
	})
}

// turnUsage reads a `thread/tokenUsage/updated` notification into the usage
// of the turn it names: the growth of the process's running total since
// that turn began (see activeRun). Nil when the notification names no turn
// or the run is gone.
func (e *Engine) turnUsage(id string, p map[string]any) (string, *cv.Usage) {
	turn := agent.String(p["turnId"])
	e.mu.Lock()
	a := e.active[id]
	e.mu.Unlock()
	if turn == "" || a == nil {
		return "", nil
	}
	total := cv.UsageFrom(agent.Map(agent.Map(p["tokenUsage"])["total"]))
	if a.usageTurn != turn {
		a.usageTurn, a.usageBase = turn, a.usage
	}
	a.usage = total
	usage := total.Sub(a.usageBase)
	return turn, &usage
}
func (e *Engine) notification(ctx context.Context, id string, f agent.Frame) error {
	// Looked up before the store is locked: the engine lock is taken around
	// store updates elsewhere, never inside one.
	var usage *cv.Usage
	var usageTurn string
	if f.Method == "thread/tokenUsage/updated" {
		usageTurn, usage = e.turnUsage(id, f.Params)
	}
	if f.Method == "item/completed" {
		// An image a Read returned is stored before the entry records it
		// (images.go keepReadImage): the store keeps an id, never bytes.
		e.keepReadImage(ctx, id, agent.Map(f.Params["item"]))
	}
	return e.Store.update(func(st *State) error {
		chat := st.chat(id)
		if f.Method == "permissions/modeChanged" {
			chat.applyMode(agent.String(f.Params["mode"]))
			return nil
		}
		c := &chat.Conversation
		p := f.Params
		turn := agent.String(p["turnId"])
		if turn == "" {
			turn = agent.String(agent.Map(p["turn"])["id"])
		}
		switch f.Method {
		case "thread/started":
			thread := agent.Map(p["thread"])
			c.ThreadID = cv.Ptr(agent.String(thread["id"]))
			chat.ForkSession = false // the copy has its own session now (fork.go)
			chat.sessionStarted(thread)
		case "turn/started":
			c.ActiveTurnID = cv.Ptr(turn)
		case "item/started", "item/completed":
			c.Upsert(agent.Map(p["item"]), turn, f.Method == "item/completed")
		case "item/agentMessage/delta":
			c.Delta(agent.String(p["itemId"]), turn, agent.String(p["delta"]), "assistant")
		case "item/commandExecution/outputDelta":
			c.Delta(agent.String(p["itemId"]), turn, agent.String(p["delta"]), "activity")
		case "item/reasoning/summaryTextDelta":
			c.Delta(agent.String(p["itemId"]), turn, agent.String(p["delta"]), "thinking")
		case "item/reasoning/summaryPartAdded":
			c.Break(agent.String(p["itemId"]))
		case "turn/completed":
			for _, v := range agent.Array(agent.Map(p["turn"])["items"]) {
				c.Upsert(agent.Map(v), turn, true)
			}
			c.Finish(turn, e.at())
		case "thread/tokenUsage/updated":
			if usage != nil {
				c.Report(usageTurn, *usage)
			}
		case "thread/context/updated":
			// How full the agent's context is (the Claude adapter reports
			// it per model call and after a compaction).
			if context := cv.ContextFrom(agent.Map(p["context"])); context != nil {
				c.Context = context
			}
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
	case methodPermission:
		// A Claude tool ask: the chat's mode and rules answer it, or the
		// owner does from a card that shows the call as its transcript
		// card and, for a plan, the plan (permissions.go).
		tool, input := agent.String(f.Params["tool"]), agent.Map(f.Params["input"])
		var verdict *Verdict
		err := e.Store.update(func(st *State) error {
			chat := st.chat(c.ID)
			entry := permissionEntry(f.Params)
			if verdict = st.decide(chat, tool, input); verdict != nil {
				// Answered by a rule or the mode: history only (rules.go).
				ev := PermissionEvent{At: e.at(), Tool: tool, Summary: askSummary(tool, input, entry), Decision: "allow", How: "auto"}
				if verdict.Decision == "decline" {
					ev.Decision, ev.Message = "deny", verdict.Message
				}
				if verdict.Rule != nil {
					rule := verdict.Rule.Rule
					ev.How, ev.Rule, ev.Scope = "rule", &rule, verdict.Rule.Scope
				}
				chat.record(ev)
				return nil
			}
			params := map[string]any{"tool": tool, "input": input}
			if tool != "ExitPlanMode" {
				// What "Allow always" would remember; a plan never is.
				pattern := RuleFor(tool, input)
				params["rule"], params["always"] = pattern, RuleLabel(pattern)
			}
			if entry != nil {
				params["entry"] = entry
			}
			for _, k := range []string{"description", "plan"} {
				if v := agent.String(f.Params[k]); v != "" {
					params[k] = v
				}
			}
			chat.Approvals = append(chat.Approvals, Approval{ID: cv.ID(), RunID: c.RunID, RPCID: append(json.RawMessage(nil), f.ID...), Method: f.Method, Params: params, State: "pending"})
			return nil
		})
		if err != nil || verdict == nil {
			return err
		}
		reply := map[string]any{"decision": verdict.Decision}
		if verdict.Message != "" {
			reply["message"] = verdict.Message
		}
		return client.Reply(f.ID, reply)
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
	return e.Answer(chatID, approvalID, Answer{Allow: allow, Answers: answers}, actor)
}

// Answer is what the person says to an approval. Allow and Answers (a
// question's) serve every kind; a tool permission ask also takes Always
// (allow, and remember the call's rule for the chat, or for every chat of
// the workspace when Scope is "workspace"), Message (why it is denied,
// read by the model) and, for a plan, Mode (the permission mode the chat
// moves to on approval: auto or ask).
type Answer struct {
	Allow   bool
	Answers map[string][]string
	Always  bool
	Scope   string
	Message string
	Mode    string
}

// Answer resolves an approval with the given answer on behalf of actor.
func (e *Engine) Answer(chatID, approvalID string, answer Answer, actor cv.Actor) error {
	allow, answers := answer.Allow || answer.Always, answer.Answers
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
	case methodNetworkAllow, methodRepositoryAccess, methodGitHubWrite, methodHostImport, methodHostExport, methodResources:
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
	case methodPermission:
		decision := map[string]any{"decision": "decline"}
		if allow {
			decision["decision"] = "accept"
		} else if answer.Message != "" {
			decision["message"] = answer.Message
		}
		tool, input := agent.String(approval.Params["tool"]), agent.Map(approval.Params["input"])
		plan := tool == "ExitPlanMode"
		if plan && allow {
			// An approved plan moves the chat out of plan mode, into auto
			// unless the answer asks to keep asking; the CLI's mode moves
			// with the answer (adapter: updatedPermissions setMode).
			mode := ModeAuto
			if answer.Mode == ModeAsk {
				mode = ModeAsk
			}
			decision["mode"] = mode
		}
		err = e.Store.update(func(st *State) error {
			c := st.chat(chatID)
			entry := permissionEntry(approval.Params)
			ev := PermissionEvent{At: e.at(), Tool: tool, Summary: askSummary(tool, input, entry), Decision: "deny", How: "card", By: &actor, Message: answer.Message}
			if allow {
				ev.Decision, ev.Message = "allow", ""
			}
			switch {
			case plan && allow:
				mode := agent.String(decision["mode"])
				c.Mode = mode
				c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", "Plan approved — "+strings.TrimPrefix(modeNotice(mode), "Permission mode: ")))
			case answer.Always && !plan:
				// The call's rule, remembered for this chat or, at the
				// workspace scope, for every chat of the sandbox (rules.go).
				rule := Rule{Kind: RuleAllow, Pattern: RuleFor(tool, input)}.stamp("always", actor, e.at())
				ev.Rule, ev.Scope = &rule, "chat"
				if answer.Scope == "workspace" {
					rule.ChatID = chatID
					ev.Scope = "workspace"
					if st.Environments == nil {
						st.Environments = map[string]*EnvironmentRecord{}
					}
					env := st.Environments[c.SandboxID]
					if env == nil {
						env = &EnvironmentRecord{}
						st.Environments[c.SandboxID] = env
					}
					env.Rules, _ = addRule(env.Rules, rule)
				} else {
					c.Rules, _ = addRule(c.Rules, rule)
				}
			}
			c.record(ev)
			return nil
		})
		if err != nil {
			return err
		}
		result = decision
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

// validateAgent is sandbox.ValidateAgent plus this Warden's leave: the
// 1M-context variants only when the operator allows them.
func (e *Engine) validateAgent(provider, model string) error {
	if err := sandbox.ValidateAgent(provider, model); err != nil {
		return err
	}
	if longContextModel(model) && !e.AllowLongContext {
		return errors.New("1M-context models are not enabled on this Warden (providers.claude.allowLongContext)")
	}
	return nil
}

// ConfigureAgent records a chat's provider and model for its next session
// (an idle chat only). ConfigureAgentAndRelease is the route's entry: it
// applies the model to a live session where it can.
func (e *Engine) ConfigureAgent(id, provider, model string) error {
	return e.configureAgent(id, provider, model, true)
}

// configureAgent stores the selection; with idleOnly the chat must not be
// running (the session is about to be released), otherwise the model was
// applied to the live session and a running turn is fine. A model change
// on a conversation leaves a marker.
func (e *Engine) configureAgent(id, provider, model string, idleOnly bool) error {
	if err := e.validateAgent(provider, model); err != nil {
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
		if idleOnly && (c.Status == "running" || c.Status == "queued" || c.Status == "stopping") {
			return errors.New("wait until the conversation is idle")
		}
		old := c.Provider
		if old == "" {
			old = "codex"
		}
		if provider != old && len(c.Conversation.Entries) > 0 {
			return errors.New("start a new conversation to change providers")
		}
		if provider != old {
			c.Commands, c.Session = nil, nil
		}
		if model != c.Model && len(c.Conversation.Entries) > 0 {
			c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", modelNotice(model)))
		}
		c.Provider = provider
		c.Model = model
		return nil
	})
}

// ConfigureAgentAndRelease applies a provider and model choice. A model
// change on a Claude chat with a live session is made on that session
// (set_model through the adapter): the process, its thread and its
// context stay, and the CLI's next system/init reports the model it
// resolved (chat.session.model). A model the CLI does not know is refused
// with its message and nothing changes. Otherwise (no live session, the
// CLI refusing the request for another reason, a Codex chat, whose
// app-server takes the model at launch here) the selection is recorded
// and the resident session ends, so the next message starts an agent
// with it.
func (e *Engine) ConfigureAgentAndRelease(ctx context.Context, id, provider, model string) error {
	if provider == "" {
		provider = "codex"
	}
	if model == "" {
		model = e.DefaultModel(provider)
	}
	if current := e.Store.Snapshot().chat(id); current != nil && current.Provider == "claude" && provider == "claude" && model != current.Model {
		if client := e.liveClient(id); client != nil {
			if err := e.validateAgent(provider, model); err != nil {
				return err
			}
			err := e.pushModel(ctx, client, model)
			if err == nil {
				return e.configureAgent(id, provider, model, false)
			}
			if modelRefused(err) {
				return err
			}
			log.Printf("model %s not applied to the running session of chat %s (%v); restarting the session", model, id, err)
		}
	}
	if err := e.ConfigureAgent(id, provider, model); err != nil {
		return err
	}
	e.releaseChat(ctx, id)
	return nil
}
