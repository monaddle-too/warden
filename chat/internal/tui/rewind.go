package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// /rewind and /diff: checkpoints, rewind and the session diff (the web's
// rewind.ts; docs/claude-parity.md, item 11).

// rewindTargets are the chat's own user messages, oldest first, the ones
// a rewind can name (a subagent's prompt is not one).
func rewindTargets(c *Chat) []Entry {
	var out []Entry
	for _, e := range c.Conversation.Entries {
		if e.Role == "user" && e.ParentID == "" {
			out = append(out, e)
		}
	}
	return out
}

// rewindScope reads the scope word: code, conversation (conv) or both;
// both when empty.
func rewindScope(word string) (string, bool) {
	switch strings.ToLower(word) {
	case "", "both", "all":
		return "both", true
	case "code", "files":
		return "code", true
	case "conv", "conversation", "chat":
		return "conversation", true
	}
	return "", false
}

// rewind lists the messages (no argument) or asks to confirm a rewind to
// before message N with the scope given, then runs it.
func (a *App) rewind(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	targets := rewindTargets(c)
	if len(targets) == 0 {
		a.setNotice("no messages to rewind to")
		return
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		known := map[string]bool{}
		if list, err := a.Client.Checkpoints(ctx, c.ID); err == nil {
			for _, cp := range list {
				known[cp.ID] = true
			}
		}
		var b strings.Builder
		for i, e := range targets {
			mark := " "
			if known[e.ID] {
				mark = "•"
			}
			fmt.Fprintf(&b, "%2d %s %s\n", i+1, mark, truncate(excerptOf(e.Text), 70))
		}
		b.WriteString("/rewind N [code|conv|both] goes back to before message N (• has a checkpoint; code needs one)")
		a.setNotice(b.String())
		return
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 1 || n > len(targets) {
		a.setNotice("/rewind N with N from /rewind")
		return
	}
	scope := ""
	if len(fields) > 1 {
		scope = fields[1]
	}
	what, ok := rewindScope(scope)
	if !ok || len(fields) > 2 {
		a.setNotice("/rewind N [code|conv|both]")
		return
	}
	if c.Running() {
		a.setNotice("stop the agent first (Esc)")
		return
	}
	target := targets[n-1]
	label := map[string]string{"code": "code", "conversation": "conversation", "both": "code and conversation"}[what]
	id := c.ID
	a.confirm = &confirmation{
		prompt: fmt.Sprintf("Rewind to before message %d %q (%s)? Type y and Enter to confirm; anything else cancels", n, truncate(excerptOf(target.Text), 60), label),
		run: func(ctx context.Context) {
			result, err := a.Client.Rewind(ctx, id, target.ID, what)
			if err != nil {
				a.setNotice(err.Error())
				return
			}
			a.diff = nil
			a.refreshState(ctx)
			a.setNotice(rewindNotice(result, n, label))
		},
	}
}

// rewindNotice says what a rewind did, as the marker's second line does.
func rewindNotice(r RewindResult, n int, label string) string {
	parts := []string{fmt.Sprintf("rewound to before message %d (%s)", n, label)}
	if r.What != "conversation" {
		if len(r.Restored)+len(r.Removed) == 0 {
			parts = append(parts, "the workspace already matched the checkpoint")
		} else {
			parts = append(parts, fmt.Sprintf("%d file(s) restored, %d removed", len(r.Restored), len(r.Removed)))
		}
	}
	switch r.Conversation {
	case "pending":
		parts = append(parts, "the agent forgets the messages when its session resumes")
	case "fresh":
		parts = append(parts, "the agent's session could not rewind; the next message starts a new one with the conversation so far as context")
	}
	return strings.Join(parts, "; ")
}

// excerptOf is a message's first words on one line.
func excerptOf(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if line == "" {
		return "(attachments)"
	}
	return line
}

// showDiff toggles the session diff under the transcript: fetched now,
// shown folded per file until Tab expands; /diff again (or an argument
// of "close") hides it.
func (a *App) showDiff(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	if a.diff != nil || arg == "close" || arg == "off" {
		a.diff = nil
		a.setNotice("changes hidden")
		return
	}
	changes, err := a.Client.Diff(ctx, c.ID)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	a.diff = changes
	a.scroll = 0
	if len(changes.Files) == 0 {
		a.setNotice("no changes since this chat began")
		return
	}
	a.setNotice(fmt.Sprintf("%d changed file(s); Tab expands the hunks, /diff hides them", len(changes.Files)))
}

// RenderChanges lays out the session diff: a heading with the counts,
// then each file as one line (path, +added −removed), followed by its
// hunks when expanded.
func RenderChanges(d *WorkspaceChanges, width int, expanded bool) []string {
	added, removed := 0, 0
	for _, f := range d.Files {
		added += f.Added
		removed += f.Removed
	}
	head := fmt.Sprintf("Changes since this chat began: %d file(s), %s+%d%s %s−%d%s", len(d.Files), green, added, reset, red, removed, reset)
	if d.Truncated {
		head += dim + " (diff cut at 2 MiB)" + reset
	}
	out := wrap(bold+head+reset, width, "", "  ")
	if len(d.Files) == 0 {
		return append(out, dim+"  no changes"+reset)
	}
	hunks := splitDiff(d.Diff)
	for _, f := range d.Files {
		counts := fmt.Sprintf("%s+%d%s %s−%d%s", green, f.Added, reset, red, f.Removed, reset)
		if f.Binary {
			counts = dim + "binary" + reset
		}
		out = append(out, wrap(bold+sanitize(f.Path)+reset+"  "+counts, width, "  ", "    ")...)
		if !expanded {
			continue
		}
		text, ok := hunks[f.Path]
		if !ok {
			out = append(out, dim+"    │ (not in the diff)"+reset)
			continue
		}
		lines, _, _ := diffLines(text)
		out = append(out, renderBody(lines, width, true, false)...)
	}
	if !expanded {
		out = append(out, dim+"  Tab expands the hunks"+reset)
	}
	return out
}

// splitDiff cuts one git diff into its files by the path each `diff
// --git` line names (the new path).
func splitDiff(diff string) map[string]string {
	out := map[string]string{}
	current := ""
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			header := strings.TrimSuffix(line, "\n")
			current = strings.TrimPrefix(header, "diff --git ")
			if i := strings.Index(current, " b/"); i >= 0 {
				current = current[i+3:]
			}
		}
		if current != "" {
			out[current] += line
		}
	}
	return out
}
