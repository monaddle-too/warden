package policy

import (
	"database/sql"
	"encoding/json"
)

// scrubRequests removes captured bodies from approval summaries and SQLite
// free pages, including legacy records written before body retention ended.
func scrubRequests(db *sql.DB) (int, error) {
	if _, err := db.Exec("PRAGMA secure_delete=ON"); err != nil {
		return 0, err
	}
	rows, err := db.Query("SELECT id,summary FROM requests")
	if err != nil {
		return 0, err
	}
	type update struct{ id, summary string }
	var updates []update
	for rows.Next() {
		var id, summary string
		if err = rows.Scan(&id, &summary); err != nil {
			rows.Close()
			return 0, err
		}
		var parsed any
		if json.Unmarshal([]byte(summary), &parsed) != nil {
			continue
		}
		cleaned := WithoutBodies(parsed)
		if !jsonEqual(cleaned, parsed) {
			updates = append(updates, update{id, Dumps(cleaned)})
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, u := range updates {
		if _, err = db.Exec("UPDATE requests SET summary=? WHERE id=?", u.summary, u.id); err != nil {
			return 0, err
		}
	}
	if len(updates) > 0 {
		for _, statement := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "VACUUM", "PRAGMA wal_checkpoint(TRUNCATE)"} {
			if _, err = db.Exec(statement); err != nil {
				return 0, err
			}
		}
	}
	return len(updates), nil
}
