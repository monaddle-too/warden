package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"warden/chat/internal/chats"
	"warden/chat/internal/config"
	"warden/chat/internal/tui"
)

// The menu bar item (chat/menu/main.swift, `warden-menu`) renders and
// runs `warden`; it decides nothing itself. `warden menu feed` is its
// model: one JSON line per change, following the chat service's event
// stream while the endpoint answers and the service manager while it
// does not (docs/menu-bar-plan.md).

// menuState is one line of the feed.
type menuState struct {
	// Service is running (the event stream is connected), starting (the
	// manager or a detached launcher runs but the chat service does not
	// answer yet) or stopped.
	Service    string `json:"service"`
	Registered bool   `json:"registered"` // the service is registered with the manager
	State      string `json:"state"`      // the state directory (Show Logs)
	// Attention is what waits on the owner, oldest chat first: pending
	// approvals, reviews only the app can settle, a recent failure.
	Attention []menuAttention `json:"attention"`
	// Chats are the non-archived chats, those with a running turn first,
	// then by last activity, at most menuChatRows; More counts the rest.
	Chats []menuChat `json:"chats"`
	More  int        `json:"more"`
	// Working counts the chats with a running turn.
	Working int        `json:"working"`
	Spend   *menuSpend `json:"spend,omitempty"`
}

type menuAttention struct {
	ChatID string `json:"chatID"`
	Chat   string `json:"chat"`
	Kind   string `json:"kind"` // approval, review, error
	Label  string `json:"label"`
}

type menuChat struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Activity is what the agent is doing while the turn runs (the
	// startup stage until it does), "" otherwise.
	Activity string `json:"activity,omitempty"`
	// LastActive is when the chat's last turn ended (or started), unix
	// seconds; 0 when it never ran.
	LastActive float64 `json:"lastActive"`
}

type menuSpend struct {
	TodayUSD float64 `json:"todayUSD"`
	Turns    int     `json:"turns"`
	Priced   bool    `json:"priced"`
}

// menuChatRows is the most chats the menu lists.
const menuChatRows = 8

// menuErrorWindow is how long a failed chat counts as needing attention.
const menuErrorWindow = time.Hour

// menuModel derives the menu's model from a state snapshot.
func menuModel(s *tui.State, spend *chats.SpendReport, now time.Time) menuState {
	m := menuState{Service: "running", Attention: []menuAttention{}, Chats: []menuChat{}}
	var rows []menuChat
	for _, c := range s.Chats {
		if c.Archived {
			continue
		}
		row := menuChat{ID: c.ID, Title: c.Title, Status: c.Status, LastActive: lastActive(c)}
		if c.Running() {
			m.Working++
			switch {
			case c.Startup != nil:
				row.Activity = c.Startup.Stage
				if c.Startup.Detail != "" {
					row.Activity += ": " + c.Startup.Detail
				}
			default:
				row.Activity = tui.RunningLabel(c.Conversation.Entries)
			}
		}
		rows = append(rows, row)
		for _, a := range c.Pending() {
			m.Attention = append(m.Attention, menuAttention{ChatID: c.ID, Chat: c.Title, Kind: "approval", Label: a.Summary()})
		}
		for _, r := range c.Reviews {
			m.Attention = append(m.Attention, menuAttention{ChatID: c.ID, Chat: c.Title, Kind: "review", Label: r.Summary(c.Provider)})
		}
		if c.Status == "failed" && c.Error != "" && row.LastActive > 0 && now.Sub(time.Unix(int64(row.LastActive), 0)) < menuErrorWindow {
			m.Attention = append(m.Attention, menuAttention{ChatID: c.ID, Chat: c.Title, Kind: "error", Label: c.Error})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rows[i].Activity != "", rows[j].Activity != ""
		if ri != rj {
			return ri
		}
		return rows[i].LastActive > rows[j].LastActive
	})
	if len(rows) > menuChatRows {
		m.More = len(rows) - menuChatRows
		rows = rows[:menuChatRows]
	}
	m.Chats = append(m.Chats, rows...)
	if spend != nil {
		m.Spend = &menuSpend{TodayUSD: spend.Today.CostUSD, Turns: spend.Today.Turns, Priced: spend.Today.Priced}
	}
	return m
}

// lastActive is when the chat's newest turn ended, or started while it
// runs.
func lastActive(c *tui.Chat) float64 {
	var at float64
	for _, t := range c.Conversation.Turns {
		at = max(at, t.EndedAt, t.StartedAt)
	}
	return at
}

// menuFeed is `warden menu feed`: the model on stdout, one line per
// change, until stdin closes or a signal arrives.
func (c *cli) menuCommand(args []string) error {
	if len(args) == 0 || args[0] != "feed" {
		fmt.Fprintln(c.stderr, "usage: warden menu feed [--config PATH] [--state DIR]")
		return errUsage
	}
	fs, configPath, state := serviceFlags("warden menu feed", c)
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		// The menu bar item holds the pipe; it closing is the end.
		io.Copy(io.Discard, c.stdin)
		cancel()
	}()
	f := &menuFeeder{cfg: cfg, service: func() serviceManager { return c.registeredService(cfg) }, out: c.stdout, now: time.Now, sleep: menuSleep}
	f.run(ctx)
	return nil
}

// menuFeeder follows the service and writes the feed. A test drives it
// with a fake chat service, a fake manager and its own clock.
type menuFeeder struct {
	cfg config.Config
	// service is the registered manager, nil when none is (asked each
	// time: `warden service install` may run while the feed does).
	service func() serviceManager
	out     io.Writer
	now     func() time.Time
	sleep   func(context.Context, time.Duration)
	last    string // the last line written
	spend   *chats.SpendReport
	// spendAt is when the spend was fetched; running is how many chats
	// had a running turn at the last snapshot (a drop refetches the spend).
	spendAt time.Time
	running int
}

// menuRetry is the pause between attempts while the service is unreachable.
const menuRetry = 2 * time.Second

// menuSpendAge is how old the spend may be before a snapshot refetches it.
const menuSpendAge = 30 * time.Second

func menuSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (f *menuFeeder) run(ctx context.Context) {
	for ctx.Err() == nil {
		base, token, err := endpoint(f.cfg.OwnerTokenFile())
		if err != nil {
			f.offline(ctx)
			continue
		}
		client := &tui.Client{Base: base, Token: token}
		client.Stream(ctx, func(s *tui.State) {
			f.emit(menuModel(s, f.refreshSpend(ctx, client, s), f.now()))
		})
		if ctx.Err() != nil {
			return
		}
		f.offline(ctx)
	}
}

// offline emits the state while the chat service does not answer and
// waits before the next attempt.
func (f *menuFeeder) offline(ctx context.Context) {
	m := menuState{Service: "stopped", Attention: []menuAttention{}, Chats: []menuChat{}}
	if svc := f.service(); svc != nil && svc.status().Running {
		m.Service = "starting"
	}
	if _, alive := runningPID(f.cfg); alive {
		m.Service = "starting"
	}
	f.emit(m)
	f.sleep(ctx, menuRetry)
}

// refreshSpend fetches the spend when it is stale or a turn just ended.
func (f *menuFeeder) refreshSpend(ctx context.Context, client *tui.Client, s *tui.State) *chats.SpendReport {
	running := 0
	for _, c := range s.Chats {
		if c.Running() {
			running++
		}
	}
	ended := running < f.running
	f.running = running
	if f.spend != nil && !ended && f.now().Sub(f.spendAt) < menuSpendAge {
		return f.spend
	}
	fetch, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report, err := client.Spend(fetch)
	if err != nil {
		return f.spend
	}
	f.spend, f.spendAt = &report, f.now()
	return f.spend
}

// emit writes the model when it differs from the last line written.
func (f *menuFeeder) emit(m menuState) {
	m.State = f.cfg.Paths.State
	m.Registered = f.service() != nil
	line, err := json.Marshal(m)
	if err != nil || string(line) == f.last {
		return
	}
	f.last = string(line)
	fmt.Fprintln(f.out, f.last)
}
