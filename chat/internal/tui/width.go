package tui

// Column widths. The transcript is painted into the terminal's normal
// buffer and the live tail is rewritten in place by moving the cursor up
// by the rows it took, so every line must take exactly the rows the
// painter counted: a line the terminal wraps because a rune took two
// columns would leave a stale row behind at every paint. Widths are
// therefore measured in columns, not runes — wide East Asian text and
// emoji take two, combining marks, joiners and variation selectors none.
// An over-estimate only wraps a line early; an under-estimate corrupts the
// screen, so doubtful emoji count as wide.

// runeWidth is the columns r takes on the terminal.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0 // controls never reach the screen (sanitize)
	case r < 0x300:
		return 1
	case zeroWidth(r):
		return 0
	case wide(r):
		return 2
	}
	return 1
}

// zeroWidth reports the combining marks, joiners, format characters and
// variation selectors that take no column of their own.
func zeroWidth(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036f, // combining diacriticals
		r >= 0x0483 && r <= 0x0489,
		r >= 0x0591 && r <= 0x05bd,
		r >= 0x0610 && r <= 0x061a,
		r >= 0x064b && r <= 0x065f,
		r >= 0x0e31 && r <= 0x0e3a && r != 0x0e32 && r != 0x0e33,
		r >= 0x0e47 && r <= 0x0e4e,
		r >= 0x1ab0 && r <= 0x1aff,
		r >= 0x1dc0 && r <= 0x1dff,
		r >= 0x200b && r <= 0x200f, // zero-width space, joiners, marks
		r >= 0x2028 && r <= 0x202e,
		r >= 0x2060 && r <= 0x2064,
		r >= 0x20d0 && r <= 0x20ff,
		r >= 0x302a && r <= 0x302d,
		r >= 0x3099 && r <= 0x309a,
		r >= 0xfe00 && r <= 0xfe0f, // variation selectors
		r >= 0xfe20 && r <= 0xfe2f,
		r == 0xfeff,
		r >= 0x1f3fb && r <= 0x1f3ff, // emoji skin tones
		r >= 0xe0020 && r <= 0xe007f, // tags
		r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// wide reports the runes that take two columns: East Asian wide and
// fullwidth forms, and the emoji that terminals render wide without a
// variation selector.
func wide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, punctuation
		r >= 0x3041 && r <= 0x33ff, // kana, Hangul compatibility, enclosed CJK
		r >= 0x3400 && r <= 0x4dbf,
		r >= 0x4e00 && r <= 0x9fff, // CJK unified ideographs
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xa960 && r <= 0xa97f,
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1b000 && r <= 0x1b2ff,
		r >= 0x20000 && r <= 0x2fffd,
		r >= 0x30000 && r <= 0x3fffd:
		return true
	}
	return emojiWide(r)
}

// emojiWide is the Emoji_Presentation set: what a terminal draws as a
// two-column picture on its own (no U+FE0F needed). Symbols outside it —
// ⚠ ✓ ✗ ☐ — are one column until a variation selector follows them.
func emojiWide(r rune) bool {
	switch {
	case r == 0x231a || r == 0x231b,
		r >= 0x23e9 && r <= 0x23ec,
		r == 0x23f0 || r == 0x23f3,
		r == 0x25fd || r == 0x25fe,
		r == 0x2614 || r == 0x2615,
		r >= 0x2648 && r <= 0x2653,
		r == 0x267f || r == 0x2693 || r == 0x26a1,
		r == 0x26aa || r == 0x26ab,
		r == 0x26bd || r == 0x26be,
		r == 0x26c4 || r == 0x26c5,
		r == 0x26ce || r == 0x26d4 || r == 0x26ea,
		r == 0x26f2 || r == 0x26f3 || r == 0x26f5 || r == 0x26fa || r == 0x26fd,
		r == 0x2705,
		r == 0x270a || r == 0x270b,
		r == 0x2728 || r == 0x274c || r == 0x274e,
		r >= 0x2753 && r <= 0x2755,
		r == 0x2757,
		r >= 0x2795 && r <= 0x2797,
		r == 0x27b0 || r == 0x27bf,
		r == 0x2b1b || r == 0x2b1c || r == 0x2b50 || r == 0x2b55,
		r == 0x1f004 || r == 0x1f0cf || r == 0x1f18e,
		r >= 0x1f191 && r <= 0x1f19a,
		r >= 0x1f1e6 && r <= 0x1f1ff, // regional indicators (flags)
		r == 0x1f201 || r == 0x1f21a || r == 0x1f22f,
		r >= 0x1f232 && r <= 0x1f236,
		r >= 0x1f238 && r <= 0x1f23a,
		r == 0x1f250 || r == 0x1f251,
		r >= 0x1f300 && r <= 0x1f320,
		r >= 0x1f32d && r <= 0x1f335,
		r >= 0x1f337 && r <= 0x1f37c,
		r >= 0x1f37e && r <= 0x1f393,
		r >= 0x1f3a0 && r <= 0x1f3ca,
		r >= 0x1f3cf && r <= 0x1f3d3,
		r >= 0x1f3e0 && r <= 0x1f3f0,
		r == 0x1f3f4,
		r >= 0x1f3f8 && r <= 0x1f43e,
		r == 0x1f440,
		r >= 0x1f442 && r <= 0x1f4fc,
		r >= 0x1f4ff && r <= 0x1f53d,
		r >= 0x1f54b && r <= 0x1f54e,
		r >= 0x1f550 && r <= 0x1f567,
		r == 0x1f57a,
		r == 0x1f595 || r == 0x1f596,
		r == 0x1f5a4,
		r >= 0x1f5fb && r <= 0x1f64f,
		r >= 0x1f680 && r <= 0x1f6c5,
		r == 0x1f6cc,
		r >= 0x1f6d0 && r <= 0x1f6d2,
		r >= 0x1f6d5 && r <= 0x1f6d7,
		r >= 0x1f6dc && r <= 0x1f6df,
		r == 0x1f6eb || r == 0x1f6ec,
		r >= 0x1f6f4 && r <= 0x1f6fc,
		r >= 0x1f7e0 && r <= 0x1f7eb,
		r == 0x1f7f0,
		r >= 0x1f90c && r <= 0x1f93a,
		r >= 0x1f93c && r <= 0x1f945,
		r >= 0x1f947 && r <= 0x1f9ff,
		r >= 0x1fa70 && r <= 0x1fa7c,
		r >= 0x1fa80 && r <= 0x1fa89,
		r >= 0x1fa8f && r <= 0x1fac6,
		r >= 0x1face && r <= 0x1fadc,
		r >= 0x1fadf && r <= 0x1fae9,
		r >= 0x1faf0 && r <= 0x1faf8:
		return true
	}
	return false
}

// textWidth is the columns of plain text (no escape sequences): the sum
// of its runes' widths, where U+FE0F after a one-column symbol asks for
// the emoji picture and makes it two columns.
func textWidth(s string) int {
	w := 0
	prev := 0
	for _, r := range s {
		if r == 0xfe0f && prev == 1 {
			w++
			prev = 2
			continue
		}
		rw := runeWidth(r)
		w += rw
		if rw != 0 {
			prev = rw
		}
	}
	return w
}
