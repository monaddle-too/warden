package policy

import (
	"path/filepath"
	"testing"
)

// TestPrincipalMigrationBackfillsOwner opens stores created with the
// pre-principal schema and confirms every existing row becomes the owner's
// while new rows record the principal they were written with.
func TestPrincipalMigrationBackfillsOwner(t *testing.T) {
	dir := t.TempDir()
	db, err := openSQLite(filepath.Join(dir, "sharing.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, reason TEXT, status TEXT, created REAL, expires REAL, documents TEXT, delivered INTEGER DEFAULT 0, access TEXT NOT NULL DEFAULT 'read', title TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE repositories (chat TEXT, sandbox TEXT, owner TEXT, app INTEGER, name TEXT, id INTEGER, grant_id TEXT, PRIMARY KEY(chat,sandbox,name))`,
		`CREATE TABLE pull_requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, status TEXT, proposal TEXT, outcome TEXT, delivered INTEGER DEFAULT 0)`,
		`INSERT INTO requests VALUES ('r1','c1','s1','why','granted',1,9999,'[{"id":"doc-a"}]',1,'read','')`,
		`INSERT INTO repositories VALUES ('c1','s1','owner',123,'owner/repo',9,'g1')`,
		`INSERT INTO pull_requests VALUES ('p1','c1','s1','failed','{}','{}',1)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	google, err := openSQLite(filepath.Join(dir, "google.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = google.Exec(`CREATE TABLE credentials (id INTEGER PRIMARY KEY, data TEXT); INSERT INTO credentials VALUES (1,'{"client_id":"x"}')`); err != nil {
		t.Fatal(err)
	}
	google.Close()

	s, err := NewSharing(dir, &fakeGoogle{}, func() float64 { return 1000 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, query := range []string{"SELECT principal FROM requests WHERE id='r1'", "SELECT principal FROM repositories WHERE grant_id='g1'", "SELECT principal FROM pull_requests WHERE id='p1'"} {
		var principal string
		if err = s.DB.QueryRow(query).Scan(&principal); err != nil || principal != OwnerPrincipal {
			t.Fatalf("%s: %q %v", query, principal, err)
		}
	}
	// Behaviour is unchanged: the old grant still applies and lookups do
	// not filter by principal.
	if !s.Active("r1", "c1", "s1") {
		t.Fatal("migrated grant inactive")
	}
	r, err := s.Dispatch("request", map[string]any{"chatID": "c1", "sandboxID": "s1", "reason": "read the plan", "callID": "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Dispatch("request", map[string]any{"chatID": "c1", "sandboxID": "s1", "reason": "read the plan", "callID": "call-2", "principal": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for id, expected := range map[any]string{r["request_id"]: OwnerPrincipal, r2["request_id"]: "alice"} {
		var principal string
		if err = s.DB.QueryRow("SELECT principal FROM requests WHERE id=?", id).Scan(&principal); err != nil || principal != expected {
			t.Fatalf("new request principal: %q %v", principal, err)
		}
	}
	state, _ := s.Dispatch("state", nil)
	if requests, _ := state["requests"].([]any); len(requests) != 3 {
		t.Fatalf("state: %v", state)
	}

	g, err := NewGoogleConnection(dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	var principal string
	if err = g.DB.QueryRow("SELECT principal FROM credentials WHERE id=1").Scan(&principal); err != nil || principal != OwnerPrincipal {
		t.Fatalf("google principal: %q %v", principal, err)
	}
}
