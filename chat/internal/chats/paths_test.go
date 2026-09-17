package chats

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The composer's @-mention completion asks the worker's paths op for the
// typed prefix and passes its answer through as JSON.
func TestPathsRouteCompletesWorkspacePaths(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Paths", "", "")
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
	if rec := get("chats/"+id+"/paths?q=src", false); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	if rec := get("chats/missing/paths?q=src", true); rec.Code != 404 {
		t.Fatalf("missing chat: %d", rec.Code)
	}
	if rec := get("chats/"+id+"/paths?q="+strings.Repeat("a", 1025), true); rec.Code != 400 {
		t.Fatalf("long query: %d", rec.Code)
	}
	// No answer from the worker is an empty list, never null.
	if rec := get("chats/"+id+"/paths?q=none", true); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"paths":[]}` {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	w.paths = []string{"src/components/", "src/conv.ts"}
	before := len(w.requests)
	w.mu.Unlock()
	rec := get("chats/"+id+"/paths?q=src%2Fco", true)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"paths":["src/components/","src/conv.ts"]}` {
		t.Fatalf("listing: %d %s", rec.Code, rec.Body.String())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) != before+1 || w.requests[before].Operation != "paths" || w.requests[before].Directory != "src/co" || w.requests[before].ChatID != id {
		t.Fatalf("%+v", w.requests[before:])
	}
	// A worker refusal (a stopped sandbox) reaches the composer as the error.
	w.fail = true
	w.mu.Unlock()
	rec = get("chats/"+id+"/paths?q=src", true)
	w.mu.Lock()
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "unverified sandbox") {
		t.Fatalf("failure: %d %s", rec.Code, rec.Body.String())
	}
}
