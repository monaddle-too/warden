package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"warden/chat/internal/chats"
)

// App is the interactive terminal client for one chat at a time.
type App struct {
	Client    *Client
	ChatID    string
	Input     io.Reader          // raw terminal bytes
	Output    io.Writer          // the terminal
	Size      func() (int, int)  // columns, rows
	OpenURL   func(string) error // browser opener, may be nil
	Clipboard func(string) error // clipboard writer for /copy, may be nil
	AppURL    string             // where `/open` sends the browser (the launch URL)
	Provider  string             // provider for /new when none is given
	Now       func() time.Time
	Resize    <-chan struct{} // terminal size changed; redraw
	// HistoryDir keeps the prompt history per chat (<dir>/<chatID>); empty
	// keeps it in memory for the session.
	HistoryDir string
	// ExportDir is where /export writes a relative file name; empty is the
	// working directory.
	ExportDir string
	// ChatCommands lists commands the chat itself offers (the agent's slash
	// commands, once the chat state carries them) for the / menu; nil today.
	ChatCommands func(*Chat) []Command
	// BellFile keeps the /bell setting ("on" or "off"); empty keeps it for
	// the session.
	BellFile string

	mu       sync.Mutex
	bell     *bool // the bell setting once read (longtail.go)
	lastSeen *Chat // the selected chat as of the last snapshot, for the bell's events
	title    string
	state    *State
	live     bool
	editor   Editor
	notice   string
	noticeAt time.Time
	scroll   int  // lines scrolled up from the tail
	rows     int  // transcript rows in the last frame
	expanded bool // show tool output and diffs in full
	quiet    bool // hide tool steps and thinking (Ctrl+O)
	// diff is the session diff /diff fetched, shown under the transcript
	// (one line per file; Tab expands the hunks) until /diff again.
	diff    *WorkspaceChanges
	redraw  bool // clear the screen on the next draw (Ctrl+L)
	quit    bool
	ctrlC   time.Time // last Ctrl+C; a second within ctrlCQuit quits
	lastEsc time.Time // last Esc on an idle chat; a second within doubleEscape edits the last message

	menu        *Menu
	menuOff     string // the draft the menu was dismissed for (Esc)
	pathSeq     atomic.Int64
	pathResults chan pathResult
	// later carries what a background request (a "!" command, which may
	// run for a minute) has to apply on the main loop: a notice, a state
	// refresh.
	later   chan func(context.Context)
	search  *searchState
	confirm *confirmation
	// editing is set while the composer holds a file or the person's
	// instructions (/memory edit, /instructions edit): Enter saves it
	// through save, Esc cancels. memoryFiles is the last listing per chat,
	// so /memory N can name a file by number.
	editing     *editing
	memoryFiles map[string][]MemoryFile
	attachments map[string][]Attachment // uploads waiting for the next message, per chat
	histories   map[string][]string     // in-memory history per chat when HistoryDir is empty
	historyChat string                  // chat whose history the editor holds
}

const ctrlCQuit = 2 * time.Second

// pathDebounce is how long a keystroke waits before the workspace is asked
// for paths, so typing a segment costs one lookup.
var pathDebounce = 150 * time.Millisecond

// MenuItem is one row of the completion menu.
type MenuItem struct {
	Insert string // replaces the trigger's range when picked
	Label  string
	Hint   string
	Run    bool // Enter sends the completed draft (a command with no argument to type)
}

// Menu is the open completion menu.
type Menu struct {
	Trigger  Trigger
	Items    []MenuItem
	Selected int
	Note     string // shown under the rows: looking up, nothing found
}

type pathResult struct {
	seq   int64
	query string
	paths []string
	err   error
}

// searchState is a Ctrl+R reverse search through the prompt history.
type searchState struct {
	query string
	index int // match in the editor's history; -1 for none
}

// confirmation is a question the next line answers (y confirms).
type confirmation struct {
	prompt string
	run    func(context.Context)
}

// editing is what the composer is editing instead of a message: a label
// for the status, and save, which stores the text (the draft comes back
// when it fails).
type editing struct {
	label string
	save  func(context.Context, string) error
}

// page is how far PgUp/PgDn move: a screen minus two lines of context.
func (a *App) page() int {
	if a.rows > 4 {
		return a.rows - 2
	}
	return 10
}

const helpText = `commands   type / for the menu (Tab or Enter completes); /help lists them
           /new [title] /chats /switch N · /rename TITLE /archive /restore /delete
           /attach PATH /attachments /detach N · /export [md|json] [all] [FILE]
           /stop /model M /provider P /mode M · /open /previews /preview N /unpublish N
           /rewind (list) /rewind N [code|conv|both] · /diff (toggle; Tab expands)
           /queue (list) /queue send · /withdraw N · /edit [N] [both] (N from /rewind)
           /fork (list) /fork N|all copies the chat into a sibling · /cost totals so far
           /btw QUESTION asks a copy of the session (never sent to the agent)
           /style [default|Explanatory|Learning] · /bell [on|off]
           /find TEXT /copy /expand /verbose /clear /quit
           /instructions [edit|clear] your standing instructions, given to the agent in every chat
           /memory [FILE] [edit FILE] the workspace's CLAUDE.md, rules and auto-memory files
           /compact [what to keep] asks Claude to replace the history with a summary
composer   Enter sends · Alt+Enter (or Ctrl+J) inserts a line break · paste keeps newlines
           a long paste becomes [Pasted text #N — M lines] and is sent in full
           !cmd runs a shell command in the workspace as you (not the agent)
           #note appends a bullet to the workspace's CLAUDE.md
           @path completes a workspace path (Tab or Enter accepts)
           Up/Down recall prompts (or move between lines) · Ctrl+R searches them
           a message sent while the agent runs is queued: ↑ (empty draft) edits the last one
           Esc Esc (empty draft, agent idle) edits your last message: the conversation rewinds to before it
           Ctrl+A/E line start/end · Ctrl+U/K delete to line start/end · Ctrl+W a word
keys       y / n answer the first pending approval; typed text answers a question
           tool asks: y allow · a allow always · n [message] deny
           plans: y approve (auto) · a approve, ask before edits · n [feedback] keep planning
           Shift+Tab cycles a Claude chat's permission mode (auto → ask → plan)
           Esc interrupts the agent · Ctrl+C clears the draft (twice quits) · Ctrl+D quits
           Ctrl+O shows or hides tool steps and thinking · Tab (empty draft) expands output
           Ctrl+L redraws · scroll: mouse wheel, PgUp/PgDn, Home/End with an empty draft`

// NewMessageID is a fresh client message id (retries reuse it).
func NewMessageID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) setNotice(s string) {
	a.notice, a.noticeAt = s, a.now()
}

func (a *App) chat() *Chat {
	if a.state == nil {
		return nil
	}
	return a.state.Chat(a.ChatID)
}

// escapeAlone is how long a lone Escape byte waits for the rest of a
// sequence before it counts as the Escape key.
const escapeAlone = 60 * time.Millisecond

// readKeys decodes the terminal's bytes into keys until the input ends or
// ctx is done. A lone Escape is reported after escapeAlone so that Esc on
// its own (interrupt) is seen without the next keypress.
func (a *App) readKeys(ctx context.Context, keys chan<- Key) {
	var mu sync.Mutex
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	gen := 0
	send := func(k Key) bool {
		select {
		case keys <- k:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		n, err := a.Input.Read(chunk)
		mu.Lock()
		gen++
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		decoded, rest := DecodeKeys(buf)
		buf = append(buf[:0], rest...)
		pendingEsc := len(buf) == 1 && buf[0] == 0x1b
		g := gen
		mu.Unlock()
		for _, k := range decoded {
			if !send(k) {
				return
			}
		}
		if pendingEsc {
			time.AfterFunc(escapeAlone, func() {
				mu.Lock()
				alone := gen == g && len(buf) == 1 && buf[0] == 0x1b
				if alone {
					buf = buf[:0]
				}
				mu.Unlock()
				if alone {
					send(Key{Kind: KeyEscape})
				}
			})
		}
		if err != nil {
			send(Key{Kind: KeyEOF})
			return
		}
	}
}

// Run drives the terminal until the person quits or ctx ends. The caller
// puts the terminal in raw mode and restores it; Run owns the alternate
// screen.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	snapshots := make(chan *State, 1)
	streamErr := make(chan error, 1)
	go func() {
		err := a.Client.Events(ctx, func(s *State) {
			select {
			case snapshots <- s:
			default:
				// Replace the stale snapshot with the fresh one.
				select {
				case <-snapshots:
				default:
				}
				snapshots <- s
			}
		})
		streamErr <- err
	}()
	keys := make(chan Key, 64)
	go a.readKeys(ctx, keys)
	if a.pathResults == nil {
		a.pathResults = make(chan pathResult, 4)
	}
	if a.later == nil {
		a.later = make(chan func(context.Context), 8)
	}
	if s, err := a.Client.State(ctx); err == nil {
		a.state, a.live = s, true
	} else {
		a.setNotice(err.Error())
	}
	a.loadHistory()
	// Alternate screen, cursor hidden while painting, and mouse wheel
	// reporting (SGR encoding) so the wheel scrolls the transcript. Text
	// selection then needs the terminal's modifier (Option or Shift).
	// The terminal's title is pushed (xterm's title stack) and popped at
	// exit, so the person gets theirs back; setTitle keeps it current.
	fmt.Fprint(a.Output, "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[22;0t")
	defer fmt.Fprint(a.Output, "\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l\x1b[23;0t")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	a.draw()
	for !a.quit {
		select {
		case <-ctx.Done():
			return nil
		case err := <-streamErr:
			if err != nil {
				a.live = false
				a.setNotice(err.Error())
				a.draw()
				return err
			}
			return nil
		case s := <-snapshots:
			a.state, a.live = s, true
			a.bellForEvents()
			a.draw()
		case k := <-keys:
			a.handleKey(ctx, k)
			a.draw()
		case r := <-a.pathResults:
			a.applyPaths(r)
			a.draw()
		case f := <-a.later:
			f(ctx)
			a.draw()
		case <-a.Resize:
			a.draw()
		case <-ticker.C:
			a.draw() // clock, spinner, notices ageing
		}
	}
	return nil
}

// selectChat makes id the current chat: the view goes to the tail, menus
// close and the editor takes that chat's prompt history.
func (a *App) selectChat(id string) {
	a.diff = nil
	if a.ChatID != id {
		a.saveHistory()
		a.lastSeen = nil
	}
	a.ChatID = id
	a.scroll = 0
	a.menu, a.confirm, a.search = nil, nil, nil
	a.loadHistory()
}

// loadHistory gives the editor the current chat's prompts.
func (a *App) loadHistory() {
	if a.historyChat == a.ChatID && a.editor.History() != nil {
		return
	}
	a.historyChat = a.ChatID
	if a.HistoryDir != "" {
		a.editor.SetHistory(LoadHistory(a.HistoryDir, a.ChatID))
		return
	}
	a.editor.SetHistory(a.histories[a.ChatID])
}

// saveHistory keeps the editor's in-memory history for the chat it holds.
func (a *App) saveHistory() {
	if a.HistoryDir != "" || a.historyChat == "" {
		return
	}
	if a.histories == nil {
		a.histories = map[string][]string{}
	}
	a.histories[a.historyChat] = append([]string(nil), a.editor.History()...)
}

// send submits the draft: it goes to history (the file when HistoryDir is
// set) and then to the chat or the command.
func (a *App) send(ctx context.Context) {
	var text string
	if ed := a.editing; ed != nil {
		// The composer holds a file: Enter saves it as typed (no trimming,
		// no history), and the draft stays if the save fails.
		text = a.editor.Text()
		a.menu = nil
		a.scroll = 0
		if err := ed.save(ctx, text); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.editing = nil
		a.editor.Clear()
		a.setNotice("saved " + ed.label)
		return
	}
	if a.confirm != nil {
		// An answer to a confirmation is not a prompt worth recalling.
		text = strings.TrimSpace(a.editor.Text())
		a.editor.Clear()
	} else {
		text = a.editor.Submit()
		if text != "" && a.HistoryDir != "" && a.ChatID != "" {
			AppendHistory(a.HistoryDir, a.ChatID, text)
		}
	}
	a.menu = nil
	a.scroll = 0
	if text == "" {
		return
	}
	a.submit(ctx, text)
}

func (a *App) handleKey(ctx context.Context, k Key) {
	if a.search != nil {
		a.searchKey(k)
		return
	}
	if a.menu != nil && a.menuKey(ctx, k) {
		return
	}
	c := a.chat()
	switch k.Kind {
	case KeyEOF:
		a.quit = true
	case KeyCtrlD:
		if a.editor.Text() == "" {
			a.quit = true
			return
		}
		a.editor.Handle(Key{Kind: KeyDelete})
		a.refreshMenu(ctx)
	case KeyCtrlC:
		a.confirm = nil
		now := a.now()
		if ed := a.editing; ed != nil {
			a.editing = nil
			a.editor.Clear()
			a.menu = nil
			a.ctrlC = now
			a.setNotice("edit of " + ed.label + " cancelled · Ctrl+C again to quit")
			return
		}
		if a.editor.Text() != "" || a.menu != nil {
			a.editor.Clear()
			a.menu = nil
			a.ctrlC = now
			a.setNotice("draft cleared · Ctrl+C again to quit")
			return
		}
		if !a.ctrlC.IsZero() && now.Sub(a.ctrlC) < ctrlCQuit {
			a.quit = true
			return
		}
		a.ctrlC = now
		if c != nil && c.Running() {
			a.setNotice("Ctrl+C again to quit · Esc interrupts the agent")
		} else {
			a.setNotice("Ctrl+C again to quit")
		}
	case KeyCtrlL:
		a.redraw = true
	case KeyEscape:
		switch {
		case a.confirm != nil:
			a.confirm = nil
			a.setNotice("cancelled")
		case a.editing != nil:
			label := a.editing.label
			a.editing = nil
			a.editor.Clear()
			a.setNotice("edit of " + label + " cancelled; nothing saved")
		case c != nil && c.Running():
			if err := a.Client.Stop(ctx, c.ID); err != nil {
				a.setNotice(err.Error())
			} else if len(queuedMessages(c)) > 0 {
				a.setNotice("interrupting the agent; the queued messages are held (/queue send lets them go)")
			} else {
				a.setNotice("interrupting the agent")
			}
		case c != nil && a.editor.Text() == "" && !a.lastEsc.IsZero() && a.now().Sub(a.lastEsc) <= doubleEscape:
			// Esc-Esc (Claude Code's): the last message back into the
			// editor, the conversation rewound to before it (queue.go).
			a.lastEsc = time.Time{}
			a.editLast(ctx, c)
		default:
			a.lastEsc = a.now()
			a.scroll = 0
		}
	case KeyCtrlO:
		a.quiet = !a.quiet
		if a.quiet {
			a.setNotice("hiding tool steps and thinking (Ctrl+O shows them)")
		} else {
			a.setNotice("showing tool steps and thinking")
		}
	case KeyCtrlR:
		a.search = &searchState{index: -1}
	case KeyPageUp:
		a.scroll += a.page()
	case KeyPageDown:
		a.scroll = max(0, a.scroll-a.page())
	case KeyWheelUp:
		a.scroll += 3
	case KeyWheelDown:
		a.scroll = max(0, a.scroll-3)
	case KeyHome, KeyEnd:
		// With nothing typed these go to the top and bottom of the
		// transcript; while composing they move the cursor.
		if a.editor.Text() == "" {
			if k.Kind == KeyHome {
				a.scroll = 1 << 30 // clamped to the top by frame
			} else {
				a.scroll = 0
			}
			return
		}
		a.editor.Handle(k)
	case KeyShiftTab:
		a.cycleMode(ctx)
	case KeyTab:
		if a.editor.Text() == "" {
			a.expanded = !a.expanded
			if a.expanded {
				a.setNotice("showing full tool output and diffs (Tab to collapse)")
			} else {
				a.setNotice("showing the last lines of tool output (Tab to expand)")
			}
			return
		}
		a.menuOff = ""
		a.refreshMenu(ctx)
		if a.menu != nil && len(a.menu.Items) == 1 {
			a.acceptMenu(ctx, false)
		}
	case KeyEnter:
		a.send(ctx)
	case KeyUp:
		// On an empty draft with a message queued, ↑ edits the last one
		// (Claude Code's); otherwise the line above, or the history.
		if a.editor.Text() == "" && a.editLastQueued(ctx, c) {
			return
		}
		if a.editor.Handle(k) {
			a.refreshMenu(ctx)
		}
	default:
		if a.editor.Handle(k) {
			a.refreshMenu(ctx)
		}
	}
}

// searchKey handles a key while Ctrl+R searches the history.
func (a *App) searchKey(k Key) {
	s := a.search
	h := a.editor.History()
	switch k.Kind {
	case KeyRune:
		s.query += string(k.Rune)
		s.index = SearchHistory(h, s.query, len(h))
	case KeyBackspace:
		if r := []rune(s.query); len(r) > 0 {
			s.query = string(r[:len(r)-1])
		}
		s.index = SearchHistory(h, s.query, len(h))
	case KeyCtrlR:
		if i := SearchHistory(h, s.query, s.index); i >= 0 {
			s.index = i
		}
	case KeyEscape, KeyCtrlG, KeyCtrlC:
		a.search = nil
	case KeyEnter, KeyTab, KeyLeft, KeyRight, KeyHome, KeyEnd, KeyUp, KeyDown:
		if s.index >= 0 && s.index < len(h) {
			a.editor.Set(h[s.index])
		}
		a.search = nil
	}
}

// menuKey handles a key while the completion menu is open; false lets
// the key through to the composer.
func (a *App) menuKey(ctx context.Context, k Key) bool {
	m := a.menu
	switch k.Kind {
	case KeyUp, KeyCtrlP:
		if len(m.Items) > 0 {
			m.Selected = (m.Selected + len(m.Items) - 1) % len(m.Items)
		}
		return true
	case KeyDown, KeyCtrlN:
		if len(m.Items) > 0 {
			m.Selected = (m.Selected + 1) % len(m.Items)
		}
		return true
	case KeyTab, KeyEnter:
		if len(m.Items) == 0 {
			a.menu = nil
			return false
		}
		a.acceptMenu(ctx, k.Kind == KeyEnter)
		return true
	case KeyEscape:
		a.menuOff = a.editor.Text()
		a.menu = nil
		return true
	}
	return false
}

// acceptMenu puts the selected suggestion into the draft; with enter, a
// suggestion that completes a command sends it.
func (a *App) acceptMenu(ctx context.Context, enter bool) {
	m := a.menu
	if m == nil || len(m.Items) == 0 {
		return
	}
	item := m.Items[m.Selected]
	text, caret := replaceRange([]rune(a.editor.Text()), m.Trigger.Start, m.Trigger.End, item.Insert)
	a.editor.Set(string(text))
	a.editor.SetCursor(caret)
	a.menu = nil
	if enter && item.Run {
		a.send(ctx)
		return
	}
	a.refreshMenu(ctx)
}

// refreshMenu opens, updates or closes the completion menu for the draft.
func (a *App) refreshMenu(ctx context.Context) {
	text := a.editor.Text()
	if text != "" && text == a.menuOff {
		a.menu = nil
		return
	}
	a.menuOff = ""
	t, ok := triggerAt([]rune(text), a.editor.Cursor())
	if !ok {
		a.menu = nil
		return
	}
	prev := a.menu
	m := &Menu{Trigger: t}
	c := a.chat()
	switch t.Kind {
	case "command":
		for _, cmd := range commandItems(t.Query, a.chatCommands(c)) {
			insert := "/" + cmd.Name
			if cmd.Arg != "" {
				insert += " "
			}
			label := "/" + cmd.Name
			if cmd.Arg != "" {
				label += " " + cmd.Arg
			}
			m.Items = append(m.Items, MenuItem{Insert: insert, Label: label, Hint: cmd.Hint, Run: cmd.Arg == "" || strings.HasPrefix(cmd.Arg, "[")})
		}
	case "path":
		if c == nil {
			a.menu = nil
			return
		}
		if prev != nil && prev.Trigger.Kind == "path" && prev.Trigger.Query == t.Query {
			prev.Trigger = t
			return
		}
		if prev != nil && prev.Trigger.Kind == "path" {
			m.Items = prev.Items // the last answer stays until the new one arrives
		}
		m.Note = "Looking up paths…"
		a.requestPaths(ctx, c.ID, t.Query)
	case "local":
		for _, p := range localPaths(t.Query) {
			m.Items = append(m.Items, MenuItem{Insert: p, Label: p, Run: !strings.HasSuffix(p, "/")})
		}
	case "chat":
		m.Items = chatItems(a.sortedChats(), t.Query)
	}
	if len(m.Items) == 0 && m.Note == "" {
		a.menu = nil
		return
	}
	if prev != nil && prev.Trigger.Kind == t.Kind && prev.Selected < len(m.Items) {
		if prev.Selected < len(prev.Items) && prev.Items[prev.Selected].Insert == m.Items[prev.Selected].Insert {
			m.Selected = prev.Selected
		}
	}
	a.menu = m
}

// requestPaths asks the workspace for the paths matching query after a
// short pause; only the newest request's answer is applied.
func (a *App) requestPaths(ctx context.Context, chatID, query string) {
	seq := a.pathSeq.Add(1)
	if a.pathResults == nil {
		a.pathResults = make(chan pathResult, 4)
	}
	go func() {
		if pathDebounce > 0 {
			select {
			case <-time.After(pathDebounce):
			case <-ctx.Done():
				return
			}
			if a.pathSeq.Load() != seq {
				return // superseded while waiting
			}
		}
		paths, err := a.Client.Paths(ctx, chatID, query)
		select {
		case a.pathResults <- pathResult{seq: seq, query: query, paths: paths, err: err}:
		case <-ctx.Done():
		}
	}()
}

// applyPaths fills the path menu from an answer, if it is still wanted.
func (a *App) applyPaths(r pathResult) {
	if r.seq != a.pathSeq.Load() || a.menu == nil || a.menu.Trigger.Kind != "path" || a.menu.Trigger.Query != r.query {
		return
	}
	m := a.menu
	m.Items, m.Note = nil, ""
	if r.err != nil {
		m.Note = sanitize(r.err.Error())
		return
	}
	for _, p := range r.paths {
		m.Items = append(m.Items, MenuItem{Insert: mentionFor(p), Label: p})
	}
	if len(m.Items) == 0 {
		m.Note = "No matching paths"
	} else if len(m.Items) >= 50 {
		m.Note = "More paths match; keep typing"
	}
	if m.Selected >= len(m.Items) {
		m.Selected = 0
	}
}

func isYes(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "y", "yes":
		return true
	}
	return false
}

func (a *App) submit(ctx context.Context, text string) {
	if q := a.confirm; q != nil {
		a.confirm = nil
		if isYes(text) {
			q.run(ctx)
		} else {
			a.setNotice("cancelled")
		}
		return
	}
	c := a.chat()
	if strings.HasPrefix(text, "/") {
		a.command(ctx, text)
		return
	}
	if c == nil {
		a.setNotice("no chat selected; /new or /chats")
		return
	}
	if cmd, ok := strings.CutPrefix(text, "!"); ok && strings.TrimSpace(cmd) != "" {
		// A shell command by the person, not the agent: the card arrives
		// over the stream; the request runs off the loop since a command
		// may take up to a minute.
		a.shell(ctx, c.ID, strings.TrimSpace(cmd))
		return
	}
	if note, ok := strings.CutPrefix(text, "#"); ok && strings.TrimSpace(note) != "" {
		if err := a.Client.Memory(ctx, c.ID, strings.TrimSpace(note)); err != nil {
			a.setNotice(err.Error())
			a.editor.Set(text)
			return
		}
		a.setNotice("added to CLAUDE.md")
		return
	}
	pending := c.Pending()
	if len(pending) > 0 {
		first := pending[0]
		if p := first.Permission(); p != nil {
			if a.answerPermission(ctx, c.ID, first, p, text) {
				return
			}
		}
		switch strings.ToLower(text) {
		case "y", "yes", "allow":
			a.resolve(ctx, c.ID, first, true, nil)
			return
		case "n", "no", "decline", "deny":
			a.resolve(ctx, c.ID, first, false, nil)
			return
		}
		if qs := first.Questions(); len(qs) > 0 {
			answers := map[string][]string{}
			for _, q := range qs {
				answers[q.ID] = []string{text}
			}
			a.resolve(ctx, c.ID, first, true, answers)
			return
		}
	}
	a.sendMessage(ctx, c, text)
}

// sendMessage sends text to chat c with the files waiting to go with it;
// the draft comes back if the service refuses.
func (a *App) sendMessage(ctx context.Context, c *Chat, text string) {
	var ids []string
	for _, at := range a.attachments[c.ID] {
		ids = append(ids, at.ID)
	}
	if err := a.Client.Message(ctx, c.ID, text, NewMessageID(), ids...); err != nil {
		a.setNotice(err.Error())
		a.editor.Set(text) // keep what was typed
		return
	}
	delete(a.attachments, c.ID)
}

// shell runs a "!" command in the chat's workspace in the background and
// reports how it ended when it does; the command card itself arrives with
// the state stream (running, then with its output).
func (a *App) shell(ctx context.Context, chatID, command string) {
	a.setNotice("running in the workspace: " + truncate(command, 60))
	go func() {
		result, err := a.Client.Exec(ctx, chatID, command)
		report := func(context.Context) {
			switch {
			case err != nil:
				a.setNotice(err.Error())
			case result.TimedOut:
				a.setNotice("command timed out after 60s")
			case result.ExitCode != 0:
				a.setNotice(fmt.Sprintf("command exited %d", result.ExitCode))
			default:
				a.setNotice("command finished")
			}
		}
		select {
		case a.later <- report:
		case <-ctx.Done():
		}
	}()
}

// answerPermission reads a typed answer to a tool ask: y allows, a allows
// always, n denies with the rest of the line as the message to the model;
// for a plan, y approves into auto, a approves into ask, n keeps planning
// with the rest of the line as feedback. Other text is not an answer.
func (a *App) answerPermission(ctx context.Context, chatID string, ap Approval, p *Permission, text string) bool {
	word, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	rest = strings.TrimSpace(rest)
	var allow, always bool
	message, mode, said := "", "", ""
	switch strings.ToLower(word) {
	case "y", "yes", "allow", "approve":
		allow = true
		if p.IsPlan() {
			mode, said = "auto", "plan approved; mode auto"
		} else {
			said = "allowed"
		}
	case "a", "always":
		allow = true
		if p.IsPlan() {
			mode, said = "ask", "plan approved; mode ask"
		} else {
			always, said = true, "allowed always: "+p.Always
		}
	case "n", "no", "deny", "decline":
		message = rest
		if p.IsPlan() {
			said = "kept planning"
		} else {
			said = "denied"
		}
		if message != "" {
			said += " with a message"
		}
	default:
		return false
	}
	if err := a.Client.Answer(ctx, chatID, ap.ID, allow, always, message, mode); err != nil {
		a.setNotice(err.Error())
	} else {
		a.setNotice(said)
	}
	return true
}

// modes are the permission modes in the order Shift+Tab cycles them.
var modes = []string{"auto", "ask", "plan"}

// cycleMode moves a Claude chat to the next permission mode.
func (a *App) cycleMode(ctx context.Context) {
	c := a.chat()
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	if c.Provider != "claude" {
		a.setNotice("permission modes apply to Claude chats")
		return
	}
	next := modes[0]
	for i, m := range modes {
		if m == orMode(c.Mode) {
			next = modes[(i+1)%len(modes)]
		}
	}
	a.setMode(ctx, c, next)
}

func (a *App) setMode(ctx context.Context, c *Chat, mode string) {
	if err := a.Client.Mode(ctx, c.ID, mode); err != nil {
		a.setNotice(err.Error())
		return
	}
	a.setNotice("permission mode " + mode + ": " + modeHint(mode))
}

// setSetting handles /thinking, /effort and /fast: with no argument it
// says what the chat has, otherwise it sends the change (the service
// validates and refuses what this Warden does not offer).
func (a *App) setSetting(ctx context.Context, c *Chat, name, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	if c.Provider != "claude" {
		a.setNotice("thinking, effort and fast mode apply to Claude chats")
		return
	}
	change := map[string]any{}
	switch name {
	case "thinking":
		if arg == "" {
			a.setNotice("thinking " + thinkingLabel(c.Thinking) + " · /thinking on|off|TOKENS (8k)")
			return
		}
		v, err := chats.ParseThinking(arg)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		change["thinking"] = v
	case "effort":
		if arg == "" {
			a.setNotice("effort " + orDefault(c.Effort) + " · /effort " + strings.Join(chats.Efforts, "|") + "|default")
			return
		}
		if arg == "default" {
			arg = ""
		}
		if !chats.ValidEffort(arg) {
			a.setNotice("/effort " + strings.Join(chats.Efforts, "|") + "|default")
			return
		}
		change["effort"] = arg
	case "fast":
		switch arg {
		case "":
			state := "off"
			if c.Fast {
				state = "on"
			}
			if c.Session != nil && c.Session.FastMode != "" {
				state += " (session: " + c.Session.FastMode + ")"
			}
			a.setNotice("fast mode " + state + " · /fast on|off")
			return
		case "on", "off":
			change["fast"] = arg == "on"
		default:
			a.setNotice("/fast on|off")
			return
		}
	}
	if err := a.Client.Settings(ctx, c.ID, change); err != nil {
		a.setNotice(err.Error())
		return
	}
	for k, v := range change {
		switch k {
		case "thinking":
			a.setNotice("thinking " + thinkingLabel(v.(string)))
		case "effort":
			a.setNotice("effort " + orDefault(v.(string)))
		case "fast":
			if v.(bool) {
				a.setNotice("fast mode on: faster answers at a higher price, on the models that offer it")
			} else {
				a.setNotice("fast mode off")
			}
		}
	}
}

// thinkingLabel words a thinking setting: default, off, or the budget
// (8k for 8000).
func thinkingLabel(setting string) string {
	switch setting {
	case "":
		return "default (the model decides)"
	case "off":
		return "off"
	}
	if n, err := strconv.Atoi(setting); err == nil && n >= 1000 && n%1000 == 0 {
		return strconv.Itoa(n/1000) + "k tokens"
	}
	return setting + " tokens"
}

func orMode(mode string) string {
	if mode == "" {
		return "auto"
	}
	return mode
}

func modeHint(mode string) string {
	switch mode {
	case "ask":
		return "Claude asks before commands that write and before file edits"
	case "plan":
		return "Claude explores and proposes a plan; edits wait for its approval"
	}
	return "every tool call is allowed"
}

func (a *App) resolve(ctx context.Context, chatID string, ap Approval, allow bool, answers map[string][]string) {
	if err := a.Client.Resolve(ctx, chatID, ap.ID, allow, answers); err != nil {
		a.setNotice(err.Error())
		return
	}
	if allow {
		a.setNotice("allowed")
	} else {
		a.setNotice("declined")
	}
}

// sortedChats lists non-archived chats, most recently created last, as the
// web sidebar does (the store keeps creation order).
func (a *App) sortedChats() []*Chat {
	if a.state == nil {
		return nil
	}
	var out []*Chat
	for _, c := range a.state.Chats {
		if !c.Archived {
			out = append(out, c)
		}
	}
	return out
}

// refreshState fetches the state now rather than waiting for the stream,
// so a command's effect shows in the same frame.
func (a *App) refreshState(ctx context.Context) {
	if s, err := a.Client.State(ctx); err == nil {
		a.state = s
	}
}

// providerCommands are the agent's own slash commands the menu knows for
// a chat's provider: their argument and hint (the chat's reported list
// carries only a description for the workspace's own), and the menu until
// the chat has reported its list. Sent as text, the agent expands them.
var providerCommands = map[string][]Command{
	"claude": {{"compact", "[INSTRUCTIONS]", "replace the history with a summary; say what to keep"}},
}

// chatCommands lists the commands the chat itself offers: what ChatCommands
// says when set, else the chat's reported list (with the provider's
// argument and hint where known), else the provider's known ones.
func (a *App) chatCommands(c *Chat) []Command {
	if c == nil {
		return nil
	}
	if a.ChatCommands != nil {
		return a.ChatCommands(c)
	}
	known := providerCommands[c.Provider]
	if len(c.Commands) == 0 {
		return known
	}
	var out []Command
	for _, ac := range c.Commands {
		cmd := Command{Name: ac.Name, Hint: ac.Description}
		for _, k := range known {
			if k.Name == ac.Name {
				cmd.Arg = k.Arg
				if cmd.Hint == "" {
					cmd.Hint = k.Hint
				}
			}
		}
		out = append(out, cmd)
	}
	return out
}

func (a *App) command(ctx context.Context, line string) {
	name, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	name = strings.ToLower(name)
	arg = strings.TrimSpace(arg)
	c := a.chat()
	switch name {
	case "help", "?":
		a.setNotice(helpText)
	case "quit", "exit", "q":
		a.quit = true
	case "chats":
		var b strings.Builder
		for i, ch := range a.sortedChats() {
			mark := " "
			if c != nil && ch.ID == c.ID {
				mark = "*"
			}
			b.WriteString(mark + ChatLine(i+1, ch) + "\n")
		}
		if b.Len() == 0 {
			b.WriteString("no chats; /new [title]")
		}
		a.setNotice(strings.TrimRight(b.String(), "\n"))
	case "switch":
		n, err := strconv.Atoi(arg)
		chats := a.sortedChats()
		if err != nil || n < 1 || n > len(chats) {
			a.setNotice("/switch N with N from /chats")
			return
		}
		a.selectChat(chats[n-1].ID)
		a.setNotice("switched to " + sanitize(chats[n-1].Title))
	case "new":
		title := arg
		if title == "" {
			title = "Terminal chat " + a.now().Format("Jan 2 15:04")
		}
		provider := a.Provider
		if c != nil && provider == "" {
			provider = c.Provider
		}
		if provider == "" {
			provider = "codex"
		}
		id, err := a.Client.Create(ctx, title, provider, "", "", nil)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		a.refreshState(ctx)
		a.selectChat(id)
		a.setNotice("new chat " + sanitize(title) + " (" + provider + ")")
	case "rename":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		if arg == "" {
			a.setNotice("/rename TITLE")
			return
		}
		if err := a.Client.Edit(ctx, c.ID, arg, c.Archived); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.refreshState(ctx)
		a.setNotice("renamed to " + sanitize(arg))
	case "archive", "restore":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		archive := name == "archive"
		if err := a.Client.Edit(ctx, c.ID, c.Title, archive); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.refreshState(ctx)
		if archive {
			a.setNotice("archived; /restore brings it back, /chats lists the others")
		} else {
			a.setNotice("restored")
		}
	case "delete":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		if c.SandboxID == "" {
			a.setNotice("this chat has no workspace yet; /archive it instead")
			return
		}
		if c.Running() {
			a.setNotice("stop the chat first (Esc)")
			return
		}
		id, title := c.ID, sanitize(c.Title)
		siblings := 0
		for _, ch := range a.state.Chats {
			if ch.SandboxID == c.SandboxID && ch.ID != c.ID && !ch.Archived {
				siblings++
			}
		}
		others := ""
		if siblings > 0 {
			others = fmt.Sprintf(" and its %d other chat(s)", siblings)
		}
		a.confirm = &confirmation{
			prompt: fmt.Sprintf("Delete the workspace of %q%s? This stops its sandbox, deletes its files, archives its chats and cannot be undone. Type y and Enter to confirm; anything else cancels", title, others),
			run: func(ctx context.Context) {
				ch := a.state.Chat(id)
				if ch == nil {
					return
				}
				if err := a.Client.DeleteEnvironment(ctx, ch.SandboxID); err != nil {
					a.setNotice(err.Error())
					return
				}
				a.refreshState(ctx)
				a.setNotice("workspace deleted; its chats are archived")
			},
		}
	case "stop":
		if c == nil {
			return
		}
		if err := a.Client.Stop(ctx, c.ID); err != nil {
			a.setNotice(err.Error())
		} else {
			a.setNotice("stopping")
		}
	case "model", "provider":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		provider, model := c.Provider, c.Model
		if name == "model" {
			model = arg
		} else {
			provider = arg
		}
		if err := a.Client.Agent(ctx, c.ID, provider, model); err != nil {
			a.setNotice(err.Error())
		} else if name == "model" && provider == "claude" {
			// A Claude chat's live session takes the model now; the
			// status line shows what it resolved after the next turn.
			a.setNotice(fmt.Sprintf("model %s · a running session switches now, otherwise the next run", orDefault(model)))
		} else {
			a.setNotice(fmt.Sprintf("next run uses %s · %s", provider, orDefault(model)))
		}
	case "mode":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		switch arg {
		case "":
			a.setNotice("permission mode " + orMode(c.Mode) + ": " + modeHint(orMode(c.Mode)) + " · /mode auto|ask|plan, Shift+Tab cycles")
		case "auto", "ask", "plan":
			a.setMode(ctx, c, arg)
		default:
			a.setNotice("/mode auto|ask|plan")
		}
	case "thinking", "effort", "fast":
		a.setSetting(ctx, c, name, arg)
	case "attach":
		a.attach(ctx, c, arg)
	case "attachments":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		list := a.attachments[c.ID]
		if len(list) == 0 {
			a.setNotice("no files waiting; /attach PATH adds one")
			return
		}
		var b strings.Builder
		for i, at := range list {
			fmt.Fprintf(&b, "%2d  %-32s  %-5s  %s → %s\n", i+1, truncate(sanitize(at.Name), 32), at.Kind, FormatSize(at.Size), at.Path)
		}
		a.setNotice(strings.TrimRight(b.String(), "\n") + "\nsent with the next message; /detach N drops one")
	case "detach":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		list := a.attachments[c.ID]
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(list) {
			a.setNotice("/detach N with N from /attachments")
			return
		}
		at := list[n-1]
		if err := a.Client.RemoveAttachment(ctx, c.ID, at.ID); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.attachments[c.ID] = append(list[:n-1:n-1], list[n:]...)
		a.setNotice("dropped " + sanitize(at.Name))
	case "clear":
		a.editor.Clear()
		a.menu = nil
		if c != nil {
			for _, at := range a.attachments[c.ID] {
				a.Client.RemoveAttachment(ctx, c.ID, at.ID)
			}
			delete(a.attachments, c.ID)
		}
		a.setNotice("draft cleared")
	case "export":
		a.export(c, arg)
	case "rewind":
		a.rewind(ctx, c, arg)
	case "queue":
		a.queue(ctx, c, arg)
	case "withdraw":
		a.withdraw(ctx, c, arg)
	case "edit":
		a.edit(ctx, c, arg)
	case "diff":
		a.showDiff(ctx, c, arg)
	case "instructions":
		a.instructions(ctx, arg)
	case "memory":
		a.memory(ctx, c, arg)
	case "fork":
		a.fork(ctx, c, arg)
	case "btw":
		a.btw(ctx, c, arg)
	case "cost":
		a.cost(c)
	case "style":
		a.style(ctx, c, arg)
	case "bell":
		a.bellCommand(arg)
	case "verbose":
		a.handleKey(ctx, Key{Kind: KeyCtrlO})
	case "open":
		if a.OpenURL == nil || a.AppURL == "" {
			a.setNotice("no browser available")
			return
		}
		if err := a.OpenURL(a.AppURL); err != nil {
			a.setNotice(err.Error())
		} else {
			a.setNotice("opened in the browser")
		}
	case "expand":
		a.expanded = !a.expanded
		a.setNotice(map[bool]string{true: "showing full tool output and diffs", false: "showing the last lines of tool output"}[a.expanded])
	case "find":
		if arg == "" {
			a.setNotice("/find TEXT")
			return
		}
		a.find(arg)
	case "copy":
		if c == nil {
			a.setNotice("no chat selected")
			return
		}
		text := ""
		for i := len(c.Conversation.Entries) - 1; i >= 0; i-- {
			if c.Conversation.Entries[i].Role == "assistant" && strings.TrimSpace(c.Conversation.Entries[i].Text) != "" {
				text = c.Conversation.Entries[i].Text
				break
			}
		}
		switch {
		case text == "":
			a.setNotice("no reply to copy")
		case a.Clipboard == nil:
			a.setNotice("no clipboard available")
		default:
			if err := a.Clipboard(text); err != nil {
				a.setNotice(err.Error())
			} else {
				a.setNotice(fmt.Sprintf("copied the last reply (%d characters)", utf8.RuneCountInString(text)))
			}
		}
	case "preview":
		n, err := strconv.Atoi(arg)
		ports := a.ports()
		if err != nil || n < 1 || n > len(ports) {
			a.setNotice("/preview N with N from /previews")
			return
		}
		if a.OpenURL == nil {
			a.setNotice("no browser available")
			return
		}
		target := ports[n-1].URL
		// The preview needs the browser's Warden session, which the app URL
		// establishes; open the app first, then the preview.
		go func(app, preview string) {
			if app != "" {
				a.OpenURL(app)
				time.Sleep(1500 * time.Millisecond)
			}
			a.OpenURL(preview)
		}(a.AppURL, target)
		a.setNotice("opening " + target)
	case "previews":
		var b strings.Builder
		i := 0
		for _, p := range a.ports() {
			i++
			b.WriteString(fmt.Sprintf("%2d  port %-5d %-24s %s\n", i, p.Port, truncate(sanitize(p.Title), 24), p.URL))
		}
		if i == 0 {
			b.WriteString("no published previews for this chat")
		}
		a.setNotice(strings.TrimRight(b.String(), "\n"))
	case "unpublish":
		n, err := strconv.Atoi(arg)
		ports := a.ports()
		if err != nil || n < 1 || n > len(ports) {
			a.setNotice("/unpublish N with N from /previews")
			return
		}
		if err := a.Client.RevokePort(ctx, ports[n-1].ID); err != nil {
			a.setNotice(err.Error())
		} else {
			a.setNotice("unpublished; the URL now answers 410")
		}
	default:
		// A command the chat itself offers (/compact and the agent's
		// others) goes to the agent as the message, verbatim.
		for _, cmd := range a.chatCommands(c) {
			if cmd.Name == name {
				a.sendMessage(ctx, c, line)
				return
			}
		}
		a.setNotice("unknown command /" + name + "; /help")
	}
}

// instructions shows, edits or clears the person's standing instructions
// (delivered to the agent in every chat they take part in). "edit" loads
// the text into the composer; Enter saves it.
func (a *App) instructions(ctx context.Context, arg string) {
	switch strings.ToLower(arg) {
	case "":
		v, err := a.Client.Instructions(ctx)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		if strings.TrimSpace(v.Text) == "" {
			a.setNotice("no standing instructions; /instructions edit writes them (the agent gets them in every chat you take part in)")
			return
		}
		a.setNotice("your instructions (the agent gets them in every chat you take part in; /instructions edit changes them):\n" + sanitize(v.Text))
	case "edit":
		v, err := a.Client.Instructions(ctx)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		a.menu = nil
		a.editor.Set(v.Text)
		a.editing = &editing{label: "your instructions", save: a.Client.SetInstructions}
		a.setNotice("editing your instructions · Enter saves, Esc cancels")
	case "clear":
		if err := a.Client.SetInstructions(ctx, ""); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.setNotice("instructions cleared")
	default:
		a.setNotice("/instructions [edit|clear]")
	}
}

// memory lists the workspace's memory files, shows one, or loads one into
// the composer to edit (Enter saves). A file is named by its number in
// the last listing or by its label (the path; auto:PATH for an auto-memory
// file); a workspace path that does not exist yet can be edited into
// being (CLAUDE.md, .claude/rules/NAME.md).
func (a *App) memory(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	verb, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	edit := strings.EqualFold(verb, "edit")
	target := arg
	if edit {
		target = rest
	}
	if arg == "" || (edit && target == "") {
		view, err := a.Client.MemoryFiles(ctx, c.ID)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		if a.memoryFiles == nil {
			a.memoryFiles = map[string][]MemoryFile{}
		}
		a.memoryFiles[c.ID] = view.Files
		a.setNotice(memoryListing(view))
		return
	}
	f, ok := a.memoryFile(ctx, c, target)
	if !ok {
		if !edit {
			a.setNotice("no such memory file: " + sanitize(target) + "; /memory lists them")
			return
		}
		// A new workspace file (the service validates the path).
		f = MemoryFile{Scope: "workspace", Path: target}
		if strings.HasPrefix(target, "auto:") {
			f = MemoryFile{Scope: "auto", Path: strings.TrimPrefix(target, "auto:")}
		}
	}
	if !edit {
		a.setNotice(memoryText(f))
		return
	}
	if f.Truncated {
		a.setNotice(f.Label() + " is larger than the view shows; editing it here would cut it")
		return
	}
	scope, path, label := f.Scope, f.Path, f.Label()
	a.menu = nil
	a.editor.Set(f.Text)
	a.editing = &editing{label: label, save: func(ctx context.Context, text string) error {
		return a.Client.WriteMemory(ctx, c.ID, scope, path, text)
	}}
	a.setNotice("editing " + label + " · Enter saves, Esc cancels")
}

// memoryFile finds a file by number in the last listing or by label,
// fetching the listing when there is none yet.
func (a *App) memoryFile(ctx context.Context, c *Chat, target string) (MemoryFile, bool) {
	files, ok := a.memoryFiles[c.ID]
	if !ok {
		view, err := a.Client.MemoryFiles(ctx, c.ID)
		if err != nil {
			return MemoryFile{}, false
		}
		if a.memoryFiles == nil {
			a.memoryFiles = map[string][]MemoryFile{}
		}
		a.memoryFiles[c.ID] = view.Files
		files = view.Files
	}
	if n, err := strconv.Atoi(target); err == nil {
		if n >= 1 && n <= len(files) {
			return files[n-1], true
		}
		return MemoryFile{}, false
	}
	for _, f := range files {
		if f.Label() == target || f.Path == target {
			return f, true
		}
	}
	return MemoryFile{}, false
}

// memoryListing is the /memory notice: one numbered line per file with its
// size, then where they are and whether the agent reads them.
func memoryListing(view MemoryView) string {
	var b strings.Builder
	for i, f := range view.Files {
		fmt.Fprintf(&b, "%2d  %-40s %s\n", i+1, truncate(sanitize(f.Label()), 40), FormatSize(f.Size))
	}
	if len(view.Files) == 0 {
		b.WriteString("no memory files yet; /memory edit CLAUDE.md creates one\n")
	}
	fmt.Fprintf(&b, "workspace: %s", sanitize(view.Root))
	if view.Exists {
		fmt.Fprintf(&b, " · auto-memory: %s", sanitize(view.AutoDir))
	}
	b.WriteString("\n/memory N shows a file · /memory edit N (or a path) edits it")
	if view.Hint != "" {
		b.WriteString("\n" + view.Hint)
	}
	return b.String()
}

// memoryShowLines bounds what /memory FILE prints.
const memoryShowLines = 60

// memoryText is the /memory FILE notice: the file's text, cut after
// memoryShowLines lines with a pointer to editing it.
func memoryText(f MemoryFile) string {
	lines := strings.Split(strings.TrimRight(f.Text, "\n"), "\n")
	head := fmt.Sprintf("%s (%s)", f.Label(), FormatSize(f.Size))
	if len(lines) == 1 && lines[0] == "" {
		return head + ": empty"
	}
	cut := ""
	if len(lines) > memoryShowLines {
		cut = fmt.Sprintf("\n… %d more lines; /memory edit %s opens all of it", len(lines)-memoryShowLines, f.Label())
		lines = lines[:memoryShowLines]
	}
	if f.Truncated {
		cut += "\n(the view shows the first " + FormatSize(int64(len(f.Text))) + " only)"
	}
	return head + ":\n" + sanitize(strings.Join(lines, "\n")) + cut
}

// Attachment limits, the chat service's (chats/attachments.go).
const (
	maxAttachmentBytes = 8 << 20
	maxAttachments     = 8
)

// attach uploads a local file for the next message.
func (a *App) attach(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	arg = strings.Trim(arg, `"'`)
	if arg == "" {
		a.setNotice("/attach PATH (a local file; Tab completes)")
		return
	}
	if len(a.attachments[c.ID]) >= maxAttachments {
		a.setNotice(fmt.Sprintf("a message can carry at most %d attachments; /detach N drops one", maxAttachments))
		return
	}
	path := expandHome(arg)
	info, err := os.Stat(path)
	switch {
	case err != nil:
		a.setNotice(err.Error())
		return
	case info.IsDir():
		a.setNotice(arg + " is a directory; attach a file")
		return
	case info.Size() == 0:
		a.setNotice(arg + " is empty")
		return
	case info.Size() > maxAttachmentBytes:
		a.setNotice(arg + " is larger than 8 MiB")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	at, err := a.Client.Upload(ctx, c.ID, filepath.Base(path), data)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	if a.attachments == nil {
		a.attachments = map[string][]Attachment{}
	}
	a.attachments[c.ID] = append(a.attachments[c.ID], at)
	a.setNotice(fmt.Sprintf("attached %s (%s, %s); it goes with the next message as %s", sanitize(at.Name), at.Kind, FormatSize(at.Size), at.Path))
}

// export writes the transcript: /export [md|json] [all] [FILE].
func (a *App) export(c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	format, all := "md", false
	words := strings.Fields(arg)
	for len(words) > 0 {
		switch strings.ToLower(words[0]) {
		case "md", "markdown":
			format = "md"
		case "json":
			format = "json"
		case "all", "--all", "activity":
			all = true
		default:
			goto named
		}
		words = words[1:]
	}
named:
	file := strings.Join(words, " ")
	now := a.now()
	if file == "" {
		file = ExportName(c.Title, format, now)
	}
	file = expandHome(file)
	if !filepath.IsAbs(file) && a.ExportDir != "" {
		file = filepath.Join(a.ExportDir, file)
	}
	var data []byte
	if format == "json" {
		var err error
		if data, err = ExportJSON(c, all, now); err != nil {
			a.setNotice(err.Error())
			return
		}
		data = append(data, '\n')
	} else {
		data = []byte(ExportMarkdown(c, all, now, nil))
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		a.setNotice(err.Error())
		return
	}
	n := exportCount(exportTree(c, all))
	what := "messages"
	if all {
		what = "entries (tool steps included)"
	}
	a.setNotice(fmt.Sprintf("exported %d %s to %s", n, what, file))
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func (a *App) ports() []Port {
	c := a.chat()
	if c == nil || a.state == nil {
		return nil
	}
	var out []Port
	for _, p := range a.state.Ports {
		if p.ChatID == c.ID && p.State == "approved" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// Frame is what one redraw shows; exposed for tests.
type Frame struct {
	Lines       []string // transcript viewport, exactly the rows available
	Extra       []string // completion menu, waiting attachments, a confirmation: between the transcript and the status
	Status      string
	PromptLines []string // the composer, one screen line each
	CursorRow   int      // cursor position within PromptLines
	CursorCol   int      // in runes, including the prompt marker
}

// visible is the chat as the transcript shows it: without tool steps and
// thinking while they are hidden (Ctrl+O).
func (a *App) visible(c *Chat) *Chat {
	if !a.quiet {
		return c
	}
	v := *c
	v.Conversation.Entries = nil
	for _, e := range c.Conversation.Entries {
		if e.Role != "activity" && e.Role != "thinking" && e.ParentID == "" {
			v.Conversation.Entries = append(v.Conversation.Entries, e)
		}
	}
	return &v
}

// compose lays out everything above the status line for the given width.
func (a *App) compose(width int) []string {
	var body []string
	c := a.chat()
	switch {
	case c == nil && a.state == nil:
		body = []string{dim + "connecting to Warden…" + reset}
	case c == nil:
		body = append(body, bold+"Warden"+reset, "", "no chat selected", "")
		for i, ch := range a.sortedChats() {
			body = append(body, ChatLine(i+1, ch))
		}
		body = append(body, "", dim+"/switch N or /new [title]"+reset)
	default:
		body = RenderTranscript(a.visible(c), width, a.expanded)
		body = append(body, RenderApprovals(c, width)...)
		if a.diff != nil {
			body = append(body, "")
			body = append(body, RenderChanges(a.diff, width, a.expanded)...)
		}
	}
	// Notices sit under the transcript for a while.
	if a.notice != "" && a.now().Sub(a.noticeAt) < 20*time.Second {
		body = append(body, "")
		for _, l := range strings.Split(a.notice, "\n") {
			body = append(body, wrap(l, width, blue+"› "+reset, "  ")...)
		}
	}
	return body
}

// menuRows is how many suggestions the menu shows at once.
const menuRows = 8

// extraLines renders what sits between the transcript and the status
// line: the completion menu, the files waiting to be sent, a confirmation.
func (a *App) extraLines(width int) []string {
	var out []string
	if m := a.menu; m != nil {
		start := 0
		if m.Selected >= menuRows {
			start = m.Selected - menuRows + 1
		}
		end := min(len(m.Items), start+menuRows)
		labelWidth := 0
		for _, it := range m.Items[start:end] {
			labelWidth = max(labelWidth, utf8.RuneCountInString(it.Label))
		}
		labelWidth = min(labelWidth, max(10, width/2))
		for i := start; i < end; i++ {
			it := m.Items[i]
			label := truncate(it.Label, labelWidth)
			line := "  " + label + strings.Repeat(" ", labelWidth-utf8.RuneCountInString(label))
			if it.Hint != "" {
				line += "  " + dim + it.Hint + reset
			}
			if i == m.Selected {
				line = "\x1b[7m" + "▸ " + label + strings.Repeat(" ", labelWidth-utf8.RuneCountInString(label)) + reset
				if it.Hint != "" {
					line += "  " + dim + it.Hint + reset
				}
			}
			out = append(out, line)
		}
		note := m.Note
		if note == "" {
			if len(m.Items) > end-start {
				note = fmt.Sprintf("%d of %d · ", m.Selected+1, len(m.Items))
			}
			note += "Tab or Enter accepts · Esc closes"
		}
		out = append(out, dim+"  "+note+reset)
	}
	if c := a.chat(); c != nil && len(a.attachments[c.ID]) > 0 {
		var names []string
		for i, at := range a.attachments[c.ID] {
			names = append(names, fmt.Sprintf("%d %s (%s)", i+1, sanitize(at.Name), FormatSize(at.Size)))
		}
		out = append(out, wrap(strings.Join(names, " · "), width, cyan+"attached: "+reset, "          ")...)
	}
	if q := a.confirm; q != nil {
		out = append(out, wrap(q.prompt, width, bold+yellow+"? "+reset, "  ")...)
	}
	if ed := a.editing; ed != nil {
		out = append(out, wrap("editing "+ed.label+" · Enter saves · Alt+Enter (or Ctrl+J) inserts a line · Esc cancels", width, bold+cyan+"✎ "+reset, "  ")...)
	}
	return out
}

// promptLines renders the composer: the first line carries the marker, a
// buffer with more lines than fit shows the lines around the cursor. A
// reverse search shows its query and match instead.
func (a *App) promptLines(width, maxLines int) (lines []string, row, col int) {
	if s := a.search; s != nil {
		match := ""
		if h := a.editor.History(); s.index >= 0 && s.index < len(h) {
			match = strings.ReplaceAll(h[s.index], "\n", "⏎")
		} else if s.query != "" {
			match = dim + "no match" + reset
		}
		line := fmt.Sprintf("(reverse-i-search)'%s': %s", s.query, match)
		return []string{line}, 0, min(width-1, utf8.RuneCountInString("(reverse-i-search)'"+s.query+"': "))
	}
	raw, r, c := a.editor.Lines()
	for i, l := range raw {
		marker := "› "
		if i > 0 {
			marker = "  "
		}
		lines = append(lines, marker+l)
	}
	col = c + 2
	if len(lines) > maxLines {
		start := r - maxLines + 1
		if start < 0 {
			start = 0
		}
		if start > len(lines)-maxLines {
			start = len(lines) - maxLines
		}
		lines = lines[start : start+maxLines]
		r -= start
	}
	if col >= width {
		col = width - 1
	}
	return lines, r, col
}

// frame composes the screen for the given size.
func (a *App) frame(width, height int) Frame {
	if width < 20 {
		width = 20
	}
	if height < 8 {
		height = 8
	}
	body := a.compose(width)
	extra := a.extraLines(width)
	if len(extra) > height/2 {
		extra = extra[:height/2]
	}
	prompt, cursorRow, cursorCol := a.promptLines(width, min(6, height/3))
	rows := height - 1 - len(prompt) - len(extra) // status + composer
	if rows < 1 {
		rows = 1
	}
	a.rows = rows
	if a.scroll > len(body)-rows {
		a.scroll = max(0, len(body)-rows)
	}
	end := len(body) - a.scroll
	start := max(0, end-rows)
	view := body[start:end]
	for len(view) < rows {
		view = append(view, "")
	}
	status := ""
	c := a.chat()
	if c != nil {
		status = StatusLine(c, a.stateports(), a.live, a.now())
		if a.quiet {
			status += "  " + dim + "steps hidden" + reset
		}
		if a.scroll > 0 {
			status += fmt.Sprintf("  %s↑ %d lines below · End to follow%s", yellow, a.scroll, reset)
		}
		status += "  " + dim + "/help" + reset
	} else if a.state != nil {
		status = dim + "Warden · no chat selected · /help" + reset
	}
	return Frame{Lines: view, Extra: extra, Status: status, PromptLines: prompt, CursorRow: cursorRow, CursorCol: cursorCol}
}

func (a *App) stateports() []Port {
	if a.state == nil {
		return nil
	}
	return a.state.Ports
}

// draw paints the frame. It always repaints fully; snapshots are small and
// the terminal is local.
func (a *App) draw() {
	width, height := 100, 30
	if a.Size != nil {
		if w, h := a.Size(); w > 0 && h > 0 {
			width, height = w, h
		}
	}
	f := a.frame(width, height)
	a.setTitle()
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	if a.redraw {
		b.WriteString("\x1b[2J")
		a.redraw = false
	}
	b.WriteString("\x1b[H")
	for _, l := range f.Lines {
		b.WriteString(clip(l, width) + "\x1b[K\r\n")
	}
	for _, l := range f.Extra {
		b.WriteString(clip(l, width) + "\x1b[K\r\n")
	}
	b.WriteString(clip(f.Status, width) + "\x1b[K\r\n")
	for i, l := range f.PromptLines {
		b.WriteString(clip(l, width) + "\x1b[K")
		if i < len(f.PromptLines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J") // clear anything left below a shrinking composer
	// Place the cursor inside the composer (rows are 1-based).
	row := len(f.Lines) + len(f.Extra) + 1 + f.CursorRow + 1
	b.WriteString(fmt.Sprintf("\x1b[%d;%dH\x1b[?25h", row, f.CursorCol+1))
	io.WriteString(a.Output, b.String())
}

// clip cuts a styled line to width visible runes, ignoring escape sequences.
func clip(s string, width int) string {
	var b strings.Builder
	visible := 0
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
			b.WriteRune(r)
		default:
			if visible >= width {
				continue
			}
			b.WriteRune(r)
			visible++
		}
	}
	return b.String() + reset
}

// find scrolls so the nearest earlier line containing term (case-insensitive)
// is at the top of the view, searching upward from what is visible. A
// match the rendered lines hide — inside a subagent's collapsed card,
// past a card's folded output, in a step Ctrl+O hides — is reached by
// expanding the transcript (Tab) or showing the steps again first, when
// the chat's entries themselves contain the term.
func (a *App) find(term string) {
	width := 100
	if a.Size != nil {
		if w, _ := a.Size(); w > 0 {
			width = w
		}
	}
	rows := a.rows
	if rows <= 0 {
		rows = 20
	}
	needle := strings.ToLower(term)
	body := a.compose(width)
	top := len(body) - a.scroll - rows // index of the first visible line
	if i := findAbove(body, needle, top); i < 0 && findOnScreen(body, needle, top) < 0 {
		// Not on the screen: is it in the entries at all?
		if c := a.chat(); c != nil && entriesContain(c.Conversation.Entries, needle) {
			opened := []string{}
			if a.quiet {
				a.quiet = false
				opened = append(opened, "steps shown")
			}
			if !a.expanded {
				a.expanded = true
				opened = append(opened, "output expanded")
			}
			if len(opened) > 0 {
				body = a.compose(width)
				top = len(body) - a.scroll - rows
				if i := findAbove(body, needle, top); i >= 0 {
					a.scroll = max(0, len(body)-rows-i)
					a.setNotice(fmt.Sprintf("found %q %d lines up (%s); /find again for the previous one", term, len(body)-i, strings.Join(opened, ", ")))
					return
				}
				if findOnScreen(body, needle, top) >= 0 {
					a.setNotice(fmt.Sprintf("%q is on screen (%s)", term, strings.Join(opened, ", ")))
					return
				}
			}
		}
	}
	if i := findAbove(body, needle, top); i >= 0 {
		a.scroll = max(0, len(body)-rows-i)
		a.setNotice(fmt.Sprintf("found %q %d lines up; /find again for the previous one", term, len(body)-i))
		return
	}
	// Nothing above: say whether it is on screen, so a search that "fails"
	// on a visible match is not confusing.
	if findOnScreen(body, needle, top) >= 0 {
		a.setNotice(fmt.Sprintf("%q is on screen; nothing earlier matches", term))
		return
	}
	a.setNotice(fmt.Sprintf("%q not found; End then /find searches from the bottom", term))
}

// findAbove is the index of the nearest line above top containing needle
// (lower-cased), -1 for none.
func findAbove(body []string, needle string, top int) int {
	for i := min(top-1, len(body)-1); i >= 0; i-- {
		if strings.Contains(strings.ToLower(plainText(body[i])), needle) {
			return i
		}
	}
	return -1
}

// findOnScreen is the index of the first line from top on containing
// needle, -1 for none.
func findOnScreen(body []string, needle string, top int) int {
	for i := max(top, 0); i < len(body); i++ {
		if strings.Contains(strings.ToLower(plainText(body[i])), needle) {
			return i
		}
	}
	return -1
}

// entriesContain says whether any entry's text or detail (a step's
// output, a side question's answer) contains needle, a subagent's nested
// entries and the person's own commands included — what /find can reach
// once the transcript shows everything.
func entriesContain(entries []Entry, needle string) bool {
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Text), needle) {
			return true
		}
		if (e.Role == "activity" || e.Role == "aside") && strings.Contains(strings.ToLower(e.Detail), needle) {
			return true
		}
	}
	return false
}

// plainText strips styling for searching.
func plainText(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
