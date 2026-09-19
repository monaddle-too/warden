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
//
// A review (a pull request proposal, suggested document edits, a document
// selection or creation) is different: only the app can do it, so the
// launcher opens the app on its chat under every mode but silent, which
// is for a machine with no browser to open (2026-09-18).
const (
	popupsAuto    = "auto"    // browser when detached, notification when in the foreground
	popupsBrowser = "browser" // desktop notification and open the chat in the browser
	popupsNotify  = "notify"  // desktop notification only
	popupsNone    = "none"    // the default: approvals wait; reviews still open the app
	popupsSilent  = "silent"  // nothing, not even for a review
)

// popupModes is the flag's accepted values, in the order the help lists them.
var popupModes = []string{popupsAuto, popupsBrowser, popupsNotify, popupsNone, popupsSilent}

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

// popupper surfaces what the running chat service waits on, per the
// --popups mode: an approval (a desktop notification in notify and
// browser modes, the app opened on the chat in browser mode) and a review
// (the app opened on the chat in every mode, a notification where
// approvals get one). It never answers anything; answering stays with the
// app, the terminal client or `warden chat approve`. The notifier and the
// opener are fields so a test sees what would have opened.
type popupper struct {
	mode   string // auto already resolved
	appURL string // the launch URL; "" when it could not be built
	log    io.Writer
	notify func(title, body string) error
	open   func(url string) error
}

func (p *popupper) notifies() bool { return p.mode == popupsNotify || p.mode == popupsBrowser }

func (p *popupper) approval(c *tui.Chat, a tui.Approval) {
	if !p.notifies() {
		return
	}
	summary := a.Summary()
	fmt.Fprintf(p.log, "warden: approval pending in %q: %s\n", c.Title, summary)
	if err := p.notify("Warden: "+strings.TrimSpace(c.Title), summary); err != nil {
		fmt.Fprintf(p.log, "warden: notification failed: %v\n", err)
	}
	if p.mode == popupsBrowser {
		p.openChat(c)
	}
}

func (p *popupper) review(c *tui.Chat, r tui.Review) {
	if p.mode == popupsSilent {
		return
	}
	summary := r.Summary(c.Provider)
	fmt.Fprintf(p.log, "warden: review pending in %q: %s; opening the app\n", c.Title, summary)
	if p.notifies() {
		if err := p.notify("Warden: "+strings.TrimSpace(c.Title), summary+" — review it in the app"); err != nil {
			fmt.Fprintf(p.log, "warden: notification failed: %v\n", err)
		}
	}
	p.openChat(c)
}

// openChat opens the app on the chat, when the launch URL is known.
func (p *popupper) openChat(c *tui.Chat) {
	if p.appURL == "" {
		fmt.Fprintf(p.log, "warden: cannot open the app on %q: no launch URL\n", c.Title)
		return
	}
	if err := p.open(tui.PopupURL(p.appURL, c.ID)); err != nil {
		fmt.Fprintf(p.log, "warden: could not open the browser: %v\n", err)
	}
}

// resolvePopups turns auto into the mode for this launch.
func resolvePopups(mode string, detached bool) string {
	if mode != popupsAuto {
		return mode
	}
	if detached {
		return popupsBrowser
	}
	return popupsNotify
}

// popups watches the running chat service and surfaces each approval and
// review once (popupper). Under silent nothing is watched.
func popups(ctx context.Context, cfg config.Config, mode string, detached bool, log io.Writer) {
	mode = resolvePopups(mode, detached)
	if mode == popupsSilent {
		return
	}
	base, token, err := endpoint(cfg.OwnerTokenFile())
	if err != nil {
		fmt.Fprintf(log, "warden: approval popups disabled: %v\n", err)
		return
	}
	client := &tui.Client{Base: base, Token: token}
	p := &popupper{mode: mode, log: log, notify: desktopNotify, open: openBrowser}
	if url, err := appURL(cfg, false, time.Now()); err == nil {
		p.appURL = url
	}
	err = tui.Watch(ctx, client, tui.Watcher{Approval: p.approval, Review: p.review})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(log, "warden: approval popups stopped: %v\n", err)
	}
}
