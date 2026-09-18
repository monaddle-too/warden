package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// currentTurn picks the turn the status line describes: the running one
// while a turn is in progress, else the last turn that has a record.
// start is when the owner sent the message that began it (the service's
// record when that message is gone), 0 when unknown.
func currentTurn(c *Chat) (t *Turn, start float64) {
	id := ""
	if c.Conversation.ActiveTurnID != nil {
		id = *c.Conversation.ActiveTurnID
	}
	if id == "" {
		for i := len(c.Conversation.Entries) - 1; i >= 0; i-- {
			if e := c.Conversation.Entries[i]; e.TurnID != nil {
				id = *e.TurnID
				break
			}
		}
	}
	if id == "" && len(c.Conversation.Turns) > 0 {
		id = c.Conversation.Turns[len(c.Conversation.Turns)-1].ID
	}
	if id == "" {
		return nil, 0
	}
	t = c.Turn(id)
	for _, e := range c.Conversation.Entries {
		if e.Role == "user" && e.TurnID != nil && *e.TurnID == id {
			start = e.CreatedAt
			break
		}
	}
	if start == 0 && t != nil {
		start = t.StartedAt
	}
	return t, start
}

// runStart is when the current run began: the turn's start, else the last
// user message (a chat from before turn records were kept).
func runStart(c *Chat) float64 {
	if _, start := currentTurn(c); start > 0 {
		return start
	}
	start := 0.0
	for _, e := range c.Conversation.Entries {
		if e.Role == "user" && e.CreatedAt > start {
			start = e.CreatedAt
		}
	}
	return start
}

// RunIndicator is the spinner and elapsed time of the current run; empty
// when nothing runs.
func RunIndicator(c *Chat, now time.Time) string {
	if !c.Running() {
		return ""
	}
	frame := spinnerFrames[(now.UnixNano()/int64(120*time.Millisecond))%int64(len(spinnerFrames))]
	start := runStart(c)
	if start == 0 {
		return yellow + frame + reset
	}
	elapsed := max(0, float64(now.UnixMilli())/1000-start)
	return fmt.Sprintf("%s%s %s%s", yellow, frame, formatElapsed(elapsed), reset)
}

// formatElapsed is a running clock: whole seconds under a minute, then
// minutes and hours as FormatDuration.
func formatElapsed(seconds float64) string {
	whole := math.Floor(seconds)
	if whole < 60 {
		return fmt.Sprintf("%ds", int(whole))
	}
	return FormatDuration(whole)
}

// TurnStats is what the current turn took so far, or the last one took:
// duration (finished turns), tokens and cost when the provider reported
// them (TurnStats.tsx shows the same under the message).
func TurnStats(c *Chat) string {
	t, start := currentTurn(c)
	if t == nil {
		return ""
	}
	var parts []string
	if !c.Running() && t.EndedAt > 0 && start > 0 {
		parts = append(parts, FormatDuration(t.EndedAt-start))
	}
	if t.Usage != nil {
		// A /compact turn reports no tokens, only the compaction's cost.
		if t.Usage.Total > 0 {
			parts = append(parts, UsageSummary(*t.Usage))
		}
		if t.Usage.CostUSD != 0 {
			parts = append(parts, FormatCost(t.Usage.CostUSD))
		}
	}
	return strings.Join(parts, " · ")
}

// ContextIndicator is how full the agent's context is, "ctx 43k/200k
// (21%)": plain until 80 % of the way to where the agent compacts on its
// own (its threshold when it reports one, else the window), yellow from
// there, red from 95 % (a compaction is imminent). Empty when the agent
// has reported none.
func ContextIndicator(c *Chat) string {
	ctx := c.Conversation.Context
	if ctx == nil || ctx.Used == 0 {
		return ""
	}
	if ctx.Window <= 0 {
		return "ctx " + FormatTokens(ctx.Used)
	}
	limit := ctx.Window
	if ctx.Threshold > 0 && ctx.Threshold < limit {
		limit = ctx.Threshold
	}
	fraction := float64(ctx.Used) / float64(limit)
	text := fmt.Sprintf("ctx %s/%s (%d%%)", FormatTokens(ctx.Used), FormatTokens(ctx.Window), int(math.Round(math.Min(1, float64(ctx.Used)/float64(ctx.Window))*100)))
	switch {
	case fraction >= 0.95:
		return red + text + reset
	case fraction >= 0.8:
		return yellow + text + reset
	}
	return text
}

// StatusLine is the status bar as one line, the parts two spaces apart;
// the screen lays the same parts out over several rows (LayoutStatus) so
// none of them is lost on a narrow terminal.
func StatusLine(c *Chat, ports []Port, live bool, now time.Time) string {
	return strings.Join(StatusParts(c, ports, live, now), statusGap)
}

// statusGap separates the parts of the status bar.
const statusGap = "  "

// StatusMaxRows is the most rows the status bar takes; whatever still does
// not fit on the last row is cut at the edge.
const StatusMaxRows = 4

// StatusParts are the pieces of the status bar in order of importance,
// which is what a narrow screen keeps first: the connection and the
// chat's title, provider and model, what the agent is doing (with the
// startup stage or the run's elapsed time) — always these three — then
// pending approvals, the chat's error, the turn's tokens and cost, the
// context and previews. Each part is one styled string that is kept whole.
func StatusParts(c *Chat, ports []Port, live bool, now time.Time) []string {
	link := green + "●" + reset
	if !live {
		link = red + "○" + reset
	}
	status := c.Status
	switch c.Status {
	case "running", "stopping", "queued":
		status = yellow + c.Status + reset
	case "failed", "interrupted":
		status = red + c.Status + reset
	}
	if c.Startup != nil && (c.Status == "running" || c.Status == "queued") {
		// The startup stage, with the runtime's detail, until the turn runs.
		stage := c.Startup.Stage
		if c.Startup.Detail != "" {
			stage += ": " + sanitize(c.Startup.Detail)
		}
		status = yellow + stage + reset
	}
	if ind := RunIndicator(c, now); ind != "" {
		status = ind + " " + status
	}
	model := c.Model
	if model == "" {
		model = "default"
	}
	if c.Session != nil && c.Session.Model != "" && c.Session.Model != model {
		// The model the session resolved is the truth (a live switch is
		// reported at the next turn): "opus (claude-opus-5)".
		model += " (" + sanitize(c.Session.Model) + ")"
	}
	agent := c.Provider + " · " + model
	if c.Provider == "claude" {
		// The permission mode (Shift+Tab cycles it) beside the model, and
		// the session settings when they are not the defaults.
		agent += " · " + orMode(c.Mode)
		if c.Thinking != "" {
			agent += " · thinking " + strings.TrimSuffix(thinkingLabel(c.Thinking), " tokens")
		}
		if c.Effort != "" {
			agent += " · effort " + c.Effort
		}
		if c.Fast {
			agent += " · fast"
		}
	}
	parts := []string{fmt.Sprintf("%s %s%s%s", link, bold, sanitize(c.Title), reset), agent, status}
	if waiting := WaitingLabel(c); waiting != "" {
		parts = append(parts, yellow+"⚠ "+waiting+reset)
	}
	if c.Error != "" {
		parts = append(parts, red+sanitize(c.Error)+reset)
	}
	if stats := TurnStats(c); stats != "" {
		parts = append(parts, dim+stats+reset)
	}
	if ind := ContextIndicator(c); ind != "" {
		parts = append(parts, ind)
	}
	published := 0
	for _, p := range ports {
		if p.ChatID == c.ID && p.State == "approved" {
			published++
		}
	}
	if published > 0 {
		parts = append(parts, fmt.Sprintf("previews:%d", published))
	}
	return parts
}

// WaitingLabel counts what waits for the person: the pending approvals
// and the reviews open in the app ("1 approval · 2 reviews"); "" when
// nothing does.
func WaitingLabel(c *Chat) string {
	var parts []string
	if n := len(c.Pending()); n > 0 {
		word := "approvals"
		if n == 1 {
			word = "approval"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, word))
	}
	if n := len(c.Reviews); n > 0 {
		word := "reviews"
		if n == 1 {
			word = "review"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, word))
	}
	return strings.Join(parts, " · ")
}

// LayoutStatus packs the status parts into rows no wider than width,
// filling each row before starting the next and never splitting a part;
// a part wider than the row stands alone and is cut by the screen. At
// most maxRows rows: the parts that do not fit by then are dropped (they
// are the least important, and a terminal that narrow shows the rest).
func LayoutStatus(parts []string, width, maxRows int) []string {
	if width < 8 {
		width = 8
	}
	if maxRows < 1 {
		maxRows = 1
	}
	var rows []string
	row, used := "", 0
	for _, part := range parts {
		w := visibleWidth(part)
		switch {
		case row == "":
			row, used = part, w
		case used+len(statusGap)+w <= width:
			row += statusGap + part
			used += len(statusGap) + w
		default:
			rows = append(rows, row)
			if len(rows) == maxRows {
				return rows
			}
			row, used = part, w
		}
	}
	if row != "" || len(rows) == 0 {
		rows = append(rows, row)
	}
	return rows
}

// visibleWidth counts the runes of s that reach the screen: the styling
// (escape sequences) takes no columns.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(plainText(s))
}
