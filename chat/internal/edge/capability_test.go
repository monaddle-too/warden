package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/transport"
)

// mutualChat is a chat behind a tls:// listener admitting the edge's
// certificate, answering the port inventory and echoing the requests it
// sees; it is the Kubernetes-shape upstream.
func mutualChat(t *testing.T) (upstream string, edgeTLS *transport.TLS, seen chan *http.Request) {
	t.Helper()
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	seen = make(chan *http.Request, 16)
	chat := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		if r.URL.Path == "/api/ports" {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, "state")
	})}
	l, err := transport.Listen("tls://127.0.0.1:0", transport.ListenOptions{TLS: material(t, ca, dir, transport.Chat), Peers: []string{transport.Edge}})
	if err != nil {
		t.Fatal(err)
	}
	go chat.Serve(l)
	t.Cleanup(func() { chat.Close() })
	return "tls://" + l.Addr().String(), material(t, ca, dir, transport.Edge), seen
}

var launchShape = regexp.MustCompile(`^http://127\.0\.0\.1:18781/\?launch=[0-9]+#session=([a-f0-9]{64})$`)

// readEndpoint is the chat's endpoint file shape, as the edge's reader,
// `warden open` and the launcher's readiness check understand it.
func readEndpoint(t *testing.T, path string) (url, token string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var endpoint struct{ URL, Token string }
	if err = json.Unmarshal(raw, &endpoint); err != nil {
		t.Fatalf("%s: %v: %s", path, err, raw)
	}
	return endpoint.URL, endpoint.Token, info.Mode().Perm()
}

// In owner mode over a tls:// upstream the chat writes no endpoint file, so
// the edge mints the owner capability itself: persisted 0600 in its own
// file in the chat's shape, announced as the launch URL in the log, and
// the credential the browser then signs in with exactly as on the loopback
// shape (bearer, then a cookie session for navigations), while the chat is
// still reached by certificate alone. Rotating it ends every session.
func TestOwnerCapabilityMintedOverTLS(t *testing.T) {
	upstream, edgeTLS, seen := mutualChat(t)
	path := filepath.Join(t.TempDir(), "edge", "endpoint.json") // the state directory may not exist yet
	c := Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: upstream, UpstreamHost: "warden-chat:7445", UpstreamTLS: edgeTLS, OwnerTokenFile: path, Listen: "0.0.0.0:18781"}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	s.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if !s.MintsOwnerCapability() {
		t.Fatal("owner mode over tls:// does not mint the capability")
	}
	// Nothing signs in before the first mint: there is no capability.
	if out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", strings.Repeat("x", 64)); out.Code != 401 {
		t.Fatal("signed in without a capability:", out.Code)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	launch, err := s.RotateOwnerCapability(now)
	if err != nil {
		t.Fatal(err)
	}
	m := launchShape.FindStringSubmatch(launch)
	if m == nil || !strings.Contains(launch, "?launch="+fmt.Sprint(now.UnixNano())+"#") {
		t.Fatalf("launch URL %q", launch)
	}
	token := m[1]
	if u, got, mode := readEndpoint(t, path); u != "http://127.0.0.1:18781" || got != token || mode != 0o600 {
		t.Fatalf("endpoint file: url %q token %q mode %o", u, got, mode)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left beside the endpoint file")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "launch URL") || !strings.Contains(logged[0], launch) || !strings.Contains(logged[0], path) {
		t.Fatalf("log: %q", logged)
	}
	if !s.Auth.ActiveSession("capability:" + s.Auth.(*ownerAuth).current()) {
		t.Fatal("minted capability is not the live one")
	}
	// The owner signs in with it: the bearer identifies them, the chat is
	// reached by certificate (no bearer forwarded) with the owner principal,
	// and the cookie session carries later navigations.
	owner := ownerCookieFor(t, s, token)
	up := <-seen
	if up.URL.Path != "/api/state" || up.Header.Get("Authorization") != "" || up.Host != "warden-chat:7445" || up.TLS == nil || transport.IdentityOf(*up.TLS) != transport.Edge || up.Header.Get(HeaderPrincipal) != "owner" {
		t.Fatalf("proxied request: host %s headers %+v", up.Host, up.Header)
	}
	if out := invoke(s, "http://127.0.0.1:18781/api/state", owner); out.Code != 200 {
		t.Fatal("cookie session refused:", out.Code)
	}
	// Rotation: a fresh capability, the file replaced in place, the old
	// bearer and every session minted from it gone, the new one announced.
	rotated, err := s.RotateOwnerCapability(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next := launchShape.FindStringSubmatch(rotated)
	if next == nil || next[1] == token {
		t.Fatalf("rotation returned %q after %q", rotated, launch)
	}
	if _, got, mode := readEndpoint(t, path); got != next[1] || mode != 0o600 {
		t.Fatalf("endpoint file after rotation: token %q mode %o", got, mode)
	}
	if len(logged) != 2 || !strings.Contains(logged[1], rotated) {
		t.Fatalf("log after rotation: %q", logged)
	}
	if out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", token); out.Code != 401 {
		t.Fatal("previous capability still signs in:", out.Code)
	}
	if out := invoke(s, "http://127.0.0.1:18781/api/state", owner); out.Code != 401 {
		t.Fatal("session from the previous capability survived rotation:", out.Code)
	}
	if out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", next[1]); out.Code != 200 {
		t.Fatal("rotated capability refused:", out.Code)
	}
	// The endpoint file is the only source: hand-edit it and the edge follows.
	os.WriteFile(path, []byte(`{"url":"http://127.0.0.1:18781","token":"`+strings.Repeat("h", 64)+`"}`), 0o600)
	if out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", strings.Repeat("h", 64)); out.Code != 200 {
		t.Fatal("edge does not read its own endpoint file:", out.Code)
	}
}

// The chat holds the capability on the loopback shape: the edge reads the
// chat's file, mints nothing and refuses to overwrite it.
func TestOwnerCapabilityNotMintedOverLoopback(t *testing.T) {
	s, token := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "state") }))
	before, _ := os.ReadFile(s.Config.OwnerTokenFile)
	if s.MintsOwnerCapability() {
		t.Fatal("loopback edge claims the chat's capability")
	}
	if launch, err := s.RotateOwnerCapability(time.Now()); err == nil || launch != "" {
		t.Fatalf("rotated over loopback: %q %v", launch, err)
	}
	if after, _ := os.ReadFile(s.Config.OwnerTokenFile); string(after) != string(before) {
		t.Fatal("the chat's endpoint file was touched")
	}
	if out := bearer(s, "GET", "http://127.0.0.1:18781/api/state", token); out.Code != 200 {
		t.Fatal("chat's capability refused:", out.Code)
	}
}

// Google mode over tls:// has no capability at all (sign-in is Google's and
// the chat is reached by certificate); and the minting edge needs to know
// where its file lives.
func TestOwnerCapabilityConfiguration(t *testing.T) {
	upstream, edgeTLS, _ := mutualChat(t)
	google := Config{Origin: "https://warden.example.com", PreviewSuffix: "preview.example.com", ClientID: "test-client", OwnerEmails: "owner@gmail.com", Upstream: upstream, UpstreamHost: "warden-chat:7445", UpstreamTLS: edgeTLS}
	s, err := New(google)
	if err != nil {
		t.Fatal(err)
	}
	if s.MintsOwnerCapability() {
		t.Fatal("google mode mints an owner capability")
	}
	owner := Config{Mode: ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: upstream, UpstreamHost: "warden-chat:7445", UpstreamTLS: edgeTLS, Listen: "0.0.0.0:18781"}
	for name, file := range map[string]string{"no file": "", "relative file": "edge/endpoint.json"} {
		c := owner
		c.OwnerTokenFile = file
		if _, err := New(c); err == nil {
			t.Fatal("accepted:", name)
		}
	}
	owner.OwnerTokenFile = filepath.Join(t.TempDir(), "endpoint.json")
	if s, err = New(owner); err != nil || !s.MintsOwnerCapability() {
		t.Fatal(err)
	}
	// The launch URL is what `warden open` builds from the chat's file: the
	// capability in the fragment, which never reaches a server.
	launch, err := s.RotateOwnerCapability(time.Unix(0, 42))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(launch)
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:18781" || u.Path != "/" || u.Query().Get("launch") != "42" || len(u.Fragment) != len("session=")+64 {
		t.Fatalf("launch URL %q: %v", launch, err)
	}
}
