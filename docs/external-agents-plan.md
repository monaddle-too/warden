# External agents and shared conversations

## Objective
Connect external MCP clients to one chosen organization: list/read/create/edit/comment/suggest on Panta documents, and explicitly upload conversations for organization members to read. Keep durable state in PostgreSQL.

## Design
- Edge hosts stateless Streamable HTTP MCP at `/mcp`, OAuth discovery, dynamic public-client registration, authorization-code callback with required S256 PKCE and resource binding, rotating refresh tokens, and revocation.
- Consent names the client, callback host, organization and permissions. Sign-in resumes the consent page. No Warden runtime/admin tools are exposed. Every request checks current account/membership; membership removal revokes grants permanently.
- Reuse the existing private Panta bridge and its revision/idempotency checks. Suggestions remain pending for human review.
- Shared conversations are immutable organization-only snapshots with explicit messages, a stable URL, and operation-id retry protection. Uploads do not execute messages or access local agent history automatically.
- Personal settings shows the MCP URL and connected agents with organization and Disconnect. Shared conversations has a persistent sidebar entry and a read-only view.

## Steps
- [x] OAuth schema, callback, consent, token rotation/revocation and membership guards.
- [x] MCP document tools and shared conversation upload/list/view.
- [x] UI and feature map.
- [x] PostgreSQL tests: callback/PKCE/replay, rotation/revocation, tenancy, upload idempotency, restart.
- [x] Real MCP SDK integration, browser review, build and deployment/live verification.

## Deployment
Deployed 2026-09-26, Helm revision 44, Warden image `cloud-org-88c39b9`, digest `sha256:aa0775259f5b5ee1e947dc5ba0bb3777e60d73b57ca287ef1d0ac2f9102bb2d4`. Private Panta image remains `aaade56`. All five services ready, zero restarts. Public history must not be pushed: inherited cloud branch contains private installation identifiers. Local commits only.

## Verification so far
- PostgreSQL tests pass, including actual SDK OAuth discovery/DCR, browser-consent POST, loopback callback, S256 exchange, refresh rotation/replay revocation, membership removal/re-add, organization switching, shared snapshot retry/conflict and store reopen.
- Existing Go cloudauth/chats/edge suites pass; web 39 files / 302 tests pass; TypeScript and production build pass.
- Live cloud verification passed with the official MCP SDK, using an isolated temporary organization/account. Discovery, registration, actual loopback callback, PKCE, all document operations, sharing, refresh, and revocation passed.
- Browser checked connection settings and consent layout, shared conversation list/detail and Markdown rendering, live Panta edit/comment/pending suggestion, and organization switch refusing the previous organization’s shared URL. Temporary cloud test data and credentials removed afterward.
- Connection-list organization labels, CSRF protection and personal-settings disconnect verified in PostgreSQL integration tests.
- Full SDK workflow against real Panta/PostgreSQL passed: create/read/edit, stale revision rejection, comments, pending suggestions (live text unchanged), document-create retry, shared-conversation upload/retry, and OAuth refresh/revoke. No private Panta source changes required.
