package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Unread entries and the jump to the bottom (docs/claude-parity.md,
// R2.18; the web's transcript.ts keeps the same mark in localStorage).
// The last entry the reader had in view is remembered per chat, in
// SeenFile (<state>/tui/seen.json), and advances with every frame of the
// selected chat (the transcript is the terminal's, which follows what is
// printed). Against it, /chats and the /switch menu mark a chat with new
// entries, the status bar counts them across the other chats, and
// switching into a chat prints a "── new ──" divider before the first
// unseen entry, with a notice saying how many are new. G (vim, empty
// draft), End and /bottom reprint the chat so the terminal's view is at
// its end.

// Seen is the mark: the entry's ID, and when it was made, so a
// transcript the service replaced (a rewind) still divides by time.
type Seen struct {
	ID string  `json:"id"`
	At float64 `json:"at"`
}

// LoadSeen reads the marks; a missing or unreadable file is no marks.
func LoadSeen(path string) map[string]Seen {
	out := map[string]Seen{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	if json.Unmarshal(b, &out) != nil {
		return map[string]Seen{}
	}
	return out
}

// SaveSeen writes the marks.
func SaveSeen(path string, marks map[string]Seen) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(marks)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

// UnreadStart is the index of the first entry the reader has not seen:
// the one after the mark, or the first newer than the mark's time when
// its entry is gone. -1 without a mark (a first visit: everything would
// be new, so a divider would say nothing), when nothing follows the
// mark, and when every entry is new.
func UnreadStart(entries []Entry, mark Seen, ok bool) int {
	if !ok || mark.ID == "" {
		return -1
	}
	start := -1
	for i, e := range entries {
		if e.ID == mark.ID {
			start = i + 1
			break
		}
	}
	if start < 0 {
		for i, e := range entries {
			if e.CreatedAt > mark.At {
				start = i
				break
			}
		}
	}
	if start <= 0 || start >= len(entries) {
		return -1
	}
	return start
}

// UnreadCount is how many messages (not tool steps, thinking or
// compaction dividers; the subagents' own entries not counted) follow
// the mark; 0 without a mark.
func UnreadCount(entries []Entry, mark Seen, ok bool) int {
	start := UnreadStart(entries, mark, ok)
	if start < 0 {
		return 0
	}
	n := 0
	for _, e := range entries[start:] {
		if e.ParentID == "" && e.Role != "activity" && e.Role != "thinking" && e.Role != "compaction" {
			n++
		}
	}
	return n
}

// UnreadDivider is the line before the first unseen entry.
const UnreadDivider = "── new ──"

// unreadMark is what a listing appends to a chat with unread messages:
// "• 3".
func unreadMark(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%s• %d%s", cyan, n, reset)
}

// loadSeen reads SeenFile once.
func (a *App) loadSeen() {
	if a.seenRead {
		return
	}
	a.seenRead = true
	a.seen = LoadSeen(a.SeenFile)
	if a.seen == nil {
		a.seen = map[string]Seen{}
	}
}

// unreadOf counts the chat's unread messages against its mark.
func (a *App) unreadOf(c *Chat) int {
	a.loadSeen()
	mark, ok := a.seen[c.ID]
	return UnreadCount(c.Conversation.Entries, mark, ok)
}

// unreadElsewhere sums the unread messages of the other listed chats.
func (a *App) unreadElsewhere(current *Chat) int {
	n := 0
	for _, ch := range a.sortedChats() {
		if current != nil && ch.ID == current.ID {
			continue
		}
		n += a.unreadOf(ch)
	}
	return n
}

// markUnread fixes where the selected chat's divider goes, from its mark
// as it is now (the web does the same when a chat opens), and returns a
// note saying how many messages are new ("" for none).
func (a *App) markUnread() string {
	a.unreadID = ""
	c := a.chat()
	if c == nil {
		return ""
	}
	a.loadSeen()
	mark, ok := a.seen[c.ID]
	i := UnreadStart(c.Conversation.Entries, mark, ok)
	if i < 0 {
		return ""
	}
	a.unreadID = c.Conversation.Entries[i].ID
	if n := UnreadCount(c.Conversation.Entries, mark, ok); n > 0 {
		return fmt.Sprintf("%d new %s since you were here; %s marks the first", n, plural2(n, "message"), UnreadDivider)
	}
	return ""
}

// unreadDividerLine is the divider as printed, across the width.
func unreadDividerLine(width int) string {
	return cyan + UnreadDivider + strings.Repeat("─", max(0, width-len([]rune(UnreadDivider))-1)) + reset
}

// markSeen advances the selected chat's mark to its last entry.
func (a *App) markSeen(c *Chat) {
	if c == nil || len(c.Conversation.Entries) == 0 {
		return
	}
	a.loadSeen()
	last := c.Conversation.Entries[len(c.Conversation.Entries)-1]
	if a.seen[c.ID].ID == last.ID {
		return
	}
	a.seen[c.ID] = Seen{ID: last.ID, At: last.CreatedAt}
	a.seenDirty = true
}

// flushSeen writes the marks when they changed.
func (a *App) flushSeen() {
	if !a.seenDirty {
		return
	}
	a.seenDirty = false
	if a.SeenFile != "" {
		_ = SaveSeen(a.SeenFile, a.seen)
	}
}
