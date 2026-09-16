package chats

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type HTTP struct {
	Engine                      *Engine
	Token, Host, Origin, WebDir string
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; frame-src http://127.0.0.1:*")
	if r.Host != h.Host {
		http.Error(w, "untrusted host", http.StatusForbidden)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != h.Origin {
		http.Error(w, "untrusted origin", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/oauth/google_docs/callback" {
		h.googleCallback(w, r)
		return
	}
	if r.URL.Path == "/auth/session" {
		json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		http.FileServer(http.Dir(h.WebDir)).ServeHTTP(w, r)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) != 1 {
		http.Error(w, "Warden sign-in required", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if strings.HasPrefix(path, "chats/") && strings.Contains(path, "/images/") {
		h.imageHTTP(w, r, path)
		return
	}
	if strings.HasPrefix(path, "sharing/") {
		h.sharingHTTP(w, r, path)
		return
	}
	if r.Method == "GET" && path == "ports" {
		json.NewEncoder(w).Encode(h.Engine.Store.Snapshot().Ports)
		return
	}
	if strings.HasPrefix(path, "ports/") {
		rest := strings.TrimPrefix(r.URL.Path, "/api/ports/")
		id, sub, ok := strings.Cut(rest, "/")
		if ok && strings.HasPrefix(sub, "proxy/") {
			h.Engine.ServePort(id, "/"+strings.TrimPrefix(sub, "proxy/"), w, r)
			return
		}
	}
	if r.Method == "GET" && path == "state" {
		json.NewEncoder(w).Encode(h.Engine.Store.Snapshot())
		return
	}
	if r.Method == "GET" && path == "environments" {
		result, err := h.Engine.Environments(r.Context())
		respond(w, result, err)
		return
	}
	if r.Method == "GET" && path == "events" {
		h.events(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "runtime" {
		res, err := h.Engine.Runtime(r.Context(), parts[1], "status")
		respond(w, res, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "file" {
		name := r.URL.Query().Get("path")
		if name == "" || len(name) > 2048 {
			http.Error(w, "file path required", 400)
			return
		}
		state := h.Engine.Store.Snapshot()
		c := state.chat(parts[1])
		if c == nil {
			http.Error(w, "chat not found", 404)
			return
		}
		req := request(c, "file")
		req.Directory = name
		res, err := h.Engine.Worker.Call(r.Context(), req)
		if err != nil {
			respond(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(name)}))
		w.Write(res.Bytes)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "not found", 404)
		return
	}
	var body struct {
		Provider   string              `json:"provider"`
		Model      string              `json:"model"`
		Title      string              `json:"title"`
		SandboxID  string              `json:"sandboxID"`
		Repository string              `json:"repository"`
		Text       string              `json:"text"`
		ID         string              `json:"id"`
		Archived   bool                `json:"archived"`
		Allow      bool                `json:"allow"`
		Answers    map[string][]string `json:"answers"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	var err error
	var result any = map[string]bool{"ok": true}
	switch {
	case len(parts) == 3 && parts[0] == "ports" && parts[2] == "revoke":
		err = h.Engine.RevokePort(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "stop":
		err = h.Engine.StopEnvironment(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "archive":
		err = h.Engine.ArchiveEnvironment(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "delete":
		err = h.Engine.DeleteEnvironment(r.Context(), parts[1])
	case path == "chats":
		var id string
		id, err = h.Engine.Create(body.Title, body.SandboxID, body.Repository, body.Provider, body.Model)
		result = map[string]string{"id": id}
	case len(parts) == 3 && parts[0] == "chats":
		switch parts[2] {
		case "agent":
			err = h.Engine.ConfigureAgentAndRelease(r.Context(), parts[1], body.Provider, body.Model)
		case "message":
			err = h.Engine.Message(parts[1], body.Text, body.ID)
		case "edit":
			err = h.Engine.Edit(parts[1], body.Title, body.Archived)
		case "stop":
			err = h.Engine.Stop(r.Context(), parts[1])
		case "activity":
			result, err = h.Engine.Runtime(r.Context(), parts[1], "activity")
		default:
			http.Error(w, "not found", 404)
			return
		}
	case len(parts) == 4 && parts[0] == "chats" && parts[2] == "approvals":
		err = h.Engine.Resolve(parts[1], parts[3], body.Allow, body.Answers)
	default:
		http.Error(w, "not found", 404)
		return
	}
	respond(w, result, err)
}
func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(value)
}
func (h *HTTP) events(w http.ResponseWriter, r *http.Request) {
	_, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	previous := ""
	lastWrite := time.Time{}
	for {
		data, _ := json.Marshal(h.Engine.Store.Snapshot())
		if string(data) != previous || time.Since(lastWrite) >= 5*time.Second {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			lastWrite = time.Now()
			previous = string(data)
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
