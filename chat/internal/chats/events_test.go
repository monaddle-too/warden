package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventsDeliverStopAfterIdleDeadline(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.update(func(s *State) error { s.Chats = []*Chat{{ID: "demo", Status: "running"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: &Engine{Store: store}}
	server := httptest.NewServer(http.HandlerFunc(h.events))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// An idle stream previously kept its expired ten-second write deadline.
	time.Sleep(11 * time.Second)
	if err := store.update(func(s *State) error { s.Chats[0].Status = "interrupted"; return nil }); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream lost stop update after idle: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var state State
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &state); err != nil {
			t.Fatal(err)
		}
		if state.Chats[0].Status == "interrupted" {
			return
		}
	}
}
