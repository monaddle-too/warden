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
// output and diffs in full instead of their last lines.
func RenderTranscript(c *Chat, width int, expanded bool) []string {
	var out []string
	for _, e := range c.Conversation.Entries {
		text := sanitize(e.Text)
		switch e.Role {
		case "user":
			// Another person's message carries their name (else email) on
			// a shared web deployment; the owner's own is "you".
			label := "you"
			if e.Sender != nil && e.Sender.PrincipalID != "owner" {
				switch {
				case e.Sender.Name != "":
					label = sanitize(e.Sender.Name)
				case e.Sender.Email != "":
					label = sanitize(e.Sender.Email)
				}
			}
			out = append(out, wrap(text, width, bold+cyan+label+" › "+reset, strings.Repeat(" ", len(label)+3))...)
			if e.Delivery != "" && e.Delivery != "delivered" && e.Delivery != "confirmed" {
				out = append(out, dim+"      ("+sanitize(e.Delivery)+")"+reset)
			}
		case "assistant":
			name := c.Provider
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
				out = append(out, "")
				continue
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
		default:
			out = append(out, wrap(text, width, dim+"  "+e.Role+": "+reset, "    ")...)
		}
		out = append(out, "")
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
	failed := t.Status == "failed"
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
	default:
		body = strings.Split(detail, "\n")
		if t.Kind == "mcp" && expanded {
			body = append(inputLines(t.Input), body...)
		}
	}
	if failed {
		head += " " + red + "failed" + reset
	}
	out := wrap(head, width, marker+reset, "    ")
	if t.Description != "" {
		out = append(out, wrap(sanitize(t.Description), width, dim+"    ", "    ")...)
		out[len(out)-1] += reset
	}
	if len(body) == 1 && strings.TrimSpace(body[0]) == "" {
		body = nil
	}
	if len(body) > 0 {
		out = append(out, renderBody(body, width, expanded, fromEnd)...)
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
