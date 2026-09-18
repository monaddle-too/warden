package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Follow prints a chat's transcript to out as it grows, for scripts and
// `warden chat send --wait`: every entry is printed once, when it is complete,
// and pending approvals are announced. With messageID "" it follows the
// whole chat (`--wait-all`) and returns when the chat is no longer running
// or queued and nothing is streaming; with the ID of a message just sent
// it follows that message's own turn only — the message, then its turn's
// entries, nothing of the turns queued before or after it — and returns
// once that turn has ended, or when the message is withdrawn, fails to
// deliver, or stays queued while the chat stops (held after a Stop). It
// returns the chat's status, "idle" for a turn that ended while the queue
// moved on, or when ctx ends. Entries whose IDs are in seen are not
// reprinted.
func Follow(ctx context.Context, client *Client, chatID string, out io.Writer, seen map[string]bool, messageID string) (string, error) {
	if seen == nil {
		seen = map[string]bool{}
	}
	announced := map[string]bool{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final := ""
	var finalErr error
	settled := 0
	// The followed message's turn once the agent has it, and whether the
	// message has been seen at all (a later disappearance is a withdraw).
	turn, sighted := "", false
	err := client.Events(ctx, func(s *State) {
		c := s.Chat(chatID)
		if c == nil {
			finalErr = fmt.Errorf("chat %s not found", chatID)
			cancel()
			return
		}
		if messageID != "" {
			done, status, err := followMessage(out, c, messageID, seen, &turn, &sighted)
			if err != nil || done {
				final, finalErr = status, err
				if !done {
					final = c.Status
				}
			}
			if err != nil {
				cancel()
				return
			}
			for _, a := range c.Pending() {
				if !announced[a.ID] {
					announced[a.ID] = true
					fmt.Fprintf(out, "approval pending (%s): answer it in the Warden app or with `warden chat approve %s`\n", a.Method, chatID)
				}
			}
			if !done {
				settled = 0
				return
			}
			settled++
			if settled >= 3 {
				cancel()
			}
			return
		}
		streaming := false
		for _, e := range c.Conversation.Entries {
			if e.IsStreaming {
				streaming = true
				continue
			}
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			printEntry(out, c, e)
		}
		for _, a := range c.Pending() {
			if !announced[a.ID] {
				announced[a.ID] = true
				fmt.Fprintf(out, "approval pending (%s): answer it in the Warden app or with `warden chat approve %s`\n", a.Method, chatID)
			}
		}
		for _, r := range c.Reviews {
			if !announced[r.ID] {
				announced[r.ID] = true
				fmt.Fprintf(out, "review pending: %s — review it in the Warden app\n", r.Summary(c.Provider))
			}
		}
		active := c.Status == "running" || c.Status == "queued" || c.Status == "stopping" || streaming
		if active {
			settled = 0
			return
		}
		// Two consecutive quiet snapshots (the stream ticks every 200 ms)
		// avoid returning between a send and the run starting.
		settled++
		if settled >= 3 {
			final = c.Status
			if c.Error != "" {
				finalErr = fmt.Errorf("%s: %s", c.Status, c.Error)
			}
			cancel()
		}
	})
	if finalErr != nil {
		return final, finalErr
	}
	if err != nil {
		return final, err
	}
	return final, nil
}

// followMessage is one snapshot of following message messageID's own
// turn: it prints the message when first seen (with how many are queued
// ahead of it) and, once the agent has it, the complete entries of its
// turn. done reports the turn over (its record ended and nothing of it
// streams), with the status to return: "idle" when the turn completed
// (the chat may already be on the next queued message), the chat's own
// status when it did not. An error ends the wait: the message was
// withdrawn, failed to deliver, or is held in a stopped chat's queue.
func followMessage(out io.Writer, c *Chat, messageID string, seen map[string]bool, turn *string, sighted *bool) (bool, string, error) {
	var message *Entry
	for i := range c.Conversation.Entries {
		if c.Conversation.Entries[i].ID == messageID {
			message = &c.Conversation.Entries[i]
			break
		}
	}
	if message == nil {
		if *sighted {
			return false, c.Status, fmt.Errorf("the message was withdrawn")
		}
		return false, c.Status, fmt.Errorf("message %s not found in chat %s", messageID, c.ID)
	}
	if !*sighted {
		*sighted = true
		seen[message.ID] = true
		printEntry(out, c, *message)
		if message.Delivery == "queued" {
			ahead := 0
			for _, e := range c.Conversation.Entries {
				if e.ID == messageID {
					break
				}
				if e.Role == "user" && e.ParentID == "" && e.Delivery == "queued" {
					ahead++
				}
			}
			if ahead > 0 {
				fmt.Fprintf(out, "  (queued: %d message(s) ahead)\n", ahead)
			} else if c.Status == "running" {
				fmt.Fprintln(out, "  (queued: sends when the agent finishes)")
			}
		}
	}
	switch message.Delivery {
	case "failed":
		detail := message.Detail
		if detail == "" {
			detail = "not delivered"
		}
		return false, c.Status, fmt.Errorf("%s: %s", c.Status, detail)
	case "queued":
		if !c.Running() {
			return false, c.Status, fmt.Errorf("%s: the message is held in the queue; send it from the app, with `warden chat send`, or withdraw it", c.Status)
		}
		return false, c.Status, nil
	}
	if message.TurnID == nil {
		return false, c.Status, nil // "sending": handed over, not yet confirmed with its turn
	}
	*turn = *message.TurnID
	streaming := false
	for _, e := range c.Conversation.Entries {
		if e.TurnID == nil || *e.TurnID != *turn {
			continue
		}
		if e.IsStreaming {
			streaming = true
			continue
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		printEntry(out, c, e)
	}
	ended := false
	if t := c.Turn(*turn); t != nil && t.EndedAt != 0 {
		ended = true
	}
	if !ended || streaming {
		return false, c.Status, nil
	}
	status := c.Status
	if c.Running() {
		status = "idle" // this turn is over; the queue's next one has begun
	} else if c.Error != "" {
		return true, status, fmt.Errorf("%s: %s", c.Status, c.Error)
	}
	return true, status, nil
}

func printEntry(out io.Writer, c *Chat, e Entry) {
	text := sanitize(e.Text)
	switch e.Role {
	case "user":
		fmt.Fprintf(out, "you: %s\n", text)
	case "assistant":
		name := c.Provider
		if name == "" {
			name = "agent"
		}
		fmt.Fprintf(out, "%s: %s\n", name, text)
	case "activity":
		fmt.Fprintf(out, "  · %s\n", text)
		for _, l := range lastLines(sanitize(e.Detail), 4) {
			fmt.Fprintf(out, "    │ %s\n", l)
		}
	case "system":
		fmt.Fprintf(out, "  ! %s\n", text)
	case "compaction":
		fmt.Fprintf(out, "  ── %s ──\n", sanitize(CompactionText(e)))
	default:
		fmt.Fprintf(out, "  %s: %s\n", e.Role, text)
	}
}

// WaitIdle returns when the chat is neither running nor queued, polling the
// state; used before sending when a run may still be finishing.
func WaitIdle(ctx context.Context, client *Client, chatID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s, err := client.State(ctx)
		if err != nil {
			return err
		}
		c := s.Chat(chatID)
		if c == nil {
			return fmt.Errorf("chat %s not found", chatID)
		}
		if c.Status != "running" && c.Status != "queued" && c.Status != "stopping" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chat is still %s", strings.TrimSpace(c.Status))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
