package cloudauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type passkeyInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (s *Store) offerPasskey(ctx context.Context, userID string) (bool, error) {
	var offer bool
	err := s.db.QueryRowContext(ctx, `SELECT NOT passkey_prompt_seen AND NOT EXISTS(SELECT 1 FROM warden_cloud.passkeys WHERE user_id=$1) FROM warden_cloud.users WHERE id=$1`, userID).Scan(&offer)
	return offer, err
}
func (s *Store) passkeys(ctx context.Context, userID string) ([]passkeyInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id,name,created_at,last_used_at FROM warden_cloud.passkeys WHERE user_id=$1 ORDER BY created_at,credential_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []passkeyInfo{}
	for rows.Next() {
		var p passkeyInfo
		var id []byte
		if err := rows.Scan(&id, &p.Name, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, err
		}
		p.ID = base64.RawURLEncoding.EncodeToString(id)
		result = append(result, p)
	}
	return result, rows.Err()
}

// Never remove the old key until the new credential has been verified. Both
// changes commit together, and a concurrent removal makes replacement fail.
func (s *Store) savePasskey(ctx context.Context, userID, name string, replacement []byte, credential *webauthn.Credential) error {
	data, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(replacement) != 0 {
		result, err := tx.ExecContext(ctx, `DELETE FROM warden_cloud.passkeys WHERE user_id=$1 AND credential_id=$2`, userID, replacement)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.passkeys(credential_id,user_id,credential,name) VALUES($1,$2,$3,$4)`, credential.ID, userID, data, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE warden_cloud.users SET passkey_prompt_seen=true WHERE id=$1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Auth) profileHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := a.current(r)
		if err != nil {
			respond(w, 401, map[string]string{"error": "sign in again"})
			return
		}
		if r.Method != "GET" && !a.csrf(r, session) {
			respond(w, 403, map[string]string{"error": "reload before updating your account"})
			return
		}
		store := a.Login.Store
		switch {
		case r.URL.Path == "/auth/profile" && r.Method == "GET":
			keys, err := store.passkeys(r.Context(), session.User.ID)
			if err != nil {
				respond(w, 503, map[string]string{"error": "settings unavailable"})
				return
			}
			respond(w, 200, map[string]any{"name": session.User.Name, "email": session.User.Email, "passkeys": keys})
		case r.URL.Path == "/auth/profile" && r.Method == "POST":
			var input struct {
				Name string `json:"name"`
			}
			if decode(r, &input) != nil {
				respond(w, 400, map[string]string{"error": "invalid profile"})
				return
			}
			name, err := ValidName(input.Name)
			if err != nil {
				respond(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if _, err = store.db.ExecContext(r.Context(), `UPDATE warden_cloud.users SET name=$1 WHERE id=$2`, name, session.User.ID); err != nil {
				respond(w, 503, map[string]string{"error": "unable to save name"})
				return
			}
			respond(w, 200, map[string]string{"name": name})
		case r.URL.Path == "/auth/profile/passkey-prompt" && r.Method == "POST":
			if _, err = store.db.ExecContext(r.Context(), `UPDATE warden_cloud.users SET passkey_prompt_seen=true WHERE id=$1`, session.User.ID); err != nil {
				respond(w, 503, map[string]string{"error": "unable to save preference"})
				return
			}
			respond(w, 200, map[string]string{"status": "saved"})
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *Auth) managePasskey(w http.ResponseWriter, r *http.Request) {
	session, err := a.current(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "sign in again"})
		return
	}
	if !a.csrf(r, session) {
		respond(w, 403, map[string]string{"error": "reload before changing passkeys"})
		return
	}
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 {
		respond(w, 400, map[string]string{"error": "invalid passkey"})
		return
	}
	var result sql.Result
	switch r.Method {
	case "DELETE":
		result, err = a.Login.Store.db.ExecContext(r.Context(), `DELETE FROM warden_cloud.passkeys WHERE user_id=$1 AND credential_id=$2`, session.User.ID, id)
	case "PATCH":
		var input struct {
			Name string `json:"name"`
		}
		if decode(r, &input) != nil {
			respond(w, 400, map[string]string{"error": "invalid passkey name"})
			return
		}
		name, e := ValidName(input.Name)
		if e != nil {
			respond(w, 400, map[string]string{"error": e.Error()})
			return
		}
		result, err = a.Login.Store.db.ExecContext(r.Context(), `UPDATE warden_cloud.passkeys SET name=$1 WHERE user_id=$2 AND credential_id=$3`, name, session.User.ID, id)
	}
	if err != nil {
		respond(w, 503, map[string]string{"error": "unable to update passkey"})
		return
	}
	if result == nil {
		http.NotFound(w, r)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		respond(w, 503, map[string]string{"error": "unable to update passkey"})
		return
	}
	if n != 1 {
		respond(w, 404, map[string]string{"error": "passkey no longer available"})
		return
	}
	respond(w, 200, map[string]string{"status": "saved"})
}

func (s *Store) registrationChallenge(ctx context.Context, userID, name, replacementID string, session *webauthn.SessionData) (string, error) {
	var replacement []byte
	if replacementID != "" {
		var err error
		replacement, err = base64.RawURLEncoding.DecodeString(replacementID)
		if err != nil || len(replacement) == 0 {
			return "", errors.New("invalid replacement passkey")
		}
		var exists bool
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM warden_cloud.passkeys WHERE user_id=$1 AND credential_id=$2)`, userID, replacement).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return "", errors.New("passkey no longer available")
		}
	}
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	id := ID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO warden_cloud.passkey_challenges(id,user_id,kind,session,expires_at,name,replacement_id) VALUES($1,$2,'register',$3,$4,$5,$6)`, id, userID, data, session.Expires, name, replacement)
	return id, err
}
