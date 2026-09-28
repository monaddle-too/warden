package edge

import (
	"context"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

func docsPath(path string) bool {
	for _, prefix := range []string{"/docs-app", "/docs-assets", "/api/workspace", "/api/nodes", "/api/documents", "/api/designs", "/api/design-assets", "/api/tokens", "/collab"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// docsProxy is the sole browser ingress to the private Panta Docs/Design
// service. The service key is never given to a browser, and every request is
// bound to the current database-backed organization selection. Cancellation
// also closes a collaboration socket after membership is revoked.
func (s *Server) docsProxy(w http.ResponseWriter, r *http.Request) {
	if s.docsTarget == nil {
		http.Error(w, "documents unavailable", http.StatusServiceUnavailable)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.Config.Origin {
		http.Error(w, "origin refused", http.StatusForbidden)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && r.Header.Get("Origin") != s.Config.Origin {
		http.Error(w, "origin required for collaboration", http.StatusForbidden)
		return
	}
	if s.Auth.Role(r) == "" {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	scoped, ok := s.Auth.(interface {
		Organization(*http.Request) string
		ActiveSessionInOrganization(string, string) bool
	})
	if !ok {
		http.Error(w, "organization unavailable", http.StatusForbidden)
		return
	}
	organization := scoped.Organization(r)
	principal, email, name, known := s.Auth.Identity(r)
	parent, sessionOK := s.Auth.SessionRef(r)
	if !known || !sessionOK || organization == "" || principal == "" || email == "" {
		http.Error(w, "choose an organization", http.StatusForbidden)
		return
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
				if !scoped.ActiveSessionInOrganization(parent, organization) {
					cancel()
					return
				}
			}
		}
	}()
	proxy := httputil.NewSingleHostReverseProxy(s.docsTarget)
	proxy.Transport = s.upstream
	proxy.Director = func(req *http.Request) {
		for key := range req.Header {
			lower := strings.ToLower(key)
			if strings.HasPrefix(lower, "x-panta-") || strings.HasPrefix(lower, "x-warden-") || strings.HasPrefix(lower, "x-forwarded-") {
				req.Header.Del(key)
			}
		}
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		req.Header.Del("Forwarded")
		req.Header.Set("X-Panta-Key", s.Config.DocsKey)
		req.Header.Set("X-Panta-Subject", principal)
		req.Header.Set("X-Panta-Email", email)
		req.Header.Set("X-Panta-Name", name)
		req.Header.Set("X-Panta-Organization", organization)
		if strings.HasPrefix(req.URL.Path, "/docs-app/") {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/docs-app")
			req.URL.RawPath = ""
		}
		req.URL.Scheme = s.docsTarget.Scheme
		req.URL.Host = s.docsTarget.Host
		req.Host = s.docsTarget.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "documents unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}
