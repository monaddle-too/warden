package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Cloud runner inventory uses PostgreSQL. The old service volume is read once
// for migration and remains intact for rollback.
func (w *Worker) openCloudStore() (*runnerStore, error) {
	if !strings.HasPrefix(w.DatabaseURL, "postgres://") && !strings.HasPrefix(w.DatabaseURL, "postgresql://") {
		return nil, errors.New("cloud runner state requires PostgreSQL")
	}
	db, err := sql.Open("pgx", w.DatabaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err = ensureCloudRunnerSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &runnerStore{db: db, cloud: true}, nil
}

func ensureCloudRunnerSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`SET LOCAL lock_timeout='5s'`,
		`SET LOCAL statement_timeout='30s'`,
		`SELECT pg_advisory_xact_lock(8173001)`,
		`CREATE SCHEMA IF NOT EXISTS warden_cloud`,
		`CREATE TABLE IF NOT EXISTS warden_cloud.runner_state(id TEXT PRIMARY KEY,state JSONB NOT NULL,updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS warden_cloud.runner_cancelled_runs(instance TEXT NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(instance,hash))`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (w *Worker) cloudStoreID() string {
	if w.Instance != "" {
		return w.Instance
	}
	return "main"
}

func (w *Worker) loadManagedCloud(st *runnerStore) error {
	var saved []byte
	err := st.db.QueryRow(`SELECT state FROM warden_cloud.runner_state WHERE id=$1`, w.cloudStoreID()).Scan(&saved)
	switch {
	case err == nil:
		if err = json.Unmarshal(saved, w.managed); err != nil {
			return fmt.Errorf("cloud runner state: %w", err)
		}
		st.cloudShadow = string(saved)
	case errors.Is(err, sql.ErrNoRows):
		legacy := filepath.Join(w.Root, "managed-v2.json")
		data, readErr := os.ReadFile(legacy)
		if readErr == nil {
			if err = json.Unmarshal(data, w.managed); err != nil {
				return fmt.Errorf("legacy runner state: %w", err)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		} else if _, statErr := os.Stat(filepath.Join(w.Root, runnerDBFile)); statErr == nil {
			return errors.New("cloud runner import found SQLite but no JSON backup; export the runner inventory first")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err = w.importCloudCancelledRuns(st); err != nil {
			return err
		}
		if err = w.writeManagedCloud(st); err != nil {
			return err
		}
	default:
		return err
	}
	return nil
}

func (w *Worker) importCloudCancelledRuns(st *runnerStore) error {
	entries, err := os.ReadDir(filepath.Join(w.Root, "cancelled-runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		hash := strings.TrimSuffix(entry.Name(), ".json")
		if _, err = tx.Exec(`INSERT INTO warden_cloud.runner_cancelled_runs(instance,hash) VALUES($1,$2) ON CONFLICT DO NOTHING`, w.cloudStoreID(), hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (w *Worker) writeManagedCloud(st *runnerStore) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	data, err := json.Marshal(w.managed)
	if err != nil {
		return err
	}
	if string(data) == st.cloudShadow {
		return nil
	}
	_, err = st.db.Exec(`INSERT INTO warden_cloud.runner_state(id,state,updated_at) VALUES($1,$2,now()) ON CONFLICT(id) DO UPDATE SET state=EXCLUDED.state,updated_at=EXCLUDED.updated_at`, w.cloudStoreID(), data)
	if err != nil {
		return err
	}
	st.cloudShadow = string(data)
	return nil
}
