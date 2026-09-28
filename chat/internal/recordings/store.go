// Package recordings owns organization-scoped device audio. PostgreSQL is the
// durable ingest queue; finalized audio lives in private object storage.
package recordings

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const Schema = `
CREATE TABLE IF NOT EXISTS warden_cloud.audio_devices (
 id TEXT PRIMARY KEY, organization_id TEXT NOT NULL REFERENCES warden_cloud.organizations(id),
 user_id TEXT NOT NULL REFERENCES warden_cloud.users(id), name TEXT NOT NULL,
 secret_hash TEXT NOT NULL UNIQUE, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_seen_at TIMESTAMPTZ, revoked BOOLEAN NOT NULL DEFAULT false);
CREATE INDEX IF NOT EXISTS audio_devices_org ON warden_cloud.audio_devices(organization_id);
CREATE TABLE IF NOT EXISTS warden_cloud.recordings (
 id TEXT PRIMARY KEY, organization_id TEXT NOT NULL REFERENCES warden_cloud.organizations(id),
 device_id TEXT NOT NULL REFERENCES warden_cloud.audio_devices(id), operation_id TEXT NOT NULL,
 title TEXT NOT NULL, language TEXT NOT NULL DEFAULT 'en-US', mode TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'live', bytes BIGINT NOT NULL DEFAULT 0,
 next_seq BIGINT NOT NULL DEFAULT 0, archived_bytes BIGINT NOT NULL DEFAULT 0,
 provisional TEXT NOT NULL DEFAULT '', provisional_bytes BIGINT NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 lease_until TIMESTAMPTZ, lease_token TEXT NOT NULL DEFAULT '', retry_at TIMESTAMPTZ,
 worker_error TEXT NOT NULL DEFAULT '', UNIQUE(device_id,operation_id));
CREATE INDEX IF NOT EXISTS recordings_org ON warden_cloud.recordings(organization_id,created_at DESC);
CREATE TABLE IF NOT EXISTS warden_cloud.audio_chunks (
 recording_id TEXT NOT NULL REFERENCES warden_cloud.recordings(id), seq BIGINT NOT NULL,
 offset_bytes BIGINT NOT NULL, size INTEGER NOT NULL, digest TEXT NOT NULL, audio BYTEA,
 PRIMARY KEY(recording_id,seq));
CREATE TABLE IF NOT EXISTS warden_cloud.audio_segments (
 recording_id TEXT NOT NULL REFERENCES warden_cloud.recordings(id), offset_bytes BIGINT NOT NULL,
 size INTEGER NOT NULL, object_key TEXT NOT NULL, text TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'waiting', attempts INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(recording_id,offset_bytes));
`
const BytesPerSecond = 32000
const MaxChunk = 32000
const MaxRecordingBytes = BytesPerSecond * 7200

var ErrDenied = errors.New("device credential is invalid, revoked, or its owner no longer belongs to the organization")

type Problem struct {
	Status  int
	Message string
}

func (p *Problem) Error() string         { return p.Message }
func problem(status int, s string) error { return &Problem{status, s} }
func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Secret() string       { return "wrd_" + ID() + ID() }
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type Actor struct {
	UserID, OrganizationID string
	Admin                  bool
}
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Creator    string     `json:"creator"`
	UserID     string     `json:"userId"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeenAt *time.Time `json:"lastSeenAt"`
	Revoked    bool       `json:"revoked"`
	CanManage  bool       `json:"canManage"`
}
type Segment struct {
	Offset int64  `json:"offsetBytes"`
	Size   int    `json:"size"`
	Text   string `json:"text"`
	State  string `json:"state"`
}
type Recording struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	DeviceID      string    `json:"deviceId"`
	DeviceName    string    `json:"deviceName"`
	Language      string    `json:"language"`
	Mode          string    `json:"mode"`
	Status        string    `json:"status"`
	Transcription string    `json:"transcription"`
	Bytes         int64     `json:"bytes"`
	NextSeq       int64     `json:"nextSeq"`
	ArchivedBytes int64     `json:"archivedBytes"`
	Provisional   string    `json:"provisional"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Segments      []Segment `json:"segments,omitempty"`
	Error         string    `json:"error,omitempty"`
}
type Store struct{ DB *sql.DB }

func (s *Store) Authenticate(ctx context.Context, raw string) (Actor, string, error) {
	if !strings.HasPrefix(raw, "wrd_") || len(raw) != 68 {
		return Actor{}, "", ErrDenied
	}
	var a Actor
	var id string
	e := s.DB.QueryRowContext(ctx, `SELECT d.id,d.organization_id,d.user_id FROM warden_cloud.audio_devices d JOIN warden_cloud.users u ON u.id=d.user_id WHERE d.secret_hash=$1 AND NOT d.revoked AND NOT u.disabled AND (u.full_admin OR EXISTS(SELECT 1 FROM warden_cloud.memberships m WHERE m.user_id=d.user_id AND m.organization_id=d.organization_id))`, hash([]byte(raw))).Scan(&id, &a.OrganizationID, &a.UserID)
	if e != nil {
		return Actor{}, "", ErrDenied
	}
	return a, id, nil
}
func (s *Store) Devices(ctx context.Context, a Actor) ([]Device, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT d.id,d.name,u.name,d.user_id,d.created_at,d.last_seen_at,d.revoked FROM warden_cloud.audio_devices d JOIN warden_cloud.users u ON u.id=d.user_id WHERE d.organization_id=$1 ORDER BY d.created_at DESC`, a.OrganizationID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if e = rows.Scan(&d.ID, &d.Name, &d.Creator, &d.UserID, &d.CreatedAt, &d.LastSeenAt, &d.Revoked); e != nil {
			return nil, e
		}
		d.CanManage = a.Admin || a.UserID == d.UserID
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) Register(ctx context.Context, a Actor, name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return "", "", problem(400, "Enter a device name of at most 120 characters.")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT id FROM warden_cloud.users WHERE id=$1 FOR UPDATE`, a.UserID); e != nil {
		return "", "", e
	}
	var n int
	e = tx.QueryRowContext(ctx, `SELECT count(*) FROM warden_cloud.audio_devices WHERE user_id=$1 AND organization_id=$2 AND NOT revoked`, a.UserID, a.OrganizationID).Scan(&n)
	if e != nil {
		return "", "", e
	}
	if n >= 20 {
		return "", "", problem(429, "Limit of 20 active devices per member reached.")
	}
	id, key := ID(), Secret()
	_, e = tx.ExecContext(ctx, `INSERT INTO warden_cloud.audio_devices(id,organization_id,user_id,name,secret_hash) VALUES($1,$2,$3,$4,$5)`, id, a.OrganizationID, a.UserID, name, hash([]byte(key)))
	if e != nil {
		return "", "", e
	}
	return id, key, tx.Commit()
}
func (s *Store) Manage(ctx context.Context, a Actor, id string, revoke bool) (string, error) {
	key := Secret()
	q := `UPDATE warden_cloud.audio_devices SET secret_hash=$1,revoked=$2 WHERE id=$3 AND organization_id=$4 AND (user_id=$5 OR $6) AND NOT revoked`
	result, e := s.DB.ExecContext(ctx, q, hash([]byte(key)), revoke, id, a.OrganizationID, a.UserID, a.Admin)
	if e != nil {
		return "", e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", problem(404, "Active device not found or not yours to manage.")
	}
	if revoke {
		return "", nil
	}
	return key, nil
}

type Create struct {
	OperationID string `json:"operationId"`
	Title       string `json:"title"`
	Language    string `json:"language"`
	Mode        string `json:"mode"`
	Encoding    string `json:"encoding"`
	SampleRate  int    `json:"sampleRate"`
	Channels    int    `json:"channels"`
}

func (s *Store) Create(ctx context.Context, a Actor, device string, in Create) (string, error) {
	if len(in.OperationID) < 8 || len(in.OperationID) > 128 || len(in.Title) > 200 || strings.TrimSpace(in.Title) == "" {
		return "", problem(400, "A title and operationId (8–128 characters) are required.")
	}
	if in.Language == "" {
		in.Language = "en-US"
	}
	if in.Language != "en-US" {
		return "", problem(400, "This first version supports en-US.")
	}
	if in.Encoding != "pcm_s16le" || in.SampleRate != 16000 || in.Channels != 1 || (in.Mode != "stream" && in.Mode != "upload") {
		return "", problem(400, "Use pcm_s16le, sampleRate 16000, channels 1, and mode stream or upload.")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT id FROM warden_cloud.audio_devices WHERE id=$1 FOR UPDATE`, device); e != nil {
		return "", e
	}
	var id, title, mode, language string
	e = tx.QueryRowContext(ctx, `SELECT id,title,mode,language FROM warden_cloud.recordings WHERE device_id=$1 AND operation_id=$2`, device, in.OperationID).Scan(&id, &title, &mode, &language)
	if e == nil {
		if title != in.Title || mode != in.Mode || language != in.Language {
			return "", problem(409, "operationId was already used with different metadata.")
		}
		return id, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	var n int
	e = tx.QueryRowContext(ctx, `SELECT count(*) FROM warden_cloud.recordings WHERE device_id=$1 AND status!='finished'`, device).Scan(&n)
	if e != nil {
		return "", e
	}
	if n >= 2 {
		return "", problem(429, "Finish an existing recording before starting another (maximum 2 unfinished/device).")
	}
	e = tx.QueryRowContext(ctx, `SELECT count(*) FROM warden_cloud.recordings WHERE organization_id=$1 AND created_at>now()-interval '1 day'`, a.OrganizationID).Scan(&n)
	if e != nil {
		return "", e
	}
	if n >= 100 {
		return "", problem(429, "Daily organization recording limit reached (100).")
	}
	id = ID()
	status := "live"
	if in.Mode == "upload" {
		status = "uploading"
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO warden_cloud.recordings(id,organization_id,device_id,operation_id,title,language,mode,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, a.OrganizationID, device, in.OperationID, in.Title, in.Language, in.Mode, status)
	if e != nil {
		return "", e
	}
	return id, tx.Commit()
}
func (s *Store) Append(ctx context.Context, a Actor, device, id string, seq int64, audio []byte) (int64, error) {
	if len(audio) == 0 || len(audio) > MaxChunk || len(audio)%2 != 0 || seq < 0 {
		return 0, problem(400, "Chunk must contain 1–16000 complete PCM samples, with a nonnegative sequence.")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	var next, total, archived int64
	var status string
	e = tx.QueryRowContext(ctx, `SELECT next_seq,bytes,archived_bytes,status FROM warden_cloud.recordings WHERE id=$1 AND organization_id=$2 AND device_id=$3 FOR UPDATE`, id, a.OrganizationID, device).Scan(&next, &total, &archived, &status)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, problem(404, "Recording not found.")
	}
	if e != nil {
		return 0, e
	}
	if seq < next {
		var digest string
		e = tx.QueryRowContext(ctx, `SELECT digest FROM warden_cloud.audio_chunks WHERE recording_id=$1 AND seq=$2`, id, seq).Scan(&digest)
		if e != nil {
			return 0, e
		}
		if digest != hash(audio) {
			return 0, problem(409, "Sequence was already acknowledged with different audio.")
		}
		return next, nil
	}
	if seq != next {
		return next, problem(409, fmt.Sprintf("Expected sequence %d. Resume from the acknowledged offset.", next))
	}
	if status == "finished" {
		return next, problem(409, "Recording has finished.")
	}
	if total+int64(len(audio)) > MaxRecordingBytes {
		return next, problem(413, "Recording exceeds two hours.")
	}
	if total-archived > 5*1024*1024 {
		return next, problem(429, "Processing is behind. Retain audio and retry after 5 seconds.")
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO warden_cloud.audio_chunks(recording_id,seq,offset_bytes,size,digest,audio) VALUES($1,$2,$3,$4,$5,$6)`, id, seq, total, len(audio), hash(audio), audio)
	if e != nil {
		return 0, e
	}
	_, e = tx.ExecContext(ctx, `UPDATE warden_cloud.recordings SET next_seq=next_seq+1,bytes=bytes+$2,updated_at=now() WHERE id=$1`, id, len(audio))
	if e != nil {
		return 0, e
	}
	_, e = tx.ExecContext(ctx, `UPDATE warden_cloud.audio_devices SET last_seen_at=now() WHERE id=$1`, device)
	if e != nil {
		return 0, e
	}
	return next + 1, tx.Commit()
}
func (s *Store) Finish(ctx context.Context, a Actor, device, id string, next int64) error {
	q := `UPDATE warden_cloud.recordings SET status='finished',updated_at=now() WHERE id=$1 AND organization_id=$2 AND ($3='' OR device_id=$3) AND next_seq=$4`
	res, e := s.DB.ExecContext(ctx, q, id, a.OrganizationID, device, next)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return problem(409, "Recording not found or final sequence does not match; check status and resume missing audio.")
	}
	return nil
}

const recordColumns = `r.id,r.title,r.device_id,d.name,r.language,r.mode,r.status,r.bytes,r.next_seq,r.archived_bytes,r.provisional,r.created_at,r.updated_at,r.worker_error`

func scanRecording(row interface{ Scan(...any) error }, extra ...any) (Recording, error) {
	var r Recording
	fields := []any{&r.ID, &r.Title, &r.DeviceID, &r.DeviceName, &r.Language, &r.Mode, &r.Status, &r.Bytes, &r.NextSeq, &r.ArchivedBytes, &r.Provisional, &r.CreatedAt, &r.UpdatedAt, &r.Error}
	e := row.Scan(append(fields, extra...)...)
	if r.Status != "finished" && time.Since(r.UpdatedAt) > 15*time.Second {
		r.Status = "interrupted"
	}
	return r, e
}
func (s *Store) Get(ctx context.Context, a Actor, device, id string) (Recording, error) {
	r, e := scanRecording(s.DB.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM warden_cloud.recordings r JOIN warden_cloud.audio_devices d ON d.id=r.device_id WHERE r.id=$1 AND r.organization_id=$2 AND ($3='' OR r.device_id=$3)`, id, a.OrganizationID, device))
	if errors.Is(e, sql.ErrNoRows) {
		return r, problem(404, "Recording not found.")
	}
	if e != nil {
		return r, e
	}
	rows, e := s.DB.QueryContext(ctx, `SELECT offset_bytes,size,text,state FROM warden_cloud.audio_segments WHERE recording_id=$1 ORDER BY offset_bytes`, id)
	if e != nil {
		return r, e
	}
	defer rows.Close()
	r.Segments = []Segment{}
	r.Transcription = "complete"
	for rows.Next() {
		var v Segment
		if e = rows.Scan(&v.Offset, &v.Size, &v.Text, &v.State); e != nil {
			return r, e
		}
		r.Segments = append(r.Segments, v)
		if v.State == "failed" {
			r.Transcription = "failed"
		} else if v.State != "complete" && r.Transcription != "failed" {
			r.Transcription = "transcribing"
		}
	}
	if r.Transcription != "failed" && (r.Status != "finished" || r.ArchivedBytes < r.Bytes) {
		r.Transcription = "transcribing"
	}
	if r.Bytes == 0 && r.Status != "finished" {
		r.Transcription = "waiting"
	}
	return r, rows.Err()
}
func (s *Store) List(ctx context.Context, a Actor) ([]Recording, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT `+recordColumns+`,CASE
 WHEN r.bytes=0 AND r.status!='finished' THEN 'waiting'
 WHEN EXISTS(SELECT 1 FROM warden_cloud.audio_segments s WHERE s.recording_id=r.id AND s.state='failed') THEN 'failed'
 WHEN r.status!='finished' OR r.archived_bytes<r.bytes OR EXISTS(SELECT 1 FROM warden_cloud.audio_segments s WHERE s.recording_id=r.id AND s.state='waiting') THEN 'transcribing'
 ELSE 'complete' END
 FROM warden_cloud.recordings r JOIN warden_cloud.audio_devices d ON d.id=r.device_id
 WHERE r.organization_id=$1 ORDER BY r.created_at DESC LIMIT 100`, a.OrganizationID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Recording{}
	for rows.Next() {
		var state string
		r, e := scanRecording(rows, &state)
		if e != nil {
			return nil, e
		}
		r.Transcription = state
		out = append(out, r)
	}
	return out, rows.Err()
}
