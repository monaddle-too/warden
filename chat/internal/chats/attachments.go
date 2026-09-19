package chats

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// Attachments are files the owner sends with a message. The chat service
// keeps a copy beside its state (attachments/<chat>/<id> plus <id>.json) so
// the transcript can show them whether or not the sandbox is running or
// the agent has since moved the file, and writes each into the sandbox at
// .warden/attachments/<id>.<ext> when the message is delivered, so an
// upload works before the first turn has started the sandbox. Images are
// normalised by imageguard on upload, like an agent's attach_image, and the
// normalised PNG is what both the transcript and the agent see.

const (
	maxPendingAttachments = 32
	maxMessageAttachments = 8
	pendingAttachmentTTL  = 24 * time.Hour
	// claudeImageBudget caps the image bytes sent inline in one Claude
	// turn; images past it are still in the workspace for Claude to read.
	claudeImageBudget = 12 << 20
)

var attachmentID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var attachmentExtension = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

func (e *Engine) attachmentDir(chatID string) string {
	return filepath.Join(e.Store.dir(), "attachments", chatID)
}

// attachmentName is the sender's file name reduced to something safe to
// show and to hand the agent: one path component, printable, bounded.
func attachmentName(name string) string {
	name = strings.NewReplacer("\\", "/").Replace(name)
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	if runes := []rune(name); len(runes) > 120 {
		name = string(runes[:120])
	}
	return name
}

// attachmentKind sniffs the content: PNG/JPEG becomes an image (normalised
// to PNG), anything else a file whose extension follows the name.
func attachmentKind(name string, data []byte) (kind, ext string) {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg":
		return "image", "png"
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		if ext = strings.ToLower(name[i+1:]); attachmentExtension.MatchString(ext) {
			return "file", ext
		}
	}
	return "file", "bin"
}

// storeAttachment keeps an uploaded file for the chat and returns its record.
func (e *Engine) storeAttachment(ctx context.Context, c *Chat, name string, data []byte) (cv.Attachment, error) {
	if len(data) == 0 || len(data) > sandbox.MaxAttachmentBytes {
		return cv.Attachment{}, errors.New("attachments must be between 1 byte and 8 MiB")
	}
	name = attachmentName(name)
	kind, ext := attachmentKind(name, data)
	if kind == "image" {
		png, err := e.normalizeImage(ctx, data)
		if err != nil {
			return cv.Attachment{}, err
		}
		data = png
	}
	dir := e.attachmentDir(c.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return cv.Attachment{}, err
	}
	if pending := e.pruneAttachments(c); pending >= maxPendingAttachments {
		return cv.Attachment{}, errors.New("too many unsent attachments; send or remove some first")
	}
	a := cv.Attachment{ID: cv.ID(), Name: name, Kind: kind, Size: int64(len(data))}
	a.Path = ".warden/attachments/" + a.ID + "." + ext
	if err := os.WriteFile(filepath.Join(dir, a.ID), data, 0600); err != nil {
		return cv.Attachment{}, err
	}
	meta, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(dir, a.ID+".json"), meta, 0600); err != nil {
		os.Remove(filepath.Join(dir, a.ID))
		return cv.Attachment{}, err
	}
	return a, nil
}

// pruneAttachments drops uploads that were never sent once they are a day
// old and returns how many unsent ones remain.
func (e *Engine) pruneAttachments(c *Chat) int {
	dir := e.attachmentDir(c.ID)
	entries, _ := os.ReadDir(dir)
	pending := 0
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || c.attachment(id) != nil {
			continue
		}
		if info, err := entry.Info(); err == nil && e.now().Sub(info.ModTime()) > pendingAttachmentTTL {
			os.Remove(filepath.Join(dir, id))
			os.Remove(filepath.Join(dir, id+".json"))
			continue
		}
		pending++
	}
	return pending
}

// attachment finds an attachment by ID across the chat's entries.
func (c *Chat) attachment(id string) *cv.Attachment {
	for i := range c.Conversation.Entries {
		for j := range c.Conversation.Entries[i].Attachments {
			if a := &c.Conversation.Entries[i].Attachments[j]; a.ID == id {
				return a
			}
		}
	}
	return nil
}

// attachmentRecord reads a stored attachment's metadata (sent or not).
func (e *Engine) attachmentRecord(chatID, id string) (cv.Attachment, error) {
	var a cv.Attachment
	if !attachmentID.MatchString(id) {
		return a, errors.New("attachment not found")
	}
	raw, err := os.ReadFile(filepath.Join(e.attachmentDir(chatID), id+".json"))
	if err != nil || json.Unmarshal(raw, &a) != nil || a.ID != id {
		return a, errors.New("attachment not found")
	}
	return a, nil
}
func (e *Engine) attachmentBytes(chatID, id string) ([]byte, error) {
	if !attachmentID.MatchString(id) {
		return nil, errors.New("attachment not found")
	}
	f, err := os.Open(filepath.Join(e.attachmentDir(chatID), id))
	if err != nil {
		return nil, errors.New("attachment not found")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, sandbox.MaxAttachmentBytes+1))
	if err != nil || len(data) == 0 || len(data) > sandbox.MaxAttachmentBytes {
		return nil, errors.New("attachment unavailable")
	}
	return data, nil
}

// removeAttachment forgets an upload that no message has used.
func (e *Engine) removeAttachment(chatID, id string) error {
	c := e.Store.Chat(chatID)
	if c == nil {
		return errors.New("chat not found")
	}
	if _, err := e.attachmentRecord(chatID, id); err != nil {
		return err
	}
	if c.attachment(id) != nil {
		return errors.New("attachment was already sent")
	}
	dir := e.attachmentDir(chatID)
	os.Remove(filepath.Join(dir, id))
	return os.Remove(filepath.Join(dir, id+".json"))
}

// claimAttachments resolves the IDs a new message names to stored records.
// An ID an earlier message of this chat carried is allowed again: a retry
// or an edit-and-resend names the same upload, and the stored copy stays
// for as long as the chat does (pruning only forgets unsent uploads).
func (e *Engine) claimAttachments(c *Chat, ids []string) ([]cv.Attachment, error) {
	if len(ids) > maxMessageAttachments {
		return nil, fmt.Errorf("a message can carry at most %d attachments", maxMessageAttachments)
	}
	var out []cv.Attachment
	seen := map[string]bool{}
	for _, id := range ids {
		a, err := e.attachmentRecord(c.ID, id)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, errors.New("attachment named twice")
		}
		seen[id] = true
		out = append(out, a)
	}
	return out, nil
}

// deliverAttachments writes a message's files into the sandbox workspace.
// It runs before the agent sees the message and again if the message is
// retried; the write replaces whatever is at the path.
func (e *Engine) deliverAttachments(ctx context.Context, c *Chat, m cv.Entry) error {
	for _, a := range m.Attachments {
		data, err := e.attachmentBytes(c.ID, a.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		r := request(c, "attachment-write")
		r.Directory = a.Path
		r.Bytes = data
		if _, err = e.Worker.Call(ctx, r); err != nil {
			return fmt.Errorf("could not send %s to the sandbox: %w", a.Name, err)
		}
	}
	return nil
}

// input is the agent's view of a user message: its text, a note naming
// each attachment by workspace path, and an image item per image so an
// image-capable agent sees it. Codex reads a localImage from the path
// inside the sandbox; the Claude adapter runs on the host, so for Claude the
// normalised PNG rides along as base64 (up to claudeImageBudget per turn).
func (e *Engine) input(ctx context.Context, c *Chat, cwd string, m cv.Entry) ([]any, error) {
	// The transcript keeps the message as typed; the agent gets its
	// resource mentions expanded (mentions.go).
	m.Text = e.expandMessage(ctx, c, m.Text)
	if len(m.Attachments) == 0 {
		return messageInput(m, cwd, nil), nil
	}
	if err := e.deliverAttachments(ctx, c, m); err != nil {
		return nil, err
	}
	var images func(cv.Attachment) []byte
	if c.Provider == "claude" {
		budget := claudeImageBudget
		images = func(a cv.Attachment) []byte {
			data, err := e.attachmentBytes(c.ID, a.ID)
			if err != nil || len(data) > budget {
				return nil
			}
			budget -= len(data)
			return data
		}
	}
	return messageInput(m, cwd, images), nil
}

// messageInput builds the turn input items; images, when given, supplies the
// PNG bytes to send inline for an image attachment (nil sends the path only).
func messageInput(m cv.Entry, cwd string, images func(cv.Attachment) []byte) []any {
	items := []any{map[string]any{"type": "text", "text": m.Text + attachmentNote(m.Attachments), "text_elements": []any{}}}
	for _, a := range m.Attachments {
		if a.Kind != "image" {
			continue
		}
		item := map[string]any{"type": "localImage", "path": path.Join(cwd, a.Path)}
		if images != nil {
			if data := images(a); len(data) > 0 {
				item["data"] = base64.StdEncoding.EncodeToString(data)
			}
		}
		items = append(items, item)
	}
	return items
}

// attachmentNote tells the agent where the sender's files are. The sender's
// name is quoted so a name that reads like an instruction stays a name.
func attachmentNote(attachments []cv.Attachment) string {
	if len(attachments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAttached files (paths relative to the workspace):\n")
	for _, a := range attachments {
		fmt.Fprintf(&b, "- %s (%s %q, %s)\n", a.Path, a.Kind, a.Name, attachmentSize(a.Size))
	}
	return strings.TrimRight(b.String(), "\n")
}
func attachmentSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// attachmentUpload takes one multipart file (field "file") for a chat and
// answers with its record; the message that sends it names the ID.
func (h *HTTP) attachmentUpload(w http.ResponseWriter, r *http.Request, chatID string) {
	c := h.Engine.Store.Chat(chatID)
	if c == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	if c.Archived {
		respond(w, nil, errors.New("chat is archived"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandbox.MaxAttachmentBytes+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "one file of at most 8 MiB required", 400)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, sandbox.MaxAttachmentBytes+1))
	if err != nil {
		http.Error(w, "invalid upload", 400)
		return
	}
	a, err := h.Engine.storeAttachment(r.Context(), c, header.Filename, data)
	respond(w, a, err)
}

// attachmentHTTP serves the chat service's copy of an attachment: an image
// as the normalised PNG with the images route's headers, anything else as a
// download the browser never renders.
func (h *HTTP) attachmentHTTP(w http.ResponseWriter, r *http.Request, chatID, id string) {
	if h.Engine.Store.Chat(chatID) == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	a, err := h.Engine.attachmentRecord(chatID, id)
	if err != nil {
		http.Error(w, "attachment not found", 404)
		return
	}
	data, err := h.Engine.attachmentBytes(chatID, id)
	if err != nil {
		http.Error(w, "attachment not found", 404)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	if a.Kind == "image" {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": a.Name})
		if disposition == "" {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Write(data)
}
