package cloudauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"warden/chat/internal/documenttools"
)

func externalTools() []any {
	out := []any{}
	for _, raw := range documenttools.Tools() {
		t := raw.(map[string]any)
		delete(t, "type")
		t["description"] = strings.ReplaceAll(t["description"].(string), "this chat's organization", "the connected organization")
		out = append(out, t)
	}
	out = append(out, map[string]any{"name": "warden_share_conversation", "description": "Upload an explicit conversation snapshot for everyone in the connected organization to read. Only upload messages the person wants to share. Supply the messages yourself; this does not retrieve local history. Immutable snapshots, Markdown content, maximum 500 messages / 2 MiB total. Reuse operationId only for identical retries. Returns an organization-only link.", "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"operationId", "title", "messages"}, "properties": map[string]any{"operationId": map[string]any{"type": "string", "format": "uuid"}, "title": map[string]any{"type": "string", "maxLength": 200}, "messages": map[string]any{"type": "array", "minItems": 1, "maxItems": 500, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"role", "content"}, "properties": map[string]any{"role": map[string]any{"type": "string", "enum": []string{"user", "assistant", "system", "tool"}}, "content": map[string]any{"type": "string", "maxLength": 100000}, "name": map[string]any{"type": "string", "maxLength": 120}}}}}}})
	return out
}

// Stateless Streamable HTTP: JSON responses, no server-initiated requests or
// SSE session. GET/DELETE are intentionally unsupported. Notifications return
// 202. Supported protocol versions share the tools-only wire representation.
func (a *Auth) agentMCP(w http.ResponseWriter, r *http.Request) {
	c, err := a.bearerConnection(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.Origin+`/.well-known/oauth-protected-resource/mcp", scope="panta"`)
		oauthError(w, 401, "invalid_token", "Connect your agent to Warden")
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", 405)
		return
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", 415)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !mcpVersion(v) {
		http.Error(w, "unsupported MCP protocol version", 400)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	rpcErr := func(code int, message string) {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	if decodeLimit(r, &req, 3<<20) != nil {
		rpcErr(-32700, "Invalid JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		rpcErr(-32600, "Invalid request")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	var id any
	if json.Unmarshal(req.ID, &id) != nil {
		rpcErr(-32600, "Invalid request ID")
		return
	}
	switch id.(type) {
	case string, float64:
	default:
		rpcErr(-32600, "Request ID must be a string or number")
		return
	}
	result := func(v any) { respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": v}) }
	switch req.Method {
	case "initialize":
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &in) != nil {
			rpcErr(-32602, "Invalid initialize parameters")
			return
		}
		version := in.ProtocolVersion
		if !mcpVersion(version) {
			version = "2025-11-25"
		}
		result(map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "warden-panta", "version": "1.0.0"}, "instructions": "All Panta documents in the connected organization are available to read and write. Read current revisions before editing. Use panta_propose_document_edit for suggestions that a person can accept or reject. Use UUID operationId values and reuse them for identical retries. warden_share_conversation uploads only the messages explicitly supplied and shares them within this organization. No runtime or administrator tools are exposed."})
	case "ping":
		result(map[string]any{})
	case "tools/list":
		result(map[string]any{"tools": externalTools()})
	case "tools/call":
		var in struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if json.Unmarshal(req.Params, &in) != nil {
			rpcErr(-32602, "Invalid tool parameters")
			return
		}
		known := false
		for _, t := range externalTools() {
			if t.(map[string]any)["name"] == in.Name {
				known = true
			}
		}
		if !known {
			rpcErr(-32602, "Unknown tool")
			return
		}
		var output []byte
		if in.Name == "warden_share_conversation" {
			output, err = a.shareConversation(r.Context(), c, in.Arguments)
		} else {
			var args map[string]any
			if len(in.Arguments) == 0 {
				in.Arguments = json.RawMessage(`{}`)
			}
			if e := json.Unmarshal(in.Arguments, &args); e != nil || args == nil {
				err = errors.New("arguments must be an object")
			} else {
				output, err = documenttools.Call(r.Context(), a.DocsAddress, a.DocsKey, c.OrganizationID, "external-"+c.ID, in.Name, string(req.ID), args)
			}
		}
		if err == nil {
			var value map[string]any
			if json.Unmarshal(output, &value) == nil {
				if link, ok := value["url"].(string); ok && strings.HasPrefix(link, "/documents/") {
					value["url"] = a.Origin + link
					output, _ = json.Marshal(value)
				}
			}
		}
		text := string(output)
		if err != nil {
			text = err.Error()
		}
		result(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": err != nil})
	default:
		rpcErr(-32601, "Method not found")
	}
}
func mcpVersion(v string) bool { return v == "2025-11-25" || v == "2025-06-18" || v == "2025-03-26" }

type sharedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}
type sharedConversation struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	OrganizationName string          `json:"organizationName"`
	Author           string          `json:"author"`
	Agent            string          `json:"agent"`
	CreatedAt        time.Time       `json:"createdAt"`
	Messages         []sharedMessage `json:"messages,omitempty"`
}

var operationShape = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (a *Auth) shareConversation(ctx context.Context, c agentConnection, raw json.RawMessage) ([]byte, error) {
	var in struct {
		OperationID string          `json:"operationId"`
		Title       string          `json:"title"`
		Messages    []sharedMessage `json:"messages"`
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("conversation exceeds 2 MiB")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || !operationShape.MatchString(in.OperationID) || strings.TrimSpace(in.Title) == "" || len([]rune(in.Title)) > 200 || len(in.Messages) == 0 || len(in.Messages) > 500 {
		return nil, errors.New("supply a UUID operationId, title (1–200 characters), and 1–500 messages")
	}
	for _, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "tool" {
			return nil, errors.New("message role must be user, assistant, system or tool")
		}
		if len([]rune(m.Content)) > 100000 || len([]rune(m.Name)) > 120 {
			return nil, errors.New("message content or name too long")
		}
	}
	canonical, _ := json.Marshal(in)
	fingerprint := hashSecret(string(canonical))
	messages, _ := json.Marshal(in.Messages)
	var id, stored string
	// The no-op conflict update locks an identical retry, preventing duplicate
	// snapshots even when two replicas receive the operation concurrently.
	err := a.Login.Store.db.QueryRowContext(ctx, `INSERT INTO warden_cloud.shared_conversations(id,organization_id,user_id,connection_id,operation_id,fingerprint,title,messages) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(connection_id,operation_id) DO UPDATE SET operation_id=EXCLUDED.operation_id RETURNING id,fingerprint`, ID(), c.OrganizationID, c.UserID, c.ID, in.OperationID, fingerprint, in.Title, messages).Scan(&id, &stored)
	if err != nil {
		return nil, errors.New("conversation upload unavailable")
	}
	if stored != fingerprint {
		return nil, errors.New("operationId already used with different content; use a new UUID")
	}
	return json.Marshal(map[string]string{"id": id, "url": a.Origin + "/shared-conversations/" + id, "visibility": "organization", "organizationId": c.OrganizationID})
}
func (a *Auth) sharedConversations(w http.ResponseWriter, r *http.Request) {
	s, err := a.current(r)
	if err != nil {
		http.Error(w, "Sign in required", 401)
		return
	}
	if s.OrganizationID == "" {
		http.Error(w, "Choose an organization", 403)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/auth/shared-conversations")
	id = strings.TrimPrefix(id, "/")
	query := `SELECT t.id,t.title,o.name,u.name,c.name,t.created_at,t.messages FROM warden_cloud.shared_conversations t JOIN warden_cloud.organizations o ON o.id=t.organization_id JOIN warden_cloud.users u ON u.id=t.user_id JOIN warden_cloud.agent_connections a ON a.id=t.connection_id JOIN warden_cloud.agent_clients c ON c.id=a.client_id WHERE t.organization_id=$1`
	var args = []any{s.OrganizationID}
	if id != "" {
		query += ` AND t.id=$2`
		args = append(args, id)
	} else {
		// Cursor pagination keeps every shared conversation discoverable.
		before := r.URL.Query().Get("before")
		if before != "" {
			query += ` AND (t.created_at,t.id)<(SELECT created_at,id FROM warden_cloud.shared_conversations WHERE id=$2 AND organization_id=$1)`
			args = append(args, before)
		}
		query = strings.Replace(query, "t.messages FROM", "NULL::jsonb FROM", 1) + ` ORDER BY t.created_at DESC,t.id DESC LIMIT 50`
	}
	rows, err := a.Login.Store.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "Conversations unavailable", 503)
		return
	}
	defer rows.Close()
	items := []sharedConversation{}
	for rows.Next() {
		var item sharedConversation
		var raw []byte
		if rows.Scan(&item.ID, &item.Title, &item.OrganizationName, &item.Author, &item.Agent, &item.CreatedAt, &raw) != nil {
			http.Error(w, "Conversations unavailable", 503)
			return
		}
		if id != "" && json.Unmarshal(raw, &item.Messages) != nil {
			http.Error(w, "Conversation unavailable", 503)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		http.Error(w, "Conversations unavailable", 503)
		return
	}
	if id != "" {
		if len(items) == 0 {
			http.Error(w, "Conversation not found in the current organization", 404)
			return
		}
		respond(w, 200, items[0])
		return
	}
	next := ""
	if len(items) == 50 {
		next = items[len(items)-1].ID
	}
	respond(w, 200, map[string]any{"items": items, "next": next})
}
