package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

	mu       sync.Mutex
	rang     map[string]bool // approvals already announced with the bell
	state    *State
	live     bool
	editor   Editor
	notice   string
	noticeAt time.Time
	scroll   int  // lines scrolled up from the tail
	rows     int  // transcript rows in the last frame
	expanded bool // show tool output and diffs in full
	quit     bool
}

// page is how far PgUp/PgDn move: a screen minus two lines of context.
func (a *App) page() int {
	if a.rows > 4 {
		return a.rows - 2
	}
	return 10
}

const helpText = `commands   /new [title]   start a chat on a fresh environment
           /chats         list chats      /switch N   open chat N
           /stop          stop the run    /model M    set the model for the next run
           /provider P    codex or claude /open       open this chat in the browser
           /previews      list published previews    /unpublish N
           /preview N     open preview N in the browser
           /find TEXT     scroll to the previous line containing TEXT
           /copy          put the agent's last reply on the clipboard
           /expand        toggle full tool output and diffs (Tab does the same)
           /quit          leave (Ctrl+D)
composer   Enter sends · Alt+Enter (or Ctrl+J) inserts a line break · paste keeps newlines
keys       Enter send · while a run is active a message steers it
           y / n answer the first pending approval; typed text answers a question
           scroll: mouse wheel, Up/Down with an empty composer, PgUp/PgDn,
           Home/End; Ctrl+P/Ctrl+N recall sent messages
           Ctrl+C stop the current run (or quit when idle) · Ctrl+D quit`

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
	go func() {
		buf := make([]byte, 0, 256)
		chunk := make([]byte, 256)
		for {
			n, err := a.Input.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
				decoded, rest := DecodeKeys(buf)
				buf = append(buf[:0], rest...)
				for _, k := range decoded {
					select {
					case keys <- k:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				select {
				case keys <- Key{Kind: KeyCtrlD}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	if s, err := a.Client.State(ctx); err == nil {
		a.state, a.live = s, true
	} else {
		a.setNotice(err.Error())
	}
	// Alternate screen, cursor hidden while painting, and mouse wheel
	// reporting (SGR encoding) so the wheel scrolls the transcript. Text
	// selection then needs the terminal's modifier (Option or Shift).
	fmt.Fprint(a.Output, "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[?2004h")
	defer fmt.Fprint(a.Output, "\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
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
			a.bellForNewApprovals()
			a.draw()
		case k := <-keys:
			a.handleKey(ctx, k)
			a.draw()
		case <-a.Resize:
			a.draw()
		case <-ticker.C:
			a.draw() // clock, spinner, notices ageing
		}
	}
	return nil
}

func (a *App) handleKey(ctx context.Context, k Key) {
	switch k.Kind {
	case KeyCtrlD:
		a.quit = true
	case KeyCtrlC:
		if c := a.chat(); c != nil && (c.Status == "running" || c.Status == "queued") {
			if err := a.Client.Stop(ctx, c.ID); err != nil {
				a.setNotice(err.Error())
			} else {
				a.setNotice("stopping the run")
			}
			return
		}
		a.quit = true
	case KeyCtrlL:
		a.scroll = 0
	case KeyEscape:
		a.scroll = 0
	case KeyPageUp:
		a.scroll += a.page()
	case KeyPageDown:
		a.scroll = max(0, a.scroll-a.page())
	case KeyWheelUp:
		a.scroll += 3
	case KeyWheelDown:
		a.scroll = max(0, a.scroll-3)
	case KeyUp, KeyDown, KeyHome, KeyEnd:
		// With nothing typed these navigate the transcript; while composing
		// they edit (history and cursor). Ctrl+P/Ctrl+N always recall history.
		if a.editor.Text() == "" {
			switch k.Kind {
			case KeyUp:
				a.scroll++
			case KeyDown:
				a.scroll = max(0, a.scroll-1)
			case KeyHome:
				a.scroll = 1 << 30 // clamped to the top by frame
			case KeyEnd:
				a.scroll = 0
			}
			return
		}
		a.editor.Handle(k)
	case KeyTab:
		if a.editor.Text() == "" {
			a.expanded = !a.expanded
			if a.expanded {
				a.setNotice("showing full tool output and diffs (Tab to collapse)")
			} else {
				a.setNotice("showing the last lines of tool output (Tab to expand)")
			}
		}
	case KeyEnter:
		text := a.editor.Submit()
		a.scroll = 0
		if text == "" {
			return
		}
		a.submit(ctx, text)
	default:
		if a.editor.Handle(k) {
			return
		}
	}
}

func (a *App) submit(ctx context.Context, text string) {
	c := a.chat()
	if strings.HasPrefix(text, "/") {
		a.command(ctx, text)
		return
	}
	if c == nil {
		a.setNotice("no chat selected; /new or /chats")
		return
	}
	pending := c.Pending()
	if len(pending) > 0 {
		first := pending[0]
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
	if err := a.Client.Message(ctx, c.ID, text, NewMessageID()); err != nil {
		a.setNotice(err.Error())
		a.editor.Set(text) // keep what was typed
	}
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

func (a *App) command(ctx context.Context, line string) {
	name, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
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
		a.ChatID = chats[n-1].ID
		a.scroll = 0
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
		a.ChatID = id
		a.scroll = 0
		a.setNotice("new chat " + sanitize(title) + " (" + provider + ")")
		if s, err := a.Client.State(ctx); err == nil {
			a.state = s
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
		} else {
			a.setNotice(fmt.Sprintf("next run uses %s · %s", provider, orDefault(model)))
		}
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
		a.setNotice("unknown command /" + name + "; /help")
	}
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
	Status      string
	PromptLines []string // the composer, one screen line each
	CursorRow   int      // cursor position within PromptLines
	CursorCol   int      // in runes, including the prompt marker
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
		body = RenderTranscript(c, width, a.expanded)
		body = append(body, RenderApprovals(c, width)...)
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

// promptLines renders the composer: the first line carries the marker, a
// buffer with more lines than fit shows the lines around the cursor.
func (a *App) promptLines(width, maxLines int) (lines []string, row, col int) {
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

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// runIndicator is the spinner and elapsed time of the current run.
func (a *App) runIndicator(c *Chat) string {
	if c.Status != "running" && c.Status != "queued" && c.Status != "stopping" {
		return ""
	}
	start := 0.0
	for _, e := range c.Conversation.Entries {
		if e.Role == "user" && e.CreatedAt > start {
			start = e.CreatedAt
		}
	}
	now := a.now()
	frame := spinnerFrames[(now.UnixNano()/int64(120*time.Millisecond))%int64(len(spinnerFrames))]
	if start == 0 {
		return yellow + frame + reset
	}
	elapsed := time.Duration(now.Unix()-int64(start)) * time.Second
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("%s%s %s%s", yellow, frame, elapsed.Truncate(time.Second), reset)
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
	prompt, cursorRow, cursorCol := a.promptLines(width, min(6, height/3))
	rows := height - 1 - len(prompt) // status + composer
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
		status = RenderStatus(c, a.stateports(), a.live, width)
		if ind := a.runIndicator(c); ind != "" {
			status += "  " + ind
		}
		if a.scroll > 0 {
			status += fmt.Sprintf("  %s↑ %d lines below · End to follow%s", yellow, a.scroll, reset)
		}
	} else if a.state != nil {
		status = dim + "Warden · no chat selected · /help" + reset
	}
	return Frame{Lines: view, Status: status, PromptLines: prompt, CursorRow: cursorRow, CursorCol: cursorCol}
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
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[H")
	for _, l := range f.Lines {
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
	row := len(f.Lines) + 1 + f.CursorRow + 1
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
			visible += utf8.RuneLen(r) / utf8.RuneLen(r) // 1 per rune
		}
	}
	return b.String() + reset
}

// bellForNewApprovals rings the terminal bell once per approval that
// became pending on the selected chat, so a request is noticed without
// watching the screen.
func (a *App) bellForNewApprovals() {
	c := a.chat()
	if c == nil {
		return
	}
	if a.rang == nil {
		a.rang = map[string]bool{}
	}
	for _, ap := range c.Pending() {
		if !a.rang[ap.ID] {
			a.rang[ap.ID] = true
			fmt.Fprint(a.Output, "\a")
		}
	}
}

// find scrolls so the nearest earlier line containing term (case-insensitive)
// is at the top of the view, searching upward from what is visible.
func (a *App) find(term string) {
	width := 100
	if a.Size != nil {
		if w, _ := a.Size(); w > 0 {
			width = w
		}
	}
	body := a.compose(width)
	rows := a.rows
	if rows <= 0 {
		rows = 20
	}
	top := len(body) - a.scroll - rows // index of the first visible line
	needle := strings.ToLower(term)
	for i := min(top-1, len(body)-1); i >= 0; i-- {
		if strings.Contains(strings.ToLower(plainText(body[i])), needle) {
			a.scroll = len(body) - rows - i
			if a.scroll < 0 {
				a.scroll = 0
			}
			a.setNotice(fmt.Sprintf("found %q %d lines up; /find again for the previous one", term, len(body)-i))
			return
		}
	}
	// Nothing above: say whether it is on screen, so a search that "fails"
	// on a visible match is not confusing.
	for i := max(top, 0); i < len(body); i++ {
		if strings.Contains(strings.ToLower(plainText(body[i])), needle) {
			a.setNotice(fmt.Sprintf("%q is on screen; nothing earlier matches", term))
			return
		}
	}
	a.setNotice(fmt.Sprintf("%q not found; End then /find searches from the bottom", term))
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
