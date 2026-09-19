package chats

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/sandbox"
)

// onDisk is the state as the database holds it, read over a connection of
// its own (WAL lets it read beside the store's).
func onDisk(t *testing.T, s *Store) State {
	t.Helper()
	db, err := openChatDB(filepath.Join(s.root, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other := &Store{db: db}
	if err := other.load(); err != nil {
		t.Fatal(err)
	}
	return other.state
}

// A durable update is on disk when it returns; a streamed one is in every
// snapshot at once and on disk within the delay, or sooner when a
// durable update follows it. Nothing is written when nothing changed.
func TestStoreStreamCoalescesWrites(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.saveDelay = 50 * time.Millisecond
	if err := s.update(func(st *State) error { st.Chats = append(st.Chats, &Chat{ID: "c", Title: "t"}); return nil }); err != nil {
		t.Fatal(err)
	}
	v := s.Version()
	if got := onDisk(t, s); len(got.Chats) != 1 || got.Chats[0].Title != "t" {
		t.Fatalf("durable update not on disk: %+v", got.Chats)
	}
	for _, part := range []string{"Hel", "lo"} {
		if err := s.stream(func(st *State) error { st.Chats[0].Title += part; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if s.Version() != v+2 {
		t.Fatalf("version %d after two streamed mutations from %d", s.Version(), v)
	}
	if got := s.Snapshot().Chats[0].Title; got != "tHello" {
		t.Fatalf("snapshot misses the streamed text: %q", got)
	}
	if got := onDisk(t, s).Chats[0].Title; got != "t" {
		t.Fatalf("streamed text written at once: %q", got)
	}
	until(t, func() bool { return onDisk(t, s).Chats[0].Title == "tHello" })
	if err := s.stream(func(st *State) error { st.Chats[0].Title += "!"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(st *State) error { st.Chats[0].Archived = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, s); got.Chats[0].Title != "tHello!" || !got.Chats[0].Archived {
		t.Fatalf("the durable update did not carry the streamed text: %+v", got.Chats[0])
	}
	writes := s.writes
	if err := s.update(func(st *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s.writes != writes || s.Version() != v+4 {
		t.Fatal("a mutation that changed nothing was written or counted")
	}
}

// Snapshots of one version decode the same encoding; Wait returns on the
// next mutation and not before.
func TestStoreSnapshotEncodesOncePerVersionAndWaitWakes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.update(func(st *State) error { st.Chats = append(st.Chats, &Chat{ID: "c"}); return nil })
	s.mu.Lock()
	first := s.encode()
	s.mu.Unlock()
	a, b := s.Snapshot(), s.Snapshot()
	s.mu.Lock()
	again := s.encode()
	s.mu.Unlock()
	if &first[0] != &again[0] {
		t.Fatal("the encoding was rebuilt without a mutation")
	}
	a.Chats[0].Title = "mine"
	if b.Chats[0].Title != "" {
		t.Fatal("snapshots share chats")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	v := s.Version()
	if got := s.Wait(ctx, v); got != v || ctx.Err() == nil {
		t.Fatalf("Wait returned %d before a mutation", got)
	}
	woke := make(chan uint64, 1)
	go func() { woke <- s.Wait(context.Background(), v) }()
	time.Sleep(20 * time.Millisecond)
	_ = s.stream(func(st *State) error { st.Chats[0].Title = "x"; return nil })
	select {
	case got := <-woke:
		if got != v+1 {
			t.Fatalf("woke at %d, want %d", got, v+1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake on the mutation")
	}
}

// stalledWorker answers health only once released, as a runner whose
// mutex a workspace stop holds.
type stalledWorker struct {
	fakeWorker
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

func (w *stalledWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	if r.Operation != "health" {
		return w.fakeWorker.Call(ctx, r)
	}
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	select {
	case <-w.release:
		l := sbxLimits
		return sandbox.Response{Limits: &l}, nil
	case <-ctx.Done():
		return sandbox.Response{}, ctx.Err()
	}
}

// The view never waits on the runner: an offer once fetched is served
// from memory while the refresher keeps it current, and the encoded
// view is built once per generation, not once per reader.
func TestViewDoesNotWaitOnTheRunner(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &stalledWorker{release: make(chan struct{})}
	e := NewEngine(s, w)
	close(w.release)
	if e.View().Sandboxes == nil {
		t.Fatal("the first view fetched no offer")
	}
	w.release = make(chan struct{}) // the runner stalls from here on
	e.limitsMu.Lock()
	e.limitsAt = time.Time{} // the offer is due for a refresh
	e.limitsMu.Unlock()
	started := time.Now()
	data, key, _ := e.ViewJSON()
	if time.Since(started) > time.Second {
		t.Fatalf("the view waited %s on the runner", time.Since(started))
	}
	if !strings.Contains(string(data), `"sandboxes"`) {
		t.Fatal("the stalled refresh dropped the offer from the view")
	}
	again, key2, _ := e.ViewJSON()
	if key2 != key || &again[0] != &data[0] {
		t.Fatal("the view was rebuilt without a change")
	}
	e.setStartup("c", stageQueued, "")
	if _, key3, _ := e.ViewJSON(); key3 == key {
		t.Fatal("a startup stage did not change the view's generation")
	}
	_ = s.update(func(st *State) error { st.Chats = append(st.Chats, &Chat{ID: "c"}); return nil })
	if _, key4, _ := e.ViewJSON(); key4.store == key.store {
		t.Fatal("a store mutation did not change the view's generation")
	}
	close(w.release)
}

// streamRecorder is a response the test may read while the handler
// writes.
type streamRecorder struct {
	mu   sync.Mutex
	head http.Header
	body strings.Builder
}

func (r *streamRecorder) Header() http.Header { r.mu.Lock(); defer r.mu.Unlock(); return r.head }
func (r *streamRecorder) WriteHeader(int)     {}
func (r *streamRecorder) Flush()              {}
func (r *streamRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}
func (r *streamRecorder) text() string { r.mu.Lock(); defer r.mu.Unlock(); return r.body.String() }

// An event stream sends a frame on a change, paced, and none while idle
// (a keepalive apart).
func TestEventsStreamSendsOnChangeOnly(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := NewEngine(s, &fakeWorker{})
	h := &HTTP{Engine: e}
	rec := &streamRecorder{head: http.Header{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); h.events(rec, req) }()
	frames := func() int { return strings.Count(rec.text(), "data: ") }
	until(t, func() bool { return frames() == 1 })
	time.Sleep(500 * time.Millisecond)
	if n := frames(); n != 1 {
		t.Fatalf("%d frames while idle", n)
	}
	_ = s.update(func(st *State) error { st.Chats = append(st.Chats, &Chat{ID: "c", Title: "one"}); return nil })
	until(t, func() bool { return frames() == 2 })
	for i := 0; i < 5; i++ {
		_ = s.stream(func(st *State) error { st.Chats[0].Title += "."; return nil })
	}
	until(t, func() bool { return strings.Contains(rec.text(), `"one....."`) })
	if n := frames(); n > 4 {
		t.Fatalf("%d frames for a burst of five streamed tokens", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end with its request")
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal(rec.Header())
	}
}
