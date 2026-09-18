package bugreport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/config"
)

// The redaction is the part to be thorough about: a table of strings that
// must not survive into a draft, and the ids that must.
func TestRedactRemovesTokensSecretsEmailsAndHome(t *testing.T) {
	cap := strings.Repeat("ab12", 16) // an owner capability: 64 hex
	r := Redactor{Secrets: []string{cap, "warden-codex-login", "short"}, Home: "/Users/someone"}
	cases := []struct{ in, want string }{
		{"Authorization: Bearer " + cap, "Authorization: <token>"},
		{"authorization=Bearer abcdefghijklmnop", "authorization=<token>"},
		{`"Authorization": "Basic dXNlcjpwYXNz"`, `"Authorization": "<token>"`},
		{"GET /api/state Bearer 0123456789abcdef and on", "GET /api/state Bearer <token> and on"},
		{"http://127.0.0.1:18780/?launch=1#session=" + cap, "http://127.0.0.1:18780/?launch=1#session=<token>"},
		{"?token=abc123&x=1", "?token=<token>&x=1"},
		{"capability=deadbeefdeadbeef", "capability=<token>"},
		{`{"token":"abc","url":"x"}`, `{"token":"<token>","url":"x"}`},
		{"access_token=ya29.a0AfH6SMBxyz-abc_def", "access_token=<token>"},
		{"key sk-ant-oat01-" + strings.Repeat("a", 40) + " used", "key <secret> used"},
		{"sk-proj-abcdefghijklmnop", "<secret>"},
		{"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "<secret>"},
		{"gho_ABCDEFGHIJKLMNOP", "<secret>"},
		{"github_pat_11ABCDEFG_abcdefghijklmnop", "<secret>"},
		{"ya29.a0AfH6SMBxyzabc-def_ghi", "<secret>"},
		{"owner someone@example.com wrote", "owner <email> wrote"},
		{"x-warden-email: first.last+tag@sub.example.co.uk", "x-warden-email: <email>"},
		{"/Users/someone/.warden/warden-chat.log", "~/.warden/warden-chat.log"},
		{"state /Users/someone and /Users/someone/x", "state ~ and ~/x"},
		{"secret value warden-codex-login in a log", "secret value <secret> in a log"},
		{"the capability " + cap + " appears bare", "the capability <secret> appears bare"},
		// Ids survive: 32 hex is a chat, run or report id.
		{"chat 0123456789abcdef0123456789abcdef run fedcba9876543210fedcba9876543210", "chat 0123456789abcdef0123456789abcdef run fedcba9876543210fedcba9876543210"},
		// A short secret is not replaced (it would match ordinary text).
		{"a short word", "a short word"},
		// Ordinary text is untouched.
		{"chat abc: the owner ran \"ls\" in the workspace: completed", "chat abc: the owner ran \"ls\" in the workspace: completed"},
		{"", ""},
	}
	for _, c := range cases {
		if got := r.Redact(c.in); got != c.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	// Nothing sensitive survives anywhere in a whole report.
	rep := Report{Summary: "failed for someone@example.com", Description: "token=" + cap, Error: &Error{Message: "Bearer " + cap, Stack: "goroutine 1\n/Users/someone/x.go:1", Operation: "ghp_ABCDEFGHIJKLMNOP"}, Logs: []Log{{Name: "warden-chat.log", Lines: []string{"Authorization: Bearer x", cap, "fine"}}}, Context: &Context{ChatID: "0123456789abcdef0123456789abcdef", Model: "sk-model-abcdefghij"}}
	r.Report(&rep)
	b, _ := json.Marshal(rep)
	for _, bad := range []string{cap, "someone@example.com", "/Users/someone", "ghp_", "Bearer x"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q survived in %s", bad, b)
		}
	}
	if rep.Context.ChatID != "0123456789abcdef0123456789abcdef" || rep.Logs[0].Lines[2] != "fine" {
		t.Errorf("ids or plain lines changed: %+v", rep)
	}
}

func TestNewRedactorCollectsConfigSecretsAndTheOwnerCapability(t *testing.T) {
	state := t.TempDir()
	cfg := config.Defaults(state)
	cfg.Providers.Codex.Secret = "warden-codex-login"
	cfg.Providers.GitHub.Secret = "warden-github-login"
	cfg.Providers.GitHub.AuthFile = ""
	os.MkdirAll(cfg.AppState(), 0o700)
	os.WriteFile(cfg.OwnerTokenFile(), []byte(`{"url":"http://127.0.0.1:1","token":"cafebabecafebabecafebabecafebabe"}`), 0o600)
	r := NewRedactor(cfg, "extra-in-memory-secret")
	got := r.Redact("warden-codex-login warden-github-login cafebabecafebabecafebabecafebabe extra-in-memory-secret")
	if got != "<secret> <secret> <secret> <secret>" {
		t.Fatalf("%q", got)
	}
}

func TestTailKeepsTheLastLinesWithinTheByteBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warden-chat.log")
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString(strings.Repeat("x", 100))
		b.WriteString("\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	l := Tail(path)
	if l.Name != "warden-chat.log" || len(l.Lines) != MaxLogLines || len(l.Lines[0]) != 100 {
		t.Fatalf("%s: %d lines, first %d bytes", l.Name, len(l.Lines), len(l.Lines[0]))
	}
	// Long lines: the byte bound wins and the cut line is dropped.
	b.Reset()
	for i := 0; i < 20; i++ {
		b.WriteString(strings.Repeat("y", 10000))
		b.WriteString("\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	l = Tail(path)
	total := 0
	for _, line := range l.Lines {
		total += len(line) + 1
		if len(line) != 10000 {
			t.Fatalf("a cut line survived: %d bytes", len(line))
		}
	}
	if total > MaxLogBytes || len(l.Lines) == 0 {
		t.Fatalf("%d lines, %d bytes", len(l.Lines), total)
	}
	if l = Tail(filepath.Join(dir, "missing.log")); len(l.Lines) != 1 || !strings.Contains(l.Lines[0], "no such file") {
		t.Fatalf("%+v", l)
	}
	if got := TailText("a\r\nb\nc\n"); strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("%q", got)
	}
}

func testCapturer(t *testing.T, enabled bool) (*Capturer, string) {
	t.Helper()
	state := t.TempDir()
	cfg := config.Defaults(state)
	cfg.Reporting.Enabled = enabled
	cfg.Reporting.URL = "http://127.0.0.1:1/api/bug-reports"
	os.WriteFile(filepath.Join(state, "install.json"), []byte(`{"layout":1,"warden":"v0.0.0-dev.abc","codex":"0.154.0","claude":"2.1.272","guestArch":"arm64"}`), 0o600)
	c := New(cfg, "", ComponentChat, "owner-capability-value")
	c.Now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	c.Logf = func(string, ...any) {}
	return c, state
}

func TestCaptureWritesARedactedDraftOnlyWhenEnabled(t *testing.T) {
	off, state := testCapturer(t, false)
	r := off.Draft(KindError, TriggerPanic, "boom")
	if path, written, err := off.Capture(r); err != nil || written || path != "" {
		t.Fatalf("disabled capture: %q %v %v", path, written, err)
	}
	if _, err := os.Stat(PendingDir(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled reporting created the pending directory")
	}
	on, state := testCapturer(t, true)
	os.WriteFile(filepath.Join(state, "warden-chat.log"), []byte("started\nowner owner-capability-value asked someone@example.com\n"), 0o600)
	r = on.Draft(KindError, TriggerPanic, "panic in POST /api/x: boom\nsecond line")
	r.Error = &Error{Message: "boom", Stack: "goroutine 7 [running]:\nmain.main()", Operation: "POST /api/x"}
	on.AddServiceLog(&r, "warden-chat")
	on.AddServiceLog(&r, "warden") // absent: skipped
	path, written, err := on.Capture(r)
	if err != nil || !written {
		t.Fatal(written, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != 1 || !ValidID(got.ID) || got.Kind != KindError || got.Trigger != TriggerPanic || got.Component != ComponentChat || got.CreatedAt != "2026-09-18T12:00:00Z" || got.Summary != "panic in POST /api/x: boom" {
		t.Fatalf("%+v", got)
	}
	if got.Warden.Protocol != 2 || got.Warden.Runtime != "sbx" || got.Warden.Installed != (Installed{Codex: "0.154.0", Claude: "2.1.272", GuestArch: "arm64"}) || got.Warden.Version == "" {
		t.Fatalf("%+v", got.Warden)
	}
	if got.System.OS == "" || got.System.Arch == "" || got.System.CPUs < 1 {
		t.Fatalf("%+v", got.System)
	}
	if len(got.Logs) != 1 || got.Logs[0].Name != "warden-chat.log" || strings.Join(got.Logs[0].Lines, "|") != "started|owner <secret> asked <email>" {
		t.Fatalf("%+v", got.Logs)
	}
	drafts, err := Pending(state)
	if err != nil || len(drafts) != 1 || drafts[0].Path != path {
		t.Fatalf("%+v %v", drafts, err)
	}
	if err := Discard(path); err != nil {
		t.Fatal(err)
	}
	if drafts, _ = Pending(state); len(drafts) != 0 {
		t.Fatal("still pending")
	}
	if err := Discard(path); err != nil {
		t.Fatal("a second discard is not an error:", err)
	}
}

// The reporting section is re-read from warden.json, so `warden bugs on`
// reaches a running service.
func TestCapturerReadsTheSettingAgainFromTheConfigFile(t *testing.T) {
	// A short path: config validation bounds the state's socket paths.
	state, err := os.MkdirTemp("/tmp", "wb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	cfg := config.Defaults(state)
	path := filepath.Join(state, "warden.json")
	if err := config.Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	c := New(cfg, path, ComponentRunner)
	c.Logf = func(string, ...any) {}
	if c.Enabled() {
		t.Fatal("enabled without opt-in")
	}
	cfg.Reporting.Enabled = true
	if err := config.Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	if !c.Enabled() {
		t.Fatal("the file's change was not seen")
	}
	if _, written, _ := c.Capture(c.Draft(KindUser, TriggerUser, "hello")); !written {
		t.Fatal("not written")
	}
	// An unreadable file falls back to the construction-time value.
	os.Remove(path)
	if c.Enabled() {
		t.Fatal("fallback should be the construction-time false")
	}
}

func TestRecoverCapturesAndRaisesAgain(t *testing.T) {
	c, state := testCapturer(t, true)
	caught := func() (v any) {
		defer func() { v = recover() }()
		func() {
			defer c.Recover("worker op prepare")
			panic("prepare exploded")
		}()
		return nil
	}()
	if caught != "prepare exploded" {
		t.Fatalf("the panic was swallowed: %v", caught)
	}
	drafts, _ := Pending(state)
	if len(drafts) != 1 || drafts[0].Report.Trigger != TriggerPanic || drafts[0].Report.Error == nil || drafts[0].Report.Error.Message != "prepare exploded" || drafts[0].Report.Error.Operation != "worker op prepare" || !strings.Contains(drafts[0].Report.Error.Stack, "bugreport") {
		t.Fatalf("%+v", drafts)
	}
	// Trap keeps the deliberate test panic in the process.
	func() {
		defer c.Trap(TriggerTest, "bug-test")
		panic("test exception from /test bugreporting")
	}()
	drafts, _ = Pending(state)
	if len(drafts) != 2 {
		t.Fatalf("%d drafts", len(drafts))
	}
	var test *Report
	for i := range drafts {
		if drafts[i].Report.Trigger == TriggerTest {
			test = &drafts[i].Report
		}
	}
	if test == nil || test.Summary != "panic in bug-test: test exception from /test bugreporting" {
		t.Fatalf("%+v", drafts)
	}
	// A nil capturer guards nothing and changes nothing.
	var none *Capturer
	caught = func() (v any) {
		defer func() { v = recover() }()
		func() {
			defer none.Recover("x")
			panic("still raised")
		}()
		return nil
	}()
	if caught != "still raised" {
		t.Fatal(caught)
	}
	// Recent sees the panic draft the launcher would otherwise duplicate.
	if !Recent(state, ComponentChat, TriggerPanic, c.now().Add(5*time.Second), 30*time.Second) || Recent(state, ComponentRunner, TriggerPanic, c.now(), 30*time.Second) || Recent(state, ComponentChat, TriggerPanic, c.now().Add(time.Hour), 30*time.Second) {
		t.Fatal("Recent")
	}
}

func TestHandlerCapturesAPanicAndLetsNetHTTPLogIt(t *testing.T) {
	c, state := testCapturer(t, true)
	SetDefault(c)
	t.Cleanup(func() { SetDefault(nil) })
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/boom" {
			panic("handler exploded")
		}
		w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	if res, err := http.Get(srv.URL + "/api/fine"); err != nil || res.StatusCode != 200 {
		t.Fatal(res, err)
	}
	// net/http recovers the re-raised panic and closes the connection.
	if _, err := http.Get(srv.URL + "/api/boom"); err == nil {
		t.Fatal("the panic did not abort the connection")
	}
	drafts, _ := Pending(state)
	if len(drafts) != 1 || drafts[0].Report.Error.Operation != "GET /api/boom" {
		t.Fatalf("%+v", drafts)
	}
}

func TestSendMapsTheReceiversAnswers(t *testing.T) {
	var got Report
	status := 202
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s", r.Method, r.Header.Get("Content-Type"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		switch status {
		case 429:
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(status)
		switch status {
		case 202:
			w.Write([]byte(`{"id":"` + got.ID + `","received":"2026-09-18T12:00:00Z"}`))
		case 400:
			w.Write([]byte("unknown schema"))
		}
	}))
	defer srv.Close()
	r := Report{Schema: 1, ID: NewID(), Kind: KindUser, Summary: "s"}
	receipt, err := Send(context.Background(), srv.URL, r)
	if err != nil || receipt.ID != r.ID || got.ID != r.ID {
		t.Fatal(receipt, err)
	}
	for _, c := range []struct {
		status int
		want   string
	}{{404, "not accepting bug reports (404)"}, {413, "larger than the receiver accepts"}, {429, "try again after 120"}, {400, "rejected the report (400): unknown schema"}, {500, "answered 500"}} {
		status = c.status
		_, err := Send(context.Background(), srv.URL, r)
		var se *SendError
		if !errors.As(err, &se) || se.Status != c.status || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: %v", c.status, err)
		}
	}
	if _, err := Send(context.Background(), "http://127.0.0.1:1/x", r); err == nil || !strings.Contains(err.Error(), "could not reach the receiver") {
		t.Fatal(err)
	}
	big := r
	big.Description = strings.Repeat("x", MaxBody+1)
	if _, err := Send(context.Background(), srv.URL, big); err == nil || !strings.Contains(err.Error(), "over the receiver's") {
		t.Fatal(err)
	}
}

// receiver is a fake cloud endpoint that records what it was sent.
type receiver struct {
	mu   sync.Mutex
	got  []Report
	srv  *httptest.Server
	fail int // a status to answer with instead of 202
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		json.NewDecoder(r.Body).Decode(&rep)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.fail != 0 {
			w.WriteHeader(rc.fail)
			return
		}
		rc.got = append(rc.got, rep)
		w.WriteHeader(202)
		w.Write([]byte(`{"id":"` + rep.ID + `","received":"now"}`))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

// The review page, driven with an HTTP client: GET renders every section;
// Send posts the edited description to the receiver, deletes the draft and
// shows the id; Don't send deletes the draft.
func TestPresentPageSendAndDiscard(t *testing.T) {
	c, _ := testCapturer(t, true)
	rc := newReceiver(t)
	r := c.Draft(KindError, TriggerServiceExit, "warden-runner exited unexpectedly (exit status 2)")
	r.Error = &Error{Message: "exit status 2", Operation: "warden-runner"}
	r.Logs = []Log{{Name: "warden-runner.log", Lines: []string{"line one", "line two"}}}
	r.Context = &Context{ChatID: "0123456789abcdef0123456789abcdef", Provider: "claude", Model: "opus"}
	path, _, err := c.Capture(r)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan string, 1)
	notified := make(chan string, 1)
	var out strings.Builder
	p := Presenter{URL: rc.srv.URL, Open: func(u string) error { opened <- u; return nil }, Notify: func(title, body string) error { notified <- title + ": " + body; return nil }, Out: &out, Redactor: Redactor{Home: "/Users/someone"}}
	type result struct {
		o   Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, err := p.Present(context.Background(), path)
		done <- result{o, err}
	}()
	pageURL := <-opened
	if !strings.Contains(out.String(), pageURL) || !strings.Contains(<-notified, "bug report to review") {
		t.Fatalf("terminal: %q", out.String())
	}
	u, _ := url.Parse(pageURL)
	if u.Hostname() != "127.0.0.1" || len(strings.Trim(u.Path, "/")) != 32 {
		t.Fatalf("page url %s", pageURL)
	}
	res, err := http.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readAll(res)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("%d %s", res.StatusCode, res.Header)
	}
	for _, want := range []string{"Warden bug report", "warden-runner exited unexpectedly", "error · service-exit · chat", r.ID, "exit status 2", "warden-runner.log (2 lines)", "line one", "0123456789abcdef0123456789abcdef", "claude", "protocol 2", "Codex 0.154.0", "Send report", "Don't send", `name="description"`, `&#34;schema&#34;: 1`, "/send", "/discard"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The wrong token, and the token without the page: not found.
	if res, _ = http.Get("http://" + u.Host + "/" + strings.Repeat("0", 32)); res.StatusCode != 404 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	// A cross-origin post is refused.
	req, _ := http.NewRequest("POST", pageURL+"/discard", nil)
	req.Header.Set("Origin", "http://evil.example")
	if res, _ = http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", res.StatusCode)
	}
	// Send with an edited description: the receiver gets it redacted, the
	// draft is gone, the page shows the id.
	rc.mu.Lock()
	rc.fail = 503
	rc.mu.Unlock()
	res, err = http.PostForm(pageURL+"/send", url.Values{"description": {"it broke for someone@example.com\r\nin /Users/someone/x"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = readAll(res)
	if !strings.Contains(body, "Not sent: the receiver answered 503") || !strings.Contains(body, "Send report") {
		t.Fatalf("failure page: %s", body)
	}
	if kept, _ := Read(path); kept.Description != "it broke for <email>\nin ~/x" {
		t.Fatalf("draft after a failed send: %q", kept.Description)
	}
	rc.mu.Lock()
	rc.fail = 0
	rc.mu.Unlock()
	res, err = http.PostForm(pageURL+"/send", url.Values{"description": {"it broke for someone@example.com\r\nin /Users/someone/x"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = readAll(res)
	if !strings.Contains(body, "Report sent") || !strings.Contains(body, r.ID) {
		t.Fatalf("sent page: %s", body)
	}
	rc.mu.Lock()
	sent := rc.got
	rc.mu.Unlock()
	if len(sent) != 1 || sent[0].ID != r.ID || sent[0].Description != "it broke for <email>\nin ~/x" || sent[0].Logs[0].Lines[1] != "line two" {
		t.Fatalf("%+v", sent)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the draft survived the send")
	}
	select {
	case got := <-done:
		if got.err != nil || !got.o.Sent || got.o.ID != r.ID || got.o.URL != pageURL {
			t.Fatalf("%+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Present did not return after the decision")
	}
	// Don't send: the draft is deleted and nothing reaches the receiver.
	path, _, _ = c.Capture(c.Draft(KindUser, TriggerUser, "user text"))
	go func() {
		o, err := p.Present(context.Background(), path)
		done <- result{o, err}
	}()
	pageURL = <-opened
	<-notified
	res, err = http.Post(pageURL+"/discard", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = readAll(res)
	if !strings.Contains(body, "Report discarded") {
		t.Fatalf("%s", body)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the draft survived the discard")
	}
	got := <-done
	if got.err != nil || !got.o.Discarded || got.o.Sent {
		t.Fatalf("%+v", got)
	}
	rc.mu.Lock()
	if len(rc.got) != 1 {
		t.Fatal("the discarded draft was sent")
	}
	rc.mu.Unlock()
	// A user report needs its description.
	path, _, _ = c.Capture(c.Draft(KindUser, TriggerUser, "user text"))
	go func() {
		o, err := p.Present(context.Background(), path)
		done <- result{o, err}
	}()
	pageURL = <-opened
	<-notified
	res, _ = http.PostForm(pageURL+"/send", url.Values{"description": {"  "}})
	body, _ = readAll(res)
	if !strings.Contains(body, "the description is empty") {
		t.Fatalf("%s", body)
	}
	http.Post(pageURL+"/discard", "", nil)
	<-done
}

func TestPresentTimesOutAndKeepsTheDraft(t *testing.T) {
	c, _ := testCapturer(t, true)
	path, _, _ := c.Capture(c.Draft(KindError, TriggerPanic, "x"))
	p := Presenter{URL: "http://127.0.0.1:1", Timeout: 50 * time.Millisecond}
	o, err := p.Present(context.Background(), path)
	if err != nil || !o.TimedOut || o.Sent || o.Discarded {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the draft was lost")
	}
	if _, err := http.Get(o.URL); err == nil {
		t.Fatal("the page is still served after the timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if o, err = p.Present(ctx, path); err != nil || !o.Interrupted {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := (Presenter{}).Present(context.Background(), filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("a missing draft is an error")
	}
	timed := Outcome{TimedOut: true}
	if !strings.Contains(timed.String(), "stays pending") {
		t.Fatal(timed.String())
	}
}

func readAll(res *http.Response) (string, error) {
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}
