// Package chats owns Warden's local chat state. No Panta service or database is used.
package chats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

type Approval struct {
	ID     string          `json:"id"`
	RunID  string          `json:"runID"`
	RPCID  json.RawMessage `json:"-"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
	State  string          `json:"state"`
}
type Chat struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	SandboxID  string `json:"sandboxID"`
	Repository string `json:"repository"`
	// Resources is the size the workspace was created with, chosen by
	// whoever started its first chat; nil is the runner's default. The
	// runner's record is the current size (a grant or the owner may have
	// changed it since), which Environment.Resources reports.
	Resources *sandbox.Resources `json:"resources,omitempty"`
	// Network is the workspace's own network access (network.go): "" follows
	// the install's egress setting, else "restricted" or "open". Chosen by
	// the owner at creation or from the workspace panel, never by an agent;
	// the same on every chat of the workspace.
	Network string `json:"network,omitempty"`
	// Mode is the chat's permission mode (permissions.go): auto when
	// empty. Rules are its own permission rules (rules.go: "Allow always"
	// answers and rules added to the chat), in the order given; Allowed
	// is where item 3 kept them, read once and moved into Rules on open.
	// Permissions is the chat's permission history (the last historyCap
	// decisions), served by chats/{id}/permissions and left out of the
	// state clients stream.
	Mode        string            `json:"mode,omitempty"`
	Rules       []Rule            `json:"rules,omitempty"`
	Allowed     []Rule            `json:"allowed,omitempty"`
	Permissions []PermissionEvent `json:"permissions,omitempty"`
	// Thinking, Effort and Fast are the chat's session settings
	// (settings.go): the thinking budget ("" the agent's default, "off",
	// or a number of tokens), the effort level ("" the model's default)
	// and fast mode. Applied to a live Claude session at once and to every
	// session at its start.
	Thinking     string                    `json:"thinking,omitempty"`
	Effort       string                    `json:"effort,omitempty"`
	Fast         bool                      `json:"fast,omitempty"`
	Status       string                    `json:"status"`
	RunID        string                    `json:"runID"`
	Error        string                    `json:"error,omitempty"`
	Archived     bool                      `json:"archived"`
	Conversation conversation.Conversation `json:"conversation"`
	Approvals    []Approval                `json:"approvals"`
	// Reviews are the agent's requests that only the app can settle (a
	// pull request proposal, suggested document edits, a document
	// selection or creation), kept while they wait in the policy service
	// (reviews.go); the other clients point at the app for them.
	Reviews []Review `json:"reviews,omitempty"`
	// Commands is what the agent's session offers as slash commands (Claude
	// Code's built-ins and the workspace's own commands and skills, from its
	// `system/init`), for the composer's "/" menu. A message "/name …" is
	// sent as text and the agent expands it. Empty for Codex.
	Commands []Command `json:"commands,omitempty"`
	// Session is what the agent reported when its session started: the
	// model it resolved, its permission mode and output style. Nil for
	// Codex.
	Session *Session `json:"session,omitempty"`
	// Creator is who created the chat (the requester the edge identified,
	// or the owner); their standing instructions reach the agent with the
	// senders' (instructions.go). Nil on a chat from before it was kept.
	Creator *conversation.Actor `json:"creator,omitempty"`
	// Typing is who is composing a message right now. It is filled in for
	// clients by Engine.View and never stored.
	Typing []Typist `json:"typing,omitempty"`
	// Startup is where the chat's start is while its message waits for the
	// agent (startup.go); filled in by Engine.View, never stored.
	Startup *Startup `json:"startup,omitempty"`
	// DiffBase is the checkpoint the session diff is taken against: the
	// chat's first, or the one its last code rewind restored (rewind.go).
	DiffBase string `json:"diffBase,omitempty"`
	// Rewind is a conversation rewind the agent's session has yet to
	// apply, Recap the kept transcript the next message carries to a
	// fresh session, NewSession that the next run starts one instead of
	// resuming the recorded thread (rewind.go).
	Rewind     *PendingRewind `json:"rewind,omitempty"`
	Recap      string         `json:"recap,omitempty"`
	NewSession bool           `json:"newSession,omitempty"`
	// RewoundTail is what the last conversation rewind removed, kept so
	// the rewind can be undone until the next turn starts (rewind.go);
	// never sent to clients, which get UndoRewind, the marker whose
	// rewind can be undone, filled in by Engine.View and never stored.
	RewoundTail *RewoundTail `json:"rewoundTail,omitempty"`
	UndoRewind  string       `json:"undoRewind,omitempty"`
	// ForkSession marks a chat forked from another whose session (the
	// ThreadID it carries) its first run resumes as a copy; cleared once
	// the agent reports the copy's own session (fork.go).
	ForkSession bool `json:"forkSession,omitempty"`
	// OutputStyle is the Claude output style the chat's process launches
	// with; "" is the CLI's default (style.go).
	OutputStyle string `json:"outputStyle,omitempty"`
	// Titled says how the chat got its title: "" while it still has the
	// default one and waits to be named from its first exchange, "auto"
	// once it was, "manual" once a person gave or changed the title, which
	// ends the naming (title.go).
	Titled string `json:"titled,omitempty"`
	// Spend is what the chat's turns took so far (spend.go), filled in for
	// clients by Engine.View and never stored.
	Spend *Spend `json:"spend,omitempty"`
	// Origin, on the first chat of a workspace created as a copy of
	// another (fork.go, a fork with copyWorkspace), says which workspace
	// it was copied from and when; the workspace panel shows it.
	Origin *WorkspaceOrigin `json:"origin,omitempty"`
}

// WorkspaceOrigin records that a workspace was created as a copy of
// another: the source workspace (its sandbox and the name it had then),
// the chat the copy was forked from, and when the copy was taken (unix
// seconds).
type WorkspaceOrigin struct {
	SandboxID string  `json:"sandboxID"`
	Name      string  `json:"name"`
	ChatID    string  `json:"chatID"`
	At        float64 `json:"at"`
}

// Command is one slash command the agent's session offers.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Session is the agent's own report of its session settings: the model
// it resolved (the truth after a live model change), its permission mode,
// output style and whether fast mode is serving ("on", "off", "cooldown").
// AutoMemory is the auto-memory directory the agent's CLI reported for
// the workspace (`memory_paths.auto`), which the memory view lists; ""
// when not reported.
type Session struct {
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	OutputStyle    string `json:"outputStyle,omitempty"`
	FastMode       string `json:"fastMode,omitempty"`
	AutoMemory     string `json:"autoMemory,omitempty"`
}

// sessionStarted records what the agent sent with `thread/started`: the
// commands its session offers and its settings. Codex's carries neither
// and leaves the chat's as they were; Claude's arrives with every turn
// (its `system/init`), so the list follows the workspace.
func (c *Chat) sessionStarted(thread map[string]any) {
	if list, ok := thread["commands"].([]any); ok {
		c.Commands = []Command{}
		for _, v := range list {
			m, _ := v.(map[string]any)
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			description, _ := m["description"].(string)
			c.Commands = append(c.Commands, Command{Name: name, Description: description})
		}
	}
	model, _ := thread["model"].(string)
	mode, _ := thread["permissionMode"].(string)
	style, _ := thread["outputStyle"].(string)
	fast, _ := thread["fastMode"].(string)
	auto, _ := thread["autoMemory"].(string)
	if len(auto) > 1024 || strings.ContainsAny(auto, "\x00\n\r") {
		auto = ""
	}
	if model != "" || mode != "" || style != "" || auto != "" {
		c.Session = &Session{Model: model, PermissionMode: mode, OutputStyle: style, FastMode: fast, AutoMemory: auto}
	}
}

// Typist is one person composing a message in a chat.
type Typist struct {
	PrincipalID string  `json:"principalID"`
	Name        string  `json:"name"`
	Until       float64 `json:"until"` // unix seconds when the indicator lapses
}
type State struct {
	Version          int           `json:"version"`
	Chats            []*Chat       `json:"chats"`
	Ports            []PortBinding `json:"ports"`
	DeletedSandboxes []string      `json:"deletedSandboxes,omitempty"`
	// Instructions are each person's standing instructions for the agent,
	// by principal (instructions.go). Never sent to clients as part of the
	// state: a person reads their own through me/instructions.
	Instructions map[string]*Instructions `json:"instructions,omitempty"`
	// Environments is what is kept per workspace beyond its chats, by
	// sandbox id: its permission rules (rules.go).
	Environments map[string]*EnvironmentRecord `json:"environments,omitempty"`
	// Catalog is each provider's model catalog as its CLI last reported
	// it (catalog.go); clients get it as agentOptions.models.
	Catalog map[string]*Catalog `json:"catalog,omitempty"`
}
type Store struct {
	mu     sync.Mutex
	path   string
	state  State
	failed error
	unlock func()
	// version counts accepted mutations; encoded is the JSON of state at
	// encodedVersion, kept so a snapshot, the before-image of an update
	// and the bytes a save writes cost one marshal per version, not one
	// per reader.
	version        uint64
	encoded        []byte
	encodedVersion uint64
	// dirty is set by stream: the state in memory is newer than chats.json
	// and flush is scheduled (or a durable update writes it first).
	dirty     bool
	flushing  *time.Timer
	saveDelay time.Duration
	// changed is closed and replaced on every mutation, so Wait can block
	// without polling.
	changed chan struct{}
}

// streamDelay is how long stream coalesces writes: a streamed token
// reaches the disk within this after it reached the state.
const streamDelay = 250 * time.Millisecond

func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("chat state must be a real private directory")
	}
	if err = os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	unlock, err := sandbox.LockRoot(root)
	if err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(root, "chats.json"), state: State{Version: 1, Chats: []*Chat{}}, unlock: unlock, changed: make(chan struct{}), saveDelay: streamDelay}
	b, err := os.ReadFile(s.path)
	if err == nil {
		err = json.Unmarshal(b, &s.state)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, err
	}
	if s.state.Version != 1 {
		unlock()
		return nil, fmt.Errorf("unsupported chat data version")
	}
	// A restart never replays a message whose delivery might have reached the agent.
	for _, c := range s.state.Chats {
		if len(c.Allowed) > 0 {
			// Item 3's allow-always rules, in the rules' place now.
			c.Rules = append(c.Rules, c.Allowed...)
			c.Allowed = nil
		}
		if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
			c.Status = "interrupted"
			c.Error = "Warden restarted. Send a new message to resume."
		}
		c.Conversation.ActiveTurnID = nil
		for i := range c.Conversation.Entries {
			e := &c.Conversation.Entries[i]
			e.IsStreaming = false
			if e.Delivery == "queued" || e.Delivery == "sending" {
				e.Delivery = "failed"
				e.Detail = "Interrupted before confirmed delivery"
			}
		}
		for i := range c.Approvals {
			if c.Approvals[i].State == "pending" || c.Approvals[i].State == "resolving" {
				c.Approvals[i].State = "expired"
			}
		}
	}
	if err = s.save(s.encode()); err != nil {
		unlock()
		return nil, err
	}
	return s, nil
}

// Close writes what stream left in memory, then releases the directory.
func (s *Store) Close() {
	s.mu.Lock()
	if s.flushing != nil {
		s.flushing.Stop()
	}
	s.flushLocked()
	s.mu.Unlock()
	s.unlock()
}

// dir is the private state directory; attachments live beside chats.json.
func (s *Store) dir() string { return filepath.Dir(s.path) }

// encode is the JSON of the state, marshalled once per version.
func (s *Store) encode() []byte {
	if s.encoded == nil || s.encodedVersion != s.version {
		s.encoded, _ = json.Marshal(s.state)
		s.encodedVersion = s.version
	}
	return s.encoded
}

// bump records an accepted mutation: the encoding is stale and waiters
// wake.
func (s *Store) bump() {
	s.version++
	close(s.changed)
	s.changed = make(chan struct{})
}

// save writes b as chats.json: a temp file beside it, fsynced, renamed
// over it, the directory fsynced.
func (s *Store) save(b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(s.path), ".chats-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err == nil {
		var d *os.File
		d, err = os.Open(filepath.Dir(s.path))
		if err == nil {
			err = d.Sync()
			d.Close()
		}
	}
	return err
}

// update applies fn and persists the result before returning: a message,
// an approval or a setting is on disk when its caller acknowledges it. A
// mutation that changes nothing is not written. A failed write puts the
// state back and marks the store failed.
func (s *Store) update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return fmt.Errorf("chat storage unavailable: %w", s.failed)
	}
	before := s.encode()
	if err := fn(&s.state); err != nil {
		return err
	}
	after, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		if s.dirty {
			// Nothing new, but what stream left is due; a durable update
			// covers it.
			return s.writeLocked(after)
		}
		return nil
	}
	s.bump()
	s.encoded, s.encodedVersion = after, s.version
	if err := s.writeLocked(after); err != nil {
		_ = json.Unmarshal(before, &s.state)
		s.bump()
		s.encoded, s.encodedVersion = before, s.version
		return err
	}
	return nil
}

// writeLocked saves b as the current state and clears what stream left.
func (s *Store) writeLocked(b []byte) error {
	if err := s.save(b); err != nil {
		s.failed = err
		return err
	}
	s.dirty = false
	if s.flushing != nil {
		s.flushing.Stop()
		s.flushing = nil
	}
	return nil
}

// stream applies fn in memory and writes it within streamDelay, together
// with whatever else streams meanwhile: a streamed token is shown at once
// and reaches the disk shortly after, instead of each token marshalling
// and fsyncing the whole state. The next durable update writes it sooner.
// A restart loses at most the last streamDelay of streamed text, and a
// restart already marks a running chat interrupted.
func (s *Store) stream(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return fmt.Errorf("chat storage unavailable: %w", s.failed)
	}
	if err := fn(&s.state); err != nil {
		return err
	}
	s.bump()
	s.dirty = true
	if s.flushing == nil {
		s.flushing = time.AfterFunc(s.saveDelay, s.flush)
	}
	return nil
}

// flush writes what stream left in memory.
func (s *Store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushing = nil
	s.flushLocked()
}
func (s *Store) flushLocked() {
	if !s.dirty || s.failed != nil {
		return
	}
	if err := s.save(s.encode()); err != nil {
		s.failed = err
		return
	}
	s.dirty = false
}

// Snapshot is an independent copy of the state, decoded from the encoding
// of the current version outside the lock: the lock is held to fetch the
// bytes, not for the decode, and the encoding is shared until the next
// mutation.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	b := s.encode()
	s.mu.Unlock()
	var st State
	_ = json.Unmarshal(b, &st)
	return st
}

// Version is the count of mutations so far; Wait returns once it exceeds
// since, or ctx ends.
func (s *Store) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}
func (s *Store) Wait(ctx context.Context, since uint64) uint64 {
	s.mu.Lock()
	v, ch := s.version, s.changed
	s.mu.Unlock()
	if v > since {
		return v
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return s.Version()
}
func (s State) chat(id string) *Chat {
	for _, c := range s.Chats {
		if c.ID == id {
			return c
		}
	}
	return nil
}
func request(c *Chat, op string) sandbox.Request {
	return sandbox.Request{Provider: c.Provider, Model: c.Model, Operation: op, ProjectID: "warden-local", ChatID: c.ID, SandboxID: c.SandboxID, RunID: c.RunID, PrincipalID: "owner"}
}
