package chats

import (
	"net/http/httptest"
	"strings"
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

// The transcript's inline images read workspace files through the worker's
// image-file op (relative paths only, no symlinks) and never store anything.
// The fake worker returns no bytes, which stops the request before the
// normaliser subprocess would run.
func TestImageFileRouteReadsWorkspaceImages(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Images", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	get := func(path string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost:18780/api/"+path, nil)
		if authenticated {
			r.Header.Set("Authorization", "Bearer owner-secret")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := get("chats/"+id+"/image-file?path=out/plot.png", false); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	if rec := get("chats/missing/image-file?path=out/plot.png", true); rec.Code != 404 {
		t.Fatalf("missing chat: %d", rec.Code)
	}
	if rec := get("chats/"+id+"/image-file", true); rec.Code != 400 {
		t.Fatalf("no path: %d", rec.Code)
	}
	if rec := get("chats/"+id+"/image-file?path="+strings.Repeat("a", 1025), true); rec.Code != 400 {
		t.Fatalf("long path: %d", rec.Code)
	}
	w.mu.Lock()
	before := len(w.requests)
	w.mu.Unlock()
	rec := get("chats/"+id+"/image-file?path=out%2Fplot.png", true)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "image unavailable") {
		t.Fatalf("empty file: %d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) != before+1 || w.requests[before].Operation != "image-file" || w.requests[before].Directory != "out/plot.png" || w.requests[before].ChatID != id {
		t.Fatalf("%+v", w.requests[before:])
	}
}
