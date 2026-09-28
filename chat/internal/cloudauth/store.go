// Package cloudauth owns cloud organizations, memberships, and browser identity.
// It has no filesystem fallback: the cloud control plane must have PostgreSQL.
package cloudauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"warden/chat/internal/recordings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const schema = `
CREATE SCHEMA IF NOT EXISTS warden_cloud;
CREATE TABLE IF NOT EXISTS warden_cloud.users (
 id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
 full_admin BOOLEAN NOT NULL DEFAULT false, disabled BOOLEAN NOT NULL DEFAULT false,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS warden_cloud.organizations (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS warden_cloud.memberships (
 organization_id TEXT NOT NULL REFERENCES warden_cloud.organizations(id),
 user_id TEXT NOT NULL REFERENCES warden_cloud.users(id),
 role TEXT NOT NULL CHECK (role IN ('admin','user')),
 PRIMARY KEY (organization_id,user_id));
CREATE INDEX IF NOT EXISTS memberships_user ON warden_cloud.memberships(user_id);
CREATE TABLE IF NOT EXISTS warden_cloud.login_codes (
 email TEXT PRIMARY KEY, digest BYTEA NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 sent_at TIMESTAMPTZ NOT NULL, attempts INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS warden_cloud.login_attempts (
 key TEXT NOT NULL, at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS login_attempts_key_time ON warden_cloud.login_attempts(key,at);
CREATE TABLE IF NOT EXISTS warden_cloud.sessions (
 digest TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES warden_cloud.users(id),
 organization_id TEXT REFERENCES warden_cloud.organizations(id),
 csrf TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS sessions_user ON warden_cloud.sessions(user_id);
CREATE TABLE IF NOT EXISTS warden_cloud.passkeys (
 credential_id BYTEA PRIMARY KEY, user_id TEXT NOT NULL REFERENCES warden_cloud.users(id),
 credential JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_used_at TIMESTAMPTZ);
CREATE INDEX IF NOT EXISTS passkeys_user ON warden_cloud.passkeys(user_id);
CREATE TABLE IF NOT EXISTS warden_cloud.passkey_challenges (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES warden_cloud.users(id),
 kind TEXT NOT NULL CHECK (kind IN ('register','login')),
 session JSONB NOT NULL, expires_at TIMESTAMPTZ NOT NULL);
ALTER TABLE warden_cloud.users ADD COLUMN IF NOT EXISTS passkey_prompt_seen BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE warden_cloud.passkeys ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT 'Passkey';
ALTER TABLE warden_cloud.passkey_challenges ADD COLUMN IF NOT EXISTS replacement_id BYTEA;
ALTER TABLE warden_cloud.passkey_challenges ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT 'Passkey';
`

type Store struct {
	db              *sql.DB
	now             func() time.Time
	recordingCancel context.CancelFunc
}

type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	FullAdmin bool   `json:"fullAdmin"`
	Disabled  bool   `json:"disabled"`
}
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Member struct {
	User
	Role string `json:"role"`
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func NormalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") || strings.Count(email, "@") != 1 {
		return "", errors.New("enter one valid email address")
	}
	return email, nil
}
func ValidName(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("enter a name of at most 120 characters")
	}
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 120 {
		return "", errors.New("enter a name of at most 120 characters")
	}
	return value, nil
}

func Open(ctx context.Context, dsn, bootstrapEmail string) (*Store, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, errors.New("cloud identity requires a PostgreSQL URL")
	}
	owner, err := NormalizeEmail(bootstrapEmail)
	if err != nil {
		return nil, fmt.Errorf("bootstrap full admin: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	s := &Store{db: db, now: time.Now}
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if err = db.PingContext(ctx); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='30s'"); err != nil {
		return fail(err)
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(8173001)"); err != nil {
		return fail(err)
	}
	for _, statement := range strings.Split(schema+externalSchema+recordings.Schema, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fail(err)
		}
	}
	// A fresh installation starts with one full admin. On an existing DB the
	// upsert never changes the user's name, role, or disabled state.
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.users(id,email,name,full_admin) VALUES($1,$2,$3,true) ON CONFLICT(email) DO UPDATE SET full_admin=true`, ID(), owner, owner); err != nil {
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error {
	if s.recordingCancel != nil {
		s.recordingCancel()
	}
	return s.db.Close()
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.db.QueryRowContext(ctx, `SELECT id,email,name,full_admin,disabled FROM warden_cloud.users WHERE email=$1`, email).Scan(&u.ID, &u.Email, &u.Name, &u.FullAdmin, &u.Disabled)
	return u, err
}
func (s *Store) User(ctx context.Context, id string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,email,name,full_admin,disabled FROM warden_cloud.users WHERE id=$1`, id).Scan(&u.ID, &u.Email, &u.Name, &u.FullAdmin, &u.Disabled)
	return u, err
}
func (s *Store) Organizations(ctx context.Context, userID string, fullAdmin bool) ([]Organization, error) {
	query := `SELECT o.id,o.name FROM warden_cloud.organizations o JOIN warden_cloud.memberships m ON m.organization_id=o.id WHERE m.user_id=$1 ORDER BY o.name`
	if fullAdmin {
		query = `SELECT id,name FROM warden_cloud.organizations ORDER BY name`
	}
	var rows *sql.Rows
	var err error
	if fullAdmin {
		rows, err = s.db.QueryContext(ctx, query)
	} else {
		rows, err = s.db.QueryContext(ctx, query, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Organization{}
	for rows.Next() {
		var o Organization
		if err = rows.Scan(&o.ID, &o.Name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) Role(ctx context.Context, userID, organizationID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM warden_cloud.memberships WHERE user_id=$1 AND organization_id=$2`, userID, organizationID).Scan(&role)
	return role, err
}
func (s *Store) CreateOrganization(ctx context.Context, actor User, name string) (Organization, error) {
	if !actor.FullAdmin || actor.Disabled {
		return Organization{}, errors.New("full admin required")
	}
	name, err := ValidName(name)
	if err != nil {
		return Organization{}, err
	}
	o := Organization{ID: ID(), Name: name}
	if err = s.EnsureOrganization(ctx, actor, o.ID, o.Name); err != nil {
		return Organization{}, err
	}
	return o, nil
}

// EnsureOrganization is used at first cloud rollout to give legacy chats a
// stable organization before the browser is available. It does not rename an
// existing organization or change an existing membership.
var organizationIDShape = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (s *Store) EnsureOrganization(ctx context.Context, actor User, id, name string) error {
	if !actor.FullAdmin || actor.Disabled || !organizationIDShape.MatchString(id) {
		return errors.New("full admin and 32-digit organization ID required")
	}
	name, err := ValidName(name)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.organizations(id,name) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, id, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.memberships(organization_id,user_id,role) VALUES($1,$2,'admin') ON CONFLICT DO NOTHING`, id, actor.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Members(ctx context.Context, actor User, organizationID string) ([]Member, error) {
	if err := s.requireAdmin(ctx, actor, organizationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.email,u.name,u.full_admin,u.disabled,m.role FROM warden_cloud.memberships m JOIN warden_cloud.users u ON u.id=m.user_id WHERE m.organization_id=$1 ORDER BY u.name,u.email`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.ID, &m.Email, &m.Name, &m.FullAdmin, &m.Disabled, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) requireAdmin(ctx context.Context, actor User, organizationID string) error {
	if actor.Disabled {
		return errors.New("account disabled")
	}
	if actor.FullAdmin {
		return nil
	}
	role, err := s.Role(ctx, actor.ID, organizationID)
	if err != nil || role != "admin" {
		return errors.New("organization admin required")
	}
	return nil
}
func (s *Store) PutMember(ctx context.Context, actor User, organizationID, email, name, role string) (Member, error) {
	if err := s.requireAdmin(ctx, actor, organizationID); err != nil {
		return Member{}, err
	}
	if role != "user" && role != "admin" {
		return Member{}, errors.New("invalid role")
	}
	if role == "admin" && !actor.FullAdmin {
		return Member{}, errors.New("only the full admin can assign administrators")
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return Member{}, err
	}
	name, err = ValidName(name)
	if err != nil {
		return Member{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.users(id,email,name) VALUES($1,$2,$3) ON CONFLICT(email) DO NOTHING`, ID(), email, name); err != nil {
		return Member{}, err
	}
	var m Member
	if err = tx.QueryRowContext(ctx, `SELECT id,email,name,full_admin,disabled FROM warden_cloud.users WHERE email=$1`, email).Scan(&m.ID, &m.Email, &m.Name, &m.FullAdmin, &m.Disabled); err != nil {
		return Member{}, err
	}
	if m.Disabled {
		return Member{}, errors.New("account disabled")
	}
	if !actor.FullAdmin {
		var currentRole string
		err = tx.QueryRowContext(ctx, `SELECT role FROM warden_cloud.memberships WHERE organization_id=$1 AND user_id=$2`, organizationID, m.ID).Scan(&currentRole)
		if err != nil && err != sql.ErrNoRows {
			return Member{}, err
		}
		if m.FullAdmin || currentRole == "admin" {
			return Member{}, errors.New("only the full admin can change administrators")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_cloud.memberships(organization_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT(organization_id,user_id) DO UPDATE SET role=EXCLUDED.role`, organizationID, m.ID, role); err != nil {
		return Member{}, err
	}
	if err = tx.Commit(); err != nil {
		return Member{}, err
	}
	m.Role = role
	return m, nil
}
func (s *Store) RemoveMember(ctx context.Context, actor User, organizationID, email string) error {
	if err := s.requireAdmin(ctx, actor, organizationID); err != nil {
		return err
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !actor.FullAdmin {
		var role string
		var fullAdmin bool
		err = tx.QueryRowContext(ctx, `SELECT m.role,u.full_admin FROM warden_cloud.memberships m JOIN warden_cloud.users u ON u.id=m.user_id WHERE m.organization_id=$1 AND u.email=$2 FOR UPDATE`, organizationID, email).Scan(&role, &fullAdmin)
		if err != nil {
			return err
		}
		if role == "admin" || fullAdmin {
			return errors.New("only the full admin can remove administrators")
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM warden_cloud.memberships WHERE organization_id=$1 AND user_id=(SELECT id FROM warden_cloud.users WHERE email=$2)`, organizationID, email)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	// Revoked grants never revive if this person is later added back.
	if _, err = tx.ExecContext(ctx, `UPDATE warden_cloud.agent_connections SET revoked_at=now() WHERE organization_id=$1 AND user_id=(SELECT id FROM warden_cloud.users WHERE email=$2)`, organizationID, email); err != nil {
		return err
	}
	// Invalidate any session currently selecting this organization.
	_, err = tx.ExecContext(ctx, `DELETE FROM warden_cloud.sessions WHERE organization_id=$1 AND user_id=(SELECT id FROM warden_cloud.users WHERE email=$2)`, organizationID, email)
	if err != nil {
		return err
	}
	return tx.Commit()
}
