# Google Docs sharing approvals

Objective: agent requests read access; owner signs into Google, selects documents and duration; Warden persists grants, delivers the tool result, and injects credentials into authorized Google API requests.

Decisions: local implementation on the Claude integration branch; no OVH deployment. Host-only Google OAuth and file listing. Durable requests/grants scoped to owner, conversation and sandbox; enforce read-only route and expiry at dispatch and response. Keep pending tool calls alive without depending on their connection. On interrupted runs, enqueue a deduplicated continuation with the saved result. Agents discover only shared files.

Steps:
- [x] Inspect current agent, proxy and Google OAuth integration.
- [x] Build persistent sharing and Google connection service.
- [x] Wire tools, durable completion and authenticated owner endpoints.
- [x] Add sign-in/file picker/duration approval UI and active grants controls.
- [x] Verify expiration, denial, isolation, restart delivery and real local Google flow.
- [x] Preserve tested local runtime and commits; worktree is ready for clean removal.

Google listing needs Drive metadata access in addition to document read access. Reference: https://developers.google.com/workspace/drive/api/guides/api-specific-auth . Warden's picker keeps OAuth tokens off the frontend and lists Google Docs only.

Progress (2026-09-14): implemented host-only OAuth persistence and Drive listing, SQLite requests/grants, tool wait and post-restart notification delivery, owner popup with multi-select and 15-minute/1-hour/24-hour/7-day durations, active grants/revocation, and credential injection on the exact read-only Docs route. Requests bind to Warden's trusted conversation/sandbox identity, not model arguments. Reconnecting Google invalidates existing grants.

Validation: 314 Python tests pass, including actual mitmproxy injection/revocation flows and scope/expiry tests. Go race tests and vet pass, including crash-before-notification-delivery deduplication. Frontend builds. Real Claude created a request; it survived a full local stack restart. Google OAuth succeeded using the existing Warden Google Docs client; Drive API was enabled and the real file list renders. The owner delegated completion of the test. Selected the Rust document for 15 minutes; Claude resumed automatically after restart, discovered the API link, fetched document ID/title without a Google credential and with default TLS verification, then received HTTP 403 after revocation. A separate denied request also resumed and was acknowledged by Claude.

Google setup: user explicitly approved adding the loopback callback and additional secret, and authorizing document reads plus Drive metadata. Added `http://127.0.0.1:18781/oauth/google_docs/callback` without removing the old callback/secret. Configuration is private in `~/.warden-claude-local/provider/google.json`; OAuth tokens are private in the broker's `google.sqlite`. Neither tokens nor full Drive listings reach the agent. Google remains in test-app mode; users may need to reconnect when Google expires/revokes the refresh token.

Live testing also found and fixed inherited lowercase proxy variables overriding Warden, and missing guest trust for Warden’s gateway CA. Both providers now set upper/lowercase proxy variables, and the runner installs only the gateway public certificate into guest trust. A regression test covers proxy environment precedence.

Final acceptance: the uninterrupted `request_google_docs_access` tool returned the selected document and Claude acknowledged it; the durable request was marked delivered only after normal run completion. Both temporary grants were revoked. Acceptance chats were archived, preserving their transcripts. No owner action is pending.

Stable local build: `/Users/danielporter/Documents/warden-workspace/.local/warden-doc-sharing-source`, with `start-local.sh` and `LOCAL.md` restart instructions. Uses existing `~/.warden-claude-local` state and port 18781. Branch: `codex/warden-doc-sharing`. OVH remains unchanged.
