package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The first start on the database imports the pre-database inventory
// (managed-v2.json) and the cancelled-run marker files, leaves both
// renamed as backups, and the next start loads the rows; a save writes
// the rows that changed and a tombstone survives a restart.
func TestRunnerStoreImportsTheLegacyFilesOnce(t *testing.T) {
	root := t.TempDir()
	legacy := newManagedState()
	legacy.Sandboxes["sb-1"] = &managedSandbox{SandboxInfo: SandboxInfo{ID: "sb-1", ProjectID: "p", RuntimeName: "wc-1", State: "stopped"}, PrincipalID: "owner", Created: true}
	legacy.Chats["chat-1"] = &chatBinding{ID: "chat-1", ProjectID: "p", SandboxID: "sb-1", ThreadID: "thread-1"}
	legacy.Cancelled["p/chat-1/run-1"] = true
	legacy.Attachments["att-1"] = &PreviewAttachment{ID: "att-1", ChatID: "chat-1", SandboxID: "sb-1", Port: 3000, Path: "/", State: "stopped"}
	legacy.Publications[pubKey("sb-1", 3000)] = &publication{ID: pubKey("sb-1", 3000), SandboxID: "sb-1", Port: 3000, Address: "127.0.0.1", HostPort: 40000, State: "stopped"}
	legacy.Calls["call-1"] = savedAttachmentCall{Fingerprint: "f", AttachmentID: "att-1"}
	legacy.Spares["wc-spare-1"] = &spareSandbox{Name: "wc-spare-1", Created: time.Unix(100, 0)}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, "managed-v2.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(root, "cancelled-runs")
	if err := os.MkdirAll(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markers, runHash("p/chat-1/run-0")+".json"), []byte("true"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(root, "/never", "template")
	w.managed = newManagedState()
	if err := w.loadManagedLocked(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"managed-v2.json", "cancelled-runs"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was not moved aside", name)
		}
		if _, err := os.Stat(filepath.Join(root, name+".migrated")); err != nil {
			t.Fatalf("no backup of %s", name)
		}
	}
	if recorded, err := w.cancelledRunRecorded(runHash("p/chat-1/run-0")); err != nil || !recorded {
		t.Fatalf("marker not imported: %v %v", recorded, err)
	}
	if err := w.recordCancelledRun(runHash("p/chat-1/run-2")); err != nil {
		t.Fatal(err)
	}
	w.closeStore()
	// The next start loads the rows; the JSON file, were it back, is ignored.
	if err := os.WriteFile(filepath.Join(root, "managed-v2.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := NewWorker(root, "/never", "template")
	fresh.managed = newManagedState()
	if err := fresh.loadManagedLocked(); err != nil {
		t.Fatal(err)
	}
	defer fresh.closeStore()
	m := fresh.managed
	if s := m.Sandboxes["sb-1"]; s == nil || s.RuntimeName != "wc-1" || !s.Created || s.PrincipalID != "owner" {
		t.Fatalf("sandbox: %+v", s)
	}
	if c := m.Chats["chat-1"]; c == nil || c.ThreadID != "thread-1" {
		t.Fatalf("chat: %+v", c)
	}
	if !m.Cancelled["p/chat-1/run-1"] || m.Attachments["att-1"].Port != 3000 || m.Publications[pubKey("sb-1", 3000)].HostPort != 40000 || m.Calls["call-1"].AttachmentID != "att-1" || m.Spares["wc-spare-1"].Created.Unix() != 100 {
		t.Fatalf("maps: %+v %+v %+v %+v %+v", m.Cancelled, m.Attachments, m.Publications, m.Calls, m.Spares)
	}
	for _, run := range []string{"p/chat-1/run-0", "p/chat-1/run-2"} {
		if recorded, err := fresh.cancelledRunRecorded(runHash(run)); err != nil || !recorded {
			t.Fatalf("tombstone of %s lost: %v %v", run, recorded, err)
		}
	}
	// A save writes what changed: one sandbox's state, not the rest.
	m.Sandboxes["sb-1"].State = "running"
	if err := fresh.writeManagedLocked(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := fresh.store.db.QueryRow(`SELECT state FROM sandboxes WHERE id = 'sb-1'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("state column: %q %v", state, err)
	}
	if fresh.store.shadow[tableSandboxes]["sb-1"] == "" || fresh.store.shadow[tableChats]["chat-1"] == "" {
		t.Fatal("shadow not kept")
	}
	delete(m.Chats, "chat-1")
	if err := fresh.writeManagedLocked(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := fresh.store.db.QueryRow(`SELECT count(*) FROM chats`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deleted binding still a row: %d %v", n, err)
	}
}
