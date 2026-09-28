package cloudauth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type passkeyUser struct {
	User
	Credentials []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { id, _ := hex.DecodeString(u.ID); return id }
func (u passkeyUser) WebAuthnName() string                       { return u.Email }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.Name }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

func (a *Auth) webauthn() (*webauthn.WebAuthn, error) {
	parsed, err := url.Parse(a.Origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("passkeys require an HTTPS origin")
	}
	// PostgreSQL rejects expired challenges, so WebAuthn must populate Expires
	// for both ceremonies (the library defaults to client-only timeouts).
	timeout := webauthn.TimeoutConfig{Enforce: true, Timeout: 5 * time.Minute, TimeoutUVD: 5 * time.Minute}
	return webauthn.New(&webauthn.Config{
		RPID: parsed.Hostname(), RPDisplayName: "Warden", RPOrigins: []string{a.Origin},
		Timeouts: webauthn.TimeoutsConfig{Registration: timeout, Login: timeout},
	})
}

func (s *Store) passkeyUser(ctx context.Context, id string) (passkeyUser, error) {
	u, err := s.User(ctx, id)
	if err != nil || u.Disabled {
		return passkeyUser{}, errors.New("account unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT credential FROM warden_cloud.passkeys WHERE user_id=$1`, id)
	if err != nil {
		return passkeyUser{}, err
	}
	defer rows.Close()
	out := passkeyUser{User: u, Credentials: []webauthn.Credential{}}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return passkeyUser{}, err
		}
		var credential webauthn.Credential
		if err = json.Unmarshal(data, &credential); err != nil {
			return passkeyUser{}, err
		}
		out.Credentials = append(out.Credentials, credential)
	}
	return out, rows.Err()
}

func (s *Store) passkeyChallenge(ctx context.Context, userID, kind string, session *webauthn.SessionData) (string, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	id := ID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO warden_cloud.passkey_challenges(id,user_id,kind,session,expires_at) VALUES($1,$2,$3,$4,$5)`, id, userID, kind, data, session.Expires)
	return id, err
}

type passkeySession struct {
	webauthn.SessionData
	Name          string
	ReplacementID []byte
}

func (s *Store) takePasskeyChallenge(ctx context.Context, id, kind string) (string, passkeySession, error) {
	var userID string
	var data []byte
	var session passkeySession
	err := s.db.QueryRowContext(ctx, `DELETE FROM warden_cloud.passkey_challenges WHERE id=$1 AND kind=$2 AND expires_at>now() RETURNING user_id,session,name,replacement_id`, id, kind).Scan(&userID, &data, &session.Name, &session.ReplacementID)
	if err != nil {
		return "", passkeySession{}, err
	}
	err = json.Unmarshal(data, &session.SessionData)
	return userID, session, err
}
func credentialRequest(r *http.Request, raw json.RawMessage) *http.Request {
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(raw))
	return copy
}

func (s *Store) newPasskeySession(ctx context.Context, user User) (Session, string, error) {
	if user.Disabled {
		return Session{}, "", errors.New("account unavailable")
	}
	orgs, err := s.Organizations(ctx, user.ID, false)
	if err != nil {
		return Session{}, "", err
	}
	if len(orgs) == 0 && !user.FullAdmin {
		return Session{}, "", errors.New("account unavailable")
	}
	session := Session{User: user, CSRF: randomSecret(), Expires: s.now().Add(sessionLifetime)}
	var org any
	if len(orgs) == 1 {
		session.OrganizationID = orgs[0].ID
		org = session.OrganizationID
	}
	raw := randomSecret()
	session.Digest = hashSecret(raw)
	_, err = s.db.ExecContext(ctx, `INSERT INTO warden_cloud.sessions(digest,user_id,organization_id,csrf,expires_at) VALUES($1,$2,$3,$4,$5)`, session.Digest, user.ID, org, session.CSRF, session.Expires)
	return session, raw, err
}

func (a *Auth) passkeyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /auth/passkeys/{id}", a.managePasskey)
	mux.HandleFunc("PATCH /auth/passkeys/{id}", a.managePasskey)
	mux.HandleFunc("POST /auth/passkeys/register/start", func(w http.ResponseWriter, r *http.Request) {
		s, err := a.current(r)
		if err != nil || !a.csrf(r, s) {
			respond(w, 403, map[string]string{"error": "sign in first"})
			return
		}
		input := struct {
			Name      string `json:"name"`
			ReplaceID string `json:"replaceID"`
		}{Name: "Passkey"}
		// Older open clients sent an empty body; keep that enrollment working.
		if r.ContentLength != 0 && decode(r, &input) != nil {
			respond(w, 400, map[string]string{"error": "invalid passkey settings"})
			return
		}
		name, err := ValidName(input.Name)
		if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		wa, err := a.webauthn()
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		u, err := a.Login.Store.passkeyUser(r.Context(), s.User.ID)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		options, session, err := wa.BeginRegistration(u)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		id, err := a.Login.Store.registrationChallenge(r.Context(), u.ID, name, input.ReplaceID, session)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		respond(w, 200, map[string]any{"id": id, "options": options})
	})
	mux.HandleFunc("POST /auth/passkeys/register/finish", func(w http.ResponseWriter, r *http.Request) {
		s, err := a.current(r)
		if err != nil || !a.csrf(r, s) {
			respond(w, 403, map[string]string{"error": "sign in first"})
			return
		}
		var input struct {
			ID         string          `json:"id"`
			Credential json.RawMessage `json:"credential"`
		}
		if decodeLimit(r, &input, 256<<10) != nil {
			respond(w, 400, map[string]string{"error": "invalid passkey"})
			return
		}
		userID, session, err := a.Login.Store.takePasskeyChallenge(r.Context(), input.ID, "register")
		if err != nil || userID != s.User.ID {
			respond(w, 403, map[string]string{"error": "passkey setup expired"})
			return
		}
		u, err := a.Login.Store.passkeyUser(r.Context(), userID)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		wa, err := a.webauthn()
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		credential, err := wa.FinishRegistration(u, session.SessionData, credentialRequest(r, input.Credential))
		if err != nil {
			respond(w, 400, map[string]string{"error": "passkey verification failed"})
			return
		}
		err = a.Login.Store.savePasskey(r.Context(), userID, session.Name, session.ReplacementID, credential)
		if err != nil {
			respond(w, 503, map[string]string{"error": "unable to save passkey"})
			return
		}
		respond(w, 200, map[string]string{"status": "passkey saved"})
	})
	mux.HandleFunc("POST /auth/passkeys/login/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != a.Origin {
			respond(w, 403, map[string]string{"error": "origin refused"})
			return
		}
		var input struct {
			Email string `json:"email"`
		}
		if decodeLimit(r, &input, 256<<10) != nil {
			respond(w, 400, map[string]string{"error": "enter a valid email"})
			return
		}
		email, err := NormalizeEmail(input.Email)
		if err != nil {
			respond(w, 400, map[string]string{"error": "enter a valid email"})
			return
		}
		user, err := a.Login.Store.UserByEmail(r.Context(), email)
		if err != nil {
			respond(w, 404, map[string]string{"error": "passkey unavailable for this account"})
			return
		}
		u, err := a.Login.Store.passkeyUser(r.Context(), user.ID)
		if err != nil || len(u.Credentials) == 0 {
			respond(w, 404, map[string]string{"error": "passkey unavailable for this account"})
			return
		}
		wa, err := a.webauthn()
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		options, session, err := wa.BeginLogin(u)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		id, err := a.Login.Store.passkeyChallenge(r.Context(), u.ID, "login", session)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		respond(w, 200, map[string]any{"id": id, "options": options})
	})
	mux.HandleFunc("POST /auth/passkeys/login/finish", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != a.Origin {
			respond(w, 403, map[string]string{"error": "origin refused"})
			return
		}
		var input struct {
			ID         string          `json:"id"`
			Credential json.RawMessage `json:"credential"`
		}
		if decode(r, &input) != nil {
			respond(w, 400, map[string]string{"error": "invalid passkey"})
			return
		}
		userID, session, err := a.Login.Store.takePasskeyChallenge(r.Context(), input.ID, "login")
		if err != nil {
			respond(w, 401, map[string]string{"error": "passkey login expired"})
			return
		}
		u, err := a.Login.Store.passkeyUser(r.Context(), userID)
		if err != nil {
			respond(w, 401, map[string]string{"error": "passkey login unavailable"})
			return
		}
		wa, err := a.webauthn()
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		credential, err := wa.FinishLogin(u, session.SessionData, credentialRequest(r, input.Credential))
		if err != nil {
			respond(w, 401, map[string]string{"error": "passkey verification failed"})
			return
		}
		data, _ := json.Marshal(credential)
		result, err := a.Login.Store.db.ExecContext(r.Context(), `UPDATE warden_cloud.passkeys SET credential=$1,last_used_at=$2 WHERE credential_id=$3 AND user_id=$4`, data, time.Now(), credential.ID, userID)
		if err != nil {
			respond(w, 503, map[string]string{"error": "passkeys unavailable"})
			return
		}
		if n, _ := result.RowsAffected(); n != 1 {
			respond(w, 401, map[string]string{"error": "passkey unavailable"})
			return
		}
		s, raw, err := a.Login.Store.newPasskeySession(r.Context(), u.User)
		if err != nil {
			respond(w, 401, map[string]string{"error": "account unavailable"})
			return
		}
		a.cookie(w, raw, int(time.Until(s.Expires).Seconds()))
		respond(w, 200, map[string]string{"status": "signed in"})
	})
	return mux
}
