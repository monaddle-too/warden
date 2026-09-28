package cloudauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

const codeLifetime = 10 * time.Minute
const sessionLifetime = 7 * 24 * time.Hour

type Mailer interface {
	SendCode(context.Context, string, string) error
}
type Login struct {
	Store  *Store
	Mailer Mailer
	Pepper []byte
}
type Session struct {
	Digest         string
	User           User
	OrganizationID string
	Role           string
	CSRF           string
	Expires        time.Time
}

func randomSecret() string {
	var b [32]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func code() string {
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%08d", n.Int64())
}
func (l *Login) digest(email, code string) []byte {
	m := hmac.New(sha256.New, l.Pepper)
	m.Write([]byte(email))
	m.Write([]byte{0})
	m.Write([]byte(code))
	return m.Sum(nil)
}
func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (l *Login) Validate() error {
	if l.Store == nil || l.Mailer == nil || len(l.Pepper) < 32 {
		return errors.New("cloud login needs its database, SMTP mailer, and 32-byte code key")
	}
	return nil
}

// IssueCode gives the same successful response for every valid email address.
// Only an enabled, admitted person receives a code; database rate limits apply
// to both recognized and unrecognized addresses.
func (l *Login) IssueCode(ctx context.Context, address, remote string) error {
	if err := l.Validate(); err != nil {
		return err
	}
	email, err := NormalizeEmail(address)
	if err != nil {
		return err
	}
	remote = strings.TrimSpace(remote)
	if len(remote) > 128 {
		remote = remote[:128]
	}
	keys := []string{"email:" + hashSecret(email), "remote:" + hashSecret(remote)}
	now := l.Store.now()
	tx, err := l.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM warden_cloud.login_attempts WHERE at < $1`, now.Add(-time.Hour)); err != nil {
		return err
	}
	for i, key := range keys {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM warden_cloud.login_attempts WHERE key=$1 AND at>=$2`, key, now.Add(-time.Hour)).Scan(&n); err != nil {
			return err
		}
		max := 5
		if i == 1 {
			max = 30
		}
		if n >= max {
			return errors.New("too many sign-in requests; try again later")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.login_attempts(key,at) VALUES($1,$2)`, key, now); err != nil {
			return err
		}
	}
	var sent time.Time
	err = tx.QueryRowContext(ctx, `SELECT sent_at FROM warden_cloud.login_codes WHERE email=$1`, email).Scan(&sent)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && now.Sub(sent) < time.Minute {
		return tx.Commit()
	}
	var admitted bool
	err = tx.QueryRowContext(ctx, `SELECT NOT disabled AND (full_admin OR EXISTS(SELECT 1 FROM warden_cloud.memberships m WHERE m.user_id=u.id)) FROM warden_cloud.users u WHERE email=$1`, email).Scan(&admitted)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows || !admitted {
		return tx.Commit()
	}
	value := code()
	digest := l.digest(email, value)
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.login_codes(email,digest,expires_at,sent_at,attempts) VALUES($1,$2,$3,$4,0) ON CONFLICT(email) DO UPDATE SET digest=EXCLUDED.digest,expires_at=EXCLUDED.expires_at,sent_at=EXCLUDED.sent_at,attempts=0`, email, digest, now.Add(codeLifetime), now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = l.Mailer.SendCode(ctx, email, value); err != nil {
		_, _ = l.Store.db.ExecContext(ctx, `DELETE FROM warden_cloud.login_codes WHERE email=$1 AND digest=$2`, email, digest)
		return err
	}
	return nil
}

func (l *Login) VerifyCode(ctx context.Context, address, value string) (Session, string, error) {
	if err := l.Validate(); err != nil {
		return Session{}, "", err
	}
	email, err := NormalizeEmail(address)
	if err != nil {
		return Session{}, "", err
	}
	if len(value) != 8 {
		return Session{}, "", errors.New("invalid or expired code")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return Session{}, "", errors.New("invalid or expired code")
		}
	}
	tx, err := l.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, "", err
	}
	defer tx.Rollback()
	var digest []byte
	var expires time.Time
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT digest,expires_at,attempts FROM warden_cloud.login_codes WHERE email=$1 FOR UPDATE`, email).Scan(&digest, &expires, &attempts)
	if err != nil {
		return Session{}, "", errors.New("invalid or expired code")
	}
	if attempts >= 5 || !l.Store.now().Before(expires) {
		return Session{}, "", errors.New("invalid or expired code")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE warden_cloud.login_codes SET attempts=attempts+1 WHERE email=$1`, email); err != nil {
		return Session{}, "", err
	}
	if subtle.ConstantTimeCompare(digest, l.digest(email, value)) != 1 {
		if err = tx.Commit(); err != nil {
			return Session{}, "", err
		}
		return Session{}, "", errors.New("invalid or expired code")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM warden_cloud.login_codes WHERE email=$1`, email); err != nil {
		return Session{}, "", err
	}
	var s Session
	err = tx.QueryRowContext(ctx, `SELECT id,email,name,full_admin,disabled FROM warden_cloud.users WHERE email=$1`, email).Scan(&s.User.ID, &s.User.Email, &s.User.Name, &s.User.FullAdmin, &s.User.Disabled)
	if err != nil || s.User.Disabled {
		return Session{}, "", errors.New("account unavailable")
	}
	rows, err := tx.QueryContext(ctx, `SELECT organization_id,role FROM warden_cloud.memberships WHERE user_id=$1 ORDER BY organization_id LIMIT 2`, s.User.ID)
	if err != nil {
		return Session{}, "", err
	}
	count := 0
	for rows.Next() {
		count++
		if count == 1 {
			if err = rows.Scan(&s.OrganizationID, &s.Role); err != nil {
				rows.Close()
				return Session{}, "", err
			}
		} else {
			var org, role string
			if err = rows.Scan(&org, &role); err != nil {
				rows.Close()
				return Session{}, "", err
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Session{}, "", err
	}
	if count > 1 {
		s.OrganizationID = ""
		s.Role = ""
	}
	if count == 0 && !s.User.FullAdmin {
		return Session{}, "", errors.New("account unavailable")
	}
	raw := randomSecret()
	s.Digest = hashSecret(raw)
	s.CSRF = randomSecret()
	s.Expires = l.Store.now().Add(sessionLifetime)
	var org any
	if s.OrganizationID != "" {
		org = s.OrganizationID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.sessions(digest,user_id,organization_id,csrf,expires_at) VALUES($1,$2,$3,$4,$5)`, s.Digest, s.User.ID, org, s.CSRF, s.Expires)
	if err != nil {
		return Session{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, "", err
	}
	return s, raw, nil
}

func (s *Store) Session(ctx context.Context, raw string) (Session, error) {
	if len(raw) < 32 || len(raw) > 128 {
		return Session{}, errors.New("invalid session")
	}
	var current Session
	var org sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT x.digest,x.csrf,x.expires_at,x.organization_id,u.id,u.email,u.name,u.full_admin,u.disabled FROM warden_cloud.sessions x JOIN warden_cloud.users u ON u.id=x.user_id WHERE x.digest=$1`, hashSecret(raw)).Scan(&current.Digest, &current.CSRF, &current.Expires, &org, &current.User.ID, &current.User.Email, &current.User.Name, &current.User.FullAdmin, &current.User.Disabled)
	if err != nil {
		return Session{}, err
	}
	if current.User.Disabled || !s.now().Before(current.Expires) {
		return Session{}, errors.New("expired session")
	}
	if org.Valid {
		current.OrganizationID = org.String
		role, err := s.Role(ctx, current.User.ID, current.OrganizationID)
		if err != nil && !current.User.FullAdmin {
			return Session{}, errors.New("membership removed")
		}
		if err == nil {
			current.Role = role
		}
	}
	if !current.User.FullAdmin && current.OrganizationID == "" {
		orgs, err := s.Organizations(ctx, current.User.ID, false)
		if err != nil || len(orgs) == 0 {
			return Session{}, errors.New("membership removed")
		}
	}
	return current, nil
}
func (s *Store) SelectOrganization(ctx context.Context, current Session, id string) error {
	if id == "" {
		return errors.New("organization required")
	}
	if !current.User.FullAdmin {
		if _, err := s.Role(ctx, current.User.ID, id); err != nil {
			return errors.New("membership required")
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE warden_cloud.sessions SET organization_id=$1 WHERE digest=$2`, id, current.Digest)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) Logout(ctx context.Context, current Session) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM warden_cloud.sessions WHERE digest=$1`, current.Digest)
	return err
}
