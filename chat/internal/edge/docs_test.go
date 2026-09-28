package edge

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type docsAuth struct{ *fakeAuth }

func (a docsAuth) Organization(r *http.Request) string {
	if _, ok := a.SessionRef(r); ok {
		return strings.Repeat("a", 32)
	}
	return ""
}
func (a docsAuth) ActiveSessionInOrganization(raw, org string) bool {
	return a.ActiveSession(raw) && org == strings.Repeat("a", 32)
}

func TestDocsProxyBindsIdentityAndOrganization(t *testing.T) {
	secret := strings.Repeat("s", 64)
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panta-Key") != secret || r.Header.Get("X-Panta-Subject") != "google-subject" || r.Header.Get("X-Panta-Email") != "owner@gmail.com" || r.Header.Get("X-Panta-Organization") != strings.Repeat("a", 32) {
			t.Errorf("wrong trusted headers: %v", r.Header)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("browser credentials forwarded to document service")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer docs.Close()
	s, auth := testServer(t, http.NotFoundHandler())
	s.Config.Mode = ModeEmail
	s.Config.DocsKey = secret
	s.docsTarget, _ = url.Parse(docs.URL)
	s.Auth = docsAuth{auth}
	request := func(origin string, signedIn bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://warden.example.com/api/documents/a", nil)
		r.Header.Set("X-Panta-Organization", strings.Repeat("b", 32))
		r.Header.Set("Authorization", "Bearer forged")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if signedIn {
			r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if got := request("", true).Code; got != http.StatusNoContent {
		t.Fatalf("signed-in document read: %d", got)
	}
	if got := request("", false).Code; got != http.StatusUnauthorized {
		t.Fatalf("unsigned document read: %d", got)
	}
	if got := request("https://other.example.com", true).Code; got != http.StatusForbidden {
		t.Fatalf("cross-origin document read: %d", got)
	}
}

func TestDocumentsKeepWardenShellAndEmbedOnlyPrivateEditor(t *testing.T) {
	s, auth := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("shell path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte("warden shell"))
	}))
	s.Config.Mode = ModeEmail
	s.Auth = docsAuth{auth}
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/documents/a" {
			t.Errorf("editor path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte("private editor"))
	}))
	defer docs.Close()
	s.docsTarget, _ = url.Parse(docs.URL)
	for _, path := range []string{"/documents", "/documents/a", "/designs/a", "/docs-app/documents/a"} {
		r := httptest.NewRequest("GET", "https://warden.example.com"+path, nil)
		r.AddCookie(&http.Cookie{Name: "main", Value: "valid"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		csp := w.Header().Get("Content-Security-Policy")
		if strings.HasPrefix(path, "/docs-app/") {
			if w.Body.String() != "private editor" || !strings.Contains(csp, "frame-ancestors 'self'") {
				t.Fatal("editor not embeddable")
			}
		} else if w.Body.String() != "warden shell" || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatal("missing protected Warden shell")
		}
		if !strings.Contains(csp, "frame-src 'self'") {
			t.Fatal("editor frame blocked")
		}
	}
}
