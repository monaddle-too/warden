package policy

import "sort"

// Suggestions are the agent's operations, one each, the way Google Docs
// keeps every edit its own suggestion: a proposal's hunks come straight
// from its ops (never from a diff, which would merge neighbours), and the
// review splits the draft's changes back along those hunks, attributing
// what is left over to the owner.

// Authors of a change on the page.
const (
	authorAgent = "agent"
	authorOwner = "owner"
	authorBoth  = "both" // an agent suggestion the owner edited further
)

// ownerChangeBase keeps owner change ids apart from proposal hunk ids.
const ownerChangeBase = 10000

// hunksFromOps turns validated ops (sorted, non-overlapping, base
// positions) into one hunk each.
func hunksFromOps(base []DocParagraph, ops []DocOp) []DocHunk {
	hunks := make([]DocHunk, 0, len(ops))
	after := 0
	pos := 0
	for _, op := range ops {
		after += op.baseFrom - pos
		h := DocHunk{From: op.baseFrom, To: op.baseTo, AfterFrom: after, Removed: append([]DocParagraph{}, base[op.baseFrom:op.baseTo]...), Added: append([]DocParagraph{}, op.Paragraphs...), Reasons: []string{}}
		if op.Reason != "" {
			h.Reasons = append(h.Reasons, op.Reason)
		}
		after += len(op.Paragraphs)
		h.AfterTo = after
		pos = op.baseTo
		hunks = append(hunks, h)
	}
	return renumberHunks(hunks)
}

func renumberHunks(hunks []DocHunk) []DocHunk {
	sort.SliceStable(hunks, func(i, j int) bool { return hunks[i].From < hunks[j].From })
	after := 0
	pos := 0
	for i := range hunks {
		hunks[i].ID = i + 1
		after += hunks[i].From - pos
		hunks[i].AfterFrom = after
		after += len(hunks[i].Added)
		hunks[i].AfterTo = after
		pos = hunks[i].To
		if hunks[i].Reasons == nil {
			hunks[i].Reasons = []string{}
		}
	}
	return hunks
}

// hunksFromRevision expresses ops written against a returned draft as
// hunks against the base: the draft's kept changes stay as they were, a
// new op elsewhere maps to base positions, and an op that touches a kept
// change replaces it (the kept change's base range, the op's result).
func hunksFromRevision(base, draft []DocParagraph, kept []DocHunk, ops []DocOp) []DocHunk {
	type keptHunk struct {
		DocHunk
		gone bool
	}
	current := make([]keptHunk, len(kept))
	for i, h := range kept {
		current[i] = keptHunk{DocHunk: h}
	}
	var out []DocHunk
	for _, op := range ops {
		dFrom, dTo := op.baseFrom, op.baseTo // draft positions
		from, to := -1, -1
		reasons := []string{}
		var touched []int
		for i, k := range current {
			if k.gone {
				continue
			}
			if rangesMeet(dFrom, dTo, k.AfterFrom, k.AfterTo) {
				touched = append(touched, i)
			}
		}
		if len(touched) == 0 {
			delta := 0
			for _, k := range current {
				if k.AfterTo <= dFrom {
					delta += len(k.Added) - len(k.Removed)
				}
			}
			from, to = dFrom-delta, dTo-delta
		} else {
			// The union of the touched kept changes, in draft and base
			// positions; the op replaces its part of that draft stretch.
			aFrom, aTo := current[touched[0]].AfterFrom, current[touched[0]].AfterTo
			from, to = current[touched[0]].From, current[touched[0]].To
			for _, i := range touched {
				k := current[i]
				aFrom, aTo = min(aFrom, k.AfterFrom), max(aTo, k.AfterTo)
				from, to = min(from, k.From), max(to, k.To)
				reasons = append(reasons, k.Reasons...)
				current[i].gone = true
			}
			aFrom, aTo = min(aFrom, dFrom), max(aTo, dTo)
			// Base paragraphs the stretch covers beyond the kept changes.
			delta := 0
			for _, k := range current {
				if !k.gone && k.AfterTo <= aFrom {
					delta += len(k.Added) - len(k.Removed)
				}
			}
			from, to = min(from, aFrom-delta), max(to, aTo-delta)
			segment := append(append(append([]DocParagraph{}, draft[aFrom:dFrom]...), op.Paragraphs...), draft[dTo:aTo]...)
			if op.Reason != "" {
				reasons = append(reasons, op.Reason)
			}
			out = append(out, DocHunk{From: from, To: to, Removed: append([]DocParagraph{}, base[from:to]...), Added: segment, Reasons: reasons})
			continue
		}
		if op.Reason != "" {
			reasons = append(reasons, op.Reason)
		}
		out = append(out, DocHunk{From: from, To: to, Removed: append([]DocParagraph{}, base[from:to]...), Added: append([]DocParagraph{}, op.Paragraphs...), Reasons: reasons})
	}
	for _, k := range current {
		if !k.gone {
			out = append(out, k.DocHunk)
		}
	}
	return renumberHunks(out)
}

// applyHunksToBase materialises hunks against base (tests and revisions).
func applyHunksToBase(base []DocParagraph, hunks []DocHunk) []DocParagraph {
	out := []DocParagraph{}
	pos := 0
	for _, h := range hunks {
		out = append(out, base[pos:h.From]...)
		out = append(out, h.Added...)
		pos = h.To
	}
	out = append(out, base[pos:]...)
	for i := range out {
		out[i].N = i + 1
	}
	return out
}

// DocChange is one change of the draft against the base as the review
// shows it: an agent suggestion (the proposal's hunk id, its reasons), an
// owner edit, or an agent suggestion the owner edited.
type DocChange struct {
	DocHunk
	Author string `json:"author"`
}

// reviewChanges splits diff(base, draft) along the proposal's hunks. A
// current hunk that contains proposal hunks applied verbatim yields one
// change per proposal hunk plus owner changes for what lies between; one
// the owner edited inside stays whole, attributed to both.
func reviewChanges(proposal docProposal, draft docDraft) []DocChange {
	var out []DocChange
	owner := ownerChangeBase
	for _, c := range documentHunks(proposal.Base, draft.Paragraphs) {
		var meeting []DocHunk
		for _, h := range proposal.Hunks {
			if rangesMeet(c.From, c.To, h.From, h.To) {
				meeting = append(meeting, h)
			}
		}
		pieces, ok := splitChange(c, meeting)
		if !ok {
			change := DocChange{DocHunk: c, Author: authorOwner}
			if len(meeting) > 0 {
				change.ID, change.Author = meeting[0].ID, authorBoth
				for _, h := range meeting {
					change.Reasons = append(change.Reasons, h.Reasons...)
				}
			}
			pieces = []DocChange{change}
		}
		for _, piece := range pieces {
			if piece.Author == authorOwner {
				piece.ID = owner
				owner++
			}
			if piece.Reasons == nil {
				piece.Reasons = []string{}
			}
			out = append(out, piece)
		}
	}
	if out == nil {
		out = []DocChange{}
	}
	return out
}

// splitChange cuts one current hunk at the proposal hunks it contains. A
// suggestion found verbatim is its own change; one the owner edited runs
// up to the next verbatim suggestion (or the end) and is attributed to
// both; base paragraphs and draft paragraphs in between are the owner's.
func splitChange(c DocHunk, meeting []DocHunk) ([]DocChange, bool) {
	if len(meeting) == 0 {
		return []DocChange{{DocHunk: c, Author: authorOwner}}, true
	}
	var pieces []DocChange
	baseCursor, addedCursor := c.From, 0
	piece := func(author string, id int, baseTo, addedTo int, reasons []string) {
		if baseTo == baseCursor && addedTo == addedCursor {
			return
		}
		pieces = append(pieces, DocChange{Author: author, DocHunk: DocHunk{ID: id, From: baseCursor, To: baseTo, AfterFrom: c.AfterFrom + addedCursor, AfterTo: c.AfterFrom + addedTo,
			Removed: append([]DocParagraph{}, c.Removed[baseCursor-c.From:baseTo-c.From]...), Added: append([]DocParagraph{}, c.Added[addedCursor:addedTo]...), Reasons: append([]string{}, reasons...)}})
		baseCursor, addedCursor = baseTo, addedTo
	}
	found := func(h DocHunk) int {
		if at := indexOfParagraphs(c.Added[addedCursor:], h.Added); at >= 0 {
			return addedCursor + at
		}
		return -1
	}
	for i := 0; i < len(meeting); i++ {
		h := meeting[i]
		if h.From < baseCursor || h.To > c.To {
			return nil, false // partly restored: keep the change whole
		}
		if at := found(h); at >= 0 {
			piece(authorOwner, 0, h.From, at, nil)
			piece(authorAgent, h.ID, h.To, at+len(h.Added), h.Reasons)
			continue
		}
		// Edited by the owner: absorb everything up to the next verbatim
		// suggestion, including unfound siblings and their reasons.
		reasons := append([]string{}, h.Reasons...)
		baseTo, addedTo := c.To, len(c.Added)
		j := i + 1
		for ; j < len(meeting); j++ {
			if at := found(meeting[j]); at >= 0 {
				baseTo, addedTo = meeting[j].From, at
				break
			}
			reasons = append(reasons, meeting[j].Reasons...)
		}
		piece(authorBoth, h.ID, baseTo, addedTo, reasons)
		i = j - 1
	}
	piece(authorOwner, 0, c.To, len(c.Added), nil)
	return pieces, true
}

// indexOfParagraphs finds needle as a contiguous run in haystack.
func indexOfParagraphs(haystack, needle []DocParagraph) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j].key() != needle[j].key() {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
