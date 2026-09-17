package conversation

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Conversation struct {
	ThreadID     *string `json:"threadID,omitempty"`
	ActiveTurnID *string `json:"activeTurnID,omitempty"`
	Entries      []Entry `json:"entries"`
	// Turns records what each agent turn took: when it started and ended
	// and the tokens the provider reported for it. Entries name their turn
	// in TurnID; a turn from before this record was kept has no row.
	Turns []Turn `json:"turns,omitempty"`
}

// Turn is the service's record of one agent turn. StartedAt is when the
// agent accepted the message and EndedAt when the service saw the turn
// complete, fail or stop (unix seconds like Entry.CreatedAt; 0 while it
// runs, or after a restart cut it short). Usage is the provider's token
// report for the turn, nil until it reports one.
type Turn struct {
	ID        string  `json:"id"`
	StartedAt float64 `json:"startedAt,omitempty"`
	EndedAt   float64 `json:"endedAt,omitempty"`
	Usage     *Usage  `json:"usage,omitempty"`
}

// Usage is the token usage of one turn summed over its model calls. Input
// counts every input token, the cached and cache-written ones included
// (Cached and CacheWrite are the parts of it); Reasoning is the part of
// Output spent thinking where the provider says; CostUSD is the provider's
// own estimate and 0 when it gives none.
type Usage struct {
	Input      int64   `json:"input"`
	Cached     int64   `json:"cached"`
	CacheWrite int64   `json:"cacheWrite,omitempty"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning,omitempty"`
	Total      int64   `json:"total"`
	CostUSD    float64 `json:"costUSD,omitempty"`
}

// Sub is the usage between two of a process's running totals: what one
// turn cost. A component that would go negative (a total that restarted)
// is the later total itself.
func (u Usage) Sub(base Usage) Usage {
	d := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return a - b
	}
	cost := u.CostUSD - base.CostUSD
	if cost < 0 {
		cost = u.CostUSD
	}
	return Usage{Input: d(u.Input, base.Input), Cached: d(u.Cached, base.Cached), CacheWrite: d(u.CacheWrite, base.CacheWrite), Output: d(u.Output, base.Output), Reasoning: d(u.Reasoning, base.Reasoning), Total: d(u.Total, base.Total), CostUSD: cost}
}

// UsageFrom reads a token usage breakdown as the agent protocol carries it
// (Codex's `thread/tokenUsage/updated` fields; `costUSD` is Warden's own
// addition, filled in by the Claude adapter).
func UsageFrom(m map[string]any) Usage {
	n := func(k string) int64 { f, _ := m[k].(float64); return int64(f) }
	cost, _ := m["costUSD"].(float64)
	return Usage{Input: n("inputTokens"), Cached: n("cachedInputTokens"), CacheWrite: n("cacheWriteInputTokens"), Output: n("outputTokens"), Reasoning: n("reasoningOutputTokens"), Total: n("totalTokens"), CostUSD: cost}
}

type Actor struct {
	PrincipalID string `json:"principalID"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
}
type Entry struct {
	Sender      *Actor  `json:"sender,omitempty"`
	ID          string  `json:"id"`
	Role        string  `json:"role"`
	Text        string  `json:"text"`
	Detail      string  `json:"detail"`
	TurnID      *string `json:"turnID,omitempty"`
	CreatedAt   float64 `json:"createdAt"`
	IsStreaming bool    `json:"isStreaming"`
	Delivery    string  `json:"delivery"`
	// Attachments are the files the sender added to a user message. Each
	// is written into the sandbox workspace at Path when the message is
	// delivered; the chat service keeps its own copy for the transcript.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file sent with a user message. Kind is "image" for a
// PNG/JPEG (stored and delivered as an imageguard-normalised PNG) and
// "file" for anything else. Name is the sender's file name, for display
// only; Path is where the agent finds the file, relative to the workspace.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func Ptr[T any](v T) *T { return &v }
func NewEntry(role, text string) Entry {
	return Entry{ID: ID(), Role: role, Text: text, CreatedAt: float64(time.Now().UnixMilli()) / 1000}
}
