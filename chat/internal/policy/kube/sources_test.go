package kube

import (
	"encoding/json"
	"net"
	"testing"

	api "warden/chat/internal/kube"
)

func podEvent(t *testing.T, typ api.EventType, name, uid, ip, phase string, deleting bool) api.Event {
	t.Helper()
	obj := map[string]any{
		"metadata": map[string]any{"name": name + "-pod", "uid": uid, "labels": map[string]string{LabelSandbox: name}},
		"status":   map[string]any{"phase": phase, "podIP": ip},
	}
	if deleting {
		obj["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-17T00:00:00Z"
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return api.Event{Type: typ, Object: raw}
}

func TestSourcesFollowTheLivePod(t *testing.T) {
	var s sources
	remote := net.ParseIP("10.42.0.25")
	s.apply(podEvent(t, api.Added, "wc-a", "u1", "10.42.0.25", "Running", false))
	if s.allows("wc-a", remote) {
		t.Fatal("allowed before the initial list synced")
	}
	s.apply(api.Event{Type: api.Synced})
	if !s.allows("wc-a", remote) {
		t.Fatal("the running pod's address must be allowed")
	}
	if s.allows("wc-a", net.ParseIP("10.42.0.26")) || s.allows("wc-b", remote) || s.allows("", remote) || s.allows("wc-a", nil) {
		t.Fatal("another address, another runtime or no runtime must be refused")
	}
	// A replacement pod appearing beside the old one is ambiguous until the
	// old one is gone.
	s.apply(podEvent(t, api.Added, "wc-a", "u2", "10.42.0.30", "Running", false))
	if s.allows("wc-a", remote) || s.allows("wc-a", net.ParseIP("10.42.0.30")) {
		t.Fatal("two live pods must be refused")
	}
	s.apply(podEvent(t, api.Modified, "wc-a", "u1", "10.42.0.25", "Running", true))
	if !s.allows("wc-a", net.ParseIP("10.42.0.30")) || s.allows("wc-a", remote) {
		t.Fatal("the terminating pod must drop out")
	}
	s.apply(podEvent(t, api.Deleted, "wc-a", "u2", "10.42.0.30", "Running", false))
	if s.allows("wc-a", net.ParseIP("10.42.0.30")) {
		t.Fatal("a deleted pod must be refused")
	}
	// Pending pods have no address yet.
	s.apply(podEvent(t, api.Added, "wc-c", "u3", "", "Pending", false))
	if s.allows("wc-c", net.ParseIP("10.42.0.40")) {
		t.Fatal("a pending pod must be refused")
	}
}

func TestSourceAllowedExemptsLoopback(t *testing.T) {
	i := &Inspector{}
	if !i.SourceAllowed("anything", net.ParseIP("127.0.0.1")) {
		t.Fatal("the policy pod's own probes come from loopback")
	}
	if i.SourceAllowed("anything", net.ParseIP("10.42.0.1")) {
		t.Fatal("an unknown pod must be refused")
	}
}
