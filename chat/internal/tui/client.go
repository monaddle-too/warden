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
	"warden/chat/internal/chats"

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
	// Compaction is what a compaction entry records (conversation.Compaction):
	// the agent compacted its context here; Detail is the summary.
	Compaction *Compaction `json:"compaction,omitempty"`
	// Fork is what a fork marker records (conversation.Fork): the chat
	// this one was copied from and the message the copy stops before.
	Fork *Fork `json:"fork,omitempty"`
	// Aside is what an aside entry records (conversation.Aside): a side
	// question (Text) answered from a copy of the session (Detail).
	Aside *Aside `json:"aside,omitempty"`
	// Rewind is what a rewind marker records (conversation.Rewind): the
	// message the chat went back to before, the scope, how the session
	// followed and the checkpoint of the workspace before a code rewind.
	Rewind *RewindMark `json:"rewind,omitempty"`
	Sender *struct {
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

// Fork mirrors conversation.Fork.
type Fork struct {
	ChatID    string `json:"chatID"`
	Title     string `json:"title"`
	MessageID string `json:"messageID"`
	Workspace bool   `json:"workspace"`
	Into      bool   `json:"into"`
}

// Aside mirrors conversation.Aside: how a side question went and what it
// cost.
type Aside struct {
	Status     string  `json:"status"`
	Error      string  `json:"error"`
	CostUSD    float64 `json:"costUSD"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	DurationMS int64   `json:"durationMS"`
	// Promoted is the user message the question was asked in chat as.
	Promoted string `json:"promoted"`
}

// Compaction mirrors conversation.Compaction: how the agent's context was
// compacted (manual for /compact, auto), the context before and the
// summary after in tokens, and whether it is running, completed or failed.
type Compaction struct {
	Trigger    string `json:"trigger"`
	PreTokens  int64  `json:"preTokens"`
	PostTokens int64  `json:"postTokens"`
	Status     string `json:"status"`
	Error      string `json:"error"`
}

// Context mirrors conversation.Context: what the agent's latest model call
// was given against the model's window, in tokens.
type Context struct {
	Used      int64  `json:"used"`
	Window    int64  `json:"window"`
	Threshold int64  `json:"threshold"` // where the agent compacts on its own; 0 when unknown
	Model     string `json:"model"`
}

// AgentCommand is one slash command the agent's session offers
// (chats.Command): sent as text, the agent expands it.
type AgentCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
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
	Context      *Context          `json:"context,omitempty"`
	Raw          []json.RawMessage `json:"-"`
}

func (c *Conversation) UnmarshalJSON(b []byte) error {
	var raw struct {
		ThreadID     *string           `json:"threadID"`
		ActiveTurnID *string           `json:"activeTurnID"`
		Entries      []json.RawMessage `json:"entries"`
		Turns        []Turn            `json:"turns"`
		Context      *Context          `json:"context"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.ThreadID, c.ActiveTurnID, c.Turns, c.Context, c.Raw = raw.ThreadID, raw.ActiveTurnID, raw.Turns, raw.Context, raw.Entries
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
	// ask or plan (chats/permissions.go). Thinking, Effort and Fast are
	// its session settings (chats/settings.go): "" is the default
	// thinking or effort, "off" or a token budget the thinking.
	Mode     string `json:"mode"`
	Thinking string `json:"thinking"`
	Effort   string `json:"effort"`
	Fast     bool   `json:"fast"`
	// Session is what the agent reported when its session started; its
	// Model is the one it resolved, the truth after a live model change;
	// OutputStyle is the style the running session has.
	Session    *SessionInfo `json:"session"`
	SandboxID  string       `json:"sandboxID"`
	Repository string       `json:"repository,omitempty"`
	// Jailbroken: the workspace has host access (chats/host.go).
	Jailbroken   bool         `json:"jailbroken"`
	Status       string       `json:"status"`
	Error        string       `json:"error,omitempty"`
	Archived     bool         `json:"archived"`
	Conversation Conversation `json:"conversation"`
	Approvals    []Approval   `json:"approvals"`
	// Reviews are the agent's requests only the app can settle (a pull
	// request proposal, suggested document edits, a document selection or
	// creation), listed while they wait (chats/reviews.go); /review opens
	// the app on them.
	Reviews []Review `json:"reviews"`
	// Commands are the slash commands the agent's session offers (Claude
	// Code's built-ins and the workspace's own), for the / menu.
	Commands []AgentCommand `json:"commands"`
	// OutputStyle is a Claude chat's output style for its next launch ("":
	// the default); Session.OutputStyle is what the running one has.
	OutputStyle string `json:"outputStyle"`
	// UndoRewind is the rewind marker whose conversation rewind can still
	// be undone (/undo-rewind; rewind.go), "" when none.
	UndoRewind string `json:"undoRewind"`
	// Startup is where the chat's start is while its message waits for the
	// agent: the stage and the runtime's detail.
	Startup *struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	} `json:"startup"`
}

// SessionInfo mirrors chats.Session: the agent's own report of its
// session settings.
type SessionInfo struct {
	Model          string `json:"model"`
	FastMode       string `json:"fastMode"`
	PermissionMode string `json:"permissionMode"`
	OutputStyle    string `json:"outputStyle"`
}

// Permission is a tool ask of a Claude chat in ask or plan mode (method
// item/tool/requestPermission): the tool, the CLI's description, the
// call as a transcript entry (a command, a diff), what "allow always"
// remembers, and the plan when the tool is ExitPlanMode.
type Permission struct {
	Tool        string `json:"tool"`
	Description string `json:"description"`
	Always      string `json:"always"`
	Rule        string `json:"rule"` // the pattern "allow always" records (rules.go)
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

// Review mirrors chats.Review: what the agent proposed and where.
type Review struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"` // pull_request, document_edit, document_access, document_create
	Status      string  `json:"status"`
	Title       string  `json:"title"`
	Repository  string  `json:"repository"`
	Document    string  `json:"document"`
	Changes     int     `json:"changes"`
	RequestedAt float64 `json:"requestedAt"`
}

// Summary is the one line a card, a notification or a log names the
// review by: who proposed what, sanitised.
func (r Review) Summary(provider string) string {
	who := ProviderName(provider)
	switch r.Kind {
	case "pull_request":
		s := who + " proposed a pull request “" + r.Title + "”"
		if r.Repository != "" {
			s += " to " + r.Repository
		}
		return sanitize(s)
	case "document_edit":
		if r.Status == "applying" {
			return sanitize("Writing the suggested edits to “" + r.Document + "”")
		}
		n := "changes"
		if r.Changes == 1 {
			n = "change"
		}
		return sanitize(fmt.Sprintf("%s suggested %d %s to “%s”", who, r.Changes, n, r.Document))
	case "document_create":
		return sanitize(who + " asked to create a document “" + r.Document + "”")
	}
	return who + " asked to choose documents"
}

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
	// AgentOptions mirrors chats.AgentOptions: the costlier Claude
	// features this Warden allows and each provider's model catalog.
	AgentOptions AgentOptions `json:"agentOptions"`
}

// AgentOptions mirrors chats.AgentOptions.
type AgentOptions struct {
	FastMode    bool                   `json:"fastMode"`
	LongContext bool                   `json:"longContext"`
	Models      map[string][]ModelInfo `json:"models"`
	// Defaults is the model a chat of each provider starts with.
	Defaults map[string]string `json:"defaults"`
}

// ModelInfo mirrors chats.ModelInfo: one row of a provider's catalog as
// its CLI reported it.
type ModelInfo struct {
	Value            string   `json:"value"`
	Resolved         string   `json:"resolved"`
	Label            string   `json:"label"`
	Description      string   `json:"description"`
	Efforts          []string `json:"efforts"`
	AdaptiveThinking bool     `json:"adaptiveThinking"`
	FastMode         bool     `json:"fastMode"`
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
// Create starts a chat: on a fresh workspace of the given size (nil is the
// runner's default) and, when network is "restricted" or "open", with that
// network access of its own (the owner's choice; "" follows the install),
// or sharing sandboxID's workspace.
func (c *Client) Create(ctx context.Context, title, provider, model, sandboxID string, resources *sandbox.Resources, network ...string) (string, error) {
	req := CreateRequest{Title: title, Provider: provider, Model: model, SandboxID: sandboxID, Resources: resources}
	if len(network) > 0 {
		req.Network = network[0]
	}
	return c.CreateChat(ctx, req)
}

// CreateRequest is what a chat is created with: Create's arguments plus
// Jailbreak, the fresh workspace's host access (the owner's choice on a
// Warden with dogfood.jailbreak; chats/host.go).
type CreateRequest struct {
	Title, Provider, Model, SandboxID string
	Resources                         *sandbox.Resources
	Network                           string
	Jailbreak                         bool
}

// CreateChat starts a chat as CreateRequest says.
func (c *Client) CreateChat(ctx context.Context, req CreateRequest) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	body := map[string]any{"title": req.Title, "provider": req.Provider, "model": req.Model, "sandboxID": req.SandboxID}
	if req.Resources != nil {
		body["resources"] = req.Resources
	}
	if req.Network != "" {
		body["network"] = req.Network
	}
	if req.Jailbreak {
		body["jailbreak"] = true
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

// Exec runs a shell command in the chat's workspace as the person (the
// composer's "!cmd"); the transcript gets a command card attributed to
// them, and the answer is what it came to.
func (c *Client) Exec(ctx context.Context, chatID, command string) (ExecResult, error) {
	var result ExecResult
	err := c.do(ctx, "POST", "chats/"+chatID+"/exec", map[string]any{"text": command}, &result)
	return result, err
}

// ExecResult mirrors chats.ExecResult.
type ExecResult struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Output   string `json:"output"`
}

// Memory appends a note to the workspace's CLAUDE.md (the composer's
// "#note"); the transcript gets a system line saying so.
func (c *Client) Memory(ctx context.Context, chatID, note string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/memory", map[string]any{"text": note}, nil)
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

// Resources lists what the chat can mention with "@": its workspace's
// shared documents, repositories and previews (chats/mentions.go).
func (c *Client) Resources(ctx context.Context, chatID string) (chats.Resources, error) {
	var res chats.Resources
	err := c.do(ctx, "GET", "chats/"+url.PathEscape(chatID)+"/resources", nil, &res)
	return res, err
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
// rule is remembered for the chat, or for the workspace when scope is
// "workspace"), or deny with a message the model reads; for a plan, allow
// with the mode the chat moves to (auto or ask) or deny with feedback.
func (c *Client) Answer(ctx context.Context, chatID, approvalID string, allow, always bool, scope, message, mode string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/approvals/"+approvalID, map[string]any{"allow": allow, "always": always, "scope": scope, "message": message, "mode": mode, "answers": map[string][]string{}}, nil)
}

// Mode sets a Claude chat's permission mode (auto, ask or plan).
// Instructions is a person's standing instructions for the agent, as
// me/instructions answers.
type Instructions struct {
	Text      string  `json:"text"`
	UpdatedAt float64 `json:"updatedAt,omitempty"`
	Name      string  `json:"name,omitempty"`
}

func (c *Client) Instructions(ctx context.Context) (Instructions, error) {
	var v Instructions
	err := c.do(ctx, "GET", "me/instructions", nil, &v)
	return v, err
}

// SetInstructions replaces the person's text; blank removes it.
func (c *Client) SetInstructions(ctx context.Context, text string) error {
	return c.do(ctx, "POST", "me/instructions", map[string]any{"text": text}, nil)
}

// MemoryFile is one of the workspace's instruction or memory files, as
// chats/{id}/memory lists them: Scope "workspace" (under the workspace
// root) or "auto" (under the CLI's auto-memory directory).
type MemoryFile struct {
	Scope     string `json:"scope"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Label is how the file is named in the listing and addressed by /memory:
// the path, prefixed "auto:" for an auto-memory file.
func (f MemoryFile) Label() string {
	if f.Scope == "auto" {
		return "auto:" + f.Path
	}
	return f.Path
}

// MemoryView is chats/{id}/memory: the files, where they are, and whether
// the agent's launch reads them (Hint says).
type MemoryView struct {
	Root    string       `json:"root"`
	AutoDir string       `json:"autoDir"`
	Exists  bool         `json:"autoDirExists"`
	Files   []MemoryFile `json:"files"`
	Read    bool         `json:"read"`
	Hint    string       `json:"hint,omitempty"`
}

// MemoryFiles lists the workspace's memory files with their contents.
func (c *Client) MemoryFiles(ctx context.Context, chatID string) (MemoryView, error) {
	var v MemoryView
	err := c.do(ctx, "GET", "chats/"+chatID+"/memory", nil, &v)
	return v, err
}

// WriteMemory replaces one memory file's contents.
func (c *Client) WriteMemory(ctx context.Context, chatID, scope, path, text string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/memory/write", map[string]any{"scope": scope, "path": path, "text": text}, nil)
}

func (c *Client) Mode(ctx context.Context, chatID, mode string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/mode", map[string]any{"mode": mode}, nil)
}

// Settings changes a Claude chat's session settings: the keys given
// (thinking, effort, fast) apply, the rest stay.
func (c *Client) Settings(ctx context.Context, chatID string, change map[string]any) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/settings", change, nil)
}

// Spend is the spend report (chats.SpendReport): today, the week, all time.
func (c *Client) Spend(ctx context.Context) (chats.SpendReport, error) {
	var v chats.SpendReport
	err := c.do(ctx, "GET", "spend", nil, &v)
	return v, err
}

func (c *Client) RevokePort(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "ports/"+id+"/revoke", map[string]any{}, nil)
}

// Events follows the server's event stream, delivering every full snapshot
// to receive until ctx ends. It reconnects after a pause when the stream
// drops; a 401 is fatal because the capability has rotated.
func (c *Client) Events(ctx context.Context, receive func(*State)) error {
	for {
		err := c.Stream(ctx, receive)
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

// Stream follows one connection to the event stream until it drops or
// ctx ends; Events wraps it with the reconnects. A caller that needs to
// know when the service is unreachable (the menu bar feed) uses it
// directly.
func (c *Client) Stream(ctx context.Context, receive func(*State)) error {
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

// Checkpoint is one workspace checkpoint as the runner records it: the
// user message it was taken before (ID) and the snapshot commit.
type Checkpoint struct {
	ID      string `json:"id"`
	ChatID  string `json:"chatID"`
	Commit  string `json:"commit"`
	Store   string `json:"store"`
	Changed bool   `json:"changed"`
}

// ChangedFile is one file of the session diff with its counts.
type ChangedFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"`
}

// WorkspaceChanges is the session diff: the workspace against the chat's
// first checkpoint (or its last code rewind), as git's unified diff.
type WorkspaceChanges struct {
	Base      string        `json:"base"`
	Files     []ChangedFile `json:"files"`
	Diff      string        `json:"diff"`
	Truncated bool          `json:"truncated"`
}

// RewindResult is what a rewind did (chats.RewindResult).
type RewindResult struct {
	MessageID    string   `json:"messageID"`
	What         string   `json:"what"`
	Restored     []string `json:"restored"`
	Removed      []string `json:"removed"`
	Conversation string   `json:"conversation"`
	// Withdrawn counts the queued messages a conversation rewind took
	// out of the queue (queue.go).
	Withdrawn int `json:"withdrawn"`
}

// RewindMark mirrors conversation.Rewind.
type RewindMark struct {
	MessageID    string `json:"messageID"`
	What         string `json:"what"`
	Conversation string `json:"conversation"`
	Before       string `json:"before"`
}

// UndoResult is what undoing a rewind did (chats.UndoResult).
type UndoResult struct {
	MessageID string   `json:"messageID"`
	What      string   `json:"what"`
	Entries   int      `json:"entries"`
	Requeued  int      `json:"requeued"`
	Session   string   `json:"session"`
	Code      string   `json:"code"`
	Restored  []string `json:"restored"`
	Removed   []string `json:"removed"`
}

// EditQueued replaces a queued message's text in place, keeping its slot
// and ID (queue.go); the edited entry comes back. A message the agent got
// meanwhile is refused.
func (c *Client) EditQueued(ctx context.Context, chatID, messageID, text string) (Entry, error) {
	var out Entry
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/queued/"+url.PathEscape(messageID)+"/edit", map[string]string{"text": text}, &out)
	return out, err
}

// UndoRewind puts back what the rewind marked by markerID removed; code
// asks for the workspace as it was before the rewind too (rewind.go).
func (c *Client) UndoRewind(ctx context.Context, chatID, markerID string, code bool) (UndoResult, error) {
	var out UndoResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/undo-rewind", map[string]any{"id": markerID, "code": code}, &out)
	return out, err
}

// Withdraw takes a queued message out of the chat before the agent gets
// it; the entry comes back for the editor (queue.go).
func (c *Client) Withdraw(ctx context.Context, chatID, messageID string) (Entry, error) {
	var out Entry
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/withdraw", map[string]string{"id": messageID}, &out)
	return out, err
}

// SendQueued lets a held queue go: the queued messages send in order.
func (c *Client) SendQueued(ctx context.Context, chatID string) error {
	return c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/send-queued", map[string]any{}, nil)
}

func (c *Client) Checkpoints(ctx context.Context, chatID string) ([]Checkpoint, error) {
	var out struct {
		Checkpoints []Checkpoint `json:"checkpoints"`
	}
	err := c.do(ctx, "GET", "chats/"+url.PathEscape(chatID)+"/checkpoints", nil, &out)
	return out.Checkpoints, err
}

// Rewind takes the chat back to before a user message: what is "code",
// "conversation" or "both".
func (c *Client) Rewind(ctx context.Context, chatID, messageID, what string) (RewindResult, error) {
	var out RewindResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/rewind", map[string]string{"turnID": messageID, "what": what}, &out)
	return out, err
}

// ForkResult is what a fork made (chats.ForkResult).
type ForkResult struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Session   string `json:"session"`
	SandboxID string `json:"sandboxID"`
	Workspace string `json:"workspace"`
}

// Fork copies the chat into a sibling up to messageID ("" for the whole
// of it); copy gives the fork a copy of the workspace too.
func (c *Client) Fork(ctx context.Context, chatID, messageID string, copy bool) (ForkResult, error) {
	var out ForkResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/fork", map[string]any{"turnID": messageID, "copyWorkspace": copy}, &out)
	return out, err
}

// AsideResult is what a side question came to (chats.AsideResult).
type AsideResult struct {
	ID      string  `json:"id"`
	Text    string  `json:"text"`
	Error   string  `json:"error"`
	CostUSD float64 `json:"costUSD"`
}

// Aside asks a side question of a copy of the chat's session.
func (c *Client) Aside(ctx context.Context, chatID, question string) (AsideResult, error) {
	var out AsideResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/aside", map[string]string{"text": question}, &out)
	return out, err
}

// PromoteResult is what promoting a side question came to
// (chats.PromoteResult): the message it went as and its text.
type PromoteResult struct {
	MessageID string `json:"messageID"`
	Text      string `json:"text"`
}

// PromoteAside asks a side question in chat: its question goes as the
// person's message with the answer quoted.
func (c *Client) PromoteAside(ctx context.Context, chatID, entryID string) (PromoteResult, error) {
	var out PromoteResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/aside/"+url.PathEscape(entryID)+"/promote", map[string]string{}, &out)
	return out, err
}

// Style sets a Claude chat's output style for its next launch.
func (c *Client) Style(ctx context.Context, chatID, style string) error {
	return c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/style", map[string]string{"style": style}, nil)
}

func (c *Client) Diff(ctx context.Context, chatID string) (*WorkspaceChanges, error) {
	var out WorkspaceChanges
	if err := c.do(ctx, "GET", "chats/"+url.PathEscape(chatID)+"/diff", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BugResult is what the bug routes answer (docs/bug-reporting-plan.md):
// whether a draft was written, its id, and the line to show the person.
type BugResult struct {
	Drafted bool   `json:"drafted"`
	ID      string `json:"id,omitempty"`
	Notice  string `json:"notice"`
}

// Bug drafts a user bug report from the chat (the composer's /bug); the
// launcher presents it for review.
func (c *Client) Bug(ctx context.Context, chatID, text string) (BugResult, error) {
	var out BugResult
	err := c.do(ctx, "POST", "chats/"+url.PathEscape(chatID)+"/bug", map[string]string{"text": text}, &out)
	return out, err
}

// BugTest raises the test exception in the chat service (/test
// bugreporting), which drafts a report through its own recovery.
func (c *Client) BugTest(ctx context.Context) (BugResult, error) {
	var out BugResult
	err := c.do(ctx, "POST", "bug-test", map[string]string{}, &out)
	return out, err
}
