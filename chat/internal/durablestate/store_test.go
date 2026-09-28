package durablestate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An explicit opt-in rehearsal against a read-only volume snapshot. It uses a
// unique namespace and removes only the database schemas it created.
func TestCloudLegacyVolume(t *testing.T) {
	root, dsn := os.Getenv("WARDEN_TEST_LEGACY_ROOT"), os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if root == "" || dsn == "" {
		t.Skip("requires snapshot root and disposable database access")
	}
	namespace := fmt.Sprintf("rehearsal-%d", time.Now().UnixNano())
	store, err := Open(root, dsn, namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var schemas []string
	defer func() {
		for _, schema := range schemas {
			if _, err := store.DB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
				t.Error(err)
			}
		}
		for _, table := range []string{"objects", "audit", "imports"} {
			if _, err := store.DB.Exec(`DELETE FROM warden_durable.`+table+` WHERE namespace=$1`, namespace); err != nil {
				t.Error(err)
			}
		}
	}()
	tables, records := 0, 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sqlite") {
			return nil
		}
		cloud, err := OpenSQL(path)
		if err != nil {
			return err
		}
		defer cloud.Close()
		var schema string
		if err := cloud.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
			return err
		}
		schemas = append(schemas, schema)
		// The snapshot has no writers; WAL files and any preexisting index are
		// immutable for the duration of this count comparison.
		copyPath := filepath.Join(t.TempDir(), "legacy.sqlite")
		for _, suffix := range []string{"", "-wal"} {
			b, err := os.ReadFile(path + suffix)
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			if err != nil {
				return err
			}
			if err = os.WriteFile(copyPath+suffix, b, 0600); err != nil {
				return err
			}
		}
		legacy, err := sql.Open("sqlite", "file:"+copyPath+"?mode=ro")
		if err != nil {
			return err
		}
		defer legacy.Close()
		rows, err := legacy.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, name := range names {
			if !identifier.MatchString(name) {
				return fmt.Errorf("invalid table")
			}
			var before, after int
			q := `SELECT count(*) FROM "` + name + `"`
			if err = legacy.QueryRow(q).Scan(&before); err != nil {
				return err
			}
			if err = cloud.QueryRow(q).Scan(&after); err != nil {
				return err
			}
			if before != after {
				return fmt.Errorf("table %s row count changed: %d != %d", name, before, after)
			}
			tables++
			records += after
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var objects, audit int
	if err = store.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM warden_durable.objects WHERE namespace=$1`, namespace).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if err = store.DB.QueryRow(`SELECT count(*) FROM warden_durable.audit WHERE namespace=$1`, namespace).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	t.Logf("Verified %d relational tables, %d relational records, %d named objects and %d audit events", tables, records, objects, audit)
}
