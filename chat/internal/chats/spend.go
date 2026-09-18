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
// spend). Side questions (R2.19) count too — their one-shots are the
// chat's spend as much as its turns — marked as asides (Asides,
// AsideCostUSD) beside the turns; title one-shots (a fraction of a cent
// each) are not counted. Codex reports tokens and no cost.

// Spend is the sum of some turns and side questions: how many of each,
// their tokens (Input counts the cached ones), and the provider's cost
// estimate where it gave one — Priced says whether anything was priced,
// so a zero cost reads right. AsideCostUSD is the side questions' share
// of CostUSD.
type Spend struct {
	Turns        int     `json:"turns"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	Total        int64   `json:"total"`
	CostUSD      float64 `json:"costUSD"`
	Priced       bool    `json:"priced"`
	Asides       int     `json:"asides,omitempty"`
	AsideCostUSD float64 `json:"asideCostUSD,omitempty"`
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

// addAside sums one answered side question in.
func (s *Spend) addAside(a *cv.Aside) {
	s.Asides++
	s.Input += a.Input
	s.Output += a.Output
	s.Total += a.Input + a.Output
	if a.CostUSD > 0 {
		s.Priced = true
		s.CostUSD += a.CostUSD
		s.AsideCostUSD += a.CostUSD
	}
}

// spendOf sums a chat's turn records and its answered side questions.
func spendOf(c *Chat) Spend {
	var s Spend
	for i := range c.Conversation.Turns {
		s.add(c.Conversation.Turns[i].Usage)
	}
	for _, v := range c.Conversation.Entries {
		if asideCounts(v) {
			s.addAside(v.Aside)
		}
	}
	return s
}

// asideCounts reports whether entry v is a side question the model
// answered (a failed or unanswered one cost nothing the CLI reported).
func asideCounts(v cv.Entry) bool {
	return v.Role == "aside" && v.Aside != nil && v.Aside.Status == "completed"
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
			for _, v := range c.Conversation.Entries {
				if !asideCounts(v) || v.EndedAt < p.since {
					continue
				}
				p.period.addAside(v.Aside)
				by := p.period.Providers[provider]
				by.addAside(v.Aside)
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
