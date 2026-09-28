package chats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/durablestate"
	"warden/chat/internal/sandbox"
)

var cloudOrganizationID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// OpenCloud keeps chat state in PostgreSQL. The legacy organization's ID is
// required only when importing the old single-owner JSON or SQLite state. A
// cloud instance is one writer; the state row is named explicitly so isolated
// migration tests cannot touch production's "main" row.
func OpenCloud(root, dsn, legacyOrganization, instance string) (*Store, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, errors.New("cloud chat state requires PostgreSQL")
	}
	if instance == "" || len(instance) > 80 {
		return nil, errors.New("cloud chat instance required")
	}
	unlock, err := durablestate.Lock(dsn, "chat:"+instance)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root, cloud: true, state: State{Version: 1, Chats: []*Chat{}}, unlock: unlock, changed: make(chan struct{}), saveDelay: streamDelay, dirty: map[string]bool{}, shadow: newShadow()}
	fail := func(err error) (*Store, error) {
		if s.db != nil {
			s.db.Close()
		}
		unlock()
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fail(err)
	}
	s.db = db
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return fail(err)
	}
	if err = ensureCloudChatSchema(ctx, db); err != nil {
		return fail(err)
	}
	err = s.loadCloud(ctx, instance)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		var saved []byte
		err = db.QueryRowContext(ctx, `SELECT state FROM warden_cloud.chat_state WHERE id=$1`, instance).Scan(&saved)
		if err == nil {
			if err = json.Unmarshal(saved, &s.state); err != nil {
				return fail(err)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if err = s.importCloudLegacy(root, legacyOrganization); err != nil {
				return fail(err)
			}
			if err = s.importCloudAttachments(root); err != nil {
				return fail(err)
			}
		} else {
			return fail(err)
		}
	default:
		return fail(err)
	}
	if s.state.Version != 1 {
		return fail(errors.New("unsupported chat data version"))
	}
	s.reconcileOnOpen()
	s.cloudInstance = instance
	if _, err = s.writeLocked(scopeAll); err != nil {
		return fail(err)
	}
	return s, nil
}

func ensureCloudChatSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='30s'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(8173001)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS warden_cloud`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS warden_cloud.chat_state(id TEXT PRIMARY KEY,state JSONB NOT NULL,updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS warden_cloud.chat_meta(instance TEXT PRIMARY KEY, globals JSONB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS warden_cloud.chat_records(instance TEXT NOT NULL,id TEXT NOT NULL,position INTEGER NOT NULL,record JSONB NOT NULL,PRIMARY KEY(instance,id))`,
		`CREATE TABLE IF NOT EXISTS warden_cloud.chat_items(instance TEXT NOT NULL,chat_id TEXT NOT NULL,kind INTEGER NOT NULL,seq INTEGER NOT NULL,item JSONB NOT NULL,PRIMARY KEY(instance,chat_id,kind,seq))`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS warden_cloud.chat_attachments(organization_id TEXT NOT NULL,chat_id TEXT NOT NULL,id TEXT NOT NULL,metadata JSONB NOT NULL,data BYTEA NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT now(),PRIMARY KEY(chat_id,id))`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) importCloudLegacy(root, organization string) error {
	legacy := filepath.Join(root, legacyFile)
	data, err := os.ReadFile(legacy)
	if err == nil {
		if err = json.Unmarshal(data, &s.state); err != nil {
			return fmt.Errorf("legacy chat JSON: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		oldDB := filepath.Join(root, dbFile)
		if _, statErr := os.Stat(oldDB); statErr == nil {
			legacyDB, openErr := openChatDB(oldDB)
			if openErr != nil {
				return openErr
			}
			cloudDB := s.db
			s.db = legacyDB
			loadErr := s.load()
			s.db = cloudDB
			legacyDB.Close()
			if loadErr != nil {
				return loadErr
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	} else {
		return err
	}
	if len(s.state.Chats) == 0 {
		return nil
	}
	if !cloudOrganizationID.MatchString(organization) {
		return errors.New("legacy chat import needs a 32-digit organization ID")
	}
	var exists bool
	if err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM warden_cloud.organizations WHERE id=$1)`, organization).Scan(&exists); err != nil || !exists {
		return errors.New("legacy chat import organization is not in the identity database")
	}
	for _, chat := range s.state.Chats {
		if chat.OrganizationID == "" {
			chat.OrganizationID = organization
		}
	}
	for i := range s.state.Ports {
		if s.state.Ports[i].OrganizationID != "" {
			continue
		}
		if chat := s.state.chat(s.state.Ports[i].ChatID); chat != nil {
			s.state.Ports[i].OrganizationID = chat.OrganizationID
		}
	}
	return nil
}

func (s *Store) importCloudAttachments(root string) error {
	base := filepath.Join(root, "attachments")
	chats, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, folder := range chats {
		if !folder.IsDir() {
			continue
		}
		chat := s.state.chat(folder.Name())
		if chat == nil || chat.OrganizationID == "" {
			continue
		}
		dir := filepath.Join(base, folder.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), ".json")
			if !ok || !attachmentID.MatchString(id) {
				continue
			}
			metadata, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return err
			}
			var attachment struct {
				ID string `json:"id"`
			}
			if err = json.Unmarshal(metadata, &attachment); err != nil || attachment.ID != id {
				return fmt.Errorf("invalid legacy attachment %s/%s", folder.Name(), id)
			}
			data, err := os.ReadFile(filepath.Join(dir, id))
			if err != nil {
				return err
			}
			if len(data) == 0 || len(data) > sandbox.MaxAttachmentBytes {
				return fmt.Errorf("legacy attachment %s/%s has invalid size", folder.Name(), id)
			}
			if _, err = s.db.Exec(`INSERT INTO warden_cloud.chat_attachments(organization_id,chat_id,id,metadata,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, chat.OrganizationID, chat.ID, id, metadata, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cloud rows mirror the local SQLite shape. A streamed entry changes one
// item row, rather than rewriting every conversation into a single JSONB row.
func (s *Store) loadCloud(ctx context.Context, instance string) error {
	var globals []byte
	if err := s.db.QueryRowContext(ctx, `SELECT globals FROM warden_cloud.chat_meta WHERE instance=$1`, instance).Scan(&globals); err != nil {
		return err
	}
	st := State{}
	if err := json.Unmarshal(globals, &st); err != nil {
		return err
	}
	st.Chats = []*Chat{}
	sh := newShadow()
	byID := map[string]*Chat{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,position,record FROM warden_cloud.chat_records WHERE instance=$1 ORDER BY position`, instance)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, record string
		var position int
		if err = rows.Scan(&id, &position, &record); err != nil {
			break
		}
		c := &Chat{}
		if err = json.Unmarshal([]byte(record), c); err != nil {
			break
		}
		c.ID = id
		c.Conversation.Entries = []cv.Entry{}
		c.Approvals = []Approval{}
		st.Chats = append(st.Chats, c)
		byID[id] = c
		sh.chats[id] = &chatShadow{position: position, record: record}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT chat_id,kind,item FROM warden_cloud.chat_items WHERE instance=$1 ORDER BY chat_id,kind,seq`, instance)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, item string
		var kind int
		if err = rows.Scan(&id, &kind, &item); err != nil {
			break
		}
		if kind < 0 || kind >= listKinds || byID[id] == nil {
			err = fmt.Errorf("invalid cloud chat item %s/%d", id, kind)
			break
		}
		if err = listTables[kind].load(byID[id], json.RawMessage(item)); err != nil {
			break
		}
		sh.chats[id].lists[kind] = append(sh.chats[id].lists[kind], item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	s.state, s.shadow, s.cloudShadow = st, sh, string(globals)
	return nil
}

func cloudGlobals(st State) string {
	st.Chats = nil
	return encodeAny(st)
}

func (s *Store) persistCloud(_ map[string]bool) (bool, error) {
	globals := cloudGlobals(s.state)
	changed := globals != s.cloudShadow
	next := newShadow()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if changed {
		if _, err = tx.Exec(`INSERT INTO warden_cloud.chat_meta(instance,globals) VALUES($1,$2) ON CONFLICT(instance) DO UPDATE SET globals=EXCLUDED.globals`, s.cloudInstance, globals); err != nil {
			return false, err
		}
	}
	for position, c := range s.state.Chats {
		id := c.ID
		record := encodeRecord(c)
		old := s.shadow.chats[id]
		sh := &chatShadow{position: position, record: record}
		next.chats[id] = sh
		if old == nil || old.position != position || old.record != record {
			if _, err = tx.Exec(`INSERT INTO warden_cloud.chat_records(instance,id,position,record) VALUES($1,$2,$3,$4) ON CONFLICT(instance,id) DO UPDATE SET position=EXCLUDED.position,record=EXCLUDED.record`, s.cloudInstance, id, position, record); err != nil {
				return false, err
			}
			changed = true
		}
		for kind, lt := range listTables {
			var previous []string
			if old != nil {
				previous = old.lists[kind]
			}
			for seq := 0; seq < lt.length(c); seq++ {
				item := encodeAny(lt.item(c, seq))
				sh.lists[kind] = append(sh.lists[kind], item)
				if seq < len(previous) && previous[seq] == item {
					continue
				}
				if _, err = tx.Exec(`INSERT INTO warden_cloud.chat_items(instance,chat_id,kind,seq,item) VALUES($1,$2,$3,$4,$5) ON CONFLICT(instance,chat_id,kind,seq) DO UPDATE SET item=EXCLUDED.item`, s.cloudInstance, id, kind, seq, item); err != nil {
					return false, err
				}
				changed = true
			}
			if len(previous) > lt.length(c) {
				if _, err = tx.Exec(`DELETE FROM warden_cloud.chat_items WHERE instance=$1 AND chat_id=$2 AND kind=$3 AND seq >= $4`, s.cloudInstance, id, kind, lt.length(c)); err != nil {
					return false, err
				}
				changed = true
			}
		}
	}
	// Sort removals for predictable transactions and easier operational review.
	removed := []string{}
	for id := range s.shadow.chats {
		if next.chats[id] == nil {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	for _, id := range removed {
		if _, err = tx.Exec(`DELETE FROM warden_cloud.chat_items WHERE instance=$1 AND chat_id=$2`, s.cloudInstance, id); err != nil {
			return false, err
		}
		if _, err = tx.Exec(`DELETE FROM warden_cloud.chat_records WHERE instance=$1 AND id=$2`, s.cloudInstance, id); err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.shadow, s.cloudShadow = next, globals
	s.writes++
	return true, nil
}
