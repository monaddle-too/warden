package chats

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/transport"
)

func material(t *testing.T, ca *transport.CA, dir, id string) *transport.TLS {
	t.Helper()
	m, err := ca.Material(filepath.Join(dir, ca.Certificate.Subject.CommonName, id), id, []string{"127.0.0.1"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func leaf(t *testing.T, m *transport.TLS) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(m.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// With Peer set, the chat admits a request by the client certificate's
// identity and nothing else: no bearer exists, and the identity headers
// are honoured because only the edge can reach the handler.
func TestPeerCertificateReplacesTheCapability(t *testing.T) {
	e, _, _ := setup(t)
	now := time.Unix(1000, 0)
	e.Now = func() time.Time { return now }
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Peer: transport.Edge, Host: "warden-chat:7445", Origin: "https://warden-chat:7445", WebDir: t.TempDir()}
	id, _ := e.Create("shared", "", "")
	call := func(method, path, body string, state *tls.ConnectionState, headers map[string]string) int {
		t.Helper()
		r := httptest.NewRequest(method, h.Origin+"/api/"+path, strings.NewReader(body))
		r.Host = h.Host
		r.TLS = state
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	edge := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf(t, material(t, ca, dir, transport.Edge))}}
	runner := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf(t, material(t, ca, dir, transport.Runner))}}
	if code := call("GET", "ports", "", edge, nil); code != 200 {
		t.Fatalf("edge: %d", code)
	}
	for name, state := range map[string]*tls.ConnectionState{"another identity": runner, "no client certificate": {}, "plain http": nil} {
		if code := call("GET", "ports", "", state, nil); code != 401 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	// A bearer means nothing in this mode, even a correct-looking one.
	h.Token = "private"
	if code := call("GET", "ports", "", nil, map[string]string{"Authorization": "Bearer private"}); code != 401 {
		t.Fatalf("bearer admitted beside a peer identity: %d", code)
	}
	h.Token = ""
	alice := map[string]string{"X-Warden-Principal": "sub-alice", "X-Warden-Email": "alice@example.com", "X-Warden-Name": "Alice Example"}
	if code := call("POST", "chats/"+id+"/message", `{"text":"hello","id":"`+strings.Repeat("a", 32)+`"}`, edge, alice); code != 200 {
		t.Fatalf("message: %d", code)
	}
	if s := e.View().chat(id).Conversation.Entries[0].Sender; s == nil || s.PrincipalID != "sub-alice" || s.Name != "Alice Example" {
		t.Fatalf("sender: %+v", s)
	}
	if code := call("POST", "chats/"+id+"/message", `{"text":"nope","id":"`+strings.Repeat("b", 32)+`"}`, runner, alice); code != 401 {
		t.Fatalf("another identity wrote: %d", code)
	}
}

// Served over a tls:// listener, the chat completes the handshake only with
// the edge's certificate; the runner's, the policy service's and a
// certificate from another CA never reach the handler.
func TestChatListenerAdmitsOnlyTheEdge(t *testing.T) {
	e, _, _ := setup(t)
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Peer: transport.Edge, Host: "warden-chat:7445", Origin: "https://warden-chat:7445", WebDir: t.TempDir()}
	l, err := transport.Listen("tls://127.0.0.1:0", transport.ListenOptions{TLS: material(t, ca, dir, transport.Chat), Peers: []string{transport.Edge}})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h}
	go server.Serve(l)
	defer server.Close()
	get := func(m *transport.TLS) (int, string, error) {
		config, err := transport.ClientConfig(m, transport.Chat)
		if err != nil {
			return 0, "", err
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: config}}
		req, _ := http.NewRequest("GET", "https://"+l.Addr().String()+"/api/ports", nil)
		req.Host = h.Host
		res, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), nil
	}
	if code, body, err := get(material(t, ca, dir, transport.Edge)); err != nil || code != 200 || strings.TrimSpace(body) == "" {
		t.Fatalf("edge: %d %q %v", code, body, err)
	}
	other, err := transport.NewCA("other", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*transport.TLS{"runner": material(t, ca, dir, transport.Runner), "policy": material(t, ca, dir, transport.Policy), "edge from another CA": material(t, other, dir, transport.Edge)} {
		if code, _, err := get(m); err == nil {
			t.Fatalf("%s admitted: %d", name, code)
		}
	}
}
