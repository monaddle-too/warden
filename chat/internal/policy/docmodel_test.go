package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
)

// docFixture builds a documents.get response from canonical paragraphs the
// way Google would return it: UTF-16 indexes, a leading section break,
// runs with text styles, bullets with list properties, and tables or
// images where a paragraph is frozen.
func docFixture(revision string, paragraphs ...DocParagraph) map[string]any {
	content := []any{map[string]any{"startIndex": 0, "endIndex": 1, "sectionBreak": map[string]any{}}}
	lists := map[string]any{}
	index := 1
	for i, p := range paragraphs {
		start := index
		switch p.Frozen {
		case "table":
			// A 1×1 table with one empty cell paragraph: 5 index units.
			index += 5
			content = append(content, map[string]any{"startIndex": start, "endIndex": index, "table": map[string]any{"rows": 1, "columns": 1, "tableRows": []any{map[string]any{"tableCells": []any{map[string]any{"content": []any{}}}}}}})
			continue
		case "image":
			elements := []any{map[string]any{"startIndex": start, "endIndex": start + 1, "inlineObjectElement": map[string]any{"inlineObjectId": "kix.img"}}, map[string]any{"startIndex": start + 1, "endIndex": start + 2, "textRun": map[string]any{"content": "\n", "textStyle": map[string]any{}}}}
			index += 2
			content = append(content, map[string]any{"startIndex": start, "endIndex": index, "paragraph": map[string]any{"elements": elements, "paragraphStyle": map[string]any{"namedStyleType": "NORMAL_TEXT"}}})
			continue
		}
		var elements []any
		runs := parseInline(p.Text)
		for _, run := range runs {
			style := map[string]any{}
			if run.bold {
				style["bold"] = true
			}
			if run.italic {
				style["italic"] = true
			}
			if run.link != "" {
				style["link"] = map[string]any{"url": run.link}
			}
			length := utf16Len(run.text)
			elements = append(elements, map[string]any{"startIndex": index, "endIndex": index + length, "textRun": map[string]any{"content": run.text, "textStyle": style}})
			index += length
		}
		if len(elements) > 0 {
			last := elements[len(elements)-1].(map[string]any)
			run := last["textRun"].(map[string]any)
			run["content"] = run["content"].(string) + "\n"
			last["endIndex"] = index + 1
		} else {
			elements = append(elements, map[string]any{"startIndex": index, "endIndex": index + 1, "textRun": map[string]any{"content": "\n", "textStyle": map[string]any{}}})
		}
		index++
		paragraph := map[string]any{"elements": elements, "paragraphStyle": map[string]any{"namedStyleType": "NORMAL_TEXT"}}
		if named, ok := docStyleNames[p.Style]; ok {
			paragraph["paragraphStyle"] = map[string]any{"namedStyleType": named}
		}
		if docListStyle(p.Style) {
			listID := "kix." + p.Style
			paragraph["bullet"] = map[string]any{"listId": listID, "nestingLevel": p.Depth}
			levels := []any{}
			for level := 0; level <= docListDepthMax; level++ {
				if p.Style == "numbered" {
					levels = append(levels, map[string]any{"glyphType": "DECIMAL"})
				} else {
					levels = append(levels, map[string]any{"glyphSymbol": "●", "glyphType": "GLYPH_TYPE_UNSPECIFIED"})
				}
			}
			lists[listID] = map[string]any{"listProperties": map[string]any{"nestingLevels": levels}}
		}
		_ = i
		content = append(content, map[string]any{"startIndex": start, "endIndex": index, "paragraph": paragraph})
	}
	return map[string]any{"documentId": "doc-a", "title": "Plan", "revisionId": revision, "body": map[string]any{"content": content}, "lists": lists}
}

func P(style, text string) DocParagraph { return DocParagraph{Style: style, Text: text} }
func L(style string, depth int, text string) DocParagraph {
	return DocParagraph{Style: style, Depth: depth, Text: text}
}
func F(kind string) DocParagraph {
	text := "[" + kind + "]"
	if kind == "table" {
		text = "[table 1×1]"
	}
	return DocParagraph{Frozen: kind, Style: "text", Text: text}
}

func TestProjectionReadsStylesMarksListsAndFreezesTheRest(t *testing.T) {
	doc := docFixture("r1", P("h1", "Plan"), P("text", "Say **hello** to *the* [world](https://example.com/a) today"), L("bullet", 1, "nested item"), L("numbered", 0, "first"), F("table"), F("image"), P("text", ""))
	projection, err := ProjectDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RevisionID != "r1" || projection.Title != "Plan" || len(projection.Paragraphs) != 7 {
		t.Fatalf("unexpected projection %+v", projection)
	}
	got := projection.Paragraphs
	want := []struct {
		style, text, frozen string
		depth               int
	}{{"h1", "Plan", "", 0}, {"text", "Say **hello** to *the* [world](https://example.com/a) today", "", 0}, {"bullet", "nested item", "", 1}, {"numbered", "first", "", 0}, {"text", "[table 1×1]", "table", 0}, {"text", "[image]", "image", 0}, {"text", "", "", 0}}
	for i, w := range want {
		if got[i].N != i+1 || got[i].Style != w.style || got[i].Text != w.text || got[i].Frozen != w.frozen || got[i].Depth != w.depth {
			t.Fatalf("paragraph %d: %+v", i+1, got[i])
		}
	}
	// Ranges are contiguous UTF-16 units from index 1.
	index := 1
	for _, p := range got {
		if p.Start != index || p.End <= p.Start {
			t.Fatalf("paragraph %d range %d-%d, expected start %d", p.N, p.Start, p.End, index)
		}
		index = p.End
	}
	if len(projection.Notes) != 1 || !strings.Contains(projection.Notes[0], "2 paragraph(s) are frozen") {
		t.Fatalf("notes %v", projection.Notes)
	}
	if got[0].named != "HEADING_1" || got[2].named != "NORMAL_TEXT" {
		t.Fatalf("underlying named styles %q %q", got[0].named, got[2].named)
	}
}

func TestInlineMarksRoundTripAndEscape(t *testing.T) {
	cases := []string{"plain", "**bold** and *italic*", "***both*** then **bold*and italic***", "[link](https://x.y/z?q=1)", "a \\* literal \\[star\\] and \\\\ slash", "**[bold link](https://x.y/)** tail", "", "emoji 😀 **bold**"}
	for _, text := range cases {
		if rendered := renderInline(parseInline(text)); rendered != text {
			t.Fatalf("%q rendered as %q", text, rendered)
		}
	}
	// Equivalent spellings normalise to one canonical text.
	if a, b := renderInline(parseInline("[**bold link**](https://x.y/) tail")), "**[bold link](https://x.y/)** tail"; a != b {
		t.Fatalf("canonical form %q, want %q", a, b)
	}
	runs := parseInline("**a*****b***")
	if len(runs) != 2 || !runs[0].bold || runs[0].italic || !runs[1].bold || !runs[1].italic {
		t.Fatalf("toggle grammar parsed %+v", runs)
	}
	if plainInline("**a** [b](https://c) \\*d") != "a b *d" {
		t.Fatalf("plain text %q", plainInline("**a** [b](https://c) \\*d"))
	}
	if got := renderInline([]docRun{{text: "see [1]"}}); got != "see \\[1\\]" {
		t.Fatalf("escape %q", got)
	}
	if got := parseInline("[not a link"); len(got) != 1 || got[0].text != "[not a link" {
		t.Fatalf("unmatched bracket %+v", got)
	}
	if utf16Len("a😀b") != 4 {
		t.Fatal("surrogate pairs count twice")
	}
}

func TestOpsAreValidatedAndApplied(t *testing.T) {
	base := []DocParagraph{P("h1", "Plan"), P("text", "one"), P("text", "two"), F("table"), P("text", "three")}
	for i := range base {
		base[i].N = i + 1
	}
	ops, err := decodeDocOps([]any{
		map[string]any{"type": "replace", "start": 2, "end": 2, "paragraphs": []any{map[string]any{"text": "ONE", "style": "text"}}, "reason": "clearer"},
		map[string]any{"type": "insert", "after": 0, "paragraphs": []any{map[string]any{"text": "Summary", "style": "h2"}}},
		map[string]any{"type": "delete", "start": 3, "end": 3},
		map[string]any{"type": "insert", "after": 5, "paragraphs": []any{map[string]any{"text": "item", "style": "bullet", "depth": 2}}},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	out := applyDocumentOps(base, ops)
	var texts []string
	for _, p := range out {
		texts = append(texts, p.Style+":"+p.Text)
	}
	if strings.Join(texts, "|") != "h2:Summary|h1:Plan|text:ONE|text:[table 1×1]|text:three|bullet:item" || out[3].Frozen != "table" || out[5].Depth != 2 {
		t.Fatalf("applied %v", texts)
	}
	for i, p := range out {
		if p.N != i+1 {
			t.Fatal("renumbered")
		}
	}
	rejects := map[string]any{
		"frozen":     []any{map[string]any{"type": "delete", "start": 4, "end": 4}},
		"overlap":    []any{map[string]any{"type": "replace", "start": 2, "end": 3, "paragraphs": []any{map[string]any{"text": "x"}}}, map[string]any{"type": "delete", "start": 3, "end": 3}},
		"range":      []any{map[string]any{"type": "delete", "start": 3, "end": 2}},
		"style":      []any{map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "x", "style": "h9"}}}},
		"depth":      []any{map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "x", "style": "text", "depth": 1}}}},
		"tab":        []any{map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "\tx", "style": "bullet"}}}},
		"newline":    []any{map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "x\ny"}}}},
		"field":      []any{map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "x", "n": 3}}}},
		"beyond":     []any{map[string]any{"type": "insert", "after": 9, "paragraphs": []any{map[string]any{"text": "x"}}}},
		"empty":      []any{},
		"delete+par": []any{map[string]any{"type": "delete", "start": 2, "end": 2, "paragraphs": []any{}}},
	}
	for name, raw := range rejects {
		if _, err := decodeDocOps(raw, base); err == nil || !isValueError(err) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestHunksAnchorOnFrozenParagraphsAndBindReasons(t *testing.T) {
	base := []DocParagraph{P("text", "a"), F("table"), P("text", "b"), P("text", "c")}
	after := []DocParagraph{P("text", "a2"), F("table"), P("text", "b"), P("text", "c2"), P("text", "d")}
	hunks := documentHunks(base, after)
	if len(hunks) != 2 || hunks[0].From != 0 || hunks[0].To != 1 || hunks[1].From != 3 || hunks[1].To != 4 || len(hunks[1].Added) != 2 {
		t.Fatalf("hunks %+v", hunks)
	}
	ops := []DocOp{{Reason: "first", baseFrom: 0, baseTo: 1}, {Reason: "tail", baseFrom: 3, baseTo: 4}, {Reason: "append", baseFrom: 4, baseTo: 4}}
	bindReasons(hunks, ops)
	if strings.Join(hunks[0].Reasons, ",") != "first" || strings.Join(hunks[1].Reasons, ",") != "tail,append" {
		t.Fatalf("reasons %v %v", hunks[0].Reasons, hunks[1].Reasons)
	}
	// A frozen paragraph moved by the draft falls back to a plain diff and
	// the compiler refuses it.
	moved := []DocParagraph{F("table"), P("text", "a"), P("text", "b"), P("text", "c")}
	if hunks := documentHunks(base, moved); len(hunks) == 0 {
		t.Fatal("moved frozen paragraph must diff")
	}
}

func TestThreeWayMergeCombinesDistinctChangesAndReportsConflicts(t *testing.T) {
	base := []DocParagraph{P("text", "a"), P("text", "b"), P("text", "c"), P("text", "d")}
	ours := []DocParagraph{P("text", "A"), P("text", "b"), P("text", "c"), P("text", "d"), P("text", "e")}
	theirs := []DocParagraph{P("text", "a"), P("text", "b"), P("text", "C"), P("text", "d")}
	merged, conflicts := mergeDocuments(base, ours, theirs)
	if len(conflicts) != 0 || joinTexts(merged) != "A|b|C|d|e" {
		t.Fatalf("merged %s conflicts %+v", joinTexts(merged), conflicts)
	}
	theirs = []DocParagraph{P("text", "AA"), P("text", "b"), P("text", "c"), P("text", "d")}
	merged, conflicts = mergeDocuments(base, ours, theirs)
	if len(conflicts) != 1 || joinTexts(merged) != "AA|b|c|d|e" || joinTexts(conflicts[0].Ours) != "A" || joinTexts(conflicts[0].Theirs) != "AA" || conflicts[0].Position != 1 {
		t.Fatalf("merged %s conflicts %+v", joinTexts(merged), conflicts)
	}
	// Insertions at the same point are ambiguous; adjacent replacements are not.
	ours = []DocParagraph{P("text", "a"), P("text", "x"), P("text", "b"), P("text", "c"), P("text", "d")}
	theirs = []DocParagraph{P("text", "a"), P("text", "y"), P("text", "b"), P("text", "c"), P("text", "d")}
	if _, conflicts = mergeDocuments(base, ours, theirs); len(conflicts) != 1 {
		t.Fatalf("same-point insertions must conflict: %+v", conflicts)
	}
	ours = []DocParagraph{P("text", "A"), P("text", "b"), P("text", "c"), P("text", "d")}
	theirs = []DocParagraph{P("text", "a"), P("text", "B"), P("text", "c"), P("text", "d")}
	if merged, conflicts = mergeDocuments(base, ours, theirs); len(conflicts) != 0 || joinTexts(merged) != "A|B|c|d" {
		t.Fatalf("adjacent edits: %s %+v", joinTexts(merged), conflicts)
	}
}

func joinTexts(paragraphs []DocParagraph) string {
	var out []string
	for _, p := range paragraphs {
		out = append(out, p.Text)
	}
	return strings.Join(out, "|")
}

// simDoc replays batchUpdate requests the way the Docs API applies them:
// UTF-16 indexes, newlines carry paragraph properties, inserted text
// copies the properties at its index, createParagraphBullets consumes
// leading tabs as nesting. It then projects the result for comparison.
type simDoc struct {
	units  []uint16
	marks  []docRun // per unit: bold, italic, link
	paras  []simPara
	frozen map[int]DocParagraph // paragraph index → frozen paragraph, never touched
}

type simPara struct {
	named string
	list  string
	depth int
}

func newSimDoc(t *testing.T, paragraphs []DocParagraph) *simDoc {
	t.Helper()
	d := &simDoc{units: []uint16{0}, marks: []docRun{{}}, frozen: map[int]DocParagraph{}}
	for i, p := range paragraphs {
		para := simPara{named: docNamedStyle(p)}
		if docListStyle(p.Style) {
			para.list, para.depth = p.Style, p.Depth
		}
		if p.Frozen != "" {
			// The same unit layout docFixture uses: a table is 5 units, an
			// inline image 2; their content is opaque to the compiler.
			d.frozen[i] = p
			filler := 1
			if p.Frozen == "table" {
				filler = 4
			}
			for j := 0; j < filler; j++ {
				d.units = append(d.units, 0xFFFD)
				d.marks = append(d.marks, docRun{})
			}
			d.units = append(d.units, '\n')
			d.marks = append(d.marks, docRun{})
			d.paras = append(d.paras, para)
			continue
		}
		for _, run := range parseInline(p.Text) {
			for _, u := range utf16.Encode([]rune(run.text)) {
				d.units = append(d.units, u)
				d.marks = append(d.marks, docRun{bold: run.bold, italic: run.italic, link: run.link})
			}
		}
		d.units = append(d.units, '\n')
		d.marks = append(d.marks, docRun{})
		d.paras = append(d.paras, para)
	}
	return d
}

// paragraphAt is the paragraph index owning unit index i.
func (d *simDoc) paragraphAt(i int) int {
	n := 0
	for j := 1; j < i && j < len(d.units); j++ {
		if d.units[j] == '\n' {
			n++
		}
	}
	return n
}

func (d *simDoc) bounds(p int) (start, newline int) {
	n := 0
	start = 1
	for i := 1; i < len(d.units); i++ {
		if n == p {
			for j := i; j < len(d.units); j++ {
				if d.units[j] == '\n' {
					return i, j
				}
			}
		}
		if d.units[i] == '\n' {
			n++
			start = i + 1
		}
	}
	return start, len(d.units) - 1
}

func (d *simDoc) apply(t *testing.T, body map[string]any) {
	t.Helper()
	requests := body["requests"].([]map[string]any)
	for _, request := range requests {
		for kind, raw := range request {
			r := raw.(map[string]any)
			rng, _ := r["range"].(map[string]any)
			start, _ := rng["startIndex"].(int)
			end, _ := rng["endIndex"].(int)
			switch kind {
			case "insertText":
				index := r["location"].(map[string]any)["index"].(int)
				if index < 1 || index >= len(d.units) {
					t.Fatalf("insert outside document at %d (len %d)", index, len(d.units))
				}
				p := d.paragraphAt(index)
				if _, ok := d.frozen[p]; ok {
					t.Fatalf("insert into frozen paragraph %d", p)
				}
				text := utf16.Encode([]rune(r["text"].(string)))
				inherit := d.marks[index-1]
				var marks []docRun
				newlines := 0
				for _, u := range text {
					marks = append(marks, inherit)
					if u == '\n' {
						newlines++
					}
				}
				d.units = append(d.units[:index], append(text, d.units[index:]...)...)
				d.marks = append(d.marks[:index], append(marks, d.marks[index:]...)...)
				copies := make([]simPara, newlines)
				for i := range copies {
					copies[i] = d.paras[p]
				}
				d.paras = append(d.paras[:p], append(copies, d.paras[p:]...)...)
				shifted := map[int]DocParagraph{}
				for k, v := range d.frozen {
					if k >= p {
						k += newlines
					}
					shifted[k] = v
				}
				d.frozen = shifted
			case "deleteContentRange":
				if start < 1 || end > len(d.units)-1 || start >= end {
					t.Fatalf("delete range %d-%d outside document (len %d)", start, end, len(d.units))
				}
				first := d.paragraphAt(start)
				removed := 0
				for i := start; i < end; i++ {
					if _, ok := d.frozen[d.paragraphAt(i)]; ok {
						t.Fatalf("delete touches frozen paragraph at %d", i)
					}
					if d.units[i] == '\n' {
						removed++
					}
				}
				d.units = append(d.units[:start], d.units[end:]...)
				d.marks = append(d.marks[:start], d.marks[end:]...)
				// Removed newlines merge their text into the next surviving
				// paragraph, which keeps its own properties.
				d.paras = append(d.paras[:first], d.paras[first+removed:]...)
				shifted := map[int]DocParagraph{}
				for k, v := range d.frozen {
					if k >= first+removed {
						k -= removed
					}
					shifted[k] = v
				}
				d.frozen = shifted
			case "updateParagraphStyle":
				style := r["paragraphStyle"].(map[string]any)
				for _, p := range d.overlapping(t, start, end) {
					if named, ok := style["namedStyleType"].(string); ok {
						d.paras[p].named = named
					}
				}
			case "deleteParagraphBullets":
				for _, p := range d.overlapping(t, start, end) {
					d.paras[p].list, d.paras[p].depth = "", 0
				}
			case "createParagraphBullets":
				preset := r["bulletPreset"].(string)
				list := "bullet"
				if strings.HasPrefix(preset, "NUMBERED") {
					list = "numbered"
				}
				for _, p := range d.overlapping(t, start, end) {
					s, _ := d.bounds(p)
					tabs := 0
					for s+tabs < len(d.units) && d.units[s+tabs] == '\t' {
						tabs++
					}
					d.units = append(d.units[:s], d.units[s+tabs:]...)
					d.marks = append(d.marks[:s], d.marks[s+tabs:]...)
					d.paras[p].list, d.paras[p].depth = list, tabs
				}
			case "updateTextStyle":
				if start < 1 || end > len(d.units) || start >= end {
					t.Fatalf("text style range %d-%d outside document", start, end)
				}
				style := r["textStyle"].(map[string]any)
				fields := strings.Split(r["fields"].(string), ",")
				for i := start; i < end; i++ {
					if d.units[i] == '\n' {
						t.Fatalf("text style range %d-%d crosses a paragraph", start, end)
					}
					for _, field := range fields {
						switch field {
						case "bold":
							d.marks[i].bold, _ = style["bold"].(bool)
						case "italic":
							d.marks[i].italic, _ = style["italic"].(bool)
						case "link":
							link, _ := style["link"].(map[string]any)
							d.marks[i].link = stringField(link, "url")
						}
					}
				}
			default:
				t.Fatalf("unexpected request %s", kind)
			}
		}
	}
}

func (d *simDoc) overlapping(t *testing.T, start, end int) []int {
	t.Helper()
	if start < 1 || end > len(d.units) || start >= end {
		t.Fatalf("paragraph range %d-%d outside document", start, end)
	}
	var out []int
	seen := map[int]bool{}
	for i := start; i < end; i++ {
		p := d.paragraphAt(i)
		if _, frozen := d.frozen[p]; frozen {
			t.Fatalf("paragraph request %d-%d touches frozen paragraph %d", start, end, p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// project renders the simulated document as canonical paragraphs.
func (d *simDoc) project() []DocParagraph {
	var out []DocParagraph
	p := 0
	var runs []docRun
	var units [][]uint16
	for i := 1; i < len(d.units); i++ {
		if d.units[i] == '\n' {
			for j := range runs {
				runs[j].text = string(utf16.Decode(units[j]))
			}
			para := d.paras[p]
			dp := DocParagraph{N: p + 1, Text: renderInline(runs), Style: docNamedStyles[para.named]}
			if para.list != "" {
				dp.Style, dp.Depth = para.list, para.depth
			}
			if frozen, ok := d.frozen[p]; ok {
				dp.Frozen, dp.Text = frozen.Frozen, frozen.Text
			}
			out = append(out, dp)
			runs, units = nil, nil
			p++
			continue
		}
		m := d.marks[i]
		if n := len(runs); n == 0 || runs[n-1].bold != m.bold || runs[n-1].italic != m.italic || runs[n-1].link != m.link {
			runs = append(runs, docRun{bold: m.bold, italic: m.italic, link: m.link})
			units = append(units, nil)
		}
		units[len(units)-1] = append(units[len(units)-1], d.units[i])
	}
	return out
}

// compileCase compiles draft against base, replays the requests and checks
// the simulated document equals the draft.
func compileCase(t *testing.T, name string, base, draft []DocParagraph) map[string]any {
	t.Helper()
	for i := range base {
		base[i].N = i + 1
	}
	projection, err := ProjectDocument(docFixture("r1", base...))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	body, _, err := CompileDocumentUpdate(projection, draft)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	sim := newSimDoc(t, base)
	sim.apply(t, body)
	got := sim.project()
	if len(got) != len(draft) {
		t.Fatalf("%s: %d paragraphs, want %d: %s vs %s", name, len(got), len(draft), joinTexts(got), joinTexts(draft))
	}
	for i := range draft {
		g, w := got[i], draft[i]
		if g.Frozen != w.Frozen || (w.Frozen == "" && (g.Text != w.Text || g.Style != w.Style || g.Depth != w.Depth)) {
			t.Fatalf("%s: paragraph %d is %+v, want %+v\nrequests: %s", name, i+1, g, w, mustJSON(body["requests"]))
		}
	}
	if control := body["writeControl"].(map[string]any); control["requiredRevisionId"] != "r1" {
		t.Fatalf("%s: revision control %v", name, control)
	}
	return body
}

func TestCompiledWriteReproducesTheDraft(t *testing.T) {
	base := []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}
	cases := []struct {
		name  string
		draft []DocParagraph
	}{
		{"edit one paragraph, keep marks", []DocParagraph{P("h1", "Plan"), P("text", "Say **hi** to the [world](https://example.com/) tomorrow"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"restyle heading to text and text to list", []DocParagraph{P("text", "Plan"), L("numbered", 1, "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"list item to heading", []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), P("h2", "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"insert after a paragraph", []DocParagraph{P("h1", "Plan"), P("h2", "Summary"), P("text", "New *intro*"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"insert at the top", []DocParagraph{P("title", "Top"), P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"insert list items before a table", []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), L("bullet", 2, "three"), L("numbered", 0, "four"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"insert after a table", []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "fresh"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"delete in the middle", []DocParagraph{P("h1", "Plan"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"delete the end", []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), P("text", "")}},
		{"replace many with fewer", []DocParagraph{P("h1", "Plan"), P("text", "merged"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"replace few with many", []DocParagraph{P("h1", "Plan"), P("text", "a"), P("text", "b"), P("text", "c"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last")}},
		{"append at the end", []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last"), P("h2", "Next"), L("numbered", 0, "x"), L("numbered", 0, "y")}},
		{"empty a paragraph and fill an empty one", []DocParagraph{P("h1", ""), P("text", "Say **hello** to the [world](https://example.com/) today"), L("bullet", 0, "one"), L("bullet", 1, "two"), F("table"), P("text", "after table"), L("numbered", 0, "n1"), P("text", "last 😀 emoji")}},
	}
	for _, c := range cases {
		compileCase(t, c.name, append([]DocParagraph{}, base...), c.draft)
	}
	frozenTop := []DocParagraph{F("table"), P("text", "x")}
	projection, _ := ProjectDocument(docFixture("r1", frozenTop...))
	if _, _, err := CompileDocumentUpdate(projection, []DocParagraph{F("table"), P("text", "x")}); err == nil || !strings.Contains(err.Error(), "nothing to write") {
		t.Fatalf("unchanged: %v", err)
	}
	if _, _, err := CompileDocumentUpdate(projection, []DocParagraph{P("text", "x"), F("table")}); err == nil || !isValueError(err) {
		t.Fatalf("moved table: %v", err)
	}
}

func TestCompiledRequestsNeverTouchUnchangedParagraphs(t *testing.T) {
	base := []DocParagraph{P("text", "keep one"), P("text", "change me"), P("text", "keep two")}
	body := compileCase(t, "isolation", base, []DocParagraph{P("text", "keep one"), P("text", "changed"), P("text", "keep two")})
	projection, _ := ProjectDocument(docFixture("r1", base...))
	keep1, keep2 := projection.Paragraphs[0], projection.Paragraphs[2]
	raw, _ := json.Marshal(body["requests"])
	var requests []map[string]map[string]any
	_ = json.Unmarshal(raw, &requests)
	for _, request := range requests {
		for _, r := range request {
			rng, _ := r["range"].(map[string]any)
			if rng == nil {
				continue
			}
			start, end := int(rng["startIndex"].(float64)), int(rng["endIndex"].(float64))
			if start < keep1.End || end > keep2.Start {
				t.Fatalf("request %v touches an unchanged paragraph", r)
			}
		}
	}
}
