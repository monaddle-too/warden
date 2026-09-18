package tui

import (
	"strings"
	"unicode/utf8"
)

// Key is one decoded keypress.
type Key struct {
	Rune rune   // printable input when Kind == KeyRune
	Text string // pasted text when Kind == KeyPaste
	Kind KeyKind
}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyEnter
	KeyBackspace
	KeyLeft
	KeyRight
	KeyUp
	KeyDown
	KeyHome
	KeyEnd
	KeyDelete
	KeyCtrlC
	KeyCtrlD
	KeyCtrlU
	KeyCtrlK
	KeyCtrlL
	KeyEscape
	KeyTab
	KeyPageUp
	KeyPageDown
	KeyWheelUp
	KeyWheelDown
	KeyCtrlP
	KeyCtrlN
	KeyCtrlA
	KeyCtrlE
	KeyCtrlW // Ctrl+W or Alt+Backspace: delete the word before the cursor
	KeyCtrlO
	KeyCtrlR
	KeyCtrlG
	KeyShiftTab
	KeyNewline // Alt+Enter or Ctrl+J: a line break inside the composer
	KeyPaste   // bracketed paste; Text holds the pasted text
	KeyEOF     // the input ended
	KeyUnknown
)

// DecodeKeys turns raw terminal bytes into keys, returning what was decoded
// and the bytes that need more input (an incomplete escape or UTF-8 rune).
func DecodeKeys(buf []byte) (keys []Key, rest []byte) {
	for len(buf) > 0 {
		b := buf[0]
		switch {
		case b == 0x1b:
			if len(buf) == 1 {
				return keys, buf // may be a lone Escape or the start of a sequence
			}
			if buf[1] == '\r' || buf[1] == '\n' {
				keys = append(keys, Key{Kind: KeyNewline}) // Alt+Enter
				buf = buf[2:]
				continue
			}
			if buf[1] == 0x7f || buf[1] == 0x08 {
				keys = append(keys, Key{Kind: KeyCtrlW}) // Alt+Backspace
				buf = buf[2:]
				continue
			}
			if strings.HasPrefix(string(buf), "\x1b[200~") {
				// Bracketed paste: everything up to the closing marker is text.
				end := strings.Index(string(buf), "\x1b[201~")
				if end < 0 {
					if len(buf) > 1<<20 {
						buf = buf[6:]
						continue
					}
					return keys, buf
				}
				keys = append(keys, Key{Kind: KeyPaste, Text: strings.ReplaceAll(string(buf[6:end]), "\r", "\n")})
				buf = buf[end+6:]
				continue
			}
			if buf[1] == '[' || buf[1] == 'O' {
				// CSI / SS3: find the final byte.
				end := -1
				for i := 2; i < len(buf); i++ {
					if (buf[i] >= 'A' && buf[i] <= 'Z') || (buf[i] >= 'a' && buf[i] <= 'z') || buf[i] == '~' {
						end = i
						break
					}
				}
				if end < 0 {
					if len(buf) > 16 {
						buf = buf[1:]
						continue
					}
					return keys, buf
				}
				seq := string(buf[2 : end+1])
				buf = buf[end+1:]
				// SGR mouse report: ESC [ < button ; col ; row M|m. Only the
				// wheel is used; presses are ignored.
				if strings.HasPrefix(seq, "<") {
					fields := strings.Split(strings.TrimRight(seq[1:], "Mm"), ";")
					if len(fields) == 3 && strings.HasSuffix(seq, "M") {
						switch fields[0] {
						case "64":
							keys = append(keys, Key{Kind: KeyWheelUp})
						case "65":
							keys = append(keys, Key{Kind: KeyWheelDown})
						}
					}
					continue
				}
				switch seq {
				case "A":
					keys = append(keys, Key{Kind: KeyUp})
				case "B":
					keys = append(keys, Key{Kind: KeyDown})
				case "C":
					keys = append(keys, Key{Kind: KeyRight})
				case "D":
					keys = append(keys, Key{Kind: KeyLeft})
				case "H", "1~", "7~":
					keys = append(keys, Key{Kind: KeyHome})
				case "F", "4~", "8~":
					keys = append(keys, Key{Kind: KeyEnd})
				case "3~":
					keys = append(keys, Key{Kind: KeyDelete})
				case "5~":
					keys = append(keys, Key{Kind: KeyPageUp})
				case "6~":
					keys = append(keys, Key{Kind: KeyPageDown})
				case "Z":
					keys = append(keys, Key{Kind: KeyShiftTab})
				default:
					keys = append(keys, Key{Kind: KeyUnknown})
				}
				continue
			}
			// Escape followed by an ordinary byte: treat as Escape, keep the byte.
			keys = append(keys, Key{Kind: KeyEscape})
			buf = buf[1:]
		case b == '\r':
			keys = append(keys, Key{Kind: KeyEnter})
			buf = buf[1:]
		case b == '\n':
			keys = append(keys, Key{Kind: KeyNewline}) // Ctrl+J
			buf = buf[1:]
		case b == 0x7f || b == 0x08:
			keys = append(keys, Key{Kind: KeyBackspace})
			buf = buf[1:]
		case b == 0x03:
			keys = append(keys, Key{Kind: KeyCtrlC})
			buf = buf[1:]
		case b == 0x04:
			keys = append(keys, Key{Kind: KeyCtrlD})
			buf = buf[1:]
		case b == 0x15:
			keys = append(keys, Key{Kind: KeyCtrlU})
			buf = buf[1:]
		case b == 0x0b:
			keys = append(keys, Key{Kind: KeyCtrlK})
			buf = buf[1:]
		case b == 0x0c:
			keys = append(keys, Key{Kind: KeyCtrlL})
			buf = buf[1:]
		case b == 0x10:
			keys = append(keys, Key{Kind: KeyCtrlP})
			buf = buf[1:]
		case b == 0x0e:
			keys = append(keys, Key{Kind: KeyCtrlN})
			buf = buf[1:]
		case b == 0x01:
			keys = append(keys, Key{Kind: KeyCtrlA})
			buf = buf[1:]
		case b == 0x05:
			keys = append(keys, Key{Kind: KeyCtrlE})
			buf = buf[1:]
		case b == 0x17:
			keys = append(keys, Key{Kind: KeyCtrlW})
			buf = buf[1:]
		case b == 0x0f:
			keys = append(keys, Key{Kind: KeyCtrlO})
			buf = buf[1:]
		case b == 0x12:
			keys = append(keys, Key{Kind: KeyCtrlR})
			buf = buf[1:]
		case b == 0x07:
			keys = append(keys, Key{Kind: KeyCtrlG})
			buf = buf[1:]
		case b == '\t':
			keys = append(keys, Key{Kind: KeyTab})
			buf = buf[1:]
		case b < 0x20:
			keys = append(keys, Key{Kind: KeyUnknown})
			buf = buf[1:]
		default:
			r, size := utf8.DecodeRune(buf)
			if r == utf8.RuneError && size == 1 {
				if !utf8.FullRune(buf) {
					return keys, buf
				}
				buf = buf[1:]
				continue
			}
			keys = append(keys, Key{Kind: KeyRune, Rune: r})
			buf = buf[size:]
		}
	}
	return keys, nil
}

// Editor is the composer: a buffer with a cursor (newlines allowed) and a
// prompt history, browsed with Up/Down at the draft's edges or Ctrl+P/N.
type Editor struct {
	buf     []rune
	cursor  int
	history []string
	hist    int    // index into history while browsing; len(history) = not browsing
	draft   string // the line being typed before browsing history
}

func (e *Editor) Text() string { return string(e.buf) }
func (e *Editor) Cursor() int  { return e.cursor }

func (e *Editor) Set(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
	e.hist = len(e.history)
}

// SetCursor moves the cursor to rune index n, clamped to the text.
func (e *Editor) SetCursor(n int) {
	e.cursor = max(0, min(n, len(e.buf)))
}

// History is the prompts recorded so far, oldest first.
func (e *Editor) History() []string { return e.history }

// SetHistory replaces the recorded prompts (loaded for a chat).
func (e *Editor) SetHistory(h []string) {
	e.history = append([]string(nil), h...)
	e.hist = len(e.history)
	e.draft = ""
}

// Remember records a prompt in history, skipping a repeat of the last one.
func (e *Editor) Remember(s string) {
	if s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
		e.history = append(e.history, s)
	}
	e.hist = len(e.history)
}

// Submit returns the current text, records it in history and clears.
func (e *Editor) Submit() string {
	s := strings.TrimSpace(string(e.buf))
	e.Remember(s)
	e.buf, e.cursor, e.draft = nil, 0, ""
	return s
}

// Clear drops the text without recording it.
func (e *Editor) Clear() {
	e.buf, e.cursor, e.draft = nil, 0, ""
	e.hist = len(e.history)
}

// lineBounds returns the rune range [start, end) of the line the cursor is on.
func (e *Editor) lineBounds() (start, end int) {
	start = e.cursor
	for start > 0 && e.buf[start-1] != '\n' {
		start--
	}
	end = e.cursor
	for end < len(e.buf) && e.buf[end] != '\n' {
		end++
	}
	return start, end
}

// historyPrev recalls the previous prompt; false when there is none.
func (e *Editor) historyPrev() bool {
	if len(e.history) == 0 || e.hist == 0 {
		return false
	}
	if e.hist == len(e.history) {
		e.draft = string(e.buf)
	}
	e.hist--
	e.buf = []rune(e.history[e.hist])
	e.cursor = len(e.buf)
	return true
}

// historyNext moves toward the draft; false when not browsing.
func (e *Editor) historyNext() bool {
	if e.hist >= len(e.history) {
		return false
	}
	e.hist++
	if e.hist == len(e.history) {
		e.buf = []rune(e.draft)
	} else {
		e.buf = []rune(e.history[e.hist])
	}
	e.cursor = len(e.buf)
	return true
}

// moveLine moves the cursor a line up (-1) or down (+1), keeping the
// column where it can; false when the cursor is already on the edge line.
func (e *Editor) moveLine(delta int) bool {
	start, end := e.lineBounds()
	col := e.cursor - start
	if delta < 0 {
		if start == 0 {
			return false
		}
		prevEnd := start - 1
		prevStart := prevEnd
		for prevStart > 0 && e.buf[prevStart-1] != '\n' {
			prevStart--
		}
		e.cursor = prevStart + min(col, prevEnd-prevStart)
		return true
	}
	if end >= len(e.buf) {
		return false
	}
	nextStart := end + 1
	nextEnd := nextStart
	for nextEnd < len(e.buf) && e.buf[nextEnd] != '\n' {
		nextEnd++
	}
	e.cursor = nextStart + min(col, nextEnd-nextStart)
	return true
}

// Handle applies one key. It returns true when the key was consumed.
func (e *Editor) Handle(k Key) bool {
	switch k.Kind {
	case KeyRune:
		e.buf = append(e.buf[:e.cursor], append([]rune{k.Rune}, e.buf[e.cursor:]...)...)
		e.cursor++
	case KeyNewline:
		e.Insert("\n")
	case KeyPaste:
		e.Insert(k.Text)
	case KeyBackspace:
		if e.cursor > 0 {
			e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
			e.cursor--
		}
	case KeyDelete:
		if e.cursor < len(e.buf) {
			e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
		}
	case KeyLeft:
		if e.cursor > 0 {
			e.cursor--
		}
	case KeyRight:
		if e.cursor < len(e.buf) {
			e.cursor++
		}
	case KeyHome, KeyCtrlA:
		e.cursor, _ = e.lineBounds()
	case KeyEnd, KeyCtrlE:
		_, e.cursor = e.lineBounds()
	case KeyCtrlU:
		// Delete from the line's start to the cursor (readline).
		start, _ := e.lineBounds()
		e.buf = append(e.buf[:start], e.buf[e.cursor:]...)
		e.cursor = start
	case KeyCtrlK:
		_, end := e.lineBounds()
		e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
	case KeyCtrlW:
		// Delete the word before the cursor: trailing spaces, then the run
		// of non-spaces, never past the line's start.
		start, _ := e.lineBounds()
		i := e.cursor
		for i > start && e.buf[i-1] == ' ' {
			i--
		}
		for i > start && e.buf[i-1] != ' ' {
			i--
		}
		e.buf = append(e.buf[:i], e.buf[e.cursor:]...)
		e.cursor = i
	case KeyUp:
		if !e.moveLine(-1) {
			e.historyPrev()
		}
	case KeyDown:
		if !e.moveLine(1) {
			e.historyNext()
		}
	case KeyCtrlP:
		e.historyPrev()
	case KeyCtrlN:
		e.historyNext()
	default:
		return false
	}
	return true
}

// Insert puts text at the cursor; newlines are kept.
func (e *Editor) Insert(text string) {
	runes := []rune(text)
	e.buf = append(e.buf[:e.cursor], append(runes, e.buf[e.cursor:]...)...)
	e.cursor += len(runes)
}

// Lines splits the buffer at newlines and reports the cursor as a row and
// column within them.
func (e *Editor) Lines() (lines []string, row, col int) {
	current := []rune{}
	row, col = 0, 0
	for i, r := range e.buf {
		if i == e.cursor {
			row, col = len(lines), len(current)
		}
		if r == '\n' {
			lines = append(lines, string(current))
			current = []rune{}
			continue
		}
		current = append(current, r)
	}
	if e.cursor == len(e.buf) {
		row, col = len(lines), len(current)
	}
	lines = append(lines, string(current))
	return lines, row, col
}
