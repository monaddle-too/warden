// Package durablestate keeps cloud control-plane records in PostgreSQL.
// Paths are stable record names, not files in cloud mode. Unregistered roots
// retain the local installation's filesystem behavior.
package durablestate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	DB                   *sql.DB
	root, namespace, dsn string
	unlock               func()
}

var roots sync.Map

// Open acquires exclusive database ownership before importing any legacy
// records. Import and its completion marker commit together. Legacy files
// are read only and are never used again after the marker exists.
func Open(root, dsn, namespace string) (*Store, error) {
	if namespace == "" {
		return nil, errors.New("durable state namespace required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{DB: db, root: root, namespace: namespace, dsn: dsn}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.unlock, err = Lock(dsn, namespace)
	if err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(8173002)`); err != nil {
		return fail(err)
	}
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS warden_durable`,
		`CREATE TABLE IF NOT EXISTS warden_durable.objects(namespace TEXT NOT NULL, name TEXT NOT NULL, data BYTEA NOT NULL, PRIMARY KEY(namespace,name))`,
		`CREATE TABLE IF NOT EXISTS warden_durable.imports(namespace TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(namespace,name))`,
		`CREATE TABLE IF NOT EXISTS warden_durable.audit(namespace TEXT NOT NULL, stream TEXT NOT NULL, ordinal BIGINT GENERATED ALWAYS AS IDENTITY, data TEXT NOT NULL, PRIMARY KEY(namespace,stream,ordinal))`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fail(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	if err = s.importFiles(ctx); err != nil {
		return fail(err)
	}
	if _, loaded := roots.LoadOrStore(root, s); loaded {
		return fail(errors.New("cloud root already registered"))
	}
	if err = s.importDatabases(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	roots.CompareAndDelete(s.root, s)
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return s.DB.Close()
}
func Lookup(path string) (*Store, string) {
	path, _ = filepath.Abs(path)
	var found *Store
	var name string
	roots.Range(func(k, v any) bool {
		root := k.(string)
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			found = v.(*Store)
			name = strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))
			return false
		}
		return true
	})
	return found, filepath.ToSlash(name)
}
func IsCloud(path string) bool { s, _ := Lookup(path); return s != nil }
func (s *Store) importFiles(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM warden_durable.imports WHERE namespace=$1 AND name='files')`, s.namespace).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	err = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, e error) error {
		if os.IsNotExist(e) && path == s.root {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("legacy state contains symlink: %s", path)
		}
		name, _ := filepath.Rel(s.root, path)
		name = filepath.ToSlash(name)
		if strings.Contains(name, ".sqlite") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.HasSuffix(name, "events.jsonl") {
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO warden_durable.audit(namespace,stream,data) VALUES($1,$2,$3)`, s.namespace, name, line); e != nil {
					return e
				}
			}
			return nil
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO warden_durable.objects(namespace,name,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.namespace, name, raw)
		return e
	})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO warden_durable.imports VALUES($1,'files')`, s.namespace); err != nil {
		return err
	}
	return tx.Commit()
}
func ReadFile(path string) ([]byte, error) {
	s, n := Lookup(path)
	if s == nil {
		return os.ReadFile(path)
	}
	var b []byte
	err := s.DB.QueryRow(`SELECT data FROM warden_durable.objects WHERE namespace=$1 AND name=$2`, s.namespace, n).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		err = &os.PathError{Op: "read", Path: path, Err: os.ErrNotExist}
	}
	return b, err
}
func WriteFile(path string, b []byte, mode fs.FileMode) error {
	s, n := Lookup(path)
	if s == nil {
		return os.WriteFile(path, b, mode)
	}
	_, err := s.DB.Exec(`INSERT INTO warden_durable.objects(namespace,name,data) VALUES($1,$2,$3) ON CONFLICT(namespace,name) DO UPDATE SET data=EXCLUDED.data`, s.namespace, n, b)
	return err
}
func MkdirAll(path string, mode fs.FileMode) error {
	if IsCloud(path) {
		return nil
	}
	return os.MkdirAll(path, mode)
}
func Chmod(path string, mode fs.FileMode) error {
	if IsCloud(path) {
		return nil
	}
	return os.Chmod(path, mode)
}

type info struct {
	name string
	size int64
	dir  bool
}

func (i info) Name() string { return i.name }
func (i info) Size() int64  { return i.size }
func (i info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0700
	}
	return 0600
}
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }
func Stat(path string) (fs.FileInfo, error) {
	s, n := Lookup(path)
	if s == nil {
		return os.Stat(path)
	}
	if n == "" {
		return info{filepath.Base(path), 0, true}, nil
	}
	b, err := ReadFile(path)
	if err == nil {
		return info{filepath.Base(path), int64(len(b)), false}, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	var exists bool
	err = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM warden_durable.objects WHERE namespace=$1 AND starts_with(name,$2))`, s.namespace, n+"/").Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists {
		return info{filepath.Base(path), 0, true}, nil
	}
	return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
}
func Remove(path string) error {
	s, n := Lookup(path)
	if s == nil {
		return os.Remove(path)
	}
	r, e := s.DB.Exec(`DELETE FROM warden_durable.objects WHERE namespace=$1 AND name=$2`, s.namespace, n)
	if e != nil {
		return e
	}
	rows, _ := r.RowsAffected()
	if rows == 0 {
		return &os.PathError{Op: "remove", Path: path, Err: os.ErrNotExist}
	}
	return nil
}
func RemoveAll(path string) error {
	s, n := Lookup(path)
	if s == nil {
		return os.RemoveAll(path)
	}
	_, e := s.DB.Exec(`DELETE FROM warden_durable.objects WHERE namespace=$1 AND (name=$2 OR starts_with(name,$3))`, s.namespace, n, n+"/")
	return e
}
func Rename(old, new string) error {
	s, a := Lookup(old)
	t, b := Lookup(new)
	if s == nil && t == nil {
		return os.Rename(old, new)
	}
	if s == nil || s != t {
		return errors.New("cross-store rename refused")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var exists bool
	if e = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM warden_durable.objects WHERE namespace=$1 AND (name=$2 OR starts_with(name,$3)))`, s.namespace, a, a+"/").Scan(&exists); e != nil {
		return e
	}
	if !exists {
		return &os.PathError{Op: "rename", Path: old, Err: os.ErrNotExist}
	}
	if _, e = tx.Exec(`DELETE FROM warden_durable.objects WHERE namespace=$1 AND name=$2`, s.namespace, b); e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE warden_durable.objects SET name=$3 || substring(name FROM length($2)+1) WHERE namespace=$1 AND (name=$2 OR starts_with(name,$4))`, s.namespace, a, b, a+"/"); e != nil {
		return e
	}
	return tx.Commit()
}
func AppendAudit(path, line string) error {
	s, n := Lookup(path)
	if s == nil {
		return errors.New("cloud audit requires registered root")
	}
	_, e := s.DB.Exec(`INSERT INTO warden_durable.audit(namespace,stream,data) VALUES($1,$2,$3)`, s.namespace, n, line)
	return e
}

// Lock is a database-wide singleton lease. Losing its connection terminates
// the service rather than allowing it to reconnect as an unfenced writer.
func Lock(dsn, namespace string) (func(), error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, errors.New("cloud state requires PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	var owned bool
	err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "warden-state:"+namespace).Scan(&owned)
	if err != nil || !owned {
		conn.Close()
		db.Close()
		if err == nil {
			err = errors.New("another service owns this cloud state")
		}
		return nil, err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				check, finish := context.WithTimeout(context.Background(), 3*time.Second)
				err := conn.PingContext(check)
				finish()
				if err != nil {
					select {
					case <-stop:
						return
					default:
					}
					slog.Error("cloud state ownership connection lost; stopping service", "namespace", namespace)
					os.Exit(1)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "warden-state:"+namespace)
			conn.Close()
			db.Close()
		})
	}, nil
}

// WriteBatch commits related immutable artifacts together.
func WriteBatch(records map[string][]byte) error {
	var store *Store
	rows := map[string][]byte{}
	for path, data := range records {
		s, name := Lookup(path)
		if s == nil || (store != nil && store != s) {
			return errors.New("batch requires one cloud state root")
		}
		store = s
		rows[name] = data
	}
	if store == nil {
		return nil
	}
	tx, err := store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, data := range rows {
		if _, err = tx.Exec(`INSERT INTO warden_durable.objects(namespace,name,data) VALUES($1,$2,$3) ON CONFLICT(namespace,name) DO UPDATE SET data=EXCLUDED.data`, store.namespace, name, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rotate atomically retires the current record prefix and promotes its replacement.
func Rotate(current, next, retired string) error {
	s, a := Lookup(current)
	t, b := Lookup(next)
	u, c := Lookup(retired)
	if s == nil || s != t || s != u {
		return errors.New("rotation requires one cloud root")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, move := range [][2]string{{a, c}, {b, a}} {
		if _, err = tx.Exec(`UPDATE warden_durable.objects SET name=$3 || substring(name FROM length($2)+1) WHERE namespace=$1 AND starts_with(name,$4)`, s.namespace, move[0], move[1], move[0]+"/"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Import every legacy database, including stopped workspaces which are opened
// lazily by the policy registry. A completed import never needs the old volume.
func (s *Store) importDatabases(ctx context.Context) error {
	var done bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM warden_durable.imports WHERE namespace=$1 AND name='all-sql')`, s.namespace).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == s.root {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sqlite") {
			return nil
		}
		// Chat and runner have their own relational importers.
		if filepath.Base(path) == "runner.sqlite" || filepath.Base(path) == "chats.sqlite" {
			return nil
		}
		db, err := OpenSQL(path)
		if err != nil {
			return fmt.Errorf("import legacy database %s: %w", filepath.Base(path), err)
		}
		return db.Close()
	})
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO warden_durable.imports(namespace,name) VALUES($1,'all-sql')`, s.namespace)
	return err
}
