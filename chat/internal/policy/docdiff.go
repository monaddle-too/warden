package policy

import "sort"

// DocHunk is one contiguous change between two paragraph lists: base
// positions [From, To) (0-based, half-open; empty for a pure insertion)
// become Added. Hunks are a view computed from two documents, never state.
type DocHunk struct {
	ID        int            `json:"id"`
	From      int            `json:"from"`
	To        int            `json:"to"`
	AfterFrom int            `json:"after_from"`
	AfterTo   int            `json:"after_to"`
	Removed   []DocParagraph `json:"removed"`
	Added     []DocParagraph `json:"added"`
	Reasons   []string       `json:"reasons,omitempty"`
}

// docDiffCells bounds the LCS table; beyond it the changed core is one hunk.
const docDiffCells = 4_000_000

// documentHunks diffs two documents at paragraph granularity. Frozen
// paragraphs are anchors: when both sides carry the same frozen sequence,
// each stretch between two anchors is diffed on its own, so a frozen block
// is never inside a hunk.
func documentHunks(base, after []DocParagraph) []DocHunk {
	anchorsA, anchorsB := frozenPositions(base), frozenPositions(after)
	if len(anchorsA) == 0 || len(anchorsA) != len(anchorsB) {
		return diffParagraphs(base, after, 0, 0)
	}
	for i := range anchorsA {
		if base[anchorsA[i]].key() != after[anchorsB[i]].key() {
			return diffParagraphs(base, after, 0, 0)
		}
	}
	var hunks []DocHunk
	a, b := 0, 0
	for i := 0; i <= len(anchorsA); i++ {
		endA, endB := len(base), len(after)
		if i < len(anchorsA) {
			endA, endB = anchorsA[i], anchorsB[i]
		}
		hunks = append(hunks, diffParagraphs(base[a:endA], after[b:endB], a, b)...)
		a, b = endA+1, endB+1
	}
	for i := range hunks {
		hunks[i].ID = i + 1
	}
	return hunks
}

func frozenPositions(paragraphs []DocParagraph) []int {
	var out []int
	for i, p := range paragraphs {
		if p.Frozen != "" {
			out = append(out, i)
		}
	}
	return out
}

// diffParagraphs diffs two stretches whose positions start at offsetA and
// offsetB in their documents.
func diffParagraphs(base, after []DocParagraph, offsetA, offsetB int) []DocHunk {
	keysA := make([]string, len(base))
	for i, p := range base {
		keysA[i] = p.key()
	}
	keysB := make([]string, len(after))
	for i, p := range after {
		keysB[i] = p.key()
	}
	n, m := len(keysA), len(keysB)
	prefix := 0
	for prefix < n && prefix < m && keysA[prefix] == keysB[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < n-prefix && suffix < m-prefix && keysA[n-1-suffix] == keysB[m-1-suffix] {
		suffix++
	}
	core, other := keysA[prefix:n-suffix], keysB[prefix:m-suffix]
	var ops []diffOp
	if len(core)*len(other) > docDiffCells {
		if len(core) > 0 {
			ops = append(ops, diffOp{kind: '-', a: 0, b: 0, length: len(core)})
		}
		if len(other) > 0 {
			ops = append(ops, diffOp{kind: '+', a: len(core), b: 0, length: len(other)})
		}
	} else {
		ops = lcsOps(core, other)
	}
	var hunks []DocHunk
	var current *DocHunk
	for _, op := range ops {
		if op.kind == '=' {
			current = nil
			continue
		}
		if current == nil {
			hunks = append(hunks, DocHunk{From: prefix + op.a, To: prefix + op.a, AfterFrom: prefix + op.b, AfterTo: prefix + op.b, Removed: []DocParagraph{}, Added: []DocParagraph{}})
			current = &hunks[len(hunks)-1]
		}
		switch op.kind {
		case '-':
			current.Removed = append(current.Removed, base[current.To:current.To+op.length]...)
			current.To += op.length
		case '+':
			current.Added = append(current.Added, after[current.AfterTo:current.AfterTo+op.length]...)
			current.AfterTo += op.length
		}
	}
	for i := range hunks {
		hunks[i].ID = i + 1
		hunks[i].From += offsetA
		hunks[i].To += offsetA
		hunks[i].AfterFrom += offsetB
		hunks[i].AfterTo += offsetB
	}
	return hunks
}

// bindReasons attaches each op's reason to the hunks its base range meets.
func bindReasons(hunks []DocHunk, ops []DocOp) {
	for i := range hunks {
		h := &hunks[i]
		seen := map[string]bool{}
		for _, op := range ops {
			if op.Reason == "" || seen[op.Reason] || !rangesMeet(h.From, h.To, op.baseFrom, op.baseTo) {
				continue
			}
			seen[op.Reason] = true
			h.Reasons = append(h.Reasons, op.Reason)
		}
	}
}

// rangesMeet reports whether two half-open base ranges overlap, treating an
// empty range as a point that meets any range containing or touching it.
func rangesMeet(aFrom, aTo, bFrom, bTo int) bool {
	if aFrom == aTo {
		return bFrom <= aFrom && aFrom <= bTo
	}
	if bFrom == bTo {
		return aFrom <= bFrom && bFrom <= aTo
	}
	return aFrom < bTo && bFrom < aTo
}

// DocConflict is a region where the owner's draft and the current document
// both changed the same base paragraphs. The merged document keeps the
// current text there; Ours is what the draft had.
type DocConflict struct {
	From, To int            `json:"-"`
	Position int            `json:"position"`
	Length   int            `json:"length"`
	Ours     []DocParagraph `json:"ours"`
	Theirs   []DocParagraph `json:"theirs"`
	Base     []DocParagraph `json:"base"`
}

type mergeSide struct {
	hunk DocHunk
	ours bool
}

// mergeDocuments rebases a draft: base is the revision the proposal was
// read from, ours the draft, theirs the document as it is now. Changes to
// distinct paragraphs combine; overlapping ones are conflicts resolved in
// favour of theirs and reported.
func mergeDocuments(base, ours, theirs []DocParagraph) ([]DocParagraph, []DocConflict) {
	var sides []mergeSide
	for _, h := range documentHunks(base, ours) {
		sides = append(sides, mergeSide{hunk: h, ours: true})
	}
	for _, h := range documentHunks(base, theirs) {
		sides = append(sides, mergeSide{hunk: h})
	}
	sort.SliceStable(sides, func(i, j int) bool {
		a, b := sides[i].hunk, sides[j].hunk
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	var merged []DocParagraph
	var conflicts []DocConflict
	pos := 0
	emit := func(paragraphs []DocParagraph) {
		for _, p := range paragraphs {
			p.N = len(merged) + 1
			merged = append(merged, p)
		}
	}
	for i := 0; i < len(sides); {
		// A cluster is a run of hunks whose base ranges conflict.
		j := i + 1
		from, to := sides[i].hunk.From, sides[i].hunk.To
		hasOurs, hasTheirs := sides[i].ours, !sides[i].ours
		for j < len(sides) && hunksConflict(from, to, sides[j].hunk.From, sides[j].hunk.To) {
			if sides[j].hunk.To > to {
				to = sides[j].hunk.To
			}
			hasOurs = hasOurs || sides[j].ours
			hasTheirs = hasTheirs || !sides[j].ours
			j++
		}
		emit(base[pos:from])
		if hasOurs && hasTheirs {
			c := DocConflict{From: from, To: to, Position: len(merged) + 1, Base: append([]DocParagraph{}, base[from:to]...)}
			c.Ours = applyHunks(base, from, to, sides[i:j], true)
			c.Theirs = applyHunks(base, from, to, sides[i:j], false)
			c.Length = len(c.Theirs)
			emit(c.Theirs)
			conflicts = append(conflicts, c)
		} else {
			emit(applyHunks(base, from, to, sides[i:j], hasOurs))
		}
		pos = to
		i = j
	}
	emit(base[pos:])
	if merged == nil {
		merged = []DocParagraph{}
	}
	return merged, conflicts
}

// hunksConflict: overlapping ranges conflict (an insertion point strictly
// inside a replaced range included), as do two insertions at one point;
// an insertion at the edge of another change keeps its side of it.
func hunksConflict(aFrom, aTo, bFrom, bTo int) bool {
	if aFrom < bTo && bFrom < aTo {
		return true
	}
	return aFrom == aTo && bFrom == bTo && aFrom == bFrom
}

// applyHunks renders base[from:to] with one side's hunks applied.
func applyHunks(base []DocParagraph, from, to int, sides []mergeSide, ours bool) []DocParagraph {
	out := []DocParagraph{}
	pos := from
	for _, side := range sides {
		if side.ours != ours {
			continue
		}
		out = append(out, base[pos:side.hunk.From]...)
		out = append(out, side.hunk.Added...)
		pos = side.hunk.To
	}
	out = append(out, base[pos:to]...)
	return out
}

// sameDocument reports whether two paragraph lists are the same content.
func sameDocument(a, b []DocParagraph) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].key() != b[i].key() {
			return false
		}
	}
	return true
}
