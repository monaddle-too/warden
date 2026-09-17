# Suggestion layer

A Google Docs–style review of proposed edits on top of the Tiptap editor:
inserted text in colour, deleted text struck through in place, a card per
suggestion in the right margin with Accept / Reject, comments anchored to
text, and the reviewer editing the page directly.

The layer renders a **suggestion document**: an ordinary Tiptap document
whose changed text carries `suggestInsert` / `suggestDelete` marks
(`attrs.id` = the suggestion, `attrs.author` = `agent` | `owner` | `both`)
and whose changed blocks carry a `suggestion` attribute (`{id, kind:
replace|insert|delete|restyle|accepted, author?, from?}`). One suggestion
is one edit as it was made — never a merge of neighbouring edits — and
the reviewer's own edits are drawn in a second colour with their own
cards (`author: owner`, an *Undo* instead of accept / reject). Deleted text stays in the document so it can be shown; whoever
consumes the edited page strips it. Two more block attributes exist for
what Tiptap nodes cannot express: `docStyle` (`title` | `subtitle`) on
paragraphs and `synthetic` on list wrapper paragraphs that stand for no
real paragraph. Content that cannot be edited is a `frozenBlock` or
`frozenTable` atom keyed by its order in the document.

Node set: `doc`, `paragraph`, `heading` (1–6), `bulletList`,
`orderedList`, `listItem`, `text`, `hardBreak`, `frozenBlock`,
`frozenTable`; marks `bold`, `italic`, `link`, `suggestInsert`,
`suggestDelete`. The editor registers exactly this set.

The page never decides anything itself: `onAccept` / `onReject` /
`onRestore` and `onSave` (the edited document, debounced) go to the caller,
which holds the draft and hands back a fresh document with a bumped
`revision`; the editor applies it as one transaction over the changed
range, so the caret and the rest of the page stay put. Comments are `{paragraph, from, to, quote, text}` in draft
coordinates (paragraph numbers exclude deleted blocks; offsets count code
points of surviving text), so they survive the page being replaced.

Warden produces and consumes this shape in `chat/internal/policy/docview.go`
and vendors this directory into `chat/web/src/documents/suggestions/`.
Keep the two copies identical; change this one first.
