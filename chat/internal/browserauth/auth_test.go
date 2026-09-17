package browserauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

const origin = "https://siem.example.com"

func testAuth(t *testing.T) (*Auth, jose.Signer) {
	t.Helper()
	a, err := New(Config{ClientID: "siem-client", Origin: origin, AdminEmails: "admin@gmail.com", ReadEmails: "reader@gmail.com"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(server.Close)
	a.verify = oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(context.Background(), server.URL), &oidc.Config{ClientID: "siem-client", SupportedSigningAlgs: []string{oidc.RS256}}).Verify
	return a, signer
}
func bootstrap(t *testing.T, a *Auth) (*http.Cookie, string) {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", origin+"/auth/session", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var body struct{ Nonce string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return w.Result().Cookies()[0], body.Nonce
}
func signed(t *testing.T, signer jose.Signer, nonce string, changes map[string]any) string {
	t.Helper()
	claims := map[string]any{"iss": "https://accounts.google.com", "aud": "siem-client", "sub": "google-subject", "email": "admin@gmail.com", "email_verified": true, "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range changes {
		claims[k] = v
	}
	data, _ := json.Marshal(claims)
	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	token, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func login(a *Auth, c *http.Cookie, token, requestOrigin string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"credential": token})
	r := httptest.NewRequest("POST", origin+"/auth/google", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", requestOrigin)
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestGoogleIdentityValidation(t *testing.T) {
	a, signer := testAuth(t)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		status int
	}{
		{"allowed", nil, 200}, {"reader", map[string]any{"email": "reader@gmail.com"}, 200},
		{"wrong audience", map[string]any{"aud": "other-client"}, 401}, {"wrong issuer", map[string]any{"iss": "https://evil.example"}, 401},
		{"expired", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, 401}, {"missing subject", map[string]any{"sub": ""}, 401},
		{"wrong nonce", map[string]any{"nonce": "wrong"}, 401}, {"unverified", map[string]any{"email_verified": false}, 403},
		{"unlisted", map[string]any{"email": "stranger@gmail.com"}, 403}, {"lookalike", map[string]any{"email": "admin@gmail.com.evil.example"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, nonce := bootstrap(t, a)
			w := login(a, c, signed(t, signer, nonce, tc.claims), origin)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("forged signature", func(t *testing.T) {
		_, other := testAuth(t)
		c, nonce := bootstrap(t, a)
		if w := login(a, c, signed(t, other, nonce, nil), origin); w.Code != 401 {
			t.Fatal(w.Code)
		}
	})
}
func TestSessionLifecycleAndCSRF(t *testing.T) {
	a, signer := testAuth(t)
	c, nonce := bootstrap(t, a)
	token := signed(t, signer, nonce, nil)
	for _, badOrigin := range []string{"", "https://evil.example", "https://sub.siem.example.com"} {
		if w := login(a, c, token, badOrigin); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := login(a, nil, token, origin); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := login(a, c, token, origin)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var body struct {
		User User
		CSRF string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.User.Subject != "google-subject" || body.User.Role != "admin" || body.CSRF == "" {
		t.Fatal(body)
	}
	var sc *http.Cookie
	for _, v := range w.Result().Cookies() {
		if v.Name == sessionCookie {
			sc = v
		}
	}
	if sc == nil || !sc.Secure || !sc.HttpOnly || sc.Path != "/" || sc.Domain != "" || sc.SameSite != http.SameSiteLaxMode || sc.MaxAge != 28800 {
		t.Fatal(sc)
	}
	if w := login(a, c, token, origin); w.Code != 401 {
		t.Fatal("replay accepted")
	}
	r := httptest.NewRequest("GET", origin+"/api/v1/status", nil)
	r.AddCookie(sc)
	if a.Role(r) != "admin" {
		t.Fatal("session rejected")
	}
	w = httptest.NewRecorder()
	r.URL.Path = "/auth/session"
	a.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), body.CSRF) {
		t.Fatal("session not restored")
	}
	r.Method = "POST"
	r.URL.Path = "/api/v1/events"
	if a.Role(r) != "" {
		t.Fatal("missing CSRF accepted")
	}
	r.Header.Set("Origin", origin)
	if a.Role(r) != "" {
		t.Fatal("missing token accepted")
	}
	r.Header.Set("X-Warden-CSRF", body.CSRF)
	if a.Role(r) != "admin" {
		t.Fatal("legitimate write rejected")
	}
	r.Header.Set("Origin", "https://evil.example")
	if a.Role(r) != "" {
		t.Fatal("cross-origin write accepted")
	}
	r.URL.Path = "/auth/logout"
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	r.Header.Set("Origin", origin)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || a.Role(r) != "" {
		t.Fatal("logout failed")
	}
}
func TestExpiredAndRevokedSessions(t *testing.T) {
	a, _ := testAuth(t)
	r := httptest.NewRequest("GET", origin+"/api/v1/status", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session"})
	a.sessions["session"] = session{User: User{Subject: "sub", Email: "admin@gmail.com", Role: "admin"}, Expires: time.Now().Add(-time.Second)}
	if a.Role(r) != "" {
		t.Fatal("expired accepted")
	}
	a.sessions["session"] = session{User: User{Subject: "sub", Email: "removed@gmail.com", Role: "admin"}, Expires: time.Now().Add(time.Hour)}
	if a.Role(r) != "" {
		t.Fatal("removed account accepted")
	}
	c, _ := bootstrap(t, a)
	a.challenges[c.Value] = challenge{Nonce: "n", Expires: time.Now().Add(-time.Second)}
	if w := login(a, c, "anything", origin); w.Code != 401 {
		t.Fatal("expired challenge accepted")
	}
}
func TestConfigurationFailsClosed(t *testing.T) {
	for _, c := range []Config{{Origin: origin}, {ClientID: "id"}, {ClientID: "id", Origin: "http://siem.example.com", AdminEmails: "admin@gmail.com"}, {ClientID: "id", Origin: origin}, {ClientID: "id", Origin: origin, AdminEmails: "*@gmail.com"}} {
		if _, err := New(c); err == nil {
			t.Fatalf("accepted %#v", c)
		}
	}
	a, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/auth/session", nil))
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal(w.Body.String())
	}
}

func TestDemoDomainRequiresVerifiedMatchingWorkspace(t *testing.T) {
	a, signer := testAuth(t)
	a.demoDomains["digitalasset.com"] = true
	for _, tc := range []struct {
		email, domain string
		verified      bool
		code          int
	}{
		{"employee@digitalasset.com", "digitalasset.com", true, 200},
		{"employee@digitalasset.com", "", true, 403},
		{"employee@digitalasset.com", "other.com", true, 403},
		{"employee@digitalasset.com.evil.com", "digitalasset.com", true, 403},
		{"employee@digitalasset.com", "digitalasset.com", false, 403},
		{"outsider@gmail.com", "", true, 403},
	} {
		c, n := bootstrap(t, a)
		w := login(a, c, signed(t, signer, n, map[string]any{"email": tc.email, "hd": tc.domain, "email_verified": tc.verified}), origin)
		if w.Code != tc.code {
			t.Fatalf("%s/%s: %d %s", tc.email, tc.domain, w.Code, w.Body.String())
		}
		if tc.code == 200 {
			var body struct {
				User User
				CSRF string
			}
			json.Unmarshal(w.Body.Bytes(), &body)
			if body.User.Role != "demo" {
				t.Fatal(body.User)
			}
			var sc *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == sessionCookie {
					sc = c
				}
			}
			r := httptest.NewRequest("POST", origin+"/api/sharing/select", nil)
			r.AddCookie(sc)
			r.Header.Set("Origin", origin)
			r.Header.Set("X-Warden-CSRF", body.CSRF)
			if a.Role(r) != "demo" {
				t.Fatal("demo session cannot perform authenticated writes")
			}
			r.Header.Del("X-Warden-CSRF")
			if a.Role(r) != "" {
				t.Fatal("write without CSRF accepted")
			}
			delete(a.demoDomains, "digitalasset.com")
			if a.ActiveSession(sc.Value) {
				t.Fatal("removed domain session remained active")
			}
			a.demoDomains["digitalasset.com"] = true
		}
	}
}
func TestDemoDomainConfig(t *testing.T) {
	for _, d := range []string{"*", "*.digitalasset.com", "digitalasset.com/", "@digitalasset.com", "digitalasset..com"} {
		if _, err := New(Config{ClientID: "client", Origin: origin, DemoDomains: d}); err == nil {
			t.Fatalf("accepted %q", d)
		}
	}
	if _, err := New(Config{ClientID: "client", Origin: origin, DemoDomains: "digitalasset.com"}); err != nil {
		t.Fatal(err)
	}
}
func TestOnLoginObservesOnlyAdmittedSignIns(t *testing.T) {
	a, signer := testAuth(t)
	a.demoDomains["digitalasset.com"] = true
	var seen []Login
	a.OnLogin = func(l Login) { seen = append(seen, l) }
	for _, tc := range []struct {
		claims map[string]any
		code   int
	}{
		{map[string]any{"email": "Admin@gmail.com"}, 200},
		{map[string]any{"email": "stranger@gmail.com"}, 403},
		{map[string]any{"email": "employee@digitalasset.com", "hd": "digitalasset.com", "sub": "employee-subject"}, 200},
		{map[string]any{"email": "employee@digitalasset.com", "hd": "", "sub": "employee-subject"}, 403},
	} {
		c, n := bootstrap(t, a)
		if w := login(a, c, signed(t, signer, n, tc.claims), origin); w.Code != tc.code {
			t.Fatalf("%v: %d %s", tc.claims, w.Code, w.Body.String())
		}
	}
	if len(seen) != 2 || seen[0].Email != "admin@gmail.com" || seen[0].Role != "admin" || seen[0].HostedDomain != "" || seen[0].At.IsZero() {
		t.Fatalf("unexpected owner login record: %+v", seen)
	}
	if seen[1].Email != "employee@digitalasset.com" || seen[1].Role != "demo" || seen[1].HostedDomain != "digitalasset.com" || seen[1].Subject != "employee-subject" {
		t.Fatalf("unexpected demo login record: %+v", seen[1])
	}
}

// The display name from the ID token rides along in the session for
// attribution; a missing one is simply absent.
func TestSessionCarriesGoogleName(t *testing.T) {
	a, signer := testAuth(t)
	c, nonce := bootstrap(t, a)
	w := login(a, c, signed(t, signer, nonce, map[string]any{"name": "  Ada Lovelace  "}), origin)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		User User `json:"user"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.User.Name != "Ada Lovelace" || body.User.Email != "admin@gmail.com" {
		t.Fatalf("user: %+v", body.User)
	}
	r := httptest.NewRequest("GET", origin+"/auth/session", nil)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			r.AddCookie(c)
		}
	}
	if principal, email, name, ok := a.Identity(r); !ok || principal != "google-subject" || email != "admin@gmail.com" || name != "Ada Lovelace" {
		t.Fatalf("identity: %q %q %q %v", principal, email, name, ok)
	}
}
