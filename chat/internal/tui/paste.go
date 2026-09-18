package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A long paste (bracketed paste from the terminal) does not land in the
// composer as it is: it becomes a placeholder, "[Pasted text #1 — 240
// lines]", and the text itself is kept aside until the prompt is sent,
// when every placeholder still in the draft is put back. The web composer
// does the same (web/src/paste.ts), with the same thresholds.

// PasteLines and PasteChars are the thresholds above which a paste is
// collapsed: more lines than fit a glance, or more text than a sentence.
const (
	PasteLines = 8
	PasteChars = 1000
)

var placeholder = regexp.MustCompile(`\[Pasted text #(\d+) — \d+ lines?\]`)

// LongPaste reports whether pasted text is collapsed rather than inserted.
func LongPaste(text string) bool {
	return len(text) > PasteChars || strings.Count(strings.TrimRight(text, "\n"), "\n")+1 > PasteLines
}

// PastePlaceholder is what stands in the draft for paste number n.
func PastePlaceholder(n int, text string) string {
	lines := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	return fmt.Sprintf("[Pasted text #%d — %d %s]", n, lines, unit)
}

// ExpandPastes puts the kept text back in place of each placeholder whose
// number names one of pastes (numbered from 1); a placeholder the person
// typed for a paste that does not exist stays as written.
func ExpandPastes(text string, pastes []string) string {
	return placeholder.ReplaceAllStringFunc(text, func(m string) string {
		n, _ := strconv.Atoi(placeholder.FindStringSubmatch(m)[1])
		if n >= 1 && n <= len(pastes) {
			return pastes[n-1]
		}
		return m
	})
}
