package policy

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The canonical document model behind Google Docs suggestions: a document
// is a numbered list of paragraphs, each a named style (or a list item with
// a nesting depth) and Markdown-like inline text. Anything the model cannot
// express — tables, images, footnotes, breaks, smart chips — is a frozen
// paragraph the agent sees but can neither change nor delete, and no
// compiled write ever touches. Only the first tab is projected.

const (
	docParagraphLimit = 32 * 1024  // one paragraph's canonical text
	docTextLimit      = 256 * 1024 // all proposed text in one proposal
	docOpsLimit       = 200
	docParagraphsMax  = 5000
	docListDepthMax   = 8
)

// DocParagraph is one paragraph of the canonical model. N is 1-based.
// Start/End are the paragraph's UTF-16 range in the projected revision
// (End includes the trailing newline); they exist only on projections.
type DocParagraph struct {
	N      int    `json:"n"`
	Style  string `json:"style"`
	Depth  int    `json:"depth,omitempty"`
	Text   string `json:"text"`
	Frozen string `json:"frozen,omitempty"`
	Start  int    `json:"-"`
	End    int    `json:"-"`
	// named is the underlying namedStyleType of a projected list item and
	// endsWithLink whether its last run is a link: what inserted text would
	// inherit from it.
	named        string
	endsWithLink bool
	listID       string
}

// key is the paragraph's identity for diffing: everything the agent can
// change, plus the frozen kind so a frozen block never equals text.
func (p DocParagraph) key() string {
	return p.Frozen + "\x00" + p.Style + "\x00" + strconv.Itoa(p.Depth) + "\x00" + p.Text
}

// DocProjection is one revision of a Google document in the canonical model.
type DocProjection struct {
	DocumentID string         `json:"document_id"`
	Title      string         `json:"title"`
	RevisionID string         `json:"revision_id"`
	Paragraphs []DocParagraph `json:"paragraphs"`
	Notes      []string       `json:"notes,omitempty"`
}

var docNamedStyles = map[string]string{"NORMAL_TEXT": "text", "TITLE": "title", "SUBTITLE": "subtitle", "HEADING_1": "h1", "HEADING_2": "h2", "HEADING_3": "h3", "HEADING_4": "h4", "HEADING_5": "h5", "HEADING_6": "h6"}
var docStyleNames = map[string]string{"text": "NORMAL_TEXT", "title": "TITLE", "subtitle": "SUBTITLE", "h1": "HEADING_1", "h2": "HEADING_2", "h3": "HEADING_3", "h4": "HEADING_4", "h5": "HEADING_5", "h6": "HEADING_6"}

func docListStyle(style string) bool { return style == "bullet" || style == "numbered" }

// utf16Len is the Docs API length of s: indexes count UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// ProjectDocument converts a documents.get response into the canonical
// model. Frozen paragraphs keep their ranges so the compiler can route
// around them.
func ProjectDocument(doc map[string]any) (*DocProjection, error) {
	id, _ := doc["documentId"].(string)
	if !googleDocumentID.MatchString(id) {
		return nil, errors.New("invalid document response")
	}
	revision, _ := doc["revisionId"].(string)
	if revision == "" || len(revision) > 256 {
		return nil, errors.New("document response lacks a revision")
	}
	title, _ := doc["title"].(string)
	body, _ := doc["body"].(map[string]any)
	content, _ := body["content"].([]any)
	if len(content) == 0 {
		return nil, errors.New("document response lacks a body")
	}
	lists, _ := doc["lists"].(map[string]any)
	out := &DocProjection{DocumentID: id, Title: title, RevisionID: revision, Paragraphs: []DocParagraph{}}
	if tabs, _ := doc["tabs"].([]any); len(tabs) > 1 {
		out.Notes = append(out.Notes, "The document has "+strconv.Itoa(len(tabs))+" tabs; only the first tab is shown and editable.")
	}
	frozen := 0
	for _, raw := range content {
		element, _ := raw.(map[string]any)
		start, _ := asInt(element["startIndex"])
		end, _ := asInt(element["endIndex"])
		p := DocParagraph{Start: int(start), End: int(end), Style: "text"}
		switch {
		case element["paragraph"] != nil:
			paragraph, _ := element["paragraph"].(map[string]any)
			projectParagraph(&p, paragraph, lists)
		case element["table"] != nil:
			table, _ := element["table"].(map[string]any)
			rows, _ := asInt(table["rows"])
			columns, _ := asInt(table["columns"])
			p.Frozen, p.Text = "table", "[table "+strconv.FormatInt(rows, 10)+"×"+strconv.FormatInt(columns, 10)+"]"
		case element["tableOfContents"] != nil:
			p.Frozen, p.Text = "table of contents", "[table of contents]"
		case element["sectionBreak"] != nil:
			if start == 0 {
				continue // the document's own start marker, not content
			}
			p.Frozen, p.Text = "section break", "[section break]"
		default:
			p.Frozen, p.Text = "unsupported", "[unsupported content]"
		}
		if p.End <= p.Start {
			return nil, errors.New("document response has an invalid range")
		}
		if p.Frozen != "" {
			frozen++
		}
		p.N = len(out.Paragraphs) + 1
		out.Paragraphs = append(out.Paragraphs, p)
		if len(out.Paragraphs) > docParagraphsMax {
			return nil, errors.New("document is too long to review as suggestions")
		}
	}
	if frozen > 0 {
		out.Notes = append(out.Notes, strconv.Itoa(frozen)+" paragraph(s) are frozen (tables, images, footnotes, breaks or chips) and cannot be changed or deleted.")
	}
	return out, nil
}

type docRun struct {
	text         string
	bold, italic bool
	link         string
}

func projectParagraph(p *DocParagraph, paragraph map[string]any, lists map[string]any) {
	style, _ := paragraph["paragraphStyle"].(map[string]any)
	p.named = "NORMAL_TEXT"
	if named, ok := docNamedStyles[stringField(style, "namedStyleType")]; ok {
		p.Style = named
		p.named = stringField(style, "namedStyleType")
	}
	if bullet, ok := paragraph["bullet"].(map[string]any); ok {
		p.listID = stringField(bullet, "listId")
		depth, _ := asInt(bullet["nestingLevel"])
		p.Depth = int(depth)
		p.Style = "bullet"
		if docListNumbered(lists, p.listID, p.Depth) {
			p.Style = "numbered"
		}
	}
	if ids, _ := paragraph["positionedObjectIds"].([]any); len(ids) > 0 {
		p.Frozen = "anchored object"
	}
	var runs []docRun
	elements, _ := paragraph["elements"].([]any)
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		switch {
		case element["textRun"] != nil:
			run, _ := element["textRun"].(map[string]any)
			text, _ := run["content"].(string)
			ts, _ := run["textStyle"].(map[string]any)
			bold, _ := ts["bold"].(bool)
			italic, _ := ts["italic"].(bool)
			link, _ := ts["link"].(map[string]any)
			url := stringField(link, "url")
			runs = append(runs, docRun{text: text, bold: bold, italic: italic, link: url})
		case element["inlineObjectElement"] != nil:
			p.Frozen = "image"
			runs = append(runs, docRun{text: "[image]"})
		case element["footnoteReference"] != nil:
			p.Frozen = "footnote"
			runs = append(runs, docRun{text: "[footnote]"})
		case element["horizontalRule"] != nil:
			p.Frozen = "horizontal rule"
			runs = append(runs, docRun{text: "[horizontal rule]"})
		case element["pageBreak"] != nil, element["columnBreak"] != nil:
			p.Frozen = "break"
			runs = append(runs, docRun{text: "[break]"})
		case element["equation"] != nil:
			p.Frozen = "equation"
			runs = append(runs, docRun{text: "[equation]"})
		default:
			// Smart chips (person, richLink, dateSegment), autoText and
			// anything newer than this projection.
			p.Frozen = "chip"
			runs = append(runs, docRun{text: "[chip]"})
		}
	}
	if n := len(runs); n > 0 {
		runs[n-1].text = strings.TrimSuffix(runs[n-1].text, "\n")
		p.endsWithLink = runs[n-1].link != ""
	}
	if p.Frozen != "" {
		// Placeholders are for reading, not round-tripping.
		var b strings.Builder
		for _, run := range runs {
			b.WriteString(run.text)
		}
		p.Text = b.String()
		return
	}
	p.Text = renderInline(runs)
}

// docListNumbered reports whether a list level renders numbers rather than
// bullets: numbered levels carry a glyph type, bulleted ones a symbol.
func docListNumbered(lists map[string]any, listID string, depth int) bool {
	list, _ := lists[listID].(map[string]any)
	properties, _ := list["listProperties"].(map[string]any)
	levels, _ := properties["nestingLevels"].([]any)
	if depth < 0 || depth >= len(levels) {
		return false
	}
	level, _ := levels[depth].(map[string]any)
	glyph := stringField(level, "glyphType")
	return glyph != "" && glyph != "GLYPH_TYPE_UNSPECIFIED"
}

// Inline marks are a strict toggle grammar, not CommonMark: "**" toggles
// bold, "*" toggles italic, "[text](url)" is a link, and backslash escapes
// "\", "*", "[" and "]". A projection escapes those characters in literal
// text so unchanged paragraphs round-trip exactly.

func escapeInline(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '\\', '*', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderInline emits the fewest toggles that reproduce the runs.
func renderInline(runs []docRun) string {
	var b strings.Builder
	bold, italic, link := false, false, ""
	closeLink := func() {
		if link != "" {
			b.WriteString("](" + strings.ReplaceAll(link, ")", "%29") + ")")
			link = ""
		}
	}
	for _, run := range runs {
		if run.text == "" {
			continue
		}
		if run.link != link {
			closeLink()
		}
		if run.bold != bold {
			b.WriteString("**")
			bold = run.bold
		}
		if run.italic != italic {
			b.WriteString("*")
			italic = run.italic
		}
		if run.link != "" && link == "" {
			b.WriteString("[")
			link = run.link
		}
		b.WriteString(escapeInline(run.text))
	}
	closeLink()
	if bold {
		b.WriteString("**")
	}
	if italic {
		b.WriteString("*")
	}
	return b.String()
}

// parseInline is renderInline's inverse. An unmatched "[" is literal.
func parseInline(text string) []docRun {
	var runs []docRun
	var current strings.Builder
	bold, italic, link := false, false, ""
	flush := func() {
		if current.Len() > 0 {
			runs = append(runs, docRun{text: current.String(), bold: bold, italic: italic, link: link})
			current.Reset()
		}
	}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text):
			_, size := utf8.DecodeRuneInString(text[i+1:])
			current.WriteString(text[i+1 : i+1+size])
			i += 1 + size
		case c == '*' && i+1 < len(text) && text[i+1] == '*':
			flush()
			bold = !bold
			i += 2
		case c == '*':
			flush()
			italic = !italic
			i++
		case c == '[' && link == "":
			if url, ok := inlineLinkTarget(text, i+1); ok {
				flush()
				link = url
				i++
				continue
			}
			current.WriteByte(c)
			i++
		case c == ']' && link != "" && i+1 < len(text) && text[i+1] == '(':
			end := strings.IndexByte(text[i+2:], ')')
			flush()
			link = ""
			i += 3 + end
		default:
			current.WriteByte(c)
			i++
		}
	}
	flush()
	return runs
}

// inlineLinkTarget finds the "](url)" closing a link text that starts at
// from, skipping escaped characters.
func inlineLinkTarget(text string, from int) (url string, ok bool) {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case ']':
			if i+1 < len(text) && text[i+1] == '(' {
				close := strings.IndexByte(text[i+2:], ')')
				if close < 0 {
					return "", false
				}
				url = text[i+2 : i+2+close]
				return url, url != "" && !strings.ContainsAny(url, " \t\v\n\r")
			}
		}
	}
	return "", false
}

// plainInline is the paragraph's text without marks: what a reader sees.
func plainInline(text string) string {
	var b strings.Builder
	for _, run := range parseInline(text) {
		b.WriteString(run.text)
	}
	return b.String()
}

// validateParagraph checks a proposed paragraph: a known style, a list
// depth only on list items, bounded text without line breaks, control or
// bidirectional characters, and no leading tab on a list item (the
// compiler encodes nesting as leading tabs).
func validateParagraph(p *DocParagraph) error {
	if p.Frozen != "" {
		return valueErr("frozen paragraphs cannot be proposed")
	}
	if _, named := docStyleNames[p.Style]; !named && !docListStyle(p.Style) {
		return valueErr("unknown paragraph style " + strconv.Quote(p.Style))
	}
	if p.Depth < 0 || p.Depth > docListDepthMax || (p.Depth > 0 && !docListStyle(p.Style)) {
		return valueErr("list depth must be 0–8 and only on bullet or numbered paragraphs")
	}
	if len(p.Text) > docParagraphLimit || !utf8.ValidString(p.Text) {
		return valueErr("paragraph text is invalid or exceeds 32 KiB")
	}
	for _, r := range p.Text {
		if (r < 32 && r != '\t' && r != '\v') || r == 0x7f || strings.ContainsRune("‪‫‬‭‮⁦⁧⁨⁩", r) {
			return valueErr("paragraph text cannot contain line breaks, control or hidden characters")
		}
	}
	if docListStyle(p.Style) && strings.HasPrefix(p.Text, "\t") {
		return valueErr("list items cannot start with a tab; use depth for nesting")
	}
	// Canonical marks, so "[**x**](u)" and "**[x](u)**" are the same text.
	p.Text = renderInline(parseInline(p.Text))
	return nil
}

// DocOp is one agent edit against paragraph numbers of a read.
type DocOp struct {
	Type       string         `json:"type"`
	Start      int            `json:"start,omitempty"`
	End        int            `json:"end,omitempty"`
	After      int            `json:"after,omitempty"`
	Paragraphs []DocParagraph `json:"paragraphs,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	// baseFrom/baseTo is the op's half-open range in base positions, used
	// to bind reasons to the hunks Warden computes.
	baseFrom, baseTo int
}

// decodeDocOps validates the operations of a proposal against its base.
func decodeDocOps(raw any, base []DocParagraph) ([]DocOp, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 || len(items) > docOpsLimit {
		return nil, valueErr("Provide 1–200 operations")
	}
	n := len(base)
	var ops []DocOp
	total := 0
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, valueErr("each operation must be an object")
		}
		op := DocOp{Type: stringField(fields, "type")}
		for key := range fields {
			switch key {
			case "type", "start", "end", "after", "paragraphs", "reason":
			default:
				return nil, valueErr("unexpected operation field " + strconv.Quote(key))
			}
		}
		if r, present := fields["reason"]; present {
			reason, err := proposalText(r, 2000, true)
			if err != nil {
				return nil, err
			}
			op.Reason = strings.TrimSpace(reason)
		}
		number := func(key string) (int, error) {
			v, ok := asInt(fields[key])
			if _, isBool := fields[key].(bool); !ok || isBool || v < 0 || v > int64(n) {
				return 0, valueErr(key + " must be a paragraph number of the read")
			}
			return int(v), nil
		}
		var err error
		switch op.Type {
		case "replace", "delete":
			if op.Start, err = number("start"); err != nil {
				return nil, err
			}
			if op.End, err = number("end"); err != nil {
				return nil, err
			}
			if op.Start < 1 || op.End < op.Start {
				return nil, valueErr("start must be ≥ 1 and end ≥ start")
			}
			op.baseFrom, op.baseTo = op.Start-1, op.End
			for _, p := range base[op.baseFrom:op.baseTo] {
				if p.Frozen != "" {
					return nil, valueErr("paragraph " + strconv.Itoa(p.N) + " is frozen (" + p.Frozen + ") and cannot be changed or deleted")
				}
			}
		case "insert":
			if op.After, err = number("after"); err != nil {
				return nil, err
			}
			op.baseFrom, op.baseTo = op.After, op.After
		default:
			return nil, valueErr("operation type must be replace, insert or delete")
		}
		if op.Type != "delete" {
			list, ok := fields["paragraphs"].([]any)
			if !ok || len(list) == 0 || (op.Type == "insert" && len(list) > docParagraphsMax) {
				return nil, valueErr("replace and insert need a non-empty paragraphs list")
			}
			for _, raw := range list {
				pf, ok := raw.(map[string]any)
				if !ok {
					return nil, valueErr("each paragraph must be an object")
				}
				for key := range pf {
					switch key {
					case "style", "depth", "text":
					default:
						return nil, valueErr("unexpected paragraph field " + strconv.Quote(key))
					}
				}
				p := DocParagraph{Style: stringField(pf, "style")}
				if p.Style == "" {
					p.Style = "text"
				}
				if d, present := pf["depth"]; present {
					depth, ok := asInt(d)
					if _, isBool := d.(bool); !ok || isBool {
						return nil, valueErr("depth must be an integer")
					}
					p.Depth = int(depth)
				}
				p.Text, _ = pf["text"].(string)
				if err := validateParagraph(&p); err != nil {
					return nil, err
				}
				total += len(p.Text)
				op.Paragraphs = append(op.Paragraphs, p)
			}
		} else if _, present := fields["paragraphs"]; present {
			return nil, valueErr("delete takes no paragraphs")
		}
		ops = append(ops, op)
	}
	if total > docTextLimit {
		return nil, valueErr("Proposed text exceeds 256 KiB; split the proposal")
	}
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].baseFrom != ops[j].baseFrom {
			return ops[i].baseFrom < ops[j].baseFrom
		}
		return ops[i].baseTo < ops[j].baseTo
	})
	for i := 1; i < len(ops); i++ {
		prev, cur := ops[i-1], ops[i]
		if cur.baseFrom < prev.baseTo {
			return nil, valueErr("operations overlap; merge them into one replace")
		}
	}
	return ops, nil
}

// applyDocumentOps materialises the proposed document. Frozen paragraphs
// pass through untouched; the result is renumbered.
func applyDocumentOps(base []DocParagraph, ops []DocOp) []DocParagraph {
	out := make([]DocParagraph, 0, len(base))
	pos := 0
	emit := func(p DocParagraph) {
		p.N = len(out) + 1
		p.Start, p.End, p.named, p.endsWithLink, p.listID = 0, 0, "", false, ""
		out = append(out, p)
	}
	for _, op := range ops {
		for pos < op.baseFrom {
			emit(base[pos])
			pos++
		}
		for _, p := range op.Paragraphs {
			emit(p)
		}
		pos = op.baseTo
	}
	for pos < len(base) {
		emit(base[pos])
		pos++
	}
	return out
}

// decodeDraft validates an owner's draft: the proposal's paragraphs with
// frozen ones kept exactly where the base has them.
func decodeDraft(raw any, base []DocParagraph) ([]DocParagraph, error) {
	items, ok := raw.([]any)
	if !ok || len(items) > docParagraphsMax {
		return nil, valueErr("draft must be a list of paragraphs")
	}
	var frozen []DocParagraph
	for _, p := range base {
		if p.Frozen != "" {
			frozen = append(frozen, p)
		}
	}
	var out []DocParagraph
	total := 0
	seen := 0
	for _, item := range items {
		pf, ok := item.(map[string]any)
		if !ok {
			return nil, valueErr("each draft paragraph must be an object")
		}
		p := DocParagraph{Style: stringField(pf, "style"), Frozen: stringField(pf, "frozen")}
		depth, _ := asInt(pf["depth"])
		p.Depth = int(depth)
		p.Text, _ = pf["text"].(string)
		if p.Frozen != "" {
			if seen >= len(frozen) || frozen[seen].key() != p.key() {
				return nil, valueErr("frozen paragraphs must stay as the document has them")
			}
			seen++
		} else if err := validateParagraph(&p); err != nil {
			return nil, err
		}
		total += len(p.Text)
		p.N = len(out) + 1
		out = append(out, p)
	}
	if seen != len(frozen) {
		return nil, valueErr("frozen paragraphs must stay as the document has them")
	}
	if total > docTextLimit*2 {
		return nil, valueErr("draft exceeds the review limit")
	}
	if out == nil {
		out = []DocParagraph{}
	}
	return out, nil
}
