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
