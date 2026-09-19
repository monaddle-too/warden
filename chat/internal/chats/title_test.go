package chats

import (
	"strings"
	"testing"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// claudeStreamedText is the CLI frame streaming a piece of the reply.
func claudeStreamedText(text string) map[string]any {
	return map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}}
}

// A Claude chat left at the default title is named after its first turn
// by a one-shot beside the resident session: the runner op carries the
// cheapest model, the naming system prompt and the first exchange; the
// answer is cleaned into the title, marked automatic, with no transcript
// entry; later turns leave it alone.
func TestAutoTitleAfterTheFirstTurn(t *testing.T) {
	e, w, _ := claudeResidentSetup(t)
	id, err := e.Create("", "", "", nil, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.Title != DefaultTitle || c.Titled != "" {
		t.Fatalf("new chat: %q %q", c.Title, c.Titled)
	}
	w.mu.Lock()
	w.frames = []map[string]any{claudeStreamedText("Hello! I can "), claudeStreamedText("help with the build.")}
	w.mu.Unlock()
	oneTurn(t, e, id, "hi there, can you fix the build?")
	until(t, func() bool { return e.Store.Snapshot().chat(id).Titled == "auto" })
	c := e.Store.Snapshot().chat(id)
	if c.Title != "Greeting the assistant" {
		t.Fatalf("title %q", c.Title)
	}
	shots := w.requestsOf("oneshot")
	if len(shots) != 1 || shots[0].Model != TitleModel || shots[0].Instructions != TitleSystemPrompt || shots[0].ChatID != id || shots[0].ThreadID != "" {
		t.Fatalf("oneshot request: %+v", shots)
	}
	if want := "User: hi there, can you fix the build?\n\nAssistant: Hello! I can help with the build."; shots[0].Command != want {
		t.Fatalf("prompt %q", shots[0].Command)
	}
	for _, v := range c.Conversation.Entries {
		if v.Role != "user" && v.Role != "assistant" {
			t.Fatalf("the naming left an entry: %+v", v)
		}
	}
	// A second turn does not name again.
	oneTurn(t, e, id, "thanks")
	until(t, func() bool { return e.sessionIdle(id) })
	if len(w.requestsOf("oneshot")) != 1 || e.Store.Snapshot().chat(id).Title != "Greeting the assistant" {
		t.Fatal("a titled chat was named again")
	}
	// A chat created with a title is never named.
	named, _ := e.Create("My own name", "", "", nil, "claude", "")
	if c := e.Store.Snapshot().chat(named); c.Titled != "manual" {
		t.Fatalf("created title: %+v", c.Titled)
	}
	oneTurn(t, e, named, "hello")
	until(t, func() bool { return e.sessionIdle(named) })
	if len(w.requestsOf("oneshot")) != 1 || e.Store.Snapshot().chat(named).Title != "My own name" {
		t.Fatal("a chat named at creation was renamed")
	}
}

// A rename wins: made before the turn it stops the naming, and one that
// lands while the one-shot runs is kept over its answer.
func TestRenameWinsOverAutoTitle(t *testing.T) {
	e, w, _ := claudeResidentSetup(t)
	id, _ := e.Create("", "", "", nil, "claude", "")
	if err := e.Edit(id, "Build fixes", false); err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(id); c.Titled != "manual" {
		t.Fatalf("after a rename: %+v", c.Titled)
	}
	oneTurn(t, e, id, "hi")
	until(t, func() bool { return e.sessionIdle(id) })
	if len(w.requestsOf("oneshot")) != 0 || e.Store.Snapshot().chat(id).Title != "Build fixes" {
		t.Fatal("a renamed chat was named")
	}
	// Archiving with the same title changes nothing about the naming.
	other, _ := e.Create("", "", "", nil, "claude", "")
	if err := e.Edit(other, DefaultTitle, true); err != nil {
		t.Fatal(err)
	}
	if c := e.Store.Snapshot().chat(other); c.Titled != "" || !c.Archived {
		t.Fatalf("archive: %+v", c)
	}
	// The one-shot's answer arrives after a rename: the rename stays.
	late, _ := e.Create("", "", "", nil, "claude", "")
	renamed := make(chan struct{})
	w.mu.Lock()
	w.oneshotGate = renamed
	w.mu.Unlock()
	oneTurn(t, e, late, "name me")
	until(t, func() bool { return len(w.requestsOf("oneshot")) == 1 })
	if err := e.Edit(late, "Chosen by hand", false); err != nil {
		t.Fatal(err)
	}
	close(renamed)
	until(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.titling[late]
	})
	if c := e.Store.Snapshot().chat(late); c.Title != "Chosen by hand" || c.Titled != "manual" {
		t.Fatalf("late answer won: %q %q", c.Title, c.Titled)
	}
}

// Without a one-shot — Codex, or a one-shot that fails — the first
// message's first line is the title, still marked automatic.
func TestAutoTitleFallsBackToTheFirstMessage(t *testing.T) {
	e, w := residentSetup(t)
	id, err := e.Create("", "", "", nil, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	sendAndDeliver(t, e, id, "# Please look at the flaky kube test\nIt times out under the full run.")
	completeTurn(t, e, w, id)
	until(t, func() bool { return e.Store.Snapshot().chat(id).Titled == "auto" })
	if c := e.Store.Snapshot().chat(id); c.Title != "Please look at the flaky kube test" {
		t.Fatalf("fallback title %q", c.Title)
	}
	if w.count("oneshot") != 0 {
		t.Fatal("Codex was asked for a one-shot")
	}
	// A failing one-shot on Claude falls back the same way.
	ce, cw, _ := claudeResidentSetup(t)
	cid, _ := ce.Create("", "", "", nil, "claude", "")
	cw.mu.Lock()
	cw.oneshot = &sandbox.AsideResult{Error: "API Error: 503"}
	cw.mu.Unlock()
	oneTurn(t, ce, cid, "why does go vet complain about the copied lock?")
	until(t, func() bool { return ce.Store.Snapshot().chat(cid).Titled == "auto" })
	if c := ce.Store.Snapshot().chat(cid); c.Title != "why does go vet complain about the copied lock?" {
		t.Fatalf("fallback after a failed one-shot: %q", c.Title)
	}
	// A run that is not resident (the process ends with the turn) has no
	// session to run the one-shot beside: the fallback names it.
	se, sw, _ := claudeSetup(t)
	sid, _ := se.Create("", "", "", nil, "claude", "")
	oneTurn(t, se, sid, "hello there")
	until(t, func() bool { return se.Store.Snapshot().chat(sid).Titled == "auto" })
	if c := se.Store.Snapshot().chat(sid); c.Title != "hello there" || len(sw.requestsOf("oneshot")) != 0 {
		t.Fatalf("non-resident: %q, %d one-shots", c.Title, len(sw.requestsOf("oneshot")))
	}
}

// Cleaning the model's answer, and the fallback's cut.
func TestCleanTitleAndFallbackTitle(t *testing.T) {
	for in, want := range map[string]string{
		"\"Fixing the flaky kube test.\"\n":            "Fixing the flaky kube test",
		"Title: Sandbox resize on GKE":                 "Sandbox resize on GKE",
		"  **Auto  titles   design**  ":                "Auto titles design",
		"\n\nGreeting the assistant\n\nIt says hello.": "Greeting the assistant",
		"":                          "",
		"“Quoted” title!":           "Quoted” title",
		strings.Repeat("word ", 30): strings.TrimSpace(strings.Repeat("word ", 12)),
	} {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := "Please have a look at why the Go tests in the sandbox package time out under the full run"
	for in, want := range map[string]string{
		"hi":                                 "hi",
		"\n\n  # Fix   the build \nand more": "Fix the build",
		"/btw what is this":                  "/btw what is this",
		long:                                 "Please have a look at why the Go tests in the sandbox…",
		strings.Repeat("x", 80):              strings.Repeat("x", 59) + "…",
		"   \n\t":                            "",
	} {
		if got := FallbackTitle(in); got != want {
			t.Errorf("FallbackTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FallbackTitle(long); len([]rune(got)) > titleLimit {
		t.Errorf("fallback over the limit: %d", len([]rune(got)))
	}
	user, reply := firstExchange([]cv.Entry{cv.NewEntry("system", "x"), cv.NewEntry("user", " first "), cv.NewEntry("thinking", "hm"), cv.NewEntry("assistant", ""), cv.NewEntry("assistant", "the reply"), cv.NewEntry("user", "second")})
	if user != "first" || reply != "the reply" {
		t.Fatalf("exchange %q %q", user, reply)
	}
	if user, reply := firstExchange([]cv.Entry{cv.NewEntry("user", "alone")}); user != "alone" || reply != "" {
		t.Fatalf("exchange %q %q", user, reply)
	}
	if p := titlePrompt("a", ""); p != "User: a" {
		t.Fatalf("prompt %q", p)
	}
}
