package edge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const ownerCookie = "warden-owner"
const ownerSessionTTL = 8 * time.Hour

// ownerAuth is the auth.mode "owner" authenticator: one person, identified
// by the launcher capability from endpoint.json exactly as warden-chat
// identifies them. API calls carry it as a bearer token. Because the preview
// ticket flow starts with a plain navigation that carries no header, a
// bearer-authenticated request also mints a host-only cookie session; that
// session is only ever accepted for reads and expires with the capability
// (the chat rotates it at every start), so a stale browser cannot outlive a
// restart.
//
// Only a browser can present a cookie back, and only browsers send fetch
// metadata, so a request without Sec-Fetch-Site (curl, the TUI, a test
// harness) mints nothing: each of those would otherwise hold a session for
// eight hours and, at ownerSessionBound, deny the web app its own. The
// bound evicts the oldest session rather than refusing.
//
// The cookie name carries the origin's port: browsers scope host-only
// cookies by host alone, so a port-forwarded cluster edge on
// 127.0.0.1:28781 next to a local `warden start` on 127.0.0.1:18781 would
// otherwise overwrite the other's cookie at every API call.
type ownerAuth struct {
	token    func() (string, error)
	secure   bool
	cookie   string
	mu       sync.Mutex
	sessions map[string]ownerSession
}
type ownerSession struct {
	Capability string // hash of the capability the session was minted from
	Minted     time.Time
	Expires    time.Time
}

const ownerSessionBound = 64

func newOwnerAuth(token func() (string, error), secure bool, cookie string) *ownerAuth {
	return &ownerAuth{token: token, secure: secure, cookie: cookie, sessions: map[string]ownerSession{}}
}

// current returns the hash of the live capability, or "" when the chat is
// not running (its endpoint file is absent or invalid).
func (a *ownerAuth) current() string {
	token, err := a.token()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func (a *ownerAuth) bearer(r *http.Request) bool {
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if presented == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	token, err := a.token()
	return err == nil && subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}
func (a *ownerAuth) cookieSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(a.cookie)
	if err != nil {
		return "", false
	}
	return c.Value, a.ActiveSession(c.Value)
}

// Role is "admin" for the capability holder. A cookie session authenticates
// reads only; writes must present the capability, so there is no CSRF surface.
func (a *ownerAuth) Role(r *http.Request) string {
	if a.bearer(r) {
		return "admin"
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		if _, ok := a.cookieSession(r); ok {
			return "admin"
		}
	}
	return ""
}

// Identity: a local install has one person, the owner.
func (a *ownerAuth) Identity(r *http.Request) (string, string, string, bool) {
	if a.Role(r) == "" {
		return "", "", "", false
	}
	return "owner", "", "", true
}

// SessionRef is the cookie session when present, else a reference to the
// capability itself for bearer requests.
func (a *ownerAuth) SessionRef(r *http.Request) (string, bool) {
	if key, ok := a.cookieSession(r); ok {
		return key, true
	}
	if a.bearer(r) {
		return "capability:" + a.current(), true
	}
	return "", false
}
func (a *ownerAuth) ActiveSession(id string) bool {
	live := a.current()
	if live == "" {
		return false
	}
	if strings.HasPrefix(id, "capability:") {
		return subtle.ConstantTimeCompare([]byte(id), []byte("capability:"+live)) == 1
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return false
	}
	if !time.Now().Before(s.Expires) || s.Capability != live {
		delete(a.sessions, id)
		return false
	}
	return true
}

// Establish mints the cookie session for a bearer-authenticated request that
// has none yet.
func (a *ownerAuth) Establish(w http.ResponseWriter, r *http.Request) {
	if !a.bearer(r) || r.Header.Get("Sec-Fetch-Site") == "" {
		return
	}
	if _, ok := a.cookieSession(r); ok {
		return
	}
	live := a.current()
	if live == "" {
		return
	}
	key := random()
	a.mu.Lock()
	now := time.Now()
	for k, s := range a.sessions {
		if !now.Before(s.Expires) || s.Capability != live {
			delete(a.sessions, k)
		}
	}
	for len(a.sessions) >= ownerSessionBound {
		oldest := ""
		for k, s := range a.sessions {
			if oldest == "" || s.Minted.Before(a.sessions[oldest].Minted) {
				oldest = k
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[key] = ownerSession{Capability: live, Minted: now, Expires: now.Add(ownerSessionTTL)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: a.cookie, Value: key, Path: "/", Secure: a.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(ownerSessionTTL / time.Second)})
}

// Handler serves the /auth/ surface the web app expects: sign-in is not a
// browser flow in owner mode, and logout drops the cookie session.
func (a *ownerAuth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":false}` + "\n"))
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(a.cookie); err == nil {
			a.mu.Lock()
			delete(a.sessions, c.Value)
			a.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: a.cookie, Value: "", Path: "/", Secure: a.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"signed_out":true}` + "\n"))
	})
	return mux
}
