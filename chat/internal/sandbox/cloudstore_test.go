package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"warden/chat/internal/durablestate"
)

func TestCloudRunnerImportsLegacyInventory(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("WARDEN_TEST_POSTGRES_URL is not set")
	}
	root := t.TempDir()
	legacy := newManagedState()
	legacy.Sandboxes["sandbox-a"] = &managedSandbox{SandboxInfo: SandboxInfo{ID: "sandbox-a", RuntimeName: "wc-a", State: "stopped"}}
	legacy.Chats["chat-a"] = &chatBinding{ID: "chat-a", SandboxID: "sandbox-a"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "managed-v2.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(root, "cancelled-runs")
	if err = os.Mkdir(markers, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(markers, "old-run.json"), []byte("true"), 0600); err != nil {
		t.Fatal(err)
	}
	instance := "cloud-runner-test-" + randomID()
	w := NewWorker(root, "/never", "template")
	w.DatabaseURL, w.Instance, w.managed = dsn, instance, newManagedState()
	if err = w.loadManagedLocked(); err != nil {
		t.Fatal(err)
	}
	if got := w.managed.Chats["chat-a"]; got == nil || got.SandboxID != "sandbox-a" {
		t.Fatalf("imported chat: %+v", got)
	}
	if recorded, err := w.cancelledRunRecorded("old-run"); err != nil || !recorded {
		t.Fatalf("imported tombstone: %v %v", recorded, err)
	}
	if err = w.recordCancelledRun("new-run"); err != nil {
		t.Fatal(err)
	}
	w.closeStore()
	// The old file is retained for a controlled rollback, but PostgreSQL wins
	// after the first successful import.
	if err = os.WriteFile(filepath.Join(root, "managed-v2.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := NewWorker(root, "/never", "template")
	fresh.DatabaseURL, fresh.Instance, fresh.managed = dsn, instance, newManagedState()
	if err = fresh.loadManagedLocked(); err != nil {
		t.Fatal(err)
	}
	defer fresh.closeStore()
	if fresh.managed.Sandboxes["sandbox-a"] == nil {
		t.Fatal("runner inventory lost on restart")
	}
	if recorded, err := fresh.cancelledRunRecorded("new-run"); err != nil || !recorded {
		t.Fatalf("new tombstone: %v %v", recorded, err)
	}
}

func TestCloudRetainedReviewSurvivesLegacyRemoval(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires PostgreSQL")
	}
	w, runtime, req, _ := reviewFixture(t)
	w.closeStore()
	// Move the in-memory fixture to a fresh cloud store without reopening SQLite.
	w.store, w.storeErr, w.storeOnce = nil, nil, sync.Once{}
	w.DatabaseURL, w.Instance = dsn, "review-"+randomID()
	state, err := durablestate.Open(w.Root, dsn, w.Instance)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	// Seed the disposable public-base cache from the local fixture.
	base := filepath.Join("repository-bundles", req.SandboxID, "review-base.bundle")
	raw, err := os.ReadFile(filepath.Join(w.Root, base))
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(w.scratchRoot(), base)
	if err = os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cache, raw, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(cache))
	if err = w.saveManagedLocked(); err != nil {
		t.Fatal(err)
	}
	defer w.closeStore()
	legacy := w.Root + "-legacy"
	if err = os.Rename(w.Root, legacy); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(legacy)
	if err = os.WriteFile(filepath.Join(runtime.directory, "plan.md"), []byte("Cloud review\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Operation, req.CallID = "review.capture", "cloud-review"
	captured, err := w.dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Operation, req.Expected = "review.publish-plan", captured.Review.Head
	plan, err := w.dispatch(context.Background(), req)
	if err != nil || plan.PublishPlan == nil {
		t.Fatalf("retained publication: %v", err)
	}
	if _, err = os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatalf("wrote legacy root: %v", err)
	}
}
