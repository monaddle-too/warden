package chats

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"warden/chat/internal/conversation"
)

func TestOrganizationCannotOpenAnotherWorkspaceOrChat(t *testing.T) {
	e, _, stop := setup(t)
	defer stop()
	actor := conversation.Actor{PrincipalID: "alice"}
	first, err := e.CreateForOrganization("org-one", actor, "Private", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := e.Store.Snapshot().chat(first)
	if chat == nil {
		t.Fatal("chat missing")
	}
	if _, err = e.CreateForOrganization("org-two", actor, "Intruder", chat.SandboxID, "", nil); err == nil || !strings.Contains(err.Error(), "another organization") {
		t.Fatalf("shared workspace accepted: %v", err)
	}
	h := &HTTP{Engine: e}
	for _, path := range []string{"chats/" + first + "/file", "chats/" + first + "/attachments/x", "environments/" + chat.SandboxID + "/rules"} {
		recorder := httptest.NewRecorder()
		if h.organizationGuard(recorder, path, "org-two") || recorder.Code != http.StatusNotFound {
			t.Fatalf("cross-org %s: status %d", path, recorder.Code)
		}
	}
	view := organizationView(View{State: e.Store.Snapshot()}, "org-two")
	if len(view.Chats) != 0 || len(view.Ports) != 0 {
		t.Fatal("other organization's state leaked")
	}
	if hits := e.Search("Private", 10, "org-two"); len(hits.Hits) != 0 {
		t.Fatal("other organization's search result leaked")
	}
}

// The browser consumes the workspace inventory as an array, including when
// the organization has never created a workspace.
func TestEmptyOrganizationWorkspaceArray(t *testing.T) {
	e, _, stop := setup(t)
	defer stop()
	h := &HTTP{Engine: e, Token: "test-token", Host: "example.com"}
	check := func(org string) {
		t.Helper()
		r := httptest.NewRequest("GET", "https://example.com/api/environments", nil)
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set("X-Warden-Organization", org)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatalf("empty inventory: %d %s", w.Code, w.Body.String())
		}
	}
	check("")
	check("empty-org")
	if _, err := e.CreateForOrganization("other-org", conversation.Actor{PrincipalID: "alice"}, "Private", "", "", nil); err != nil {
		t.Fatal(err)
	}
	check("empty-org")
}
