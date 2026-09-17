# Sharing Google documents with sandboxed agents

In a Warden chat, ask the agent to request Google Docs access. The owner sees a popup with Google sign-in (when needed), a paginated document list, multi-selection and an access duration. Grants allow reads for 15 minutes, 1 hour, 24 hours or 7 days. The Shared documents control lists active grants and can revoke them immediately. A waiting-request shortcut navigates to the requesting chat.

Agent tools:
- `request_google_docs_access({reason})` waits for an owner decision. The returned result includes status, request ID, expiration, selected document IDs, titles, browser links and Google API URLs.
- `list_shared_documents({})` returns only this conversation's active grants and selected documents.

The agent makes an ordinary GET to the returned `https://docs.googleapis.com/v1/documents/{id}` URL, using the sandbox proxy environment. It does not supply a Google credential. Warden verifies the active sandbox/conversation lease, exact read-only operation and selected document, then injects the host-held credential. Expiry/revocation are checked at dispatch and response delivery. All four uppercase/lowercase HTTP proxy variables point to Warden. The sandbox trusts the gateway's public CA certificate; TLS verification remains enabled, and the CA signing key stays on the host.

Permission requests and grants are persisted in SQLite independently of the tool connection. While the run is alive, the saved decision is returned as its tool result. It is acknowledged only after a successful run completion. After interruption, Warden queues a deterministic notification containing the saved result and resumes the chat. Only Warden-generated notifications are eligible for retry; original user tasks are not replayed. Consumers should use `list_shared_documents` to recheck current access because grants can expire or be revoked between notification and retrieval.

## Local setup

Use `scripts/warden-chat start --google-config /private/path/google.json` alongside the normal runner/provider options. The private configuration contains `client_id`, `client_secret`, and `redirect_uri`. The callback is the local chat origin plus `/oauth/google_docs/callback`. Configure it in the Google OAuth web client and enable both Google Docs and Drive APIs.

New connections request `documents` and `drive.metadata.readonly` under `https://www.googleapis.com/auth/`. Legacy `documents.readonly` connections remain usable for reads; creation/editing prompts for expanded consent. The host file list contains only Google Docs. Tokens are persisted in the private host broker state, refreshed there, and never returned by owner APIs or agent tools. Google consent and reconnecting after token revocation/expiry remain necessary. Reconnecting invalidates existing Warden grants.

This implementation uses the existing single-owner local Warden installation. It has not been deployed to OVH and is not a multi-user OAuth account store.

## Document creation and spreadsheet editing

Agents can call `request_google_document_creation({title, reason})`. Warden shows the title and expiry. On approval the host creates one blank Google document and grants the requesting conversation read access to that ID; the agent fills it through suggestions (below), never directly. A creation timeout/crash is marked failed and never automatically retried: check Google for a possibly-created document before submitting another request.

`request_google_docs_access` accepts `access: "read" | "write" | "structure"` (read by default); each level includes the ones below, on the selected IDs only. The higher levels apply to spreadsheets: `write` covers Sheets cell values (update, append, clear) and `structure` also `spreadsheets:batchUpdate` (adding and deleting sheets, formats, charts, merges). Google Docs are read at every level — `documents.get` only; any Docs `batchUpdate` is refused with a pointer to `propose_google_document_edit`, so every change to a document is a suggestion the owner reviews (2026-09-17: the direct `write`/`structure` Docs levels were retired once suggestions had been used live). Creating other files, deleting documents and changing sharing permissions are denied at every level. Grants expire and can be revoked. Creating a document does not authorize another conversation to read it.

Inline images: the policy service can still publish an attached PNG (`attach_image`) at `<auth.publicURL>/published/<token>.png` for the duration of one Docs edit, but no grant reaches that path any more; it stays for the suggestion writer to use once suggestions can carry images. Any other image URL is refused, since it would let the agent make Google fetch an address of its choosing.

Owners can use **Shared documents → Share documents with this chat** before the first message to select documents, permission and expiry. This setup action does not automatically launch the agent. The host API is `POST /api/sharing/select` with `chatID`, `documents`, `access` and `duration`; it derives sandbox identity from the conversation. A second agent retrieves its explicitly shared plan with `list_shared_documents`.

Google creation/editing requires the `documents` OAuth scope in addition to `drive.metadata.readonly`; Google describes the former as access to see, edit, create and delete all Docs. Warden holds that credential privately and enforces the narrower operation/ID grants for agents. Existing read-only connections continue to work; write requests require reconnecting with Google's expanded consent. Existing grants are revoked on reconnect. Deployment remains local until the live workflow and production configuration are separately verified.

## Suggestions: reviewed edits without a write grant

The only way for an agent to change a shared document is to propose
suggestions, which needs `read` access:

- `read_google_document({document_id})` returns the document as numbered
  paragraphs `{n, style, depth, text, frozen?}`. Styles are `title`,
  `subtitle`, `h1`–`h6`, `text`, `bullet` and `numbered` (with `depth` for
  list nesting); text carries `**bold**`, `*italic*` and `[text](url)` marks
  with backslash escapes. Tables, images, footnotes, breaks and smart chips
  appear as frozen placeholders that cannot be changed or deleted, and only
  the first tab is shown.
- `propose_google_document_edit({document_id, summary, ops})` submits
  `replace`/`insert`/`delete` operations against those paragraph numbers,
  each with an optional `reason`, and waits for the owner. Warden reads the
  document itself, materialises the proposal and computes the diff; the
  agent's operations are never applied to Google directly.

The owner reviews the proposal in the chat's **Review suggestions…** card
(also listed under *Document suggestions* in the workspace panel): each
change is shown inline in the document with the agent's reason, and can be
rejected (or accepted again); the whole draft can be edited paragraph by
paragraph, and comments can be attached to paragraphs. **Approve** re-reads
the document, rebases the draft onto its current revision (non-overlapping
collaborator edits merge; overlapping ones mark the proposal *stale* and
show the conflict for another look), compiles `diff(current, draft)` into a
single `batchUpdate` with `requiredRevisionId`, and writes it with the
owner's Google credential. Unchanged paragraphs are never inside a request
range; edited paragraphs keep their paragraph object so alignment, spacing
and indentation survive. **Reject** returns feedback to the agent. **Send
back to agent** returns the edited draft and comments: the agent reads them
with `read_google_document({document_id, proposal_id})` and submits a new
proposal with `revises` set to the returned request ID, its operations then
addressing the returned draft's numbering.

Outcomes reach the agent as tool results, or as a durable notification after
an interruption, like document grants and pull request reviews. Google Docs
cannot receive native suggestions through its API, so the suggestion layer
exists only in Warden; the approved write appears in Google as one edit by
the owner. Direct `write`/`structure` grants keep working for the flows that
need them (created documents, tables, images).

## Large PR proposals

After implementing a document-derived change, agents can write the full `{repository, base, title, body, files, images?}` proposal to a JSON file under their workspace and call `request_pull_request({proposal_path: ".warden/proposal.json"})`. Warden reads that file once and creates the same immutable review snapshot as an inline proposal. The path cannot be combined with inline fields; symlinks, traversal, nonregular files and JSON over 2 MiB are rejected. The existing 20-file / 256 KiB content limit and editable owner-reviewed body still apply. Later changes to the workspace file do not change the pending review.
