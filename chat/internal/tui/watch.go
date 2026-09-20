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
	case a.Method == "warden/network/allow":
		return fmt.Sprintf("allow network access to %v for %v min: %v", a.Params["host"], a.Params["duration_minutes"], a.Params["reason"])
	case a.Method == "warden/ci/read":
		return fmt.Sprintf("read CI results of %v for %v min: %v", a.Params["repository"], a.Params["duration_minutes"], a.Params["reason"])
	case a.Method == "warden/repository/access":
		return fmt.Sprintf("share repository %v (%v): %v", a.Params["repository"], a.Params["categories"], a.Params["reason"])
	case a.Method == "warden/github/write":
		return fmt.Sprintf("github %v on %v #%v", a.Params["action"], a.Params["repository"], a.Params["number"])
	case a.Method == "warden/host/import":
		return fmt.Sprintf("copy host directory %v into the sandbox: %v", a.Params["path"], a.Params["reason"])
	case a.Method == "warden/host/export":
		return fmt.Sprintf("copy the sandbox's files back over %v", a.Params["path"])
	case len(a.Questions()) > 0:
		qs := a.Questions()
		return "question: " + qs[0].Question
	case a.Permission() != nil:
		p := a.Permission()
		switch {
		case p.IsPlan():
			return "plan ready for review"
		case p.Entry != nil && p.Entry.Tool != nil && p.Entry.Tool.Kind == "command":
			return "run command: " + p.Entry.Text
		case p.Entry != nil && p.Entry.Text != "":
			return "allow " + p.Entry.Text
		}
		return "allow " + p.Tool
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

// Watcher receives what Watch reports, each once with the chat it belongs
// to: an approval that becomes pending, and a review (a request only the
// app can settle) that appears. Either may be nil.
type Watcher struct {
	Approval func(*Chat, Approval)
	Review   func(*Chat, Review)
}

// Watch follows the event stream and reports every approval that becomes
// pending and every review that appears, once each. Those already waiting
// when Watch starts are reported too (once), so a restart never hides a
// waiting request. It returns when ctx ends or the stream fails.
func Watch(ctx context.Context, client *Client, w Watcher) error {
	seen := map[string]bool{}
	return client.Events(ctx, func(s *State) {
		for _, c := range s.Chats {
			if c.Archived {
				continue
			}
			for _, a := range c.Pending() {
				if seen[a.ID] || w.Approval == nil {
					continue
				}
				seen[a.ID] = true
				w.Approval(c, a)
			}
			for _, r := range c.Reviews {
				if seen[r.ID] || w.Review == nil {
					continue
				}
				seen[r.ID] = true
				w.Review(c, r)
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
