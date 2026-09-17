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
