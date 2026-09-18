package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/tui"
)

// Popup modes for `warden start --popups`. The default is none: a pending
// approval waits in the app and the terminal client, which both show it and
// answer it, until someone opens one; an approval a person is already looking
// at in one client must not open another (2026-09-18, the owner's call after
// a question asked in the terminal client opened the browser).
const (
	popupsAuto    = "auto"    // browser when detached, notification when in the foreground
	popupsBrowser = "browser" // desktop notification and open the chat in the browser
	popupsNotify  = "notify"  // desktop notification only
	popupsNone    = "none"    // the default
)

// notifyCommand builds the desktop notification command for this host:
// osascript on macOS, notify-send elsewhere. Text goes through as data,
// never interpolated into a script: the AppleScript string literal is
// escaped, and notify-send takes arguments.
func notifyCommand(title, body string) *exec.Cmd {
	if path, err := exec.LookPath("osascript"); err == nil {
		return exec.Command(path, "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", title, body)
	}
	if path, err := exec.LookPath("notify-send"); err == nil {
		return exec.Command(path, "--app-name=Warden", title, body)
	}
	return nil
}

func desktopNotify(title, body string) error {
	cmd := notifyCommand(title, body)
	if cmd == nil {
		return errors.New("no desktop notification command (osascript or notify-send) found")
	}
	return cmd.Run()
}

// popups watches the running chat service for approvals and surfaces each
// one once: a desktop notification, and in browser mode the app opened on
// that chat so the card is in front of the person. It never answers
// anything; answering stays with the app, the terminal client or
// `warden chat approve`.
func popups(ctx context.Context, cfg config.Config, mode string, detached bool, log io.Writer) {
	if mode == popupsAuto {
		if detached {
			mode = popupsBrowser
		} else {
			mode = popupsNotify
		}
	}
	if mode == popupsNone {
		return
	}
	base, token, err := endpoint(cfg.OwnerTokenFile())
	if err != nil {
		fmt.Fprintf(log, "warden: approval popups disabled: %v\n", err)
		return
	}
	client := &tui.Client{Base: base, Token: token}
	appURL := ""
	if url, err := launchURL(cfg.OwnerTokenFile(), time.Now()); err == nil {
		if cfg.Auth.Mode == "owner" && cfg.Auth.PublicURL != "" {
			url, _ = throughEdge(url, cfg.Auth.PublicURL)
		}
		appURL = url
	}
	err = tui.Watch(ctx, client, func(c *tui.Chat, a tui.Approval) {
		summary := a.Summary()
		title := "Warden: " + strings.TrimSpace(c.Title)
		fmt.Fprintf(log, "warden: approval pending in %q: %s\n", c.Title, summary)
		if err := desktopNotify(title, summary); err != nil {
			fmt.Fprintf(log, "warden: notification failed: %v\n", err)
		}
		if mode == popupsBrowser && appURL != "" {
			if err := openBrowser(tui.PopupURL(appURL, c.ID)); err != nil {
				fmt.Fprintf(log, "warden: could not open the browser: %v\n", err)
			}
		}
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(log, "warden: approval popups stopped: %v\n", err)
	}
}
