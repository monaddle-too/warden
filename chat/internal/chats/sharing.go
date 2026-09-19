package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/transport"
)

func sharingTools() []any {
	return append(imageTools(), []any{
		map[string]any{"type": "function", "name": "request_pull_request", "description": "Submit a COMPLETE pull request proposal for owner review in Warden. Requires a shared repository. Provide base branch, title, Markdown body and all changed text files with their entire new UTF-8 content; null content deletes a file. Maximum 20 files / 256 KiB. Warden computes the diff from GitHub and waits for approval or rejection with feedback. Approval creates a dedicated branch and PR from the reviewed snapshot. Do not push first or request write credentials. Include up to four attach_image IDs in images to show screenshots in the review and published PR; Warden adds the image references to the body before review. Other binary files, symlinks and workflow files are unsupported. On rejection, discuss feedback and submit a new revised proposal. For large proposals, write the same complete JSON object to a workspace file and pass only proposal_path (relative path); Warden snapshots its contents before review.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"proposal_path": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"}, "base": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"},
			"images": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"}},
			"files":  map[string]any{"type": "array", "minItems": 0, "maxItems": 20, "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": []string{"string", "null"}}}, "required": []string{"path", "content"}, "additionalProperties": false}},
		}, "required": []string{}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "list_shared_repositories", "description": "List the GitHub repositories shared with this conversation, with clone and API URLs and which read categories the user granted each (access: contents = code, branches, commits and git clone; issues = issues, comments, labels, milestones; pull_requests = pull requests, their files and reviews). Persistent read-only access lasts until the user removes a repository. Use HTTPS Git or GitHub REST through the sandbox proxy; never request credentials. Check this tool when you need repository access.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_google_document_creation", "description": "Request owner approval to create one empty Google document with this title and grant this conversation temporary read access to it. Waits for approval; returns the created document ID and API URL. Then fill it with propose_google_document_edit (read_google_document first): the owner reviews the content as suggestions and Warden writes what they approve. Documents are never written directly. Do not call documents.create yourself. Never request credentials.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string", "maxLength": 200}, "reason": map[string]any{"type": "string", "maxLength": 2000}}, "required": []string{"title", "reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_google_docs_access", "description": "Ask the user to select Google documents or spreadsheets and an access duration at one permission level (default read). read: Docs documents.get and Sheets reads. write: also Sheets cell value writes (values update/append/clear). structure: also Sheets spreadsheets:batchUpdate (add/delete sheets, formats, charts, merges). The write and structure levels apply to spreadsheets only: Google Docs are never written directly (batchUpdate is refused at every level); read them with read_google_document and propose changes with propose_google_document_edit, which need only read. Each level includes the ones below, on the selected IDs only. Ask for the lowest level that does the job: read for documents. Waits for their decision; returns the selected IDs with their API URLs (docs.googleapis.com for documents, sheets.googleapis.com for spreadsheets). Read them with HTTPS requests through the sandbox proxy. Never request Google credentials.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string", "maxLength": 2000}, "access": map[string]any{"type": "string", "enum": []string{"read", "write", "structure"}}}, "required": []string{"reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "list_shared_documents", "description": "List this conversation's currently shared Google documents, API URLs and grant expiry times.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "read_google_document", "description": "Read a shared Google document (read access suffices) as numbered paragraphs: {n, style, depth, text, frozen?}. Styles: title, subtitle, h1–h6, text, bullet, numbered (depth = list nesting). Text uses Markdown-like marks: **bold**, *italic*, [text](url); backslash escapes \\\\ \\* \\[ \\]. Frozen paragraphs (tables, images, footnotes, breaks, chips) are shown as placeholders and cannot be changed or deleted. Only the first tab is shown. Pass proposal_id to read instead the draft the owner returned with that proposal (see propose_google_document_edit). Prefer this over documents.get.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"document_id": map[string]any{"type": "string"}, "proposal_id": map[string]any{"type": "string"}}, "required": []string{"document_id"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "propose_google_document_edit", "description": "Propose edits to a shared Google document as suggestions the owner reviews in Warden; only read access is needed and nothing is written until they approve. Give a summary and ops against paragraph numbers from read_google_document: {type: replace, start, end, paragraphs, reason?} replaces paragraphs start..end (inclusive), {type: insert, after, paragraphs, reason?} inserts after paragraph number after (0 = at the top), {type: delete, start, end, reason?} deletes. Paragraphs are {style, depth?, text} as read_google_document shows them; put the paragraph's full new text in text. Ops must not overlap or touch frozen paragraphs; up to 200 ops / 256 KiB. Add a short reason to each op: the owner sees it beside the change. Waits for the decision: applied (written to the document), rejected (feedback), returned (the owner edited the draft and/or left comments: read it with read_google_document {document_id, proposal_id} and submit a new proposal with revises = that request_id and ops against the returned draft's numbering), or failed. Do not write with batchUpdate when this tool is available.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"document_id": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string", "maxLength": 4000}, "revises": map[string]any{"type": "string"},
			"ops": map[string]any{"type": "array", "minItems": 1, "maxItems": 200, "items": map[string]any{"type": "object", "properties": map[string]any{
				"type": map[string]any{"type": "string", "enum": []string{"replace", "insert", "delete"}}, "start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}, "after": map[string]any{"type": "integer"}, "reason": map[string]any{"type": "string", "maxLength": 2000},
				"paragraphs": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"style": map[string]any{"type": "string", "enum": []string{"title", "subtitle", "h1", "h2", "h3", "h4", "h5", "h6", "text", "bullet", "numbered"}}, "depth": map[string]any{"type": "integer", "minimum": 0, "maximum": 8}, "text": map[string]any{"type": "string"}}, "required": []string{"text"}, "additionalProperties": false}},
			}, "required": []string{"type"}, "additionalProperties": false}},
		}, "required": []string{"document_id", "summary", "ops"}, "additionalProperties": false}},
	}...)
}
func (e *Engine) sharingCall(ctx context.Context, op string, data map[string]any) (map[string]any, error) {
	if e.PolicyAddress == "" {
		return nil, errors.New("Sharing is not configured")
	}
	conn, err := transport.Dial(ctx, e.PolicyAddress, transport.DialOptions{TLS: e.PolicyTLS})
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
		Error  string         `json:"error"`
		Result map[string]any `json:"result"`
	}
	if err = json.Unmarshal(scanner.Bytes(), &res); err != nil {
		return nil, err
	}
	if !res.OK {
		// The policy service words its sharing refusals for people; an
		// answer without one is the control server rejecting the frame.
		if res.Error == "" {
			res.Error = "Sharing unavailable; check the connected account and selected resources"
		}
		return nil, errors.New(res.Error)
	}
	return res.Result, nil
}

// githubDisconnected reports a sharing error that means no GitHub
// connection is usable at all (never connected, sign-in missing or
// rejected, sharing not running), as opposed to a refusal of one request.
func githubDisconnected(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return msg == "GitHub is not connected" || strings.HasPrefix(msg, "Refresh the GitHub sign-in") || strings.HasPrefix(msg, "Sharing unavailable") || msg == "sharing unavailable" || msg == "Sharing is not configured"
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
	if name := agent.String(f.Params["tool"]); name == "read_google_document" || name == "propose_google_document_edit" {
		op = "doc_read"
		if name == "propose_google_document_edit" {
			op = "doc_submit"
			data["callID"] = c.RunID + ":" + string(f.ID)
		}
		raw, _ := json.Marshal(f.Params["arguments"])
		if v, ok := f.Params["arguments"].(string); ok {
			raw = []byte(v)
		}
		var input map[string]any
		if err := json.Unmarshal(raw, &input); err != nil || input == nil {
			return client.Reply(f.ID, sharingToolResult(nil, errors.New("invalid arguments")))
		}
		for key, value := range input {
			switch {
			case key == "document_id", key == "proposal_id" && op == "doc_read", (key == "summary" || key == "ops" || key == "revises") && op == "doc_submit":
				data[key] = value
			default:
				return client.Reply(f.ID, sharingToolResult(nil, errors.New("unexpected field "+key)))
			}
		}
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
	if (op == "pr_submit" || op == "doc_submit" || op == "doc_read") && err == nil && result["status"] == "invalid" {
		return client.Reply(f.ID, sharingToolResult(nil, errors.New(agent.String(result["error"]))))
	}
	if err != nil || op == "list" || op == "github_list" || op == "doc_read" {
		return client.Reply(f.ID, sharingToolResult(result, err))
	}
	id := agent.String(result["request_id"])
	// The request waits for the person in the app: the chat records it
	// while it does (reviews.go), for the clients that cannot answer it.
	e.recordReview(c.ID, op, result)
	// The request is already durable; this goroutine is just its live delivery path.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if result["status"] != "pending" && result["status"] != "publishing" && result["status"] != "applying" && result["status"] != "stale" {
				// A successful pipe write is not proof the model consumed it.
				// Ack only when this run completes normally; a disconnect
				// keeps the result available for durable continuation.
				e.mu.Lock()
				if active := e.active[c.ID]; active != nil && active.runID == c.RunID {
					active.sharingResults = append(active.sharingResults, id)
				}
				e.mu.Unlock()
				e.dropReview(id)
				_ = client.Reply(f.ID, sharingToolResult(result, nil))
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pollOp := "get"
			switch op {
			case "pr_submit":
				pollOp = "pr_get"
			case "doc_submit":
				pollOp = "doc_get"
			}
			next, err := e.sharingCall(ctx, pollOp, map[string]any{"id": id, "chatID": c.ID, "sandboxID": c.SandboxID})
			if err == nil {
				result = next
				e.recordReview(c.ID, pollOp, result)
			}
		}
	}()
	return nil
}

// Once the old run is gone, resume with a durable, deduplicated message instead
// of replaying a stale tool RPC ID. Never automatically retry the original task.
func (e *Engine) sharingDelivery(ctx context.Context) {
	// The reviews recorded on the chats are matched to what waits in the
	// policy service at the start (a restart lost the runs' polls) and
	// every reconcileEvery ticks after (a review settled in the app while
	// no run polled it, and its result acknowledged before this loop saw
	// it, would otherwise stay).
	e.reconcileReviews(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if tick%reconcileEvery == 0 {
			e.reconcileReviews(ctx)
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
			e.dropReview(id) // settled, whichever chat holds it and whatever its run is doing
			chatID := agent.String(r["chatID"])
			e.mu.Lock()
			active := e.active[chatID] != nil
			e.mu.Unlock()
			if active {
				continue
			}
			chat := e.Store.Chat(chatID)
			if chat == nil || chat.Archived || chat.SandboxID != agent.String(r["sandboxID"]) {
				continue
			}
			b, _ := json.Marshal(r)
			notification := "Warden permission request resolved: " + string(b) + "\nUse list_shared_documents to check currently active access before fetching."
			if r["kind"] == "pull_request" {
				notification = "Warden pull request review resolved: " + string(b) + "\nIf rejected, discuss the feedback and submit a revised request_pull_request proposal. If published, share the GitHub URL. If failed, explain the reported failure before retrying."
			}
			if r["kind"] == "document_proposal" {
				notification = "Warden document suggestion review resolved: " + string(b) + "\nIf applied, tell the user what was written. If rejected, discuss the feedback before proposing again. If returned, read the owner's draft with read_google_document {document_id, proposal_id} and submit a revised propose_google_document_edit with revises set to this request_id. If failed, explain the reported failure before retrying."
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
		if op != "state" && op != "status" && op != "files" && op != "blocked" && op != "github_repositories" && op != "github_list" && op != "github_login_status" && op != "pr_state" && op != "pr_preview" && op != "doc_state" && op != "doc_preview" && op != "egress" && op != "history" {
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
		if op == "history" || (op == "egress" && r.URL.Query().Get("sandboxID") != "") {
			data["sandboxID"] = r.URL.Query().Get("sandboxID")
		}
		if op == "pr_preview" || op == "doc_preview" {
			data["id"] = r.URL.Query().Get("id")
		}
		if op == "files" {
			data["page"] = r.URL.Query().Get("page")
		}
	} else if r.Method == "POST" {
		if op != "select" && op != "connect" && op != "disconnect" && op != "github_login_start" && op != "github_login_cancel" && op != "egress_set" && op != "resolve" && op != "revoke" && op != "block" && op != "unblock" && op != "github_select" && op != "pr_resolve" && op != "doc_draft" && op != "doc_decide" && op != "doc_resolve" && op != "doc_return" && op != "doc_rebase" {
			http.Error(w, "not found", 404)
			return
		}
		limit := int64(16384)
		if op == "pr_resolve" || op == "doc_draft" {
			limit = 1 << 20
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
		c := h.Engine.Store.Chat(agent.String(data["chatID"]))
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
