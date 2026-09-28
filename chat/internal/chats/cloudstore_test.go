package chats

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warden/chat/internal/cloudauth"
	cv "warden/chat/internal/conversation"
)

func TestCloudStoreImportsLegacyIntoOrganization(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("WARDEN_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	auth, err := cloudauth.Open(ctx, dsn, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	owner, err := auth.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	org, err := auth.CreateOrganization(ctx, owner, "Cloud chat test "+cloudauth.ID())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	legacy := State{Version: 1, Chats: []*Chat{{ID: "old-chat", SandboxID: "old-sandbox", Title: "Previous chat"}}, Ports: []PortBinding{{ID: "old-port", ChatID: "old-chat"}}}
	data, _ := json.Marshal(legacy)
	if err = os.WriteFile(filepath.Join(root, legacyFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	legacyAttachment := cv.Attachment{ID: strings.Repeat("a", 32), Name: "old.txt", Kind: "file", Size: 3}
	attachmentDir := filepath.Join(root, "attachments", "old-chat")
	if err = os.MkdirAll(attachmentDir, 0700); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(legacyAttachment)
	if err = os.WriteFile(filepath.Join(attachmentDir, legacyAttachment.ID+".json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(attachmentDir, legacyAttachment.ID), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	instance := "test-" + cloudauth.ID()
	store, err := OpenCloud(root, dsn, org.ID, instance)
	if err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	if len(state.Chats) != 1 || state.Chats[0].OrganizationID != org.ID || len(state.Ports) != 1 || state.Ports[0].OrganizationID != org.ID {
		t.Fatalf("legacy organization migration: %+v", state)
	}
	engine := &Engine{Store: store}
	if old, err := engine.attachmentBytes("old-chat", legacyAttachment.ID); err != nil || string(old) != "old" {
		t.Fatalf("imported attachment: %q %v", old, err)
	}
	newAttachment, err := engine.storeAttachment(ctx, state.Chats[0], "new.txt", []byte("database only"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := engine.attachmentBytes("old-chat", newAttachment.ID); err != nil || string(got) != "database only" {
		t.Fatalf("new cloud attachment: %q %v", got, err)
	}
	if err = store.updateChat("old-chat", func(c *Chat) error {
		c.Conversation.Entries = []cv.Entry{cv.NewEntry("user", "first"), cv.NewEntry("assistant", "second")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var firstRowVersion string
	if err = store.db.QueryRow(`SELECT xmin::text FROM warden_cloud.chat_items WHERE instance=$1 AND chat_id='old-chat' AND kind=0 AND seq=0`, instance).Scan(&firstRowVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.updateChat("old-chat", func(c *Chat) error {
		c.Conversation.Entries[1].Text = "second, updated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var unchangedRowVersion string
	if err = store.db.QueryRow(`SELECT xmin::text FROM warden_cloud.chat_items WHERE instance=$1 AND chat_id='old-chat' AND kind=0 AND seq=0`, instance).Scan(&unchangedRowVersion); err != nil {
		t.Fatal(err)
	}
	if unchangedRowVersion != firstRowVersion {
		t.Fatal("updating an entry rewrote an unchanged entry row")
	}
	if _, err = os.Stat(filepath.Join(attachmentDir, newAttachment.ID)); !os.IsNotExist(err) {
		t.Fatalf("new attachment written to disk: %v", err)
	}
	store.Close()
	if _, err = os.Stat(filepath.Join(root, dbFile)); !os.IsNotExist(err) {
		t.Fatalf("cloud mode created SQLite: %v", err)
	}
	store, err = OpenCloud(root, dsn, "", instance)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Snapshot().Chats[0].Approvals == nil {
		t.Fatal("empty approvals must survive restart as an array, not null")
	}
	if got := store.Snapshot().Chats[0].OrganizationID; got != org.ID {
		t.Fatalf("reopened organization = %q", got)
	}
	if got := store.Snapshot().Chats[0].Conversation.Entries[1].Text; got != "second, updated" {
		t.Fatalf("reopened entry = %q", got)
	}
}
