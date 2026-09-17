package chats

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/imageguard"
)

func imageTools() []any {
	return []any{map[string]any{"type": "function", "name": "attach_image", "description": "Show a PNG/JPEG image from a relative path beneath this conversation's workspace. Warden sanitizes and stores an immutable image attachment in the chat. Returns an image_id to include in request_pull_request images, or to place in a shared Google Doc: with structure-level document access, send a Docs batchUpdate insertInlineImage whose uri is warden-image:<image_id> and Warden serves the image to Google for that edit only. Maximum 8 MiB input and 4 megapixels. Do not use URLs or absolute paths.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "caption": map[string]any{"type": "string", "maxLength": 500}}, "required": []string{"path", "caption"}, "additionalProperties": false}}}
}
func (e *Engine) imageTool(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	var in struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	raw, _ := json.Marshal(f.Params["arguments"])
	if s, ok := f.Params["arguments"].(string); ok {
		raw = []byte(s)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return client.Reply(f.ID, sharingToolResult(nil, err))
	}
	result, err := e.attachImage(ctx, c, in.Path, in.Caption)
	return client.Reply(f.ID, sharingToolResult(result, err))
}
func (e *Engine) attachImage(ctx context.Context, c *Chat, path, caption string) (map[string]any, error) {
	if len(path) > 1024 || len(caption) > 2000 {
		return nil, errors.New("image path or caption too long")
	}
	png, err := e.workspaceImage(ctx, c, path)
	if err != nil {
		return nil, err
	}
	image, err := e.sharingCall(ctx, "image_add", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID, "caption": caption, "png": base64.StdEncoding.EncodeToString(png)})
	if err != nil {
		return nil, err
	}
	id := agent.String(image["image_id"])
	err = e.Store.update(func(s *State) error {
		chat := s.chat(c.ID)
		if chat == nil || chat.SandboxID != c.SandboxID {
			return errors.New("conversation changed")
		}
		for _, entry := range chat.Conversation.Entries {
			if entry.ID == "image-"+id {
				return nil
			}
		}
		entry := cv.NewEntry("image", caption)
		entry.ID = "image-" + id
		entry.Detail = id
		chat.Conversation.Entries = append(chat.Conversation.Entries, entry)
		return nil
	})
	return image, err
}

// workspaceImage reads a PNG/JPEG the agent wrote beneath the workspace and
// returns it as an imageguard-normalised PNG. The worker's image-file op
// refuses symlinks, non-regular files and anything over 8 MiB; the
// normaliser runs in a separate, memory-capped process because the input
// is untrusted. Both attach_image and the transcript's inline images go
// through here.
func (e *Engine) workspaceImage(ctx context.Context, c *Chat, path string) ([]byte, error) {
	r := request(c, "image-file")
	r.Directory = path
	result, err := e.Worker.Call(ctx, r)
	if err != nil {
		return nil, err
	}
	if len(result.Bytes) == 0 {
		return nil, errors.New("image unavailable")
	}
	if len(result.Bytes) > 8<<20 {
		return nil, errors.New("image too large")
	}
	return e.normalizeImage(ctx, result.Bytes)
}

// normalizeImage re-encodes an untrusted PNG/JPEG as PNG in a separate,
// memory-capped process (imageguard); every image shown in the transcript
// or sent to an agent passes through here.
func (e *Engine) normalizeImage(ctx context.Context, raw []byte) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "serve", "--normalize-image")
	cmd.Env = []string{"GOMEMLIMIT=96MiB", "GOMAXPROCS=1"}
	cmd.Dir = "/tmp"
	cmd.Stdin = bytes.NewReader(raw)
	var out imageOutput
	cmd.Stdout = &out
	if err = cmd.Run(); err != nil {
		return nil, errors.New("image rejected: provide a valid PNG/JPEG up to 4 megapixels and 4 MiB after normalization")
	}
	return out.Bytes(), nil
}

// imageFileHTTP serves a workspace image for an inline `![alt](path)` in
// the transcript: same normalisation as attach_image, but nothing is stored,
// so a re-render shows the file as it is now. Only relative workspace paths
// are accepted (the worker rejects the rest); remote URLs never reach here
// because the web client keeps them as alt text.
func (h *HTTP) imageFileHTTP(w http.ResponseWriter, r *http.Request, chatID string) {
	name := r.URL.Query().Get("path")
	if name == "" || len(name) > 1024 {
		http.Error(w, "image path required", 400)
		return
	}
	c := h.Engine.Store.Snapshot().chat(chatID)
	if c == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	png, err := h.Engine.workspaceImage(r.Context(), c, name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(png)
}

// publishedHTTP serves an attached image Google was told to fetch for a
// Docs inline-image edit. No sign-in: the token is the capability, minted by
// the policy service for one edit and withdrawn when the edit finishes.
func (h *HTTP) publishedHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/published/"), ".png")
	if !ok || token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	result, err := h.Engine.sharingCall(r.Context(), "image_published", map[string]any{"token": token})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	png, err := base64.StdEncoding.DecodeString(agent.String(result["png"]))
	if err != nil || len(png) > imageguard.MaxBytes {
		http.Error(w, "invalid image", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

type imageOutput struct{ bytes.Buffer }

func (b *imageOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > imageguard.MaxBytes {
		return 0, errors.New("image output too large")
	}
	return b.Buffer.Write(p)
}
func (h *HTTP) imageHTTP(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if r.Method != "GET" || len(parts) != 4 || parts[0] != "chats" || parts[2] != "images" {
		http.NotFound(w, r)
		return
	}
	c := h.Engine.Store.Snapshot().chat(parts[1])
	if c == nil {
		http.NotFound(w, r)
		return
	}
	result, err := h.Engine.sharingCall(r.Context(), "image_get", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID, "id": parts[3]})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	png, err := base64.StdEncoding.DecodeString(agent.String(result["png"]))
	if err != nil || len(png) > imageguard.MaxBytes {
		http.Error(w, "invalid image", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(png)
}
