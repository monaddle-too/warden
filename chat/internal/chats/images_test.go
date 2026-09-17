package chats

import (
	"net/http/httptest"
	"testing"
	"warden/chat/internal/agent"
)

// /published/<token>.png needs no sign-in: the token is the capability,
// and the policy service decides whether it is live.
func TestPublishedImageRouteIsTokenGated(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sharing, socket := newFakeSharing(t)
	sharing.results["image_published"] = map[string]any{"png": "iVBORw0KGgo="}
	e := NewEngine(store, nil)
	e.PolicyAddress = "unix://" + socket
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780", WebDir: t.TempDir()}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost:18780/published/0123456789abcdef0123456789abcdef.png", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() != 8 {
		t.Fatalf("%d %s %d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	if sharing.op(0)["action"] != "image_published" || agent.Map(sharing.op(0)["data"])["token"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("%v", sharing.op(0))
	}
	for _, path := range []string{"/published/x", "/published/a/b.png", "/published/.png"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost:18780"+path, nil))
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestImageEndpointRequiresOwnerAndExistingConversation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := &HTTP{Engine: NewEngine(store, nil), Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	for _, authenticated := range []bool{false, true} {
		r := httptest.NewRequest("GET", "http://localhost:18780/api/chats/missing/images/abc", nil)
		if authenticated {
			r.Header.Set("Authorization", "Bearer owner-secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 401
		if authenticated {
			want = 404
		}
		if w.Code != want {
			t.Fatalf("got %d, want %d", w.Code, want)
		}
	}
}
