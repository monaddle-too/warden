package policy

import "strings"

// The compiler turns diff(current, draft) into one Docs batchUpdate. Every
// request carries an index into the revision that was read, so requests are
// emitted from the end of the document towards the start: nothing a
// request changes lies below an index a later request uses. Within one
// paragraph, createParagraphBullets comes last because it removes the
// leading tabs that encode nesting.

const docRequestsMax = 5000

var docBulletPresets = map[string]string{"bullet": "BULLET_DISC_CIRCLE_SQUARE", "numbered": "NUMBERED_DECIMAL_ALPHA_ROMAN"}

// Google's own link colour, so links Warden writes look like links Docs makes.
var docLinkColor = map[string]any{"color": map[string]any{"rgbColor": map[string]any{"red": 0.0667, "green": 0.3333, "blue": 0.8}}}

type docWriter struct {
	requests []map[string]any
}

func (w *docWriter) add(kind string, request map[string]any) {
	w.requests = append(w.requests, map[string]any{kind: request})
}

func docRange(start, end int) map[string]any {
	return map[string]any{"startIndex": start, "endIndex": end}
}

// CompileDocumentUpdate builds the batchUpdate body that turns the projected
// revision into draft, or an error naming why it cannot be expressed.
func CompileDocumentUpdate(current *DocProjection, draft []DocParagraph) (map[string]any, []DocHunk, error) {
	hunks := documentHunks(current.Paragraphs, draft)
	if len(hunks) == 0 {
		return nil, nil, valueErr("The draft matches the document; nothing to write")
	}
	w := &docWriter{}
	base := current.Paragraphs
	n := len(base)
	for i := len(hunks) - 1; i >= 0; i-- {
		h := hunks[i]
		for _, p := range h.Removed {
			if p.Frozen != "" {
				return nil, nil, valueErr("Frozen paragraph " + p.Text + " cannot be moved, changed or deleted")
			}
		}
		for _, p := range h.Added {
			if p.Frozen != "" {
				return nil, nil, valueErr("Frozen paragraph " + p.Text + " cannot be moved, changed or deleted")
			}
		}
		k := len(h.Removed)
		if len(h.Added) < k {
			k = len(h.Added)
		}
		// force marks a paragraph whose properties can no longer be trusted
		// because it absorbed the document's final newline (see below).
		force := -1
		switch {
		case len(h.Removed) > k:
			// Extra base paragraphs go entirely, newline included. The
			// document's final newline cannot go, so a deletion reaching the
			// end removes the preceding paragraph's newline instead: that
			// paragraph then ends with the final newline and is asserted in
			// full, since a merge may leave it with the last paragraph's
			// properties.
			first, last := base[h.From+k], base[h.To-1]
			switch {
			case h.To < n:
				w.add("deleteContentRange", map[string]any{"range": docRange(first.Start, last.End)})
			case h.From+k > 0 && base[h.From+k-1].Frozen == "":
				prev := base[h.From+k-1]
				w.add("deleteContentRange", map[string]any{"range": docRange(prev.End-1, last.End-1)})
				if k > 0 {
					force = k - 1
				} else {
					w.replaceParagraph(prev, prev, true)
				}
			default:
				if last.End-1 > first.Start {
					w.add("deleteContentRange", map[string]any{"range": docRange(first.Start, last.End-1)})
				}
				w.add("updateParagraphStyle", map[string]any{"range": docRange(first.Start, first.Start+1), "paragraphStyle": map[string]any{"namedStyleType": "NORMAL_TEXT"}, "fields": "namedStyleType"})
				w.clearBullets(first.Start)
			}
		case len(h.Added) > k:
			if err := w.insertParagraphs(base, h, k); err != nil {
				return nil, nil, err
			}
		}
		for p := k - 1; p >= 0; p-- {
			w.replaceParagraph(base[h.From+p], h.Added[p], p == force)
		}
		if len(w.requests) > docRequestsMax {
			return nil, nil, valueErr("The change is too large for one write; split the proposal")
		}
	}
	return map[string]any{"requests": w.requests, "writeControl": map[string]any{"requiredRevisionId": current.RevisionID}}, hunks, nil
}

// insertParagraphs places h.Added[k:] after the last paired paragraph, or
// before the paragraph that follows the hunk. Text goes in front of the
// preceding paragraph's newline (or at the start of the following one), so
// the new paragraphs copy that neighbour's style; each is then asserted.
func (w *docWriter) insertParagraphs(base []DocParagraph, h DocHunk, k int) error {
	added := h.Added[k:]
	prev, next := h.From+k-1, h.From+k
	var at int
	var text strings.Builder
	var sourceList bool
	switch {
	case prev >= 0 && base[prev].Frozen == "":
		at = base[prev].End - 1
		sourceList = docListStyle(base[prev].Style)
		text.WriteString("\n")
	case next < len(base) && base[next].Frozen == "":
		at = base[next].Start
		sourceList = docListStyle(base[next].Style)
	default:
		return valueErr("Paragraphs cannot be inserted between frozen content; edit a text paragraph next to it instead")
	}
	starts := make([]int, len(added))
	bodies := make([]string, len(added))
	cursor := at
	if prev >= 0 && base[prev].Frozen == "" {
		cursor++ // past the newline that closes the preceding paragraph
	}
	for i, p := range added {
		bodies[i] = strings.Repeat("\t", listDepth(p)) + plainInline(p.Text)
		starts[i] = cursor
		cursor += utf16Len(bodies[i]) + 1
		if i > 0 {
			text.WriteString("\n")
		}
		text.WriteString(bodies[i])
	}
	if !(prev >= 0 && base[prev].Frozen == "") {
		text.WriteString("\n")
	}
	w.add("insertText", map[string]any{"location": map[string]any{"index": at}, "text": text.String()})
	for i := len(added) - 1; i >= 0; i-- {
		w.assertParagraph(starts[i], added[i], sourceList)
	}
	// One createParagraphBullets per run of list items keeps numbering
	// continuous; runs are emitted last, highest first, since each removes
	// the leading tabs of its paragraphs.
	for end := len(added); end > 0; {
		if !docListStyle(added[end-1].Style) {
			end--
			continue
		}
		start := end
		for start > 0 && added[start-1].Style == added[end-1].Style {
			start--
		}
		w.add("createParagraphBullets", map[string]any{"range": docRange(starts[start], starts[end-1]+1), "bulletPreset": docBulletPresets[added[end-1].Style]})
		end = start
	}
	return nil
}

func listDepth(p DocParagraph) int {
	if docListStyle(p.Style) {
		return p.Depth
	}
	return 0
}

// replaceParagraph edits one paragraph in place so alignment, spacing and
// indentation survive: its text is swapped, then its named style, marks and
// bullets asserted where they differ, or everywhere when force says the
// paragraph's properties are unknown.
func (w *docWriter) replaceParagraph(from DocParagraph, to DocParagraph, force bool) {
	listChanged := force || docListStyle(from.Style) != docListStyle(to.Style) || from.Style != to.Style || from.Depth != to.Depth
	tabs := 0
	if listChanged && docListStyle(to.Style) {
		tabs = to.Depth
	}
	if from.Text != to.Text {
		if from.End-1 > from.Start {
			w.add("deleteContentRange", map[string]any{"range": docRange(from.Start, from.End-1)})
		}
		body := strings.Repeat("\t", tabs) + plainInline(to.Text)
		if body != "" {
			w.add("insertText", map[string]any{"location": map[string]any{"index": from.Start}, "text": body})
		}
		w.assertText(from.Start+tabs, to.Text, from.endsWithLink)
	} else if tabs > 0 {
		w.add("insertText", map[string]any{"location": map[string]any{"index": from.Start}, "text": strings.Repeat("\t", tabs)})
	}
	if named := docNamedStyle(to); force || named != from.named {
		w.add("updateParagraphStyle", map[string]any{"range": docRange(from.Start, from.Start+1), "paragraphStyle": map[string]any{"namedStyleType": named}, "fields": "namedStyleType"})
	}
	if listChanged {
		if force || docListStyle(from.Style) {
			w.clearBullets(from.Start)
		}
		if docListStyle(to.Style) {
			w.add("createParagraphBullets", map[string]any{"range": docRange(from.Start, from.Start+1), "bulletPreset": docBulletPresets[to.Style]})
		}
	}
}

// assertParagraph sets everything an inserted paragraph copied from its
// neighbour: named style, marks and (except its bullets, which the caller
// creates per run) list membership.
func (w *docWriter) assertParagraph(start int, p DocParagraph, sourceList bool) {
	w.add("updateParagraphStyle", map[string]any{"range": docRange(start, start+1), "paragraphStyle": map[string]any{"namedStyleType": docNamedStyle(p)}, "fields": "namedStyleType"})
	w.assertText(start+listDepth(p), p.Text, true)
	if sourceList {
		w.clearBullets(start)
	}
}

// clearBullets removes a paragraph's bullet and the indent Docs keeps to
// preserve its look, so a former list item reads as plain text.
func (w *docWriter) clearBullets(start int) {
	w.add("deleteParagraphBullets", map[string]any{"range": docRange(start, start+1)})
	w.add("updateParagraphStyle", map[string]any{"range": docRange(start, start+1), "paragraphStyle": map[string]any{}, "fields": "indentFirstLine,indentStart"})
}

func docNamedStyle(p DocParagraph) string {
	if named, ok := docStyleNames[p.Style]; ok {
		return named
	}
	return "NORMAL_TEXT"
}

// assertText resets the marks inserted text inherited and applies the
// paragraph's own, run by run. resetColor also drops an inherited link
// colour; replaced text keeps its paragraph's colour otherwise.
func (w *docWriter) assertText(start int, text string, resetColor bool) {
	runs := parseInline(text)
	length := 0
	for _, run := range runs {
		length += utf16Len(run.text)
	}
	if length == 0 {
		return
	}
	reset := map[string]any{"bold": false, "italic": false, "underline": false, "strikethrough": false}
	fields := "bold,italic,underline,strikethrough,link"
	if resetColor {
		fields += ",foregroundColor"
	}
	w.add("updateTextStyle", map[string]any{"range": docRange(start, start+length), "textStyle": reset, "fields": fields})
	cursor := start
	for _, run := range runs {
		end := cursor + utf16Len(run.text)
		style := map[string]any{}
		var names []string
		if run.bold {
			style["bold"] = true
			names = append(names, "bold")
		}
		if run.italic {
			style["italic"] = true
			names = append(names, "italic")
		}
		if run.link != "" {
			style["link"] = map[string]any{"url": run.link}
			style["underline"] = true
			style["foregroundColor"] = docLinkColor
			names = append(names, "link", "underline", "foregroundColor")
		}
		if len(names) > 0 {
			w.add("updateTextStyle", map[string]any{"range": docRange(cursor, end), "textStyle": style, "fields": strings.Join(names, ",")})
		}
		cursor = end
	}
}
