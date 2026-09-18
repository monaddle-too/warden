// Package tui is Warden's terminal client: a thin client of the same HTTP API
// and event stream the web UI uses, so a chat driven from the terminal is
// the same chat the browser shows, with the same store, approvals and trust
// line (the person types on the host; only the agent runs in the sandbox).
package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"warden/chat/internal/sandbox"
)

// Types mirror the JSON the chat service serves (chat/internal/chats).

type Entry struct {
	ID          string  `json:"id"`
	Role        string  `json:"role"`
	Text        string  `json:"text"`
	Detail      string  `json:"detail"`
	TurnID      *string `json:"turnID,omitempty"`
	CreatedAt   float64 `json:"createdAt"`
	EndedAt     float64 `json:"endedAt,omitempty"`
	IsStreaming bool    `json:"isStreaming"`
	Delivery    string  `json:"delivery"`
	Tool        *Tool   `json:"tool"` // the tool call an activity entry records (render.go)
	// ParentID names the subagent's card (an Agent call) this entry
	// belongs to; "" for the conversation's own entries.
	ParentID string `json:"parentID,omitempty"`
	Sender   *struct {
		PrincipalID string `json:"principalID"`
		Email       string `json:"email"`
		Name        string `json:"name"`
	} `json:"sender,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file sent with a user message (conversation.Attachment).
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// Turn and Usage mirror conversation.Turn: what one agent turn took.
type Turn struct {
	ID        string  `json:"id"`
	StartedAt float64 `json:"startedAt,omitempty"`
	EndedAt   float64 `json:"endedAt,omitempty"`
	Usage     *Usage  `json:"usage,omitempty"`
}

type Usage struct {
	Input      int64   `json:"input"`
	Cached     int64   `json:"cached"`
	CacheWrite int64   `json:"cacheWrite,omitempty"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning,omitempty"`
	Total      int64   `json:"total"`
	CostUSD    float64 `json:"costUSD,omitempty"`
}

// Conversation is the chat's transcript. Raw keeps each entry's JSON as the
// service sent it, so an export carries fields this client does not model.
type Conversation struct {
	ThreadID     *string           `json:"threadID,omitempty"`
	ActiveTurnID *string           `json:"activeTurnID,omitempty"`
	Entries      []Entry           `json:"entries"`
	Turns        []Turn            `json:"turns,omitempty"`
	Raw          []json.RawMessage `json:"-"`
}

func (c *Conversation) UnmarshalJSON(b []byte) error {
	var raw struct {
		ThreadID     *string           `json:"threadID"`
		ActiveTurnID *string           `json:"activeTurnID"`
		Entries      []json.RawMessage `json:"entries"`
		Turns        []Turn            `json:"turns"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.ThreadID, c.ActiveTurnID, c.Turns, c.Raw = raw.ThreadID, raw.ActiveTurnID, raw.Turns, raw.Entries
	c.Entries = make([]Entry, 0, len(raw.Entries))
	for _, r := range raw.Entries {
		var e Entry
		if err := json.Unmarshal(r, &e); err != nil {
			return err
		}
		c.Entries = append(c.Entries, e)
	}
	return nil
}

type Question struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type Approval struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	State  string         `json:"state"`
	Params map[string]any `json:"params"`
}

// Questions decodes params.questions when the approval is a user-input request.
func (a Approval) Questions() []Question {
	raw, ok := a.Params["questions"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var qs []Question
	if json.Unmarshal(b, &qs) != nil {
		return nil
	}
	return qs
}

type Chat struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Mode is a Claude chat's permission mode: auto (also when empty),
	// ask or plan (chats/permissions.go).
	Mode         string       `json:"mode"`
	SandboxID    string       `json:"sandboxID"`
	Repository   string       `json:"repository,omitempty"`
	Status       string       `json:"status"`
	Error        string       `json:"error,omitempty"`
	Archived     bool         `json:"archived"`
	Conversation Conversation `json:"conversation"`
	Approvals    []Approval   `json:"approvals"`
	// Startup is where the chat's start is while its message waits for the
	// agent: the stage and the runtime's detail.
	Startup *struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	} `json:"startup"`
}

// Permission is a tool ask of a Claude chat in ask or plan mode (method
// item/tool/requestPermission): the tool, the CLI's description, the
// call as a transcript entry (a command, a diff), what "allow always"
// remembers, and the plan when the tool is ExitPlanMode.
type Permission struct {
	Tool        string `json:"tool"`
	Description string `json:"description"`
	Always      string `json:"always"`
	Plan        string `json:"plan"`
	Entry       *Entry `json:"entry"`
}

// Permission decodes the ask when the approval is one, else nil.
func (a Approval) Permission() *Permission {
	if a.Method != "item/tool/requestPermission" {
		return nil
	}
	b, err := json.Marshal(a.Params)
	if err != nil {
		return nil
	}
	var p Permission
	if json.Unmarshal(b, &p) != nil || p.Tool == "" {
		return nil
	}
	return &p
}

// IsPlan reports whether the ask is the model's plan.
func (p *Permission) IsPlan() bool { return p != nil && p.Tool == "ExitPlanMode" }

// Pending returns the approvals still waiting for the owner.
func (c *Chat) Pending() []Approval {
	var out []Approval
	for _, a := range c.Approvals {
		if a.State == "pending" {
			out = append(out, a)
		}
	}
	return out
}

// Running reports whether a turn is in progress (or about to be).
func (c *Chat) Running() bool {
	return c.Status == "running" || c.Status == "queued" || c.Status == "stopping"
}

// Turn finds a turn record by id.
func (c *Chat) Turn(id string) *Turn {
	for i := range c.Conversation.Turns {
		if c.Conversation.Turns[i].ID == id {
			return &c.Conversation.Turns[i]
		}
	}
	return nil
}

type Port struct {
	ID        string `json:"id"`
	ChatID    string `json:"chatID"`
	SandboxID string `json:"sandboxID"`
	Port      int    `json:"port"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"`
}

type State struct {
	Version int     `json:"version"`
	Chats   []*Chat `json:"chats"`
	Ports   []Port  `json:"ports"`
}

// Chat finds a chat by id.
func (s *State) Chat(id string) *Chat {
	for _, c := range s.Chats {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// Client talks to warden-chat with the owner capability.
type Client struct {
	Base  string // e.g. http://127.0.0.1:18780
	Token string
	HTTP  *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode == 401 {
		return errors.New("Warden sign-in required: the capability in endpoint.json is stale; is Warden running?")
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) State(ctx context.Context) (*State, error) {
	var s State
	if err := c.do(ctx, "GET", "state", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Create starts a chat; resources sizes a fresh workspace (nil is the
// runner's default) and must be nil when sandboxID shares an existing one.
func (c *Client) Create(ctx context.Context, title, provider, model, sandboxID string, resources *sandbox.Resources) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	body := map[string]any{"title": title, "provider": provider, "model": model, "sandboxID": sandboxID}
	if resources != nil {
		body["resources"] = resources
	}
	if err := c.do(ctx, "POST", "chats", body, &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

// Message sends text to the chat; attachments are the IDs of uploads
// (Upload) the message carries.
func (c *Client) Message(ctx context.Context, chatID, text, messageID string, attachments ...string) error {
	body := map[string]any{"text": text, "id": messageID}
	if len(attachments) > 0 {
		body["attachments"] = attachments
	}
	return c.do(ctx, "POST", "chats/"+chatID+"/message", body, nil)
}

// Upload stores one file for the chat (multipart field "file", as the web
// composer sends it) and returns its record; a message then names its ID.
func (c *Client) Upload(ctx context.Context, chatID, name string, data []byte) (Attachment, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return Attachment{}, err
	}
	part.Write(data)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/api/chats/"+chatID+"/attachments", &buf)
	if err != nil {
		return Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Attachment{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Attachment{}, err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return Attachment{}, errors.New(e.Error)
		}
		return Attachment{}, fmt.Errorf("upload: %s: %s", res.Status, strings.TrimSpace(string(raw)))
	}
	var a Attachment
	if err := json.Unmarshal(raw, &a); err != nil || a.ID == "" {
		return Attachment{}, errors.New("upload: unexpected answer")
	}
	return a, nil
}

// RemoveAttachment forgets an upload no message has sent.
func (c *Client) RemoveAttachment(ctx context.Context, chatID, id string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/attachments/"+id+"/remove", map[string]any{}, nil)
}

// Paths completes a partial workspace path (the composer's @-mentions):
// the guest's listing for query, at most 50 names, directories with a
// trailing slash. It needs the sandbox running.
func (c *Client) Paths(ctx context.Context, chatID, query string) ([]string, error) {
	var res struct {
		Paths []string `json:"paths"`
	}
	if err := c.do(ctx, "GET", "chats/"+chatID+"/paths?q="+url.QueryEscape(query), nil, &res); err != nil {
		return nil, err
	}
	return res.Paths, nil
}

// DeleteEnvironment deletes a workspace: its sandbox and files go, its
// chats are archived (what the web's Delete does).
func (c *Client) DeleteEnvironment(ctx context.Context, sandboxID string) error {
	return c.do(ctx, "POST", "environments/"+sandboxID+"/delete", map[string]any{}, nil)
}

func (c *Client) Stop(ctx context.Context, chatID string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/stop", map[string]any{}, nil)
}

func (c *Client) Edit(ctx context.Context, chatID, title string, archived bool) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/edit", map[string]any{"title": title, "archived": archived}, nil)
}

func (c *Client) Agent(ctx context.Context, chatID, provider, model string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/agent", map[string]any{"provider": provider, "model": model}, nil)
}

func (c *Client) Resolve(ctx context.Context, chatID, approvalID string, allow bool, answers map[string][]string) error {
	if answers == nil {
		answers = map[string][]string{}
	}
	return c.do(ctx, "POST", "chats/"+chatID+"/approvals/"+approvalID, map[string]any{"allow": allow, "answers": answers}, nil)
}

// Answer resolves a tool permission ask: allow, allow always (the call's
// rule is remembered for the chat), or deny with a message the model
// reads; for a plan, allow with the mode the chat moves to (auto or ask)
// or deny with feedback.
func (c *Client) Answer(ctx context.Context, chatID, approvalID string, allow, always bool, message, mode string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/approvals/"+approvalID, map[string]any{"allow": allow, "always": always, "message": message, "mode": mode, "answers": map[string][]string{}}, nil)
}

// Mode sets a Claude chat's permission mode (auto, ask or plan).
func (c *Client) Mode(ctx context.Context, chatID, mode string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/mode", map[string]any{"mode": mode}, nil)
}

func (c *Client) RevokePort(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "ports/"+id+"/revoke", map[string]any{}, nil)
}

// Events follows the server's event stream, delivering every full snapshot
// to receive until ctx ends. It reconnects after a pause when the stream
// drops; a 401 is fatal because the capability has rotated.
func (c *Client) Events(ctx context.Context, receive func(*State)) error {
	for {
		err := c.stream(ctx, receive)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && strings.Contains(err.Error(), "sign-in required") {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) stream(ctx context.Context, receive func(*State)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/api/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == 401 {
		return errors.New("Warden sign-in required: the capability has rotated")
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("event stream: %s", res.Status)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case line == "":
			if len(data) > 0 {
				var s State
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &s) == nil {
					receive(&s)
				}
				data = nil
			}
		}
	}
	return scanner.Err()
}
