package tui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What the agent is doing right now, in a few words for the status line:
// read off the newest entry still running. A command says what it runs,
// a file change which file, a read which file, a search what for, a fetch
// where, a subagent how far it is; the model's thinking and a compaction
// say so. The web derives the same words in activity.ts; the two share
// the cases in testdata/activity.json, so they cannot drift apart.

const (
	activityThinking   = "Agent is thinking"
	activityCompacting = "Compacting context"
	activityWorking    = "Agent is working"
)

var spaceRun = regexp.MustCompile(`\s+`)

// trimCommand is a command as the status shows it: its first line,
// whitespace collapsed, cut to limit characters.
func trimCommand(command string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	line = spaceRun.ReplaceAllString(line, " ")
	if utf8.RuneCountInString(line) > limit {
		r := []rune(line)
		return string(r[:limit-1]) + "…"
	}
	return line
}

// basename is the last segment of a path.
func basename(path string) string {
	clean := strings.TrimRight(path, "/")
	if i := strings.LastIndex(clean, "/"); i >= 0 {
		return clean[i+1:]
	}
	return clean
}

var urlHost = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://([^/?#]+)`)

// host is the host of a URL; the URL itself when it does not parse.
func host(u string) string {
	if m := urlHost.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return u
}

func entryRunning(e Entry) bool {
	return e.IsStreaming || (e.Tool != nil && e.Tool.Status == "running")
}

// subagentLabel is "Explore agent: 3 tool calls · Reading hello.txt": the
// subagent's type, how far it is (from its entries or the agent's own
// count, whichever is further) and what it is doing now when the agent
// says.
func subagentLabel(card Entry, entries []Entry) string {
	label := "Subagent"
	if card.Tool != nil {
		if t, _ := card.Tool.Input["subagent_type"].(string); t != "" {
			label = t + " agent"
		}
	}
	calls := 0
	for _, e := range entries {
		if e.ParentID == card.ID && e.Tool != nil {
			calls++
		}
	}
	if card.Tool != nil && card.Tool.Progress != nil && int(card.Tool.Progress.ToolCalls) > calls {
		calls = int(card.Tool.Progress.ToolCalls)
	}
	out := label + ": starting"
	if calls == 1 {
		out = label + ": 1 tool call"
	} else if calls > 1 {
		out = fmt.Sprintf("%s: %d tool calls", label, calls)
	}
	// What the subagent is doing now, in its agent's words when given.
	if card.Tool != nil && card.Tool.Progress != nil && card.Tool.Progress.Activity != "" {
		out += " · " + trimCommand(card.Tool.Progress.Activity, 48)
	}
	return out
}

// stepLabel is the words for one running step, by its tool's kind.
func stepLabel(e Entry, entries []Entry) string {
	t := e.Tool
	if t == nil {
		return e.Text
	}
	file := ""
	if len(t.Paths) > 0 {
		file = basename(t.Paths[0])
	}
	or := func(s, fallback string) string {
		if s != "" {
			return s
		}
		return fallback
	}
	switch t.Kind {
	case "command":
		if c := trimCommand(e.Text, 48); c != "" {
			return "Running " + c
		}
		return "Running a command"
	case "edit":
		verb := "Editing"
		if t.Name == "Write" {
			verb = "Writing"
		}
		if len(t.Paths) > 1 {
			return fmt.Sprintf("%s %d files", verb, len(t.Paths))
		}
		if file != "" {
			return verb + " " + file
		}
		return verb + " a file"
	case "read":
		if file != "" {
			return "Reading " + file
		}
		return "Reading a file"
	case "search":
		if t.Name == "LS" {
			if file != "" {
				return "Listing " + file
			}
			return "Listing files"
		}
		if t.Query != "" {
			return "Searching for " + trimCommand(t.Query, 32)
		}
		return "Searching files"
	case "webSearch":
		if t.Query != "" {
			return "Searching the web for " + trimCommand(t.Query, 32)
		}
		return "Searching the web"
	case "fetch":
		if t.Query != "" {
			return "Fetching " + host(t.Query)
		}
		return "Fetching a page"
	case "mcp":
		if t.Name != "" {
			return "Calling " + t.Name
		}
		return "Calling a tool"
	case "task":
		return subagentLabel(e, entries)
	case "todo":
		return "Updating the todo list"
	}
	if s := trimCommand(e.Text, 60); s != "" {
		return s
	}
	return or(t.Name, "")
}

// subagentOf is the card of the subagent e works for, when that subagent
// still runs: the outermost one when subagents nest.
func subagentOf(e Entry, entries []Entry) (Entry, bool) {
	var top Entry
	found := false
	seen := map[string]bool{}
	for id := e.ParentID; id != "" && !seen[id]; {
		seen[id] = true
		var parent *Entry
		for i := range entries {
			if entries[i].ID == id {
				parent = &entries[i]
				break
			}
		}
		if parent == nil || parent.Tool == nil || parent.Tool.Kind != "task" || !entryRunning(*parent) {
			break
		}
		top, found = *parent, true
		id = parent.ParentID
	}
	return top, found
}

// ActivityLabel is what the agent is doing, from the newest running
// entry; "" when no entry says (a reply streaming, a turn between steps).
// A command the person ran (`!cmd`) and a command running in the
// background are not what the agent is doing now.
func ActivityLabel(entries []Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !entryRunning(e) {
			continue
		}
		switch e.Role {
		case "thinking":
			return activityThinking
		case "compaction":
			return activityCompacting
		case "activity":
		default:
			continue
		}
		if e.Sender != nil {
			continue
		}
		if e.Tool != nil && e.Tool.Background && e.Tool.Kind != "task" {
			continue
		}
		if card, ok := subagentOf(e, entries); ok {
			return subagentLabel(card, entries)
		}
		if label := stepLabel(e, entries); label != "" {
			return label
		}
	}
	return ""
}

// RunningLabel is the status line's words while the agent's turn runs.
func RunningLabel(entries []Entry) string {
	if s := ActivityLabel(entries); s != "" {
		return s
	}
	return activityWorking
}
