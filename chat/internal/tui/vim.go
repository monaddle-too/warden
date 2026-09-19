package tui

import (
	"strings"
	"unicode"
)

// Vim mode for the composer (docs/claude-parity.md, R2.16): a state
// machine over the Editor's buffer. Insert mode is the editor as it is;
// Esc enters normal mode, where the keys are vim's — motions with counts,
// the operators d c y over them, dd cc yy x X p P, i a I A o O, u and
// Ctrl-R over a snapshot stack, "." for the last change, a ":" line (:w
// sends, :q quits, :wq, :set novim) and "/" to search the transcript.
// Enter in normal mode sends. The app owns the keys that are not the
// composer's (Ctrl-C, Ctrl-D, scrolling, Tab); Handle passes them
// through, and passes every insert-mode key but Esc so that completion,
// paste placeholders and history keep working as without vim.

// VimMode is the state of the machine.
type VimMode int

const (
	VimInsert  VimMode = iota
	VimNormal          // the vim keys
	VimCommand         // the ":" line is being typed
	VimSearch          // the "/" line is being typed
)

// String is the mode as the status bar names it.
func (m VimMode) String() string {
	switch m {
	case VimNormal, VimCommand, VimSearch:
		return "NORMAL"
	}
	return "INSERT"
}

// VimAction is what the app does after a key.
type VimAction int

const (
	VimHandled VimAction = iota // the machine consumed the key
	VimPass                     // the key is the app's, as without vim (insert-mode editing, Ctrl-C, scrolling)
	VimSend                     // send the draft (Enter, :w; Text "quit" for :wq)
	VimQuit                     // leave (:q)
	VimDisable                  // :set novim
	VimFind                     // search the transcript for Text (the "/" line, n)
	VimScroll                   // Text "top" or "bottom": gg or G on an empty draft
	VimEscape                   // Escape with nothing pending: the app's own Escape
	VimNotice                   // show Text
)

// VimResult is a key's outcome.
type VimResult struct {
	Action VimAction
	Text   string
}

type vimSnap struct {
	buf    []rune
	cursor int
}

func snapOf(e *Editor) vimSnap {
	return vimSnap{buf: append([]rune(nil), e.buf...), cursor: e.cursor}
}

func (s vimSnap) restore(e *Editor) {
	e.buf = append([]rune(nil), s.buf...)
	e.cursor = min(s.cursor, len(e.buf))
}

func (s vimSnap) equals(e *Editor) bool {
	if len(s.buf) != len(e.buf) {
		return false
	}
	for i, r := range s.buf {
		if e.buf[i] != r {
			return false
		}
	}
	return true
}

// Vim is the state machine; the zero value is insert mode, off.
type Vim struct {
	Enabled bool
	Mode    VimMode

	count   int  // the count typed before the command; 0 for none
	op      rune // the pending operator (d, c, y), 0 for none
	opCount int  // the count typed before the operator
	prefix  rune // 'g' typed, waiting for its second key

	reg     string // the register: what was last deleted or yanked
	regLine bool   // the register holds whole lines

	undo, redo []vimSnap
	insertSnap *vimSnap // the buffer when the insert session began; pushed to undo when it ends with a change

	line     []rune // the ":" or "/" line
	lastFind string

	// pending are the keys of the command being typed, kept as the last
	// change for "." once the command changes the buffer; an insert
	// session's keys are recorded until its Esc.
	pending   []Key
	last      []Key
	recording bool // an insert session started by a normal-mode command is being recorded
	replaying bool
}

// Enable turns vim mode on in insert mode.
func (v *Vim) Enable() {
	v.Enabled = true
	v.Reset()
}

// Disable turns vim mode off.
func (v *Vim) Disable() {
	v.Enabled = false
	v.Reset()
}

// Reset returns to insert mode with nothing pending and no history to
// undo: after a send or a cleared draft.
func (v *Vim) Reset() {
	v.Mode = VimInsert
	v.count, v.op, v.opCount, v.prefix = 0, 0, 0, 0
	v.undo, v.redo, v.insertSnap = nil, nil, nil
	v.line = nil
	v.pending, v.recording = nil, false
}

// Line is the ":" or "/" line being typed, with its prompt.
func (v *Vim) Line() string {
	switch v.Mode {
	case VimCommand:
		return ":" + string(v.line)
	case VimSearch:
		return "/" + string(v.line)
	}
	return ""
}

// Handle applies one key to the editor's buffer and says what the app
// does next.
func (v *Vim) Handle(e *Editor, k Key) VimResult {
	if !v.Enabled {
		return VimResult{Action: VimPass}
	}
	switch v.Mode {
	case VimInsert:
		return v.insertKey(e, k)
	case VimCommand, VimSearch:
		return v.lineKey(e, k)
	}
	return v.normalKey(e, k)
}

// insertKey: Esc ends the session, everything else is the editor's.
func (v *Vim) insertKey(e *Editor, k Key) VimResult {
	if v.insertSnap == nil {
		s := snapOf(e)
		v.insertSnap = &s
	}
	if k.Kind != KeyEscape {
		if v.recording {
			v.pending = append(v.pending, k)
		}
		if v.replaying {
			e.Handle(k)
			return VimResult{Action: VimHandled}
		}
		return VimResult{Action: VimPass}
	}
	v.endInsert(e)
	if v.recording {
		v.pending = append(v.pending, k)
		v.finishChange()
	}
	return VimResult{Action: VimHandled}
}

// endInsert leaves insert mode: the session is one undo step when it
// changed the buffer, and the cursor steps back onto the last character
// typed, as vim's does.
func (v *Vim) endInsert(e *Editor) {
	if s := v.insertSnap; s != nil {
		if !s.equals(e) {
			v.undo = append(v.undo, *s)
			v.redo = nil
		}
		v.insertSnap = nil
	}
	start, _ := e.lineBounds()
	if e.cursor > start {
		e.cursor--
	}
	v.Mode = VimNormal
}

// beginInsert enters insert mode from a normal-mode command, the buffer
// as it is before the command's own change being the undo point.
func (v *Vim) beginInsert(e *Editor) {
	s := snapOf(e)
	v.insertSnap = &s
	v.Mode = VimInsert
	if !v.replaying {
		v.recording = true
	}
}

// lineKey types the ":" or "/" line.
func (v *Vim) lineKey(e *Editor, k Key) VimResult {
	switch k.Kind {
	case KeyRune:
		v.line = append(v.line, k.Rune)
	case KeyPaste:
		v.line = append(v.line, []rune(strings.ReplaceAll(k.Text, "\n", " "))...)
	case KeyBackspace:
		if len(v.line) == 0 {
			v.Mode = VimNormal
			return VimResult{Action: VimHandled}
		}
		v.line = v.line[:len(v.line)-1]
	case KeyEscape, KeyCtrlC, KeyCtrlG:
		v.line = nil
		v.Mode = VimNormal
	case KeyEnter:
		line := strings.TrimSpace(string(v.line))
		mode := v.Mode
		v.line = nil
		v.Mode = VimNormal
		if mode == VimSearch {
			if line == "" {
				line = v.lastFind
			}
			if line == "" {
				return VimResult{Action: VimNotice, Text: "/TEXT searches the transcript upward; n repeats"}
			}
			v.lastFind = line
			return VimResult{Action: VimFind, Text: line}
		}
		return v.command(line)
	}
	return VimResult{Action: VimHandled}
}

// command runs a ":" line.
func (v *Vim) command(line string) VimResult {
	switch strings.Join(strings.Fields(line), " ") {
	case "":
		return VimResult{Action: VimHandled}
	case "w", "w!", "write":
		return VimResult{Action: VimSend}
	case "wq", "wq!", "x", "x!":
		return VimResult{Action: VimSend, Text: "quit"}
	case "q", "q!", "qa", "qa!", "quit":
		return VimResult{Action: VimQuit}
	case "set novim", "set vim!", "novim":
		return VimResult{Action: VimDisable}
	case "set vim":
		return VimResult{Action: VimNotice, Text: "vim mode is on; :set novim turns it off"}
	}
	return VimResult{Action: VimNotice, Text: "not a vim command: :" + line + " · :w sends, :q quits, :wq, :set novim"}
}

// normalKey is a key in normal mode.
func (v *Vim) normalKey(e *Editor, k Key) VimResult {
	switch k.Kind {
	case KeyEscape:
		if v.count != 0 || v.op != 0 || v.prefix != 0 {
			v.clear()
			return VimResult{Action: VimHandled}
		}
		return VimResult{Action: VimEscape}
	case KeyEnter:
		v.clear()
		return VimResult{Action: VimSend}
	case KeyCtrlR:
		v.clear()
		v.redoOnce(e)
		return VimResult{Action: VimHandled}
	case KeyLeft:
		return v.normalRune(e, 'h', k)
	case KeyRight:
		return v.normalRune(e, 'l', k)
	case KeyUp:
		return v.normalRune(e, 'k', k)
	case KeyDown:
		return v.normalRune(e, 'j', k)
	case KeyBackspace:
		return v.normalRune(e, 'h', k)
	case KeyDelete:
		return v.normalRune(e, 'x', k)
	case KeyHome, KeyEnd:
		if len(e.buf) == 0 {
			return VimResult{Action: VimPass} // the transcript's top and bottom
		}
		if k.Kind == KeyHome {
			return v.normalRune(e, '0', k)
		}
		return v.normalRune(e, '$', k)
	case KeyRune:
		return v.normalRune(e, k.Rune, k)
	}
	// Ctrl-C, Ctrl-D, Ctrl-L, Ctrl-O, Tab, Shift-Tab, PgUp/PgDn, the
	// wheel, a paste: the app's, as without vim.
	return VimResult{Action: VimPass}
}

func (v *Vim) clear() {
	v.count, v.op, v.opCount, v.prefix = 0, 0, 0, 0
	v.pending = nil
}

// finishChange keeps the command's keys for ".".
func (v *Vim) finishChange() {
	if !v.replaying {
		v.last = append([]Key(nil), v.pending...)
	}
	v.pending, v.recording = nil, false
}

// normalRune is a normal-mode key given as the rune it stands for.
func (v *Vim) normalRune(e *Editor, r rune, k Key) VimResult {
	if !v.replaying {
		v.pending = append(v.pending, k)
	}
	// A count: digits, "0" only after another digit.
	if r >= '1' && r <= '9' || (r == '0' && v.count > 0) {
		v.count = v.count*10 + int(r-'0')
		return VimResult{Action: VimHandled}
	}
	if v.prefix == 'g' {
		v.prefix = 0
		if r != 'g' {
			v.clear()
			return VimResult{Action: VimHandled}
		}
		if v.op == 0 && len(e.buf) == 0 {
			v.clear()
			return VimResult{Action: VimScroll, Text: "top"}
		}
		return v.motionKey(e, 'g')
	}
	if v.op != 0 {
		if r == v.op {
			// dd, cc, yy: the operator over count lines.
			n := max(1, v.opCount) * max(1, v.count)
			from := e.cursor
			to, _ := moveLines(e.buf, e.cursor, n-1)
			changed := v.applyOp(e, v.op, from, to, linewise)
			v.settle(changed)
			return VimResult{Action: VimHandled}
		}
		return v.motionKey(e, r)
	}
	switch r {
	case 'd', 'c', 'y':
		v.op, v.opCount, v.count = r, v.count, 0
		return VimResult{Action: VimHandled}
	case 'D', 'C':
		v.op, v.opCount, v.count = unicode.ToLower(r), v.count, 0
		return v.motionKey(e, '$')
	case 'g':
		v.prefix = 'g'
		return VimResult{Action: VimHandled}
	case 'x', 'X':
		n := max(1, v.count)
		start, end := e.lineBounds()
		a, b := e.cursor, min(end, e.cursor+n)
		if r == 'X' {
			a, b = max(start, e.cursor-n), e.cursor
		}
		if a == b {
			v.clear()
			return VimResult{Action: VimHandled}
		}
		v.pushUndo(e)
		v.reg, v.regLine = string(e.buf[a:b]), false
		e.buf = append(e.buf[:a], e.buf[b:]...)
		e.cursor = a
		v.clampCursor(e)
		v.settle(true)
		return VimResult{Action: VimHandled}
	case 'p', 'P':
		v.put(e, r == 'P', max(1, v.count))
		v.settle(true)
		return VimResult{Action: VimHandled}
	case 'u':
		v.clear()
		v.undoOnce(e)
		return VimResult{Action: VimHandled}
	case '.':
		return v.repeat(e)
	case 'i', 'a', 'I', 'A', 'o', 'O':
		v.count, v.opCount = 0, 0
		v.beginInsert(e)
		start, end := e.lineBounds()
		switch r {
		case 'a':
			if e.cursor < end {
				e.cursor++
			}
		case 'I':
			e.cursor = firstNonBlank(e.buf, start, end)
		case 'A':
			e.cursor = end
		case 'o':
			e.cursor = end
			e.Insert("\n")
		case 'O':
			e.cursor = start
			e.Insert("\n")
			e.cursor = start
		}
		return VimResult{Action: VimHandled}
	case ':':
		v.clear()
		v.Mode, v.line = VimCommand, nil
		return VimResult{Action: VimHandled}
	case '/':
		v.clear()
		v.Mode, v.line = VimSearch, nil
		return VimResult{Action: VimHandled}
	case 'n':
		v.clear()
		if v.lastFind == "" {
			return VimResult{Action: VimNotice, Text: "nothing searched yet; /TEXT searches the transcript"}
		}
		return VimResult{Action: VimFind, Text: v.lastFind}
	case 'G':
		if len(e.buf) == 0 {
			v.clear()
			return VimResult{Action: VimScroll, Text: "bottom"}
		}
	case 'j', 'k':
		if len(e.buf) == 0 || !canMoveLine(e.buf, e.cursor, map[rune]int{'j': 1, 'k': -1}[r]) {
			// Past the draft's edge the line keys recall the history, as
			// Up and Down do in insert mode.
			v.clear()
			if r == 'k' {
				e.historyPrev()
			} else {
				e.historyNext()
			}
			v.clampCursor(e)
			return VimResult{Action: VimHandled}
		}
	}
	return v.motionKey(e, r)
}

// motionKey moves the cursor by motion r, or applies the pending operator
// over it.
func (v *Vim) motionKey(e *Editor, r rune) VimResult {
	n := v.count
	if v.op != 0 && v.opCount > 0 {
		n = v.opCount * max(1, n)
	}
	target, kind, ok := motion(e.buf, e.cursor, r, n, v.op != 0)
	if !ok {
		v.clear()
		return VimResult{Action: VimHandled}
	}
	if v.op == 0 {
		e.cursor = target
		v.clampCursor(e)
		v.clear()
		return VimResult{Action: VimHandled}
	}
	if v.op == 'c' && r == 'w' && e.cursor < len(e.buf) && !unicode.IsSpace(e.buf[e.cursor]) {
		// cw on a word changes to its end, as vim's does.
		target, kind, _ = motion(e.buf, e.cursor, 'e', n, true)
	}
	changed := v.applyOp(e, v.op, e.cursor, target, kind)
	v.settle(changed)
	return VimResult{Action: VimHandled}
}

// settle ends a command: a change is kept for ".", unless it opened an
// insert session, which is kept when the session ends.
func (v *Vim) settle(changed bool) {
	v.count, v.op, v.opCount, v.prefix = 0, 0, 0, 0
	if v.Mode == VimInsert {
		return
	}
	if changed {
		v.finishChange()
	} else {
		v.pending = nil
	}
}

// applyOp runs operator op over the range between from and to (either
// order), as the motion's kind bounds it; true when the buffer changed.
func (v *Vim) applyOp(e *Editor, op rune, from, to int, kind motionKind) bool {
	a, b := min(from, to), max(from, to)
	switch kind {
	case inclusive:
		b = min(len(e.buf), b+1)
	case linewise:
		a, _ = lineAt(e.buf, a)
		_, b = lineAt(e.buf, b)
	}
	if a == b && kind != linewise && op != 'c' {
		return false
	}
	if a != b {
		v.reg, v.regLine = string(e.buf[a:b]), kind == linewise
	}
	switch op {
	case 'y':
		if kind != linewise {
			e.cursor = a
		}
		v.clampCursor(e)
		return false
	case 'd':
		v.pushUndo(e)
		if kind == linewise {
			switch {
			case b < len(e.buf):
				b++ // the line's newline
			case a > 0:
				a-- // the last line: the newline before it
			}
		}
		e.buf = append(e.buf[:a], e.buf[b:]...)
		e.cursor = a
		if kind == linewise {
			start, end := e.lineBounds()
			e.cursor = firstNonBlank(e.buf, start, end)
		}
		v.clampCursor(e)
		return true
	case 'c':
		v.beginInsert(e)
		e.buf = append(e.buf[:a], e.buf[b:]...)
		e.cursor = a
		return true
	}
	return false
}

// put pastes the register after the cursor (before with before), n times.
func (v *Vim) put(e *Editor, before bool, n int) {
	if v.reg == "" {
		v.clear()
		return
	}
	v.pushUndo(e)
	text := strings.Repeat(v.reg, n)
	if v.regLine {
		start, end := e.lineBounds()
		lines := strings.TrimSuffix(strings.Repeat(v.reg+"\n", n), "\n")
		if before {
			e.cursor = start
			e.Insert(lines + "\n")
			e.cursor = start
		} else if end >= len(e.buf) {
			e.cursor = end
			e.Insert("\n" + lines)
			e.cursor = end + 1
		} else {
			e.cursor = end + 1
			e.Insert(lines + "\n")
			e.cursor = end + 1
		}
		s, t := e.lineBounds()
		e.cursor = firstNonBlank(e.buf, s, t)
		return
	}
	if !before && e.cursor < len(e.buf) && e.buf[e.cursor] != '\n' {
		e.cursor++
	}
	e.Insert(text)
	e.cursor--
	v.clampCursor(e)
}

func (v *Vim) pushUndo(e *Editor) {
	v.undo = append(v.undo, snapOf(e))
	v.redo = nil
}

func (v *Vim) undoOnce(e *Editor) {
	if len(v.undo) == 0 {
		return
	}
	v.redo = append(v.redo, snapOf(e))
	s := v.undo[len(v.undo)-1]
	v.undo = v.undo[:len(v.undo)-1]
	s.restore(e)
	v.clampCursor(e)
}

func (v *Vim) redoOnce(e *Editor) {
	if len(v.redo) == 0 {
		return
	}
	v.undo = append(v.undo, snapOf(e))
	s := v.redo[len(v.redo)-1]
	v.redo = v.redo[:len(v.redo)-1]
	s.restore(e)
	v.clampCursor(e)
}

// repeat replays the last change; a count given before "." replaces the
// change's own.
func (v *Vim) repeat(e *Editor) VimResult {
	count := v.count
	v.clear()
	if len(v.last) == 0 {
		return VimResult{Action: VimHandled}
	}
	keys := v.last
	if count > 0 {
		i := 0
		for i < len(keys) && keys[i].Kind == KeyRune && keys[i].Rune >= '0' && keys[i].Rune <= '9' {
			i++
		}
		var digits []Key
		for _, d := range []rune(itoa(count)) {
			digits = append(digits, Key{Kind: KeyRune, Rune: d})
		}
		keys = append(digits, keys[i:]...)
	}
	v.replaying = true
	for _, k := range keys {
		v.Handle(e, k)
	}
	v.replaying = false
	if v.Mode == VimInsert {
		// A recorded insert always ends with its Esc; a cut recording
		// (the machine was reset) is closed here.
		v.endInsert(e)
	}
	v.pending = nil
	return VimResult{Action: VimHandled}
}

// clampCursor keeps the cursor on a character in normal mode: never past
// the line's last one, unless the line is empty.
func (v *Vim) clampCursor(e *Editor) {
	if v.Mode != VimNormal {
		return
	}
	e.cursor = max(0, min(e.cursor, len(e.buf)))
	start, end := e.lineBounds()
	if e.cursor >= end && end > start {
		e.cursor = end - 1
	}
}

// The motions, as functions of the buffer.

type motionKind int

const (
	exclusive motionKind = iota // up to, not including, the target
	inclusive                   // the target too
	linewise                    // whole lines from the cursor's to the target's
)

// lineAt bounds the line holding position pos: [start, end), end the
// newline or the buffer's end.
func lineAt(buf []rune, pos int) (start, end int) {
	pos = max(0, min(pos, len(buf)))
	start = pos
	for start > 0 && buf[start-1] != '\n' {
		start--
	}
	end = pos
	for end < len(buf) && buf[end] != '\n' {
		end++
	}
	return start, end
}

func firstNonBlank(buf []rune, start, end int) int {
	for start < end && (buf[start] == ' ' || buf[start] == '\t') {
		start++
	}
	return start
}

// canMoveLine says whether a line lies delta (±1) lines away.
func canMoveLine(buf []rune, pos, delta int) bool {
	start, end := lineAt(buf, pos)
	if delta < 0 {
		return start > 0
	}
	return end < len(buf)
}

// moveLines moves pos delta lines (negative up), keeping the column where
// it can, as far as the buffer allows; moved says how many lines it went.
func moveLines(buf []rune, pos, delta int) (int, int) {
	moved := 0
	for ; delta != 0; delta -= sign(delta) {
		start, end := lineAt(buf, pos)
		col := pos - start
		if delta < 0 {
			if start == 0 {
				break
			}
			ps, pe := lineAt(buf, start-1)
			pos = ps + min(col, pe-ps)
		} else {
			if end >= len(buf) {
				break
			}
			ns, ne := lineAt(buf, end+1)
			pos = ns + min(col, ne-ns)
		}
		moved++
	}
	return pos, moved
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// charClass tells words apart the way vim does: blanks, keyword
// characters, and everything else.
func charClass(r rune) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
		return 1
	}
	return 2
}

// wordForward is vim's w: to the start of the next word; an empty line
// is a word.
func wordForward(buf []rune, pos int) int {
	if pos >= len(buf) {
		return len(buf)
	}
	i := pos
	if c := charClass(buf[i]); c != 0 {
		for i < len(buf) && charClass(buf[i]) == c {
			i++
		}
	}
	for i < len(buf) && charClass(buf[i]) == 0 {
		if buf[i] == '\n' && i > pos && (i+1 == len(buf) || buf[i+1] == '\n') {
			return i + 1 // an empty line
		}
		i++
	}
	return i
}

// wordEnd is vim's e: to the end of the word the cursor is in, or of the
// next one when already at an end.
func wordEnd(buf []rune, pos int) int {
	i := pos + 1
	for i < len(buf) && charClass(buf[i]) == 0 {
		i++
	}
	if i >= len(buf) {
		return max(0, len(buf)-1)
	}
	c := charClass(buf[i])
	for i+1 < len(buf) && charClass(buf[i+1]) == c {
		i++
	}
	return i
}

// wordBack is vim's b: to the start of the word before the cursor.
func wordBack(buf []rune, pos int) int {
	i := min(pos, len(buf)) - 1
	for i > 0 && charClass(buf[i]) == 0 {
		i--
	}
	if i <= 0 {
		return 0
	}
	c := charClass(buf[i])
	for i > 0 && charClass(buf[i-1]) == c {
		i--
	}
	return i
}

// motion says where motion r takes the cursor from pos, n times (0 for
// no count), and how an operator bounds the range; forOp is whether an
// operator will use it (w then stays on its line, as vim's dw does). ok
// is false for a rune that is no motion, or a line motion with nowhere
// to go.
func motion(buf []rune, pos int, r rune, n int, forOp bool) (target int, kind motionKind, ok bool) {
	times := max(1, n)
	start, end := lineAt(buf, pos)
	switch r {
	case 'h':
		return max(start, pos-times), exclusive, true
	case 'l':
		t := min(end, pos+times)
		if !forOp && t >= end && end > start {
			t = end - 1
		}
		return t, exclusive, true
	case '0':
		return start, exclusive, true
	case '^':
		return firstNonBlank(buf, start, end), exclusive, true
	case '$':
		p := pos
		if times > 1 {
			p, _ = moveLines(buf, pos, times-1)
		}
		_, e := lineAt(buf, p)
		if forOp {
			return e, exclusive, true
		}
		return max(e-1, start), exclusive, true
	case 'j', 'k':
		delta := times
		if r == 'k' {
			delta = -times
		}
		t, moved := moveLines(buf, pos, delta)
		if moved == 0 {
			return pos, linewise, false
		}
		return t, linewise, true
	case 'w':
		t := pos
		for i := 0; i < times; i++ {
			t = wordForward(buf, t)
		}
		if forOp {
			// dw on a line's last word deletes to the line's end, not
			// the newline; the count may still span lines.
			if _, e := lineAt(buf, pos); t > e && times == 1 {
				t = e
			}
		}
		return t, exclusive, true
	case 'e':
		t := pos
		for i := 0; i < times; i++ {
			t = wordEnd(buf, t)
		}
		return t, inclusive, true
	case 'b':
		t := pos
		for i := 0; i < times; i++ {
			t = wordBack(buf, t)
		}
		return t, exclusive, true
	case 'G':
		if n > 0 {
			return lineStartN(buf, n), linewise, true
		}
		s, _ := lineAt(buf, len(buf))
		return s, linewise, true
	case 'g': // gg
		return lineStartN(buf, max(1, n)), linewise, true
	}
	return pos, exclusive, false
}

// lineStartN is where line n (from 1) starts; past the end, the last line.
func lineStartN(buf []rune, n int) int {
	pos := 0
	for line := 1; line < n; line++ {
		_, end := lineAt(buf, pos)
		if end >= len(buf) {
			break
		}
		pos = end + 1
	}
	return pos
}
