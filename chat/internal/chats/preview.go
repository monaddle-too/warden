package chats

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"warden/chat/internal/agent"
	"warden/chat/internal/sandbox"
)

func previewTools() []any {
	return []any{map[string]any{"type": "function", "name": "preview_attach", "description": "Attach a web preview to this chat. Start its server on 0.0.0.0 inside this sandbox first. Warden chooses the loopback URL.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "path": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}}, "required": []string{"port", "path", "title"}, "additionalProperties": false}}}
}
func (e *Engine) tool(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	if agent.String(f.Params["tool"]) == "attach_image" {
		return e.imageTool(ctx, c, client, f)
	}
	if _, ok := grantMethods[agent.String(f.Params["tool"])]; ok {
		return e.requestGrant(c, client, f)
	}
	if name := agent.String(f.Params["tool"]); name == "request_google_document_creation" || name == "request_google_docs_access" || name == "list_shared_documents" || name == "list_shared_repositories" || name == "request_pull_request" || name == "read_google_document" || name == "propose_google_document_edit" {
		return e.sharingTool(ctx, c, client, f)
	}
	if e.PublicPreviewSuffix != "" && (agent.String(f.Params["tool"]) == "preview_attach" || agent.String(f.Params["tool"]) == "sandbox_bind_port") {
		return e.requestPort(c, client, f)
	}
	var res sandbox.Response
	var err error
	if agent.String(f.Params["tool"]) != "preview_attach" {
		err = errors.New("unsupported tool")
	} else {
		var input struct {
			Port  int    `json:"port"`
			Path  string `json:"path"`
			Title string `json:"title"`
		}
		raw, _ := json.Marshal(f.Params["arguments"])
		if value, ok := f.Params["arguments"].(string); ok {
			raw = []byte(value)
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&input)
		if err == nil {
			r := request(c, "preview.attach")
			r.Port = input.Port
			r.Path = input.Path
			r.Title = input.Title
			r.CallID = agent.String(f.Params["callId"])
			if r.CallID == "" {
				r.CallID = string(f.ID)
			}
			res, err = e.Worker.Call(ctx, r)
		}
		if err == nil {
			if res.Attachment == nil || res.Attachment.ChatID != c.ID || res.Attachment.SandboxID != c.SandboxID {
				err = errors.New("preview binding mismatch")
			} else {
				err = validateAttachment(*res.Attachment)
			}
		}
	}
	text := ""
	if err != nil {
		text = err.Error()
	} else {
		b, _ := json.Marshal(res.Attachment)
		text = string(b)
	}
	return client.Reply(f.ID, map[string]any{"success": err == nil, "contentItems": []any{map[string]any{"type": "inputText", "text": text}}})
}
func validateAttachment(a sandbox.PreviewAttachment) error {
	if a.State != "available" {
		return nil
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil {
		return errors.New("invalid worker preview URL")
	}
	return nil
}
