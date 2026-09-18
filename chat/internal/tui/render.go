package tui

import (
	"fmt"
	"sort"
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
