package chats

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharingNotificationSurvivesInterruptedDelivery(t *testing.T) {
	root, err := os.MkdirTemp("", "wshare-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	store, err := Open(filepath.Join(root, "chat"))
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store, nil)
	id, err := engine.Create("test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := store.Snapshot().chat(id).SandboxID
	var ack atomic.Bool
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var m map[string]any
				_ = json.NewDecoder(conn).Decode(&m)
				result := map[string]any{}
				if m["action"] == "undelivered" && !ack.Load() {
					result["requests"] = []any{map[string]any{"request_id": "1234567890123456789012345678901234567890123456789012345678901234", "chatID": id, "sandboxID": sandbox, "status": "denied", "documents": []any{}}}
				}
				if m["action"] == "ack" {
					ack.Store(true)
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "result": result})
			}()
		}
	}()
	run := func(e *Engine, condition func() bool) {
		t.Helper()
		e.PolicyAddress = "unix://" + socket
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); e.sharingDelivery(ctx) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(5 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("notification did not progress")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	run(engine, func() bool { return store.Snapshot().chat(id).Status == "queued" })
	if ack.Load() {
		t.Fatal("acknowledged before delivery")
	}
	store.Close()
	store, err = Open(filepath.Join(root, "chat"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Snapshot().chat(id).Conversation.Entries[0].Delivery != "failed" {
		t.Fatal("expected interrupted queued message")
	}
	engine = NewEngine(store, nil)
	run(engine, func() bool { return store.Snapshot().chat(id).Status == "queued" })
	if len(store.Snapshot().chat(id).Conversation.Entries) != 1 {
		t.Fatal("duplicate notification")
	}
	_ = store.update(func(st *State) error {
		st.chat(id).Conversation.Entries[0].Delivery = "sent"
		st.chat(id).Status = "idle"
		return nil
	})
	run(engine, ack.Load)
}

func TestRejectedPullRequestResumesWithFeedback(t *testing.T) {
	root, err := os.MkdirTemp("", "wpr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	store, err := Open(filepath.Join(root, "chat"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := NewEngine(store, nil)
	id, err := engine.Create("PR review", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := store.Snapshot().chat(id).SandboxID
	socket := filepath.Join(root, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var m map[string]any
				_ = json.NewDecoder(conn).Decode(&m)
				_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "result": map[string]any{"requests": []any{map[string]any{"request_id": "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd", "kind": "pull_request", "chatID": id, "sandboxID": sandbox, "status": "rejected", "feedback": "Add coverage for empty inputs"}}}})
			}()
		}
	}()
	engine.PolicyAddress = "unix://" + socket
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); engine.sharingDelivery(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := store.Snapshot().chat(id)
		if len(c.Conversation.Entries) > 0 {
			text := c.Conversation.Entries[0].Text
			if !strings.Contains(text, "Add coverage for empty inputs") || !strings.Contains(text, "request_pull_request") || strings.Contains(text, "list_shared_documents") {
				t.Fatalf("unexpected continuation: %s", text)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("rejection did not resume conversation")
}
