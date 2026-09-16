# Admin console: logins, connected accounts, shareable resources

## Objective
Give the demo owner an admin screen in Warden that shows every Google identity
that has signed in, which provider accounts are connected (only the owner can
connect accounts, so only the owner's identity lists any), what each connected
account can share (Google documents from the Drive listing, GitHub repositories
from the App installation), and lets the owner tag a Google document as
"unsharable with AI" so it can never be granted to a conversation.

## Design
- Identity is verified only at the edge (`chat/internal/browserauth`), and its
  sessions are in memory. A durable login ledger (`loginsFile` edge config,
  JSON, atomically replaced, mode 0600) records first/last sign-in, count,
  role and hosted domain per email. Without `loginsFile` the ledger is
  in-memory and the API reports `persistent: false`.
- The edge serves `GET /api/admin/users` itself (admin role only, never
  proxied). Demo users receive 403 and the request never reaches upstream.
- Connected accounts are attributed to the configured owner emails because the
  edge only lets admins reach `/api/sharing/connect` and `/oauth/`.
  `sharing/status` gains `github: {connected, owner}`; file and repository
  listings reuse the existing `sharing/files` and `sharing/github_repositories`.
- "Unsharable with AI" tags live in `sharing.sqlite` (`blocked_documents`).
  Operations: `blocked` (GET), `block` and `unblock` (POST, admin-only at the
  edge). Tagging revokes any active grant that includes the document. `files`
  annotates each document with `blocked`; `select`/`resolve` reject blocked
  IDs; `authorize` rejects sandbox requests for blocked documents even when a
  stale grant exists. The picker renders blocked documents disabled.
- Frontend: `AdminConsole` component reachable from the sidebar for admin
  sessions only; local no-edge mode has no admin identity and no button.

## Steps
- [x] Inspect edge, browser auth, sharing service and frontend components.
- [x] Login ledger + `/api/admin/users` in edge with tests (749a4d6).
- [x] Blocked-document tags in `sharing.py`, Go route allowlist, tests (02e495a).
- [x] `AdminConsole` UI, sidebar entry, picker rendering of blocked docs.
- [x] Go race tests, vet, Python tests, TypeScript build.
- [x] Deployed to OVH as release 8b01191 (2026-09-15).

## Progress and decisions
- Branch `codex/warden-admin-console` from `main` (96b4d09) in worktree
  `.local/warden-admin-console`.
- Implemented and verified 2026-09-15: `go -C chat test -race ./...` and vet
  pass; 349 Python tests pass; `pnpm build` and `pnpm test` pass; prettier clean.
- UI checked in the browser against a mock API (scratchpad only, not in the
  repo): owner sees the sidebar "Admin console" entry, signed-in users with
  Owner/Demo badges and sign-in counts, Google Docs and GitHub App accounts
  under the owner, per-document "Unsharable with AI" toggles (with a notice
  when tagging revoked active grants), a summary list of tagged documents with
  "Allow sharing"; the share picker renders tagged documents disabled; a demo
  session has no admin entry.
- Not verified against the live stack: real Google sign-in through the edge
  and real Drive/GitHub listings. Local no-edge mode has no admin identity, so
  the console is only reachable through the public edge.

## Deployment (done 2026-09-15)
- Release 8b01191 built as image warden:8b01191 and installed at
  /opt/warden/releases/8b01191 (/opt/warden/current). All three containers
  (policy, runner, chat) now run this image; this was the first policy/runner
  bump since 23697ee and the only host-side change in between was sharing.py.
  No sandboxes were running during the switch.
- Edge binary replaced, config gained loginsFile=/var/lib/warden/edge/logins.json,
  directory /var/lib/warden/edge (warden:warden 0700) added with a systemd
  drop-in /etc/systemd/system/warden-edge.service.d/ledger.conf (ReadWritePaths).
  Edge restarted 18:54 UTC, signing viewers out; the ledger starts empty from then.
- Backups: /var/backups/warden/admin-8b01191/ (previous edge binary/config,
  unit, sharing.sqlite). Release b40b973 and its image are kept for rollback.
- Post-deploy checks: services active, root 200, signed-out /api/state 401,
  /api/admin/users and /api/sharing/block 403, served bundle matches the build,
  blocked_documents table present and empty.
- Not yet observed: a real owner sign-in through the deployed console.

## Original deployment notes
- Edge config needs `"loginsFile": "/opt/warden-preview/logins.json"` (or any
  private absolute path writable by the edge service user); without it the
  ledger is in-memory and the console says so. The ledger starts empty, so
  sign-ins before the new edge binary is installed are not listed.
- Requires a new edge binary (ledger, `/api/admin/users`, owner-only
  block/unblock), a new chat image (Go route allowlist + frontend), and the
  updated Python host (`sharing.py`) for the policy container. `sharing.sqlite`
  gains `blocked_documents` automatically on start.

## Follow-up: login screen alignment (2026-09-15)
- The Google sign-in button rendered left-aligned inside its block wrapper on
  the sign-in screen; `.signin .google-button` now centers it (34f62f1).
- Deployed as a frontend-only overlay image warden:34f62f1 (FROM warden:8b01191
  with the rebuilt web assets), release /opt/warden/releases/34f62f1 copied
  from 8b01191. Only the chat container was recreated; policy and runner remain
  on 8b01191. No sandboxes or chats were running. Served bundle matches the
  build; the live button center matches the sign-in card center (423px both).
