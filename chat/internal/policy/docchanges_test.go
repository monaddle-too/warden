package policy

import (
	"strconv"
	"strings"
	"testing"
)

func changeShapes(changes []DocChange) string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Author+"#"+strconv.Itoa(c.ID)+":"+joinTexts(c.Removed)+"→"+joinTexts(c.Added))
	}
	return strings.Join(out, " | ")
}

func TestReviewSplitsChangesAlongSuggestionsAndAttributesTheRest(t *testing.T) {
	base := []DocParagraph{P("h1", "Plan"), P("text", "intro"), P("text", "a"), P("text", "b"), P("text", "c"), P("text", "end")}
	for i := range base {
		base[i].N = i + 1
	}
	ops, err := decodeDocOps([]any{
		map[string]any{"type": "replace", "start": 3, "end": 3, "paragraphs": []any{map[string]any{"text": "A"}}, "reason": "one"},
		map[string]any{"type": "replace", "start": 4, "end": 4, "paragraphs": []any{map[string]any{"text": "B"}}, "reason": "two"},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	proposal := docProposal{Base: base, Hunks: hunksFromOps(base, ops)}
	// Verbatim: two suggestions, not one merged hunk.
	draft := applyDocumentOps(base, ops)
	if got := changeShapes(reviewChanges(proposal, docDraft{Paragraphs: draft})); got != "agent#1:a→A | agent#2:b→B" {
		t.Fatalf("verbatim: %s", got)
	}
	// The owner edits the paragraph before, and the one after: theirs.
	draft[1].Text, draft[4].Text = "Intro", "C"
	if got := changeShapes(reviewChanges(proposal, docDraft{Paragraphs: draft})); got != "owner#10000:intro→Intro | agent#1:a→A | agent#2:b→B | owner#10001:c→C" {
		t.Fatalf("owner edits around: %s", got)
	}
	// The owner edits inside a suggestion: that one is both, its sibling stays.
	draft = applyDocumentOps(base, ops)
	draft[2].Text = "A!"
	got := reviewChanges(proposal, docDraft{Paragraphs: draft})
	if s := changeShapes(got); s != "both#1:a→A! | agent#2:b→B" {
		t.Fatalf("edited inside: %s", s)
	}
	if strings.Join(got[0].Reasons, ",") != "one" {
		t.Fatalf("reasons kept: %v", got[0].Reasons)
	}
	// Rejecting one suggestion leaves the other its own change; reverting
	// by id restores exactly that paragraph.
	full := docDraft{Paragraphs: applyDocumentOps(base, ops)}
	if err := revert(proposal, &full, 1); err != nil {
		t.Fatal(err)
	}
	if got := changeShapes(reviewChanges(proposal, full)); got != "agent#2:b→B" {
		t.Fatalf("after revert: %s", got)
	}
}

func TestRevisionOpsBecomeHunksAgainstTheBase(t *testing.T) {
	base := []DocParagraph{P("text", "a"), P("text", "b"), P("text", "c"), P("text", "d")}
	for i := range base {
		base[i].N = i + 1
	}
	first, _ := decodeDocOps([]any{
		map[string]any{"type": "insert", "after": 1, "paragraphs": []any{map[string]any{"text": "x"}}, "reason": "add x"},
		map[string]any{"type": "replace", "start": 3, "end": 3, "paragraphs": []any{map[string]any{"text": "C"}}, "reason": "cap"},
	}, base)
	proposal := docProposal{Base: base, Hunks: hunksFromOps(base, first)}
	returned := applyDocumentOps(base, first) // a x b C d
	var kept []DocHunk
	for _, c := range reviewChanges(proposal, docDraft{Paragraphs: returned}) {
		kept = append(kept, c.DocHunk)
	}
	// Round two, against the returned draft: change "d" (untouched before)
	// and "C" (inside the kept replacement).
	second, err := decodeDocOps([]any{
		map[string]any{"type": "replace", "start": 4, "end": 4, "paragraphs": []any{map[string]any{"text": "CC"}}, "reason": "more"},
		map[string]any{"type": "replace", "start": 5, "end": 5, "paragraphs": []any{map[string]any{"text": "D"}}, "reason": "cap d"},
	}, returned)
	if err != nil {
		t.Fatal(err)
	}
	hunks := hunksFromRevision(base, returned, kept, second)
	proposed := applyDocumentOps(returned, second)
	if !sameDocument(applyHunksToBase(base, hunks), proposed) {
		t.Fatalf("revision hunks do not reproduce the proposal: %s vs %s", joinTexts(applyHunksToBase(base, hunks)), joinTexts(proposed))
	}
	var shapes []string
	for _, h := range hunks {
		shapes = append(shapes, joinTexts(h.Removed)+"→"+joinTexts(h.Added)+"["+strings.Join(h.Reasons, ",")+"]")
	}
	if got := strings.Join(shapes, " | "); got != "→x[add x] | c→CC[cap,more] | d→D[cap d]" {
		t.Fatalf("revision hunks: %s", got)
	}
}
