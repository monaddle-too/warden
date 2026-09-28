package durablestate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// OpenSQL imports a legacy SQLite database transactionally into its own
// PostgreSQL schema. Tables remain relational; no database file is stored as
// a blob. Each sandbox has a separate schema to preserve its existing scope.
func OpenSQL(path string) (*sql.DB, error) {
	s, name := Lookup(path)
	if s == nil {
		return nil, fmt.Errorf("unregistered cloud database")
	}
	sum := sha256.Sum256([]byte(s.namespace + ":" + name))
	schema := fmt.Sprintf("warden_policy_%x", sum[:16])
	cfg, e := pgx.ParseConfig(s.dsn)
	if e != nil {
		return nil, e
	}
	cfg.RuntimeParams["search_path"] = schema
	if _, e = s.DB.Exec(`CREATE SCHEMA IF NOT EXISTS ` + schema); e != nil {
		return nil, e
	}
	db := sql.OpenDB(connector{base: stdlib.GetConnector(*cfg)})
	db.SetMaxOpenConns(1)
	if e = importSQL(s, db, path, name); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func importSQL(s *Store, db *sql.DB, path, name string) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var done bool
	if e = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM warden_durable.imports WHERE namespace=? AND name=?)`, s.namespace, name).Scan(&done); e != nil {
		return e
	}
	if done {
		return nil
	}
	if _, e = os.Stat(path); e == nil {
		// SQLite may need to rebuild its WAL index even for a read-only
		// connection. Copy the database and WAL to disposable memory-backed
		// scratch so importing never changes the legacy service volume.
		scratch, e := os.MkdirTemp("", "warden-sql-import-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(scratch)
		copyPath := filepath.Join(scratch, "legacy.sqlite")
		for _, suffix := range []string{"", "-wal"} {
			source, err := os.Open(path + suffix)
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			if err != nil {
				return err
			}
			target, err := os.OpenFile(copyPath+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				source.Close()
				return err
			}
			_, err = io.Copy(target, source)
			source.Close()
			closeErr := target.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		legacy, e := sql.Open("sqlite", "file:"+copyPath+"?mode=ro")
		if e != nil {
			return e
		}
		defer legacy.Close()
		legacy.SetMaxOpenConns(1)
		rows, e := legacy.Query(`SELECT name,sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if e != nil {
			return e
		}
		type table struct{ name, ddl string }
		var tables []table
		for rows.Next() {
			var t table
			if e = rows.Scan(&t.name, &t.ddl); e != nil {
				rows.Close()
				return e
			}
			tables = append(tables, t)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, t := range tables {
			if !identifier.MatchString(t.name) {
				return fmt.Errorf("invalid legacy table name")
			}
			if _, e = tx.Exec(t.ddl); e != nil {
				return fmt.Errorf("import %s schema: %w", t.name, e)
			}
			data, e := legacy.Query(`SELECT * FROM "` + t.name + `"`)
			if e != nil {
				return e
			}
			cols, e := data.Columns()
			if e != nil {
				data.Close()
				return e
			}
			quoted := make([]string, len(cols))
			marks := make([]string, len(cols))
			for i, c := range cols {
				if !identifier.MatchString(c) {
					data.Close()
					return fmt.Errorf("invalid legacy column")
				}
				quoted[i] = `"` + c + `"`
				marks[i] = "?"
			}
			insert := `INSERT INTO "` + t.name + `" (` + strings.Join(quoted, ",") + `) VALUES (` + strings.Join(marks, ",") + `)`
			for data.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if e = data.Scan(ptrs...); e != nil {
					data.Close()
					return e
				}
				if _, e = tx.Exec(insert, values...); e != nil {
					data.Close()
					return fmt.Errorf("import %s row: %w", t.name, e)
				}
			}
			e = data.Err()
			data.Close()
			if e != nil {
				return e
			}
			if strings.Contains(strings.ToUpper(t.ddl), "AUTOINCREMENT") {
				if _, e = tx.Exec(`SELECT setval(pg_get_serial_sequence(?, 'id'), COALESCE((SELECT MAX(id) FROM "`+t.name+`"),0)+1, false)`, t.name); e != nil {
					return e
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO warden_durable.imports(namespace,name) VALUES(?,?)`, s.namespace, name); e != nil {
		return e
	}
	return tx.Commit()
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var types = regexp.MustCompile(`\b(INTEGER PRIMARY KEY AUTOINCREMENT|INTEGER|REAL|BLOB)\b`)

// Translate is deliberately limited to SQL used by the policy stores. Values
// remain bound parameters; question marks inside quoted literals are untouched.
func Translate(q string) string {
	q = strings.TrimSpace(q)
	upper := strings.ToUpper(q)
	if strings.HasPrefix(upper, "PRAGMA TABLE_INFO(") {
		table := strings.TrimSuffix(q[len("PRAGMA table_info("):], ")")
		if identifier.MatchString(table) {
			return `SELECT ordinal_position-1,column_name,data_type,CASE WHEN is_nullable='NO' THEN 1 ELSE 0 END,column_default,0 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='` + table + `' ORDER BY ordinal_position`
		}
	}
	if strings.HasPrefix(upper, "PRAGMA ") {
		return "SELECT 1"
	}
	if strings.HasPrefix(upper, "CREATE TABLE") || strings.HasPrefix(upper, "ALTER TABLE") {
		q = types.ReplaceAllStringFunc(q, func(t string) string {
			switch t {
			case "INTEGER PRIMARY KEY AUTOINCREMENT":
				return "BIGSERIAL PRIMARY KEY"
			case "INTEGER":
				return "BIGINT"
			case "REAL":
				return "DOUBLE PRECISION"
			case "BLOB":
				return "BYTEA"
			}
			return t
		})
	}
	if strings.HasPrefix(upper, "INSERT OR IGNORE INTO ") {
		q = strings.Replace(q, "INSERT OR IGNORE INTO ", "INSERT INTO ", 1) + " ON CONFLICT DO NOTHING"
	}
	if strings.HasPrefix(upper, "INSERT OR REPLACE INTO ") {
		q = strings.Replace(q, "INSERT OR REPLACE INTO ", "INSERT INTO ", 1)
		switch {
		case regexp.MustCompile(`(?i)INTO\s+credentials\s*\(`).MatchString(q):
			q += " ON CONFLICT(id) DO UPDATE SET data=EXCLUDED.data,principal=EXCLUDED.principal"
		case regexp.MustCompile(`(?i)INTO\s+blocked_documents\s*(\(|VALUES)`).MatchString(q):
			q += " ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,blocked=EXCLUDED.blocked"
		case regexp.MustCompile(`(?i)INTO\s+ci_grants\s*\(`).MatchString(q):
			q += " ON CONFLICT(sandbox,repository) DO UPDATE SET expires=EXCLUDED.expires,actor=EXCLUDED.actor"
		}
	}
	var b strings.Builder
	param := 0
	var quote byte
	for i := 0; i < len(q); i++ {
		c := q[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				if i+1 < len(q) && q[i+1] == quote {
					i++
					b.WriteByte(q[i])
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		}
		if c == '?' {
			param++
			fmt.Fprintf(&b, "$%d", param)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

type connector struct{ base driver.Connector }

func (c connector) Driver() driver.Driver { return c.base.Driver() }
func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.base.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return &conn{Conn: v}, nil
}

type conn struct{ driver.Conn }

func (c *conn) Prepare(q string) (driver.Stmt, error) { return c.Conn.Prepare(Translate(q)) }
func (c *conn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, Translate(q))
}
func (c *conn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, Translate(q), a)
}
func (c *conn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, Translate(q), a)
}
func (c *conn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *conn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }
func (c *conn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *conn) CheckNamedValue(v *driver.NamedValue) error {
	if check, ok := c.Conn.(driver.NamedValueChecker); ok {
		return check.CheckNamedValue(v)
	}
	return driver.ErrSkip
}
