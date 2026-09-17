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
	draft := []DocParagraph{P("h2", "Plan"), P("text", "Say **hi** to the *whole* world"), L("bullet", 0, "one"), L("bullet", 0, "two"), L("bullet", 0, "three"), P("text", "end"), P("text", "Something completely different from what stood here")}
	for i := range base {
		base[i].N = i + 1
	}
	for i := range draft {
		draft[i].N = i + 1
	}
	current := documentHunks(base, draft)
	doc, cards := suggestionDocument(base, draft, current)
	raw, _ := json.Marshal(doc)
	text := string(raw)
	// Adjacent changed paragraphs form one hunk, so one card: the heading
	// restyle and the intro edit share id 1; "gone" → "three" is too
	// different to pair and shows as a deleted and an inserted paragraph.
	for _, want := range []string{
		`"attrs":{"level":2,"suggestion":{"from":"h1","id":1,"kind":"restyle"}}`,
		`"marks":[{"type":"bold"},{"attrs":{"id":1},"type":"suggestDelete"}],"text":"hello"`,
		`"marks":[{"type":"bold"},{"attrs":{"id":1},"type":"suggestInsert"}],"text":"hi"`,
		`"marks":[{"type":"italic"},{"attrs":{"id":1},"type":"suggestInsert"}],"text":"whole"`,
		`"attrs":{"suggestion":{"id":2,"kind":"delete"}},"content":[{"marks":[{"attrs":{"id":2},"type":"suggestDelete"}],"text":"gone"`,
		`"attrs":{"suggestion":{"id":2,"kind":"insert"}},"content":[{"marks":[{"attrs":{"id":2},"type":"suggestInsert"}],"text":"three"`,
		`"attrs":{"suggestion":{"id":3,"kind":"insert"}}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("view lacks %s:\n%s", want, text)
		}
	}
	var summaries []string
	for _, c := range cards {
		summaries = append(summaries, c.Kind+": "+c.Summary)
	}
	if got := strings.Join(summaries, " | "); got != "replace: Replace “hello” with “hi whole” · h1 → h2 | replace: Replace “gone” with “three” | insert: Add “Something completely different from what stood here”" {
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
	// Words that only moved are neither deleted nor inserted.
	runs, ratio := inlineDiff(parseInline("a b c"), parseInline("a c b"), 1)
	if ratio < 0.5 || len(runs) < 3 {
		t.Fatalf("inline diff %v %v", runs, ratio)
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
}
