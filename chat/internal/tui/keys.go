package tui

import (
	"context"
	"fmt"
	"strings"
)

// Every key the terminal client answers to, in one table
// (docs/claude-parity.md, R2.20): handleKey dispatches the composer's
// keys through it and /keys prints it, so what the help says and what the
// keys do cannot drift. Rows without a handler describe what another
// piece does with the key — the Editor's own line editing (editor.go),
// the / and @ menu (menuKey), the prompt search (searchKey) — or what is
// typed rather than pressed (the answers to a card, the composer's
// prefixes); keys_test.go checks that every key those pieces consume is
// a row of the right mode. The web's shortcuts.ts is the same idea.

const (
	areaComposer   = "composer"
	areaTranscript = "transcript"
	areaApprovals  = "approvals"
	areaNavigation = "navigation"
	areaVim        = "vim"
)

// A binding's mode says who handles the key: "" the app (run), modeEditor
// the Editor, modeMenu the completion menu, modeSearch the prompt search,
// modeText something typed and sent, modePrefix a draft's first character.
const (
	modeEditor = "editor"
	modeMenu   = "menu"
	modeSearch = "search"
	modeText   = "text"
	modePrefix = "prefix"
)

type binding struct {
	kinds []KeyKind // the key kinds it answers to (nil for modeText and modePrefix)
	keys  string    // as /keys prints them
	area  string
	what  string
	when  string
	mode  string
	run   func(a *App, ctx context.Context, k Key, c *Chat)
}

// keyBindings is the table, in the order /keys prints the rows within
// each area. A key kind the app handles appears once with a run; the
// same kind may be listed again for another mode (Up moves in the menu
// and recalls a prompt in the composer). Filled in init: the handlers
// reach /keys through the command dispatch, which a package-level
// initializer may not depend on.
var keyBindings []binding

func init() {
	keyBindings = []binding{
		// The composer.
		{kinds: []KeyKind{KeyEnter}, keys: "Enter", area: areaComposer, what: "sends the draft: a message, a /command, !cmd, #note, or an answer to a card", run: (*App).keyEnter},
		{kinds: []KeyKind{KeyNewline}, keys: "Alt+Enter, Ctrl+J", area: areaComposer, what: "a line break in the draft (a paste keeps its newlines)", mode: modeEditor},
		{kinds: []KeyKind{KeyUp}, keys: "Up", area: areaComposer, what: "edits your last queued message in its place", when: "empty draft, a message of yours queued", run: (*App).keyUp},
		{kinds: []KeyKind{KeyUp, KeyDown}, keys: "Up / Down", area: areaComposer, what: "the line above / below; at the draft's edge the previous / next prompt", mode: modeEditor},
		{kinds: []KeyKind{KeyCtrlP, KeyCtrlN}, keys: "Ctrl+P / Ctrl+N", area: areaComposer, what: "the previous / next prompt", mode: modeEditor},
		{kinds: []KeyKind{KeyCtrlR}, keys: "Ctrl+R", area: areaComposer, what: "searches your earlier prompts", run: (*App).keyCtrlR},
		{kinds: []KeyKind{KeyEscape}, keys: "Esc Esc", area: areaComposer, what: "edits your last message: the conversation rewinds to before it", when: "empty draft, agent idle", run: (*App).keyEscape},
		{kinds: []KeyKind{KeyTab}, keys: "Tab", area: areaComposer, what: "completes a / command or an @ path (with one match); on an empty draft expands or folds tool output and diffs", run: (*App).keyTab},
		{kinds: []KeyKind{KeyLeft, KeyRight}, keys: "Left / Right", area: areaComposer, what: "move the cursor", mode: modeEditor},
		{kinds: []KeyKind{KeyHome, KeyEnd, KeyCtrlA, KeyCtrlE}, keys: "Home / End, Ctrl+A / Ctrl+E", area: areaComposer, what: "the line's start / end", mode: modeEditor},
		{kinds: []KeyKind{KeyEnd}, keys: "End", area: areaTranscript, what: "reprints the chat so the terminal's view is at its end (/bottom too)", when: "empty draft", run: (*App).keyEnd},
		{kinds: []KeyKind{KeyCtrlP}, keys: "Ctrl+P", area: areaComposer, what: "previews the long paste under the cursor (/paste N prints it)", when: "on a [Pasted text #N] placeholder"},
		{kinds: []KeyKind{KeyBackspace, KeyDelete}, keys: "Backspace / Delete", area: areaComposer, what: "delete before / after the cursor", mode: modeEditor},
		{kinds: []KeyKind{KeyCtrlU, KeyCtrlK}, keys: "Ctrl+U / Ctrl+K", area: areaComposer, what: "delete to the line's start / end", mode: modeEditor},
		{kinds: []KeyKind{KeyCtrlW}, keys: "Ctrl+W, Alt+Backspace", area: areaComposer, what: "delete the word before the cursor", mode: modeEditor},
		{kinds: []KeyKind{KeyCtrlC}, keys: "Ctrl+C", area: areaComposer, what: "clears the draft (or leaves an edit); twice in a row quits", run: (*App).keyCtrlC},
		{kinds: []KeyKind{KeyCtrlD, KeyEOF}, keys: "Ctrl+D", area: areaComposer, what: "quits on an empty draft; with a draft deletes after the cursor", run: (*App).keyCtrlD},
		{keys: "/", area: areaComposer, what: "a command (the menu opens; /help lists them, /btw QUESTION asks a side question)", mode: modePrefix},
		{keys: "@", area: areaComposer, what: "a workspace path, completed from the menu", mode: modePrefix},
		{keys: "!cmd", area: areaComposer, what: "runs a shell command in the workspace as you, not the agent", mode: modePrefix},
		{keys: "#note", area: areaComposer, what: "appends a bullet to the workspace's CLAUDE.md", mode: modePrefix},
		// The transcript.
		{kinds: []KeyKind{KeyCtrlO}, keys: "Ctrl+O", area: areaTranscript, what: "hides or shows tool steps and thinking", run: (*App).keyCtrlO},
		{kinds: []KeyKind{KeyCtrlL}, keys: "Ctrl+L", area: areaTranscript, what: "clears the screen and reprints the chat", run: (*App).keyCtrlL},
		{kinds: []KeyKind{KeyPageUp, KeyPageDown, KeyWheelUp, KeyWheelDown}, keys: "PgUp / PgDn, wheel", area: areaTranscript, what: "your terminal's own scrolling, search and selection: the transcript is its scrollback", run: (*App).keyScroll},
		// Approvals and the permission mode.
		{keys: "y, yes", area: areaApprovals, what: "allows the first pending ask; a plan: approves it (mode auto)", mode: modeText},
		{keys: "a", area: areaApprovals, what: "allows always for this chat; a plan: approves it, asks before edits", mode: modeText},
		{keys: "A", area: areaApprovals, what: "allows always for this workspace", mode: modeText},
		{keys: "n [message]", area: areaApprovals, what: "denies, with a message if given; a plan: keeps planning with the feedback", mode: modeText},
		{keys: "typed text", area: areaApprovals, what: "answers a question the agent asked", mode: modeText},
		{kinds: []KeyKind{KeyShiftTab}, keys: "Shift+Tab", area: areaApprovals, what: "cycles a Claude chat's permission mode: auto → ask → plan", run: (*App).keyShiftTab},
		{kinds: []KeyKind{KeyEscape}, keys: "Esc", area: areaApprovals, what: "interrupts the agent's turn (queued messages are held); cancels a confirmation or an edit", when: "agent running"},
		// Navigation: the menus.
		{kinds: []KeyKind{KeyUp, KeyDown, KeyCtrlP, KeyCtrlN}, keys: "Up / Down, Ctrl+P / Ctrl+N", area: areaNavigation, what: "move in the / and @ menu", when: "menu open", mode: modeMenu},
		{kinds: []KeyKind{KeyTab, KeyEnter}, keys: "Tab, Enter", area: areaNavigation, what: "take the highlighted item (Enter also sends a completed command)", when: "menu open", mode: modeMenu},
		{kinds: []KeyKind{KeyEscape}, keys: "Esc", area: areaNavigation, what: "puts the menu away until the draft changes", when: "menu open", mode: modeMenu},
		{kinds: []KeyKind{KeyRune, KeyBackspace}, keys: "typing", area: areaNavigation, what: "narrows the prompt search", when: "Ctrl+R search open", mode: modeSearch},
		{kinds: []KeyKind{KeyCtrlR}, keys: "Ctrl+R", area: areaNavigation, what: "the next older match", when: "Ctrl+R search open", mode: modeSearch},
		{kinds: []KeyKind{KeyEnter, KeyTab, KeyLeft, KeyRight, KeyHome, KeyEnd, KeyUp, KeyDown}, keys: "Enter, Tab, arrows", area: areaNavigation, what: "put the match in the composer", when: "Ctrl+R search open", mode: modeSearch},
		{kinds: []KeyKind{KeyEscape, KeyCtrlG, KeyCtrlC}, keys: "Esc, Ctrl+G, Ctrl+C", area: areaNavigation, what: "leave the prompt search", when: "Ctrl+R search open", mode: modeSearch},
		{keys: "/keys", area: areaNavigation, what: "this list · /help lists the commands", mode: modeText},
	}
	extraKeys = vimRows
}

// vimRows are vim mode's rows (vim.go; round 2 F): normal mode's motions,
// operators and commands, listed whether or not the mode is on (the
// heading says how to turn it on); keys_test.go checks every rune and
// command vim.go switches on is in one of them.
func vimRows(a *App) []binding {
	on := "vim on (/vim on)"
	return []binding{
		{keys: "Esc", area: areaVim, what: "enters normal mode (-- NORMAL --) from insert mode; in normal mode with nothing pending, the app's Esc", when: on, mode: modeText},
		{keys: "i a I A o O", area: areaVim, what: "insert: here, after the cursor, at the line's start, at its end, on a new line below, above", when: on, mode: modeText},
		{keys: "h j k l w b e 0 ^ $ gg G", area: areaVim, what: "motions, with a count (5w); arrows move too; G on an empty draft reprints to the end", when: on, mode: modeText},
		{keys: "d c y + motion, dd cc yy, D C", area: areaVim, what: "delete / change / yank over a motion, the whole line, to the line's end", when: on, mode: modeText},
		{keys: "x X p P", area: areaVim, what: "delete the character under / before the cursor; put the yank after / before it", when: on, mode: modeText},
		{keys: "u Ctrl+R .", area: areaVim, what: "undo, redo, repeat the last change", when: on, mode: modeText},
		{keys: "Enter, :w", area: areaVim, what: "sends the draft", when: on, mode: modeText},
		{keys: ":q :wq :set novim", area: areaVim, what: "quits; sends and quits; turns vim mode off (:set vim says it is on)", when: on, mode: modeText},
		{keys: "/TEXT n", area: areaVim, what: "searches the transcript (/find); n repeats it", when: on, mode: modeText},
		{keys: ":", area: areaVim, what: "a command line (Esc leaves it)", when: on, mode: modeText},
	}
}

// extraKeys adds rows to /keys beyond the app's own table: vim mode's
// (vimRows, whose handling lives in vim.go and gates handleKey before
// the table), so /keys stays the one listing. A test may replace it.
var extraKeys func(a *App) []binding

// keyAreas are the areas in the order /keys prints them, with their
// headings.
var keyAreas = []struct{ id, title string }{
	{areaComposer, "composer"},
	{areaTranscript, "transcript"},
	{areaApprovals, "approvals and permission mode (typed answers, then Enter)"},
	{areaNavigation, "menus and search"},
	{areaVim, "vim mode (/vim on; normal mode's keys)"},
}

// bindingFor is the app's own handler for a key kind (mode ""), nil when
// the key is the editor's or unbound.
func bindingFor(kind KeyKind) *binding {
	for i := range keyBindings {
		b := &keyBindings[i]
		if b.mode != "" || b.run == nil {
			continue
		}
		for _, k := range b.kinds {
			if k == kind {
				return b
			}
		}
	}
	return nil
}

// KeysText is the /keys notice: every binding by area, the keys padded
// into a column, the condition in brackets.
func KeysText(rows []binding) string {
	width := 0
	for _, b := range rows {
		width = max(width, len([]rune(b.keys)))
	}
	var out strings.Builder
	for _, area := range keyAreas {
		var lines []string
		for _, b := range rows {
			if b.area != area.id {
				continue
			}
			line := fmt.Sprintf("  %-*s  %s", width, b.keys, b.what)
			if b.when != "" {
				line += " [" + b.when + "]"
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(area.title + "\n" + strings.Join(lines, "\n") + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// keys prints the key table (with the rows a mode adds).
func (a *App) keys() {
	rows := keyBindings
	if extraKeys != nil {
		rows = append(append([]binding(nil), rows...), extraKeys(a)...)
	}
	a.setNotice(KeysText(rows))
}

// The handlers, one per app binding (the switch handleKey used to be).

func (a *App) keyCtrlD(ctx context.Context, k Key, c *Chat) {
	if k.Kind == KeyEOF || a.editor.Text() == "" {
		a.quit = true
		return
	}
	a.editor.Handle(Key{Kind: KeyDelete})
	a.refreshMenu(ctx)
}

func (a *App) keyCtrlC(ctx context.Context, k Key, c *Chat) {
	a.confirm = nil
	now := a.now()
	if ed := a.editing; ed != nil {
		a.editing = nil
		a.editor.Clear()
		a.vim.Reset()
		a.menu = nil
		a.ctrlC = now
		a.setNotice("edit of " + ed.label + " cancelled · Ctrl+C again to quit")
		return
	}
	if a.editor.Text() != "" || a.menu != nil {
		a.editor.Clear()
		a.vim.Reset()
		a.preview = nil
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
}

func (a *App) keyCtrlL(ctx context.Context, k Key, c *Chat) {
	a.redraw = true
}

// keyEscape: the Escape key's work is escape (app.go), which vim's
// normal mode hands over to as well.
func (a *App) keyEscape(ctx context.Context, k Key, c *Chat) {
	a.escape(ctx, c)
}

// keyEnd: with nothing typed, End goes back to the transcript's end (the
// reprint lands the terminal there); while composing it moves the cursor.
func (a *App) keyEnd(ctx context.Context, k Key, c *Chat) {
	if a.editor.Text() == "" {
		a.bottom()
		return
	}
	a.editor.Handle(k)
}

func (a *App) keyCtrlO(ctx context.Context, k Key, c *Chat) {
	a.quiet = !a.quiet
	if a.quiet {
		a.setNotice("hiding tool steps and thinking (Ctrl+O shows them)")
	} else {
		a.setNotice("showing tool steps and thinking")
	}
}

func (a *App) keyCtrlR(ctx context.Context, k Key, c *Chat) {
	a.search = &searchState{index: -1}
}

// keyScroll: the transcript is the terminal's, which scrolls it (these
// keys reach the terminal, not the app, when nothing asks for mouse
// reports; a stray report is dropped here rather than typed).
func (a *App) keyScroll(ctx context.Context, k Key, c *Chat) {}

func (a *App) keyShiftTab(ctx context.Context, k Key, c *Chat) {
	a.cycleMode(ctx)
}

func (a *App) keyTab(ctx context.Context, k Key, c *Chat) {
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
}

func (a *App) keyEnter(ctx context.Context, k Key, c *Chat) {
	a.send(ctx)
}

// keyUp: on an empty draft with a message queued, ↑ edits the last one
// (Claude Code's); otherwise the line above, or the history (the editor).
func (a *App) keyUp(ctx context.Context, k Key, c *Chat) {
	if a.editor.Text() == "" && a.editLastQueued(ctx, c) {
		return
	}
	if a.editor.Handle(k) {
		a.refreshMenu(ctx)
	}
}
