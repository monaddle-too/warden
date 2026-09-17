// Package browserauth exchanges verified Google identities for revocable,
// same-origin browser sessions. Machine API tokens remain independent.
package browserauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const sessionCookie = "__Host-warden-session"
const challengeCookie = "__Host-warden-login"
const sessionTTL = 8 * time.Hour
const challengeTTL = 10 * time.Minute
const maxEntries = 4096

type Config struct{ ClientID, Origin, AdminEmails, ReadEmails, DemoDomains string }
type User struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Role    string `json:"role"`
	// Name is the account's display name from the ID token, when Google
	// provides one; attribution falls back to the email otherwise.
	Name string `json:"name,omitempty"`
}
type session struct {
	HostedDomain string
	User         User
	CSRF         string
	Expires      time.Time
}

// Login is a verified sign-in that created a session. OnLogin observers record
// it durably; sessions themselves stay in memory and revocable.
type Login struct {
	User
	HostedDomain string
	At           time.Time
}
type challenge struct {
	Nonce   string
	Expires time.Time
}
type Auth struct {
	config      Config
	roles       map[string]string
	demoDomains map[string]bool
	verify      func(context.Context, string) (*oidc.IDToken, error)
	mu          sync.Mutex
	sessions    map[string]session
	challenges  map[string]challenge
	OnLogin     func(Login)
}

func New(c Config) (*Auth, error) {
	a := &Auth{config: c, roles: map[string]string{}, demoDomains: map[string]bool{}, sessions: map[string]session{}, challenges: map[string]challenge{}}
	if c.ClientID == "" {
		if c.Origin != "" || c.AdminEmails != "" || c.ReadEmails != "" || c.DemoDomains != "" {
			return nil, errors.New("Google client ID required when browser auth is configured")
		}
		return a, nil
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Google auth origin must be an HTTPS origin without a path")
	}
	for _, group := range []struct{ emails, role string }{{c.ReadEmails, "read"}, {c.AdminEmails, "admin"}} {
		for _, entry := range strings.Split(group.emails, ",") {
			email := strings.ToLower(strings.TrimSpace(entry))
			if email == "" {
				continue
			}
			parsed, err := mail.ParseAddress(email)
			if err != nil || parsed.Address != email || strings.Contains(email, "*") {
				return nil, errors.New("Google allowlists must contain exact email addresses")
			}
			a.roles[email] = group.role
		}
	}
	for _, entry := range strings.Split(c.DemoDomains, ",") {
		domain := strings.ToLower(strings.TrimSpace(entry))
		if domain == "" {
			continue
		}
		if len(domain) > 253 || !hostedDomainPattern.MatchString(domain) {
			return nil, errors.New("Google demo domains must contain exact DNS domain names")
		}
		a.demoDomains[domain] = true
	}
	if len(a.roles) == 0 && len(a.demoDomains) == 0 {
		return nil, errors.New("Google auth requires an explicit email allowlist")
	}
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 10 * time.Second})
	keys := oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs")
	a.verify = oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{oidc.RS256}}).Verify
	return a, nil
}

var hostedDomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Domain grants require Google's signed Workspace domain to match the exact
// email domain. Explicit email roles take precedence; domain grants only demo access.
func (a *Auth) roleFor(email, hostedDomain string) string {
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || strings.Count(email, "@") != 1 {
		return ""
	}
	if !strings.HasSuffix(email, "@gmail.com") && hostedDomain == "" {
		return ""
	}
	if role := a.roles[email]; role != "" {
		return role
	}
	_, domain, _ := strings.Cut(email, "@")
	if a.demoDomains[domain] && strings.EqualFold(hostedDomain, domain) {
		return "demo"
	}
	return ""
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return a != "" && b != "" && subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func cookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	maxAge := int(ttl.Seconds())
	if ttl < 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func reject(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func (a *Auth) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == a.config.Origin && a.config.Origin != ""
}
func (a *Auth) current(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || !time.Now().Before(s.Expires) || a.roleFor(s.User.Email, s.HostedDomain) != s.User.Role {
		delete(a.sessions, c.Value)
		return session{}, false
	}
	return s, true
}

// Role only accepts cookie-authenticated writes with both an exact Origin and
// the session's CSRF header. It never authenticates an API bearer credential.
func (a *Auth) Role(r *http.Request) string {
	s, ok := a.current(r)
	if !ok {
		return ""
	}
	if r.Method != "GET" && r.Method != "HEAD" && (!a.sameOrigin(r) || !equal(r.Header.Get("X-Warden-CSRF"), s.CSRF)) {
		return ""
	}
	return s.User.Role
}
func (a *Auth) cleanupLocked() {
	now := time.Now()
	for k, s := range a.sessions {
		if !now.Before(s.Expires) {
			delete(a.sessions, k)
		}
	}
	for k, c := range a.challenges {
		if !now.Before(c.Expires) {
			delete(a.challenges, k)
		}
	}
}
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/session", a.bootstrap)
	mux.HandleFunc("POST /auth/google", a.login)
	mux.HandleFunc("POST /auth/logout", a.logout)
	return mux
}
func (a *Auth) bootstrap(w http.ResponseWriter, r *http.Request) {
	if a.config.ClientID == "" {
		respond(w, 200, map[string]any{"enabled": false})
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		reject(w, 403, "same-origin request required")
		return
	}
	if s, ok := a.current(r); ok {
		respond(w, 200, map[string]any{"enabled": true, "user": s.User, "csrf": s.CSRF})
		return
	}
	a.mu.Lock()
	a.cleanupLocked()
	// Reuse a valid challenge so concurrent tabs and StrictMode do not invalidate
	// a sign-in already underway in this browser.
	if c, err := r.Cookie(challengeCookie); err == nil {
		if pending, ok := a.challenges[c.Value]; ok {
			a.mu.Unlock()
			respond(w, 200, map[string]any{"enabled": true, "client_id": a.config.ClientID, "nonce": pending.Nonce})
			return
		}
	}
	if len(a.challenges) >= maxEntries {
		a.mu.Unlock()
		reject(w, 429, "too many sign-in attempts; try again later")
		return
	}
	id, nonce := random(), random()
	a.challenges[id] = challenge{nonce, time.Now().Add(challengeTTL)}
	a.mu.Unlock()
	cookie(w, challengeCookie, id, challengeTTL)
	respond(w, 200, map[string]any{"enabled": true, "client_id": a.config.ClientID, "nonce": nonce})
}
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if a.config.ClientID == "" {
		reject(w, 503, "Google sign-in is not configured")
		return
	}
	if !a.sameOrigin(r) {
		reject(w, 403, "same-origin request required")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		reject(w, 415, "JSON required")
		return
	}
	var body struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.Credential == "" {
		reject(w, 400, "Google credential required")
		return
	}
	c, err := r.Cookie(challengeCookie)
	if err != nil {
		reject(w, 401, "sign-in expired; reload and try again")
		return
	}
	a.mu.Lock()
	pending, ok := a.challenges[c.Value]
	delete(a.challenges, c.Value)
	a.mu.Unlock()
	cookie(w, challengeCookie, "", -time.Second)
	if !ok || !time.Now().Before(pending.Expires) {
		reject(w, 401, "sign-in expired; reload and try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	token, err := a.verify(ctx, body.Credential)
	if err != nil || token.Subject == "" || !equal(token.Nonce, pending.Nonce) {
		reject(w, 401, "Google identity could not be verified; reload and try again")
		return
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Domain   string `json:"hd"`
		Name     string `json:"name"`
	}
	if err := token.Claims(&claims); err != nil || !claims.Verified {
		reject(w, 403, "a verified Google email is required")
		return
	}
	email := strings.ToLower(claims.Email)
	role := a.roleFor(email, claims.Domain)
	if role == "" || (!strings.HasSuffix(email, "@gmail.com") && claims.Domain == "") {
		reject(w, 403, "this Google account does not have access to the Warden")
		return
	}
	id := random()
	name := strings.TrimSpace(claims.Name)
	if len(name) > 120 {
		name = name[:120]
	}
	s := session{HostedDomain: claims.Domain, User: User{Subject: token.Subject, Email: email, Role: role, Name: name}, CSRF: random(), Expires: time.Now().Add(sessionTTL)}
	a.mu.Lock()
	a.cleanupLocked()
	if old, err := r.Cookie(sessionCookie); err == nil {
		delete(a.sessions, old.Value)
	}
	if len(a.sessions) >= maxEntries {
		a.mu.Unlock()
		reject(w, 429, "too many sessions; try again later")
		return
	}
	a.sessions[id] = s
	a.mu.Unlock()
	if a.OnLogin != nil {
		a.OnLogin(Login{User: s.User, HostedDomain: s.HostedDomain, At: time.Now()})
	}
	cookie(w, sessionCookie, id, sessionTTL)
	respond(w, 200, map[string]any{"enabled": true, "user": s.User, "csrf": s.CSRF})
}
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		reject(w, 403, "same-origin request required")
		return
	}
	if s, ok := a.current(r); ok {
		if !equal(r.Header.Get("X-Warden-CSRF"), s.CSRF) {
			reject(w, 403, "valid CSRF token required")
			return
		}
		c, _ := r.Cookie(sessionCookie)
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	cookie(w, sessionCookie, "", -time.Second)
	respond(w, 200, map[string]bool{"signed_out": true})
}

// SessionRef is an opaque host-only session reference, never sent to a preview.
// Identity reports the signed-in person behind r for attribution: the
// stable Google subject, the email and the display name ("" when Google
// gave none). ok is false without a live session.
func (a *Auth) Identity(r *http.Request) (principal, email, name string, ok bool) {
	s, ok := a.current(r)
	if !ok {
		return "", "", "", false
	}
	return s.User.Subject, s.User.Email, s.User.Name, true
}

func (a *Auth) SessionRef(r *http.Request) (string, bool) {
	if _, ok := a.current(r); !ok {
		return "", false
	}
	c, err := r.Cookie(sessionCookie)
	return c.Value, err == nil
}
func (a *Auth) ActiveSession(id string) bool {
	r := &http.Request{Header: http.Header{}}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	_, ok := a.current(r)
	return ok
}
