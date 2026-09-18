// Package edge is Warden's authenticated ingress. In public mode it sits
// behind a TLS terminator with Google sign-in; in loopback mode it listens on
// a loopback port over plain HTTP, serves previews as <binding>.localhost and
// authenticates the owner by the launcher capability. Its private upstream
// is the chat server: on the single-host shapes the loopback chat, reached
// with the owner capability from endpoint.json as a bearer; on Kubernetes a
// tls:// address reached with the edge's own certificate, which is its
// authority to forward the identity headers (no bearer, and no endpoint
// file from the chat: in owner mode the edge mints the capability itself,
// see capability.go).
package edge

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/browserauth"
	"warden/chat/internal/transport"
)

const previewCookie = "__Host-warden-preview"
const challengeCookie = "__Host-warden-preview-challenge"

// Auth modes; empty means Google (the original edge configuration).
const (
	ModeGoogle = "google"
	ModeOwner  = "owner"
)

type Config struct {
	// Mode is "google" (default) or "owner": one person identified by the
	// launcher capability, no sign-in client.
	Mode          string `json:"mode,omitempty"`
	Origin        string `json:"origin"`
	PreviewSuffix string `json:"previewSuffix"`
	ClientID      string `json:"clientID"`
	OwnerEmails   string `json:"ownerEmails"`
	DemoDomains   string `json:"demoDomains"`
	// Upstream is the chat: http://127.0.0.1:<port>, or tls://<host>:<port>
	// dialed with UpstreamTLS. UpstreamHost is the Host header the chat
	// expects (its listen host:port, or the host of its tls:// address).
	Upstream     string         `json:"upstream"`
	UpstreamHost string         `json:"upstreamHost"`
	UpstreamTLS  *transport.TLS `json:"upstreamTLS,omitempty"`
	// OwnerTokenFile is the endpoint file holding the owner capability:
	// the chat's, over a loopback upstream; the edge's own, which it mints
	// (RotateOwnerCapability), in owner mode over a tls:// upstream.
	OwnerTokenFile string `json:"ownerTokenFile"`
	LoginsFile     string `json:"loginsFile"`
	// SessionsFile keeps the Google sign-in sessions across restarts;
	// empty puts it beside the ledger (sessions.json), or, without a
	// ledger, keeps them in memory.
	SessionsFile string `json:"sessionsFile,omitempty"`
	Listen       string `json:"listen"`
}
type previewSession struct {
	Parent, Binding string
	Expires         time.Time
}
type ticket struct {
	Parent, Binding, Challenge, Path string
	Expires                          time.Time
}
type binding struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type Authenticator interface {
	Role(*http.Request) string
	Handler() http.Handler
	SessionRef(*http.Request) (string, bool)
	ActiveSession(string) bool
	// Identity names the person behind an authenticated request so the
	// chat can attribute what they write: a stable principal, their email
	// and display name (either may be empty). ok is false when unknown.
	Identity(*http.Request) (principal, email, name string, ok bool)
}

// Identity headers the edge sets on requests it forwards to the chat; a
// client cannot supply them because the edge strips its own copies first.
const (
	HeaderPrincipal = "X-Warden-Principal"
	HeaderEmail     = "X-Warden-Email"
	HeaderName      = "X-Warden-Name"
)

type Server struct {
	Config      Config
	Auth        Authenticator
	host        string
	scheme      string // origin and preview scheme: https (public) or http (loopback)
	previewPort string // port carried by loopback preview hosts; "" in public mode
	secure      bool   // Secure, __Host- cookies (https only)
	target      *url.URL
	upstreamTLS *tls.Config // mutual TLS to a tls:// upstream; nil for loopback http
	mint        bool        // owner mode over tls://: the edge holds the capability (capability.go)
	// Logf receives the edge's one-line notices, the launch URL among
	// them; log.Printf unless replaced.
	Logf        func(format string, args ...any)
	mu          sync.Mutex
	sessions    map[string]previewSession
	tickets     map[string]ticket
	bindings    map[string]bool
	lastRefresh time.Time
	Client      *http.Client
	logins      *ledger
}

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func New(c Config) (*Server, error) {
	origin, err := url.Parse(c.Origin)
	if err != nil || origin.Host == "" || origin.Path != "" || origin.User != nil || (origin.Scheme != "https" && origin.Scheme != "http") {
		return nil, errors.New("canonical Warden origin required")
	}
	target, err := url.Parse(c.Upstream)
	if err != nil || target.Port() == "" || target.Path != "" || target.User != nil || target.RawQuery != "" {
		return nil, errors.New("private loopback or tls:// upstream required")
	}
	var upstreamTLS *tls.Config
	switch {
	case target.Scheme == "http" && target.Hostname() == "127.0.0.1":
	case target.Scheme == "tls" && target.Hostname() != "":
		if c.UpstreamTLS == nil {
			return nil, errors.New("a tls:// upstream needs the edge's TLS material")
		}
		if upstreamTLS, err = transport.ClientConfig(c.UpstreamTLS, target.Hostname()); err != nil {
			return nil, err
		}
		target = &url.URL{Scheme: "https", Host: target.Host}
	default:
		return nil, errors.New("private loopback or tls:// upstream required")
	}
	if c.UpstreamHost == "" || strings.ContainsAny(c.PreviewSuffix, "/:@?#*") {
		return nil, errors.New("upstream host and preview suffix required")
	}
	s := &Server{Config: c, host: origin.Host, scheme: origin.Scheme, secure: origin.Scheme == "https", target: target, upstreamTLS: upstreamTLS, Logf: log.Printf, sessions: map[string]previewSession{}, tickets: map[string]ticket{}, bindings: map[string]bool{}, Client: &http.Client{Timeout: 5 * time.Second}}
	if upstreamTLS != nil {
		s.Client.Transport = &http.Transport{Proxy: nil, TLSClientConfig: upstreamTLS}
	}
	mode := c.Mode
	if mode == "" {
		mode = ModeGoogle
	}
	switch {
	case origin.Scheme == "https" && mode == ModeGoogle:
		// Public mode: TLS terminator in front, Google sign-in, dotted suffix.
		if !strings.Contains(c.PreviewSuffix, ".") {
			return nil, errors.New("public previews need a dotted hostname suffix")
		}
		sessions := c.SessionsFile
		if sessions == "" && c.LoginsFile != "" {
			sessions = filepath.Join(filepath.Dir(c.LoginsFile), "sessions.json")
		}
		auth, err := browserauth.New(browserauth.Config{ClientID: c.ClientID, Origin: c.Origin, AdminEmails: c.OwnerEmails, DemoDomains: c.DemoDomains, SessionsFile: sessions})
		if err != nil {
			return nil, err
		}
		if c.ClientID == "" {
			return nil, errors.New("public ingress requires Google sign-in")
		}
		logins, err := newLedger(c.LoginsFile)
		if err != nil {
			return nil, err
		}
		auth.OnLogin = logins.record
		s.Auth, s.logins = auth, logins
	case origin.Scheme == "http" && mode == ModeOwner:
		// Loopback mode: plain HTTP on a loopback port, previews on
		// <binding>.localhost:<port>, the owner identified by the capability.
		if ip := net.ParseIP(origin.Hostname()); ip == nil || !ip.IsLoopback() || origin.Port() == "" {
			return nil, errors.New("loopback mode needs an http://127.0.0.1:<port> origin")
		}
		if c.PreviewSuffix != "localhost" {
			return nil, errors.New("loopback previews use the \"localhost\" suffix")
		}
		if c.ClientID != "" || c.OwnerEmails != "" || c.DemoDomains != "" {
			return nil, errors.New("owner mode has no Google sign-in configuration")
		}
		s.previewPort = origin.Port()
		if c.Listen != "" {
			if _, port, err := net.SplitHostPort(c.Listen); err != nil || port != s.previewPort {
				return nil, errors.New("loopback mode listens on the origin's port")
			}
		}
		// Over tls:// the chat writes no endpoint file, so the capability
		// the owner signs in with is the edge's own (capability.go).
		if s.mint = upstreamTLS != nil; s.mint && !filepath.IsAbs(c.OwnerTokenFile) {
			return nil, errors.New("owner mode over a tls:// upstream keeps its capability in an absolute ownerTokenFile")
		}
		s.Auth = newOwnerAuth(s.token, false, ownerCookie+"-"+s.previewPort)
		s.logins, _ = newLedger("")
	default:
		return nil, errors.New("https origins use Google sign-in; owner mode is loopback http only")
	}
	return s, nil
}

// previewOrigin is the origin viewers open for one binding.
func (s *Server) previewOrigin(id string) string {
	host := id + "." + s.Config.PreviewSuffix
	if s.previewPort != "" {
		host += ":" + s.previewPort
	}
	return s.scheme + "://" + host
}
func (s *Server) cookieName(base string) string {
	if s.secure {
		return base
	}
	// __Host- names require Secure; loopback HTTP keeps them host-scoped by
	// omitting Domain, which browsers then bind to the exact preview host.
	return strings.TrimPrefix(base, "__Host-")
}
func (s *Server) token() (string, error) {
	b, err := os.ReadFile(s.Config.OwnerTokenFile)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(b))
	if strings.HasPrefix(value, "{") {
		var endpoint struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(b, &endpoint) != nil {
			return "", errors.New("invalid chat endpoint")
		}
		value = endpoint.Token
	}
	if len(value) < 32 {
		return "", errors.New("upstream credential unavailable")
	}
	return value, nil
}

// credential is the bearer the chat admits on the loopback shape; over
// mutual TLS the edge's certificate is the credential and none is sent.
func (s *Server) credential() (string, error) {
	if s.upstreamTLS != nil {
		return "", nil
	}
	return s.token()
}
func (s *Server) Refresh(ctx context.Context) error {
	token, err := s.credential()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.target.String()+"/api/ports", nil)
	if err != nil {
		return err
	}
	req.Host = s.Config.UpstreamHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("upstream port inventory unavailable: %d", res.StatusCode)
	}
	var ports []binding
	if err = json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&ports); err != nil {
		return err
	}
	next := map[string]bool{}
	for _, p := range ports {
		if idPattern.MatchString(p.ID) && p.State == "approved" {
			next[p.ID] = true
		}
	}
	s.mu.Lock()
	s.bindings = next
	s.lastRefresh = time.Now()
	s.cleanup()
	s.mu.Unlock()
	return nil
}
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		_ = s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Server) known(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindings[id] && time.Since(s.lastRefresh) < 10*time.Second
}
func (s *Server) cleanup() {
	now := time.Now()
	for id, v := range s.sessions {
		if !now.Before(v.Expires) {
			delete(s.sessions, id)
		}
	}
	for id, v := range s.tickets {
		if !now.Before(v.Expires) {
			delete(s.tickets, id)
		}
	}
}
func (s *Server) bindingHost(host string) string {
	if s.previewPort != "" {
		name, port, err := net.SplitHostPort(host)
		if err != nil || port != s.previewPort {
			return ""
		}
		host = name
	}
	suffix := "." + s.Config.PreviewSuffix
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	id := strings.TrimSuffix(host, suffix)
	if !idPattern.MatchString(id) {
		return ""
	}
	return id
}
func headers(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	headers(w)
	if r.URL.Path == "/_tls/allow" {
		id := s.bindingHost(r.URL.Query().Get("domain"))
		if id != "" && s.known(id) {
			w.WriteHeader(204)
		} else {
			http.Error(w, "unknown binding", 403)
		}
		return
	}
	if r.Host == s.host {
		s.main(w, r)
		return
	}
	id := s.bindingHost(r.Host)
	if id == "" {
		http.Error(w, "unknown Warden host", 403)
		return
	}
	s.preview(id, w, r)
}
func (s *Server) main(w http.ResponseWriter, r *http.Request) {
	// Guest HTML must never run on Warden's authenticated application origin.
	if strings.HasPrefix(r.URL.Path, "/api/ports/") && strings.Contains(r.URL.Path, "/proxy/") {
		http.Error(w, "open the dedicated preview hostname", http.StatusForbidden)
		return
	}
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
	w.Header().Set("Content-Security-Policy", s.mainCSP())
	if r.URL.Path == "/auth/preview" {
		s.authorizePreview(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/auth/") {
		s.Auth.Handler().ServeHTTP(w, r)
		return
	}
	// The demo shares chats and grants, but provider account connections, the
	// admin console and "unsharable with AI" tags remain owner-only.
	if ownerOnly(r.URL.Path) {
		if s.Auth.Role(r) != "admin" {
			http.Error(w, "Only the demo owner can manage connected accounts and sharing rules", http.StatusForbidden)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		s.admin(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") && s.Auth.Role(r) == "" {
		http.Error(w, "Warden sign-in required", 401)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") && r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		parent, ok := s.Auth.SessionRef(r)
		if !ok {
			http.Error(w, "Warden sign-in required", 401)
			return
		}
		if minter, ok := s.Auth.(sessionMinter); ok {
			minter.Establish(w, r)
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if !s.Auth.ActiveSession(parent) {
						cancel()
						return
					}
				}
			}
		}()
		r = r.WithContext(ctx)
	}
	s.proxy("", w, r)
}
func ownerOnly(path string) bool {
	trimmed := strings.Trim(path, "/")
	return strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/api/admin/") || strings.HasPrefix(path, "/api/cluster") || trimmed == "api/spend" ||
		trimmed == "api/sharing/connect" || trimmed == "api/sharing/disconnect" || trimmed == "api/sharing/egress_set" || trimmed == "api/sharing/block" || trimmed == "api/sharing/unblock" ||
		// The GitHub sign-in code binds whichever account types it to this
		// Warden, so only the owner may see or start one.
		strings.HasPrefix(trimmed, "api/sharing/github_login_")
}

// admin answers from the edge's own login ledger; identity is verified only here
// and the upstream never learns which admitted user is browsing.
func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || strings.Trim(r.URL.Path, "/") != "api/admin/users" {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"persistent": s.logins.persistent(), "users": s.logins.snapshot()})
}
func (s *Server) setCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(name), Value: value, Path: "/", Secure: s.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}

// mainCSP is the application origin's policy. Previews may be framed only
// from their own scheme, suffix and port; Google's sign-in sources appear
// only in Google mode.
func (s *Server) mainCSP() string {
	frames := s.scheme + "://*." + s.Config.PreviewSuffix
	if s.previewPort != "" {
		frames += ":" + s.previewPort
	}
	if !s.secure {
		return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; frame-src " + frames + "; connect-src 'self'; img-src 'self' blob: data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	}
	return "default-src 'self'; script-src 'self' https://accounts.google.com/gsi/client; style-src 'self' 'unsafe-inline' https://accounts.google.com; frame-src https://accounts.google.com " + frames + "; connect-src 'self' https://accounts.google.com; img-src 'self' blob: data: https://*.googleusercontent.com; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
}

// sessionMinter is implemented by authenticators that turn a header-
// authenticated request into a cookie session for later navigations.
type sessionMinter interface {
	Establish(http.ResponseWriter, *http.Request)
}

func safePath(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n") {
		return "/"
	}
	return path
}
func (s *Server) authorizePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	id, challenge := r.URL.Query().Get("binding"), r.URL.Query().Get("challenge")
	if !s.known(id) || !idPattern.MatchString(id) || len(challenge) != 64 {
		http.Error(w, "preview binding unavailable", 404)
		return
	}
	parent, ok := s.Auth.SessionRef(r)
	if !ok {
		http.Redirect(w, r, s.Config.Origin+"/?next="+url.QueryEscape(r.URL.RequestURI()), 303)
		return
	}
	code := random()
	s.mu.Lock()
	s.cleanup()
	if len(s.tickets) >= 4096 {
		s.mu.Unlock()
		http.Error(w, "too many sign-ins", 429)
		return
	}
	s.tickets[code] = ticket{Parent: parent, Binding: id, Challenge: challenge, Path: safePath(r.URL.Query().Get("path")), Expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	http.Redirect(w, r, s.previewOrigin(id)+"/_warden/login?code="+code, 303)
}
func (s *Server) preview(id string, w http.ResponseWriter, r *http.Request) {
	if !s.known(id) {
		http.Error(w, "preview binding unavailable or revoked", 410)
		return
	}
	origin := s.scheme + "://" + r.Host
	if r.URL.Path == "/_warden/login" {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		code := r.URL.Query().Get("code")
		cookie, err := r.Cookie(s.cookieName(challengeCookie))
		s.mu.Lock()
		t, ok := s.tickets[code]
		if ok {
			delete(s.tickets, code)
		}
		s.mu.Unlock()
		if err != nil || !ok || t.Binding != id || t.Challenge != cookie.Value || !time.Now().Before(t.Expires) || !s.Auth.ActiveSession(t.Parent) {
			http.Error(w, "Warden preview sign-in expired; reopen the preview", 401)
			return
		}
		key := random()
		s.mu.Lock()
		s.cleanup()
		if len(s.sessions) >= 4096 {
			s.mu.Unlock()
			http.Error(w, "too many preview sessions", 429)
			return
		}
		s.sessions[key] = previewSession{Parent: t.Parent, Binding: id, Expires: time.Now().Add(8 * time.Hour)}
		s.mu.Unlock()
		s.setCookie(w, challengeCookie, "", -1)
		s.setCookie(w, previewCookie, key, 8*3600)
		http.Redirect(w, r, safePath(t.Path), 303)
		return
	}
	cookie, err := r.Cookie(s.cookieName(previewCookie))
	var session previewSession
	var ok bool
	if err == nil {
		s.mu.Lock()
		session, ok = s.sessions[cookie.Value]
		s.mu.Unlock()
	}
	if !ok || session.Binding != id || !time.Now().Before(session.Expires) || !s.Auth.ActiveSession(session.Parent) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Warden sign-in required", 401)
			return
		}
		challenge := random()
		s.setCookie(w, challengeCookie, challenge, 300)
		next := s.Config.Origin + "/auth/preview?binding=" + id + "&challenge=" + challenge + "&path=" + url.QueryEscape(safePath(r.URL.RequestURI()))
		http.Redirect(w, r, next, 303)
		return
	}
	if incoming := r.Header.Get("Origin"); incoming != "" && incoming != origin {
		http.Error(w, "invalid preview origin", 403)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != origin {
		http.Error(w, "same-origin preview write required", 403)
		return
	}
	w.Header().Set("Content-Security-Policy", "frame-ancestors "+s.Config.Origin)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !s.Auth.ActiveSession(session.Parent) || !s.known(id) || !time.Now().Before(session.Expires) {
					cancel()
					return
				}
			}
		}
	}()
	s.proxy(id, w, r.WithContext(ctx))
}
func (s *Server) proxy(binding string, w http.ResponseWriter, r *http.Request) {
	token, err := s.credential()
	if err != nil {
		http.Error(w, "Warden host unavailable", 503)
		return
	}
	principal, email, name, known := s.Auth.Identity(r)
	proxy := httputil.NewSingleHostReverseProxy(s.target)
	proxy.Director = func(req *http.Request) {
		req.Header.Del(HeaderPrincipal)
		req.Header.Del(HeaderEmail)
		req.Header.Del(HeaderName)
		if known && principal != "" {
			req.Header.Set(HeaderPrincipal, principal)
			if email != "" {
				req.Header.Set(HeaderEmail, email)
			}
			if name != "" {
				req.Header.Set(HeaderName, name)
			}
		}
		req.URL.Scheme = s.target.Scheme
		req.URL.Host = s.target.Host
		req.Host = s.Config.UpstreamHost
		if binding != "" {
			req.URL.Path = "/api/ports/" + binding + "/proxy" + req.URL.Path
			req.URL.RawPath = ""
		}
		req.Header.Del("Cookie")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Forwarded")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
		req.Header.Del("X-Warden-CSRF")
		req.Header.Del("Authorization")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", s.target.Scheme+"://"+s.Config.UpstreamHost)
		}
	}
	proxy.FlushInterval = -1
	proxy.Transport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, TLSClientConfig: s.upstreamTLS}
	proxy.ModifyResponse = func(res *http.Response) error {
		res.Header.Del("Set-Cookie")
		res.Header.Del("Content-Security-Policy")
		res.Header.Set("Cache-Control", "no-store")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "Warden host is offline", 503) }
	proxy.ServeHTTP(w, r)
}
