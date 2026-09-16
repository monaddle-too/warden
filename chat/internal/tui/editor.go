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
	KeyNewline // Alt+Enter or Ctrl+J: a line break inside the composer
	KeyPaste   // bracketed paste; Text holds the pasted text
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

// Editor is a single-line composer with a cursor and history.
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

// Submit returns the current text, records it in history and clears.
func (e *Editor) Submit() string {
	s := strings.TrimSpace(string(e.buf))
	if s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
		e.history = append(e.history, s)
	}
	e.buf, e.cursor, e.draft = nil, 0, ""
	e.hist = len(e.history)
	return s
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
	case KeyHome:
		e.cursor = 0
	case KeyEnd:
		e.cursor = len(e.buf)
	case KeyCtrlU:
		e.buf = e.buf[e.cursor:]
		e.cursor = 0
	case KeyCtrlK:
		e.buf = e.buf[:e.cursor]
	case KeyUp, KeyCtrlP:
		if len(e.history) == 0 || e.hist == 0 {
			return true
		}
		if e.hist == len(e.history) {
			e.draft = string(e.buf)
		}
		e.hist--
		e.buf = []rune(e.history[e.hist])
		e.cursor = len(e.buf)
	case KeyDown, KeyCtrlN:
		if e.hist >= len(e.history) {
			return true
		}
		e.hist++
		if e.hist == len(e.history) {
			e.buf = []rune(e.draft)
		} else {
			e.buf = []rune(e.history[e.hist])
		}
		e.cursor = len(e.buf)
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
