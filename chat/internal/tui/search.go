package tui

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// /search TEXT looks through the titles and transcripts of every chat of
// the service (GET chats/search, chats/search.go: archived chats, a
// subagent's nested entries and the person's own commands included) and
// lists the hits, numbered, with the chat's title, where the match is
// and when; /search N, or Enter on a listed hit, opens that chat and
// prints the entry (the transcript is in the terminal's scrollback),
// expanding the transcript when the entry sits inside a subagent's card
// or past a fold.

// SearchHit mirrors chats.SearchHit: one match of a search across chats.
type SearchHit struct {
	ChatID    string  `json:"chatID"`
	Title     string  `json:"title"`
	Archived  bool    `json:"archived"`
	Provider  string  `json:"provider"`
	EntryID   string  `json:"entryID"`
	Field     string  `json:"field"`
	Role      string  `json:"role"`
	ParentID  string  `json:"parentID"`
	CreatedAt float64 `json:"createdAt"`
	Sender    *struct {
		PrincipalID string `json:"principalID"`
		Email       string `json:"email"`
		Name        string `json:"name"`
	} `json:"sender"`
	Snippet struct {
		Before string `json:"before"`
		Match  string `json:"match"`
		After  string `json:"after"`
	} `json:"snippet"`
}

// SearchResult mirrors chats.SearchResult.
type SearchResult struct {
	Hits []SearchHit `json:"hits"`
	More int         `json:"more"`
}

// Search asks the service for the chats and entries matching query.
func (c *Client) Search(ctx context.Context, query string, limit int) (SearchResult, error) {
	var res SearchResult
	path := "chats/search?q=" + url.QueryEscape(query)
	if limit > 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}
	err := c.do(ctx, "GET", path, nil, &res)
	return res, err
}

// hitWhere says where a hit's match is, for its row: the chat's title,
// a message by whom, a step's output, a subagent's entry.
func hitWhere(h SearchHit) string {
	switch {
	case h.Field == "title":
		return "title"
	case h.Role == "activity" && h.Sender != nil:
		return "command by " + senderLabel(Entry{Sender: h.Sender})
	case h.Role == "activity" && h.Field == "detail":
		return "tool output"
	case h.Role == "activity":
		return "agent step"
	case h.Role == "user":
		return senderLabel(Entry{Sender: h.Sender})
	case h.Role == "thinking":
		return "thinking"
	case h.Role == "aside":
		return "side question"
	case h.Role == "assistant" && h.Provider != "":
		return h.Provider
	}
	return h.Role
}

// searchCommand runs /search: a number jumps to that hit of the last
// search, anything else searches and lists.
func (a *App) searchCommand(ctx context.Context, arg string) {
	if arg == "" {
		a.setNotice("/search TEXT looks through every chat · /search N (or Enter on a hit) opens hit N")
		return
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(a.searchHits) {
			a.setNotice(fmt.Sprintf("/search N with N from the last search (%d hits)", len(a.searchHits)))
			return
		}
		a.jumpToHit(a.searchHits[n-1])
		return
	}
	res, err := a.Client.Search(ctx, arg, 40)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	a.searchHits, a.searchQuery = res.Hits, arg
	if len(res.Hits) == 0 {
		a.setNotice(fmt.Sprintf("no chat mentions %q", arg))
		return
	}
	m := &Menu{Trigger: Trigger{Kind: "hit"}}
	for i, h := range res.Hits {
		n := strconv.Itoa(i + 1)
		title := truncate(sanitize(h.Title), 30)
		if h.Archived {
			title += " (archived)"
		}
		where := hitWhere(h)
		if h.ParentID != "" {
			where += " in a subagent"
		}
		hint := where
		if h.Field != "title" {
			hint += " · " + LocalTime(h.CreatedAt) + " · " + sanitize(truncate(strings.TrimSpace(h.Snippet.Before+h.Snippet.Match+h.Snippet.After), 70))
		}
		m.Items = append(m.Items, MenuItem{Insert: "/search " + n, Label: n + "  " + title, Hint: hint, Run: true})
	}
	more := ""
	if res.More > 0 {
		more = fmt.Sprintf(" (%d more; narrow the search)", res.More)
	}
	m.Note = fmt.Sprintf("%d hits for %q%s · ↑↓ then Enter opens · /search N · Esc closes", len(res.Hits), arg, more)
	a.menu = m
	a.setNotice(fmt.Sprintf("%d hits for %q%s", len(res.Hits), arg, more))
}

// jumpToHit opens the hit's chat and prints the entry — its rendered
// lines, or its card's for a subagent's entry — under a line saying
// where it is, the way /find prints its matches: the transcript is in
// the terminal's scrollback, so that is where the person then finds it.
// What hides the entry is shown first: the steps (Ctrl+O) and the full
// transcript (Tab) for an entry in a subagent's card or past a fold.
func (a *App) jumpToHit(h SearchHit) {
	if a.ChatID != h.ChatID {
		a.selectChat(h.ChatID)
	}
	c := a.chat()
	if c == nil {
		a.setNotice("chat " + h.ChatID + " is not in the state yet; try again")
		return
	}
	title := sanitize(c.Title)
	if h.EntryID == "" {
		a.setNotice("opened " + title)
		return
	}
	var opened []string
	if a.quiet && (h.Role == "activity" || h.Role == "thinking" || h.ParentID != "") {
		a.quiet = false
		opened = append(opened, "steps shown")
	}
	if !a.expanded && (h.ParentID != "" || h.Field == "detail") {
		a.expanded = true
		opened = append(opened, "output expanded")
	}
	head := fmt.Sprintf("%s · %s · %s", title, hitWhere(h), LocalTime(h.CreatedAt))
	if len(opened) > 0 {
		head += " (" + strings.Join(opened, ", ") + ")"
	}
	width, _ := a.size()
	lines := entryLines(a.visible(c), width, a.expanded, h.EntryID, h.Snippet.Match)
	if len(lines) == 0 {
		a.setNotice(head + " — the entry is not shown (hidden by Ctrl+O?)")
		return
	}
	a.setNotice(head + ":\n" + strings.Join(lines, "\n"))
}

// entryLines is entry id as the transcript renders it — a subagent's
// entry with its card, since its lines sit under the card — cut to
// findLimit lines around the first line containing match (the whole
// head of the block when none does); nil when the entry is not shown.
func entryLines(c *Chat, width int, expanded bool, id, match string) []string {
	byID := map[string]Entry{}
	for _, e := range c.Conversation.Entries {
		byID[e.ID] = e
	}
	// The entry's outermost card.
	anchor := id
	seen := map[string]bool{}
	for {
		e, ok := byID[anchor]
		if !ok || e.ParentID == "" || seen[anchor] {
			break
		}
		if _, ok := byID[e.ParentID]; !ok {
			break
		}
		seen[anchor] = true
		anchor = e.ParentID
	}
	e, ok := byID[anchor]
	if !ok {
		return nil
	}
	_, children := nestEntries(c.Conversation.Entries)
	var lines []string
	for _, l := range renderEntry(c, e, width, expanded, children, "") {
		lines = append(lines, truncate(strings.TrimRight(plainText(l), " "), max(20, width-4)))
	}
	if len(lines) <= findLimit {
		return lines
	}
	// Around the match: from a few lines above it.
	from := 0
	if needle := strings.ToLower(strings.TrimSpace(match)); needle != "" {
		for i, l := range lines {
			if strings.Contains(strings.ToLower(l), needle) {
				from = max(0, i-3)
				break
			}
		}
	}
	to := min(len(lines), from+findLimit)
	out := append([]string{}, lines[from:to]...)
	if from > 0 {
		out = append([]string{dim + "…" + reset}, out...)
	}
	if to < len(lines) {
		out = append(out, dim+fmt.Sprintf("… %d more lines (your terminal's search finds them)", len(lines)-to)+reset)
	}
	return out
}
