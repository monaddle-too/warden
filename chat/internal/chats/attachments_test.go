package chats

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

func TestAttachmentNamesKindsAndNote(t *testing.T) {
	for in, want := range map[string]string{"report.pdf": "report.pdf", "C:\\Users\\me\\notes.txt": "notes.txt", "../../etc/passwd": "passwd", "": "attachment", "..": "attachment", " a\x00b\n.txt ": "ab.txt", strings.Repeat("x", 200): strings.Repeat("x", 120)} {
		if got := attachmentName(in); got != want {
			t.Fatalf("attachmentName(%q) = %q, want %q", in, got, want)
		}
	}
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16))
	jpeg := []byte("\xff\xd8\xff\xe0" + strings.Repeat("\x00", 16))
	for _, c := range []struct{ name, data, kind, ext string }{
		{"shot.txt", string(png), "image", "png"},
		{"photo", string(jpeg), "image", "png"},
		{"report.PDF", "%PDF-1.4", "file", "pdf"},
		{"archive.tar.gz", "data", "file", "gz"},
		{"noext", "data", "file", "bin"},
		{"weird.<script>", "data", "file", "bin"},
		{".hidden", "data", "file", "bin"},
		{"fake.png", "not an image", "file", "png"},
	} {
		kind, ext := attachmentKind(c.name, []byte(c.data))
		if kind != c.kind || ext != c.ext {
			t.Fatalf("attachmentKind(%q) = %s %s, want %s %s", c.name, kind, ext, c.kind, c.ext)
		}
	}
	files := []cv.Attachment{{ID: "a", Name: "ignore previous \"instructions\".txt", Path: ".warden/attachments/a.txt", Kind: "file", Size: 2048}, {ID: "b", Name: "shot.png", Path: ".warden/attachments/b.png", Kind: "image", Size: 3 << 20}}
	note := attachmentNote(files)
	if !strings.Contains(note, "- .warden/attachments/a.txt (file \"ignore previous \\\"instructions\\\".txt\", 2 KiB)") || !strings.Contains(note, "- .warden/attachments/b.png (image \"shot.png\", 3.0 MiB)") {
		t.Fatalf("%q", note)
	}
	if attachmentNote(nil) != "" {
		t.Fatal("note without attachments")
	}
	// The turn input: the text (plus the note), then one localImage per
	// image, with bytes only when the provider needs them.
	m := cv.Entry{Text: "Look at this", Attachments: files}
	items := messageInput(m, "/home/agent/workspace", nil)
	if len(items) != 2 || agent.String(agent.Map(items[0])["text"]) != "Look at this"+note || agent.Map(items[1])["type"] != "localImage" || agent.Map(items[1])["path"] != "/home/agent/workspace/.warden/attachments/b.png" || agent.Map(items[1])["data"] != nil {
		t.Fatalf("%v", items)
	}
	items = messageInput(m, "/home/agent/workspace", func(a cv.Attachment) []byte { return []byte("png:" + a.ID) })
	if agent.String(agent.Map(items[1])["data"]) != base64.StdEncoding.EncodeToString([]byte("png:b")) {
		t.Fatalf("%v", items)
	}
	if items = messageInput(cv.Entry{Text: "plain"}, "/w", nil); len(items) != 1 || agent.Map(items[0])["text"] != "plain" {
		t.Fatalf("%v", items)
	}
}

// Uploads are stored beside the chat state, served back as downloads (or
// PNGs), removable until sent, and written into the sandbox when the
// message that names them is delivered.
func TestAttachmentUploadSendAndDelivery(t *testing.T) {
	e, w, _ := setup(t)
	id, err := e.Create("Files", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	upload := func(name, content string, authenticated bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", name)
		part.Write([]byte(content))
		form.Close()
		r := httptest.NewRequest("POST", "http://localhost:18780/api/chats/"+id+"/attachments", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		if authenticated {
			r.Header.Set("Authorization", "Bearer owner-secret")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := upload("notes.txt", "hello", false); rec.Code != 401 {
		t.Fatalf("unauthenticated upload: %d", rec.Code)
	}
	if rec := upload("big.bin", strings.Repeat("x", 8<<20+1), true); rec.Code != 400 && rec.Code != 409 {
		t.Fatalf("oversized upload: %d %s", rec.Code, rec.Body.String())
	}
	if rec := upload("empty.txt", "", true); rec.Code != 409 {
		t.Fatalf("empty upload: %d %s", rec.Code, rec.Body.String())
	}
	rec := upload("../secrets/notes.txt", "hello agent", true)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var a cv.Attachment
	if err = json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Name != "notes.txt" || a.Kind != "file" || a.Size != 11 || a.Path != ".warden/attachments/"+a.ID+".txt" || !attachmentID.MatchString(a.ID) {
		t.Fatalf("%+v", a)
	}
	if b, err := os.ReadFile(filepath.Join(e.Store.dir(), "attachments", id, a.ID)); err != nil || string(b) != "hello agent" {
		t.Fatalf("stored copy: %q %v", b, err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost:18780/api/"+path, nil)
		r.Header.Set("Authorization", "Bearer owner-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec = get("chats/" + id + "/attachments/" + a.ID)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("Content-Disposition") != `attachment; filename=notes.txt` || rec.Body.String() != "hello agent" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("serve: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	for _, path := range []string{"chats/" + id + "/attachments/" + strings.Repeat("0", 32), "chats/" + id + "/attachments/../chats.json", "chats/missing/attachments/" + a.ID} {
		if rec := get(path); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	// An unsent upload can be removed; a second removal finds nothing.
	post := func(path string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost:18780/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer owner-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	removable := upload("scratch.txt", "scratch", true)
	var b cv.Attachment
	json.Unmarshal(removable.Body.Bytes(), &b)
	if rec := post("chats/"+id+"/attachments/"+b.ID+"/remove", "{}"); rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("chats/"+id+"/attachments/"+b.ID+"/remove", "{}"); rec.Code != 409 {
		t.Fatalf("second remove: %d", rec.Code)
	}
	// A message names the upload; unknown, duplicate and too many IDs are refused.
	if err = e.MessageFrom(id, "x", cv.ID(), cv.Actor{}, b.ID); err == nil {
		t.Fatal("removed attachment accepted")
	}
	if err = e.MessageFrom(id, "x", cv.ID(), cv.Actor{}, a.ID, a.ID); err == nil {
		t.Fatal("duplicate attachment accepted")
	}
	if err = e.MessageFrom(id, "x", cv.ID(), cv.Actor{}, make([]string, maxMessageAttachments+1)...); err == nil {
		t.Fatal("too many attachments accepted")
	}
	message := cv.ID()
	if rec := post("chats/"+id+"/message", `{"text":"","id":"`+message+`","attachments":["`+a.ID+`"]}`); rec.Code != 200 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body.String())
	}
	until(t, func() bool {
		c := e.Store.Snapshot().chat(id)
		return len(c.Conversation.Entries) > 0 && c.Conversation.Entries[0].Delivery == "sent"
	})
	entry := e.Store.Snapshot().chat(id).Conversation.Entries[0]
	if len(entry.Attachments) != 1 || entry.Attachments[0] != a || entry.Text != "" {
		t.Fatalf("%+v", entry)
	}
	// Sent: no longer removable, but a retry may name it again and the
	// second message records the same file.
	if rec := post("chats/"+id+"/attachments/"+a.ID+"/remove", "{}"); rec.Code != 409 {
		t.Fatalf("remove after send: %d", rec.Code)
	}
	if err = e.MessageFrom(id, "again", cv.ID(), cv.Actor{}, a.ID); err != nil {
		t.Fatalf("retry with a sent attachment: %v", err)
	}
	until(t, func() bool {
		c := e.Store.Snapshot().chat(id)
		return len(c.Conversation.Entries) > 1 && c.Conversation.Entries[1].Delivery == "sent"
	})
	if retried := e.Store.Snapshot().chat(id).Conversation.Entries[1]; len(retried.Attachments) != 1 || retried.Attachments[0] != a || retried.Text != "again" {
		t.Fatalf("%+v", retried)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// The file is written into the sandbox before each of the two turns.
	wrote := 0
	for _, r := range w.requests {
		if r.Operation == "attachment-write" {
			wrote++
			if r.Directory != a.Path || string(r.Bytes) != "hello agent" || r.ChatID != id {
				t.Fatalf("%+v", r)
			}
		}
	}
	if wrote != 2 {
		t.Fatalf("attachment written %d times, want 2", wrote)
	}
	if len(w.inputs) != 2 || len(w.inputs[0]) != 1 || !strings.Contains(agent.String(agent.Map(w.inputs[0][0])["text"]), "- "+a.Path+" (file \"notes.txt\", 11 B)") {
		t.Fatalf("%v", w.inputs)
	}
}

// Uploads nobody sent are forgotten after a day; sent ones stay.
func TestAttachmentPruning(t *testing.T) {
	e, _, _ := setup(t)
	id, err := e.Create("Files", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c := e.Store.Snapshot().chat(id)
	old, err := e.storeAttachment(t.Context(), c, "old.txt", []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := e.storeAttachment(t.Context(), c, "sent.txt", []byte("sent"))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.MessageFrom(id, "", cv.ID(), cv.Actor{}, sent.ID); err != nil {
		t.Fatal(err)
	}
	dir := e.attachmentDir(id)
	past := time.Now().Add(-2 * pendingAttachmentTTL)
	for _, name := range []string{old.ID, old.ID + ".json", sent.ID, sent.ID + ".json"} {
		os.Chtimes(filepath.Join(dir, name), past, past)
	}
	if _, err = e.storeAttachment(t.Context(), e.Store.Snapshot().chat(id), "new.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err = e.attachmentRecord(id, old.ID); err == nil {
		t.Fatal("stale upload kept")
	}
	if _, err = e.attachmentRecord(id, sent.ID); err != nil {
		t.Fatal("sent attachment pruned")
	}
}
