package chats

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/transport"
)

// HTTP is the chat API and web UI. Requests are admitted by Token, the
// owner capability rotated at every start that the edge reads from
// endpoint.json (the sbx shapes), or, when Peer is set, by the mutual-TLS
// client certificate carrying that identity (Kubernetes, where the edge's
// certificate is its authority to forward the X-Warden-* identity headers
// and no capability exists).
type HTTP struct {
	Engine                      *Engine
	Token, Host, Origin, WebDir string
	Peer                        string
}

// admitted reports whether the request carries the capability or, with
// Peer set, was made over a connection whose client certificate is Peer's.
func (h *HTTP) admitted(r *http.Request) bool {
	if h.Peer != "" {
		return r.TLS != nil && transport.IdentityOf(*r.TLS) == h.Peer
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return h.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) == 1
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
		if strings.HasPrefix(r.URL.Path, "/published/") {
			h.publishedHTTP(w, r)
			return
		}
		http.FileServer(http.Dir(h.WebDir)).ServeHTTP(w, r)
		return
	}
	if !h.admitted(r) {
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
		json.NewEncoder(w).Encode(h.Engine.View())
		return
	}
	if r.Method == "GET" && path == "chats/search" {
		// A search across every chat's title and transcript (search.go).
		limit := 0
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			limit = n
		}
		json.NewEncoder(w).Encode(h.Engine.Search(r.URL.Query().Get("q"), limit))
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
	if r.Method == "GET" && (path == "cluster" || path == "cluster/logs") {
		// Owner-only at the edge (ownerOnly lists api/cluster).
		h.clusterHTTP(w, r, path)
		return
	}
	if r.Method == "GET" && path == "spend" {
		// The admin console's spend totals (spend.go); owner-only at the
		// edge (ownerOnly lists api/spend).
		json.NewEncoder(w).Encode(h.Engine.Spend())
		return
	}
	if path == "me/instructions" {
		// A person's own standing instructions (instructions.go); the
		// requester is whoever the edge identified, or the owner.
		h.instructionsHTTP(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "memory" {
		view, err := h.Engine.Memory(r.Context(), parts[1])
		respond(w, view, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "runtime" {
		res, err := h.Engine.Runtime(r.Context(), parts[1], "status")
		respond(w, res, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "image-file" {
		h.imageFileHTTP(w, r, parts[1])
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "paths" {
		h.pathsHTTP(w, r, parts[1])
		return
	}
	if r.Method == "GET" && len(parts) == 3 && (parts[0] == "environments" || parts[0] == "chats") && parts[2] == "rules" {
		// A workspace's permission rules and its chats' (rules.go).
		view, err := h.Engine.Rules(parts[1])
		respond(w, view, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "permissions" {
		events, err := h.Engine.Permissions(parts[1])
		respond(w, map[string]any{"events": events}, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "diff" {
		changes, err := h.Engine.Diff(r.Context(), parts[1])
		respond(w, changes, err)
		return
	}
	if r.Method == "GET" && len(parts) == 3 && parts[0] == "chats" && parts[2] == "checkpoints" {
		list, err := h.Engine.Checkpoints(r.Context(), parts[1])
		respond(w, map[string]any{"checkpoints": list}, err)
		return
	}
	if len(parts) >= 3 && parts[0] == "chats" && parts[2] == "attachments" {
		switch {
		case r.Method == "POST" && len(parts) == 3:
			h.attachmentUpload(w, r, parts[1])
			return
		case r.Method == "GET" && len(parts) == 4:
			h.attachmentHTTP(w, r, parts[1], parts[3])
			return
		}
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
		// A tool permission's other answers (Engine.Answer): allow and
		// remember, deny with a message the model reads, the mode a plan
		// is approved into; Mode is also the body of chats/{id}/mode.
		Always    bool               `json:"always"`
		Message   string             `json:"message"`
		Mode      string             `json:"mode"`
		Resources *sandbox.Resources `json:"resources"`
		// Attachments are upload IDs a message sends along.
		Attachments []string `json:"attachments"`
		// Thinking, Effort and Fast are the body of chats/{id}/settings
		// (Engine.SetSettings): each applies when present.
		Thinking *string `json:"thinking"`
		Effort   *string `json:"effort"`
		Fast     *bool   `json:"fast"`
		// TurnID and What are a rewind's target and scope (rewind.go);
		// TurnID is also where a fork cuts (fork.go). Code asks an
		// undo-rewind to restore the workspace too.
		TurnID string `json:"turnID"`
		What   string `json:"what"`
		Code   bool   `json:"code"`
		// Scope and Path name the memory file a chats/{id}/memory/write
		// replaces with Text (memory.go); Scope is also where an "allow
		// always" answer remembers its rule ("chat" or "workspace").
		Scope string `json:"scope"`
		Path  string `json:"path"`
		// Style is the body of chats/{id}/style (style.go).
		Style string `json:"style"`
		// Kind and Pattern are a permission rule, the body of
		// environments/{id}/rules and chats/{id}/rules (rules.go).
		Kind    string `json:"kind"`
		Pattern string `json:"pattern"`
	}
	// Room for a memory file (1 MiB of text, JSON-escaped); every other
	// body is bounded far below by its own validation.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
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
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "start":
		err = h.Engine.StartEnvironment(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "archive":
		err = h.Engine.ArchiveEnvironment(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "delete":
		err = h.Engine.DeleteEnvironment(r.Context(), parts[1])
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "resize":
		err = h.Engine.ResizeEnvironment(r.Context(), parts[1], body.Resources)
	case path == "chats":
		var id string
		id, err = h.Engine.CreateFrom(requester(r), body.Title, body.SandboxID, body.Repository, body.Resources, body.Provider, body.Model)
		result = map[string]string{"id": id}
	case len(parts) == 3 && parts[0] == "chats":
		switch parts[2] {
		case "agent":
			err = h.Engine.ConfigureAgentAndRelease(r.Context(), parts[1], body.Provider, body.Model)
		case "mode":
			err = h.Engine.SetMode(r.Context(), parts[1], body.Mode)
		case "settings":
			err = h.Engine.SetSettings(r.Context(), parts[1], Settings{Thinking: body.Thinking, Effort: body.Effort, Fast: body.Fast})
		case "message":
			err = h.Engine.MessageFrom(parts[1], body.Text, body.ID, requester(r), body.Attachments...)
		case "typing":
			err = h.Engine.Typing(parts[1], requester(r))
			result = map[string]bool{"ok": true}
		case "edit":
			err = h.Engine.Edit(parts[1], body.Title, body.Archived)
		case "stop":
			err = h.Engine.Stop(r.Context(), parts[1])
		case "exec":
			// A person's own shell command in the workspace (composer.go).
			result, err = h.Engine.Exec(r.Context(), parts[1], body.Text, requester(r))
		case "memory":
			err = h.Engine.AppendMemory(r.Context(), parts[1], body.Text, requester(r))
		case "activity":
			result, err = h.Engine.Runtime(r.Context(), parts[1], "activity")
		case "rewind":
			result, err = h.Engine.Rewind(r.Context(), parts[1], body.TurnID, body.What)
		case "withdraw":
			// A queued message out of the queue, returned for the
			// composer (queue.go); send-queued lets a held queue go.
			result, err = h.Engine.Withdraw(parts[1], body.ID, requester(r))
		case "send-queued":
			err = h.Engine.SendQueued(parts[1])
		case "undo-rewind":
			// The last conversation rewind's removed transcript back in
			// place (rewind.go); ID names its marker.
			result, err = h.Engine.UndoRewind(r.Context(), parts[1], body.ID, body.Code, requester(r))
		case "fork":
			// A sibling chat copied from this one up to a message (fork.go).
			result, err = h.Engine.Fork(r.Context(), parts[1], body.TurnID, requester(r))
		case "aside":
			// A side question answered from a copy of the session (aside.go).
			result, err = h.Engine.Aside(r.Context(), parts[1], body.Text, requester(r))
		case "style":
			err = h.Engine.SetOutputStyle(r.Context(), parts[1], body.Style)
		case "rules":
			// A permission rule added to the chat (rules.go).
			result, err = h.Engine.AddRule(parts[1], body.Kind, body.Pattern, requester(r))
		default:
			http.Error(w, "not found", 404)
			return
		}
	case len(parts) == 4 && parts[0] == "chats" && parts[2] == "approvals":
		err = h.Engine.Answer(parts[1], parts[3], Answer{Allow: body.Allow, Answers: body.Answers, Always: body.Always, Scope: body.Scope, Message: body.Message, Mode: body.Mode}, requester(r))
	case len(parts) == 3 && parts[0] == "environments" && parts[2] == "rules":
		// A permission rule added to the workspace (rules.go).
		result, err = h.Engine.AddRule(parts[1], body.Kind, body.Pattern, requester(r))
	case len(parts) == 5 && (parts[0] == "environments" || parts[0] == "chats") && parts[2] == "rules" && parts[4] == "remove":
		err = h.Engine.RemoveRule(parts[1], parts[3])
	case len(parts) == 5 && parts[0] == "chats" && parts[2] == "attachments" && parts[4] == "remove":
		err = h.Engine.removeAttachment(parts[1], parts[3])
	case len(parts) == 4 && parts[0] == "chats" && parts[2] == "memory" && parts[3] == "write":
		err = h.Engine.WriteMemory(r.Context(), parts[1], body.Scope, body.Path, body.Text, requester(r))
	case len(parts) == 5 && parts[0] == "chats" && parts[2] == "queued" && parts[4] == "edit":
		// A queued message's text and attachments replaced in place
		// (queue.go); the edited entry comes back.
		result, err = h.Engine.EditQueued(parts[1], parts[3], body.Text, body.Attachments, requester(r))
	default:
		http.Error(w, "not found", 404)
		return
	}
	respond(w, result, err)
}

// instructionsHTTP answers me/instructions: GET reads the requester's own
// text, POST {"text"} replaces it (blank removes it).
func (h *HTTP) instructionsHTTP(w http.ResponseWriter, r *http.Request) {
	actor := requester(r)
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(h.Engine.Instructions(actor))
	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
		if err := h.Engine.SetInstructions(actor, body.Text); err != nil {
			respond(w, nil, err)
			return
		}
		json.NewEncoder(w).Encode(h.Engine.Instructions(actor))
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// requester is the person behind a request as the edge identified them
// (X-Warden-Principal, -Email, -Name; the edge strips client-supplied
// copies). Without the edge, the capability holder is the owner.
func requester(r *http.Request) conversation.Actor {
	principal := strings.TrimSpace(r.Header.Get("X-Warden-Principal"))
	if principal == "" || len(principal) > 128 {
		return conversation.Actor{PrincipalID: "owner"}
	}
	clip := func(v string, n int) string {
		v = strings.TrimSpace(v)
		if len(v) > n {
			return v[:n]
		}
		return v
	}
	return conversation.Actor{PrincipalID: principal, Email: clip(r.Header.Get("X-Warden-Email"), 254), Name: clip(r.Header.Get("X-Warden-Name"), 120)}
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
		data, _ := json.Marshal(h.Engine.View())
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
