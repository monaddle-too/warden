package policy

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The review page is a Tiptap (ProseMirror) document Warden computes from
// the proposal's base and draft: unchanged paragraphs as they are, changed
// text carrying suggestInsert / suggestDelete marks, changed blocks a
// suggestion attribute. Deleted text stays in the document — Google Docs'
// own model — so the page can strike it through in place and the margin
// can anchor to it. Saving strips it again (docToCanonical). Panta's docs
// editor renders the same node set; Warden vendors that layer.

// Node and mark names shared with the editor extension.
const (
	tipDoc          = "doc"
	tipParagraph    = "paragraph"
	tipHeading      = "heading"
	tipText         = "text"
	tipHardBreak    = "hardBreak"
	tipBulletList   = "bulletList"
	tipOrderedList  = "orderedList"
	tipListItem     = "listItem"
	tipFrozenBlock  = "frozenBlock"
	tipFrozenTable  = "frozenTable"
	markBold        = "bold"
	markItalic      = "italic"
	markLink        = "link"
	markInsert      = "suggestInsert"
	markDelete      = "suggestDelete"
	docSimilarity   = 0.5 // share of the shorter paragraph's tokens kept, below which a replacement shows as delete + insert
	docSummaryLimit = 80
)

type tipNode = map[string]any

// viewRun is one run of the page's text: a canonical run plus, inside a
// suggestion, whether it is inserted or deleted text.
type viewRun struct {
	docRun
	change string // "", "insert", "delete"
	id     int
}

// viewParagraph is one block of the page.
type viewParagraph struct {
	DocParagraph
	runs       []viewRun
	suggestion map[string]any // {id, kind, from?} or nil
}

func plainRuns(p DocParagraph) []viewRun {
	var runs []viewRun
	for _, run := range parseInline(p.Text) {
		runs = append(runs, viewRun{docRun: run})
	}
	return runs
}

// canonicalToDoc renders paragraphs as a Tiptap document without any
// suggestion marks: the draft as the owner would edit it.
func canonicalToDoc(paragraphs []DocParagraph) tipNode {
	blocks := make([]viewParagraph, 0, len(paragraphs))
	for _, p := range paragraphs {
		blocks = append(blocks, viewParagraph{DocParagraph: p, runs: plainRuns(p)})
	}
	return buildDoc(blocks)
}

// buildDoc nests list paragraphs by depth and emits everything else flat.
func buildDoc(blocks []viewParagraph) tipNode {
	content := []any{}
	type openList struct {
		kind  string
		depth int
		list  tipNode
		item  tipNode
	}
	var stack []*openList
	frozen := 0
	appendTo := func(parent tipNode, node tipNode) {
		items, _ := parent["content"].([]any)
		parent["content"] = append(items, node)
	}
	for _, block := range blocks {
		if block.Frozen != "" {
			stack = nil
			content = append(content, frozenNode(block.DocParagraph, frozen))
			frozen++
			continue
		}
		if !docListStyle(block.Style) {
			stack = nil
			content = append(content, blockNode(block))
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].depth > block.Depth {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 && stack[len(stack)-1].depth == block.Depth && stack[len(stack)-1].kind != block.Style {
			stack = stack[:len(stack)-1]
		}
		// Open lists down to the paragraph's depth; a level the document
		// skips gets a synthetic item the round trip drops again.
		for len(stack) == 0 || stack[len(stack)-1].depth < block.Depth {
			depth := 0
			if len(stack) > 0 {
				depth = stack[len(stack)-1].depth + 1
			}
			kind := block.Style
			list := tipNode{"type": listType(kind), "content": []any{}}
			if len(stack) == 0 {
				content = append(content, list)
			} else {
				top := stack[len(stack)-1]
				if top.item == nil {
					top.item = tipNode{"type": tipListItem, "content": []any{tipNode{"type": tipParagraph, "attrs": map[string]any{"synthetic": true}}}}
					appendTo(top.list, top.item)
				}
				appendTo(top.item, list)
			}
			stack = append(stack, &openList{kind: kind, depth: depth, list: list})
		}
		top := stack[len(stack)-1]
		top.item = tipNode{"type": tipListItem, "content": []any{blockNode(block)}}
		appendTo(top.list, top.item)
	}
	return tipNode{"type": tipDoc, "content": content}
}

func listType(kind string) string {
	if kind == "numbered" {
		return tipOrderedList
	}
	return tipBulletList
}

// blockNode is a heading or paragraph with its runs.
func blockNode(block viewParagraph) tipNode {
	attrs := map[string]any{}
	node := tipNode{"type": tipParagraph}
	switch block.Style {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		node["type"] = tipHeading
		attrs["level"], _ = strconv.Atoi(block.Style[1:])
	case "title", "subtitle":
		attrs["docStyle"] = block.Style
	}
	if block.suggestion != nil {
		attrs["suggestion"] = block.suggestion
	}
	if len(attrs) > 0 {
		node["attrs"] = attrs
	}
	content := []any{}
	for _, run := range block.runs {
		parts := strings.Split(run.text, "\v")
		for i, part := range parts {
			if i > 0 {
				content = append(content, tipNode{"type": tipHardBreak})
			}
			if part == "" {
				continue
			}
			text := tipNode{"type": tipText, "text": part}
			var marks []any
			if run.bold {
				marks = append(marks, tipNode{"type": markBold})
			}
			if run.italic {
				marks = append(marks, tipNode{"type": markItalic})
			}
			if run.link != "" {
				marks = append(marks, tipNode{"type": markLink, "attrs": map[string]any{"href": run.link}})
			}
			switch run.change {
			case "insert":
				marks = append(marks, tipNode{"type": markInsert, "attrs": map[string]any{"id": run.id}})
			case "delete":
				marks = append(marks, tipNode{"type": markDelete, "attrs": map[string]any{"id": run.id}})
			}
			if len(marks) > 0 {
				text["marks"] = marks
			}
			content = append(content, text)
		}
	}
	if len(content) > 0 {
		node["content"] = content
	}
	return node
}

// frozenNode is an atom the editor shows but cannot change; key is its
// position among the document's frozen paragraphs, which the round trip
// uses to put the base paragraph back.
func frozenNode(p DocParagraph, key int) tipNode {
	if p.Frozen == "table" && len(p.Cells) > 0 {
		rows := make([]any, 0, len(p.Cells))
		for _, row := range p.Cells {
			cells := make([]any, 0, len(row))
			for _, cell := range row {
				cells = append(cells, cell)
			}
			rows = append(rows, cells)
		}
		return tipNode{"type": tipFrozenTable, "attrs": map[string]any{"key": key, "label": p.Text, "rows": rows}}
	}
	return tipNode{"type": tipFrozenBlock, "attrs": map[string]any{"key": key, "kind": p.Frozen, "label": p.Text}}
}

// docToCanonical is the page's inverse: the owner's edited document back
// to canonical paragraphs, deletions stripped, suggestion marks dropped,
// frozen atoms replaced by the base paragraphs they stand for. Anything
// outside the supported node set is refused by name so nothing is lost
// silently.
func docToCanonical(doc any, frozen []DocParagraph) ([]DocParagraph, error) {
	root, ok := doc.(map[string]any)
	if !ok || root["type"] != tipDoc {
		return nil, valueErr("document must be a Tiptap doc")
	}
	w := &docWalker{frozen: frozen}
	if err := w.blocks(root["content"], "", 0); err != nil {
		return nil, err
	}
	if w.used != len(frozen) {
		return nil, valueErr("frozen paragraphs must stay as the document has them")
	}
	out := w.out
	if out == nil {
		out = []DocParagraph{}
	}
	for i := range out {
		out[i].N = i + 1
	}
	return out, nil
}

type docWalker struct {
	frozen []DocParagraph
	used   int
	out    []DocParagraph
}

func (w *docWalker) blocks(raw any, list string, depth int) error {
	items, _ := raw.([]any)
	for _, item := range items {
		node, _ := item.(map[string]any)
		if len(w.out) > docParagraphsMax {
			return valueErr("document is too long to review as suggestions")
		}
		kind, _ := node["type"].(string)
		attrs, _ := node["attrs"].(map[string]any)
		switch kind {
		case tipParagraph, tipHeading:
			if suggestion, _ := attrs["suggestion"].(map[string]any); suggestion != nil && suggestion["kind"] == "delete" {
				continue // a deleted paragraph is not in the draft
			}
			text := w.text(node["content"])
			if synthetic, _ := attrs["synthetic"].(bool); synthetic && text == "" {
				continue
			}
			p := DocParagraph{Style: "text", Text: text}
			if kind == tipHeading {
				level, _ := asInt(attrs["level"])
				if level < 1 || level > 6 {
					return valueErr("heading level must be 1–6")
				}
				p.Style = "h" + strconv.FormatInt(level, 10)
			} else if style, _ := attrs["docStyle"].(string); style == "title" || style == "subtitle" {
				p.Style = style
			}
			if list != "" {
				p.Style, p.Depth = list, depth
			}
			if err := validateParagraph(&p); err != nil {
				return err
			}
			w.out = append(w.out, p)
		case tipBulletList, tipOrderedList:
			next := "bullet"
			if kind == tipOrderedList {
				next = "numbered"
			}
			nextDepth := 0
			if list != "" {
				nextDepth = depth + 1
			}
			if nextDepth > docListDepthMax {
				return valueErr("lists nest at most 9 levels deep")
			}
			for _, rawItem := range listOf(node["content"]) {
				listItem, _ := rawItem.(map[string]any)
				if listItem["type"] != tipListItem {
					return valueErr("lists may only contain list items")
				}
				if err := w.blocks(listItem["content"], next, nextDepth); err != nil {
					return err
				}
			}
		case tipFrozenBlock, tipFrozenTable:
			key, _ := asInt(attrs["key"])
			if w.used >= len(w.frozen) || int(key) != w.used {
				return valueErr("frozen paragraphs must stay as the document has them")
			}
			w.out = append(w.out, w.frozen[w.used])
			w.used++
		default:
			return valueErr(strconv.Quote(kind) + " content cannot be written to a Google Doc as a suggestion")
		}
	}
	return nil
}

func listOf(raw any) []any {
	items, _ := raw.([]any)
	return items
}

// text renders a block's inline content as canonical text: deleted runs
// dropped, hard breaks as soft line breaks, only the marks the model has.
func (w *docWalker) text(raw any) string {
	var runs []docRun
	for _, item := range listOf(raw) {
		node, _ := item.(map[string]any)
		switch node["type"] {
		case tipHardBreak:
			runs = append(runs, docRun{text: "\v"})
		case tipText:
			run := docRun{}
			deleted := false
			for _, rawMark := range listOf(node["marks"]) {
				mark, _ := rawMark.(map[string]any)
				attrs, _ := mark["attrs"].(map[string]any)
				switch mark["type"] {
				case markBold:
					run.bold = true
				case markItalic:
					run.italic = true
				case markLink:
					run.link = stringField(attrs, "href")
				case markDelete:
					deleted = true
				}
			}
			if deleted {
				continue
			}
			run.text, _ = node["text"].(string)
			runs = append(runs, run)
		}
	}
	return renderInline(runs)
}

// Word-level diff of two runs of text, marks included.

type docToken struct {
	docRun
	key string
}

// A token is a word or a run of punctuation with the whitespace before it,
// so spaces never compete with words for a match and "overview." still
// matches "overview".
var wordPattern = regexp.MustCompile(`\s*[\p{L}\p{N}_]+|\s*[^\p{L}\p{N}_\s]+|\s+`)

func tokenize(runs []docRun) []docToken {
	var out []docToken
	for _, run := range runs {
		for _, part := range wordPattern.FindAllString(run.text, -1) {
			token := docToken{docRun: docRun{text: part, bold: run.bold, italic: run.italic, link: run.link}}
			token.key = part + "\x00" + strconv.FormatBool(run.bold) + strconv.FormatBool(run.italic) + run.link
			out = append(out, token)
		}
	}
	return out
}

// tokenChange is one token of a diff with what happened to it.
type tokenChange struct {
	token  docToken
	change string // "", "delete", "insert"
}

// inlineDiff merges two paragraphs' runs into one sequence with each token
// marked kept, deleted or inserted; ratio is the token overlap relative to
// the shorter side, so a short paragraph rewritten into a long one still
// reads as an edit of it.
func inlineDiff(before, after []docRun, id int) (runs []viewRun, ratio float64) {
	a, b := tokenize(before), tokenize(after)
	keysA := make([]string, len(a))
	for i, t := range a {
		keysA[i] = t.key
	}
	keysB := make([]string, len(b))
	for i, t := range b {
		keysB[i] = t.key
	}
	var changes []tokenChange
	same := 0
	for _, op := range lcsOps(keysA, keysB) {
		for k := 0; k < op.length; k++ {
			switch op.kind {
			case '=':
				changes = append(changes, tokenChange{token: a[op.a+k]})
				same++
			case '-':
				changes = append(changes, tokenChange{token: a[op.a+k], change: "delete"})
			case '+':
				changes = append(changes, tokenChange{token: b[op.b+k], change: "insert"})
			}
		}
	}
	if shorter := min(len(a), len(b)); shorter > 0 {
		ratio = float64(same) / float64(shorter)
	}
	push := func(token docToken, change string) {
		run := viewRun{docRun: token.docRun, change: change, id: id}
		if n := len(runs); n > 0 && runs[n-1].change == change && runs[n-1].bold == run.bold && runs[n-1].italic == run.italic && runs[n-1].link == run.link {
			runs[n-1].text += run.text
			return
		}
		runs = append(runs, run)
	}
	for _, c := range cleanupSemantic(changes) {
		push(c.token, c.change)
	}
	return runs, ratio
}

// cleanupSemantic reads like an editor, not a diff engine: a few kept
// characters wedged between two edits ("the", a space) are absorbed into
// one replacement, and each replacement shows its deletions before its
// insertions.
func cleanupSemantic(changes []tokenChange) []tokenChange {
	type segment struct {
		change string
		items  []tokenChange
	}
	var segments []segment
	for _, c := range changes {
		if n := len(segments); n > 0 && segments[n-1].change == c.change {
			segments[n-1].items = append(segments[n-1].items, c)
			continue
		}
		segments = append(segments, segment{change: c.change, items: []tokenChange{c}})
	}
	length := func(s segment) int {
		n := 0
		for _, c := range s.items {
			n += utf8.RuneCountInString(strings.TrimSpace(c.token.text))
		}
		return n
	}
	// Absorb short equalities between edits, repeating until stable.
	for changed := true; changed; {
		changed = false
		for i := 1; i+1 < len(segments); i++ {
			s := segments[i]
			if s.change != "" || segments[i-1].change == "" || segments[i+1].change == "" {
				continue
			}
			if length(s) > 4 || length(s) >= length(segments[i-1])+length(segments[i+1]) {
				continue
			}
			var merged []tokenChange
			for _, side := range []string{"delete", "insert"} {
				for _, part := range []segment{segments[i-1], s, segments[i+1]} {
					for _, c := range part.items {
						if part.change == side || part.change == "" {
							merged = append(merged, tokenChange{token: c.token, change: side})
						}
					}
				}
			}
			segments = append(segments[:i-1], append([]segment{{change: "mixed", items: merged}}, segments[i+2:]...)...)
			changed = true
			break
		}
	}
	// Adjacent edits read as one replacement: deletions, then insertions.
	var out []tokenChange
	for i := 0; i < len(segments); {
		if segments[i].change == "" {
			out = append(out, segments[i].items...)
			i++
			continue
		}
		j := i
		var deleted, inserted []tokenChange
		for ; j < len(segments) && segments[j].change != ""; j++ {
			for _, c := range segments[j].items {
				if c.change == "delete" {
					deleted = append(deleted, c)
				} else {
					inserted = append(inserted, c)
				}
			}
		}
		out = append(append(out, deleted...), inserted...)
		i = j
	}
	return out
}

// suggestionView is what the page needs: the document and one card per
// change, plus the proposal's rejected suggestions the margin can restore.
type suggestionCard struct {
	ID         int      `json:"id"`
	Kind       string   `json:"kind"` // replace, insert, delete, restyle
	Summary    string   `json:"summary"`
	Reasons    []string `json:"reasons"`
	Status     string   `json:"status"` // pending, accepted, rejected
	Acceptable bool     `json:"acceptable,omitempty"`
	Hunk       int      `json:"hunk,omitempty"` // the proposal's hunk, for rejected cards
}

// suggestionDocument renders base → draft as a page with suggestions;
// changes whose signature the owner accepted read as plain draft text.
func suggestionDocument(base, draft []DocParagraph, current []DocHunk, accepted []string) (tipNode, []suggestionCard) {
	var blocks []viewParagraph
	cards := []suggestionCard{}
	acknowledged := map[string]bool{}
	for _, s := range accepted {
		acknowledged[s] = true
	}
	pos := 0
	for _, h := range current {
		for ; pos < h.From; pos++ {
			blocks = append(blocks, viewParagraph{DocParagraph: base[pos], runs: plainRuns(base[pos])})
		}
		card := suggestionCard{ID: h.ID, Reasons: h.Reasons, Status: "pending"}
		if card.Reasons == nil {
			card.Reasons = []string{}
		}
		removed, added := h.Removed, h.Added
		if acknowledged[hunkSignature(h)] {
			var deleted, inserted []string
			for _, p := range removed {
				deleted = append(deleted, plainInline(p.Text))
			}
			for _, p := range added {
				inserted = append(inserted, plainInline(p.Text))
				blocks = append(blocks, viewParagraph{DocParagraph: p, runs: plainRuns(p), suggestion: map[string]any{"id": h.ID, "kind": "accepted"}})
			}
			card.Kind, card.Summary = summarize(deleted, inserted, "")
			card.Status = "accepted"
			cards = append(cards, card)
			pos = h.To
			continue
		}
		var deleted, inserted []string
		restyled := ""
		// Paragraphs edited in place show their words changing; the rest
		// show as whole deleted or inserted paragraphs.
		paired := 0
		for paired < len(removed) && paired < len(added) {
			before, after := removed[paired], added[paired]
			runs, ratio := inlineDiff(parseInline(before.Text), parseInline(after.Text), h.ID)
			if ratio < docSimilarity && before.Text != "" && after.Text != "" {
				break
			}
			block := viewParagraph{DocParagraph: after, runs: runs}
			block.suggestion = map[string]any{"id": h.ID, "kind": "replace"}
			if before.Style != after.Style || before.Depth != after.Depth {
				block.suggestion["kind"] = "restyle"
				block.suggestion["from"] = styleName(before)
				restyled = styleName(before) + " → " + styleName(after)
			}
			for _, run := range runs {
				switch run.change {
				case "delete":
					deleted = append(deleted, run.text)
				case "insert":
					inserted = append(inserted, run.text)
				}
			}
			blocks = append(blocks, block)
			paired++
		}
		for _, p := range removed[paired:] {
			runs := plainRuns(p)
			for i := range runs {
				runs[i].change, runs[i].id = "delete", h.ID
			}
			blocks = append(blocks, viewParagraph{DocParagraph: p, runs: runs, suggestion: map[string]any{"id": h.ID, "kind": "delete"}})
			deleted = append(deleted, plainInline(p.Text))
		}
		for _, p := range added[paired:] {
			runs := plainRuns(p)
			for i := range runs {
				runs[i].change, runs[i].id = "insert", h.ID
			}
			blocks = append(blocks, viewParagraph{DocParagraph: p, runs: runs, suggestion: map[string]any{"id": h.ID, "kind": "insert"}})
			inserted = append(inserted, plainInline(p.Text))
		}
		card.Kind, card.Summary = summarize(deleted, inserted, restyled)
		cards = append(cards, card)
		pos = h.To
	}
	for ; pos < len(base); pos++ {
		blocks = append(blocks, viewParagraph{DocParagraph: base[pos], runs: plainRuns(base[pos])})
	}
	return buildDoc(blocks), cards
}

func styleName(p DocParagraph) string {
	if docListStyle(p.Style) {
		return p.Style + " level " + strconv.Itoa(p.Depth+1)
	}
	return p.Style
}

// summarize words a card the way Google Docs does: Replace / Add / Delete.
func summarize(deleted, inserted []string, restyled string) (kind, summary string) {
	del := clip(strings.TrimSpace(strings.Join(deleted, " ")))
	ins := clip(strings.TrimSpace(strings.Join(inserted, " ")))
	switch {
	case del != "" && ins != "":
		kind, summary = "replace", "Replace “"+del+"” with “"+ins+"”"
	case ins != "":
		kind, summary = "insert", "Add “"+ins+"”"
	case del != "":
		kind, summary = "delete", "Delete “"+del+"”"
	default:
		kind, summary = "restyle", "Format"
	}
	if restyled != "" {
		if summary == "Format" {
			summary = "Format: " + restyled
		} else {
			summary += " · " + restyled
		}
		if kind == "restyle" || (del == "" && ins == "") {
			kind = "restyle"
		}
	}
	return kind, summary
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= docSummaryLimit {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:docSummaryLimit])) + "…"
}
