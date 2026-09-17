package policy

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// DocumentProposals manages owner-reviewed Google document edits. A
// proposal is the agent's immutable submission (base, proposed paragraphs,
// reasons); the draft is the owner's review state and starts as the
// proposal. Warden writes diff(base, draft) to Google on approval.
type DocumentProposals struct {
	s *Sharing
}

type docRow struct {
	id, chat, sandbox, status, proposal, draft, outcome string
	delivered                                           int64
	created                                             float64
}

// docProposal is the stored submission.
type docProposal struct {
	DocumentID   string         `json:"document_id"`
	Title        string         `json:"title"`
	URL          string         `json:"url"`
	BaseRevision string         `json:"base_revision"`
	Base         []DocParagraph `json:"base"`
	Proposed     []DocParagraph `json:"proposed"`
	Hunks        []DocHunk      `json:"hunks"`
	Summary      string         `json:"summary"`
	Ops          any            `json:"ops"`
	Revises      string         `json:"revises,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
	// RebasedFrom is the revision the agent read when the base has since
	// been replaced by a newer revision during review.
	RebasedFrom string `json:"rebased_from,omitempty"`
}

// docComment is an owner note anchored to a draft paragraph, optionally
// to a range of its plain text (offsets in runes) with the quoted words.
type docComment struct {
	Paragraph int    `json:"paragraph"`
	From      int    `json:"from,omitempty"`
	To        int    `json:"to,omitempty"`
	Quote     string `json:"quote,omitempty"`
	Text      string `json:"text"`
}

type docDraft struct {
	Paragraphs []DocParagraph `json:"paragraphs"`
	Comments   []docComment   `json:"comments"`
}

// docAwaiting are the statuses in which the owner still holds the proposal.
var docAwaiting = map[string]bool{"pending": true, "applying": true, "stale": true}

func newDocumentProposals(s *Sharing) (*DocumentProposals, error) {
	if _, err := s.DB.Exec("CREATE TABLE IF NOT EXISTS document_proposals (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, status TEXT, proposal TEXT, draft TEXT, outcome TEXT, delivered INTEGER DEFAULT 0, principal TEXT NOT NULL DEFAULT 'owner', created REAL)"); err != nil {
		return nil, err
	}
	// A write interrupted mid-flight is uncertain: the owner re-reads the
	// document (rebase) before deciding again.
	interrupted := mustJSON(map[string]any{"error": "Writing was interrupted. Refresh the document from Google and check it before approving again."})
	if _, err := s.DB.Exec("UPDATE document_proposals SET status='pending', outcome=? WHERE status='applying'", string(interrupted)); err != nil {
		return nil, err
	}
	return &DocumentProposals{s: s}, nil
}

const docColumns = "id,chat,sandbox,status,proposal,draft,outcome,delivered,created"

func scanDoc(scanner interface{ Scan(...any) error }) (*docRow, error) {
	var r docRow
	if err := scanner.Scan(&r.id, &r.chat, &r.sandbox, &r.status, &r.proposal, &r.draft, &r.outcome, &r.delivered, &r.created); err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DocumentProposals) rowLocked(id string) (*docRow, error) {
	return scanDoc(d.s.DB.QueryRow("SELECT "+docColumns+" FROM document_proposals WHERE id=?", id))
}

func (r *docRow) parts() (docProposal, docDraft, map[string]any) {
	var proposal docProposal
	var draft docDraft
	outcome := map[string]any{}
	_ = json.Unmarshal([]byte(r.proposal), &proposal)
	_ = json.Unmarshal([]byte(r.draft), &draft)
	_ = json.Unmarshal([]byte(r.outcome), &outcome)
	if draft.Paragraphs == nil {
		draft.Paragraphs = []DocParagraph{}
	}
	if draft.Comments == nil {
		draft.Comments = []docComment{}
	}
	return proposal, draft, outcome
}

// result is the proposal as the chat, the agent and the console list see
// it; preview adds the full proposal, draft and computed review view.
func (d *DocumentProposals) result(r *docRow, preview bool) map[string]any {
	proposal, draft, outcome := r.parts()
	out := map[string]any{"request_id": r.id, "kind": "document_proposal", "chatID": r.chat, "sandboxID": r.sandbox, "status": r.status,
		"document_id": proposal.DocumentID, "title": proposal.Title, "url": proposal.URL, "summary": proposal.Summary, "created_at": r.created,
		"changes": len(proposal.Hunks)}
	for k, v := range outcome {
		out[k] = v
	}
	if preview {
		out["proposal"] = proposal
		out["draft"] = draft
		out["view"] = reviewView(proposal, draft)
	}
	return out
}

// reviewView is what the owner reviews: the current hunks of
// diff(base, draft) with the agent's reasons, the proposal's hunks the
// draft no longer contains (each marked whether it can still be accepted),
// and the page: a Tiptap document with the changes as suggestions plus one
// card per change.
func reviewView(proposal docProposal, draft docDraft) map[string]any {
	current := currentHunks(proposal, draft)
	// A suggestion counts as taken while the draft still changes the
	// paragraphs it touched, even if the owner edited it further; only
	// one whose paragraphs read as the document does is rejected.
	applied := map[int]bool{}
	for _, c := range current {
		for _, h := range proposal.Hunks {
			if rangesMeet(c.From, c.To, h.From, h.To) {
				applied[h.ID] = true
			}
		}
	}
	document, cards := suggestionDocument(proposal.Base, draft.Paragraphs, current)
	rejected := []map[string]any{}
	for _, h := range proposal.Hunks {
		if applied[h.ID] {
			continue
		}
		_, _, ok := mapBaseRange(current, h.From, h.To)
		rejected = append(rejected, map[string]any{"hunk": h, "acceptable": ok})
		var deleted, inserted []string
		for _, p := range h.Removed {
			deleted = append(deleted, plainInline(p.Text))
		}
		for _, p := range h.Added {
			inserted = append(inserted, plainInline(p.Text))
		}
		kind, summary := summarize(deleted, inserted, "")
		reasons := h.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		cards = append(cards, suggestionCard{ID: -h.ID, Kind: kind, Summary: summary, Reasons: reasons, Status: "rejected", Acceptable: ok, Hunk: h.ID})
	}
	return map[string]any{"hunks": current, "rejected": rejected, "document": document, "suggestions": cards, "comments": draft.Comments}
}

// currentHunks diffs base and draft and binds the proposal's reasons.
func currentHunks(proposal docProposal, draft docDraft) []DocHunk {
	current := documentHunks(proposal.Base, draft.Paragraphs)
	var ops []DocOp
	for _, h := range proposal.Hunks {
		for _, reason := range h.Reasons {
			ops = append(ops, DocOp{Reason: reason, baseFrom: h.From, baseTo: h.To})
		}
	}
	bindReasons(current, ops)
	return current
}

// mapBaseRange finds where base positions [from, to) sit in the draft,
// which exists only while no current hunk changed them.
func mapBaseRange(current []DocHunk, from, to int) (int, int, bool) {
	delta := 0
	for _, h := range current {
		if h.From < to && from < h.To {
			return 0, 0, false
		}
		if from == to && h.From < from && from < h.To {
			return 0, 0, false
		}
		if h.To <= from {
			delta += len(h.Added) - len(h.Removed)
		}
	}
	return from + delta, to + delta, true
}

func (d *DocumentProposals) undeliveredLocked() ([]any, error) {
	rows, err := d.s.DB.Query("SELECT " + docColumns + " FROM document_proposals WHERE status NOT IN ('pending','applying','stale') AND delivered=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		r, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d.result(r, false))
	}
	return out, rows.Err()
}

// readable confirms the workspace holds a read grant on the document and
// the owner has not tagged it unsharable, then fetches and projects it.
func (d *DocumentProposals) readable(sandbox, document string) (*DocProjection, map[string]any, error) {
	if !googleDocumentID.MatchString(document) {
		return nil, nil, valueErr("invalid document")
	}
	d.s.mu.Lock()
	grant, err := d.s.documentGrantLocked(sandbox, document, "read")
	if err == nil {
		var blocked map[string]bool
		if blocked, err = d.s.blockedLocked(); err == nil && blocked[document] {
			err = valueErr("document is tagged unsharable with AI")
		}
	}
	d.s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if d.s.Google == nil || !d.s.Google.Connected() {
		return nil, nil, valueErr("Google is not connected")
	}
	raw, err := d.s.Google.Document(document)
	if err != nil {
		return nil, nil, err
	}
	projection, err := ProjectDocument(raw)
	if err != nil {
		return nil, nil, valueErr("The document could not be read as paragraphs: " + err.Error())
	}
	if projection.DocumentID != document {
		return nil, nil, valueErr("Google returned a different document")
	}
	return projection, grant, nil
}

// documentGrantLocked finds an unexpired grant of at least access on the
// document for the sandbox.
func (s *Sharing) documentGrantLocked(sandbox, document, access string) (map[string]any, error) {
	rows, err := s.rowsLocked("SELECT "+sharingColumns+" FROM requests WHERE sandbox=? AND status='granted' AND expires>?", sandbox, s.Clock())
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if AccessRank(r.access) < AccessRank(access) {
			continue
		}
		var documents []map[string]any
		_ = json.Unmarshal([]byte(r.documents), &documents)
		for _, doc := range documents {
			if doc["id"] == document {
				return s.result(r), nil
			}
		}
	}
	return nil, valueErr("The document is not shared with this workspace; ask for read access with request_google_docs_access")
}

// Read serves read_google_document: the live document as numbered
// paragraphs, or the draft an owner returned with a proposal.
func (d *DocumentProposals) Read(data map[string]any) (map[string]any, error) {
	sandbox, document := stringField(data, "sandboxID"), stringField(data, "document_id")
	if proposalID := stringField(data, "proposal_id"); proposalID != "" {
		d.s.mu.Lock()
		r, err := d.rowLocked(proposalID)
		d.s.mu.Unlock()
		if err != nil || r.sandbox != sandbox {
			return nil, valueErr("unknown proposal")
		}
		if r.status != "returned" {
			return nil, valueErr("Only a returned proposal has a draft to read")
		}
		proposal, draft, _ := r.parts()
		if document != "" && document != proposal.DocumentID {
			return nil, valueErr("The proposal belongs to another document")
		}
		return map[string]any{"document_id": proposal.DocumentID, "title": proposal.Title, "url": proposal.URL, "proposal_id": r.id, "revision_id": proposal.BaseRevision,
			"paragraphs": draft.Paragraphs, "comments": draft.Comments, "notes": []string{"These are the owner's returned draft paragraphs; propose against them with revises set to this proposal_id."}}, nil
	}
	projection, _, err := d.readable(sandbox, document)
	if err != nil {
		return nil, err
	}
	return map[string]any{"document_id": projection.DocumentID, "title": projection.Title, "url": "https://docs.google.com/document/d/" + projection.DocumentID + "/edit",
		"revision_id": projection.RevisionID, "paragraphs": projection.Paragraphs, "notes": projection.Notes}, nil
}

// Submit validates a proposal, reads the base from Google and stores the
// immutable submission with the draft initialised to it.
func (d *DocumentProposals) Submit(data map[string]any) (map[string]any, error) {
	for _, key := range []string{"chatID", "sandboxID", "callID"} {
		if _, err := proposalText(data[key], 512, false); err != nil {
			return nil, err
		}
	}
	for key := range data {
		switch key {
		case "chatID", "sandboxID", "callID", "principal", "document_id", "summary", "ops", "revises":
		default:
			return nil, valueErr("unexpected proposal field " + strconv.Quote(key))
		}
	}
	chat, sandbox, callID := stringField(data, "chatID"), stringField(data, "sandboxID"), stringField(data, "callID")
	id := sha256Hex([]byte("doc\x00" + chat + "\x00" + sandbox + "\x00" + callID))
	d.s.mu.Lock()
	if old, err := d.rowLocked(id); err == nil {
		result := d.result(old, false)
		d.s.mu.Unlock()
		return result, nil
	}
	d.s.mu.Unlock()
	document := stringField(data, "document_id")
	summary, err := proposalText(data["summary"], 4000, false)
	if err != nil {
		return nil, valueErr("Provide a summary of the proposed changes (up to 4000 characters)")
	}
	projection, _, err := d.readable(sandbox, document)
	if err != nil {
		return nil, err
	}
	base := projection.Paragraphs
	start := base
	revises := ""
	var revisedHunks []DocHunk
	if v, present := data["revises"]; present {
		revises, _ = v.(string)
		if len(revises) != 64 {
			return nil, valueErr("revises must be the request_id of a returned proposal")
		}
		d.s.mu.Lock()
		old, err := d.rowLocked(revises)
		d.s.mu.Unlock()
		if err != nil || old.sandbox != sandbox {
			return nil, valueErr("unknown proposal to revise")
		}
		if old.status != "returned" {
			return nil, valueErr("Only a returned proposal can be revised")
		}
		oldProposal, oldDraft, _ := old.parts()
		if oldProposal.DocumentID != document {
			return nil, valueErr("The revised proposal belongs to another document")
		}
		if !sameDocument(oldProposal.Base, base) {
			return nil, valueErr("The document changed since the draft was returned; read it again and submit a fresh proposal")
		}
		start = oldDraft.Paragraphs
		revisedHunks = oldProposal.Hunks
	}
	ops, err := decodeDocOps(data["ops"], start)
	if err != nil {
		return nil, err
	}
	proposed := applyDocumentOps(start, ops)
	hunks := documentHunks(base, proposed)
	if len(hunks) == 0 {
		return nil, valueErr("The operations leave the document unchanged")
	}
	if revises == "" {
		bindReasons(hunks, ops)
	} else {
		// The returned proposal's reasons still explain the changes the
		// owner kept in the draft; the new ops were written against that
		// draft, so their reasons bind through the draft's own diff.
		var carried []DocOp
		kept := documentHunks(base, start)
		for _, h := range revisedHunks {
			for _, k := range kept {
				if k.From == h.From && k.To == h.To && sameDocument(k.Added, h.Added) {
					for _, reason := range h.Reasons {
						carried = append(carried, DocOp{Reason: reason, baseFrom: h.From, baseTo: h.To})
					}
				}
			}
		}
		bindReasons(hunks, carried)
		draftHunks := documentHunks(start, proposed)
		bindReasons(draftHunks, ops)
		for i := range hunks {
			for _, dh := range draftHunks {
				if rangesMeet(hunks[i].AfterFrom, hunks[i].AfterTo, dh.AfterFrom, dh.AfterTo) {
					hunks[i].Reasons = append(hunks[i].Reasons, dh.Reasons...)
				}
			}
		}
	}
	proposal := docProposal{DocumentID: document, Title: projection.Title, URL: "https://docs.google.com/document/d/" + document + "/edit", BaseRevision: projection.RevisionID,
		Base: base, Proposed: proposed, Hunks: hunks, Summary: summary, Ops: data["ops"], Revises: revises, Notes: projection.Notes}
	draft := docDraft{Paragraphs: proposed, Comments: []docComment{}}
	encoded := mustJSON(proposal)
	if len(encoded) > 4<<20 {
		return nil, valueErr("Rendered proposal exceeds the review limit")
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if _, err = d.s.DB.Exec("INSERT OR IGNORE INTO document_proposals (id,chat,sandbox,status,proposal,draft,outcome,delivered,principal,created) VALUES (?,?,?,?,?,?,?,0,?,?)", id, chat, sandbox, "pending", string(encoded), string(mustJSON(draft)), "{}", principalOf(data), d.s.Clock()); err != nil {
		return nil, err
	}
	r, err := d.rowLocked(id)
	if err != nil {
		return nil, err
	}
	return d.result(r, false), nil
}

// Dispatch handles the owner console and agent polling operations.
func (d *DocumentProposals) Dispatch(op string, data map[string]any) (map[string]any, error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if op == "doc_state" {
		rows, err := d.s.DB.Query("SELECT " + docColumns + " FROM document_proposals ORDER BY rowid")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		requests := []any{}
		for rows.Next() {
			r, err := scanDoc(rows)
			if err != nil {
				return nil, err
			}
			requests = append(requests, d.result(r, false))
		}
		return map[string]any{"requests": requests}, rows.Err()
	}
	r, err := d.rowLocked(stringField(data, "id"))
	if err != nil {
		return nil, errors.New("unknown document proposal")
	}
	switch op {
	case "doc_preview":
		return d.result(r, true), nil
	case "doc_get":
		if r.chat != stringField(data, "chatID") || r.sandbox != stringField(data, "sandboxID") {
			return nil, errors.New("proposal belongs to another conversation")
		}
		return d.result(r, false), nil
	case "doc_rebase":
		if !docAwaiting[r.status] || r.status == "applying" {
			return d.result(r, true), nil
		}
		d.s.mu.Unlock()
		err := d.rebase(r.id)
		d.s.mu.Lock()
		if err != nil {
			return nil, err
		}
		if r, err = d.rowLocked(r.id); err != nil {
			return nil, err
		}
		return d.result(r, true), nil
	}
	if !docAwaiting[r.status] || r.status == "applying" {
		return nil, errors.New("The proposal is no longer open for review")
	}
	proposal, draft, outcome := r.parts()
	switch op {
	case "doc_draft":
		// The page posts the edited Tiptap document; the older paragraph
		// list is still accepted.
		var paragraphs []DocParagraph
		if document, present := data["document"]; present {
			var frozen []DocParagraph
			for _, p := range proposal.Base {
				if p.Frozen != "" {
					frozen = append(frozen, p)
				}
			}
			paragraphs, err = docToCanonical(document, frozen)
		} else {
			paragraphs, err = decodeDraft(data["paragraphs"], proposal.Base)
		}
		if err != nil {
			return nil, err
		}
		draft.Paragraphs = paragraphs
		if raw, present := data["comments"]; present {
			if draft.Comments, err = decodeComments(raw, len(paragraphs)); err != nil {
				return nil, err
			}
		}
		for i := range draft.Comments {
			if draft.Comments[i].Paragraph > len(paragraphs) {
				draft.Comments[i].Paragraph = len(paragraphs)
			}
		}
	case "doc_decide":
		// hunk names one of the proposal's suggestions (accept restores a
		// rejected one); change names a current change of the draft, which
		// reject puts back to the base text.
		accept, isBool := data["accept"].(bool)
		if !isBool {
			return nil, errors.New("invalid decision")
		}
		if changeID, ok := asInt(data["change"]); ok {
			if accept {
				return nil, errors.New("a current change is already in the draft")
			}
			err = revert(proposal, &draft, int(changeID))
		} else if hunkID, ok := asInt(data["hunk"]); ok {
			err = decide(proposal, &draft, int(hunkID), accept)
		} else {
			return nil, errors.New("invalid decision")
		}
		if err != nil {
			return nil, err
		}
	case "doc_resolve", "doc_return":
		return d.resolveLocked(r, op, data, proposal, draft, outcome)
	default:
		return nil, errors.New("unknown document operation")
	}
	if _, err = d.s.DB.Exec("UPDATE document_proposals SET draft=? WHERE id=?", string(mustJSON(draft)), r.id); err != nil {
		return nil, err
	}
	if r, err = d.rowLocked(r.id); err != nil {
		return nil, err
	}
	return d.result(r, true), nil
}

func decodeComments(raw any, paragraphs int) ([]docComment, error) {
	items, ok := raw.([]any)
	if !ok || len(items) > 200 {
		return nil, valueErr("comments must be a list of up to 200 notes")
	}
	out := []docComment{}
	for _, item := range items {
		fields, _ := item.(map[string]any)
		n, ok := asInt(fields["paragraph"])
		text, err := proposalText(fields["text"], 4000, false)
		if !ok || err != nil || n < 0 || int(n) > paragraphs {
			return nil, valueErr("each comment needs a paragraph number and text")
		}
		c := docComment{Paragraph: int(n), Text: strings.TrimSpace(text)}
		from, _ := asInt(fields["from"])
		to, _ := asInt(fields["to"])
		if from < 0 || to < from || to > docParagraphLimit {
			return nil, valueErr("comment range is invalid")
		}
		c.From, c.To = int(from), int(to)
		if quote, present := fields["quote"]; present {
			if c.Quote, err = proposalText(quote, 500, true); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// revert puts the base paragraphs back behind one current change.
func revert(proposal docProposal, draft *docDraft, changeID int) error {
	for _, c := range currentHunks(proposal, *draft) {
		if c.ID == changeID {
			replaceDraft(draft, c.AfterFrom, c.AfterTo, proposal.Base[c.From:c.To])
			return nil
		}
	}
	return errors.New("unknown change")
}

// decide applies or reverts one of the proposal's hunks in the draft.
func decide(proposal docProposal, draft *docDraft, hunkID int, accept bool) error {
	var hunk *DocHunk
	for i := range proposal.Hunks {
		if proposal.Hunks[i].ID == hunkID {
			hunk = &proposal.Hunks[i]
		}
	}
	if hunk == nil {
		return errors.New("unknown suggestion")
	}
	current := documentHunks(proposal.Base, draft.Paragraphs)
	if accept {
		from, to, ok := mapBaseRange(current, hunk.From, hunk.To)
		if !ok {
			return valueErr("Those paragraphs were edited by hand; edit the draft instead")
		}
		replaceDraft(draft, from, to, hunk.Added)
		return nil
	}
	// Reject: restore the base paragraphs behind whichever current change
	// covers the suggestion (it may have grown by adjacent hand edits).
	for _, c := range current {
		if rangesMeet(c.From, c.To, hunk.From, hunk.To) {
			replaceDraft(draft, c.AfterFrom, c.AfterTo, proposal.Base[c.From:c.To])
			return nil
		}
	}
	return nil // already the base text
}

// replaceDraft swaps draft paragraphs [from, to) for replacement and keeps
// comment anchors on the paragraphs they were written against.
func replaceDraft(draft *docDraft, from, to int, replacement []DocParagraph) {
	out := make([]DocParagraph, 0, len(draft.Paragraphs)-(to-from)+len(replacement))
	out = append(out, draft.Paragraphs[:from]...)
	out = append(out, replacement...)
	out = append(out, draft.Paragraphs[to:]...)
	for i := range out {
		out[i].N = i + 1
		out[i].Start, out[i].End = 0, 0
	}
	shift := len(replacement) - (to - from)
	for i := range draft.Comments {
		switch n := draft.Comments[i].Paragraph; {
		case n > to:
			draft.Comments[i].Paragraph = n + shift
		case n > from+len(replacement):
			draft.Comments[i].Paragraph = from + len(replacement)
		}
	}
	draft.Paragraphs = out
}

// resolveLocked records the owner's decision: reject with feedback, return
// the draft to the agent, or approve and write.
func (d *DocumentProposals) resolveLocked(r *docRow, op string, data map[string]any, proposal docProposal, draft docDraft, outcome map[string]any) (map[string]any, error) {
	feedback := ""
	if v, present := data["feedback"]; present {
		var err error
		if feedback, err = proposalText(v, 4000, true); err != nil {
			return nil, err
		}
	}
	out := map[string]any{"feedback": feedback}
	status := "rejected"
	switch {
	case op == "doc_return":
		status = "returned"
		changes := documentHunks(proposal.Proposed, draft.Paragraphs)
		out["changes"] = changes
		out["comments"] = draft.Comments
		out["draft_paragraphs"] = len(draft.Paragraphs)
	case op == "doc_resolve":
		allow, isBool := data["allow"].(bool)
		if !isBool {
			return nil, errors.New("invalid review decision")
		}
		if allow {
			if d.s.Google == nil || !d.s.Google.CanWrite() {
				return nil, errors.New("Reconnect Google to allow writing documents")
			}
			if sameDocument(proposal.Base, draft.Paragraphs) {
				return nil, valueErr("The draft matches the document; reject the proposal or restore a suggestion first")
			}
			status = "applying"
			out = map[string]any{}
		}
	}
	if _, err := d.s.DB.Exec("UPDATE document_proposals SET status=?,outcome=? WHERE id=?", status, string(mustJSON(out)), r.id); err != nil {
		return nil, err
	}
	r, err := d.rowLocked(r.id)
	if err != nil {
		return nil, err
	}
	result := d.result(r, true)
	if status == "applying" {
		go d.Apply(r.id)
	}
	return result, nil
}

// rebase re-reads the document and merges the draft onto it, recording
// conflicts for the owner. Called with the store unlocked.
func (d *DocumentProposals) rebase(id string) error {
	d.s.mu.Lock()
	r, err := d.rowLocked(id)
	d.s.mu.Unlock()
	if err != nil {
		return err
	}
	proposal, draft, outcome := r.parts()
	fresh, _, err := d.readable(r.sandbox, proposal.DocumentID)
	if err != nil {
		return err
	}
	_, err = d.rebaseOnto(r, proposal, draft, outcome, fresh)
	return err
}

// rebaseOnto moves a proposal to a fresh revision. When the content is
// unchanged only the revision moves; otherwise the draft is merged and the
// proposal marked stale if the merge conflicted. Returns the merged draft.
func (d *DocumentProposals) rebaseOnto(r *docRow, proposal docProposal, draft docDraft, outcome map[string]any, fresh *DocProjection) (docDraft, error) {
	status := "pending"
	delete(outcome, "conflicts")
	delete(outcome, "error")
	if !sameDocument(proposal.Base, fresh.Paragraphs) {
		merged, conflicts := mergeDocuments(proposal.Base, draft.Paragraphs, fresh.Paragraphs)
		draft.Paragraphs = merged
		for i := range draft.Comments {
			if draft.Comments[i].Paragraph > len(merged) {
				draft.Comments[i].Paragraph = len(merged)
			}
		}
		if len(conflicts) > 0 {
			status = "stale"
			outcome["conflicts"] = conflicts
		}
		if proposal.RebasedFrom == "" {
			proposal.RebasedFrom = proposal.BaseRevision
		}
		proposal.Base = fresh.Paragraphs
	}
	proposal.BaseRevision = fresh.RevisionID
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if d.s.DB == nil {
		return draft, errors.New("sharing closed")
	}
	_, err := d.s.DB.Exec("UPDATE document_proposals SET status=?,proposal=?,draft=?,outcome=? WHERE id=? AND status IN ('pending','applying','stale')", status, string(mustJSON(proposal)), string(mustJSON(draft)), string(mustJSON(outcome)), r.id)
	return draft, err
}

// Apply writes the approved draft. The document is read again first: an
// unchanged document is written against its current revision, a changed
// one is rebased (and handed back to the owner on conflict). A write Google
// refuses because the revision moved in between is retried once from a
// fresh read; nothing else is retried, and never after an ambiguous answer.
func (d *DocumentProposals) Apply(id string) {
	d.s.mu.Lock()
	r, err := d.rowLocked(id)
	if err != nil || r.status != "applying" {
		d.s.mu.Unlock()
		return
	}
	d.s.mu.Unlock()
	proposal, draft, outcome := r.parts()
	status := "failed"
	out := map[string]any{}
	rebased := false
	err = func() error {
		for attempt := 0; ; attempt++ {
			fresh, _, err := d.readable(r.sandbox, proposal.DocumentID)
			if err != nil {
				return err
			}
			if !sameDocument(proposal.Base, fresh.Paragraphs) {
				merged, conflicts := mergeDocuments(proposal.Base, draft.Paragraphs, fresh.Paragraphs)
				if len(conflicts) > 0 {
					if _, err := d.rebaseOnto(r, proposal, draft, outcome, fresh); err != nil {
						return err
					}
					status = "stale"
					return valueErr("The document changed since the agent read it and some changes overlap; review the merged draft and approve again")
				}
				if proposal.RebasedFrom == "" {
					proposal.RebasedFrom = proposal.BaseRevision
				}
				proposal.Base, draft.Paragraphs, rebased = fresh.Paragraphs, merged, true
				for i := range draft.Comments {
					if draft.Comments[i].Paragraph > len(merged) {
						draft.Comments[i].Paragraph = len(merged)
					}
				}
			}
			proposal.BaseRevision = fresh.RevisionID
			body, _, err := CompileDocumentUpdate(fresh, draft.Paragraphs)
			if err != nil {
				return err
			}
			code, response, err := d.s.Google.BatchUpdate(proposal.DocumentID, mustJSON(body))
			if err != nil {
				return err
			}
			if code != 200 {
				message, _ := response["error"].(map[string]any)
				text := stringField(message, "message")
				if code == 400 && strings.Contains(strings.ToLower(text), "revision") {
					if attempt == 0 {
						continue
					}
					return valueErr("The document keeps changing while writing; refresh it from Google and approve again")
				}
				return errors.New("Google rejected the write (HTTP " + strconv.Itoa(code) + ")")
			}
			control, _ := response["writeControl"].(map[string]any)
			out["revision_id"] = stringField(control, "requiredRevisionId")
			out["url"] = proposal.URL
			out["written"] = len(body["requests"].([]map[string]any))
			if proposal.RebasedFrom != "" {
				out["rebased_from"] = proposal.RebasedFrom
			}
			status = "applied"
			return nil
		}
	}()
	if err != nil && status != "stale" {
		message := "Writing to Google failed. Check the document and Google before approving again."
		if isValueError(err) {
			message = err.Error()
		}
		out["error"] = message
		out["url"] = proposal.URL
		if strings.HasPrefix(message, "The document keeps changing") {
			status = "pending"
		}
	}
	if status == "stale" {
		return // rebaseOnto stored the merged draft and conflicts
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if d.s.DB != nil {
		if status == "applied" && rebased {
			// The review now reads against the revision that was written.
			_, _ = d.s.DB.Exec("UPDATE document_proposals SET status=?,outcome=?,proposal=?,draft=? WHERE id=?", status, string(mustJSON(out)), string(mustJSON(proposal)), string(mustJSON(draft)), id)
			return
		}
		_, _ = d.s.DB.Exec("UPDATE document_proposals SET status=?,outcome=? WHERE id=?", status, string(mustJSON(out)), id)
	}
}
