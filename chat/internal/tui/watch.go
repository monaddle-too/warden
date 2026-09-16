package tui

import (
	"context"
	"fmt"
	"strings"
)

// Summary is a one-line, sanitised description of an approval for
// notifications and popups.
func (a Approval) Summary() string {
	switch {
	case a.Method == "warden/ports/bind":
		return fmt.Sprintf("bind sandbox port %v (%v)", a.Params["port"], a.Params["title"])
	case len(a.Questions()) > 0:
		qs := a.Questions()
		return "question: " + qs[0].Question
	case a.Method == "item/commandExecution/requestApproval":
		if cmd, ok := a.Params["command"].(string); ok && cmd != "" {
			return "run command: " + cmd
		}
		return "run a command"
	case a.Method == "item/fileChange/requestApproval":
		return "change files"
	case a.Method == "item/permissions/requestApproval":
		return "permission request"
	}
	return a.Method
}

// Watch follows the event stream and calls notify once for every approval
// that becomes pending, with the chat it belongs to. Approvals already
// pending when Watch starts are reported too (once), so a restart never
// hides a waiting request. It returns when ctx ends or the stream fails.
func Watch(ctx context.Context, client *Client, notify func(*Chat, Approval)) error {
	seen := map[string]bool{}
	return client.Events(ctx, func(s *State) {
		for _, c := range s.Chats {
			if c.Archived {
				continue
			}
			for _, a := range c.Pending() {
				if seen[a.ID] {
					continue
				}
				seen[a.ID] = true
				notify(c, a)
			}
		}
	})
}

// PopupURL is the app URL that opens on the given chat, for a browser popup.
func PopupURL(appURL, chatID string) string {
	base, fragment, _ := strings.Cut(appURL, "#")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	u := base + sep + "chat=" + chatID
	if fragment != "" {
		u += "#" + fragment
	}
	return u
}
