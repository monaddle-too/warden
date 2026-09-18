package tui

import (
	"strings"
	"testing"
)

// vimKeys turns a vim-style key string into keys: <Esc>, <CR>, <BS>,
// <Del>, <Left>, <Right>, <Up>, <Down>, <Home>, <End>, <C-r>, <C-c>;
// every other rune is itself.
func vimKeys(s string) []Key {
	names := map[string]Key{
		"Esc": {Kind: KeyEscape}, "CR": {Kind: KeyEnter}, "BS": {Kind: KeyBackspace}, "Del": {Kind: KeyDelete},
		"Left": {Kind: KeyLeft}, "Right": {Kind: KeyRight}, "Up": {Kind: KeyUp}, "Down": {Kind: KeyDown},
		"Home": {Kind: KeyHome}, "End": {Kind: KeyEnd}, "C-r": {Kind: KeyCtrlR}, "C-c": {Kind: KeyCtrlC},
		"Tab": {Kind: KeyTab}, "NL": {Kind: KeyNewline},
	}
	var out []Key
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '<' {
			if j := strings.IndexRune(string(runes[i:]), '>'); j > 0 {
				if k, ok := names[string(runes[i+1:i+j])]; ok {
					out = append(out, k)
					i += j
					continue
				}
			}
		}
		out = append(out, Key{Kind: KeyRune, Rune: runes[i]})
	}
	return out
}

// vimDrive feeds keys to a vim machine over an editor as the app does:
// a passed key goes to the editor. It returns every action other than
// handled and pass, in order.
func vimDrive(v *Vim, e *Editor, keys string) []VimResult {
	var out []VimResult
	for _, k := range vimKeys(keys) {
		r := v.Handle(e, k)
		switch r.Action {
		case VimPass:
			e.Handle(k)
		case VimHandled:
		default:
			out = append(out, r)
		}
	}
	return out
}

// normalVim is a machine in normal mode over text with the cursor at c.
func normalVim(text string, c int) (*Vim, *Editor) {
	var e Editor
	e.Set(text)
	e.SetCursor(c)
	v := &Vim{}
	v.Enable()
	v.Mode = VimNormal
	return v, &e
}

func TestVimKeys(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		cursor int
		keys   string
		want   string
		at     int
		mode   VimMode
	}{
		// Motions.
		{"l", "abc", 0, "l", "abc", 1, VimNormal},
		{"count l", "abc", 0, "2l", "abc", 2, VimNormal},
		{"l stops on the last character", "abc", 0, "9l", "abc", 2, VimNormal},
		{"h", "abc", 2, "h", "abc", 1, VimNormal},
		{"h stops at the line start", "ab\ncd", 3, "5h", "ab\ncd", 3, VimNormal},
		{"0", "abc", 2, "0", "abc", 0, VimNormal},
		{"$", "abc", 0, "$", "abc", 2, VimNormal},
		{"^", "  ab", 3, "^", "  ab", 2, VimNormal},
		{"w", "foo bar baz", 0, "w", "foo bar baz", 4, VimNormal},
		{"2w", "foo bar baz", 0, "2w", "foo bar baz", 8, VimNormal},
		{"w on the last word stays on the line", "foo bar", 4, "w", "foo bar", 6, VimNormal},
		{"w crosses lines", "foo\nbar", 0, "w", "foo\nbar", 4, VimNormal},
		{"w stops on an empty line", "foo\n\nbar", 0, "w", "foo\n\nbar", 4, VimNormal},
		{"w tells punctuation from words", "a.b c", 0, "w", "a.b c", 1, VimNormal},
		{"b", "foo bar", 4, "b", "foo bar", 0, VimNormal},
		{"b from inside a word", "foo bar", 6, "b", "foo bar", 4, VimNormal},
		{"e", "foo bar", 0, "e", "foo bar", 2, VimNormal},
		{"e from a word's end", "foo bar", 2, "e", "foo bar", 6, VimNormal},
		{"j keeps the column", "abcd\nef", 3, "j", "abcd\nef", 6, VimNormal},
		{"k", "ab\ncd", 4, "k", "ab\ncd", 1, VimNormal},
		{"gg", "a\nb\nc", 4, "gg", "a\nb\nc", 0, VimNormal},
		{"G", "a\nb\nc", 0, "G", "a\nb\nc", 4, VimNormal},
		{"2G", "a\nb\nc", 0, "2G", "a\nb\nc", 2, VimNormal},
		{"arrows are hjkl", "ab\ncd", 0, "<Right><Down><Left><Up>", "ab\ncd", 0, VimNormal},
		{"Home and End", "abc", 1, "<End>", "abc", 2, VimNormal},
		{"Home", "abc", 2, "<Home>", "abc", 0, VimNormal},
		{"Backspace is h", "abc", 2, "<BS>", "abc", 1, VimNormal},
		// Deleting.
		{"x", "abc", 1, "x", "ac", 1, VimNormal},
		{"x at the end clamps", "abc", 2, "x", "ab", 1, VimNormal},
		{"2x", "abc", 0, "2x", "c", 0, VimNormal},
		{"X", "abc", 2, "X", "ac", 1, VimNormal},
		{"Delete is x", "abc", 0, "<Del>", "bc", 0, VimNormal},
		{"dw", "foo bar", 0, "dw", "bar", 0, VimNormal},
		{"dw on the last word keeps the newline", "foo bar\nbaz", 4, "dw", "foo \nbaz", 3, VimNormal},
		{"d2w", "foo bar baz", 0, "d2w", "baz", 0, VimNormal},
		{"2dw", "foo bar baz", 0, "2dw", "baz", 0, VimNormal},
		{"de", "foo bar", 0, "de", " bar", 0, VimNormal},
		{"db", "foo bar", 4, "db", "bar", 0, VimNormal},
		{"d$", "abc", 1, "d$", "a", 0, VimNormal},
		{"D", "abc", 1, "D", "a", 0, VimNormal},
		{"d0", "abc", 2, "d0", "c", 0, VimNormal},
		{"dl", "abc", 0, "dl", "bc", 0, VimNormal},
		{"dh", "abc", 2, "dh", "ac", 1, VimNormal},
		{"dd", "a\nb\nc", 2, "dd", "a\nc", 2, VimNormal},
		{"dd on the first line", "a\nb", 0, "dd", "b", 0, VimNormal},
		{"dd on the last line", "a\nb", 2, "dd", "a", 0, VimNormal},
		{"dd on the only line", "abc", 1, "dd", "", 0, VimNormal},
		{"2dd", "a\nb\nc", 0, "2dd", "c", 0, VimNormal},
		{"d2d", "a\nb\nc", 0, "d2d", "c", 0, VimNormal},
		{"dj", "a\nb\nc", 0, "dj", "c", 0, VimNormal},
		{"dk on the first line does nothing", "a\nb", 0, "dk", "a\nb", 0, VimNormal},
		{"dG", "a\nb\nc", 2, "dG", "a", 0, VimNormal},
		{"dgg", "a\nb\nc", 2, "dgg", "c", 0, VimNormal},
		{"dd lands on the first non-blank", "a\n  b\nc", 0, "dd", "  b\nc", 2, VimNormal},
		{"an unknown operator target cancels", "abc", 0, "dz", "abc", 0, VimNormal},
		// Changing.
		{"cw changes to the word's end", "foo bar", 0, "cwxy<Esc>", "xy bar", 1, VimNormal},
		{"cw on blanks is dw", "a  b", 1, "cwX<Esc>", "aXb", 1, VimNormal},
		{"cc", "  foo\nbar", 3, "ccz<Esc>", "z\nbar", 0, VimNormal},
		{"C", "abc", 1, "Cz<Esc>", "az", 1, VimNormal},
		{"c$ on an empty line still inserts", "", 0, "c$z<Esc>", "z", 0, VimNormal},
		{"cj", "a\nb\nc", 0, "cjX<Esc>", "X\nc", 0, VimNormal},
		// Yank and put.
		{"yyp", "ab\ncd", 0, "yyp", "ab\nab\ncd", 3, VimNormal},
		{"yyP", "ab\ncd", 3, "yyP", "ab\ncd\ncd", 3, VimNormal},
		{"yyp on the last line", "ab\ncd", 3, "yyp", "ab\ncd\ncd", 6, VimNormal},
		{"yw then p", "foo bar", 0, "ywp", "ffoo oo bar", 4, VimNormal},
		{"yw then P", "foo bar", 0, "ywP", "foo foo bar", 3, VimNormal},
		{"ye moves the cursor to the start", "foo bar", 4, "yb", "foo bar", 0, VimNormal},
		{"xp swaps", "abc", 0, "xp", "bac", 1, VimNormal},
		{"ddp moves a line down", "a\nb\nc", 0, "ddp", "b\na\nc", 2, VimNormal},
		{"dd then p on the last line", "a\nb", 2, "ddp", "a\nb", 2, VimNormal},
		{"3p", "ab", 0, "ylP3p", "aaaaab", 3, VimNormal},
		{"p with an empty register", "ab", 0, "p", "ab", 0, VimNormal},
		// Inserting.
		{"i", "abc", 1, "iX<Esc>", "aXbc", 1, VimNormal},
		{"a", "abc", 1, "aX<Esc>", "abXc", 2, VimNormal},
		{"a at the end", "abc", 2, "aX<Esc>", "abcX", 3, VimNormal},
		{"I", "  abc", 4, "IX<Esc>", "  Xabc", 2, VimNormal},
		{"A", "abc", 0, "AX<Esc>", "abcX", 3, VimNormal},
		{"o", "abc", 0, "oX<Esc>", "abc\nX", 4, VimNormal},
		{"O", "abc", 1, "OX<Esc>", "X\nabc", 0, VimNormal},
		{"insert keys are the editor's", "ab", 0, "aXY<BS>Z<Esc>", "aXZb", 2, VimNormal},
		{"Esc from insert steps back", "abc", 0, "A<Esc>", "abc", 2, VimNormal},
		{"Esc at a line start stays", "abc", 0, "i<Esc>", "abc", 0, VimNormal},
		{"a newline in insert mode", "ab", 1, "a<NL>c<Esc>", "ab\nc", 3, VimNormal},
		// Undo and redo.
		{"u", "abc", 0, "xu", "abc", 0, VimNormal},
		{"u then Ctrl-R", "abc", 0, "xu<C-r>", "bc", 0, VimNormal},
		{"u undoes an insert session", "abc", 2, "AXY<Esc>u", "abc", 2, VimNormal},
		{"u twice", "abc", 0, "xxuu", "abc", 0, VimNormal},
		{"u on nothing", "abc", 1, "u", "abc", 1, VimNormal},
		{"a change drops the redo", "abc", 0, "xux<C-r>", "bc", 0, VimNormal},
		{"u after dd", "a\nb", 0, "ddu", "a\nb", 0, VimNormal},
		// Repeat.
		{"dw then .", "abc def ghi", 0, "dw.", "ghi", 0, VimNormal},
		{"x then . twice", "abcd", 0, "x..", "d", 0, VimNormal},
		{"3x then .", "abcdefgh", 0, "3x.", "gh", 0, VimNormal},
		{"2. gives the count", "abcdef", 0, "x2.", "def", 0, VimNormal},
		{". repeats an insert", "ab", 0, "iX<Esc>.", "XXab", 0, VimNormal},
		{". repeats an append on another line", "a\nb", 0, "A!<Esc>j.", "a!\nb!", 4, VimNormal},
		{". repeats cw", "one two three", 0, "cwX<Esc>ww.", "X two X", 6, VimNormal},
		{". after a motion repeats the change, not the motion", "abcd", 0, "xl.", "bd", 1, VimNormal},
		{". repeats p", "ab", 0, "ylp.", "aaab", 2, VimNormal},
		{"yank is not repeated", "abc", 0, "xyl.", "c", 0, VimNormal},
		{". with nothing", "abc", 0, ".", "abc", 0, VimNormal},
		// Pending state.
		{"Esc clears a count", "abc", 0, "2<Esc>l", "abc", 1, VimNormal},
		{"Esc clears an operator", "abc", 0, "d<Esc>l", "abc", 1, VimNormal},
		{"0 after a count is a digit", "abcdefghijkl", 0, "10l", "abcdefghijkl", 10, VimNormal},
		{"g then another key cancels", "abc", 2, "gx", "abc", 2, VimNormal},
		// The lines.
		{": then Esc", "abc", 0, ":wq<Esc>", "abc", 0, VimNormal},
		{": then Backspace out", "abc", 0, ":w<BS><BS>", "abc", 0, VimNormal},
		{"/ then Esc", "abc", 0, "/x<Esc>", "abc", 0, VimNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := normalVim(tc.text, tc.cursor)
			vimDrive(v, e, tc.keys)
			if e.Text() != tc.want || e.Cursor() != tc.at || v.Mode != tc.mode {
				t.Fatalf("keys %q on %q@%d: got %q@%d %v, want %q@%d %v", tc.keys, tc.text, tc.cursor, e.Text(), e.Cursor(), v.Mode, tc.want, tc.at, tc.mode)
			}
		})
	}
}

func TestVimActions(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		keys   string
		action VimAction
		out    string
	}{
		{"Enter sends", "hello", "<CR>", VimSend, ""},
		{":w sends", "hello", ":w<CR>", VimSend, ""},
		{":wq sends and quits", "hello", ":wq<CR>", VimSend, "quit"},
		{":x is :wq", "hello", ":x<CR>", VimSend, "quit"},
		{":q quits", "hello", ":q<CR>", VimQuit, ""},
		{":q! quits", "hello", ":q!<CR>", VimQuit, ""},
		{":set novim", "hello", ":set novim<CR>", VimDisable, ""},
		{":set vim says it is on", "hello", ":set vim<CR>", VimNotice, "vim mode is on; :set novim turns it off"},
		{"an unknown command", "hello", ":foo<CR>", VimNotice, "not a vim command: :foo · :w sends, :q quits, :wq, :set novim"},
		{"/ finds", "hello", "/needle<CR>", VimFind, "needle"},
		{"/ with nothing yet", "hello", "/<CR>", VimNotice, "/TEXT searches the transcript upward; n repeats"},
		{"n with nothing yet", "hello", "n", VimNotice, "nothing searched yet; /TEXT searches the transcript"},
		{"Esc with nothing pending is the app's", "hello", "<Esc>", VimEscape, ""},
		{"G on an empty draft follows the transcript", "", "G", VimScroll, "bottom"},
		{"gg on an empty draft goes to the top", "", "gg", VimScroll, "top"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := normalVim(tc.text, 0)
			got := vimDrive(v, e, tc.keys)
			if len(got) != 1 || got[0].Action != tc.action || got[0].Text != tc.out {
				t.Fatalf("keys %q: got %+v, want %v %q", tc.keys, got, tc.action, tc.out)
			}
			if v.Mode != VimNormal {
				t.Fatalf("mode after %q: %v", tc.keys, v.Mode)
			}
		})
	}
	// n repeats the last search; / with an empty line does the same.
	v, e := normalVim("x", 0)
	got := vimDrive(v, e, "/abc<CR>n/<CR>")
	if len(got) != 3 || got[1].Text != "abc" || got[2].Text != "abc" || got[2].Action != VimFind {
		t.Fatalf("n and / repeat: %+v", got)
	}
	// The line shows with its prompt while typed.
	vimDrive(v, e, ":set")
	if v.Line() != ":set" || v.Mode.String() != "NORMAL" {
		t.Fatalf("line %q mode %s", v.Line(), v.Mode)
	}
	vimDrive(v, e, "<Esc>/find")
	if v.Line() != "/find" {
		t.Fatalf("search line %q", v.Line())
	}
	vimDrive(v, e, "<Esc>")
	if v.Line() != "" {
		t.Fatalf("line after Esc %q", v.Line())
	}
}

func TestVimInsertModeAndPassThrough(t *testing.T) {
	// Off: every key passes.
	var v Vim
	var e Editor
	if r := v.Handle(&e, Key{Kind: KeyRune, Rune: 'x'}); r.Action != VimPass {
		t.Fatalf("off: %+v", r)
	}
	// On: insert mode passes everything but Esc, which enters normal
	// mode without the app seeing it.
	v.Enable()
	if v.Mode != VimInsert || v.Mode.String() != "INSERT" {
		t.Fatalf("mode %v", v.Mode)
	}
	for _, k := range []Key{{Kind: KeyRune, Rune: 'a'}, {Kind: KeyUp}, {Kind: KeyCtrlP}, {Kind: KeyTab}, {Kind: KeyCtrlC}, {Kind: KeyPaste, Text: "x"}} {
		if r := v.Handle(&e, k); r.Action != VimPass {
			t.Fatalf("insert %v: %+v", k, r)
		}
	}
	e.Set("hello")
	if r := v.Handle(&e, Key{Kind: KeyEscape}); r.Action != VimHandled || v.Mode != VimNormal || e.Cursor() != 4 {
		t.Fatalf("Esc: %+v mode %v cursor %d", r, v.Mode, e.Cursor())
	}
	// Normal mode passes the app's keys.
	for _, k := range []Key{{Kind: KeyCtrlC}, {Kind: KeyCtrlD}, {Kind: KeyCtrlL}, {Kind: KeyCtrlO}, {Kind: KeyTab}, {Kind: KeyShiftTab}, {Kind: KeyPageUp}, {Kind: KeyWheelDown}, {Kind: KeyEOF}} {
		if r := v.Handle(&e, k); r.Action != VimPass {
			t.Fatalf("normal %v: %+v", k, r)
		}
	}
	// Home and End on an empty draft are the transcript's.
	e.Clear()
	if r := v.Handle(&e, Key{Kind: KeyEnd}); r.Action != VimPass {
		t.Fatalf("End on empty: %+v", r)
	}
	// The typing before the first Esc is one undo step.
	v.Reset()
	for _, k := range vimKeys("typed") {
		v.Handle(&e, k)
		e.Handle(k)
	}
	vimDrive(&v, &e, "<Esc>u")
	if e.Text() != "" {
		t.Fatalf("undo of the first insert: %q", e.Text())
	}
	vimDrive(&v, &e, "<C-r>")
	if e.Text() != "typed" {
		t.Fatalf("redo: %q", e.Text())
	}
	// j and k past the draft's edges recall the history.
	e.SetHistory([]string{"older", "old"})
	e.Clear()
	vimDrive(&v, &e, "k")
	if e.Text() != "old" || e.Cursor() != 2 {
		t.Fatalf("k recalls: %q@%d", e.Text(), e.Cursor())
	}
	vimDrive(&v, &e, "kj")
	if e.Text() != "old" {
		t.Fatalf("k then j: %q", e.Text())
	}
	// Reset after a send: insert mode, nothing to undo.
	v.Reset()
	if v.Mode != VimInsert || len(v.undo) != 0 {
		t.Fatalf("reset: %v %d", v.Mode, len(v.undo))
	}
}
