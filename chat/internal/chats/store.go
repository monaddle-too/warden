// Package chats owns Warden's local chat state. No Panta service or database is used.
package chats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
}

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
	s := &Store{path: filepath.Join(root, "chats.json"), state: State{Version: 1, Chats: []*Chat{}}, unlock: unlock}
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
	if err = s.save(); err != nil {
		unlock()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() { s.unlock() }

// dir is the private state directory; attachments live beside chats.json.
func (s *Store) dir() string { return filepath.Dir(s.path) }
func (s *Store) save() error {
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
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
func (s *Store) update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return fmt.Errorf("chat storage unavailable: %w", s.failed)
	}
	// Mutations validate first; persist before acknowledging or dispatching work.
	before, _ := json.Marshal(s.state)
	if err := fn(&s.state); err != nil {
		return err
	}
	after, _ := json.Marshal(s.state)
	if bytes.Equal(before, after) {
		return nil
	}
	if err := s.save(); err != nil {
		_ = json.Unmarshal(before, &s.state)
		s.failed = err
		return err
	}
	return nil
}
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var st State
	_ = json.Unmarshal(b, &st)
	return st
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
