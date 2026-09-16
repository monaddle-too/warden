package chats

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"warden/chat/internal/agent"
	"warden/chat/internal/sandbox"
)

type portWorker struct {
	fakeWorker
	attachment sandbox.PreviewAttachment
	failRemove bool
	stopped    func()
	calls      []string
}

func (w *portWorker) Call(ctx context.Context, r sandbox.Request) (sandbox.Response, error) {
	w.calls = append(w.calls, r.Operation)
	if r.Operation == "preview.remove" && w.failRemove {
		return sandbox.Response{}, errors.New("worker offline")
	}
	if r.Operation == "preview.attach" && w.stopped != nil {
		w.stopped()
	}
	return sandbox.Response{Attachment: &w.attachment, Attachments: []sandbox.PreviewAttachment{w.attachment}}, nil
}
func portEngine(t *testing.T, target string) (*Engine, *portWorker, *Chat) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w := &portWorker{}
	e := NewEngine(s, w)
	e.PublicPreviewSuffix = "preview.example.com"
	id, err := e.Create("Ports", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.update(func(st *State) error { c := st.chat(id); c.Status = "running"; c.RunID = "run"; return nil })
	c := s.Snapshot().chat(id)
	w.attachment = sandbox.PreviewAttachment{ID: "attachment", ChatID: id, SandboxID: c.SandboxID, Port: 3000, URL: target, State: "available"}
	return e, w, c
}
func TestPortRequestRequiresApproval(t *testing.T) {
	e, w, c := portEngine(t, "http://127.0.0.1:23456/")
	err := e.requestPort(c, nil, agent.Frame{ID: json.RawMessage(`1`), Params: map[string]any{"arguments": map[string]any{"port": 3000, "title": "Test", "path": "/"}}})
	if err != nil {
		t.Fatal(err)
	}
	st := e.Store.Snapshot()
	if len(st.Ports) != 0 || len(w.calls) != 0 || len(st.chat(c.ID).Approvals) != 1 {
		t.Fatal("request published without approval")
	}
	a := st.chat(c.ID).Approvals[0]
	v := e.resolvePort(c, a, false).(map[string]any)
	if v["success"] != false || len(w.calls) != 0 {
		t.Fatal("denial reached worker")
	}
	v = e.resolvePort(c, a, true).(map[string]any)
	if v["success"] != true || len(e.Store.Snapshot().Ports) != 1 {
		t.Fatal(v)
	}
}
func TestPortBindingStopsAndRevocationFailClosed(t *testing.T) {
	e, w, c := portEngine(t, "http://127.0.0.1:23456/")
	w.stopped = func() { e.Store.update(func(st *State) error { st.chat(c.ID).Status = "stopped"; return nil }) }
	if _, err := e.bindPort(c, portInput{Port: 3000, Title: "Test", Path: "/"}, ""); err == nil {
		t.Fatal("stopped run published")
	}
	if len(e.Store.Snapshot().Ports) != 0 {
		t.Fatal("stale binding retained")
	}
	w.stopped = nil
	e.Store.update(func(st *State) error { st.chat(c.ID).Status = "running"; return nil })
	p, err := e.bindPort(c, portInput{Port: 3000, Title: "Test", Path: "/"}, "")
	if err != nil {
		t.Fatal(err)
	}
	w.failRemove = true
	if e.RevokePort(context.Background(), p.ID) == nil {
		t.Fatal("expected worker failure")
	}
	out := httptest.NewRecorder()
	e.ServePort(p.ID, "/", out, httptest.NewRequest("GET", "http://warden/", nil))
	if out.Code != 410 || e.Store.Snapshot().Ports[0].State != "revoked" {
		t.Fatal("worker failure restored access")
	}
}
func TestPortProxyPathsAndCredentialIsolation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/assets/?x=1" {
			t.Error(r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Warden-CSRF") != "" {
			t.Error("credential leak")
		}
		w.Header().Set("Set-Cookie", "bad=yes")
		io.WriteString(w, "asset")
	}))
	defer up.Close()
	e, _, c := portEngine(t, up.URL)
	p, err := e.bindPort(c, portInput{Port: 3000, Title: "Test", Path: "/"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://warden/api/ports/"+p.ID+"/proxy/assets/?x=1", nil)
	r.Header.Set("Authorization", "secret")
	r.Header.Set("Cookie", "session=secret")
	r.Header.Set("X-Warden-CSRF", "secret")
	out := httptest.NewRecorder()
	e.ServePort(p.ID, "/assets/", out, r)
	if out.Code != 200 || out.Body.String() != "asset" || out.Header().Get("Set-Cookie") != "" {
		t.Fatal(out.Code, out.Body.String())
	}
}
func TestLoopbackBindingURLsCarryTheEdgePort(t *testing.T) {
	e, _, c := portEngine(t, "http://127.0.0.1:23456/")
	e.PublicPreviewSuffix, e.PreviewScheme, e.PreviewPort = "localhost", "http", "18781"
	p, err := e.bindPort(c, portInput{Port: 3000, Title: "Test", Path: "/app/"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "http://"+p.ID+".localhost:18781/app/" {
		t.Fatal(p.URL)
	}
	// Public previews keep their https URL without a port.
	e.PublicPreviewSuffix, e.PreviewScheme, e.PreviewPort = "preview.example.com", "", ""
	if p, err = e.bindPort(c, portInput{Port: 3000, Title: "Test", Path: "/"}, ""); err != nil || p.URL != "https://"+p.ID+".preview.example.com/" {
		t.Fatal(p.URL, err)
	}
	if ValidatePreviewSuffix("localhost") != nil || ValidatePreviewSuffix("") != nil {
		t.Fatal("localhost and empty suffixes are valid")
	}
}
func TestPortInputRejectsDestinations(t *testing.T) {
	for _, raw := range []string{`{"port":3000,"title":"X","url":"http://evil"}`, `{"port":0,"title":"X"}`, `{"port":3000,"title":"X","path":"//evil"}`} {
		if _, err := decodePort(raw); err == nil {
			t.Fatal(raw)
		}
	}
	for _, suffix := range []string{"evil/path", "x.example@evil", "*.example.com", "example.com:443"} {
		if ValidatePreviewSuffix(suffix) == nil {
			t.Fatal(suffix)
		}
	}
	e, w, c := portEngine(t, "https://evil.example/")
	_, err := e.bindPort(c, portInput{Port: 3000, Title: "X", Path: "/"}, strings.Repeat("a", 32))
	if err == nil || len(e.Store.Snapshot().Ports) != 0 {
		t.Fatal("untrusted worker URL accepted", w)
	}
}
