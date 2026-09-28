package chats

import (
	"context"
	"warden/chat/internal/agent"
	"warden/chat/internal/documenttools"
)

const pantaPrompt = " All Panta documents in this chat's organization are available to you by default. Use panta_list_documents and panta_read_document to discover and read them; no sharing request or token setup is needed. You may create, edit and comment directly when asked. When asked for suggestions, use panta_propose_document_edit: it leaves the live document unchanged and offers the person an accept/reject review. Use the exact revision returned by panta_read_document for edits and suggestions; reread on conflict. Use UUID operationId values and reuse them for identical retries. Link documents using /documents/<id>. Google document tools are for external Google Docs only."

func pantaTools() []any { return documenttools.Tools() }
func (e *Engine) pantaCall(ctx context.Context, c *Chat, name, callID string, arguments any) ([]byte, error) {
	return documenttools.Call(ctx, e.DocsAddress, e.DocsKey, c.OrganizationID, c.ID, name, callID, arguments)
}
func (e *Engine) pantaTool(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	callID := agent.String(f.Params["callId"])
	if callID == "" {
		callID = string(f.ID)
	}
	raw, err := e.pantaCall(ctx, c, agent.String(f.Params["tool"]), callID, f.Params["arguments"])
	text := string(raw)
	if err != nil {
		text = err.Error()
	}
	return client.Reply(f.ID, map[string]any{"success": err == nil, "contentItems": []any{map[string]any{"type": "inputText", "text": text}}})
}
