# Persistent documents and local owner authentication

**Current status:** live sandbox acceptance passed and the implementation is committed and merged to local main. Source commit: 588ef5b. No deployment or remote push. Earlier uncommitted/synthetic-only notes below are historical checkpoints.

Objective: one local owner per installation, persistent project documents with reviewable agent edits, and standard HTTP document APIs reached by sandbox agents through Warden.

## Decisions
- Local launcher exchanges the private owner credential for a short-lived single-use browser login code; browser receives an HttpOnly session with CSRF protection. Localhost alone grants no authority. Preserve optional existing Google/legacy authentication.
- The sandbox is the agent permission boundary. Shared-sandbox chats share permissions; chat/run IDs are attribution. Agents never receive browser sessions or app/Warden signing secrets.
- Agent HTTP requests reach a fixed document route on their bound Warden gateway. Warden strips untrusted identity/authentication and signs exact request method, path, body and trusted active-run context. App verifies signature, freshness/replay, current assignment, and project document access.
- First document slice: Markdown content, central document IDs, project membership, revision history/restore, editable beside chat, and pending agent proposals accepted/rejected by owner. Agents can read pending proposals. Rich text/Yjs collaboration, inline comments and comment-triggered runs remain follow-ups; the existing Tiptap prototype was inspected and remains separate.
- Migrations must be additive and retain existing production/legacy data. No deployment, live-user migration, or remote push.

## Plan
1. Inspect current repos and prototype; create dedicated worktrees. Done. Streaming work is already merged in Warden main af8ff5b and will be preserved.
2. Implement local owner bootstrap/session + launcher and browser login in parallel with document storage/API and Warden gateway route.
3. Integrate document library/editor/proposal/revision UI, agent API instructions, and runtime endpoint discovery.
4. Verify local login, restart/persistence, conflicts/acceptance, Warden signed HTTP flow, cross-project/run/replay/forged-header denial, regression suites and browser behavior.
5. Update runbook and completion evidence, preserve changes safely and clean temporary worktrees.

## Work ownership
- Auth agent: local auth module, HTTP auth wiring, launcher CLI, App.tsx login integration.
- Documents agent: persistent document models/storage and owner/agent HTTP API with signature verification.
- Warden agent: narrowly scoped signed document proxy route and broker configuration/tests.
- Coordinator: document UI, runtime instructions/discovery, API contract integration, cross-repo verification and delivery.

## Warden progress
- Implemented fixed loopback document origin + private signing-key configuration.
- Added gateway document HTTP routes, broker-only exact-request HMAC signing from trusted binding/lease context, public Begin endpoint discovery and bounded JSON transport.
- Kept provider streaming behavior intact. Added real upstream HTTP tests, real mitmproxy-flow tests, and a real gateway/broker/upstream process test, including forged identity, cross-sandbox capability, stale lease, cancellation, redirect and credential-reflection denial.
- Full regression suite passed 237 tests including the process and pagination additions. A final lease-expiry ordering fix adds one regression and is covered by the focused SBX suite; exact final checks are recorded in the coordinator's delivery evidence. Cross-repo actual Go API validation uses an isolated synthetic verifier harness and no production sandbox state.
- Runbook: `docs/sbx-document-api.md`. Remaining integration belongs to the app: persistent assignment/replay checks, owner UI and agent endpoint instructions.


## Coordinator acceptance

Final full suite: 238 tests passed in 53.449 seconds. A real mitmdump/broker route was tested against the actual Go app with a synthetic active sandbox assignment, including browser proposal review/accept, forged guest headers stripped, and owner credentials denied on agent routes. The app independently verifies signatures and replay protection. No new live SBX/AI run was performed; fixture enforcement is not production containment evidence.

App implementation includes local-owner browser sessions, Markdown document editing/revisions/proposals, pagination and idempotent retries. See the sibling app docs/persistent-documents-plan.md and startup guide. Test services are stopped. Source preservation and temporary worktree cleanup are complete; no commit, merge, deployment or push is included.


## Delivery preservation

All source changes have been preserved and byte-verified in the original checkout, uncommitted on main. Private source archives and SHA-256 manifests are retained in /Users/danielporter/Documents/warden-workspace/.local/documents-validation. Test services are stopped. Temporary implementation worktrees were removed after successful source/archive verification. No merge, deployment, commit or remote push was performed for this slice.


## Live acceptance and merge

Authorized next: run an actual sandboxed agent reading a document and submitting a proposal through production Warden enforcement, verify it in the app, then commit and merge both repositories to local main. Use isolated test state and new worktrees; preserve unrelated work. No remote push or deployment requested. Source manifests checked before copying. Live acceptance passed; see [live evidence](live-document-acceptance.md). Source commits and local-main merges are complete. The verified temporary worktrees are being cleaned up as the final step.


Live validation now supersedes the earlier synthetic-only acceptance limitation. The actual agent completed the read/proposal flow, owner acceptance and reload passed, post-run access was denied, and the test environment was stopped.


## Final merge record

Local main contains the verified source commit and live evidence. Original uncommitted delivery is additionally retained in a scoped backup stash and private source archive. The merge was a fast-forward with no conflicts or additional code changes. Test services and the owned sandbox are stopped; unrelated work and the CLI assessment document were preserved. Temporary worktrees are removed after this documentation commit is included in main.
