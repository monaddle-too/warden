package bugreport

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("page").Parse(pageHTML))

// DefaultTimeout is how long the review page stays up without a decision;
// the draft stays pending after it.
const DefaultTimeout = 15 * time.Minute

// Presenter shows a draft on the standalone review page: a loopback
// listener on a random port, the page under a random token path, "Send
// report" and "Don't send". The presenting process is the installer, the
// launcher or `warden bugs`; the page depends on nothing but this binary.
type Presenter struct {
	// URL is the receiver reports are sent to (config reporting.url).
	URL string
	// Open opens the page in the browser; nil only prints the URL.
	Open func(url string) error
	// Notify announces the page (the detached launcher); nil is silent.
	Notify func(title, body string) error
	// Out is where the URL is printed (the installer's terminal); nil is
	// quiet.
	Out io.Writer
	// Timeout closes the page without a decision; 0 is DefaultTimeout.
	Timeout time.Duration
	// Redactor rewrites the description the person edits on the page.
	Redactor Redactor
	// Send is the send; nil is Send.
	Send func(ctx context.Context, url string, r Report) (Receipt, error)
	// Listen is the listener address; "" is 127.0.0.1:0.
	Listen string
}

// Outcome is what the person decided.
type Outcome struct {
	// Sent with the receiver's id, or Discarded, or TimedOut (the draft
	// stays pending) or Interrupted (the context ended; the draft stays).
	Sent        bool
	ID          string
	Discarded   bool
	TimedOut    bool
	Interrupted bool
	// URL is where the page was.
	URL string
}

// String is the one-line result for a terminal.
func (o Outcome) String() string {
	switch {
	case o.Sent:
		return "bug report sent; its id is " + o.ID
	case o.Discarded:
		return "bug report discarded"
	case o.TimedOut:
		return "no decision; the draft stays pending (`warden bugs pending` re-offers it)"
	case o.Interrupted:
		return "interrupted; the draft stays pending (`warden bugs pending` re-offers it)"
	}
	return "no decision"
}

type pageData struct {
	Mode        string // review, sent, discarded, failed
	Report      Report
	JSON        string
	Token       string
	Description string
	Result      string // the id when sent, the error when failed
}

// Present serves the draft at path until the person sends or discards it,
// the timeout passes or ctx ends. The URL is printed, announced and opened
// as the presenter is configured.
func (p Presenter) Present(ctx context.Context, path string) (Outcome, error) {
	report, err := Read(path)
	if err != nil {
		return Outcome{}, err
	}
	addr := p.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return Outcome{}, err
	}
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(tokenBytes); err != nil {
		l.Close()
		return Outcome{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	url := "http://" + l.Addr().String() + "/" + token
	s := &session{p: p, path: path, report: report, token: token, decided: make(chan struct{})}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = server.Serve(l) }()
	if p.Out != nil {
		fmt.Fprintf(p.Out, "Bug report %s (%s): review and send or discard it at\n  %s\n", report.ID, report.Summary, url)
	}
	if p.Notify != nil {
		_ = p.Notify("Warden: bug report to review", report.Summary)
	}
	if p.Open != nil {
		if err := p.Open(url); err != nil && p.Out != nil {
			fmt.Fprintf(p.Out, "could not open the browser (%v); open the URL above yourself\n", err)
		}
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	outcome := Outcome{URL: url}
	select {
	case <-s.decided:
		s.mu.Lock()
		outcome.Sent, outcome.ID, outcome.Discarded = s.sent, s.sentID, s.discarded
		s.mu.Unlock()
	case <-time.After(timeout):
		outcome.TimedOut = true
	case <-ctx.Done():
		outcome.Interrupted = true
	}
	// The decision's own response is still being written; let it finish.
	shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	_ = server.Shutdown(shutdown)
	return outcome, nil
}

// session is one presented draft.
type session struct {
	p      Presenter
	path   string
	token  string
	mu     sync.Mutex
	report Report
	// sent/discarded is the decision; decided closes once.
	sent      bool
	sentID    string
	discarded bool
	decided   chan struct{}
	once      sync.Once
}

func (s *session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+s.token)
	if !ok || (rest != "" && rest != "/send" && rest != "/discard") {
		http.NotFound(w, r)
		return
	}
	// A page from another origin cannot post here: the browser sends the
	// origin of a cross-site form, and the page's own forms send none or
	// the page's own.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && origin != "http://"+r.Host {
		http.Error(w, "untrusted origin", http.StatusForbidden)
		return
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.render(w, "review", "", http.StatusOK)
	case rest == "/send" && r.Method == http.MethodPost:
		s.send(w, r)
	case rest == "/discard" && r.Method == http.MethodPost:
		s.discard(w)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *session) render(w http.ResponseWriter, mode, result string, status int) {
	s.mu.Lock()
	report := s.report
	s.mu.Unlock()
	b, _ := json.MarshalIndent(report, "", "  ")
	data := pageData{Mode: mode, Report: report, JSON: string(b), Token: s.token, Description: report.Description, Result: result}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.Execute(w, data); err != nil {
		fmt.Fprintf(w, "<!-- %s -->", template.HTMLEscapeString(err.Error()))
	}
}

func (s *session) send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.sent || s.discarded {
		s.mu.Unlock()
		s.render(w, "sent", s.sentID, http.StatusOK)
		return
	}
	// The description as edited, redacted like everything else and kept
	// in the draft, so a retry or a later `warden bugs pending` has it.
	if desc, ok := r.PostForm["description"]; ok {
		s.report.Description = Clip(s.p.Redactor.Redact(strings.ReplaceAll(strings.TrimSpace(desc[0]), "\r\n", "\n")), MaxDescription)
		if s.report.Kind == KindUser && s.report.Description == "" {
			s.mu.Unlock()
			s.render(w, "failed", "the description is empty; say what went wrong, or don't send", http.StatusOK)
			return
		}
		_ = Write(s.path, s.report)
	}
	report := s.report
	s.mu.Unlock()
	send := s.p.Send
	if send == nil {
		send = Send
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	receipt, err := send(ctx, s.p.URL, report)
	if err != nil {
		s.render(w, "failed", err.Error(), http.StatusOK)
		return
	}
	s.mu.Lock()
	s.sent, s.sentID = true, receipt.ID
	s.mu.Unlock()
	if err := Discard(s.path); err != nil && !errors.Is(err, context.Canceled) {
		// Sent all the same; the draft would be offered again, which the
		// receiver's idempotent id makes harmless.
		_ = err
	}
	s.render(w, "sent", receipt.ID, http.StatusOK)
	s.once.Do(func() { close(s.decided) })
}

func (s *session) discard(w http.ResponseWriter) {
	s.mu.Lock()
	already := s.sent || s.discarded
	if !already {
		s.discarded = true
	}
	s.mu.Unlock()
	if !already {
		_ = Discard(s.path)
	}
	s.render(w, "discarded", "", http.StatusOK)
	s.once.Do(func() { close(s.decided) })
}
