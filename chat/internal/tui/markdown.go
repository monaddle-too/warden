package tui

import (
	"strings"
	"unicode/utf8"
)

// Lightweight markdown for the agent's replies: fenced code blocks are kept
// verbatim (hard-cut to the width, never re-flowed), headings are bold,
// bullets keep their marker, and inline `code`, **bold** and *italic* are
// styled per line after wrapping so widths stay exact. Anything else is
// plain text; nothing is interpreted beyond styling.

// renderMarkdown lays out text as terminal lines of at most width columns.
// prefix starts the first line, indent the rest.
func renderMarkdown(text string, width int, prefix, indent string) []string {
	var out []string
	lead := prefix
	next := func() string {
		l := lead
		lead = indent
		return l
	}
	inFence := false
	lines := strings.Split(text, "\n")
	var paragraph []string
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		joined := strings.Join(paragraph, " ")
		paragraph = nil
		for _, l := range wrap(joined, width, next(), indent) {
			out = append(out, styleInline(l, indent))
		}
	}
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flush()
			inFence = !inFence
			if inFence && len(trimmed) > 3 {
				out = append(out, next()+dim+"["+strings.TrimSpace(trimmed[3:])+"]"+reset)
			} else {
				lead = next() // consume the lead so the fence marker itself is silent
				_ = lead
				lead = indent
			}
		case inFence:
			avail := width - utf8.RuneCountInString(indent)
			if avail < 8 {
				avail = 8
			}
			l := next()
			runes := []rune(line)
			for len(runes) > avail {
				out = append(out, l+dim+"  "+string(runes[:avail-2])+reset)
				runes = runes[avail-2:]
				l = indent
			}
			out = append(out, l+dim+"  "+string(runes)+reset)
		case trimmed == "":
			flush()
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
		case strings.HasPrefix(trimmed, "#"):
			flush()
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			for _, l := range wrap(heading, width, next(), indent) {
				out = append(out, bold+l+reset)
			}
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "• "):
			flush()
			item := strings.TrimSpace(trimmed[2:])
			l := next()
			for i, w := range wrap(item, width, l+"• ", indent+"  ") {
				if i == 0 {
					out = append(out, styleInline(w, indent))
				} else {
					out = append(out, styleInline(w, indent))
				}
			}
		case isNumbered(trimmed):
			flush()
			n, rest, _ := strings.Cut(trimmed, " ")
			l := next()
			for _, w := range wrap(strings.TrimSpace(rest), width, l+n+" ", indent+strings.Repeat(" ", utf8.RuneCountInString(n)+1)) {
				out = append(out, styleInline(w, indent))
			}
		default:
			paragraph = append(paragraph, trimmed)
		}
	}
	flush()
	// Drop a trailing blank line.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func isNumbered(s string) bool {
	n, rest, ok := strings.Cut(s, " ")
	if !ok || len(n) < 2 || len(n) > 4 || !strings.HasSuffix(n, ".") {
		return false
	}
	for _, r := range n[:len(n)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return rest != ""
}

// styleInline applies `code`, **bold** and *italic* within one line. Markers
// are only consumed in matched pairs; a lone marker stays visible.
func styleInline(line, indent string) string {
	var b strings.Builder
	runes := []rune(line)
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch {
		case r == '`':
			end := indexRune(runes, i+1, '`')
			if end > i {
				b.WriteString(cyan + string(runes[i+1:end]) + reset)
				i = end + 1
				continue
			}
		case r == '*' && i+1 < len(runes) && runes[i+1] == '*':
			end := indexPair(runes, i+2)
			if end > i {
				b.WriteString(bold + string(runes[i+2:end]) + reset)
				i = end + 2
				continue
			}
		case r == '*' && i+1 < len(runes) && runes[i+1] != ' ':
			end := indexRune(runes, i+1, '*')
			if end > i+1 {
				b.WriteString(dim + string(runes[i+1:end]) + reset)
				i = end + 1
				continue
			}
		}
		b.WriteRune(r)
		i++
	}
	return b.String()
}

func indexRune(runes []rune, from int, want rune) int {
	for j := from; j < len(runes); j++ {
		if runes[j] == want {
			return j
		}
	}
	return -1
}

func indexPair(runes []rune, from int) int {
	for j := from; j+1 < len(runes); j++ {
		if runes[j] == '*' && runes[j+1] == '*' {
			return j
		}
	}
	return -1
}

// renderDetail lays out a tool entry's detail: a command's status and
// output, or file changes as diffs with added and removed lines coloured.
// When expanded is false only the last few lines are shown.
func renderDetail(detail string, width int, expanded bool) []string {
	lines := strings.Split(strings.TrimRight(detail, "\n"), "\n")
	if !expanded {
		kept := lastLines(detail, 6)
		if len(kept) < len(lines) {
			kept = append([]string{dim + "    │ … " + itoa(len(lines)-len(kept)) + " more lines (Tab to expand)" + reset}, kept...)
		}
		lines = kept
	}
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, dim+"    │ …") {
			out = append(out, l)
			continue
		}
		color := dim
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
			color = bold
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
