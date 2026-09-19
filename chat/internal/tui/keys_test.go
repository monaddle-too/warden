package tui

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// keyKindNames are the kinds by the names editor.go declares them with,
// read from the source so a new kind cannot be added without a row.
func keyKindNames(t *testing.T) map[string]KeyKind {
	t.Helper()
	src, err := os.ReadFile("editor.go")
	if err != nil {
		t.Fatal(err)
	}
	block := string(src)
	block = block[strings.Index(block, "KeyRune KeyKind = iota"):]
	block = block[:strings.Index(block, "\n)")]
	names := map[string]KeyKind{}
	kind := KeyKind(0)
	for _, line := range strings.Split(block, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "Key") {
			continue
		}
		names[f[0]] = kind
		kind++
	}
	if names["KeyEOF"] != KeyEOF || names["KeyShiftTab"] != KeyShiftTab {
		t.Fatalf("editor.go's kinds misread: %v", names)
	}
	return names
}

// funcBody is the text of a method's body in app.go.
func funcBody(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (a *App) "+name+"(")
	if i < 0 {
		t.Fatalf("no %s in app.go", name)
	}
	s = s[i:]
	return s[:strings.Index(s, "\n}\n")]
}

func kindsOf(rows []binding, mode string) map[KeyKind]bool {
	out := map[KeyKind]bool{}
	for _, b := range rows {
		if b.mode != mode {
			continue
		}
		for _, k := range b.kinds {
			out[k] = true
		}
	}
	return out
}

// The key table is what the keys do: every kind the decoder produces is a
// row (typing, pasting and the unknown key aside); every kind the editor
// consumes is an editor row; every kind the menu and the prompt search
// switch on is a row of that mode; the app's own handlers are one per
// kind, reachable through bindingFor.
func TestKeyTableCoversEveryBoundKey(t *testing.T) {
	names := keyKindNames(t)
	listed := map[KeyKind]bool{}
	for _, b := range keyBindings {
		for _, k := range b.kinds {
			listed[k] = true
		}
		if b.keys == "" || b.what == "" || b.area == "" {
			t.Fatalf("a row without keys, text or area: %+v", b)
		}
		if (b.mode == modeText || b.mode == modePrefix) != (b.kinds == nil) {
			t.Fatalf("a row's kinds disagree with its mode: %+v", b)
		}
		if b.mode == "" && b.run == nil && b.when == "" {
			t.Fatalf("an app row without a handler or a condition: %+v", b)
		}
		if b.mode != "" && b.run != nil {
			t.Fatalf("a %s row with a handler: %+v", b.mode, b)
		}
	}
	for name, kind := range names {
		switch name {
		case "KeyRune", "KeyPaste", "KeyUnknown":
			continue
		}
		if !listed[kind] {
			t.Errorf("%s is not in the key table", name)
		}
	}
	// The editor's keys.
	editorRows := kindsOf(keyBindings, modeEditor)
	for name, kind := range names {
		switch name {
		case "KeyRune", "KeyPaste":
			continue
		}
		e := &Editor{}
		e.Set("two words")
		if e.Handle(Key{Kind: kind}) && !editorRows[kind] {
			t.Errorf("the editor handles %s but no editor row lists it", name)
		}
		if !e.Handle(Key{Kind: kind}) && editorRows[kind] {
			t.Errorf("%s is listed as the editor's but the editor ignores it", name)
		}
	}
	// The menu's and the search's keys, from their switches.
	ident := regexp.MustCompile(`\bKey[A-Z][A-Za-z]+\b`)
	for _, sw := range []struct{ fn, mode string }{{"menuKey", modeMenu}, {"searchKey", modeSearch}} {
		rows := kindsOf(keyBindings, sw.mode)
		for _, name := range ident.FindAllString(funcBody(t, sw.fn), -1) {
			if name == "KeyKind" {
				continue
			}
			kind, ok := names[name]
			if !ok {
				t.Fatalf("%s uses %s, which editor.go does not declare", sw.fn, name)
			}
			if !rows[kind] {
				t.Errorf("%s handles %s but no %s row lists it", sw.fn, name, sw.mode)
			}
		}
	}
	// The app's own handlers: one per kind.
	owners := map[KeyKind]string{}
	for _, b := range keyBindings {
		if b.run == nil {
			continue
		}
		for _, k := range b.kinds {
			if prev, dup := owners[k]; dup {
				t.Errorf("%s handled twice: %q and %q", kindName(names, k), prev, b.keys)
			}
			owners[k] = b.keys
			if bindingFor(k) == nil || bindingFor(k).keys != b.keys {
				t.Errorf("bindingFor(%s) is not the %q row", kindName(names, k), b.keys)
			}
		}
	}
	if bindingFor(KeyCtrlA) != nil || bindingFor(KeyRune) != nil {
		t.Fatal("the editor's keys reached the app's table")
	}
	// handleKey has no switch of its own left.
	if body := funcBody(t, "handleKey"); strings.Contains(body, "switch k.Kind") || !strings.Contains(body, "bindingFor(k.Kind)") {
		t.Fatalf("handleKey does not dispatch through the table:\n%s", body)
	}
}

func kindName(names map[string]KeyKind, kind KeyKind) string {
	for n, k := range names {
		if k == kind {
			return n
		}
	}
	return "?"
}

// /keys prints every row by area with its condition, the columns
// aligned; a mode's extra rows (vim, when it lands) join; /help points
// at it and no longer lists keys itself.
func TestKeysCommandPrintsTheTable(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Keys", Provider: "claude", Status: "idle"}}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	app.refreshState(ctx)
	app.submit(ctx, "/keys")
	text := plain(app.notice)
	for _, area := range keyAreas {
		if !strings.Contains(text, "\n"+area.title+"\n") && !strings.HasPrefix(text, area.title+"\n") {
			t.Errorf("/keys lacks the %s heading:\n%s", area.id, text)
		}
	}
	for _, b := range keyBindings {
		if !strings.Contains(text, "  "+b.keys) || !strings.Contains(text, b.what) {
			t.Errorf("/keys lacks %q:\n%s", b.keys, text)
		}
		if b.when != "" && !strings.Contains(text, "["+b.when+"]") {
			t.Errorf("/keys lacks the condition of %q", b.keys)
		}
	}
	for _, want := range []string{"  Shift+Tab", "  Esc Esc", "  Ctrl+R", "  y, yes", "  A ", "  n [message]", "  /", "  @", "  !cmd", "  #note", "  Up ", "  Ctrl+O", "  Tab "} {
		if !strings.Contains(text, want) {
			t.Errorf("/keys lacks %q", want)
		}
	}
	// Vim mode's rows are there under their heading (round 2 F), whether
	// or not the mode is on.
	if !strings.Contains(text, "\nvim mode (/vim on") || !strings.Contains(text, "  h j k l w b e 0 ^ $ gg G") {
		t.Fatalf("/keys lacks the vim rows:\n%s", text)
	}
	// The keys column is aligned: every row's text starts at one column
	// (the widest keys label plus the four spaces around it).
	width := 0
	for _, b := range append(append([]binding(nil), keyBindings...), extraKeys(app)...) {
		width = max(width, len([]rune(b.keys)))
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		r := []rune(line)
		if len(r) < width+4 || r[width+2] != ' ' || r[width+3] != ' ' || r[width+4] == ' ' {
			t.Errorf("row not aligned at column %d: %q", width+4, line)
		}
	}
	if !strings.Contains(helpText, "/keys") || strings.Contains(helpText, "Shift+Tab") || strings.Contains(helpText, "Ctrl+O") {
		t.Fatalf("/help lists keys itself:\n%s", helpText)
	}
	// The hook's rows join the listing; an area without rows is not
	// printed.
	was := extraKeys
	extraKeys = func(*App) []binding {
		return []binding{{keys: "zz", area: areaTranscript, what: "a hook's row", mode: modeText}}
	}
	defer func() { extraKeys = was }()
	app.submit(ctx, "/keys")
	if hooked := plain(app.notice); !strings.Contains(hooked, "  zz") || strings.Contains(hooked, "vim mode (") {
		t.Fatalf("the hook's rows:\n%s", hooked)
	}
}

// Vim mode's rows name every key vim.go switches on: the runes of
// normal mode and its motions, the keys of its command line, and the
// commands the line accepts.
func TestVimKeysCoverVimGo(t *testing.T) {
	src, err := os.ReadFile("vim.go")
	if err != nil {
		t.Fatal(err)
	}
	body := func(name string) string {
		s := string(src)
		i := strings.Index(s, "func "+name)
		if i < 0 {
			t.Fatalf("no %s in vim.go", name)
		}
		s = s[i:]
		return s[:strings.Index(s, "\n}\n")]
	}
	var labels []string
	for _, b := range vimRows(&App{}) {
		labels = append(labels, b.keys, b.what)
	}
	all := strings.Join(labels, " ")
	runeCase := regexp.MustCompile(`case ('[^']+'(?:, '[^']+')*)`)
	for _, fn := range []string{"(v *Vim) normalRune", "motion("} {
		for _, m := range runeCase.FindAllStringSubmatch(body(fn), -1) {
			for _, lit := range strings.Split(m[1], ", ") {
				r := strings.Trim(lit, "'")
				if !strings.Contains(all, r) {
					t.Errorf("vim.go %s handles %q, which no vim row names", fn, r)
				}
			}
		}
	}
	for _, cmd := range []string{":w", ":wq", ":q", ":set novim", ":set vim"} {
		if !strings.Contains(all, cmd) {
			t.Errorf("vim.go's command %q is not in the vim rows", cmd)
		}
	}
	if !strings.Contains(body("(v *Vim) command"), `"set novim"`) {
		t.Fatal("vim.go's command line no longer knows :set novim; the rows are stale")
	}
}

// The table's handlers do what the switch did (the keys that leave a
// visible trace without a chat or a terminal).
func TestKeyTableHandlersAct(t *testing.T) {
	f := newFakeServer(t, State{Chats: []*Chat{{ID: "chat1", Title: "Keys", Provider: "claude", Status: "idle"}}})
	ctx := context.Background()
	app := &App{Client: f.client(), ChatID: "chat1", Output: io.Discard, Now: func() time.Time { return time.Unix(0, 0) }}
	app.refreshState(ctx)
	app.handleKey(ctx, Key{Kind: KeyCtrlO})
	if !app.quiet || !strings.HasPrefix(app.notice, "hiding tool steps") {
		t.Fatalf("Ctrl+O: quiet %v, %q", app.quiet, app.notice)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlL})
	if !app.redraw {
		t.Fatal("Ctrl+L did not ask for a redraw")
	}
	app.handleKey(ctx, Key{Kind: KeyTab})
	if !app.expanded {
		t.Fatal("Tab on an empty draft did not expand")
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlR})
	if app.search == nil {
		t.Fatal("Ctrl+R did not open the search")
	}
	app.handleKey(ctx, Key{Kind: KeyEscape})
	if app.search != nil {
		t.Fatal("Esc did not leave the search")
	}
	for _, r := range "abc" {
		app.handleKey(ctx, Key{Kind: KeyRune, Rune: r})
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlA})
	app.handleKey(ctx, Key{Kind: KeyRight})
	app.handleKey(ctx, Key{Kind: KeyCtrlK})
	if app.editor.Text() != "a" {
		t.Fatalf("the editor's keys: %q", app.editor.Text())
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlC})
	if app.editor.Text() != "" || !strings.HasPrefix(app.notice, "draft cleared") {
		t.Fatalf("Ctrl+C: %q %q", app.editor.Text(), app.notice)
	}
	app.handleKey(ctx, Key{Kind: KeyCtrlD})
	if !app.quit {
		t.Fatal("Ctrl+D on an empty draft did not quit")
	}
	app.quit = false
	app.handleKey(ctx, Key{Kind: KeyEOF})
	if !app.quit {
		t.Fatal("EOF did not quit")
	}
}
