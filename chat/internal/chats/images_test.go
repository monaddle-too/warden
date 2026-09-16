package chats

import (
	"net/http/httptest"
	"testing"
)

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
