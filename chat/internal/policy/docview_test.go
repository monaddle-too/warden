package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// roundTrip converts through JSON so the test sees what the browser sees.
func roundTrip(t *testing.T, doc tipNode) any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTiptapDocumentRoundTripsTheCanonicalModel(t *testing.T) {
	table := F("table")
	table.Cells = [][]string{{"a", "b"}, {"c\nd", ""}}
	paragraphs := []DocParagraph{P("title", "Plan"), P("subtitle", "for the launch"), P("h1", "Goals"), P("text", "Say **hi** to *the* [world](https://x.y/) and\vbreak"), L("bullet", 0, "one"), L("bullet", 1, "two"), L("numbered", 1, "three"), L("numbered", 3, "deep"), L("bullet", 0, "back"), table, F("image"), P("text", ""), P("h6", "tail")}
	for i := range paragraphs {
		paragraphs[i].N = i + 1
	}
	doc := canonicalToDoc(paragraphs)
	var frozen []DocParagraph
	for _, p := range paragraphs {
		if p.Frozen != "" {
			frozen = append(frozen, p)
		}
	}
	back, err := docToCanonical(roundTrip(t, doc), frozen)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDocument(paragraphs, back) {
		t.Fatalf("round trip changed the document:\n%s\nvs\n%s", joinTexts(paragraphs), joinTexts(back))
	}
	if back[9].Cells == nil {
		t.Fatal("table cells lost")
	}
	// The nesting is real Tiptap structure: a bullet list holding an item
	// whose nested ordered list holds "three" and, two levels down through a
	// synthetic item, "deep".
	raw, _ := json.Marshal(doc)
	text := string(raw)
	for _, want := range []string{`"type":"orderedList"`, `"synthetic":true`, `"type":"hardBreak"`, `"docStyle":"title"`, `"type":"frozenTable"`, `"rows":[["a","b"],["c\nd",""]]`, `"level":6`} {
		if !strings.Contains(text, want) {
			t.Fatalf("document lacks %s: %s", want, text)
		}
	}
	// Unsupported content is refused by name; unsupported marks are dropped.
	bad := tipNode{"type": "doc", "content": []any{tipNode{"type": "codeBlock", "content": []any{tipNode{"type": "text", "text": "x"}}}}}
	if _, err := docToCanonical(roundTrip(t, bad), nil); err == nil || !strings.Contains(err.Error(), "codeBlock") {
		t.Fatalf("code block accepted: %v", err)
	}
	marked := tipNode{"type": "doc", "content": []any{tipNode{"type": "paragraph", "content": []any{tipNode{"type": "text", "text": "u", "marks": []any{tipNode{"type": "underline"}}}}}}}
	if out, err := docToCanonical(roundTrip(t, marked), nil); err != nil || out[0].Text != "u" {
		t.Fatalf("underline: %v %v", out, err)
	}
	if _, err := docToCanonical(roundTrip(t, canonicalToDoc(paragraphs[:9])), frozen); err == nil {
		t.Fatal("a document that drops frozen content must be refused")
	}
}

func TestSuggestionDocumentShowsChangesInPlaceAndStripsBackToTheDraft(t *testing.T) {
	base := []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the world"), L("bullet", 0, "one"), L("bullet", 0, "two"), P("text", "gone"), P("text", "end")}
	for i := range base {
		base[i].N = i + 1
	}
	// Five ops, some on neighbouring paragraphs: five suggestions.
	ops, err := decodeDocOps([]any{
		map[string]any{"type": "replace", "start": 1, "end": 1, "paragraphs": []any{map[string]any{"style": "h2", "text": "Plan"}}, "reason": "smaller heading"},
		map[string]any{"type": "replace", "start": 2, "end": 2, "paragraphs": []any{map[string]any{"style": "text", "text": "Say **hi** to the *whole* world"}}, "reason": "warmer"},
		map[string]any{"type": "insert", "after": 4, "paragraphs": []any{map[string]any{"style": "bullet", "text": "three"}}},
		map[string]any{"type": "delete", "start": 5, "end": 5, "reason": "stale"},
		map[string]any{"type": "insert", "after": 6, "paragraphs": []any{map[string]any{"style": "text", "text": "Something completely different from what stood here"}}},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	hunks := hunksFromOps(base, ops)
	draft := applyDocumentOps(base, ops)
	if !sameDocument(applyHunksToBase(base, hunks), draft) {
		t.Fatal("hunks from ops must reproduce the proposed document")
	}
	changes := reviewChanges(docProposal{Base: base, Hunks: hunks}, docDraft{Paragraphs: draft})
	if len(changes) != 5 {
		t.Fatalf("expected one change per op, got %d: %+v", len(changes), changes)
	}
	doc, cards := suggestionDocument(base, draft, changes, nil)
	raw, _ := json.Marshal(doc)
	text := string(raw)
	for _, want := range []string{
		`"attrs":{"level":2,"suggestion":{"author":"agent","from":"h1","id":1,"kind":"restyle"}}`,
		`"marks":[{"type":"bold"},{"attrs":{"author":"agent","id":2},"type":"suggestDelete"}],"text":"hello"`,
		`"marks":[{"type":"bold"},{"attrs":{"author":"agent","id":2},"type":"suggestInsert"}],"text":"hi"`,
		`"marks":[{"type":"italic"},{"attrs":{"author":"agent","id":2},"type":"suggestInsert"}],"text":"whole"`,
		`"attrs":{"suggestion":{"author":"agent","id":3,"kind":"insert"}},"content":[{"marks":[{"attrs":{"author":"agent","id":3},"type":"suggestInsert"}],"text":"three"`,
		`"attrs":{"suggestion":{"author":"agent","id":4,"kind":"delete"}},"content":[{"marks":[{"attrs":{"author":"agent","id":4},"type":"suggestDelete"}],"text":"gone"`,
		`"attrs":{"suggestion":{"author":"agent","id":5,"kind":"insert"}}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("view lacks %s:\n%s", want, text)
		}
	}
	var summaries []string
	for _, c := range cards {
		summaries = append(summaries, c.Kind+": "+c.Summary+" ["+strings.Join(c.Reasons, ";")+"]")
	}
	if got := strings.Join(summaries, " | "); got != "restyle: Format: h1 → h2 [smaller heading] | replace: Replace “hello” with “hi whole” [warmer] | insert: Add “three” [] | delete: Delete “gone” [stale] | insert: Add “Something completely different from what stood here” []" {
		t.Fatalf("cards: %s", got)
	}
	// Saving the page as it is returns exactly the draft.
	back, err := docToCanonical(roundTrip(t, doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDocument(back, draft) {
		t.Fatalf("stripped view is not the draft:\n%s\nvs\n%s", joinTexts(back), joinTexts(draft))
	}
	// Unchanged words stay kept around an edit; a token is a word with its
	// trailing whitespace so the stripped page reproduces the draft exactly.
	runs, ratio := inlineDiff(parseInline("a b c d"), parseInline("a b x d"), 1)
	if ratio < 0.7 || len(runs) != 4 || runs[0].text != "a b" || runs[1].change != "delete" || runs[2].text != " x" || runs[3].text != " d" {
		t.Fatalf("inline diff %v %v", runs, ratio)
	}
	// A short paragraph grown into a long one is still an edit of it.
	if _, ratio := inlineDiff(parseInline("Scope creep"), parseInline("Scope creep in the landing page (freeze the outline by September 25)"), 1); ratio < docSimilarity {
		t.Fatalf("short-to-long ratio %v", ratio)
	}
	// Tiny kept islands between edits read as one replacement.
	runs, _ = inlineDiff(parseInline("page for the developer docs, with a reference to the existing overview."), parseInline("page for the developer docs; it complements the overview rather than repeating it."), 1)
	var shape []string
	for _, r := range runs {
		shape = append(shape, r.change+":"+r.text)
	}
	if got := strings.Join(shape, "|"); got != ":page for the developer docs|delete:, with a reference to the existing|insert:; it complements the|: overview|insert: rather than repeating it|:." {
		t.Fatalf("cleanup: %s", got)
	}
}

func TestPagePostsBackAsTheDraftAndChangesCanBeReverted(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	f.grant()
	id := f.dispatch("doc_submit", docSubmission(nil))["request_id"].(string)
	preview := f.dispatch("doc_preview", map[string]any{"id": id})
	view := preview["view"].(map[string]any)
	cards := view["suggestions"].([]suggestionCard)
	if len(cards) != 2 || cards[0].Kind != "replace" || cards[1].Kind != "insert" || cards[0].Status != "pending" {
		t.Fatalf("cards: %+v", cards)
	}
	// The owner types into the page: edit the inserted bullet and post the
	// document back. Deleted text and suggestion marks vanish on the way.
	document := roundTrip(t, view["document"].(tipNode)).(map[string]any)
	edited := false
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["type"] == "text" && node["text"] == "three" {
			node["text"] = "three, then four"
			edited = true
		}
		for _, child := range listOf(node["content"]) {
			walk(child.(map[string]any))
		}
	}
	walk(document)
	if !edited {
		t.Fatal("fixture changed")
	}
	saved := f.dispatch("doc_draft", map[string]any{"id": id, "document": document, "comments": []any{map[string]any{"paragraph": 2, "from": 4, "to": 6, "quote": "hi", "text": "Too casual?"}}})
	draft := saved["draft"].(docDraft)
	if joinTexts(draft.Paragraphs) != "Plan|Say **hi** to the world|one|two|three, then four|end" {
		t.Fatalf("draft after page edit: %s", joinTexts(draft.Paragraphs))
	}
	if len(draft.Comments) != 1 || draft.Comments[0].Quote != "hi" || draft.Comments[0].To != 6 {
		t.Fatalf("comments: %+v", draft.Comments)
	}
	// Rejecting a current change puts the base text back.
	cards = saved["view"].(map[string]any)["suggestions"].([]suggestionCard)
	var replaceID int
	for _, c := range cards {
		if c.Kind == "replace" {
			replaceID = c.ID
		}
	}
	reverted := f.dispatch("doc_decide", map[string]any{"id": id, "change": replaceID, "accept": false})
	if got := joinTexts(reverted["draft"].(docDraft).Paragraphs); got != "Plan|Say **hello** to the world|one|two|three, then four|end" {
		t.Fatalf("after revert: %s", got)
	}
	cards = reverted["view"].(map[string]any)["suggestions"].([]suggestionCard)
	var rejected *suggestionCard
	for i := range cards {
		if cards[i].Status == "rejected" {
			rejected = &cards[i]
		}
	}
	if rejected == nil || !rejected.Acceptable || rejected.Hunk != 1 {
		t.Fatalf("rejected card: %+v", cards)
	}
	restored := f.dispatch("doc_decide", map[string]any{"id": id, "hunk": rejected.Hunk, "accept": true})
	if got := joinTexts(restored["draft"].(docDraft).Paragraphs); got != "Plan|Say **hi** to the world|one|two|three, then four|end" {
		t.Fatalf("after restore: %s", got)
	}
	if _, err := f.s.Dispatch("doc_draft", map[string]any{"id": id, "document": map[string]any{"type": "doc", "content": []any{map[string]any{"type": "table"}}}}); err == nil {
		t.Fatal("unsupported nodes must be refused")
	}
	// Accepting a change keeps it and shows it as plain text; editing that
	// paragraph again makes it a suggestion again.
	cards = restored["view"].(map[string]any)["suggestions"].([]suggestionCard)
	accepted := f.dispatch("doc_decide", map[string]any{"id": id, "change": cards[0].ID, "accept": true})
	view = accepted["view"].(map[string]any)
	if got := view["suggestions"].([]suggestionCard); got[0].Status != "accepted" {
		t.Fatalf("accepted card: %+v", got)
	}
	if raw, _ := json.Marshal(view["document"]); strings.Contains(string(raw), `"kind":"replace"`) || !strings.Contains(string(raw), `"kind":"accepted"`) {
		t.Fatalf("accepted change still drawn as a suggestion: %s", raw)
	}
	page := roundTrip(t, view["document"].(tipNode)).(map[string]any)
	walk = func(node map[string]any) {
		if node["type"] == "text" && node["text"] == "hi" {
			node["text"] = "hey"
		}
		for _, child := range listOf(node["content"]) {
			walk(child.(map[string]any))
		}
	}
	walk(page)
	again := f.dispatch("doc_draft", map[string]any{"id": id, "document": page})
	if got := again["view"].(map[string]any)["suggestions"].([]suggestionCard); got[0].Status != "pending" || !strings.Contains(got[0].Summary, "hey") {
		t.Fatalf("edited accepted change: %+v", got)
	}
}
