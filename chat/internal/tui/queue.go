package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The message queue and edit-and-resend (the web's queue.ts;
// docs/claude-parity.md, item 10). A message sent while the agent's turn
// runs waits in the transcript as queued and becomes its own turn after
// it; until then /withdraw N drops it and /edit N (or ↑ on an empty
// draft) takes it into the editor. Esc holds the queue: /queue send, or
// the next message, lets it go. /edit N on a message the agent got
// rewinds the conversation to before it (the code too with `both`) and
// puts the message in the editor to send again.

// queuedMessages lists the chat's queued messages in the order they go.
func queuedMessages(c *Chat) []Entry {
	var out []Entry
	for _, e := range c.Conversation.Entries {
		if e.Role == "user" && e.ParentID == "" && e.Delivery == "queued" {
			out = append(out, e)
		}
	}
	return out
}

// queueHeld reports a queue nothing is draining: messages queued while
// the chat is not running (Esc interrupted the turn).
func queueHeld(c *Chat) bool {
	return !c.Running() && len(queuedMessages(c)) > 0
}

// queueMarker is the line under a queued message: what happens to it.
func queueMarker(c *Chat) string {
	switch {
	case queueHeld(c):
		return "queued · held: /queue send lets it go, or the next message"
	case c.Status == "running":
		return "queued · sends when the agent finishes"
	}
	return "queued · next up"
}

// doubleEscape is how close two Escapes must be to count as Esc-Esc.
const doubleEscape = 600 * time.Millisecond

// queue lists the queued messages, or with "send" lets a held queue go.
func (a *App) queue(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	switch strings.ToLower(arg) {
	case "":
		list := queuedMessages(c)
		if len(list) == 0 {
			a.setNotice("nothing is queued; a message sent while the agent runs waits here")
			return
		}
		var b strings.Builder
		for i, e := range list {
			fmt.Fprintf(&b, "%2d  %s\n", i+1, truncate(excerptOf(e.Text), 70))
		}
		if queueHeld(c) {
			b.WriteString("held since the agent was stopped: /queue send lets them go in order, or the next message does · /withdraw N drops one · /edit N (↑ for the last) edits one")
		} else {
			b.WriteString("sent in order after this turn · /withdraw N drops one · /edit N (↑ for the last) edits one")
		}
		a.setNotice(b.String())
	case "send", "go", "release":
		if err := a.Client.SendQueued(ctx, c.ID); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.refreshState(ctx)
		a.setNotice("sending the queued messages")
	default:
		a.setNotice("/queue lists the queued messages; /queue send lets a held queue go")
	}
}

// withdraw drops queued message N (N from /queue).
func (a *App) withdraw(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	list := queuedMessages(c)
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(list) {
		a.setNotice("/withdraw N with N from /queue")
		return
	}
	if _, err := a.Client.Withdraw(ctx, c.ID, list[n-1].ID); err != nil {
		a.setNotice(err.Error())
		return
	}
	a.refreshState(ctx)
	a.setNotice(fmt.Sprintf("withdrawn: %s", truncate(excerptOf(list[n-1].Text), 60)))
}

// takeQueued withdraws a queued message into the editor: its text is the
// draft and its files wait for the next message again. Nothing is sent
// while it is edited; sent again, it goes at the end of the queue.
func (a *App) takeQueued(ctx context.Context, c *Chat, e Entry) bool {
	entry, err := a.Client.Withdraw(ctx, c.ID, e.ID)
	if err != nil {
		a.setNotice(err.Error())
		return false
	}
	a.refreshState(ctx)
	a.editor.Set(entry.Text)
	if len(entry.Attachments) > 0 {
		if a.attachments == nil {
			a.attachments = map[string][]Attachment{}
		}
		a.attachments[c.ID] = append(a.attachments[c.ID], entry.Attachments...)
	}
	a.setNotice("editing the queued message; Enter sends it again (at the end of the queue), Ctrl+C drops it")
	return true
}

// editLastQueued is ↑ on an empty draft: the last queued message into
// the editor. False when nothing is queued.
func (a *App) editLastQueued(ctx context.Context, c *Chat) bool {
	if c == nil {
		return false
	}
	list := queuedMessages(c)
	if len(list) == 0 {
		return false
	}
	return a.takeQueued(ctx, c, list[len(list)-1])
}

// edit is /edit N [both]: message N (as /rewind numbers them) into the
// editor — a queued one leaves the queue; one the agent got is rewound
// to before (the conversation, or the code too), after confirmation.
func (a *App) edit(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	targets := rewindTargets(c)
	if len(targets) == 0 {
		a.setNotice("no messages to edit")
		return
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		fields = []string{strconv.Itoa(len(targets))}
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 1 || n > len(targets) {
		a.setNotice("/edit [N] [both] with N from /rewind (the last message when omitted)")
		return
	}
	what := "conversation"
	if len(fields) > 1 {
		switch strings.ToLower(fields[1]) {
		case "both", "all", "code":
			what = "both"
		default:
			a.setNotice("/edit [N] [both]")
			return
		}
	}
	if len(fields) > 2 {
		a.setNotice("/edit [N] [both]")
		return
	}
	target := targets[n-1]
	if target.Delivery == "queued" {
		a.takeQueued(ctx, c, target)
		return
	}
	if c.Running() {
		a.setNotice("stop the agent first (Esc)")
		return
	}
	a.confirmEdit(c, n, target, what)
}

// confirmEdit asks before rewinding to before target for an edit, then
// rewinds and puts the message in the editor.
func (a *App) confirmEdit(c *Chat, n int, target Entry, what string) {
	label := map[string]string{"conversation": "the conversation", "both": "code and conversation"}[what]
	id := c.ID
	a.confirm = &confirmation{
		prompt: fmt.Sprintf("Edit message %d %q? %s goes back to before it and the message comes into the editor to send again. Type y and Enter to confirm; anything else cancels", n, truncate(excerptOf(target.Text), 60), strings.ToUpper(label[:1])+label[1:]),
		run: func(ctx context.Context) {
			result, err := a.Client.Rewind(ctx, id, target.ID, what)
			if err != nil {
				a.setNotice(err.Error())
				return
			}
			a.diff = nil
			a.refreshState(ctx)
			a.editor.Set(target.Text)
			if len(target.Attachments) > 0 {
				if a.attachments == nil {
					a.attachments = map[string][]Attachment{}
				}
				a.attachments[id] = append(a.attachments[id], target.Attachments...)
			}
			a.setNotice(rewindNotice(result, n, map[string]string{"conversation": "conversation", "both": "code and conversation"}[what]) + "; edit the message and Enter sends it")
		},
	}
}

// editLast is Esc-Esc on an empty draft: /edit on the last message.
func (a *App) editLast(ctx context.Context, c *Chat) {
	if c == nil {
		return
	}
	a.edit(ctx, c, "")
}
