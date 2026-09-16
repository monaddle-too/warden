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

## Document creation and editing

Agents can call `request_google_document_creation({title, reason})`. Warden shows the title, proposed write access and expiry. On approval the host creates one blank Google document and grants the requesting conversation read/write access to that ID. The agent receives its browser/API URLs and fills it using `POST /v1/documents/{id}:batchUpdate` through Warden's credential-injecting proxy. A creation timeout/crash is marked failed and never automatically retried: check Google for a possibly-created document before submitting another request.

`request_google_docs_access` accepts `access: "read" | "write"` (read by default). Writes are limited to text insertion/deletion and text/paragraph styling on the selected document IDs; creating other files, deleting documents, changing sharing permissions and inserting remote images are denied. Grants expire and can be revoked. Creating a document does not authorize another conversation to read it.

Owners can use **Shared documents → Share documents with this chat** before the first message to select documents, permission and expiry. This setup action does not automatically launch the agent. The host API is `POST /api/sharing/select` with `chatID`, `documents`, `access` and `duration`; it derives sandbox identity from the conversation. A second agent retrieves its explicitly shared plan with `list_shared_documents`.

Google creation/editing requires the `documents` OAuth scope in addition to `drive.metadata.readonly`; Google describes the former as access to see, edit, create and delete all Docs. Warden holds that credential privately and enforces the narrower operation/ID grants for agents. Existing read-only connections continue to work; write requests require reconnecting with Google's expanded consent. Existing grants are revoked on reconnect. Deployment remains local until the live workflow and production configuration are separately verified.

## Large PR proposals

After implementing a document-derived change, agents can write the full `{repository, base, title, body, files, images?}` proposal to a JSON file under their workspace and call `request_pull_request({proposal_path: ".warden/proposal.json"})`. Warden reads that file once and creates the same immutable review snapshot as an inline proposal. The path cannot be combined with inline fields; symlinks, traversal, nonregular files and JSON over 2 MiB are rejected. The existing 20-file / 256 KiB content limit and editable owner-reviewed body still apply. Later changes to the workspace file do not change the pending review.
