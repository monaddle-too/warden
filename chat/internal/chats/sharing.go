package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

func sharingTools() []any {
	return append(imageTools(), []any{
		map[string]any{"type": "function", "name": "request_pull_request", "description": "Submit a COMPLETE pull request proposal for owner review in Warden. Requires a shared repository. Provide base branch, title, Markdown body and all changed text files with their entire new UTF-8 content; null content deletes a file. Maximum 20 files / 256 KiB. Warden computes the diff from GitHub and waits for approval or rejection with feedback. Approval creates a dedicated branch and PR from the reviewed snapshot. Do not push first or request write credentials. Include up to four attach_image IDs in images to show screenshots in the review and published PR; Warden adds the image references to the body before review. Other binary files, symlinks and workflow files are unsupported. On rejection, discuss feedback and submit a new revised proposal. For large proposals, write the same complete JSON object to a workspace file and pass only proposal_path (relative path); Warden snapshots its contents before review.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"proposal_path": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"}, "base": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"},
			"images": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"}},
			"files":  map[string]any{"type": "array", "minItems": 0, "maxItems": 20, "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": []string{"string", "null"}}}, "required": []string{"path", "content"}, "additionalProperties": false}},
		}, "required": []string{}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "list_shared_repositories", "description": "List the GitHub repositories shared with this conversation, with clone and API URLs and which read categories the user granted each (access: contents = code, branches, commits and git clone; issues = issues, comments, labels, milestones; pull_requests = pull requests, their files and reviews). Persistent read-only access lasts until the user removes a repository. Use HTTPS Git or GitHub REST through the sandbox proxy; never request credentials. Check this tool when you need repository access.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_google_document_creation", "description": "Request owner approval to create one Google document with this title and grant this conversation temporary read/write access to it. Waits for approval; returns the created document API URL. Then fill the document with POST {api_url}:batchUpdate through the sandbox proxy. Do not call documents.create yourself. Never request credentials.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string", "maxLength": 200}, "reason": map[string]any{"type": "string", "maxLength": 2000}}, "required": []string{"title", "reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_google_docs_access", "description": "Ask the user to select Google documents or spreadsheets and an access duration at one permission level (default read). read: Docs documents.get and Sheets reads. write: also Docs batchUpdate text insertion/deletion and text/paragraph styling, and Sheets cell value writes (values update/append/clear). structure: also every other Docs batchUpdate request (tables, tabs, headers, named ranges) and Sheets spreadsheets:batchUpdate (add/delete sheets, formats, charts, merges). Remote image insertion is never allowed. Each level includes the ones below, on the selected IDs only. Ask for the lowest level that does the job. Waits for their decision; returns the selected IDs with their API URLs (docs.googleapis.com for documents, sheets.googleapis.com for spreadsheets). Read them with HTTPS requests through the sandbox proxy. Never request Google credentials.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string", "maxLength": 2000}, "access": map[string]any{"type": "string", "enum": []string{"read", "write", "structure"}}}, "required": []string{"reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "list_shared_documents", "description": "List this conversation's currently shared Google documents, API URLs and grant expiry times.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
	}...)
}
func (e *Engine) sharingCall(ctx context.Context, op string, data map[string]any) (map[string]any, error) {
	if e.WardenSocket == "" {
		return nil, errors.New("Sharing is not configured")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", e.WardenSocket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(map[string]any{"version": 1, "operation": "sharing", "action": op, "data": data}); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 65536), 8<<20)
	if !scanner.Scan() {
		return nil, errors.New("sharing service disconnected")
	}
	var res struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err = json.Unmarshal(scanner.Bytes(), &res); err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, errors.New("Sharing unavailable; check the connected account and selected resources")
	}
	return res.Result, nil
}
func sharingToolResult(result map[string]any, err error) map[string]any {
	text := ""
	if err != nil {
		text = err.Error()
	} else {
		b, _ := json.Marshal(result)
		text = string(b)
	}
	return map[string]any{"success": err == nil, "contentItems": []any{map[string]any{"type": "inputText", "text": text}}}
}
func (e *Engine) sharingTool(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	data := map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID}
	op := "list"
	if agent.String(f.Params["tool"]) == "list_shared_repositories" {
		op = "github_list"
	}
	if agent.String(f.Params["tool"]) == "request_google_docs_access" || agent.String(f.Params["tool"]) == "request_google_document_creation" {
		op = "request"
		var input struct {
			Reason string `json:"reason"`
			Access string `json:"access"`
			Title  string `json:"title"`
		}
		raw, _ := json.Marshal(f.Params["arguments"])
		if s, ok := f.Params["arguments"].(string); ok {
			raw = []byte(s)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&input); err != nil {
			return client.Reply(f.ID, sharingToolResult(nil, err))
		}
		data["reason"] = input.Reason
		if input.Access == "" {
			input.Access = "read"
		}
		if agent.String(f.Params["tool"]) == "request_google_document_creation" {
			input.Access = "create"
		}
		data["access"] = input.Access
		data["title"] = input.Title
		data["callID"] = c.RunID + ":" + string(f.ID)
	}
	if agent.String(f.Params["tool"]) == "request_pull_request" {
		op = "pr_submit"
		raw, _ := json.Marshal(f.Params["arguments"])
		if v, ok := f.Params["arguments"].(string); ok {
			raw = []byte(v)
		}
		var input map[string]any
		if err := json.Unmarshal(raw, &input); err != nil {
			return client.Reply(f.ID, sharingToolResult(nil, err))
		}
		input, err := e.proposalInput(ctx, c, input)
		if err != nil {
			return client.Reply(f.ID, sharingToolResult(nil, err))
		}
		for key, value := range input {
			switch key {
			case "repository", "base", "title", "body", "files", "images":
				data[key] = value
			default:
				return client.Reply(f.ID, sharingToolResult(nil, errors.New("unexpected proposal field")))
			}
		}
		data["callID"] = c.RunID + ":" + string(f.ID)
	}
	result, err := e.sharingCall(ctx, op, data)
	if op == "pr_submit" && err == nil && result["status"] == "invalid" {
		return client.Reply(f.ID, sharingToolResult(nil, errors.New(agent.String(result["error"]))))
	}
	if err != nil || op == "list" || op == "github_list" {
		return client.Reply(f.ID, sharingToolResult(result, err))
	}
	id := agent.String(result["request_id"])
	// The request is already durable; this goroutine is just its live delivery path.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if result["status"] != "pending" && result["status"] != "publishing" {
				// A successful pipe write is not proof the model consumed it.
				// Ack only when this run completes normally; a disconnect
				// keeps the result available for durable continuation.
				e.mu.Lock()
				if active := e.active[c.ID]; active != nil && active.runID == c.RunID {
					active.sharingResults = append(active.sharingResults, id)
				}
				e.mu.Unlock()
				_ = client.Reply(f.ID, sharingToolResult(result, nil))
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pollOp := "get"
			if op == "pr_submit" {
				pollOp = "pr_get"
			}
			next, err := e.sharingCall(ctx, pollOp, map[string]any{"id": id, "chatID": c.ID, "sandboxID": c.SandboxID})
			if err == nil {
				result = next
			}
		}
	}()
	return nil
}

// Once the old run is gone, resume with a durable, deduplicated message instead
// of replaying a stale tool RPC ID. Never automatically retry the original task.
func (e *Engine) sharingDelivery(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		result, err := e.sharingCall(ctx, "undelivered", map[string]any{})
		if err != nil {
			continue
		}
		for _, value := range agent.Array(result["requests"]) {
			r := agent.Map(value)
			id := agent.String(r["request_id"])
			if len(id) != 64 {
				continue
			}
			chatID := agent.String(r["chatID"])
			e.mu.Lock()
			active := e.active[chatID] != nil
			e.mu.Unlock()
			if active {
				continue
			}
			chat := e.Store.Snapshot().chat(chatID)
			if chat == nil || chat.Archived || chat.SandboxID != agent.String(r["sandboxID"]) {
				continue
			}
			b, _ := json.Marshal(r)
			notification := "Warden permission request resolved: " + string(b) + "\nUse list_shared_documents to check currently active access before fetching."
			if r["kind"] == "pull_request" {
				notification = "Warden pull request review resolved: " + string(b) + "\nIf rejected, discuss the feedback and submit a revised request_pull_request proposal. If published, share the GitHub URL. If failed, explain the reported failure before retrying."
			}
			messageID := id[:32]
			delivered := false
			for _, entry := range chat.Conversation.Entries {
				if entry.ID == messageID && entry.Delivery == "sent" && chat.Status == "idle" {
					delivered = true
				}
			}
			if delivered {
				_, _ = e.sharingCall(ctx, "ack", map[string]any{"id": id})
				continue
			}
			if e.Message(chatID, notification, messageID) == nil {
				// Only this deterministic Warden notification may be retried
				// after a crash; user messages retain normal no-replay behavior.
				_ = e.Store.update(func(st *State) error {
					c := st.chat(chatID)
					for i := range c.Conversation.Entries {
						entry := &c.Conversation.Entries[i]
						if entry.ID == messageID && entry.Text == notification && (entry.Delivery == "failed" || (entry.Delivery == "sent" && (c.Status == "failed" || c.Status == "interrupted"))) {
							entry.Delivery = "queued"
							entry.Detail = ""
							c.Status = "queued"
							c.RunID = cv.ID()
							c.Error = ""
						}
					}
					return nil
				})
				e.Wake()
			}
		}
	}
}
func (h *HTTP) sharingHTTP(w http.ResponseWriter, r *http.Request, path string) {
	op := strings.TrimPrefix(path, "sharing/")
	data := map[string]any{}
	if r.Method == "GET" {
		if op != "state" && op != "status" && op != "files" && op != "blocked" && op != "github_repositories" && op != "github_list" && op != "pr_state" && op != "pr_preview" && op != "egress" && op != "history" {
			http.Error(w, "not found", 404)
			return
		}
		if op == "github_repositories" {
			page := 1
			if v := r.URL.Query().Get("page"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					http.Error(w, "invalid page", 400)
					return
				}
				page = n
			}
			data["page"] = page
		}
		if op == "github_list" {
			data["chatID"] = r.URL.Query().Get("chatID")
		}
		if op == "history" {
			data["sandboxID"] = r.URL.Query().Get("sandboxID")
		}
		if op == "pr_preview" {
			data["id"] = r.URL.Query().Get("id")
		}
		if op == "files" {
			data["page"] = r.URL.Query().Get("page")
		}
	} else if r.Method == "POST" {
		if op != "select" && op != "connect" && op != "disconnect" && op != "egress_set" && op != "resolve" && op != "revoke" && op != "block" && op != "unblock" && op != "github_select" && op != "pr_resolve" {
			http.Error(w, "not found", 404)
			return
		}
		limit := int64(16384)
		if op == "pr_resolve" {
			limit = 256 << 10
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(&data); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		// Console decisions are attributed to the person the edge identified
		// (name, else email); a client cannot claim an actor itself.
		who := requester(r)
		switch {
		case who.Name != "":
			data["actor"] = who.Name
		case who.Email != "":
			data["actor"] = who.Email
		case who.PrincipalID == "owner":
			data["actor"] = "owner"
		default:
			data["actor"] = who.PrincipalID
		}
	} else {
		http.Error(w, "not found", 404)
		return
	}
	if op == "select" || op == "github_select" || op == "github_list" {
		c := h.Engine.Store.Snapshot().chat(agent.String(data["chatID"]))
		if c == nil || c.Archived {
			http.Error(w, "conversation not found", 404)
			return
		}
		data["sandboxID"] = c.SandboxID
	}
	result, err := h.Engine.sharingCall(r.Context(), op, data)
	respond(w, result, err)
}
func (h *HTTP) googleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != "GET" {
		http.Error(w, "not found", 404)
		return
	}
	_, err := h.Engine.sharingCall(r.Context(), "callback", map[string]any{"state": r.URL.Query().Get("state"), "code": r.URL.Query().Get("code")})
	if err != nil {
		http.Error(w, "Google sign-in failed or expired. Return to Warden and try again.", 400)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><title>Google connected</title><p>Google is connected. Return to Warden to select the documents to share.</p>")
}
