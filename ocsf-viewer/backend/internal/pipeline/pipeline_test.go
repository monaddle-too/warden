package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const good = `{"time":1788990000000,"class_uid":3002,"category_uid":3,"activity_id":1,"type_uid":300201,"severity_id":2,"metadata":{"version":"1.6.0","product":{"name":"test","vendor_name":"test"}},"message":"DB::Exception: harmless log text","src_endpoint":{"ip":"10.0.0.1"}}`

func TestValidationAndQuarantine(t *testing.T) {
	b, e := parse([]byte(good+"\n\nnot json\n"+strings.Replace(good, `"type_uid":300201`, `"type_uid":1`, 1)), "application/x-ndjson", "batch", "stream", time.Now())
	if e != nil || b.Accepted != 1 || b.Rejected != 2 || b.Rejections[0].Record != 3 || b.Events[0].Raw != good {
		t.Fatalf("%+v %v", b, e)
	}
	for _, body := range []string{"[", "[]", "null", `{"events":{}}`, strings.Repeat(good+"\n", 2001)} {
		ct := "application/json"
		if strings.Contains(body, "\n") {
			ct = "application/x-ndjson"
		}
		if _, e := parse([]byte(body), ct, "b", "s", time.Now()); e == nil {
			t.Errorf("accepted malformed/oversize batch")
		}
	}
	for _, bad := range []string{strings.Replace(good, `"severity_id":2`, `"severity_id":7`, 1), strings.Replace(good, `"category_uid":3`, `"category_uid":2`, 1), strings.Replace(good, `"time":1788990000000`, `"time":"now"`, 1), strings.Replace(good, `"vendor_name":"test"`, `"vendor_name":""`, 1), strings.Repeat("x", MaxEvent+1)} {
		if _, e := validate([]byte(bad), "b", "s", 1, 1); e == nil {
			t.Error("invalid record accepted")
		}
	}
	for _, body := range []string{good, "[" + good + "]", `{"events":[` + good + `]}`} {
		b, e := parse([]byte(body), "application/json", "b", "s", time.Now())
		if e != nil || b.Accepted != 1 {
			t.Fatal(e, b)
		}
	}
}
func TestNestingLimit(t *testing.T) {
	deep := strings.TrimSuffix(good, "}") + `,"nested":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}"
	if _, e := validate([]byte(deep), "b", "s", 1, 1); e == nil {
		t.Fatal("unbounded nesting accepted")
	}
	quoted := strings.TrimSuffix(good, "}") + `,"quoted":"` + strings.Repeat("[", 100) + `"}`
	if _, e := validate([]byte(quoted), "b", "s", 1, 1); e != nil {
		t.Fatal("string content counted as nesting", e)
	}
}

func TestStoreRestartIdempotencyCapacityAndCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s, e := OpenStore(path, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := parse([]byte(good), "application/json", "b", "s", time.Now())
	var wg sync.WaitGroup
	var fresh atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, dup, e := s.Put(b, "key", "hash")
			if e != nil {
				t.Error(e)
			}
			if !dup {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Fatal(fresh.Load())
	}
	if _, _, e := s.Put(b, "key", "different"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e := s.Update("b", func(b *Batch) error { b.State = "indexed"; return errors.New("abort") }); e == nil {
		t.Fatal("expected abort")
	}
	stats, _ := s.Stats()
	if stats["queued"] != 1 || stats["indexed"] != 0 || stats["accepted"] != 1 {
		t.Fatal(stats)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = OpenStore(path, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	next, e := s.Next()
	if e != nil || next.ID != "b" || next.Events[0].Raw != good {
		t.Fatal(next, e)
	}
	if e := s.Update("b", func(b *Batch) error { b.State = "indexed"; b.Events = nil; return nil }); e != nil {
		t.Fatal(e)
	}
	next, _ = s.Next()
	if next != nil {
		t.Fatal(next)
	}
	stats, _ = s.Stats()
	if stats["indexed"] != 1 || stats["queued"] != 0 {
		t.Fatal(stats)
	}
	s.maxBytes = 1
	if _, dup, e := s.Put(b, "key", "hash"); e != nil || !dup {
		t.Fatal(e)
	}
	b.ID = "new"
	if _, _, e := s.Put(b, "new", "hash"); !errors.Is(e, ErrFull) {
		t.Fatal(e)
	}
	if e := s.Cleanup(time.Now().Add(8 * 24 * time.Hour)); e != nil {
		t.Fatal(e)
	}
	stats, _ = s.Stats()
	if stats["bytes"] != 0 || stats["accepted"] != 0 {
		t.Fatal(stats)
	}
}
func testAPI(t *testing.T, dbURL string) *API {
	t.Helper()
	a, e := New(Config{Path: filepath.Join(t.TempDir(), "queue.db"), URL: dbURL, AdminToken: strings.Repeat("a", 32), ReadToken: strings.Repeat("r", 32), IngestToken: strings.Repeat("i", 32)})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Store.Close() })
	return a
}
func call(a *API, method, path, token, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func TestAPIAuthOfflineAndSnapshotRecovery(t *testing.T) {
	a := testAPI(t, "http://127.0.0.1:1")
	admin := strings.Repeat("a", 32)
	ingest := strings.Repeat("i", 32)
	read := strings.Repeat("r", 32)
	for _, c := range []struct {
		method, path, token string
		status              int
	}{{"POST", "/api/v1/events", "", 401}, {"POST", "/api/v1/events", read, 403}, {"GET", "/api/v1/events", ingest, 403}, {"GET", "/api/v1/batches", read, 403}, {"GET", "/api/v1/backup", read, 403}, {"GET", "/api/v1/status", read, 200}} {
		w := call(a, c.method, c.path, c.token, "k", good)
		if w.Code != c.status {
			t.Fatal(c, w.Code, w.Body.String())
		}
	}
	w := call(a, "POST", "/api/v1/events", ingest, "k", good)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var receipt Batch
	json.Unmarshal(w.Body.Bytes(), &receipt)
	w = call(a, "POST", "/api/v1/events", ingest, "k", good)
	if !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatal(w.Body.String())
	}
	if w := call(a, "POST", "/api/v1/events", ingest, "k", good+" "); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := call(a, "POST", "/api/v1/events", ingest, "", good); w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = call(a, "GET", "/api/v1/backup", admin, "", "")
	path := filepath.Join(t.TempDir(), "restored.db")
	if e := os.WriteFile(path, w.Body.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	restored, e := OpenStore(path, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	b, e := restored.Get(receipt.ID)
	if e != nil || b.Events[0].Raw != good {
		t.Fatal(e, b)
	}
}
func TestWorkerRetryDeadLetterReplay(t *testing.T) {
	var inserts atomic.Int32
	var permanent atomic.Bool
	permanent.Store(true)
	db := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("query"), "INSERT") {
			inserts.Add(1)
			if permanent.Load() {
				http.Error(w, "invalid", 400)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte("harmless log text")) {
				t.Error("raw data lost")
			}
		}
	}))
	defer db.Close()
	a := testAPI(t, db.URL)
	w := call(a, "POST", "/api/v1/events", strings.Repeat("a", 32), "k", good)
	var receipt Batch
	json.Unmarshal(w.Body.Bytes(), &receipt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	await := func(state string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := a.Store.Get(receipt.ID)
			if b.State == state {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("batch never reached %s", state)
	}
	await("dead_letter")
	permanent.Store(false)
	w = call(a, "POST", "/api/v1/batches/"+receipt.ID+"/replay", strings.Repeat("a", 32), "", "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	await("indexed")
	if inserts.Load() != 2 {
		t.Fatal(inserts.Load())
	}
}
func TestQueryParametersAndCursor(t *testing.T) {
	injection := `' OR 1=1 --`
	sql, p, _, c, e := searchSQL(url.Values{"q": {injection}, "field": {"src_endpoint.ip"}, "value": {injection}})
	if e != nil || strings.Contains(sql, injection) || p.Get("q") != injection || c.Snapshot == 0 {
		t.Fatal(sql, p, e)
	}
	for _, p := range []url.Values{{"limit": {"201"}}, {"field": {"x') OR 1=1"}}, {"start": {"2"}, "end": {"1"}}, {"cursor": {"garbage"}}, {"severity_id": {"x"}}} {
		if _, _, _, _, e := searchSQL(p); e == nil {
			t.Fatal(p)
		}
	}
	db := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, good) }))
	defer db.Close()
	ch := ClickHouse{URL: db.URL, Client: db.Client()}
	if _, e := ch.query(context.Background(), "SELECT", nil, nil); e != nil {
		t.Fatal("log content mistaken for SQL error", e)
	}
}

func TestBrowserSessionRolesAndBearerPrecedence(t *testing.T) {
	a := testAPI(t, "http://127.0.0.1:1")
	role := "read"
	a.config.BrowserRole = func(r *http.Request) string { return role }
	for _, tc := range []struct {
		role, method, path, authorization string
		status                            int
	}{
		{"read", "GET", "/api/v1/status", "", 200},
		{"read", "POST", "/api/v1/events", "", 403},
		{"read", "GET", "/api/v1/batches", "", 403},
		{"read", "GET", "/api/v1/backup", "", 403},
		{"admin", "GET", "/api/v1/batches", "", 200},
		{"admin", "GET", "/api/v1/status", "Bearer invalid", 401},
		{"admin", "GET", "/api/v1/batches", "Bearer " + strings.Repeat("r", 32), 403},
		{"", "GET", "/api/v1/status", "", 401},
		{"", "POST", "/api/v1/events", "Bearer " + strings.Repeat("i", 32), 202},
	} {
		role = tc.role
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(good))
		if tc.authorization != "" {
			r.Header.Set("Authorization", tc.authorization)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "browser-roles")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
}
