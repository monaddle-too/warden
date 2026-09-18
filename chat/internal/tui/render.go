package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ANSI styles. Colors are kept to the basic eight so any terminal renders
// them; everything the agent produced is untrusted text and is printed as
// text, never interpreted (escape sequences are stripped).
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	cyan   = "\x1b[36m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	blue   = "\x1b[34m"
)

// sanitize removes control characters and escape sequences from text the
// agent produced so it cannot repaint or drive the terminal.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '~' {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wrap breaks text into lines no wider than width runes, keeping words
// together where it can and honouring existing newlines. prefix is prepended
// to the first line and indent to the rest.
func wrap(text string, width int, prefix, indent string) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, paragraph := range strings.Split(text, "\n") {
		lead := prefix
		if out != nil {
			lead = indent
		}
		avail := width - utf8.RuneCountInString(lead)
		if avail < 4 {
			avail = 4
		}
		// A paragraph that fits keeps its spacing (aligned help text,
		// command output); only over-long ones are re-flowed by word.
		if utf8.RuneCountInString(paragraph) <= avail {
			if strings.TrimSpace(paragraph) == "" {
				out = append(out, strings.TrimRight(lead, " "))
			} else {
				out = append(out, lead+strings.TrimRight(paragraph, " "))
			}
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			out = append(out, strings.TrimRight(lead, " "))
			continue
		}
		line := ""
		for _, w := range words {
			for utf8.RuneCountInString(w) > avail {
				// A single over-long token is cut hard. Flushing a pending
				// line changes the indent and therefore avail, so re-check.
				if line != "" {
					out = append(out, lead+line)
					lead, line = indent, ""
					avail = width - utf8.RuneCountInString(lead)
					if avail < 4 {
						avail = 4
					}
					continue
				}
				runes := []rune(w)
				out = append(out, lead+string(runes[:avail]))
				w = string(runes[avail:])
				lead = indent
				avail = width - utf8.RuneCountInString(lead)
				if avail < 4 {
					avail = 4
				}
			}
			switch {
			case line == "":
				line = w
			case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) <= avail:
				line += " " + w
			default:
				out = append(out, lead+line)
				lead, line = indent, w
				avail = width - utf8.RuneCountInString(lead)
			}
		}
		out = append(out, lead+line)
	}
	return out
}

// lastLines keeps the final n non-empty lines of text.
func lastLines(text string, n int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept
}

// RenderTranscript lays out a chat's entries as terminal lines of at most
// width columns (styling excluded from the width). expanded shows tool
// output and diffs in full instead of their last lines, and a subagent's
// own transcript under its card.
func RenderTranscript(c *Chat, width int, expanded bool) []string {
	top, children := nestEntries(c.Conversation.Entries)
	var out []string
	for _, e := range queuedLast(top) {
		out = append(out, renderEntry(c, e, width, expanded, children, "")...)
		out = append(out, "")
	}
	return out
}

// nestEntries splits the entries into the conversation's own and, by the
// ID of each subagent's card, the entries that subagent produced. An
// entry whose card is unknown stays at the top rather than vanishing.
func nestEntries(entries []Entry) (top []Entry, children map[string][]Entry) {
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	children = map[string][]Entry{}
	for _, e := range entries {
		if e.ParentID != "" && ids[e.ParentID] {
			children[e.ParentID] = append(children[e.ParentID], e)
		} else {
			top = append(top, e)
		}
	}
	return top, children
}

// renderEntry lays out one entry: a message, a tool card (with a
// subagent's entries nested under its card), thinking, a notice. label
// names the agent whose message it is when not the chat's provider (a
// subagent's type inside its card).
func renderEntry(c *Chat, e Entry, width int, expanded bool, children map[string][]Entry, label string) []string {
	var out []string
	{
		text := sanitize(e.Text)
		switch e.Role {
		case "user":
			label := senderLabel(e)
			out = append(out, wrap(text, width, bold+cyan+label+" › "+reset, strings.Repeat(" ", len(label)+3))...)
			switch {
			case e.Delivery == "queued":
				// Held by Warden until the agent's turn ends (queue.go).
				out = append(out, yellow+"      ("+queueMarker(c)+")"+reset)
			case e.Delivery != "" && e.Delivery != "delivered" && e.Delivery != "confirmed":
				out = append(out, dim+"      ("+sanitize(e.Delivery)+")"+reset)
			}
		case "assistant":
			name := label
			if name == "" {
				name = c.Provider
			}
			if name == "" {
				name = "agent"
			}
			out = append(out, renderMarkdown(text, width, bold+green+name+" › "+reset, strings.Repeat(" ", utf8.RuneCountInString(name)+3))...)
			if e.IsStreaming {
				out[len(out)-1] += dim + " ▍" + reset
			}
		case "activity":
			if e.Tool != nil {
				out = append(out, renderTool(e, width, expanded)...)
				if e.Tool.Kind == "task" {
					out = append(out, renderSubagent(c, e, width, expanded, children)...)
				}
				return out
			}
			marker := dim + "  · "
			if e.IsStreaming {
				marker = yellow + "  ⋯ "
			}
			out = append(out, wrap(text, width, marker, "    ")...)
			out[len(out)-1] += reset
			if strings.TrimSpace(e.Detail) != "" {
				out = append(out, renderDetail(sanitize(e.Detail), width, expanded)...)
			}
		case "thinking":
			// The model's thinking: a dim line while it streams, then how
			// long it took, and the text itself when expanded.
			label := "Thought"
			if e.IsStreaming {
				label = "Thinking…"
			} else if e.EndedAt > e.CreatedAt {
				label = fmt.Sprintf("Thought for %s", (time.Duration((e.EndedAt - e.CreatedAt) * float64(time.Second))).Round(time.Second))
			}
			out = append(out, dim+"  ∴ "+label+reset)
			if expanded && strings.TrimSpace(text) != "" {
				out = append(out, wrap(dim+text+reset, width, "    ", "    ")...)
			}
		case "system":
			out = append(out, wrap(text, width, red+"  ! "+reset, "    ")...)
		case "rewind":
			// The marker a rewind leaves: the message the chat went back to
			// before and what was taken back, then what that did.
			out = append(out, wrap(yellow+text+reset, width, yellow+"  ↶ "+reset, "    ")...)
			if e.Detail != "" {
				out = append(out, wrap(dim+e.Detail+reset, width, "    ", "    ")...)
			}
		case "notice":
			out = append(out, wrap(dim+text+reset, width, dim+"  · ", "    ")...)
		case "compaction":
			// The agent compacted its context here: a divider with the
			// trigger and the token counts, the summary it continues from
			// when expanded; the error when it failed.
			out = append(out, renderCompaction(e, width, expanded)...)
		default:
			out = append(out, wrap(text, width, dim+"  "+e.Role+": "+reset, "    ")...)
		}
	}
	return out
}

// senderLabel names the person behind an entry: another person's name
// (else email) on a shared web deployment; the owner's own is "you".
func senderLabel(e Entry) string {
	if e.Sender != nil && e.Sender.PrincipalID != "owner" {
		switch {
		case e.Sender.Name != "":
			return sanitize(e.Sender.Name)
		case e.Sender.Email != "":
			return sanitize(e.Sender.Email)
		}
	}
	return "you"
}

// renderSubagent lays out what a subagent did under its card: one line
// with the count until expanded, then its entries indented, each
// rendered as the transcript renders the conversation's own (a nested
// subagent's card recurses), and the card's result — the subagent's
// final text — last.
func renderSubagent(c *Chat, e Entry, width int, expanded bool, children map[string][]Entry) []string {
	kids := children[e.ID]
	var out []string
	if len(kids) > 0 && !expanded {
		steps, messages := 0, 0
		for _, k := range kids {
			if k.Tool != nil {
				steps++
			} else if k.Role == "assistant" {
				messages++
			}
		}
		out = append(out, dim+fmt.Sprintf("    │ … %d tool calls, %d messages (Tab to expand)", steps, messages)+reset)
	} else if len(kids) > 0 {
		label := "subagent"
		if t := e.Tool; t != nil && t.Input != nil {
			if s, ok := t.Input["subagent_type"].(string); ok && s != "" {
				label = sanitize(s)
			}
		}
		inner := width - 4
		if inner < 20 {
			inner = 20
		}
		for _, k := range kids {
			for _, l := range renderEntry(c, k, inner, expanded, children, label) {
				out = append(out, "    "+l)
			}
		}
	}
	if detail := sanitize(strings.TrimRight(e.Detail, "\n")); strings.TrimSpace(detail) != "" {
		out = append(out, renderBody(strings.Split(detail, "\n"), width, expanded, false)...)
	}
	return out
}

// Tool mirrors the chat service's record of the tool call an activity
// entry is (conversation.Tool): what a surface renders by.
type Tool struct {
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Server      string         `json:"server"`
	Status      string         `json:"status"`
	Description string         `json:"description"`
	Paths       []string       `json:"paths"`
	Query       string         `json:"query"`
	Input       map[string]any `json:"input"`
	Background  bool           `json:"background"`
}

// foldedLines is how many lines of a tool's output or diff show before
// "Tab to expand"; the last ones of a command's output (where the result
// is), the first of anything else.
const foldedLines = 8

// renderTool lays out a typed tool entry: a head line in the tool's own
// terms (the command, the file edited with its counts, the file read, the
// search and its hits), then its body — output or diff — folded unless
// expanded. A read or search collapses to its one line; a failure shows
// its message.
func renderTool(e Entry, width int, expanded bool) []string {
	t := e.Tool
	marker := dim + "  · "
	if e.IsStreaming || t.Status == "running" {
		marker = yellow + "  ⋯ "
	}
	// Anything but running or completed is a failure, in the agent's own
	// word (declined, "exit 3", timed out).
	failed := !e.IsStreaming && t.Status != "running" && t.Status != "completed"
	if failed {
		marker = red + "  ✗ "
	}
	detail := sanitize(strings.TrimRight(e.Detail, "\n"))
	head := sanitize(e.Text)
	var body []string
	fromEnd := false
	switch t.Kind {
	case "command":
		head = "$ " + head
		if e.Sender != nil {
			// A person's own command ("!cmd"), not the agent's.
			head = bold + cyan + senderLabel(e) + reset + " " + head
		}
		fromEnd = true
		body = strings.Split(detail, "\n")
	case "edit":
		files, adds, dels := diffLines(detail)
		head += fmt.Sprintf("  %s+%d%s %s−%d%s", green, adds, reset, red, dels, reset)
		if !failed {
			body = files
		}
	case "read", "search":
		body = strings.Split(detail, "\n")
		n, unit := resultCount(t.Kind, detail)
		if !failed && t.Status == "completed" {
			head += fmt.Sprintf("  %s%d %s%s", dim, n, unit, reset)
		}
		if !expanded && !failed {
			body = nil // one line is the reading; Tab shows the content
		}
	case "task":
		// A subagent: how long it took (or has been at it) and its prompt
		// under the head; renderSubagent puts its own entries and then its
		// final text below.
		if secs := taskSeconds(e); secs > 0 {
			head += fmt.Sprintf("  %s%s%s", dim, formatSeconds(secs), reset)
		}
		if t.Input != nil {
			if p, ok := t.Input["prompt"].(string); ok && strings.TrimSpace(p) != "" && t.Description == "" {
				t = &Tool{Kind: t.Kind, Name: t.Name, Status: t.Status, Description: p, Input: t.Input, Background: t.Background}
			}
		}
	case "todo":
		body = renderTodo(strings.Split(detail, "\n"))
	default:
		body = strings.Split(detail, "\n")
		if t.Kind == "mcp" && expanded {
			body = append(inputLines(t.Input), body...)
		}
	}
	if t.Background {
		head += " " + dim + "[background]" + reset
	}
	if failed {
		head += " " + red + t.Status + reset
	}
	out := wrap(head, width, marker+reset, "    ")
	if t.Description != "" {
		desc := sanitize(t.Description)
		if t.Kind == "task" && !expanded {
			desc = strings.SplitN(desc, "\n", 2)[0]
		}
		out = append(out, wrap(desc, width, dim+"    ", "    ")...)
		out[len(out)-1] += reset
	}
	if len(body) == 1 && strings.TrimSpace(body[0]) == "" {
		body = nil
	}
	if t.Kind == "todo" {
		return append(out, body...)
	}
	if len(body) > 0 {
		out = append(out, renderBody(body, width, expanded, fromEnd)...)
	}
	return out
}

// taskSeconds is how long a subagent took: to its end, or so far while
// it runs.
func taskSeconds(e Entry) float64 {
	end := e.EndedAt
	if end == 0 && (e.IsStreaming || e.Tool.Status == "running") {
		end = float64(time.Now().UnixMilli()) / 1000
	}
	if end <= e.CreatedAt {
		return 0
	}
	return end - e.CreatedAt
}

// formatSeconds reads "4s", "1m 12s", "2h 5m".
func formatSeconds(secs float64) string {
	s := int(secs + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s%60)
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}

// renderTodo colours a todo list's lines: done dim, in progress bold,
// pending plain.
func renderTodo(lines []string) []string {
	var out []string
	for _, l := range lines {
		l = sanitize(l)
		switch {
		case strings.HasPrefix(l, "[x] "):
			out = append(out, dim+"    ✓ "+l[4:]+reset)
		case strings.HasPrefix(l, "[>] "):
			out = append(out, bold+yellow+"    ▸ "+l[4:]+reset)
		case strings.HasPrefix(l, "[ ] "):
			out = append(out, "    ○ "+l[4:])
		case strings.TrimSpace(l) == "":
		default:
			out = append(out, "    "+l)
		}
	}
	return out
}

// renderBody colours a tool body's lines by prefix (a diff's added and
// removed lines, its hunk headers) and folds it to foldedLines unless
// expanded; fromEnd keeps the last lines instead of the first.
func renderBody(lines []string, width int, expanded, fromEnd bool) []string {
	hidden := 0
	if !expanded && len(lines) > foldedLines {
		hidden = len(lines) - foldedLines
		if fromEnd {
			lines = lines[hidden:]
		} else {
			lines = lines[:foldedLines]
		}
	}
	more := dim + "    │ … " + itoa(hidden) + " more lines (Tab to expand)" + reset
	var out []string
	if hidden > 0 && fromEnd {
		out = append(out, more)
	}
	for _, l := range lines {
		color := dim
		switch {
		case strings.HasPrefix(l, "+"):
			color = green
		case strings.HasPrefix(l, "-"):
			color = red
		case strings.HasPrefix(l, "@@"):
			color = cyan
		case strings.HasPrefix(l, "§ "):
			color = bold
			l = strings.TrimPrefix(l, "§ ")
		}
		for _, w := range wrap(l, width, color+"    │ ", color+"    │ ") {
			out = append(out, w+reset)
		}
	}
	if hidden > 0 && !fromEnd {
		out = append(out, more)
	}
	return out
}

// diffLines keeps what a file change's diff says: its hunks' lines with
// their +/- prefixes and hunk headers, the file's path as a `§` heading
// when the change spans several files, and the counts of added and
// removed lines. The git header lines (`diff --git`, `---`, `+++`, modes)
// and the path line the service writes before each diff are left out.
func diffLines(detail string) (lines []string, adds, dels int) {
	var files [][]string
	var current []string
	path := ""
	header := false // inside a file's git header, before its first hunk line
	for _, l := range strings.Split(detail, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			if current != nil {
				files = append(files, current)
			}
			current = []string{"§ " + path}
			header = true
		case header && (strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "index ") || strings.Contains(l, " file mode ")):
		case strings.HasPrefix(l, "+"):
			adds++
			current = append(current, l)
			header = false
		case strings.HasPrefix(l, "-"):
			dels++
			current = append(current, l)
			header = false
		case strings.HasPrefix(l, "@@") || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\\"):
			current = append(current, l)
			header = false
		default:
			path = l // the service's path line before each diff
		}
	}
	if current != nil {
		files = append(files, current)
	}
	for _, f := range files {
		if len(files) == 1 {
			f = f[1:] // the head line names the file
		}
		lines = append(lines, f...)
	}
	return lines, adds, dels
}

// resultCount is what a read or search returned, for its head line: the
// count a search tool reports ("Found 3 files"), else its lines.
func resultCount(kind, detail string) (int, string) {
	unit := "lines"
	if kind == "search" {
		unit = "results"
	}
	if detail == "" {
		return 0, unit
	}
	first := strings.SplitN(detail, "\n", 2)[0]
	if strings.HasPrefix(first, "No files found") || strings.HasPrefix(first, "No matches found") {
		return 0, unit
	}
	if fields := strings.Fields(first); len(fields) >= 3 && fields[0] == "Found" {
		if n, err := strconv.Atoi(fields[1]); err == nil {
			return n, strings.TrimSuffix(fields[2], "s") + "s"
		}
	}
	n := 0
	for _, l := range strings.Split(detail, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, unit
}

// inputLines is a tool's input as `key: value` lines, keys in order.
func inputLines(input map[string]any) []string {
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		v := input[k]
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		out = append(out, k+": "+sanitize(strings.ReplaceAll(s, "\n", " ")))
	}
	return out
}

// RenderApprovals lays out the pending approvals with the keys that answer
// them. The first pending approval is the one y/n and typed answers address.
func RenderApprovals(c *Chat, width int) []string {
	pending := c.Pending()
	if len(pending) == 0 {
		return nil
	}
	var out []string
	for i, a := range pending {
		if p := a.Permission(); p != nil {
			out = append(out, renderPermission(a, p, width, i > 0)...)
			continue
		}
		head := "approval"
		body := ""
		switch {
		case a.Method == "warden/ports/bind":
			head = fmt.Sprintf("bind sandbox port %v", a.Params["port"])
			body = fmt.Sprintf("Publish %q at a Warden URL that only your signed-in browser can open.", a.Params["title"])
		case len(a.Questions()) > 0:
			head = "the agent has a question"
			for _, q := range a.Questions() {
				body += q.Question
				if len(q.Options) > 0 {
					var labels []string
					for _, o := range q.Options {
						labels = append(labels, o.Label)
					}
					body += "  [" + strings.Join(labels, " | ") + "]"
				}
				body += "\n"
			}
		default:
			head = a.Method
			keys := make([]string, 0, len(a.Params))
			for k := range a.Params {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := a.Params[k]
				s, ok := v.(string)
				if !ok {
					s = fmt.Sprint(v)
				}
				body += k + ": " + s + "\n"
			}
		}
		hint := "y = allow, n = decline"
		if len(a.Questions()) > 0 {
			hint = "type your answer and press Enter, or n to decline"
		}
		if i > 0 {
			hint = "answered after the one above"
		}
		out = append(out, bold+yellow+"⚠ "+sanitize(head)+reset+dim+"   "+hint+reset)
		for _, l := range strings.Split(strings.TrimRight(sanitize(body), "\n"), "\n") {
			if l != "" {
				out = append(out, wrap(l, width, "  ", "  ")...)
			}
		}
	}
	return out
}

// renderPermission lays out a tool ask: the call as the transcript shows
// it (a command, a diff's lines, a read's path) or the plan, and the keys
// that answer it.
func renderPermission(a Approval, p *Permission, width int, later bool) []string {
	head, hint := "", ""
	var body []string
	e := p.Entry
	switch {
	case p.IsPlan():
		head = "Claude has a plan"
		hint = "y = approve (auto) · a = approve, ask before edits · n [feedback] = keep planning"
		body = renderMarkdown(p.Plan, width-4, "", "")
	case e != nil && e.Tool != nil && e.Tool.Kind == "command":
		head = "run a command"
		if p.Description != "" {
			head += ": " + p.Description
		}
		for _, l := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
			body = append(body, "$ "+l)
		}
	case e != nil && e.Tool != nil && e.Tool.Kind == "edit":
		head = e.Text
		lines, adds, dels := diffLines(e.Detail)
		if len(lines) > 0 {
			body = append(body, fmt.Sprintf("+%d −%d", adds, dels))
			body = append(body, lines...)
		}
	case e != nil:
		head = e.Text
		if e.Tool != nil && len(e.Tool.Input) > 0 {
			body = inputLines(e.Tool.Input)
		}
	default:
		head = "use " + p.Tool
	}
	if hint == "" {
		hint = "y = allow · a = allow always"
		if p.Always != "" {
			hint += " (" + p.Always + ")"
		}
		hint += " · n [message] = deny"
	}
	if later {
		hint = "answered after the one above"
	}
	out := []string{bold + yellow + "⚠ " + sanitize(head) + reset + dim + "   " + hint + reset}
	for _, l := range body {
		l = sanitize(l)
		if l == "" {
			continue
		}
		color := ""
		switch {
		case strings.HasPrefix(l, "+"):
			color = green
		case strings.HasPrefix(l, "-"):
			color = red
		case strings.HasPrefix(l, "@@"):
			color = cyan
		}
		for _, w := range wrap(l, width, color+"    │ ", color+"    │ ") {
			out = append(out, w+reset)
		}
	}
	return out
}

// ChatLine is one row of a chat listing.
func ChatLine(i int, c *Chat) string {
	status := c.Status
	if c.Archived {
		status = "archived"
	}
	return fmt.Sprintf("%2d  %-32s  %-6s  %s", i, truncate(sanitize(c.Title), 32), c.Provider, status)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// CompactionText is the divider's line: "Context compacted · manual ·
// 171k → 2.2k tokens", "Compacting context…" while it runs, or the
// failure with its error (context.ts says the same on the web).
func CompactionText(e Entry) string {
	c := e.Compaction
	if c == nil {
		if e.Text != "" {
			return e.Text
		}
		return "Context compacted"
	}
	switch c.Status {
	case "running":
		return "Compacting context…"
	case "failed":
		if c.Error != "" {
			return "Compaction failed: " + c.Error
		}
		return "Compaction failed"
	}
	var parts []string
	switch c.Trigger {
	case "manual":
		parts = append(parts, "manual")
	case "auto":
		parts = append(parts, "automatic")
	case "":
	default:
		parts = append(parts, c.Trigger)
	}
	switch {
	case c.PreTokens > 0 && c.PostTokens > 0:
		parts = append(parts, FormatTokens(c.PreTokens)+" → "+FormatTokens(c.PostTokens)+" tokens")
	case c.PreTokens > 0:
		parts = append(parts, "from "+FormatTokens(c.PreTokens)+" tokens")
	}
	if len(parts) == 0 {
		return "Context compacted"
	}
	return "Context compacted · " + strings.Join(parts, " · ")
}

func renderCompaction(e Entry, width int, expanded bool) []string {
	text := sanitize(CompactionText(e))
	colour := dim
	if e.Compaction != nil {
		switch e.Compaction.Status {
		case "running":
			colour = yellow
		case "failed":
			colour = red
		}
	} else if e.IsStreaming {
		colour = yellow
	}
	rule := "──"
	out := wrap(text, width, colour+"  "+rule+" ", "     ")
	out[len(out)-1] += " " + rule + reset
	if expanded && strings.TrimSpace(e.Detail) != "" && (e.Compaction == nil || e.Compaction.Status == "completed") {
		out = append(out, wrap(dim+sanitize(e.Detail)+reset, width, "     ", "     ")...)
	}
	return out
}
