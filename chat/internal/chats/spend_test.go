package chats

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
	cv "warden/chat/internal/conversation"
)

func usage(input, output int64, cost float64) *cv.Usage {
	return &cv.Usage{Input: input, Output: output, Total: input + output, CostUSD: cost}
}

// near compares summed costs, floating point being what it is.
func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A chat's spend is the sum of its turn records: turns counted whether or
// not they reported usage, tokens summed, the cost summed and marked
// priced only when a turn carried one (Codex reports none).
func TestSpendOfTurns(t *testing.T) {
	turns := func(list ...cv.Turn) *Chat { c := &Chat{}; c.Conversation.Turns = list; return c }
	s := spendOf(&Chat{})
	if s != (Spend{}) {
		t.Fatalf("empty: %+v", s)
	}
	s = spendOf(turns(cv.Turn{ID: "a", Usage: usage(1000, 100, 0.02)}, cv.Turn{ID: "b"}, cv.Turn{ID: "c", Usage: usage(500, 50, 0.01)}))
	if s.Turns != 3 || s.Input != 1500 || s.Output != 150 || s.Total != 1650 || !near(s.CostUSD, 0.03) || !s.Priced || s.Asides != 0 {
		t.Fatalf("%+v", s)
	}
	s = spendOf(turns(cv.Turn{ID: "a", Usage: usage(10, 5, 0)}))
	if s.Priced || s.CostUSD != 0 || s.Total != 15 {
		t.Fatalf("unpriced: %+v", s)
	}
	// Answered side questions count, marked as asides; a failed or
	// running one does not.
	c := turns(cv.Turn{ID: "a", Usage: usage(1000, 100, 0.02)})
	c.Conversation.Entries = []cv.Entry{
		{Role: "aside", Aside: &cv.Aside{Status: "completed", Input: 200, Output: 20, CostUSD: 0.005}},
		{Role: "aside", Aside: &cv.Aside{Status: "failed", Input: 50, Output: 5, CostUSD: 0.001}},
		{Role: "aside", Aside: &cv.Aside{Status: "running"}},
		{Role: "user", Text: "hi"},
	}
	s = spendOf(c)
	if s.Turns != 1 || s.Asides != 1 || s.Input != 1200 || s.Output != 120 || s.Total != 1320 || !near(s.CostUSD, 0.025) || !near(s.AsideCostUSD, 0.005) || !s.Priced {
		t.Fatalf("with asides: %+v", s)
	}
	// A chat of side questions alone is priced by them.
	c = &Chat{}
	c.Conversation.Entries = []cv.Entry{{Role: "aside", Aside: &cv.Aside{Status: "completed", Input: 10, Output: 1, CostUSD: 0.01}}}
	if s = spendOf(c); s.Turns != 0 || s.Asides != 1 || !s.Priced || !near(s.CostUSD, 0.01) {
		t.Fatalf("asides alone: %+v", s)
	}
}

// The state carries every chat's spend, and GET spend the totals of
// today (since the local midnight), the last seven days and all time by
// provider over every chat, archived included; a turn counts at its end
// (its start while it runs).
func TestSpendOnTheStateAndTheReport(t *testing.T) {
	now := time.Date(2026, 9, 18, 15, 0, 0, 0, time.Local)
	e, _, _ := setup(t, func(e *Engine) { e.Now = func() time.Time { return now } })
	at := func(d time.Duration) float64 { return float64(now.Add(d).UnixMilli()) / 1000 }
	claude, _ := e.Create("Claude", "", "", nil, "claude", "")
	codex, _ := e.Create("Codex", "", "", nil, "codex", "")
	old, _ := e.Create("Old", "", "", nil, "claude", "")
	_ = e.Store.update(func(st *State) error {
		st.chat(claude).Conversation.Turns = []cv.Turn{
			{ID: "t1", StartedAt: at(-2 * time.Hour), EndedAt: at(-time.Hour), Usage: usage(1000, 100, 0.10)},       // today
			{ID: "t2", StartedAt: at(-30 * time.Hour), EndedAt: at(-29 * time.Hour), Usage: usage(2000, 200, 0.20)}, // yesterday
			{ID: "t3", StartedAt: at(-10 * time.Minute), Usage: usage(10, 1, 0.01)},                                 // running: counts at its start
		}
		st.chat(codex).Conversation.Turns = []cv.Turn{{ID: "c1", StartedAt: at(-3 * 24 * time.Hour), EndedAt: at(-3 * 24 * time.Hour), Usage: usage(300, 30, 0)}}
		st.chat(old).Conversation.Turns = []cv.Turn{{ID: "o1", StartedAt: at(-20 * 24 * time.Hour), EndedAt: at(-20 * 24 * time.Hour), Usage: usage(5000, 500, 1)}}
		st.chat(old).Archived = true
		// Two side questions on the Claude chat: one answered today, one
		// eight days ago (all time only); a failed one never counts.
		st.chat(old).Conversation.Entries = []cv.Entry{{ID: "x", Role: "aside", EndedAt: at(-8 * 24 * time.Hour), Aside: &cv.Aside{Status: "completed", Input: 400, Output: 40, CostUSD: 0.04}}}
		st.chat(claude).Conversation.Entries = append(st.chat(claude).Conversation.Entries,
			cv.Entry{ID: "a1", Role: "aside", EndedAt: at(-time.Minute), Aside: &cv.Aside{Status: "completed", Input: 100, Output: 10, CostUSD: 0.02}},
			cv.Entry{ID: "a2", Role: "aside", EndedAt: at(-time.Minute), Aside: &cv.Aside{Status: "failed", Error: "no", Input: 100, Output: 10, CostUSD: 0.02}})
		return nil
	})
	view := e.View()
	var got *Spend
	for _, c := range view.Chats {
		if c.ID == claude {
			got = c.Spend
		}
	}
	if got == nil || got.Turns != 3 || got.Asides != 1 || got.Total != 3421 || !near(got.CostUSD, 0.33) || !near(got.AsideCostUSD, 0.02) || !got.Priced {
		t.Fatalf("state spend: %+v", got)
	}
	if stored := e.Store.Snapshot().chat(claude); stored.Spend != nil {
		t.Fatal("spend was stored")
	}
	report := e.Spend()
	if report.Today.Turns != 2 || report.Today.Asides != 1 || !near(report.Today.CostUSD, 0.13) || !near(report.Today.AsideCostUSD, 0.02) || report.Today.Chats != 1 || report.Today.Providers["claude"].Turns != 2 || report.Today.Providers["claude"].Asides != 1 || report.Today.Providers["codex"].Turns != 0 {
		t.Fatalf("today: %+v", report.Today)
	}
	if report.Week.Turns != 4 || report.Week.Asides != 1 || !near(report.Week.CostUSD, 0.33) || report.Week.Total != 3751 || report.Week.Chats != 2 || report.Week.Providers["codex"].Total != 330 || report.Week.Providers["codex"].Priced {
		t.Fatalf("week: %+v", report.Week)
	}
	if report.All.Turns != 5 || report.All.Asides != 2 || !near(report.All.CostUSD, 1.37) || !near(report.All.AsideCostUSD, 0.06) || report.All.Chats != 3 || !near(report.All.Providers["claude"].CostUSD, 1.37) || report.At != at(0) {
		t.Fatalf("all: %+v", report.All)
	}
	h := &HTTP{Engine: e, Token: "private", Host: "127.0.0.1:18780", Origin: "http://127.0.0.1:18780", WebDir: t.TempDir()}
	r := httptest.NewRequest("GET", h.Origin+"/api/spend", nil)
	r.Header.Set("Authorization", "Bearer private")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	var v SpendReport
	if err := json.Unmarshal(out.Body.Bytes(), &v); err != nil || out.Code != 200 || v.All.Turns != 5 || v.All.Asides != 2 || !near(v.Today.Providers["claude"].CostUSD, 0.13) {
		t.Fatalf("GET spend: %d %s %v", out.Code, out.Body.String(), err)
	}
}
