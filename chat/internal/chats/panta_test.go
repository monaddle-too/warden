package chats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPantaToolsBindOrganizationToChat(t *testing.T) {
	org := strings.Repeat("a", 32)
	calls := 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/internal/warden-tools" || r.Header.Get("X-Panta-Organization") != org || r.Header.Get("X-Panta-Key") != "private" {
			t.Error("wrong trusted scope")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["chatId"] != "chat-a" || body["callId"] != "call-a" {
			t.Error("missing host attribution")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[]}`))
	}))
	defer service.Close()
	e := &Engine{DocsAddress: service.URL, DocsKey: "private"}
	c := &Chat{ID: "chat-a", OrganizationID: org}
	for _, tool := range pantaTools() {
		name := tool.(map[string]any)["name"].(string)
		if _, err := e.pantaCall(context.Background(), c, name, "call-a", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 8 {
		t.Fatalf("calls=%d", calls)
	}
	if _, err := e.pantaCall(context.Background(), &Chat{ID: "local"}, "panta_list_documents", "call-a", map[string]any{}); err == nil {
		t.Fatal("unscoped call allowed")
	}
	if _, err := e.pantaCall(context.Background(), c, "panta_resolve_suggestion", "call-a", map[string]any{}); err == nil {
		t.Fatal("unknown tool allowed")
	}
	if calls != 8 {
		t.Fatal("invalid calls reached service")
	}
}
