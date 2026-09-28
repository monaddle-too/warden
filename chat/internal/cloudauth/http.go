package cloudauth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"warden/chat/internal/recordings"
)

const sessionCookie = "__Host-warden-session"

// Auth is the public cloud's browser authenticator. The organization selected
// by the session is rechecked against PostgreSQL on every request, so removing
// a member also closes an already-open preview or event stream.
type Auth struct {
	Recordings  *recordings.Service
	Login       *Login
	Origin      string
	DocsAddress string
	DocsKey     string
}

func (a *Auth) current(r *http.Request) (Session, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, err
	}
	return a.Login.Store.Session(r.Context(), c.Value)
}
func (a *Auth) Role(r *http.Request) string {
	s, err := a.current(r)
	if err != nil {
		return ""
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.csrf(r, s) {
		return ""
	}
	if s.User.FullAdmin {
		return "admin"
	}
	if s.OrganizationID == "" {
		return ""
	}
	return "user"
}
func (a *Auth) Identity(r *http.Request) (principal, email, name string, ok bool) {
	s, err := a.current(r)
	if err != nil || !s.User.FullAdmin && s.OrganizationID == "" {
		return "", "", "", false
	}
	return s.User.ID, s.User.Email, s.User.Name, true
}
func (a *Auth) Organization(r *http.Request) string {
	s, err := a.current(r)
	if err != nil {
		return ""
	}
	return s.OrganizationID
}
func (a *Auth) SessionRef(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	if _, err = a.Login.Store.Session(r.Context(), c.Value); err != nil {
		return "", false
	}
	return c.Value, true
}
func (a *Auth) ActiveSession(raw string) bool {
	_, err := a.Login.Store.Session(context.Background(), raw)
	return err == nil
}
func (a *Auth) ActiveSessionInOrganization(raw, organization string) bool {
	s, err := a.Login.Store.Session(context.Background(), raw)
	return err == nil && s.OrganizationID == organization
}
func (a *Auth) csrf(r *http.Request, s Session) bool {
	return r.Header.Get("Origin") == a.Origin && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Warden-CSRF")), []byte(s.CSRF)) == 1
}
func (a *Auth) cookie(w http.ResponseWriter, raw string, age int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", MaxAge: age, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decode(r *http.Request, value any) error {
	return decodeLimit(r, value, 16<<10)
}
func decodeLimit(r *http.Request, value any, limit int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return errors.New("one JSON object required")
	}
	return nil
}
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/agent-authorization/", a.agentAuthorization)
	mux.HandleFunc("/auth/agent-connections", a.agentConnections)
	mux.HandleFunc("/auth/agent-connections/", a.agentConnections)
	mux.HandleFunc("/auth/shared-conversations", a.sharedConversations)
	mux.HandleFunc("/auth/shared-conversations/", a.sharedConversations)
	mux.Handle("/auth/passkeys/", a.passkeyHandler())
	mux.Handle("/auth/profile", a.profileHandler())
	mux.Handle("/auth/profile/", a.profileHandler())
	mux.HandleFunc("GET /auth/session", func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != a.Origin {
			respond(w, 403, map[string]string{"error": "origin refused"})
			return
		}
		s, err := a.current(r)
		if err != nil {
			respond(w, 200, map[string]any{"enabled": true, "mode": "email", "authenticated": false})
			return
		}
		orgs, err := a.Login.Store.Organizations(r.Context(), s.User.ID, s.User.FullAdmin)
		if err != nil {
			respond(w, 503, map[string]string{"error": "sign-in unavailable"})
			return
		}
		offerPasskey, err := a.Login.Store.offerPasskey(r.Context(), s.User.ID)
		if err != nil {
			respond(w, 503, map[string]string{"error": "account unavailable"})
			return
		}
		role := "user"
		if s.User.FullAdmin {
			role = "admin"
		}
		respond(w, 200, map[string]any{"enabled": true, "mode": "email", "authenticated": true, "email": s.User.Email, "name": s.User.Name, "user": map[string]any{"sub": s.User.ID, "email": s.User.Email, "name": s.User.Name, "role": role}, "fullAdmin": s.User.FullAdmin, "organizationId": s.OrganizationID, "organizationRole": s.Role, "organizations": orgs, "csrf": s.CSRF, "offerPasskey": offerPasskey})
	})
	mux.HandleFunc("POST /auth/email/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != a.Origin {
			respond(w, 403, map[string]string{"error": "origin refused"})
			return
		}
		var input struct {
			Email string `json:"email"`
		}
		if err := decode(r, &input); err != nil {
			respond(w, 400, map[string]string{"error": "invalid email request"})
			return
		}
		if err := a.Login.IssueCode(r.Context(), input.Email, r.RemoteAddr); err != nil {
			if strings.Contains(err.Error(), "too many") {
				respond(w, 429, map[string]string{"error": "try again later"})
				return
			}
			if _, e := NormalizeEmail(input.Email); e != nil {
				respond(w, 400, map[string]string{"error": "enter a valid email"})
				return
			}
			respond(w, 503, map[string]string{"error": "unable to send a code right now"})
			return
		}
		respond(w, 200, map[string]string{"status": "if the account is active, a code has been sent"})
	})
	mux.HandleFunc("POST /auth/email/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != a.Origin {
			respond(w, 403, map[string]string{"error": "origin refused"})
			return
		}
		var input struct {
			Email string `json:"email"`
			Code  string `json:"code"`
		}
		if err := decode(r, &input); err != nil {
			respond(w, 400, map[string]string{"error": "invalid code request"})
			return
		}
		s, raw, err := a.Login.VerifyCode(r.Context(), input.Email, input.Code)
		if err != nil {
			respond(w, 401, map[string]string{"error": "invalid or expired code"})
			return
		}
		a.cookie(w, raw, int(time.Until(s.Expires).Seconds()))
		respond(w, 200, map[string]string{"status": "signed in"})
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		s, err := a.current(r)
		if err != nil {
			respond(w, 401, map[string]string{"error": "sign in again"})
			return
		}
		if !a.csrf(r, s) {
			respond(w, 403, map[string]string{"error": "reload before signing out"})
			return
		}
		if err = a.Login.Store.Logout(r.Context(), s); err != nil {
			respond(w, 503, map[string]string{"error": "sign-out unavailable"})
			return
		}
		a.cookie(w, "", -1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /auth/organizations/select", func(w http.ResponseWriter, r *http.Request) {
		s, err := a.current(r)
		if err != nil {
			respond(w, 401, map[string]string{"error": "sign in again"})
			return
		}
		if !a.csrf(r, s) {
			respond(w, 403, map[string]string{"error": "reload before changing organizations"})
			return
		}
		var input struct {
			ID string `json:"id"`
		}
		if err := decode(r, &input); err != nil {
			respond(w, 400, map[string]string{"error": "invalid organization"})
			return
		}
		if err = a.Login.Store.SelectOrganization(r.Context(), s, input.ID); err != nil {
			respond(w, 403, map[string]string{"error": "organization unavailable"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Admin handles organization membership separately from the legacy global
// admin console. An organization admin may manage users but cannot grant the
// admin role or alter provider credentials.
func (a *Auth) Admin(w http.ResponseWriter, r *http.Request) {
	s, err := a.current(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "sign in required"})
		return
	}
	if r.Method != "GET" && !a.csrf(r, s) {
		respond(w, 403, map[string]string{"error": "reload before changing organizations"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/organizations")
	switch {
	case path == "" && r.Method == "GET":
		orgs, err := a.Login.Store.Organizations(r.Context(), s.User.ID, s.User.FullAdmin)
		if err != nil {
			respond(w, 503, map[string]string{"error": "organizations unavailable"})
			return
		}
		respond(w, 200, orgs)
	case path == "" && r.Method == "POST":
		var input struct {
			Name string `json:"name"`
		}
		if err := decode(r, &input); err != nil {
			respond(w, 400, map[string]string{"error": "invalid organization"})
			return
		}
		org, err := a.Login.Store.CreateOrganization(r.Context(), s.User, input.Name)
		if err != nil {
			respond(w, 403, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 201, org)
	case strings.HasSuffix(path, "/members"):
		org := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/members")
		if org == "" || strings.Contains(org, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method == "GET" {
			members, err := a.Login.Store.Members(r.Context(), s.User, org)
			if err != nil {
				respond(w, 403, map[string]string{"error": "organization unavailable"})
				return
			}
			respond(w, 200, members)
			return
		}
		if r.Method == "POST" {
			var input struct {
				Email string `json:"email"`
				Name  string `json:"name"`
				Role  string `json:"role"`
			}
			if err := decode(r, &input); err != nil {
				respond(w, 400, map[string]string{"error": "invalid member"})
				return
			}
			member, err := a.Login.Store.PutMember(r.Context(), s.User, org, input.Email, input.Name, input.Role)
			if err != nil {
				respond(w, 403, map[string]string{"error": err.Error()})
				return
			}
			var organizationName string
			sent := false
			if a.Login.Store.db.QueryRowContext(r.Context(), `SELECT name FROM warden_cloud.organizations WHERE id=$1`, org).Scan(&organizationName) == nil {
				if mailer, ok := a.Login.Mailer.(interface {
					SendInvitation(context.Context, string, string, string, string) error
				}); ok {
					sent = mailer.SendInvitation(r.Context(), member.Email, organizationName, member.Role, a.Origin) == nil
				}
			}
			respond(w, 200, struct {
				Member
				InvitationSent bool `json:"invitationSent"`
			}{member, sent})
			return
		}
		if r.Method == "DELETE" {
			var input struct {
				Email string `json:"email"`
			}
			if err := decode(r, &input); err != nil {
				respond(w, 400, map[string]string{"error": "invalid member"})
				return
			}
			err := a.Login.Store.RemoveMember(r.Context(), s.User, org, input.Email)
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			if err != nil {
				respond(w, 403, map[string]string{"error": "unable to remove member"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "method not allowed", 405)
	default:
		http.NotFound(w, r)
	}
}
