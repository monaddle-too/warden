package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"warden/chat/internal/durablestate"
)

func TestCloudPolicyImportAndRestart(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root := t.TempDir()
	engineRoot := filepath.Join(root, "sandboxes", "one")
	ca, err := LoadOrCreateGatewayCA(filepath.Join(root, HostCADir))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ca.Fingerprint()
	e := newTestEngine(t, engineRoot, nil)
	if _, err = e.DB.Exec(`INSERT INTO requests(id,summary,status,created) VALUES('old','{}','pending',1)`); err != nil {
		t.Fatal(err)
	}
	if err = e.SetNetwork(false); err != nil {
		t.Fatal(err)
	}
	e.Close()
	g, err := NewGoogleConnection(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	sharing, err := NewSharing(root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sharing.DB.Exec(`INSERT INTO repository_events(at,sandbox,actor,kind,detail) VALUES(1,'s','a','test','{}')`); err != nil {
		t.Fatal(err)
	}
	sharing.Close()
	namespace := "test-" + UUID4()
	store, err := durablestate.Open(root, dsn, namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	if other, err := durablestate.Open(t.TempDir(), dsn, namespace); err == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	e = newTestEngine(t, engineRoot, nil)
	var status string
	if err = e.DB.QueryRow(`SELECT status FROM requests WHERE id=?`, "old").Scan(&status); err != nil || status != "stale" {
		t.Fatalf("migration: %q %v", status, err)
	}
	if e.networkEnabled {
		t.Fatal("disconnect lost")
	}
	if err = e.SetNetwork(true); err != nil {
		t.Fatal(err)
	}
	if err = e.SetEgressMode("public"); err != nil {
		t.Fatal(err)
	}
	ca, _, err = PrepareGatewayCA(root, 0, time.Now())
	if err != nil || ca.Fingerprint() != fingerprint {
		t.Fatalf("CA changed: %v", err)
	}
	sharing, err = NewSharing(root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sharing.DB.Exec(`INSERT OR REPLACE INTO blocked_documents VALUES (?,?,?)`, "doc", "Doc", 1.5); err != nil {
		t.Fatal(err)
	}
	if _, err = sharing.DB.Exec(`INSERT INTO repository_events(at,sandbox,actor,kind,detail) VALUES(2,'s','a','test','{}')`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = sharing.DB.QueryRow(`SELECT count(*) FROM repository_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("events: %d %v", count, err)
	}
	if _, err = sharing.DB.Exec(`INSERT OR REPLACE INTO ci_grants(sandbox,repository,expires,actor) VALUES(?,?,?,?)`, "s", "r", 1.0, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err = sharing.DB.Exec(`INSERT OR REPLACE INTO ci_grants(sandbox,repository,expires,actor) VALUES(?,?,?,?)`, "s", "r", 2.0, "b"); err != nil {
		t.Fatal(err)
	}
	g, err = NewGoogleConnection(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"old", "new"} {
		if _, err = g.DB.Exec(`INSERT OR REPLACE INTO credentials(id,data,principal) VALUES(1,?,?)`, value, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	if err = g.DB.QueryRow(`SELECT data FROM credentials WHERE id=1`).Scan(&value); err != nil || value != "new" {
		t.Fatalf("credential upsert: %s %v", value, err)
	}
	g.Close()
	sharing.Close()
	e.Close()
	store.Close()
	// Renaming the legacy tree forces all subsequent reads to use PostgreSQL;
	// no fallback can silently recreate or repopulate file state.
	backup := root + "-legacy"
	if err = os.Rename(root, backup); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(backup)
	store, err = durablestate.Open(root, dsn, namespace)
	if err != nil {
		t.Fatal(err)
	}
	e = newTestEngine(t, engineRoot, nil)
	if !e.networkEnabled {
		t.Fatal("database network state lost")
	}
	ca, _, err = PrepareGatewayCA(root, 0, time.Now())
	if err != nil || ca.Fingerprint() != fingerprint {
		t.Fatalf("restart CA changed: %v", err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("cloud wrote legacy root: %v", err)
	}
	var auditCount int
	if err = store.DB.QueryRow(`SELECT count(*) FROM warden_durable.audit WHERE namespace=$1`, namespace).Scan(&auditCount); err != nil || auditCount < 4 {
		t.Fatalf("audit import: %d %v", auditCount, err)
	}
	e.Close()
}

func TestCloudCARotation(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root := filepath.Join(t.TempDir(), "absent")
	store, err := durablestate.Open(root, dsn, "ca-"+UUID4())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, _, err := PrepareGatewayCA(root, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := ca.Fingerprint()
	ca, err = RotateGatewayCA(root, time.Now())
	if err != nil || ca.Fingerprint() == old {
		t.Fatalf("rotation: %v", err)
	}
	loaded, _, err := PrepareGatewayCA(root, 0, time.Now())
	if err != nil || loaded.Fingerprint() != ca.Fingerprint() {
		t.Fatalf("rotation persistence: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("wrote local state: %v", err)
	}
}
