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
	"net/http"
	"strings"
	"time"
)

// Types mirror the JSON the chat service serves (chat/internal/chats).

type Entry struct {
	ID          string  `json:"id"`
	Role        string  `json:"role"`
	Text        string  `json:"text"`
	Detail      string  `json:"detail"`
	CreatedAt   float64 `json:"createdAt"`
	IsStreaming bool    `json:"isStreaming"`
	Delivery    string  `json:"delivery"`
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
	ID           string `json:"id"`
	Title        string `json:"title"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SandboxID    string `json:"sandboxID"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	Archived     bool   `json:"archived"`
	Conversation struct {
		Entries []Entry `json:"entries"`
	} `json:"conversation"`
	Approvals []Approval `json:"approvals"`
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

func (c *Client) Create(ctx context.Context, title, provider, model, sandboxID string) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	body := map[string]any{"title": title, "provider": provider, "model": model, "sandboxID": sandboxID}
	if err := c.do(ctx, "POST", "chats", body, &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

func (c *Client) Message(ctx context.Context, chatID, text, messageID string) error {
	return c.do(ctx, "POST", "chats/"+chatID+"/message", map[string]any{"text": text, "id": messageID}, nil)
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
