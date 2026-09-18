package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Completion in the composer, the pure parts (the web's composer.ts): a
// command is a draft that starts with "/" (its first line is the command
// and the argument); a mention is an "@" at the start of a word, and the
// text from it to the caret is the workspace path prefix to complete. A
// command's argument can have its own list (a local file for /attach, a
// chat for /switch). Picking a suggestion replaces the trigger's range;
// nothing else about the text changes, and a mention stays in the text as
// written for the agent to read.

// Command is one slash command: its name, the argument it takes (empty
// for none) and a hint for the menu.
type Command struct {
	Name string
	Arg  string
	Hint string
}

// Commands is the `/` menu, in the order it shows them.
var Commands = []Command{
	{"help", "", "show the commands and keys"},
	{"new", "[TITLE]", "start a chat on a fresh workspace"},
	{"chats", "", "list chats"},
	{"switch", "N", "open chat N"},
	{"rename", "TITLE", "rename this chat"},
	{"archive", "", "archive this chat (stopped chats only)"},
	{"restore", "", "bring an archived chat back"},
	{"delete", "", "delete this chat's workspace and its files"},
	{"stop", "", "interrupt the agent's turn (Esc)"},
	{"model", "MODEL", "set the model for the next turn"},
	{"provider", "codex|claude", "set the provider for the next turn"},
	{"mode", "auto|ask|plan", "permission mode of a Claude chat (Shift+Tab cycles)"},
	{"thinking", "on|off|TOKENS", "how much a Claude chat thinks: the model decides, none, or a budget (8k)"},
	{"effort", "low|medium|high|xhigh|max|default", "effort level of a Claude chat"},
	{"fast", "on|off", "fast mode of a Claude chat, where Warden allows it"},
	{"attach", "PATH", "send a local file with the next message"},
	{"attachments", "", "list the files waiting to be sent"},
	{"detach", "N", "drop a waiting file"},
	{"export", "[md|json] [all] [FILE]", "write the transcript to a file"},
	{"rewind", "[N] [code|conv|both]", "go back to before message N: its code, the conversation or both"},
	{"edit", "[N] [both]", "edit message N: a queued one in place (Enter saves it into its slot), a sent one is rewound to before and sent again (↑ edits the last queued, Esc Esc the last sent)"},
	{"queue", "[send]", "list the messages queued behind the agent's turn; send lets a held queue go"},
	{"withdraw", "N", "drop queued message N (N from /queue)"},
	{"undo-rewind", "[code]", "put back what the last conversation rewind removed; code restores the workspace as it was before the rewind too"},
	{"diff", "", "show or hide what changed in the workspace since this chat began"},
	{"instructions", "[edit|clear]", "your standing instructions for the agent, in every chat"},
	{"memory", "[FILE | edit FILE]", "the workspace's CLAUDE.md, rules and auto-memory files"},
	{"fork", "[N|all]", "copy this chat into a sibling (before message N, or the whole of it)"},
	{"btw", "QUESTION", "a side question answered from this chat's context, never sent to the agent"},
	{"cost", "", "this chat's turns, tokens and cost so far"},
	{"style", "[default|Explanatory|Learning]", "Claude's output style for the next session"},
	{"bell", "[on|off]", "ring the terminal bell when the agent finishes, asks or fails"},
	{"copy", "", "put the agent's last reply on the clipboard"},
	{"find", "TEXT", "scroll to the previous line containing TEXT"},
	{"expand", "", "toggle full tool output and diffs (Tab)"},
	{"verbose", "", "show or hide tool steps and thinking (Ctrl+O)"},
	{"open", "", "open this chat in the browser"},
	{"previews", "", "list published previews"},
	{"preview", "N", "open preview N in the browser"},
	{"unpublish", "N", "take preview N down"},
	{"clear", "", "discard the draft and its attachments"},
	{"quit", "", "leave (Ctrl+D)"},
}

// Trigger is the completion the caret asks for.
type Trigger struct {
	// Kind is "command" (the name after "/"), "path" (an @-mention),
	// "local" (a local file for /attach) or "chat" (a chat for /switch).
	Kind string
	// Start and End bound the runes replaced when a suggestion is picked.
	Start, End int
	// Query is what was typed after the "/" or "@", up to the caret.
	Query string
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// argumentKinds are the commands whose argument has its own list.
var argumentKinds = map[string]string{"attach": "local", "switch": "chat"}

// triggerAt finds the trigger at the caret, if any.
func triggerAt(text []rune, caret int) (Trigger, bool) {
	caret = max(0, min(caret, len(text)))
	if len(text) > 0 && text[0] == '/' {
		end := len(text)
		for i, r := range text {
			if r == '\n' {
				end = i
				break
			}
		}
		if caret < 1 || caret > end {
			return Trigger{}, false
		}
		line := string(text[1:end])
		name, _, hasArg := strings.Cut(line, " ")
		if !hasArg || caret <= 1+len([]rune(name)) {
			return Trigger{Kind: "command", Start: 0, End: 1 + len([]rune(name)), Query: string(text[1:min(caret, 1+len([]rune(name)))])}, true
		}
		kind, ok := argumentKinds[strings.ToLower(name)]
		if !ok {
			return Trigger{}, false
		}
		argStart := 1 + len([]rune(name)) + 1
		for argStart < end && text[argStart] == ' ' {
			argStart++
		}
		if caret < argStart {
			return Trigger{}, false
		}
		return Trigger{Kind: kind, Start: argStart, End: end, Query: string(text[argStart:caret])}, true
	}
	start := caret
	for start > 0 && !isSpace(text[start-1]) {
		start--
	}
	if start >= len(text) || text[start] != '@' || caret == start {
		return Trigger{}, false
	}
	end := caret
	for end < len(text) && !isSpace(text[end]) {
		end++
	}
	return Trigger{Kind: "path", Start: start, End: end, Query: string(text[start+1 : caret])}, true
}

// commandItems are the commands whose name starts with the word typed;
// extra (commands the chat offers, when it does) come after the built-ins.
func commandItems(query string, extra []Command) []Command {
	name := strings.ToLower(strings.TrimLeftFunc(query, unicode.IsSpace))
	var out []Command
	for _, c := range append(append([]Command(nil), Commands...), extra...) {
		if strings.HasPrefix(c.Name, name) {
			out = append(out, c)
		}
	}
	return out
}

// mentionFor is what a picked path becomes in the text: a file ends the
// mention with a space, a directory keeps the caret after its slash so the
// next segment can be completed.
func mentionFor(path string) string {
	if strings.HasSuffix(path, "/") {
		return "@" + path
	}
	return "@" + path + " "
}

// replaceRange puts insert in place of the runes [start, end) and returns
// the text and where the caret goes.
func replaceRange(text []rune, start, end int, insert string) ([]rune, int) {
	ins := []rune(insert)
	out := make([]rune, 0, len(text)-(end-start)+len(ins))
	out = append(out, text[:start]...)
	out = append(out, ins...)
	out = append(out, text[end:]...)
	return out, start + len(ins)
}

// expandHome turns a leading ~ into the home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// localPaths completes a local path for /attach, shell style: the entries
// of the directory the query names whose name starts with its last
// segment, directories first with a trailing slash. At most 50.
func localPaths(query string) []string {
	dir, stem := filepath.Split(query)
	lookup := expandHome(dir)
	if lookup == "" {
		lookup = "."
	}
	entries, err := os.ReadDir(lookup)
	if err != nil {
		return nil
	}
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(stem)) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(stem, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, dir+name+"/")
		} else {
			files = append(files, dir+name)
		}
	}
	lower := func(s []string) {
		sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i]) < strings.ToLower(s[j]) })
	}
	lower(dirs)
	lower(files)
	out := append(dirs, files...)
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

// chatItems are the chats a /switch argument can name: by number, or by a
// word of the title.
func chatItems(chats []*Chat, query string) []MenuItem {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []MenuItem
	for i, c := range chats {
		n := strconv.Itoa(i + 1)
		if q != "" && !strings.HasPrefix(n, q) && !strings.Contains(strings.ToLower(c.Title), q) {
			continue
		}
		out = append(out, MenuItem{Insert: n, Label: n + "  " + truncate(sanitize(c.Title), 40), Hint: c.Provider + " · " + c.Status, Run: true})
	}
	return out
}
