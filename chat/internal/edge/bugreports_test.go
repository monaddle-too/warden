package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The receiver of docs/bug-reporting-plan.md ("Contract"), against
// docs/bug-report-sample.json.

func sampleReport(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/bug-report-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// bugServer is a Google-mode edge with the receiver on, its clock under
// the test's control and the upstream counting what reaches it.
func bugServer(t *testing.T, cfg BugReportsConfig) (*Server, *fakeAuth, *int, *time.Time) {
	t.Helper()
	count := 0
	s, a := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; w.WriteHeader(204) }))
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "bug-reports")
	}
	cfg.Enabled = true
	s.Logf = func(string, ...any) {}
	bugs, err := newBugReports(cfg, s.Logf)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	bugs.now = func() time.Time { return now }
	s.bugs = bugs
	return s, a, &count, &now
}

func post(s *Server, body []byte, ip string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://warden.example.com/api/bug-reports", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = ip + ":40000"
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func encode(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBugReportIsStoredAndAnsweredWithoutSignIn(t *testing.T) {
	s, _, upstream, now := bugServer(t, BugReportsConfig{})
	doc := sampleReport(t)
	w := post(s, encode(t, doc), "203.0.113.7")
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var answer struct{ ID, Received string }
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.ID != doc["id"] || answer.Received != now.Format(time.RFC3339) {
		t.Fatalf("%s %v", w.Body.String(), err)
	}
	if *upstream != 0 {
		t.Fatal("the bug report reached the chat")
	}
	path := filepath.Join(s.bugs.cfg.Dir, "2026-09-18", answer.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("report is not owner-only: %v", info.Mode())
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["receivedAt"] != now.Format(time.RFC3339) || stored["summary"] != doc["summary"] || stored["kind"] != "error" {
		t.Fatalf("stored: %s", raw)
	}
	source, _ := stored["source"].(string)
	if len(source) != 64 || strings.Contains(string(raw), "203.0.113.7") {
		t.Fatalf("source must be a hash, never the address: %s", raw)
	}
	// The same install later that day hashes to the same source; a
	// different address does not.
	doc["id"] = strings.Repeat("b", 32)
	post(s, encode(t, doc), "203.0.113.7")
	doc["id"] = strings.Repeat("c", 32)
	post(s, encode(t, doc), "203.0.113.8")
	same, _ := os.ReadFile(filepath.Join(s.bugs.cfg.Dir, "2026-09-18", strings.Repeat("b", 32)+".json"))
	other, _ := os.ReadFile(filepath.Join(s.bugs.cfg.Dir, "2026-09-18", strings.Repeat("c", 32)+".json"))
	if !strings.Contains(string(same), source) || strings.Contains(string(other), source) {
		t.Fatal("source hash is not per address per day")
	}
	// Unknown fields survive; the stored file is what arrived plus the
	// receiver's two fields.
	doc["id"] = strings.Repeat("d", 32)
	doc["extra"] = map[string]any{"future": true}
	post(s, encode(t, doc), "203.0.113.7")
	kept, _ := os.ReadFile(filepath.Join(s.bugs.cfg.Dir, "2026-09-18", strings.Repeat("d", 32)+".json"))
	if !strings.Contains(string(kept), `"future": true`) {
		t.Fatalf("unknown field dropped: %s", kept)
	}
}

func TestBugReportIdempotentOnIDAndServerAssignedOtherwise(t *testing.T) {
	s, _, _, now := bugServer(t, BugReportsConfig{})
	doc := sampleReport(t)
	first := post(s, encode(t, doc), "203.0.113.7")
	*now = now.Add(time.Hour)
	doc["summary"] = "changed"
	again := post(s, encode(t, doc), "203.0.113.7")
	if first.Code != 202 || again.Code != 202 || first.Body.String() != again.Body.String() {
		t.Fatalf("resend: %d %s / %d %s", first.Code, first.Body.String(), again.Code, again.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(s.bugs.cfg.Dir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("stored twice: %v", files)
	}
	if raw, _ := os.ReadFile(files[0]); strings.Contains(string(raw), "changed") {
		t.Fatal("resend overwrote the first report")
	}
	for _, id := range []any{"", "not-hex", strings.Repeat("A", 32), strings.Repeat("a", 31), 12, nil} {
		doc["id"] = id
		w := post(s, encode(t, doc), "203.0.113.7")
		var answer struct{ ID string }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || !idPattern.MatchString(answer.ID) || answer.ID == id {
			t.Fatalf("id %v: %d %s", id, w.Code, w.Body.String())
		}
		raw, err := os.ReadFile(filepath.Join(s.bugs.cfg.Dir, "2026-09-18", answer.ID+".json"))
		if err != nil || !strings.Contains(string(raw), `"id": "`+answer.ID+`"`) {
			t.Fatalf("assigned id not stored in the report: %v %s", err, raw)
		}
	}
}

func TestBugReportRejectsMalformedAndOversized(t *testing.T) {
	s, _, _, _ := bugServer(t, BugReportsConfig{})
	good := sampleReport(t)
	cases := []struct {
		name string
		body []byte
		code int
	}{
		{"not json", []byte("{"), 400},
		{"two documents", append(encode(t, good), encode(t, good)...), 400},
		{"an array", []byte("[]"), 400},
	}
	mutate := func(name string, f func(d map[string]any)) {
		d := sampleReport(t)
		f(d)
		cases = append(cases, struct {
			name string
			body []byte
			code int
		}{name, encode(t, d), 400})
	}
	mutate("schema 2", func(d map[string]any) { d["schema"] = 2 })
	mutate("schema string", func(d map[string]any) { d["schema"] = "1" })
	mutate("no schema", func(d map[string]any) { delete(d, "schema") })
	mutate("unknown kind", func(d map[string]any) { d["kind"] = "crash" })
	mutate("no kind", func(d map[string]any) { delete(d, "kind") })
	mutate("unknown component", func(d map[string]any) { d["component"] = "browser" })
	mutate("unknown trigger", func(d map[string]any) { d["trigger"] = "cron" })
	mutate("summary too long", func(d map[string]any) { d["summary"] = strings.Repeat("x", 201) })
	mutate("summary not a string", func(d map[string]any) { d["summary"] = 5 })
	mutate("bad createdAt", func(d map[string]any) { d["createdAt"] = "yesterday" })
	mutate("error not an object", func(d map[string]any) { d["error"] = "boom" })
	mutate("stack not a string", func(d map[string]any) { d["error"] = map[string]any{"stack": []int{1}} })
	mutate("logs not an array", func(d map[string]any) { d["logs"] = "x" })
	mutate("log without name", func(d map[string]any) { d["logs"] = []any{map[string]any{"lines": []string{"a"}}} })
	mutate("log lines not strings", func(d map[string]any) { d["logs"] = []any{map[string]any{"name": "a.log", "lines": []int{1}}} })
	for _, tc := range cases {
		if w := post(s, tc.body, "203.0.113.7"); w.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	// The summary bound is bytes of the contract's 200; exactly 200 passes.
	d := sampleReport(t)
	d["summary"] = strings.Repeat("y", 200)
	if w := post(s, encode(t, d), "203.0.113.7"); w.Code != 202 {
		t.Fatalf("200-byte summary: %d %s", w.Code, w.Body.String())
	}
	// Over 256 KiB: refused by the declared length and, without one, by
	// the reader.
	big := sampleReport(t)
	big["description"] = strings.Repeat("z", 256<<10)
	if w := post(s, encode(t, big), "203.0.113.7"); w.Code != 413 {
		t.Fatalf("oversized: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "https://warden.example.com/api/bug-reports", strings.NewReader(string(encode(t, big))))
	r.ContentLength = -1
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversized without length: %d", w.Code)
	}
	if w := invoke(s, "https://warden.example.com/api/bug-reports"); w.Code != 405 {
		t.Fatalf("GET: %d", w.Code)
	}
	files, _ := filepath.Glob(filepath.Join(s.bugs.cfg.Dir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("a refused report was stored: %v", files)
	}
}

func TestBugReportsDisabledAnswer404(t *testing.T) {
	count := 0
	s, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; w.WriteHeader(204) }))
	if w := post(s, encode(t, sampleReport(t)), "203.0.113.7"); w.Code != 404 {
		t.Fatalf("disabled receiver: %d", w.Code)
	}
	for _, path := range []string{"/api/admin/bug-reports", "/api/admin/bug-reports/" + strings.Repeat("a", 32)} {
		r := httptest.NewRequest("GET", "https://warden.example.com"+path, nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s disabled: %d", path, w.Code)
		}
	}
	if count != 0 {
		t.Fatal("a bug-report route reached the chat")
	}
}

func TestBugReportLimitsPerSourceAndPerDay(t *testing.T) {
	s, _, _, now := bugServer(t, BugReportsConfig{MaxPerHour: 3, MaxPerDay: 5})
	doc := sampleReport(t)
	send := func(ip string, headers ...string) *httptest.ResponseRecorder {
		doc["id"] = random()[:32]
		return post(s, encode(t, doc), ip, headers...)
	}
	for i := 0; i < 3; i++ {
		if w := send("203.0.113.7"); w.Code != 202 {
			t.Fatalf("report %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := send("203.0.113.7")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth in the hour: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if retry := w.Header().Get("Retry-After"); retry != "3601" {
		t.Fatalf("Retry-After %s, want the hour since the first", retry)
	}
	// A resend of a stored id is answered even over the limit.
	doc["id"] = strings.Repeat("e", 32)
	*now = now.Add(time.Minute)
	first := post(s, encode(t, doc), "203.0.113.9")
	*now = now.Add(time.Minute)
	if again := post(s, encode(t, doc), "203.0.113.7"); first.Code != 202 || again.Code != 202 {
		t.Fatalf("resend over the limit: %d %d", first.Code, again.Code)
	}
	// Another source has its own hour; the day is shared: five stored so
	// far is the cap.
	if w := send("203.0.113.8"); w.Code != 202 {
		t.Fatalf("other source: %d", w.Code)
	}
	if w := send("203.0.113.8"); w.Code != 429 {
		t.Fatalf("sixth of the day: %d", w.Code)
	}
	// Behind the ingress the peer is private and X-Forwarded-For names
	// the source; a public peer's header is ignored.
	*now = now.Add(2 * time.Hour)
	s.bugs.cfg.MaxPerDay = 100
	for i := 0; i < 3; i++ {
		if w := send("10.0.0.5", "X-Forwarded-For", "198.51.100.1"); w.Code != 202 {
			t.Fatalf("forwarded %d: %d", i, w.Code)
		}
	}
	if w := send("10.0.0.5", "X-Forwarded-For", "198.51.100.1"); w.Code != 429 {
		t.Fatalf("forwarded source over the limit: %d", w.Code)
	}
	if w := send("10.0.0.5", "X-Forwarded-For", "198.51.100.2"); w.Code != 202 {
		t.Fatalf("other forwarded source: %d", w.Code)
	}
	if w := send("10.0.0.5", "X-Forwarded-For", "203.0.113.1, 198.51.100.1"); w.Code != 429 {
		t.Fatalf("the proxy's entry is the last: %d", w.Code)
	}
	if w := send("203.0.113.50", "X-Forwarded-For", "198.51.100.1"); w.Code != 202 {
		t.Fatalf("a public peer's header must be ignored: %d", w.Code)
	}
	// The hour passes.
	*now = now.Add(time.Hour)
	if w := send("198.51.100.1"); w.Code != 202 {
		t.Fatalf("after the hour: %d %s", w.Code, w.Body.String())
	}
}

func TestBugReportRetentionAndCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bug-reports")
	old := filepath.Join(dir, "2026-06-01")
	os.MkdirAll(old, 0700)
	os.WriteFile(filepath.Join(old, strings.Repeat("0", 32)+".json"), []byte("{}"), 0600)
	edge := filepath.Join(dir, "2026-06-20")
	os.MkdirAll(edge, 0700)
	boundary := filepath.Join(edge, strings.Repeat("1", 32)+".json")
	os.WriteFile(boundary, []byte("{}"), 0600)
	// The file's time is its order after a restart (the receiver sets it
	// to receivedAt when it writes).
	os.Chtimes(boundary, time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC), time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC))
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("kept"), 0600)
	// At construction the directories beyond retention go (90 days
	// before 2026-09-18 is 2026-06-20, which stays).
	s, _, _, now := bugServer(t, BugReportsConfig{Dir: dir})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired day directory kept")
	}
	if _, err := os.Stat(boundary); err != nil {
		t.Fatal("report on the retention boundary removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("a file that is not a report was removed")
	}
	// The cap: the oldest go first, swept on write.
	s.bugs.maxFiles = 4
	doc := sampleReport(t)
	var ids []string
	for i := 0; i < 5; i++ {
		*now = now.Add(time.Minute)
		doc["id"] = fmt.Sprintf("%032d", i+2)
		ids = append(ids, doc["id"].(string))
		if w := post(s, encode(t, doc), "203.0.113.7"); w.Code != 202 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if len(files) != 4 {
		t.Fatalf("cap not applied: %v", files)
	}
	for _, gone := range []string{strings.Repeat("1", 32), ids[0]} {
		if s.bugs.byID[gone].ID != "" {
			t.Fatalf("%s should be the oldest and gone", gone)
		}
	}
	// A restart rebuilds the index from the files in the same order.
	reloaded, err := newBugReports(s.bugs.cfg, s.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.entries) != 4 || reloaded.entries[0].ID != ids[4] || reloaded.entries[3].ID != ids[1] {
		t.Fatalf("reloaded order: %+v", reloaded.entries)
	}
	// Retention moves on: a write 91 days later removes the boundary
	// day and everything from today.
	*now = now.AddDate(0, 0, 91)
	doc["id"] = strings.Repeat("f", 32)
	post(s, encode(t, doc), "203.0.113.7")
	files, _ = filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if len(files) != 1 || !strings.Contains(files[0], strings.Repeat("f", 32)) {
		t.Fatalf("retention after 91 days: %v", files)
	}
}

func TestBugReportAdminRoutesAreOwnerOnly(t *testing.T) {
	s, a, upstream, now := bugServer(t, BugReportsConfig{})
	doc := sampleReport(t)
	var ids []string
	for i := 0; i < 3; i++ {
		*now = now.Add(time.Minute)
		doc["id"] = fmt.Sprintf("%032d", i)
		doc["summary"] = fmt.Sprintf("report %d", i)
		ids = append(ids, doc["id"].(string))
		post(s, encode(t, doc), "203.0.113.7")
	}
	call := func(auth Authenticator, method, path string, signedIn bool) *httptest.ResponseRecorder {
		s.Auth = auth
		r := httptest.NewRequest(method, "https://warden.example.com"+path, nil)
		if signedIn {
			r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	before := *upstream
	for _, tc := range []struct {
		auth     Authenticator
		method   string
		path     string
		signedIn bool
		code     int
	}{
		{a, "GET", "/api/admin/bug-reports", false, 403},
		{demoAuth{a}, "GET", "/api/admin/bug-reports", true, 403},
		{demoAuth{a}, "GET", "/api/admin/bug-reports/" + ids[0], true, 403},
		{demoAuth{a}, "DELETE", "/api/admin/bug-reports/" + ids[0], true, 403},
		{a, "POST", "/api/admin/bug-reports", true, 404},
		{a, "GET", "/api/admin/bug-reports/nope", true, 404},
		{a, "GET", "/api/admin/bug-reports/" + strings.Repeat("9", 32), true, 404},
		{a, "GET", "/api/admin/bug-reportsx", true, 404},
		{a, "GET", "/api/admin/bug-reports?limit=0", true, 400},
		{a, "GET", "/api/admin/bug-reports?before=" + strings.Repeat("9", 32), true, 400},
	} {
		if w := call(tc.auth, tc.method, tc.path, tc.signedIn); w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if *upstream != before {
		t.Fatal("an admin bug-report route reached the chat")
	}
	// The list: newest first, the summary fields, paged by before.
	w := call(a, "GET", "/api/admin/bug-reports?limit=2", true)
	var rows []bugSummary
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(rows) != 2 || rows[0].ID != ids[2] || rows[1].ID != ids[1] || rows[0].Summary != "report 2" || rows[0].Kind != "error" || rows[0].Component != "chat" || rows[0].Version != "v0.1.0-alpha.12-210-g81d0bfc" || rows[0].OS != "darwin" || rows[0].Arch != "arm64" || rows[0].ReceivedAt == "" {
		t.Fatalf("list: %+v", rows)
	}
	w = call(a, "GET", "/api/admin/bug-reports?limit=2&before="+ids[1], true)
	rows = nil
	if json.Unmarshal(w.Body.Bytes(), &rows); len(rows) != 1 || rows[0].ID != ids[0] {
		t.Fatalf("page 2: %+v", rows)
	}
	w = call(a, "GET", "/api/admin/bug-reports?before="+ids[0], true)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("last page: %d %s", w.Code, w.Body.String())
	}
	// One report is the stored file.
	w = call(a, "GET", "/api/admin/bug-reports/"+ids[0], true)
	var stored map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stored) != nil || stored["summary"] != "report 0" || stored["receivedAt"] == nil || stored["source"] == nil {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if logs, ok := stored["logs"].([]any); !ok || len(logs) != 2 {
		t.Fatalf("logs missing from the stored report: %v", stored["logs"])
	}
	// Delete removes the file and the row; a second delete is 404.
	if w = call(a, "DELETE", "/api/admin/bug-reports/"+ids[0], true); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w = call(a, "DELETE", "/api/admin/bug-reports/"+ids[0], true); w.Code != 404 {
		t.Fatalf("second delete: %d", w.Code)
	}
	files, _ := filepath.Glob(filepath.Join(s.bugs.cfg.Dir, "*", "*.json"))
	w = call(a, "GET", "/api/admin/bug-reports", true)
	rows = nil
	if json.Unmarshal(w.Body.Bytes(), &rows); len(rows) != 2 || len(files) != 2 {
		t.Fatalf("after delete: %+v %v", rows, files)
	}
	// A resend of the deleted id is a new report.
	doc["id"] = ids[0]
	if w := post(s, encode(t, doc), "203.0.113.7"); w.Code != 202 {
		t.Fatalf("resend after delete: %d", w.Code)
	}
}

func TestBugReportsConfigFromWardenJSON(t *testing.T) {
	// A loopback edge with the receiver on: the plan's local shape when
	// enabled by hand, storing under <state>/edge/bug-reports.
	dir := t.TempDir()
	path := filepath.Join(dir, "owner")
	os.WriteFile(path, []byte(strings.Repeat("s", 64)), 0600)
	c := Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: "http://127.0.0.1:18780", UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: path, Listen: "127.0.0.1:18781"}
	c.BugReports = &BugReportsConfig{Enabled: true, Dir: "relative"}
	if _, err := New(c); err == nil {
		t.Fatal("relative directory accepted")
	}
	c.BugReports = &BugReportsConfig{Enabled: true, Dir: filepath.Join(dir, "edge", "bug-reports"), RetentionDays: 7, MaxPerHour: 2, MaxPerDay: 3}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.bugs == nil || s.bugs.cfg.RetentionDays != 7 || s.bugs.cfg.MaxPerHour != 2 || s.bugs.cfg.MaxPerDay != 3 {
		t.Fatalf("receiver: %+v", s.bugs)
	}
	if info, err := os.Stat(c.BugReports.Dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory: %v %v", err, info)
	}
	c.BugReports = &BugReportsConfig{Enabled: false, Dir: filepath.Join(dir, "nope")}
	if s, err = New(c); err != nil || s.bugs != nil {
		t.Fatalf("disabled: %v %v", err, s.bugs)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(err) {
		t.Fatal("a disabled receiver created its directory")
	}
}
