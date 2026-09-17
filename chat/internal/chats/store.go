// Package chats owns Warden's local chat state. No Panta service or database is used.
package chats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Provider     string                    `json:"provider"`
	Model        string                    `json:"model"`
	ID           string                    `json:"id"`
	Title        string                    `json:"title"`
	SandboxID    string                    `json:"sandboxID"`
	Repository   string                    `json:"repository"`
	Status       string                    `json:"status"`
	RunID        string                    `json:"runID"`
	Error        string                    `json:"error,omitempty"`
	Archived     bool                      `json:"archived"`
	Conversation conversation.Conversation `json:"conversation"`
	Approvals    []Approval                `json:"approvals"`
	// Typing is who is composing a message right now. It is filled in for
	// clients by Engine.View and never stored.
	Typing []Typist `json:"typing,omitempty"`
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
		if c.Status == "running" || c.Status == "queued" || c.Status == "stopping" {
			c.Status = "interrupted"
			c.Error = "Warden restarted. Send a new message to resume."
		}
		c.Conversation.ActiveTurnID = nil
		for i := range c.Conversation.Entries {
			e := &c.Conversation.Entries[i]
			e.IsStreaming = false
			if e.Delivery == "queued" {
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
