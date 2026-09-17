# Provenance

`suggestions/` and `rich-documents.css` are copied from Panta
(`project-management-software`, `web/src/documents/`, commit 53e4782 on
branch `feat/doc-suggestions-layer`): the Google Docs–style suggestion layer
on the Tiptap editor and the paper styling it uses. Warden's policy service
produces and consumes the documents it renders (`chat/internal/policy/docview.go`).

Keep the copy identical to Panta's; change Panta first and re-copy. The
Tiptap dependencies are pinned to Panta's versions.
