# Google Docs–style suggestion view, built on Panta docs

Follow-up to [doc-suggestions-plan.md](doc-suggestions-plan.md). Status: plan,
2026-09-17. The paragraph-list preview shipped with the first iteration is
functional but not a document; this replaces it.

## Objective

Review a proposal on a page that looks like the document: inserted text in
colour, deleted text struck through in place, formatting changes annotated,
one card per suggestion in the right margin with the agent's reason and
Accept / Reject, comments in the same margin anchored to text, and the
owner editing the page directly. The rendering, editing surface and margin
come from Panta's docs frontend (Tiptap editor, paper styling, comments
sidebar), extended with a suggestion layer that Panta's own document
proposals use too.

There is no second source of truth. The Google Doc and Warden's proposal /
draft (policy service, `document_proposals`) remain the only state. The
page is a projection Warden computes from them and accepts back; Panta
stores nothing about Google Docs.

## Decisions

1. **The page is a Tiptap document Warden computes.** `doc_preview` gains
   `view.document`: an ordinary Tiptap JSON doc in which changed text
   carries `suggestInsert{id}` / `suggestDelete{id}` marks and changed
   blocks carry a `suggestion` attribute (`insert | delete | restyle`, with
   the previous style). Deleted text is *in* the document, like Google
   Docs' own model, so accept / reject are document transforms and the
   margin can anchor to real ranges.
2. **Warden computes the diffs, including word level.** The paragraph LCS
   already lives in Go; the inline token diff moves there from `docmarks.ts`
   so the agent's `changes` and the page use one tokenizer. The client is a
   renderer.
3. **The owner edits the draft, not "suggests".** Typing on the page is a
   draft edit. Saving strips delete-marked text, drops insert marks,
   converts Tiptap → canonical paragraphs and calls `doc_draft {document}`;
   the server recomputes hunks and returns a fresh view. Accept = remove
   that suggestion's deletions and unmark its insertions; reject = the
   inverse; both are local transforms followed by a save. Hunks stay a
   view, never state.
4. **One suggestion layer, built in Panta's docs codebase, vendored into
   Warden.** Panta is where Tiptap, the paper styling, the comments
   extension and the tests live, and Panta's proposals need the same
   rendering (today a block-level before/after). Warden takes a copy under
   `chat/web/src/documents/` with a `PROVENANCE.md`, the way Warden's chat
   UI came from Panta and Panta's editor came from the prototype, so Warden
   still runs without Panta. A published package replaces the copy once
   Panta's docs module is split out.
5. **Comments anchor to text.** `{paragraph, from, to, quote, text}` in
   draft coordinates, rendered as highlights with margin cards; Panta's
   `commentExtension` and `DocumentComments` adapted without Yjs (single
   owner, no collaboration).
6. **Frozen content is shown, not edited.** Tables and images are projected
   into the view read-only (Tiptap has table and image nodes) as atoms the
   editor cannot change or delete; the compiler still never touches them.
   Everything the model cannot express stays a labelled placeholder.
7. **Panta as a review surface is a later step, as a client of Warden.**
   When Panta shows a Warden proposal it fetches `doc_preview` live and
   posts decisions to `doc_decide` / `doc_draft` / `doc_resolve`; it never
   copies the document.

## Design

### Warden policy service (Go)

- `docview.go`
  - `canonicalToDoc(paragraphs) → Tiptap JSON` (headings, paragraphs,
    nested `bulletList` / `orderedList` from `depth`, marks bold / italic /
    link, `frozenBlock` atoms; tables and images projected read-only).
  - `docToCanonical(Tiptap JSON) → paragraphs` (strict: only the supported
    node and mark set, frozen atoms must match the base in order, delete
    marks stripped, insert marks dropped) — the validator ported from
    Panta's `validateDocumentRich` rules.
  - `inlineDiff(before, after runs)` — the token diff from `docmarks.ts`.
  - `suggestionDocument(base, draft, hunks, rejected)` — the view: paired
    paragraphs get inline insert/delete runs, unpaired ones block-level
    marks, style changes the `restyle` attribute; rejected proposal hunks
    are listed for the margin but not drawn.
- `doc_preview`: `view.document`, `view.suggestions` (id, kind, reasons,
  summary "Replace … with …", acceptable), `view.comments`.
- `doc_draft`: accepts `{document}` (Tiptap JSON) or `{paragraphs}`;
  comments carry ranges.
- Tests: view → strip → canonical round-trips the draft; accept / reject
  transforms produce the same draft as `decide`; frozen atoms preserved.

### Suggestion layer (Panta repo, `web/src/documents/suggestions/`)

- `marks.ts` — `suggestInsert`, `suggestDelete` marks; `suggestionBlock`
  attribute on heading / paragraph / listItem; `frozenBlock` atom node.
- `plugin.ts` — ProseMirror plugin: delete-marked text is read-only,
  typed text never inherits suggestion marks, suggestion ranges → DOM
  rectangles for the margin, click on a range selects its card.
- `Margin.tsx` — cards beside their first range (Google Docs layout;
  Panta's comments sidebar positioning), reason, summary, Accept ✓ /
  Reject ✗, comment composer; comment cards from the same module.
- `SuggestionEditor.tsx` — the page (`rich-documents.css` paper), toolbar
  limited to the supported set, `onSave(document)` debounced,
  `accept(id)` / `reject(id)` transforms, comments API.
- Fixture page from a Warden `doc_preview` JSON; Playwright covers
  accept, reject, typing into and next to suggestions, comment anchoring,
  and that saving strips deletions.

### Warden web

- Vendored copy + Tiptap dependencies in a lazy chunk (first open loads
  it; ~200 KB gzipped).
- `DocumentReview.tsx`: the Suggestions and Draft tabs become the editor;
  Summary and the footer (Reject / Send back / Refresh / Approve) stay; the
  transcript card and workspace panel are unchanged. `docmarks.ts` goes.

### Panta

- Its own proposals render with the same layer: the backend gains the view
  builder for `richContent` proposals (port of `docview.go`) and the
  proposals API returns `view`.
- Optional: a "Warden suggestions" entry in the document panel that
  consumes Warden's review API for Google Docs proposals.

## Steps

0. Owner decision: layer in Panta + vendored into Warden (recommended,
   this plan) or Warden-only; whether step 4b is wanted.
1. Warden Go: `docview.go`, inline diff, `doc_preview.view`, `doc_draft
   {document}`, tests.
2. Panta: suggestion layer, editor, margin, fixture page, Playwright.
3. Warden web: vendor, replace the tabs, live test on the local install
   with the existing "Warden suggestions test" document (rounds: accept,
   reject, type inside a suggestion, comment, send back, approve, stale).
4. Panta: (a) its proposals on the same layer; (b) Warden proposals in
   Panta via Warden's API.

## Out of scope / later

Editing tables and images (the compiler is text-only); multiple tabs;
Yjs collaboration on the draft (single owner); pushing native Google Docs
suggestions (the API cannot create them).
