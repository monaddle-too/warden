package chats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// GET capacity is the runner's capacity answer, shared by every caller
// for a few seconds so an open picker's polling costs one read.
func TestCapacityRouteCachesTheRunnerAnswer(t *testing.T) {
	e, w := sizeEngine(t, sbxLimits)
	clock := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return clock }
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780", WebDir: t.TempDir()}
	get := func() map[string]any {
		req := httptest.NewRequest("GET", "http://localhost:18780/api/capacity", nil)
		req.Header.Set("Authorization", "Bearer owner-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := get()
	if got["kind"] != "host" || got["cpuMilli"] != 14000.0 || got["memoryAvailableMB"] != 21504.0 || got["running"] != 2.0 || got["reserved"].(map[string]any)["memoryMB"] != 4096.0 || got["limits"].(map[string]any)["max"].(map[string]any)["memoryMB"] != 8192.0 {
		t.Fatalf("%+v", got)
	}
	calls := func() int {
		w.mu.Lock()
		defer w.mu.Unlock()
		n := 0
		for _, op := range w.ops {
			if op == "capacity" {
				n++
			}
		}
		return n
	}
	get()
	if calls() != 1 {
		t.Fatalf("a second read within the TTL asked the runner again: %d", calls())
	}
	clock = clock.Add(capacityTTL + time.Second)
	if _, err := e.Capacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls() != 2 {
		t.Fatalf("a read past the TTL did not ask the runner: %d", calls())
	}
}
