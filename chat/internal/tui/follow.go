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
// and pending approvals are announced. It returns when the chat is no
// longer running or queued and nothing is streaming, or when ctx ends.
// Entries whose IDs are in seen are not reprinted.
func Follow(ctx context.Context, client *Client, chatID string, out io.Writer, seen map[string]bool) (string, error) {
	if seen == nil {
		seen = map[string]bool{}
	}
	announced := map[string]bool{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final := ""
	var finalErr error
	settled := 0
	err := client.Events(ctx, func(s *State) {
		c := s.Chat(chatID)
		if c == nil {
			finalErr = fmt.Errorf("chat %s not found", chatID)
			cancel()
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
