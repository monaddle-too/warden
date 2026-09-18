package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The long tail (docs/claude-parity.md, item 15): /fork, /btw, /cost,
// /style, /bell, the terminal title and the bell on the agent's events.
// The web's cost.ts, notify.ts and ForkDialog do the same there.

// CostSummary is what a chat's turns took so far, summed (the web's
// sessionCost).
type CostSummary struct {
	Turns, Reported int
	Usage           Usage
	Priced          bool
	Seconds         float64
}

// SessionCost sums the chat's turn records; nowSeconds counts a running
// turn up to now when positive.
func SessionCost(c *Chat, nowSeconds float64) CostSummary {
	var out CostSummary
	for _, t := range c.Conversation.Turns {
		out.Turns++
		if u := t.Usage; u != nil {
			out.Reported++
			out.Usage.Input += u.Input
			out.Usage.Cached += u.Cached
			out.Usage.CacheWrite += u.CacheWrite
			out.Usage.Output += u.Output
			out.Usage.Reasoning += u.Reasoning
			out.Usage.Total += u.Total
			if u.CostUSD > 0 {
				out.Priced = true
				out.Usage.CostUSD += u.CostUSD
			}
		}
		if t.StartedAt > 0 {
			if t.EndedAt > 0 {
				out.Seconds += max(0, t.EndedAt-t.StartedAt)
			} else if nowSeconds > 0 {
				out.Seconds += max(0, nowSeconds-t.StartedAt)
			}
		}
	}
	return out
}

// CostLines lays the summary out as the /cost notice.
func CostLines(s CostSummary) []string {
	turns := strconv.Itoa(s.Turns)
	if s.Reported < s.Turns {
		turns += fmt.Sprintf(" (%d with usage)", s.Reported)
	}
	rows := [][2]string{{"turns", turns}, {"input tokens", FormatTokens(s.Usage.Input)}}
	if s.Usage.Cached > 0 {
		rows = append(rows, [2]string{"  read from cache", FormatTokens(s.Usage.Cached)})
	}
	if s.Usage.CacheWrite > 0 {
		rows = append(rows, [2]string{"  written to cache", FormatTokens(s.Usage.CacheWrite)})
	}
	rows = append(rows, [2]string{"output tokens", FormatTokens(s.Usage.Output)})
	if s.Usage.Reasoning > 0 {
		rows = append(rows, [2]string{"  thinking", FormatTokens(s.Usage.Reasoning)})
	}
	rows = append(rows, [2]string{"total tokens", FormatTokens(s.Usage.Total)})
	cost := "not reported by this agent"
	if s.Priced {
		cost = FormatCost(s.Usage.CostUSD)
	}
	rows = append(rows, [2]string{"cost", cost}, [2]string{"time in turns", FormatDuration(s.Seconds)})
	out := []string{"cost so far (this chat; not sent to the agent)"}
	for _, r := range rows {
		out = append(out, fmt.Sprintf("  %-20s %s", r[0], r[1]))
	}
	return out
}

// WorkspaceCost sums the turns of every chat on the workspace of c
// (archived ones included) and says how many chats; nowSeconds as for
// SessionCost.
func WorkspaceCost(c *Chat, chats []*Chat, nowSeconds float64) (CostSummary, int) {
	var out CostSummary
	n := 0
	for _, other := range chats {
		if other.SandboxID != c.SandboxID {
			continue
		}
		n++
		s := SessionCost(other, nowSeconds)
		out.Turns += s.Turns
		out.Reported += s.Reported
		out.Usage.Input += s.Usage.Input
		out.Usage.Cached += s.Usage.Cached
		out.Usage.CacheWrite += s.Usage.CacheWrite
		out.Usage.Output += s.Usage.Output
		out.Usage.Reasoning += s.Usage.Reasoning
		out.Usage.Total += s.Usage.Total
		out.Usage.CostUSD += s.Usage.CostUSD
		out.Priced = out.Priced || s.Priced
		out.Seconds += s.Seconds
	}
	return out, n
}

// WorkspaceCostLine is the /cost notice's last line: the workspace's
// total over its chats (docs/claude-parity.md, R2.2).
func WorkspaceCostLine(s CostSummary, chats int) string {
	parts := []string{fmt.Sprintf("%d chat%s", chats, plural(chats)), fmt.Sprintf("%d turn%s", s.Turns, plural(s.Turns)), FormatTokens(s.Usage.Total) + " tokens"}
	if s.Priced {
		parts = append(parts, FormatCost(s.Usage.CostUSD))
	}
	return "this workspace: " + strings.Join(parts, " · ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// cost shows the chat's totals as a notice, with the workspace's under
// them.
func (a *App) cost(c *Chat) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	now := 0.0
	if c.Running() {
		now = float64(a.now().UnixMilli()) / 1000
	}
	lines := CostLines(SessionCost(c, now))
	var all []*Chat
	if a.state != nil {
		all = a.state.Chats
	}
	if total, n := WorkspaceCost(c, all, now); n > 1 {
		lines = append(lines, WorkspaceCostLine(total, n))
	}
	a.setNotice(strings.Join(lines, "\n"))
}

// fork lists the messages (no argument) or forks the chat: before
// message N, or the whole of it ("all"), then switches to the fork.
func (a *App) fork(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	targets := rewindTargets(c)
	if arg == "" {
		var b strings.Builder
		for i, e := range targets {
			fmt.Fprintf(&b, "%2d   %s\n", i+1, truncate(excerptOf(e.Text), 70))
		}
		b.WriteString("/fork N copies the chat up to before message N into a sibling chat; /fork all copies the whole of it")
		a.setNotice(b.String())
		return
	}
	target := ""
	label := "the whole conversation"
	if arg != "all" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(targets) {
			a.setNotice("/fork N (from /fork) or /fork all")
			return
		}
		target = targets[n-1].ID
		label = fmt.Sprintf("before message %d %q", n, truncate(excerptOf(targets[n-1].Text), 50))
	}
	if c.Running() {
		a.setNotice("stop the agent first (Esc)")
		return
	}
	result, err := a.Client.Fork(ctx, c.ID, target)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	a.refreshState(ctx)
	a.selectChat(result.ID)
	a.setNotice(fmt.Sprintf("forked %s into %q; %s", label, sanitize(result.Title), forkOutcome(result.Session)))
}

// forkOutcome says how the fork's agent session follows the source's.
func forkOutcome(session string) string {
	switch session {
	case "forked":
		return "the agent continues from a copy of its session"
	case "fresh":
		return "the agent's session cannot be copied; the next message starts a new one with the conversation so far as context"
	}
	return "the fork starts its own agent session"
}

// btw asks a side question in the background and reports the answer as
// a notice when it arrives; the aside entry itself comes over the stream.
func (a *App) btw(ctx context.Context, c *Chat, question string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	if question == "" {
		a.setNotice("/btw QUESTION asks a copy of the agent's session, from this chat's context; the agent never sees it")
		return
	}
	if c.Provider != "claude" {
		a.setNotice("side questions are a Claude chat's")
		return
	}
	if c.Running() {
		a.setNotice("wait for the agent's turn to finish before a side question")
		return
	}
	a.setNotice("asking a copy of the session: " + truncate(question, 60))
	go func() {
		result, err := a.Client.Aside(ctx, c.ID, question)
		report := func(context.Context) {
			switch {
			case err != nil:
				a.setNotice(err.Error())
			case result.Error != "":
				a.setNotice("side question failed: " + result.Error)
			default:
				a.setNotice("side question answered" + costSuffix(result.CostUSD))
			}
		}
		select {
		case a.later <- report:
		case <-ctx.Done():
		}
	}()
}

func costSuffix(usd float64) string {
	if usd <= 0 {
		return ""
	}
	return " (" + FormatCost(usd) + ")"
}

// outputStyles are the styles /style accepts, as the service names them.
var outputStyles = []string{"default", "Explanatory", "Learning"}

// style shows or sets a Claude chat's output style for its next session.
func (a *App) style(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	current := c.OutputStyle
	if current == "" {
		current = "default"
	}
	if arg == "" {
		running := ""
		if c.Session != nil && c.Session.OutputStyle != "" {
			running = "; the running session uses " + sanitize(c.Session.OutputStyle)
		}
		a.setNotice("output style " + current + " for the next session" + running + " · /style default|Explanatory|Learning")
		return
	}
	want := ""
	for _, s := range outputStyles {
		if strings.EqualFold(s, arg) {
			want = s
		}
	}
	if want == "" {
		a.setNotice("/style default|Explanatory|Learning")
		return
	}
	if want == "default" {
		want = ""
	}
	if err := a.Client.Style(ctx, c.ID, want); err != nil {
		a.setNotice(err.Error())
		return
	}
	a.refreshState(ctx)
	shown := want
	if shown == "" {
		shown = "default"
	}
	a.setNotice("output style " + shown + " applies when the next session starts (the next message starts one)")
}

// The bell: rung on the agent's events (a turn ending, an approval, a
// failure) unless turned off; the choice is kept in BellFile.

// bellOn reports whether the bell rings (on until turned off).
func (a *App) bellOn() bool {
	if a.bell == nil {
		on := true
		if a.BellFile != "" {
			if b, err := os.ReadFile(a.BellFile); err == nil && strings.TrimSpace(string(b)) == "off" {
				on = false
			}
		}
		a.bell = &on
	}
	return *a.bell
}

// setBell turns the bell on or off and remembers it.
func (a *App) setBell(on bool) {
	a.bell = &on
	if a.BellFile == "" {
		return
	}
	value := "on"
	if !on {
		value = "off"
	}
	if err := os.MkdirAll(filepath.Dir(a.BellFile), 0700); err == nil {
		_ = os.WriteFile(a.BellFile, []byte(value+"\n"), 0600)
	}
}

// bellCommand: /bell shows the setting, /bell on|off changes it.
func (a *App) bellCommand(arg string) {
	switch strings.ToLower(arg) {
	case "":
		state := "on"
		if !a.bellOn() {
			state = "off"
		}
		a.setNotice("bell " + state + ": rings when the agent finishes, asks or fails · /bell on|off")
	case "on", "off":
		a.setBell(arg == "on")
		a.setNotice("bell " + strings.ToLower(arg))
	default:
		a.setNotice("/bell on|off")
	}
}

// BellEvents says which of the agent's events happened between two
// snapshots of the selected chat: "completed" (a turn ended), "failed"
// (a turn failed), "approval" (something new to answer). Nothing for a
// stop the person asked for, or a chat seen for the first time.
func BellEvents(prev, next *Chat) []string {
	if prev == nil || next == nil || prev.ID != next.ID {
		return nil
	}
	var out []string
	if prev.Running() && !next.Running() {
		switch {
		case next.Status == "failed" || (next.Error != "" && next.Error != prev.Error):
			out = append(out, "failed")
		case next.Status == "idle":
			out = append(out, "completed")
		}
	}
	seen := map[string]bool{}
	for _, ap := range prev.Pending() {
		seen[ap.ID] = true
	}
	for _, ap := range next.Pending() {
		if !seen[ap.ID] {
			out = append(out, "approval")
		}
	}
	return out
}

// bellForEvents rings once for the events between the previous snapshot
// of the selected chat and this one, and remembers this one.
func (a *App) bellForEvents() {
	c := a.chat()
	prev := a.lastSeen
	if c != nil {
		copy := *c
		a.lastSeen = &copy
	} else {
		a.lastSeen = nil
	}
	if !a.bellOn() {
		return
	}
	if len(BellEvents(prev, c)) > 0 {
		fmt.Fprint(a.Output, "\a")
	}
}

// TitleFor is the terminal title: the app, the chat, and what the agent
// is up to (running, idle, or waiting for an answer).
func TitleFor(c *Chat) string {
	if c == nil {
		return "Warden"
	}
	state := "idle"
	switch {
	case len(c.Pending()) > 0:
		state = "approval"
	case c.Running():
		state = "running"
	}
	return "Warden · " + strings.Join(strings.Fields(sanitize(c.Title)), " ") + " · " + state
}

// setTitle writes the terminal title (OSC 0) when it changed.
func (a *App) setTitle() {
	title := TitleFor(a.chat())
	if title == a.title {
		return
	}
	a.title = title
	fmt.Fprint(a.Output, "\x1b]0;"+title+"\a")
}
