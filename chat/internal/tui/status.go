package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
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

// StatusLine is the one-line status bar: the connection, the chat's title,
// provider and model, what the agent is doing (with the startup stage or
// the run's elapsed time), pending approvals, the turn's tokens and cost,
// the context, previews and the chat's error.
func StatusLine(c *Chat, ports []Port, live bool, now time.Time) string {
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
	agent := c.Provider + " · " + model
	if c.Provider == "claude" {
		// The permission mode (Shift+Tab cycles it) beside the model.
		agent += " · " + orMode(c.Mode)
	}
	parts := []string{fmt.Sprintf("%s %s%s%s", link, bold, sanitize(c.Title), reset), agent, status}
	if n := len(c.Pending()); n > 0 {
		word := "approvals"
		if n == 1 {
			word = "approval"
		}
		parts = append(parts, fmt.Sprintf("%s⚠ %d %s%s", yellow, n, word, reset))
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
	if c.Error != "" {
		parts = append(parts, red+sanitize(c.Error)+reset)
	}
	return strings.Join(parts, "  ")
}
