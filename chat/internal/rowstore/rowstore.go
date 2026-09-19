// Package rowstore is what Warden's services share to keep an in-memory
// state in a private SQLite database row by row: a database opened with
// one connection, WAL and every commit fsynced, and a way to write only
// the rows of a map-shaped table whose encoding changed since they were
// last written (the "shadow").
package rowstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens (creating it) the database at path, applies the schema
// statements, and checks the schema version recorded in the meta table
// (written on creation). The file is made private; SQLite gives its WAL
// and shared-memory files the same mode.
func Open(path string, schema []string, version int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, error) { db.Close(); return nil, err }
	if err = db.Ping(); err != nil {
		return fail(err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return fail(err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fail(err)
	}
	for _, stmt := range schema {
		if _, err = db.Exec(stmt); err != nil {
			return fail(err)
		}
	}
	var got string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema'`).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		if _, err = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema', ?)`, fmt.Sprint(version)); err != nil {
			return fail(err)
		}
	case err != nil:
		return fail(err)
	case got != fmt.Sprint(version):
		return fail(fmt.Errorf("unsupported data version %s in %s", got, path))
	}
	return db, nil
}

// Table describes a map-shaped table: rows keyed by Key, the JSON of the
// value in JSON, and Columns the queried columns beside them, whose values
// Values gives for a key (in the same order).
type Table struct {
	Name    string
	Key     string
	JSON    string
	Columns []string
	Values  func(id string) []any
}

// Encode is the encodings of a map's values by key, what Sync compares
// with the shadow.
func Encode[V any](m map[string]V) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = string(b)
	}
	return out
}

// Sync writes, in tx, the rows of t whose encoding in current differs
// from shadow (inserting or replacing them) and deletes the keys shadow
// holds that current lacks. It reports whether it wrote anything and
// returns apply, which records the writes in shadow once tx committed.
func Sync(tx *sql.Tx, t Table, shadow, current map[string]string) (apply func(), changed bool, err error) {
	var applies []func()
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		enc := current[k]
		if old, ok := shadow[k]; ok && old == enc {
			continue
		}
		args := []any{k}
		cols := t.Key
		if t.Values != nil {
			args = append(args, t.Values(k)...)
			cols += ", " + strings.Join(t.Columns, ", ")
		}
		args = append(args, enc)
		cols += ", " + t.JSON
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		if _, err = tx.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s) VALUES (%s)`, t.Name, cols, marks), args...); err != nil {
			return nil, false, err
		}
		k, enc := k, enc
		applies = append(applies, func() { shadow[k] = enc })
	}
	for k := range shadow {
		if _, ok := current[k]; ok {
			continue
		}
		if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, t.Name, t.Key), k); err != nil {
			return nil, false, err
		}
		k := k
		applies = append(applies, func() { delete(shadow, k) })
	}
	return func() {
		for _, a := range applies {
			a()
		}
	}, len(applies) > 0, nil
}

// Load reads every row of t into out (decoding the JSON column into V)
// and returns the encodings read, the shadow to start from.
func Load[V any](db *sql.DB, t Table, out map[string]V) (map[string]string, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s, %s FROM %s`, t.Key, t.JSON, t.Name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shadow := map[string]string{}
	for rows.Next() {
		var k, enc string
		if err = rows.Scan(&k, &enc); err != nil {
			return nil, err
		}
		var v V
		if err = json.Unmarshal([]byte(enc), &v); err != nil {
			return nil, fmt.Errorf("%s %s: %w", t.Name, k, err)
		}
		out[k] = v
		shadow[k] = enc
	}
	return shadow, rows.Err()
}
