package chats

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
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
	id, err := e.Create("Images", "", "", nil)
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

// An image a Read returned is stored through the policy image store as
// attach_image's are, and the read entry keeps the stored id in place of
// the bytes; without a policy service the bytes are dropped and the entry
// keeps the image's description.
func TestReadImageIsStoredNotKept(t *testing.T) {
	e, w, id := claudeSetup(t)
	sharing, socket := newFakeSharing(t)
	e.PolicyAddress = "unix://" + socket
	sharing.results["image_add"] = map[string]any{"image_id": "img-1"}
	var normalized [][]byte
	e.NormalizeImage = func(_ context.Context, raw []byte) ([]byte, error) {
		normalized = append(normalized, raw)
		return append([]byte("PNG:"), raw...), nil
	}
	png := "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAUklEQVR42u3YwQkAAAwCMfdful2iUIQcLpCvmfICAAAAAAAAAAAAAAAAAPABSG4GAAAAAAAAAAAAAAAAAAAAANAF8MwBAAAAAAAAAAAAAAAA9LZBylcGAx5/OwAAAABJRU5ErkJggg=="
	w.mu.Lock()
	w.frames = []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_img", "name": "Read", "input": map[string]any{"file_path": "/home/agent/workspace/img.png"}}}}},
		{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_img", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}}}}}}, "tool_use_result": map[string]any{"type": "image", "file": map[string]any{"base64": png, "type": "image/png", "originalSize": 139.0, "dimensions": map[string]any{"originalWidth": 64.0, "originalHeight": 64.0}}}},
	}
	w.mu.Unlock()
	oneTurn(t, e, id, "read the image")
	var entry *cv.Entry
	for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
		if v.ID == "toolu_img" {
			entry = &v
		}
	}
	if entry == nil || entry.Tool == nil || entry.Tool.Kind != "read" || entry.Tool.Read == nil {
		t.Fatalf("read entry %+v", entry)
	}
	if r := entry.Tool.Read; r.Kind != "image" || r.Image != "img-1" || r.Width != 64 || r.Height != 64 || r.Bytes != 139 || entry.Detail != "PNG image, 64×64, 139 bytes" {
		t.Fatalf("read record %+v detail %q", r, entry.Detail)
	}
	raw, _ := base64.StdEncoding.DecodeString(png)
	if len(normalized) != 1 || string(normalized[0]) != string(raw) {
		t.Fatal("the bytes did not pass the normaliser once", len(normalized))
	}
	var add map[string]any
	for i := 0; ; i++ {
		op := sharing.op(i)
		if op["action"] == "image_add" {
			add = agent.Map(op["data"])
			break
		}
	}
	if add["caption"] != "Read img.png" || add["png"] != base64.StdEncoding.EncodeToString(append([]byte("PNG:"), raw...)) {
		t.Fatalf("image_add %v", add)
	}
	if data, _ := json.Marshal(e.Store.Snapshot().chat(id)); strings.Contains(string(data), png[:40]) {
		t.Fatal("the image's bytes reached the store")
	}
	// Without the policy service: no store, description kept.
	e.PolicyAddress = ""
	w.mu.Lock()
	w.frames = []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_img2", "name": "Read", "input": map[string]any{"file_path": "/home/agent/workspace/img.png"}}}}},
		{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_img2", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}}}}}}, "tool_use_result": map[string]any{"type": "image", "file": map[string]any{"base64": png, "type": "image/png"}}},
	}
	w.mu.Unlock()
	oneTurn(t, e, id, "again")
	for _, v := range e.Store.Snapshot().chat(id).Conversation.Entries {
		if v.ID == "toolu_img2" {
			if v.Tool.Read == nil || v.Tool.Read.Image != "" || v.Tool.Read.Kind != "image" || v.Detail != "PNG image" {
				t.Fatalf("unstored read %+v %q", v.Tool.Read, v.Detail)
			}
		}
	}
}
