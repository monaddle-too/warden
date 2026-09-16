# Environments as first-class entities; documents scoped to environments

Status: in progress on `codex/warden-go-backend` (September 15, 2026).

## Objective

Warden currently attaches document and repository grants to a *chat*, but
enforcement happens per *sandbox*: every chat sharing an environment reads the
same disk, and a detached process started by one chat keeps running while
another chat holds the sandbox lease. So a document shared with chat A is, in
practice, available to chat B on the same environment, and the UI never says so.

Make the environment (sandbox) the unit of sharing, show it as its own entity
in the UI with its chats and grants, and let the owner stop or delete it.

## Decisions

- New functionality is written in Go (`warden-chat`, `warden-runner`) and the
  TypeScript UI. The Python policy service keeps its store and enforcement
  path; its only change is that grant lookups match on `sandbox` instead of
  `(chat, sandbox)`. No reverse proxy is needed: the HTTP API and sign-in are
  already Go (`warden-chat` behind `warden-edge`).
- Grant rows keep recording the chat that asked (attribution, delivery of
  results to that chat's agent); they no longer restrict which chat on the
  environment may use the grant.
- Images and pull-request proposals stay chat-scoped: they are conversation
  artefacts, not environment access.
- Environment sharing between chats is kept; the UI now names every chat on
  the environment wherever a grant is approved or listed.

## Backend

Go, `chat/internal/chats`:

- `GET /api/environments` — one entry per sandbox ID in the store:
  `{id, name, chats:[{id,title,status,archived}], repository, runtime:{state,
  runtimeName}|null, documents:[grant…], repositories:[…], ports:[…],
  deleted}`. Documents come from `sharing/state` filtered to granted, unexpired
  rows for the sandbox; repositories from `github_list`; runtime from the
  worker `status` call for a chat that has run.
- `POST /api/environments/{id}/stop` — refuses while any chat on it is
  running, queued or stopping; otherwise the worker `stop` for the sandbox.
- `POST /api/environments/{id}/delete` — same running check, then revokes
  every document grant and repository selection for the sandbox, revokes its
  approved port bindings, asks the runner to `remove` the sandbox (stop +
  `sbx rm --force`, forget its managed state), archives its chats and records
  the sandbox ID in `State.DeletedSandboxes`. Chats on a deleted environment
  cannot be restored or messaged; a new chat gets a fresh environment.
- The agent prompt no longer says "preserve files shared with other chats";
  it says the workspace is shared with the environment's other chats.

Go, `chat/internal/sandbox`: worker operation `remove` and
`RuntimeDriver.Remove` (`sbx rm --force <name>`).

Python, `host/warden/sharing.py`: `list`, `get`, `active`, `authorize`,
`github_list`, `github_select`, `github_grant`, `github_active` match on
`sandbox` only. Tests in `tests/test_sharing.py` and
`tests/test_repository_sharing.py` updated: a different chat on the same
sandbox is allowed, a different sandbox is still refused.

## UI

- Sidebar gains an **Environments** section: one row per environment with a
  status dot and its chat count; selecting it opens the environment panel.
- Environment panel: name, runtime status, the chats on it, shared documents
  (with revoke), repositories, published ports, and **Stop** / **Delete**
  (delete confirms and lists what will be revoked).
- Chat context strip: the environment chip opens the panel; the documents chip
  counts the environment's grants.
- Share dialog and pending-approval copy name the environment and list every
  chat on it ("Shared with this environment: A, B").

## Steps

- [x] Python matching relaxation + tests.
- [x] Runner `remove` operation + driver + tests.
- [x] Chat engine: environments listing, stop, delete, deleted-sandbox guards;
      HTTP routes; tests.
- [x] UI: environments sidebar, panel, copy changes; build and mock check.
- [x] Deploy: one image for policy, runner and chat (all three changed);
      verify on OVH; record here.

## Deployment (2026-09-15, ~20:10 UTC)

- While this was being staged, `codex/warden-ovh-claude` deployed 76dabb1
  (Claude provider config) and then 2eac5c8 (Claude executable copied once)
  from another session; the second landed seconds around this branch's first
  switch attempt and won, which also interrupted the running "try again"
  chat. Both revisions were merged here (140abc5, b86d49b); their tests pass.
- Release b86d49b is live: `/opt/warden/current` → `/opt/warden/releases/b86d49b`,
  all three containers on `warden:b86d49b`, recreated only after the store
  showed no running, queued or stopping chat. Policy reported
  "SBX control ready"; root 200, signed-out `/api/state` and
  `/api/environments` 401, served assets `index-BSrA1Ts5.css` /
  `index-D8LwGL-n.js` match the build.
- Rollback: `ln -sfn /opt/warden/releases/2eac5c8 /opt/warden/current` and
  `docker compose … up -d` (release and image retained).
- Follow-up 2026-09-15 ~20:30 UTC: `main` 38d3a9c (this branch plus the
  Claude latency change from `codex/warden-ovh-claude`) deployed the same way;
  all three containers on `warden:38d3a9c`, switched while idle, same public
  checks green. Rollback target is now b86d49b.
- Existing grant rows are unchanged; they now apply to their whole sandbox.
- Not yet checked live with a signed-in owner: the environment panel against
  real grants, stop, delete. `codex/warden-ovh-claude` must merge this branch
  before its next release or the change is reverted.

## Progress

- Implemented 2026-09-15 on `codex/warden-go-backend` from `main` (72e7dd7).
  `sharing.py` now matches grants, repositories and `active` checks on
  `sandbox`; `test_sharing.py` proves a sibling chat on the same sandbox is
  allowed and another sandbox is refused. The runner gained `remove`
  (`RuntimeDriver.Remove` → `sbx rm --force`), covered by
  `TestManagedRemoveDeletesSandboxAndChatBindings`. `warden-chat` gained
  `GET /api/environments`, `POST /api/environments/{id}/stop|delete` and
  `State.DeletedSandboxes`; `TestEnvironmentsListStopAndDelete` covers listing
  order, the running-chat refusal, stop, delete (revoke → remove → archive)
  and the message/restore/join guards.
- UI: `EnvironmentPanel` (chats, documents with revoke, repositories,
  previews with unpublish, Stop/Delete with a confirmation listing what is
  revoked), sidebar Environments section (deleted ones under the archived
  view), environment chip opens the panel, share dialog names sibling chats.
  Checked against a mock API at 1440×900; not yet against the live stack.

## Workspace panel (2026-09-16)

The chat's context strip of chips was hard to parse, so the environment (now
called a **workspace** everywhere the owner sees it) got one surface:

- The chat title has a subtitle line of facts: sandbox state, provider,
  document and repository counts, and which other chats share the workspace.
- A **Workspace** toggle in the header opens a right-hand panel (mutually
  exclusive with Preview): status with Stop and Delete, chats in the
  workspace, documents (with *Share documents…*), repositories — the public
  checkout and GitHub-App shared repos in one list, each with its role — pull
  request proposals, and previews. It replaces the full-page environment
  panel; the sidebar's Workspaces entries open it on the workspace's chat.
- Agent requests are transcript cards: a document request shows the reason,
  names the sibling chats that would also get access, and offers Decline or
  *Choose documents…* (the existing picker); a pull request proposal offers
  *Review proposal…*. The dialogs no longer open on their own, and there are
  no "approval needed" badges.
- The provider is fixed per chat and shown only in the subtitle; the model is
  picked from the composer, disabled while a turn runs.
- API paths and Go identifiers keep `environments`/`sandbox`; only owner-facing
  strings changed.

Deployed 2026-09-16: release 963792a (`/opt/warden/current`), image
`warden:963792a` on the chat container only — policy and runner stay on
`warden:b78844b`, whose Go sources are identical in this release. Switched
while no chat was running; root 200, signed-out `/api/state` 401, served
assets `index-DXH6kQ8u.css` / `index-D0aGNZMU.js` match the build. Rollback:
`ln -sfn /opt/warden/releases/b78844b /opt/warden/current` and
`docker compose … up -d --no-deps chat`.
