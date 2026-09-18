package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Attachments and pastes (docs/claude-parity.md, R2.17). /attach takes
// several paths, quoted or bare, each a local glob; a local mention in the
// draft — @./path, @../path or @~/path, a file on this machine rather
// than the workspace's — is uploaded when the message is sent and the
// mention rewritten to the file's workspace path; /paste N shows a
// collapsed paste (paste.go) in the pager, Ctrl+P on its placeholder
// previews it and cycles to the next, and the pastes are listed above
// the status bar while the draft holds them.

// localMention matches a mention of a local file in the draft: "@" then a
// path that starts with ./, ../ or ~/ (a workspace mention has no such
// prefix, so the two are told apart by it).
var localMention = regexp.MustCompile(`(^|\s)@((?:\./|\.\./|~/)\S+)`)

// mentionTrail is punctuation a mention does not include at its end.
const mentionTrail = ",.;:!?)]}'\""

// IsLocalPath reports whether a mention's path names a file on this
// machine rather than the workspace.
func IsLocalPath(p string) bool {
	return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "~/")
}

// LocalMentions lists the local mentions in text, as written, in order,
// without repeats.
func LocalMentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range localMention.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(m[2], mentionTrail)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// AttachArgs splits a /attach line into its paths: words, or quoted
// strings for a path with spaces, each expanded from ~ and as a glob
// (a pattern that matches nothing stays, so the error names it).
func AttachArgs(line string) []string {
	var words []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	var out []string
	seen := map[string]bool{}
	for _, w := range words {
		p := expandHome(w)
		matches := []string{p}
		if strings.ContainsAny(p, "*?[") {
			if m, err := filepath.Glob(p); err == nil && len(m) > 0 {
				matches = m
			}
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// attachFile uploads one local file for the chat's next message.
func (a *App) attachFile(ctx context.Context, c *Chat, path string) (Attachment, error) {
	if len(a.attachments[c.ID]) >= maxAttachments {
		return Attachment{}, fmt.Errorf("a message can carry at most %d attachments; /detach N drops one", maxAttachments)
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return Attachment{}, err
	case info.IsDir():
		return Attachment{}, errors.New(path + " is a directory; attach a file")
	case info.Size() == 0:
		return Attachment{}, errors.New(path + " is empty")
	case info.Size() > maxAttachmentBytes:
		return Attachment{}, errors.New(path + " is larger than 8 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, err
	}
	at, err := a.Client.Upload(ctx, c.ID, filepath.Base(path), data)
	if err != nil {
		return Attachment{}, err
	}
	if a.attachments == nil {
		a.attachments = map[string][]Attachment{}
	}
	a.attachments[c.ID] = append(a.attachments[c.ID], at)
	return at, nil
}

// attach uploads the files /attach names for the next message.
func (a *App) attach(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	paths := AttachArgs(arg)
	if len(paths) == 0 {
		a.setNotice("/attach PATH [PATH…] (local files, globs allowed; Tab completes)")
		return
	}
	var done, failed []string
	for _, p := range paths {
		at, err := a.attachFile(ctx, c, p)
		if err != nil {
			failed = append(failed, err.Error())
			if strings.HasPrefix(err.Error(), "a message can carry") {
				break
			}
			continue
		}
		done = append(done, fmt.Sprintf("%s (%s, %s)", sanitize(at.Name), at.Kind, FormatSize(at.Size)))
	}
	var b strings.Builder
	switch len(done) {
	case 0:
	case 1:
		fmt.Fprintf(&b, "attached %s; it goes with the next message as %s", done[0], a.attachments[c.ID][len(a.attachments[c.ID])-1].Path)
	default:
		fmt.Fprintf(&b, "attached %d files: %s; they go with the next message (/attachments lists their workspace paths)", len(done), strings.Join(done, ", "))
	}
	for _, f := range failed {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(sanitize(f))
	}
	a.setNotice(b.String())
}

// attachMentions uploads the files the draft's local mentions name and
// rewrites each mention to the upload's workspace path, so the agent
// reads the file where it lands. A mention that names no file is sent
// as written; a file that cannot be attached stops the send.
func (a *App) attachMentions(ctx context.Context, c *Chat, text string) (string, error) {
	var notes []string
	for _, p := range LocalMentions(text) {
		path := expandHome(p)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			notes = append(notes, "no local file "+p+"; sent as written")
			continue
		}
		at, err := a.attachFile(ctx, c, path)
		if err != nil {
			return text, fmt.Errorf("@%s: %s", p, err.Error())
		}
		text = strings.ReplaceAll(text, "@"+p, "@"+at.Path)
		notes = append(notes, fmt.Sprintf("attached %s (%s) as %s", sanitize(at.Name), FormatSize(at.Size), at.Path))
	}
	if len(notes) > 0 {
		a.setNotice(strings.Join(notes, "\n"))
	}
	return text, nil
}

// The pager: a text shown in place of the transcript until Esc, a send
// or a switch closes it.

type pagerView struct {
	title string
	lines []string
	paste int // the paste it shows, from 1; 0 for another text
}

// closePager takes the transcript back.
func (a *App) closePager() {
	if a.pager == nil {
		return
	}
	a.pager = nil
	a.scroll = 0
}

// showPaste opens the pager on paste n of the draft.
func (a *App) showPaste(n int) bool {
	pastes := a.editor.Pastes()
	if n < 1 || n > len(pastes) {
		return false
	}
	text := strings.TrimRight(pastes[n-1], "\n")
	lines := strings.Split(text, "\n")
	a.pager = &pagerView{title: fmt.Sprintf("Pasted text #%d — %d %s, %s · Esc closes · Ctrl+P on the placeholder cycles", n, len(lines), plural2(len(lines), "line"), FormatSize(int64(len(pastes[n-1])))), lines: lines, paste: n}
	a.scroll = 1 << 30 // the top; clamped by the frame
	return true
}

func plural2(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// pagerLines lays the pager out.
func (a *App) pagerLines(width int) []string {
	p := a.pager
	out := []string{bold + cyan + p.title + reset, ""}
	for i, l := range p.lines {
		num := fmt.Sprintf("%s%4d %s", dim, i+1, reset)
		out = append(out, wrap(sanitize(l), width, num, "     ")...)
	}
	return out
}

// PasteListing describes the draft's pastes, one line each.
func PasteListing(pastes []string) []string {
	var out []string
	for i, p := range pastes {
		text := strings.TrimRight(p, "\n")
		lines := strings.Count(text, "\n") + 1
		first, _, _ := strings.Cut(text, "\n")
		out = append(out, fmt.Sprintf("#%d  %d %s, %s: %s", i+1, lines, plural2(lines, "line"), FormatSize(int64(len(p))), truncate(sanitize(strings.TrimSpace(first)), 50)))
	}
	return out
}

// pasteCommand: /paste lists the draft's pastes, /paste N shows one,
// /paste close puts the transcript back.
func (a *App) pasteCommand(arg string) {
	pastes := a.editor.Pastes()
	switch strings.ToLower(arg) {
	case "":
		if len(pastes) == 0 {
			a.setNotice("no collapsed paste in the draft; a paste over 8 lines or 1000 characters becomes [Pasted text #N — M lines]")
			return
		}
		a.setNotice(strings.Join(append(PasteListing(pastes), "/paste N shows one in full; Ctrl+P on its placeholder previews it"), "\n"))
	case "close", "off":
		a.closePager()
	default:
		n, err := strconv.Atoi(arg)
		if err != nil || !a.showPaste(n) {
			a.setNotice("/paste N with N from /paste (the draft holds " + strconv.Itoa(len(pastes)) + ")")
		}
	}
}

// placeholderAt is the number of the paste placeholder under the cursor,
// 0 when the cursor is not on one.
func placeholderAt(text string, cursor int) int {
	runes := []rune(text)
	cursor = max(0, min(cursor, len(runes)))
	byteAt := len(string(runes[:cursor]))
	for _, m := range placeholder.FindAllStringSubmatchIndex(text, -1) {
		if m[0] <= byteAt && byteAt <= m[1] {
			n, _ := strconv.Atoi(text[m[2]:m[3]])
			return n
		}
	}
	return 0
}

// pastePreview is Ctrl+P on a placeholder: it opens the pager on that
// paste; while the pager is open it moves to the draft's next
// placeholder, and closes after the last. False when the cursor is not
// on a placeholder (Ctrl+P is then the history's).
func (a *App) pastePreview() bool {
	text := a.editor.Text()
	n := placeholderAt(text, a.editor.Cursor())
	if n == 0 {
		return false
	}
	if a.pager == nil {
		if !a.showPaste(n) {
			a.setNotice("placeholder #" + strconv.Itoa(n) + " names no paste of this draft")
		}
		return true
	}
	// Cycle: the placeholder after the one shown, in the draft's order.
	var order []int
	for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
		k, _ := strconv.Atoi(m[1])
		order = append(order, k)
	}
	for i, k := range order {
		if k == a.pager.paste {
			for _, next := range order[i+1:] {
				if next != k && a.showPaste(next) {
					return true
				}
			}
			break
		}
	}
	a.closePager()
	return true
}

// pasteChip is the line above the status bar while the draft holds
// collapsed pastes.
func pasteChip(pastes []string, width int) []string {
	if len(pastes) == 0 {
		return nil
	}
	var parts []string
	for i, p := range pastes {
		lines := strings.Count(strings.TrimRight(p, "\n"), "\n") + 1
		parts = append(parts, fmt.Sprintf("#%d %d %s (%s)", i+1, lines, plural2(lines, "line"), FormatSize(int64(len(p)))))
	}
	return wrap(strings.Join(parts, " · ")+dim+" · /paste N or Ctrl+P on the placeholder previews"+reset, width, cyan+"pasted: "+reset, "        ")
}
