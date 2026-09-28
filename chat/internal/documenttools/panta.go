package documenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func Tools() []any {
	str := func(description string) any { return map[string]any{"type": "string", "description": description} }
	doc := str("Document ID from panta_list_documents")
	op := str("UUID for this operation. Reuse only for an identical retry.")
	rev := map[string]any{"type": "integer", "minimum": 0}
	tool := func(name, desc string, props map[string]any, required ...string) any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "function", "name": name, "description": desc, "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
	}
	return []any{
		tool("panta_list_documents", "List all documents, drawings and folders in this chat's organization. All documents are readable and writable by default.", map[string]any{}),
		tool("panta_read_document", "Read rich text, text block positions, and current revision of an organization document.", map[string]any{"documentId": doc}, "documentId"),
		tool("panta_create_document", "Create an organization document from Markdown. Return its ID and link.", map[string]any{"operationId": op, "title": str("Document title"), "content": str("Initial Markdown content")}, "operationId", "title", "content"),
		tool("panta_edit_document", "Edit live document text directly. Read first; supply current revision and nonoverlapping edits within paragraphs/headings using read_document positions, or ProseMirror steps for rich structure. Reread on conflict.", map[string]any{"documentId": doc, "operationId": op, "revision": rev, "steps": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "edits": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"from": rev, "to": rev, "text": str("Replacement text")}, "required": []string{"from", "to", "text"}, "additionalProperties": false}}}, "documentId", "operationId", "revision"),
		tool("panta_list_comments", "Read document comment threads and replies.", map[string]any{"documentId": doc}, "documentId"),
		tool("panta_add_comment", "Add a comment, optionally anchored with from/to, or reply to threadId. New threads need the revision from read_document.", map[string]any{"documentId": doc, "operationId": op, "text": str("Comment text"), "threadId": str("Existing thread ID for reply"), "revision": rev, "from": rev, "to": rev}, "documentId", "operationId", "text"),
		tool("panta_propose_document_edit", "Suggest changes for human accept/reject review without changing the live document. Read first. Supply the complete proposed document in Markdown, preserving unchanged content, plus a short reason.", map[string]any{"documentId": doc, "operationId": op, "revision": rev, "content": str("Complete proposed Markdown document"), "reason": str("Summary of the proposed changes")}, "documentId", "operationId", "revision", "content", "reason"),
		tool("panta_list_suggestions", "Read pending, accepted and rejected document suggestions.", map[string]any{"documentId": doc}, "documentId"),
	}
}
func Call(ctx context.Context, address, key, organization, actor, name, callID string, arguments any) ([]byte, error) {
	if organization == "" || address == "" || key == "" {
		return nil, errors.New("organization documents are unavailable")
	}
	known := false
	for _, t := range Tools() {
		if t.(map[string]any)["name"] == name {
			known = true
			break
		}
	}
	if !known {
		return nil, errors.New("unknown Panta tool")
	}
	if value, ok := arguments.(string); ok {
		if err := json.Unmarshal([]byte(value), &arguments); err != nil {
			return nil, errors.New("invalid tool arguments")
		}
	}
	data, err := json.Marshal(map[string]any{"name": name, "arguments": arguments, "chatId": actor, "callId": callID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(address, "/")+"/internal/warden-tools", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Panta-Key", key)
	req.Header.Set("X-Panta-Organization", organization)
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("documents service unavailable; retry shortly")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("document operation failed (%d): %s", res.StatusCode, raw)
	}
	return raw, nil
}
