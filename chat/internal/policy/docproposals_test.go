package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func docSubmission(changes map[string]any) map[string]any {
	data := map[string]any{"chatID": "chat-a", "sandboxID": "sbx-a", "callID": "call-1", "document_id": "doc-a", "summary": "Tighten the intro",
		"ops": []any{
			map[string]any{"type": "replace", "start": 2, "end": 2, "paragraphs": []any{map[string]any{"style": "text", "text": "Say **hi** to the world"}}, "reason": "shorter"},
			map[string]any{"type": "insert", "after": 4, "paragraphs": []any{map[string]any{"style": "bullet", "depth": 0, "text": "three"}}, "reason": "complete the list"},
		}}
	for k, v := range changes {
		data[k] = v
	}
	return data
}

func docBase() []DocParagraph {
	return []DocParagraph{P("h1", "Plan"), P("text", "Say **hello** to the world"), L("bullet", 0, "one"), L("bullet", 0, "two"), P("text", "end")}
}

func (f *sharingFixture) waitStatus(id string, statuses ...string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := f.dispatch("doc_preview", map[string]any{"id": id})
		for _, s := range statuses {
			if r["status"] == s {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("proposal %s never reached %v", id, statuses)
	return nil
}

func hunkTexts(raw any) []string {
	var hunks []DocHunk
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &hunks)
	var out []string
	for _, h := range hunks {
		out = append(out, joinTexts(h.Removed)+"→"+joinTexts(h.Added)+" ["+strings.Join(h.Reasons, ";")+"]")
	}
	return out
}

func TestDocumentReadAndProposalNeedAReadGrant(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	if r := f.dispatch("doc_read", map[string]any{"sandboxID": "sbx-a", "document_id": "doc-a"}); r["status"] != "invalid" || !strings.Contains(r["error"].(string), "not shared") {
		t.Fatalf("read without grant: %v", r)
	}
	if r := f.dispatch("doc_submit", docSubmission(nil)); r["status"] != "invalid" {
		t.Fatalf("submit without grant: %v", r)
	}
	f.grant()
	r := f.dispatch("doc_read", map[string]any{"sandboxID": "sbx-a", "document_id": "doc-a"})
	paragraphs, _ := r["paragraphs"].([]DocParagraph)
	if r["revision_id"] != "r1" || len(paragraphs) != 5 || paragraphs[1].Text != "Say **hello** to the world" || paragraphs[2].Style != "bullet" {
		t.Fatalf("read: %v", r)
	}
	if r := f.dispatch("doc_read", map[string]any{"sandboxID": "sbx-b", "document_id": "doc-a"}); r["status"] != "invalid" {
		t.Fatalf("read from another workspace: %v", r)
	}
	// Tagging the document unsharable revokes its grants; the read stops.
	f.dispatch("block", map[string]any{"id": "doc-a", "name": "Plan"})
	if r := f.dispatch("doc_read", map[string]any{"sandboxID": "sbx-a", "document_id": "doc-a"}); r["status"] != "invalid" {
		t.Fatalf("blocked read: %v", r)
	}
}

func TestProposalIsReviewedAsSuggestionsAndWrittenOnApproval(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	f.grant()
	first := f.dispatch("doc_submit", docSubmission(nil))
	id, _ := first["request_id"].(string)
	if first["status"] != "pending" || first["kind"] != "document_proposal" || first["title"] != "Plan" || first["changes"] != 2 {
		t.Fatalf("submit: %v", first)
	}
	if again := f.dispatch("doc_submit", docSubmission(nil)); again["request_id"] != id {
		t.Fatal("same tool call must not create a second proposal")
	}
	// Nothing was written and the listing shows the pending review.
	if len(f.google.batchCalls) != 0 {
		t.Fatal("submission must not write")
	}
	state := f.dispatch("doc_state", nil)
	if requests := state["requests"].([]any); len(requests) != 1 {
		t.Fatalf("state: %v", state)
	}
	preview := f.dispatch("doc_preview", map[string]any{"id": id})
	view := preview["view"].(map[string]any)
	if got := hunkTexts(view["hunks"]); strings.Join(got, "|") != "Say **hello** to the world→Say **hi** to the world [shorter]|→three [complete the list]" {
		t.Fatalf("hunks: %v", got)
	}
	if rejected := view["rejected"].([]map[string]any); len(rejected) != 0 {
		t.Fatalf("rejected: %v", rejected)
	}
	// Reject the first suggestion: the draft restores the base text and
	// the suggestion moves to the rejected list, still acceptable.
	after := f.dispatch("doc_decide", map[string]any{"id": id, "hunk": 1, "accept": false})
	view = after["view"].(map[string]any)
	if got := hunkTexts(view["hunks"]); strings.Join(got, "|") != "→three [complete the list]" {
		t.Fatalf("after reject: %v", got)
	}
	if rejected := view["rejected"].([]map[string]any); len(rejected) != 1 || rejected[0]["acceptable"] != true {
		t.Fatalf("rejected: %v", rejected)
	}
	after = f.dispatch("doc_decide", map[string]any{"id": id, "hunk": 1, "accept": true})
	if got := hunkTexts(after["view"].(map[string]any)["hunks"]); len(got) != 2 {
		t.Fatalf("after accept: %v", got)
	}
	// A hand edit of the draft, with a comment.
	draft := after["draft"].(docDraft)
	var paragraphs []any
	for _, p := range draft.Paragraphs {
		fields := map[string]any{"style": p.Style, "depth": p.Depth, "text": p.Text}
		if p.N == 2 {
			fields["text"] = "Say **hi** to the *whole* world"
		}
		paragraphs = append(paragraphs, fields)
	}
	edited := f.dispatch("doc_draft", map[string]any{"id": id, "paragraphs": paragraphs, "comments": []any{map[string]any{"paragraph": 2, "text": "Keep it warm"}}})
	view = edited["view"].(map[string]any)
	if got := hunkTexts(view["hunks"]); got[0] != "Say **hello** to the world→Say **hi** to the *whole* world [shorter]" {
		t.Fatalf("after edit: %v", got)
	}
	if rejected := view["rejected"].([]map[string]any); len(rejected) != 0 {
		t.Fatalf("a hand-edited suggestion is still taken, not rejected: %v", rejected)
	}
	// Approval writes exactly diff(base, draft) against the read revision.
	if r := f.dispatch("doc_resolve", map[string]any{"id": id, "allow": true, "actor": "owner"}); r["status"] != "applying" {
		t.Fatalf("approve: %v", r)
	}
	done := f.waitStatus(id, "applied", "failed", "stale", "pending")
	if done["status"] != "applied" || done["revision_id"] != "rev-after" || len(f.google.batchCalls) != 1 {
		t.Fatalf("apply: %v (%d writes)", done, len(f.google.batchCalls))
	}
	var body map[string]any
	_ = json.Unmarshal(f.google.batchCalls[0], &body)
	if body["writeControl"].(map[string]any)["requiredRevisionId"] != "r1" {
		t.Fatalf("write control: %v", body["writeControl"])
	}
	// Replayed on the base, the write yields the draft.
	sim := newSimDoc(t, docBase())
	sim.apply(t, decodeRequests(t, body))
	if got := joinTexts(sim.project()); got != "Plan|Say **hi** to the *whole* world|one|two|three|end" {
		t.Fatalf("written document: %s", got)
	}
	// The agent learns the outcome once, then it is acknowledged.
	outcomes := func() []map[string]any {
		var out []map[string]any
		for _, item := range f.dispatch("undelivered", nil)["requests"].([]any) {
			if r := item.(map[string]any); r["kind"] == "document_proposal" {
				out = append(out, r)
			}
		}
		return out
	}
	if undelivered := outcomes(); len(undelivered) != 1 || undelivered[0]["status"] != "applied" {
		t.Fatalf("undelivered: %v", undelivered)
	}
	f.dispatch("ack", map[string]any{"id": id})
	if len(outcomes()) != 0 {
		t.Fatal("acknowledged outcome delivered twice")
	}
	if _, err := f.s.Dispatch("doc_decide", map[string]any{"id": id, "hunk": 1, "accept": false}); err == nil {
		t.Fatal("an applied proposal must not be editable")
	}
}

// decodeRequests converts a JSON-decoded batchUpdate body back to the
// typed shape the simulator reads.
func decodeRequests(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	var requests []map[string]any
	for _, raw := range body["requests"].([]any) {
		request := raw.(map[string]any)
		for kind, fields := range request {
			request[kind] = intify(fields)
		}
		requests = append(requests, request)
	}
	return map[string]any{"requests": requests}
}

func intify(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			v[k] = intify(item)
		}
		return v
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
	}
	return value
}

func TestRejectionAndReturnCarryFeedbackToTheAgent(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	f.grant()
	id := f.dispatch("doc_submit", docSubmission(nil))["request_id"].(string)
	r := f.dispatch("doc_resolve", map[string]any{"id": id, "allow": false, "feedback": "Keep hello"})
	if r["status"] != "rejected" || r["feedback"] != "Keep hello" || len(f.google.batchCalls) != 0 {
		t.Fatalf("reject: %v", r)
	}
	// A second proposal is returned with the owner's edits and comments.
	id2 := f.dispatch("doc_submit", docSubmission(map[string]any{"callID": "call-2"}))["request_id"].(string)
	f.dispatch("doc_decide", map[string]any{"id": id2, "hunk": 2, "accept": false})
	returned := f.dispatch("doc_return", map[string]any{"id": id2, "feedback": "Drop the list item, otherwise fine"})
	if count, _ := asInt(returned["draft_paragraphs"]); returned["status"] != "returned" || count != 5 {
		t.Fatalf("return: %v", returned)
	}
	if got := hunkTexts(returned["changes"]); strings.Join(got, "|") != "three→ []" {
		t.Fatalf("changes relative to the proposal: %v", got)
	}
	// The agent reads the returned draft and revises against it.
	draft := f.dispatch("doc_read", map[string]any{"sandboxID": "sbx-a", "document_id": "doc-a", "proposal_id": id2})
	if paragraphs := draft["paragraphs"].([]DocParagraph); len(paragraphs) != 5 || paragraphs[1].Text != "Say **hi** to the world" {
		t.Fatalf("returned draft: %v", draft)
	}
	if r := f.dispatch("doc_submit", docSubmission(map[string]any{"callID": "call-3", "revises": id})); r["status"] != "invalid" || !strings.Contains(r["error"].(string), "returned") {
		t.Fatalf("revising a rejected proposal: %v", r)
	}
	revised := f.dispatch("doc_submit", map[string]any{"chatID": "chat-a", "sandboxID": "sbx-a", "callID": "call-3", "document_id": "doc-a", "summary": "Second round", "revises": id2,
		"ops": []any{map[string]any{"type": "replace", "start": 5, "end": 5, "paragraphs": []any{map[string]any{"style": "text", "text": "the end"}}, "reason": "closing"}}})
	if revised["status"] != "pending" {
		t.Fatalf("revised: %v", revised)
	}
	view := f.dispatch("doc_preview", map[string]any{"id": revised["request_id"]})["view"].(map[string]any)
	if got := hunkTexts(view["hunks"]); strings.Join(got, "|") != "Say **hello** to the world→Say **hi** to the world [shorter]|end→the end [closing]" {
		t.Fatalf("revised hunks against the base: %v", got)
	}
}

func TestStaleDocumentIsRebasedOrHandedBack(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	f.grant()
	id := f.dispatch("doc_submit", docSubmission(nil))["request_id"].(string)
	// A collaborator edited an untouched paragraph: the write is rebased
	// onto the new revision and includes only the draft's changes.
	moved := docBase()
	moved[4] = P("text", "END")
	f.google.document = docFixture("r2", moved...)
	f.dispatch("doc_resolve", map[string]any{"id": id, "allow": true})
	done := f.waitStatus(id, "applied", "failed", "stale", "pending")
	if done["status"] != "applied" || len(f.google.batchCalls) != 1 {
		t.Fatalf("rebased apply: %v", done)
	}
	var body map[string]any
	_ = json.Unmarshal(f.google.batchCalls[0], &body)
	if body["writeControl"].(map[string]any)["requiredRevisionId"] != "r2" {
		t.Fatalf("write must target the current revision: %v", body["writeControl"])
	}
	sim := newSimDoc(t, moved)
	sim.apply(t, decodeRequests(t, body))
	if got := joinTexts(sim.project()); got != "Plan|Say **hi** to the world|one|two|three|END" {
		t.Fatalf("written document: %s", got)
	}
	// A collaborator edited the same paragraph: the proposal comes back
	// stale with the conflict, keeps the collaborator's text in the draft,
	// and can be approved again after review.
	f.google.document = docFixture("r1", docBase()...)
	id2 := f.dispatch("doc_submit", docSubmission(map[string]any{"callID": "call-2"}))["request_id"].(string)
	conflicting := docBase()
	conflicting[1] = P("text", "Say **hello** to everyone")
	f.google.document = docFixture("r3", conflicting...)
	f.dispatch("doc_resolve", map[string]any{"id": id2, "allow": true})
	stale := f.waitStatus(id2, "applied", "failed", "stale", "pending")
	var conflicts []DocConflict
	raw, _ := json.Marshal(stale["conflicts"])
	_ = json.Unmarshal(raw, &conflicts)
	if stale["status"] != "stale" || len(conflicts) != 1 || joinTexts(conflicts[0].Ours) != "Say **hi** to the world" || joinTexts(conflicts[0].Theirs) != "Say **hello** to everyone" || conflicts[0].Position != 2 || len(f.google.batchCalls) != 1 {
		t.Fatalf("stale: %v", stale)
	}
	draft := stale["draft"].(docDraft)
	if joinTexts(draft.Paragraphs) != "Plan|Say **hello** to everyone|one|two|three|end" {
		t.Fatalf("merged draft: %s", joinTexts(draft.Paragraphs))
	}
	proposal := stale["proposal"].(docProposal)
	if proposal.BaseRevision != "r3" || proposal.RebasedFrom != "r1" {
		t.Fatalf("rebased proposal: %+v", proposal)
	}
	f.dispatch("doc_resolve", map[string]any{"id": id2, "allow": true})
	if done := f.waitStatus(id2, "applied", "failed", "stale", "pending"); done["status"] != "applied" || len(f.google.batchCalls) != 2 || done["rebased_from"] != "r1" {
		t.Fatalf("second approval: %v", done)
	}
	// A clean rebase leaves the review reading against what was written.
	rebasedPreview := f.dispatch("doc_preview", map[string]any{"id": id})
	if p := rebasedPreview["proposal"].(docProposal); p.BaseRevision != "r2" || p.RebasedFrom != "r1" || joinTexts(p.Base) != "Plan|Say **hello** to the world|one|two|END" || rebasedPreview["rebased_from"] != "r1" {
		t.Fatalf("rebased record: %+v %v", p, rebasedPreview["rebased_from"])
	}
	// Google refusing the revision is retried once from a fresh read; a
	// second refusal hands the proposal back as pending.
	f.google.document = docFixture("r1", docBase()...)
	id3 := f.dispatch("doc_submit", docSubmission(map[string]any{"callID": "call-3"}))["request_id"].(string)
	f.google.batchStatus, f.google.batchResponse = 400, map[string]any{"error": map[string]any{"message": "The document revision has changed"}}
	f.dispatch("doc_resolve", map[string]any{"id": id3, "allow": true})
	if back := f.waitStatus(id3, "applied", "failed", "stale", "pending"); back["status"] != "pending" || !strings.Contains(back["error"].(string), "keeps changing") || len(f.google.batchCalls) != 4 {
		t.Fatalf("revision refusal: %v (%d writes)", back, len(f.google.batchCalls))
	}
	f.google.batchStatus, f.google.batchResponse = 500, map[string]any{}
	f.dispatch("doc_resolve", map[string]any{"id": id3, "allow": true})
	if failed := f.waitStatus(id3, "applied", "failed", "stale"); failed["status"] != "failed" || !strings.Contains(failed["error"].(string), "Writing to Google failed") {
		t.Fatalf("server failure: %v", failed)
	}
}

func TestInterruptedWriteReopensForReview(t *testing.T) {
	f := newSharingFixture(t)
	f.google.document = docFixture("r1", docBase()...)
	f.grant()
	id := f.dispatch("doc_submit", docSubmission(nil))["request_id"].(string)
	f.s.mu.Lock()
	if _, err := f.s.DB.Exec("UPDATE document_proposals SET status='applying' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	f.s.mu.Unlock()
	f.s.Close()
	f.s = f.open()
	r := f.dispatch("doc_preview", map[string]any{"id": id})
	if r["status"] != "pending" || !strings.Contains(r["error"].(string), "interrupted") {
		t.Fatalf("restart: %v", r)
	}
	if _, err := f.s.Dispatch("doc_resolve", map[string]any{"id": id, "allow": true}); err != nil {
		t.Fatal(err)
	}
	if done := f.waitStatus(id, "applied", "failed", "stale", "pending"); done["status"] != "applied" {
		t.Fatalf("after restart: %v", done)
	}
}
