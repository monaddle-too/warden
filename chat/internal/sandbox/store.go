package sandbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"warden/chat/internal/rowstore"
)

// The runner's inventory lives in <root>/runner.sqlite, one table per
// map of managedState plus the cancelled-run tombstones
// (docs/runner-sqlite-store-plan.md). A save writes the rows whose
// encoding changed since the last save, in one transaction; the
// pre-database managed-v2.json and cancelled-runs/ markers are imported
// on the first start and left renamed as backups.

const runnerDBFile = "runner.sqlite"

const runnerSchemaVersion = 1

var runnerSchema = []string{
	`CREATE TABLE IF NOT EXISTS sandboxes (id TEXT PRIMARY KEY, runtime_name TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chats (id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cancelled (key TEXT PRIMARY KEY, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS attachments (id TEXT PRIMARY KEY, chat_id TEXT NOT NULL, sandbox_id TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS publications (id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS calls (key TEXT PRIMARY KEY, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS spares (name TEXT PRIMARY KEY, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cancelled_runs (hash TEXT PRIMARY KEY)`,
}

// runnerStore is the open database and the shadow of each table.
type runnerStore struct {
	mu     sync.Mutex
	db     *sql.DB
	shadow [managedTables]map[string]string
}

const (
	tableSandboxes = iota
	tableChats
	tableCancelled
	tableAttachments
	tablePublications
	tableCalls
	tableSpares
	managedTables
)

// tables describes each map of managedState as a table; the queried
// columns come from the map's current value.
func (w *Worker) tables() [managedTables]rowstore.Table {
	m := w.managed
	return [managedTables]rowstore.Table{
		tableSandboxes: {Name: "sandboxes", Key: "id", JSON: "record", Columns: []string{"runtime_name", "state"}, Values: func(id string) []any {
			s := m.Sandboxes[id]
			return []any{s.RuntimeName, s.State}
		}},
		tableChats:     {Name: "chats", Key: "id", JSON: "record", Columns: []string{"sandbox_id"}, Values: func(id string) []any { return []any{m.Chats[id].SandboxID} }},
		tableCancelled: {Name: "cancelled", Key: "key", JSON: "record"},
		tableAttachments: {Name: "attachments", Key: "id", JSON: "record", Columns: []string{"chat_id", "sandbox_id", "state"}, Values: func(id string) []any {
			a := m.Attachments[id]
			return []any{a.ChatID, a.SandboxID, a.State}
		}},
		tablePublications: {Name: "publications", Key: "id", JSON: "record", Columns: []string{"sandbox_id", "state"}, Values: func(id string) []any {
			p := m.Publications[id]
			return []any{p.SandboxID, p.State}
		}},
		tableCalls:  {Name: "calls", Key: "key", JSON: "record"},
		tableSpares: {Name: "spares", Key: "name", JSON: "record"},
	}
}

// openStore opens the database once. It is safe to call from any
// goroutine; the worker's other locks are not needed.
func (w *Worker) openStore() (*runnerStore, error) {
	w.storeOnce.Do(func() {
		if err := os.MkdirAll(w.Root, 0o700); err != nil {
			w.storeErr = err
			return
		}
		db, err := rowstore.Open(filepath.Join(w.Root, runnerDBFile), runnerSchema, runnerSchemaVersion)
		if err != nil {
			w.storeErr = err
			return
		}
		st := &runnerStore{db: db}
		for i := range st.shadow {
			st.shadow[i] = map[string]string{}
		}
		w.store = st
	})
	return w.store, w.storeErr
}

// closeStore releases the database.
func (w *Worker) closeStore() {
	if w.store != nil && w.store.db != nil {
		w.store.db.Close()
	}
}

// loadManagedLocked fills w.managed from the database, importing the
// pre-database files on the first start (managed-v2.json into the
// tables, the cancelled-runs/ marker names into cancelled_runs). Called
// with w.mu held.
func (w *Worker) loadManagedLocked() error {
	st, err := w.openStore()
	if err != nil {
		return err
	}
	legacy := filepath.Join(w.Root, "managed-v2.json")
	var n int
	if err = st.db.QueryRow(`SELECT count(*) FROM sandboxes`).Scan(&n); err != nil {
		return err
	}
	imported := false
	if n == 0 {
		b, err := os.ReadFile(legacy)
		if err == nil {
			if err = json.Unmarshal(b, w.managed); err != nil {
				return fmt.Errorf("invalid worker registry: %w", err)
			}
			imported = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !imported {
		if st.shadow[tableSandboxes], err = rowstore.Load(st.db, w.tables()[tableSandboxes], w.managed.Sandboxes); err != nil {
			return err
		}
		if st.shadow[tableChats], err = rowstore.Load(st.db, w.tables()[tableChats], w.managed.Chats); err != nil {
			return err
		}
		if st.shadow[tableCancelled], err = rowstore.Load(st.db, w.tables()[tableCancelled], w.managed.Cancelled); err != nil {
			return err
		}
		if st.shadow[tableAttachments], err = rowstore.Load(st.db, w.tables()[tableAttachments], w.managed.Attachments); err != nil {
			return err
		}
		if st.shadow[tablePublications], err = rowstore.Load(st.db, w.tables()[tablePublications], w.managed.Publications); err != nil {
			return err
		}
		if st.shadow[tableCalls], err = rowstore.Load(st.db, w.tables()[tableCalls], w.managed.Calls); err != nil {
			return err
		}
		if st.shadow[tableSpares], err = rowstore.Load(st.db, w.tables()[tableSpares], w.managed.Spares); err != nil {
			return err
		}
	}
	// The cancelled-run markers: one file per tombstone, named by the
	// run key's hash, which is what the table holds.
	markers := filepath.Join(w.Root, "cancelled-runs")
	if names, err := os.ReadDir(markers); err == nil {
		tx, err := st.db.Begin()
		if err != nil {
			return err
		}
		for _, f := range names {
			hash := f.Name()
			if len(hash) > 5 && hash[len(hash)-5:] == ".json" {
				hash = hash[:len(hash)-5]
			}
			if _, err = tx.Exec(`INSERT OR IGNORE INTO cancelled_runs (hash) VALUES (?)`, hash); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if err = os.Rename(markers, markers+".migrated"); err != nil {
			return err
		}
	}
	if imported {
		if err = w.saveManagedLocked(); err != nil {
			return err
		}
		if err = os.Rename(legacy, legacy+".migrated"); err != nil {
			return err
		}
	}
	return nil
}

// writeManagedLocked writes the rows of every table whose encoding
// changed since the last write, in one transaction. Called with w.mu
// held.
func (w *Worker) writeManagedLocked() error {
	st, err := w.openStore()
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	current := [managedTables]map[string]string{
		tableSandboxes:    rowstore.Encode(w.managed.Sandboxes),
		tableChats:        rowstore.Encode(w.managed.Chats),
		tableCancelled:    rowstore.Encode(w.managed.Cancelled),
		tableAttachments:  rowstore.Encode(w.managed.Attachments),
		tablePublications: rowstore.Encode(w.managed.Publications),
		tableCalls:        rowstore.Encode(w.managed.Calls),
		tableSpares:       rowstore.Encode(w.managed.Spares),
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	var applies []func()
	changed := false
	for i, t := range w.tables() {
		apply, wrote, err := rowstore.Sync(tx, t, st.shadow[i], current[i])
		if err != nil {
			tx.Rollback()
			return err
		}
		applies = append(applies, apply)
		changed = changed || wrote
	}
	if !changed {
		tx.Rollback()
		return nil
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, a := range applies {
		a()
	}
	return nil
}

// recordCancelledRun writes a run's tombstone: it survives a restart and
// never authorizes stopping a different run.
func (w *Worker) recordCancelledRun(hash string) error {
	st, err := w.openStore()
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	_, err = st.db.Exec(`INSERT OR IGNORE INTO cancelled_runs (hash) VALUES (?)`, hash)
	return err
}

// cancelledRunRecorded says whether a run's tombstone was written.
func (w *Worker) cancelledRunRecorded(hash string) (bool, error) {
	st, err := w.openStore()
	if err != nil {
		return false, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var n int
	if err = st.db.QueryRow(`SELECT count(*) FROM cancelled_runs WHERE hash = ?`, hash).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
