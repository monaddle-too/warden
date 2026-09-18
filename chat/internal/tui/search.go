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
// scrolls to the entry, expanding the transcript when the entry sits
// inside a subagent's card or past a fold.

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

// jumpToHit opens the hit's chat and scrolls to its entry, showing what
// hides it first: the steps (Ctrl+O) and the full transcript (Tab) for
// an entry in a subagent's card or past a fold.
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
		a.scroll = 0
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
	notice := fmt.Sprintf("%s · %s · %s", title, hitWhere(h), LocalTime(h.CreatedAt))
	if len(opened) > 0 {
		notice += " (" + strings.Join(opened, ", ") + ")"
	}
	a.setNotice(notice)
	width, _ := a.viewSize()
	body := a.layout(width, false)
	a.scrollTo(body, entryOffset(a.visible(c), width, a.expanded, h.EntryID))
}

// scrollTo scrolls so that line i of body (the transcript as layout lays
// it out without the notice) is the first line of the next frame, with
// the notice just set counted among the lines above the status line.
func (a *App) scrollTo(body []string, i int) {
	_, rows := a.viewSize()
	a.scroll = max(1, len(body)-rows-i)
}

// viewSize is the width and the transcript rows the next frame will
// have: the terminal's size less the status line, the composer and what
// sits between them (the notice while scrolled; the menu is closed by
// then), as frame computes them.
func (a *App) viewSize() (width, rows int) {
	width, height := 100, 0
	if a.Size != nil {
		if w, h := a.Size(); w > 0 {
			width, height = w, h
		}
	}
	if height < 8 {
		rows = a.rows
		if rows <= 0 {
			rows = 20
		}
		return width, rows
	}
	a.scroll = 1 // as it will be: the notice among the extra lines
	extra := a.extraLines(width)
	if len(extra) > height/2 {
		extra = extra[:height/2]
	}
	prompt, _, _ := a.promptLines(width, min(6, height/3))
	return width, max(1, height-1-len(prompt)-len(extra))
}

// entryOffset is the line at which entry id starts in the rendered
// transcript (RenderTranscript with the same width and expansion) — for
// a subagent's entry, where its card starts, since its lines sit under
// the card; the end of the transcript when the entry is unknown.
func entryOffset(c *Chat, width int, expanded bool, id string) int {
	top, _ := nestEntries(c.Conversation.Entries)
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
	// The transcript up to the anchor: the cards before it in the order
	// the transcript shows them, with the entries nested under them.
	before := map[string]bool{}
	found := false
	for _, e := range queuedLast(top) {
		if e.ID == anchor {
			found = true
			break
		}
		before[e.ID] = true
	}
	if !found {
		return len(RenderTranscript(c, width, expanded))
	}
	prefix := *c
	prefix.Conversation.Entries = nil
	for _, e := range c.Conversation.Entries {
		root := e.ID
		for depth := 0; depth < 64; depth++ {
			p, ok := byID[root]
			if !ok || p.ParentID == "" {
				break
			}
			if _, ok := byID[p.ParentID]; !ok {
				break
			}
			root = p.ParentID
		}
		if before[root] {
			prefix.Conversation.Entries = append(prefix.Conversation.Entries, e)
		}
	}
	return len(RenderTranscript(&prefix, width, expanded))
}
