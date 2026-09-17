# Google Docs suggestions: reviewed agent edits

Branch `feat/doc-suggestions` (worktree `.local/warden-doc-suggestions`), from
`origin/main` 174078d, 2026-09-17.

## Objective

Agents stop editing shared Google Docs directly. They read a document as
numbered paragraphs, propose paragraph-level edits with explanations, and the
owner reviews the proposal in Warden as suggestions — accept or reject each
change, edit the draft freely, add comments — before Warden writes the
approved draft to Google with the owner's credential. The owner can also send
the edited draft and comments back to the agent for another round. Only a
read grant is needed for the whole flow.

The design was worked out in a chat on 2026-09-17 (session "Google Docs agent
suggestions workflow") and never landed anywhere; this plan is its durable
form. Panta already has the same model for its own documents (whole-document
proposals, a Suggestions tab, accept with revision check, "Ask agent"
round-trip); this is the Google Docs version, owned by Warden.

## Decisions

1. **Warden computes the diff, never the agent.** The agent submits
   operations against paragraph numbers from its read (`replace`, `insert`,
   `delete`); Warden materialises the proposed document and diffs it against
   the base itself. An agent-authored `batchUpdate` (UTF‑16 offsets, order
   dependent, unreadable) is the wrong delta format.
2. **Canonical paragraph model, text-only in v1.** A paragraph is
   `{style, depth, text}` with styles `title|subtitle|h1…h6|text|bullet|
   numbered`, list `depth` 0–8 and Markdown-ish inline marks (`**bold**`,
   `*italic*`, `[text](url)`, backslash escapes). Tables, images, footnotes,
   equations, page breaks, section breaks and anything else the model cannot
   express are **frozen**: shown as opaque placeholders, never inside a hunk,
   never touched by the compiled write. Only the first tab is supported.
3. **The proposal is immutable; the draft is the review's state.** Like the
   PR review: base paragraphs + proposed paragraphs + agent explanations are
   stored once. The owner's draft starts as the proposal; accept/reject per
   hunk and free edits all mutate the draft; what gets written is
   `diff(base, draft)`, recomputed on approval. Hunks are a view, not state.
4. **Warden performs the write.** On approval Warden re-reads the document,
   checks the revision, compiles `diff(base, draft)` into one `batchUpdate`
   with `writeControl.requiredRevisionId`, and applies it with the owner's
   credential. Unchanged paragraphs are never inside any request range, so
   their formatting survives. Changed paragraphs keep their paragraph object
   (text replaced in place) so alignment, spacing and indentation survive
   too; named style, bullets and inline marks are asserted explicitly.
5. **Stale base → rebase, not silent OT.** If the document moved since the
   read, Warden three-way merges base/draft/current at paragraph level;
   clean merges proceed on the new revision, conflicts return the proposal
   to the owner marked stale with the conflicting paragraphs listed.
   `targetRevisionId` (Google's server-side transform) is not used: the
   merge is opaque and conflicts are resolved silently.
6. **Round-trip to the agent is `diff(proposal, draft)` plus comments.** The
   agent learns what the owner changed *about its proposal*, anchored to
   paragraph numbers of the returned draft, and may submit a new proposal
   that `revises` the returned one (its operations then apply to the
   returned draft, not a fresh read).
7. **Review UI is a Warden component**, modelled on `PullRequestReview.tsx`,
   with plain HTML rendering — no Tiptap, no Panta dependency ("Warden must
   operate without Panta"). Panta can mount it later.
8. **Direct write grants stay for now.** `write`/`structure`/`create` levels
   keep working (the Canton create-and-fill flow depends on `create`), but
   the tool descriptions steer agents to the proposal flow, which needs only
   `read`. Retiring `write` is an owner decision to take once proposals have
   been used live.
9. **Google cannot receive native suggestions.** The Docs API always writes
   directly, and the Drive comments API cannot anchor comments in Docs; the
   suggestion layer lives only in Warden. Reads use
   `suggestionsViewMode=PREVIEW_WITHOUT_SUGGESTIONS` so pending native
   suggestions by collaborators do not shift ranges; the compiled write
   targets the same view.

## Design

### Agent tools (chat service → policy service `doc_*`)

- `read_google_document({document_id})` → `{document_id, title, url,
  revision_id, paragraphs: [{n, style, depth?, text, frozen?}], notes}`.
  Requires an active read grant on the document for this workspace.
- `propose_google_document_edit({document_id, summary, ops, revises?})` →
  waits for the owner like `request_pull_request`; resolves to
  `{status: applied|rejected|returned|failed, feedback?, changes?, comments?,
  draft?}`. `ops` are `{type: replace, start, end, paragraphs, reason?}`,
  `{type: insert, after, paragraphs, reason?}`, `{type: delete, start, end,
  reason?}` against paragraph numbers of the read (or of the returned draft
  when `revises` names a returned proposal). Limits: 200 ops, 256 KiB of
  text, no frozen paragraph inside a range.

### Policy service (`chat/internal/policy`)

- `docmodel.go` — Docs API JSON → canonical paragraphs (`ProjectDocument`),
  inline mark rendering and parsing, UTF‑16 lengths, paragraph validation,
  operation application (`applyDocumentOps`).
- `docdiff.go` — paragraph LCS diff into hunks (`documentHunks`), pairing of
  removed/added paragraphs into in-place modifications, reason binding by
  base-range overlap, three-way merge (`mergeDocuments`).
- `doccompile.go` — hunks → `batchUpdate` requests, emitted in descending
  index order so earlier offsets stay valid; `createParagraphBullets` last
  within each paragraph because it removes the leading tabs that encode
  nesting.
- `docproposals.go` — the `document_proposals` table and operations:
  `doc_read`, `doc_submit`, `doc_get` (agent); `doc_state`, `doc_preview`,
  `doc_draft`, `doc_resolve`, `doc_return`, `doc_rebase` (owner UI);
  `Apply` (the write). Undelivered outcomes are delivered to the chat by
  the existing `undelivered`/`ack` path with `kind: document_proposal`.
- `GoogleSharing` gains `Document(id)` and `BatchUpdate(id, body)`; the
  fake in tests implements them.

### Web (`chat/web/src/components/DocumentReview.tsx`)

Dialog with three tabs: **Suggestions** (each hunk: before/after with inline
word diff, the agent's reason, Accept / Reject buttons, per-hunk comment),
**Draft** (the full draft as editable paragraphs), **Summary** (agent
summary, status, outcome). Footer: feedback textarea, *Reject*, *Send back
to agent*, *Approve and write to Google Doc*. Draft edits save through
`doc_draft`; the transcript card and the workspace panel open the dialog as
they do for pull requests.

## Steps

- [x] Worktree and plan.
- [x] `docmodel.go` + tests: projection, marks, validation, ops.
- [x] `docdiff.go` + tests: hunks, frozen anchors, reasons, three-way merge.
- [x] `doccompile.go` + tests: every compile case is replayed through a
      Docs-API simulator (UTF‑16 indexes, newline-carried paragraph
      properties, tab-encoded nesting) and must reproduce the draft.
- [x] `docproposals.go` + tests: submit/preview/decide/draft/resolve/return/
      rebase, apply against the fake Google, clean and conflicting rebases,
      revision refusal, restart during a write, delivery and ack.
- [x] Chat service: tools, dispatch, notification wording, HTTP allowlist.
- [x] Web: `DocumentReview.tsx` (+ `docmarks.ts`), CSS, ChatShell card and
      workspace panel section; checked in the browser against a stub API.
- [x] Docs: feature map row and glossary, `warden-document-sharing.md`.
- [x] Go suite, web build and tests green.
- [x] Live verification against a real Google Doc (2026-09-17, below).

## Progress

2026-09-17: implemented end to end on the branch; all unit and simulated
tests pass. The compiler's end-of-document rule: a deletion that reaches the
last paragraph removes the preceding paragraph's newline instead (the final
newline cannot be deleted) and that paragraph is then asserted in full,
since Docs may give a merged paragraph the last paragraph's properties.
Direct write grants were kept (decision 8); tool descriptions steer agents
to the proposal flow.

Live verification 2026-09-17 on the local Warden (build 8b3bf65 →
ad49511, Claude chats, document "Warden suggestions test" in the owner's
Drive, created by the agent through `request_google_document_creation`):

- `read_google_document` projects headings, bold/link runs, nested bullets
  and numbered items exactly.
- Round 1: four ops → review dialog; rejected one suggestion, hand-edited a
  paragraph, commented, *Send back* → the agent received `changes` (diff
  against its proposal), `comments` and feedback. Round 2 with `revises`
  carried the kept reasons, was approved and written in 19 edits.
- Raw document check: an item inserted by the compiler joined the existing
  numbered list (`createParagraphBullets` next to a list of the same preset
  merges into it — numbering stays continuous), the Risks items got their
  own list, bold and link survived.
- Clean rebase: proposal pending, collaborator (second chat, direct
  `batchUpdate`) changed another paragraph, approve → merged and written
  against the new revision, `rebased_from` reported; the agent whose run
  had been stopped got the outcome as the durable notification.
- Conflicting rebase: both sides changed paragraph 1 → `stale`, the conflict
  shown in Summary, the suggestion listed as rejected-but-acceptable;
  accepting it and approving wrote it.
- Bug found and fixed live: an empty current diff serialised as
  `hunks: null` and crashed the dialog (now `[]`, guarded in the UI).

Observed, not ours: the agent's own round-1 `batchUpdate` shifted indexes
after its first `createParagraphBullets` and left a HEADING_2 on a list
item, which the projection hides (a list item is shown as `bullet`/
`numbered` whatever its named style) — consider surfacing the named style
of list items. Chats sharing a workspace run one at a time; a queued chat
can wait for a wake after its sibling's stop (task spawned).

Known limits: inserting between two frozen blocks is refused; a document
whose first element is a table cannot take insertions at the top; only the
first tab.

## Remaining work / follow-ups

- The paragraph-list preview is being replaced by a Google Docs–style page
  built on Panta's docs editor: [doc-suggestions-view-plan](doc-suggestions-view-plan.md).

- Decide whether to retire the direct `write` grant level (decision 8).
- Show the underlying named style of list items in the projection.
- Tables and inline images as editable content (v2), multiple tabs,
  headers/footers.
- Panta: mount the same review component in its document panel, or adopt the
  same proposal shape for its own documents.
