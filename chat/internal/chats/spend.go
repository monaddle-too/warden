package chats

import (
	"time"
	cv "warden/chat/internal/conversation"
)

// Spend (docs/claude-parity.md, R2.2): what the agent's turns took,
// summed from each chat's turn records (conversation.Turn, the same rows
// the web's /cost card and the TUI's /cost sum locally): per chat on the
// state (Chat.Spend, for the chat header and the workspace panel's sum),
// and for the admin console the totals of today, the last seven days and
// all time by provider, over every chat, archived ones included (GET
// spend). Side questions and title one-shots are not turns and are not
// counted. Codex reports tokens and no cost.

// Spend is the sum of some turns: how many, their tokens (Input counts
// the cached ones), and the provider's cost estimate where it gave one —
// Priced says whether any turn did, so a zero cost reads right.
type Spend struct {
	Turns   int     `json:"turns"`
	Input   int64   `json:"input"`
	Output  int64   `json:"output"`
	Total   int64   `json:"total"`
	CostUSD float64 `json:"costUSD"`
	Priced  bool    `json:"priced"`
}

// add sums one turn's usage in.
func (s *Spend) add(u *cv.Usage) {
	s.Turns++
	if u == nil {
		return
	}
	s.Input += u.Input
	s.Output += u.Output
	s.Total += u.Total
	if u.CostUSD > 0 {
		s.Priced = true
		s.CostUSD += u.CostUSD
	}
}

// spendOf sums a chat's turn records.
func spendOf(turns []cv.Turn) Spend {
	var s Spend
	for i := range turns {
		s.add(turns[i].Usage)
	}
	return s
}

// SpendPeriod is the spend over a period: the sum, by provider, and the
// chats that had a turn in it.
type SpendPeriod struct {
	Spend
	Chats     int              `json:"chats"`
	Providers map[string]Spend `json:"providers"`
}

// SpendReport is what GET spend answers: today (since the local
// midnight), the last seven days and all time, each a period, plus when
// it was computed.
type SpendReport struct {
	Today SpendPeriod `json:"today"`
	Week  SpendPeriod `json:"week"`
	All   SpendPeriod `json:"all"`
	At    float64     `json:"at"`
}

// turnAt is when a turn counts: its end, or its start while it runs or
// when a restart cut it short.
func turnAt(t cv.Turn) float64 {
	if t.EndedAt > 0 {
		return t.EndedAt
	}
	return t.StartedAt
}

// Spend computes the report over every chat in the store.
func (e *Engine) Spend() SpendReport {
	now := e.now()
	year, month, day := now.Date()
	midnight := float64(time.Date(year, month, day, 0, 0, 0, 0, now.Location()).Unix())
	week := float64(now.Add(-7 * 24 * time.Hour).Unix())
	st := e.Store.Snapshot()
	periods := []struct {
		since  float64
		period *SpendPeriod
	}{{midnight, &SpendPeriod{}}, {week, &SpendPeriod{}}, {0, &SpendPeriod{}}}
	for i := range periods {
		periods[i].period.Providers = map[string]Spend{}
	}
	for _, c := range st.Chats {
		provider := c.Provider
		if provider == "" {
			provider = "codex"
		}
		for i := range periods {
			p := periods[i]
			counted := false
			for _, t := range c.Conversation.Turns {
				if turnAt(t) < p.since {
					continue
				}
				p.period.add(t.Usage)
				by := p.period.Providers[provider]
				by.add(t.Usage)
				p.period.Providers[provider] = by
				counted = true
			}
			if counted {
				p.period.Chats++
			}
		}
	}
	return SpendReport{Today: *periods[0].period, Week: *periods[1].period, All: *periods[2].period, At: float64(now.UnixMilli()) / 1000}
}
