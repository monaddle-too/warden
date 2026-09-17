package edge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// A tls:// upstream is reached with the edge's own certificate: no bearer,
// no endpoint file, the chat's name as the Host and Origin, the identity
// headers set from the edge's authenticator. An edge holding another
// service's certificate is refused by the chat and reports it offline.
func TestMutualTLSUpstreamNeedsNoCapability(t *testing.T) {
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan *http.Request, 8)
	id := strings.Repeat("a", 32)
	chat := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		if r.URL.Path == "/api/ports" {
			io.WriteString(w, `[{"id":"`+id+`","state":"approved"}]`)
			return
		}
		io.WriteString(w, "state")
	})}
	l, err := transport.Listen("tls://127.0.0.1:0", transport.ListenOptions{TLS: material(t, ca, dir, transport.Chat), Peers: []string{transport.Edge}})
	if err != nil {
		t.Fatal(err)
	}
	go chat.Serve(l)
	defer chat.Close()
	upstream := "tls://" + l.Addr().String()
	base := Config{Origin: "https://warden.example.com", PreviewSuffix: "preview.example.com", ClientID: "test-client", OwnerEmails: "owner@gmail.com", Upstream: upstream, UpstreamHost: "warden-chat:7445"}
	if _, err = New(base); err == nil {
		t.Fatal("tls:// upstream without material accepted")
	}
	c := base
	c.UpstreamTLS = material(t, ca, dir, transport.Edge)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAuth{active: true}
	s.Auth = a
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	inventory := <-seen
	if inventory.Header.Get("Authorization") != "" || inventory.Host != "warden-chat:7445" || inventory.TLS == nil || transport.IdentityOf(*inventory.TLS) != transport.Edge {
		t.Fatalf("inventory request: %+v", inventory.Header)
	}
	if !s.known(id) {
		t.Fatal("approved binding not learned over tls")
	}
	r := httptest.NewRequest("GET", "https://warden.example.com/api/state", nil)
	r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
	r.Header.Set("Authorization", "Bearer browser-supplied")
	r.Header.Set("Origin", "https://warden.example.com")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 || out.Body.String() != "state" {
		t.Fatal(out.Code, out.Body.String())
	}
	up := <-seen
	if up.Header.Get("Authorization") != "" || up.Host != "warden-chat:7445" || up.Header.Get("Origin") != "https://warden-chat:7445" || up.Header.Get(HeaderPrincipal) != "google-subject" || up.Header.Get(HeaderEmail) != "owner@gmail.com" {
		t.Fatalf("proxied request: host %s headers %+v", up.Host, up.Header)
	}
	// The wrong certificate is refused in the handshake: nothing is
	// learned and the app is reported offline.
	c.UpstreamTLS = material(t, ca, dir, transport.Runner)
	wrong, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	wrong.Auth = a
	if err = wrong.Refresh(context.Background()); err == nil {
		t.Fatal("runner certificate admitted by the chat")
	}
	out = httptest.NewRecorder()
	wrong.ServeHTTP(out, r)
	if out.Code != 503 {
		t.Fatal(out.Code)
	}
	select {
	case r := <-seen:
		t.Fatalf("request with the wrong certificate reached the chat: %s", r.URL)
	default:
	}
	// The loopback shape still needs its exact upstream.
	for _, bad := range []string{"http://10.0.0.1:18780", "tls://:7445", "tls://warden-chat:7445/x", "https://warden-chat:7445"} {
		c := base
		c.Upstream = bad
		c.UpstreamTLS = s.Config.UpstreamTLS
		if _, err := New(c); err == nil {
			t.Fatalf("upstream %q accepted", bad)
		}
	}
}
