package chats

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"warden/chat/internal/conversation"
)

// Search across chats (GET chats/search?q=&limit=): the titles and
// transcripts of every chat of the service, archived ones included, for
// a client that does not hold every transcript (the terminal client's
// /search; the web's ⌘K palette searches the state it already has, with
// the same order and folding, search.ts). A match is a literal substring
// once case is ignored and any whitespace equals a space. Title hits come
// first, then entries, newest chat and newest entry first, one hit per
// entry (its text, else a tool step's output); a subagent's nested
// entries and the person's own commands are among them.

// SearchHit is one match: the chat, and for an entry hit the entry, the
// field it matched in ("text" or "detail"; "title" for a chat's title),
// its role and sender, the subagent card it belongs to, and the text
// around the match.
type SearchHit struct {
	ChatID    string              `json:"chatID"`
	Title     string              `json:"title"`
	Archived  bool                `json:"archived,omitempty"`
	Provider  string              `json:"provider,omitempty"`
	EntryID   string              `json:"entryID,omitempty"`
	Field     string              `json:"field"`
	Role      string              `json:"role,omitempty"`
	ParentID  string              `json:"parentID,omitempty"`
	Sender    *conversation.Actor `json:"sender,omitempty"`
	CreatedAt float64             `json:"createdAt,omitempty"`
	Snippet   Snippet             `json:"snippet"`
}

// Snippet is the text around a match: up to 40 characters each side,
// cut at a word boundary when one is near, with an ellipsis where text
// was left out; whitespace runs become one space.
type Snippet struct {
	Before string `json:"before"`
	Match  string `json:"match"`
	After  string `json:"after"`
}

// SearchResult is what a search returns: the hits up to the limit and
// how many more there were.
type SearchResult struct {
	Hits []SearchHit `json:"hits"`
	More int         `json:"more"`
}

const (
	searchDefaultLimit = 40
	searchMaxLimit     = 200
	searchMaxQuery     = 200
)

// foldRunes is text as the search compares it: lower-cased, every
// whitespace rune a space, one rune per rune of the original so an
// offset into the folded text is the same offset into the original.
func foldRunes(s string) []rune {
	out := []rune(s)
	for i, r := range out {
		if unicode.IsSpace(r) {
			out[i] = ' '
		} else {
			out[i] = unicode.ToLower(r)
		}
	}
	return out
}

// foldQuery is the query as the search uses it: folded and trimmed;
// empty when it has nothing to look for.
func foldQuery(q string) []rune {
	if utf8.RuneCountInString(q) > searchMaxQuery {
		q = string([]rune(q)[:searchMaxQuery])
	}
	return []rune(strings.TrimSpace(string(foldRunes(q))))
}

// indexRunes is the rune offset of needle in hay, -1 for none.
func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(hay) {
		return -1
	}
	first := needle[0]
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i] != first {
			continue
		}
		for j := 1; j < len(needle); j++ {
			if hay[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// snippetAt is the text around the match at [start, end) of text, as
// search.ts's snippet.
func snippetAt(text []rune, start, end, radius int) Snippet {
	from := max(0, start-radius)
	to := min(len(text), end+radius)
	if from > 0 {
		// Up to 13 runes in from the cut: the first space starts the
		// snippet at a word.
		for i := from; i < min(start, from+13); i++ {
			if unicode.IsSpace(text[i]) {
				from = i + 1
				break
			}
		}
	}
	if to < len(text) {
		// The last space before the cut (past the match) ends it at a word.
		for i := to - 1; i >= max(end, to-13); i-- {
			if unicode.IsSpace(text[i]) {
				to = i
				break
			}
		}
	}
	clean := func(r []rune) string {
		return strings.Join(strings.Fields(string(r)), " ")
	}
	// Fields would drop a space at a cut's edge that separates words.
	spaced := func(r []rune, leading bool) string {
		s := clean(r)
		if len(r) > 0 && s != "" {
			if leading && unicode.IsSpace(r[0]) {
				s = " " + s
			}
			if !leading && unicode.IsSpace(r[len(r)-1]) {
				s += " "
			}
		}
		return s
	}
	out := Snippet{Before: spaced(text[from:start], false), Match: clean(text[start:end]), After: spaced(text[end:to], true)}
	if from > 0 {
		out.Before = "…" + out.Before
	}
	if to < len(text) {
		out.After += "…"
	}
	return out
}

// lastActivity is when a chat's last entry was written, 0 for none.
func lastActivity(c *Chat) float64 {
	if n := len(c.Conversation.Entries); n > 0 {
		return c.Conversation.Entries[n-1].CreatedAt
	}
	return 0
}

// Search runs a search across every chat of the service. limit 0 is the
// default; more than the maximum is the maximum.
func (e *Engine) Search(query string, limit int) SearchResult {
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	limit = min(limit, searchMaxLimit)
	result := SearchResult{Hits: []SearchHit{}}
	needle := foldQuery(query)
	if len(needle) == 0 {
		return result
	}
	chats := e.Store.Snapshot().Chats
	// Live chats first, then by the time of their last entry.
	sort.SliceStable(chats, func(i, j int) bool {
		if chats[i].Archived != chats[j].Archived {
			return !chats[i].Archived
		}
		return lastActivity(chats[i]) > lastActivity(chats[j])
	})
	total := 0
	add := func(h SearchHit) {
		total++
		if len(result.Hits) < limit {
			result.Hits = append(result.Hits, h)
		}
	}
	for _, c := range chats {
		title := []rune(c.Title)
		if at := indexRunes(foldRunes(c.Title), needle); at >= 0 {
			add(SearchHit{ChatID: c.ID, Title: c.Title, Archived: c.Archived, Provider: c.Provider, Field: "title", Snippet: snippetAt(title, at, at+len(needle), 200)})
		}
	}
	for _, c := range chats {
		entries := c.Conversation.Entries
		for i := len(entries) - 1; i >= 0; i-- {
			en := entries[i]
			field, text := "", []rune(en.Text)
			if at := indexRunes(foldRunes(en.Text), needle); at >= 0 {
				field = "text"
				add(SearchHit{ChatID: c.ID, Title: c.Title, Archived: c.Archived, Provider: c.Provider, EntryID: en.ID, Field: field, Role: en.Role, ParentID: en.ParentID, Sender: en.Sender, CreatedAt: en.CreatedAt, Snippet: snippetAt(text, at, at+len(needle), 40)})
				continue
			}
			if (en.Role != "activity" && en.Role != "aside") || en.Detail == "" {
				continue
			}
			text = []rune(en.Detail)
			if at := indexRunes(foldRunes(en.Detail), needle); at >= 0 {
				add(SearchHit{ChatID: c.ID, Title: c.Title, Archived: c.Archived, Provider: c.Provider, EntryID: en.ID, Field: "detail", Role: en.Role, ParentID: en.ParentID, Sender: en.Sender, CreatedAt: en.CreatedAt, Snippet: snippetAt(text, at, at+len(needle), 40)})
			}
		}
	}
	result.More = total - len(result.Hits)
	return result
}
