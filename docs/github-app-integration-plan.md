# GitHub App credential sharing plan

## Objective
Use the existing Monaddle Workspace GitHub App to provide Warden-managed repository credentials without users pasting PATs or OAuth tokens. Keep credentials outside agents and retain granular human approvals.

## Plan and progress
1. Done: inspect the registered app, existing credential broker, installations, and current Warden GitHub paths.
2. Done: implement host-side installation credential acquisition/renewal and sharing with the relevant Warden permission flow, plus setup UI and tests.
3. Done: connect an authorized installation and validate real approved repository reads without a user token.
4. Done: document limitations/results, commit, preserve runtime if needed, and remove the dedicated worktree safely.

## Decisions
- Dedicated codex/github-app-integration branch based on completed Google Docs integration.
- Preserve unrelated dirty main checkout and existing connected Figma/Google processes.
- Reuse the existing app; do not broaden installation repository selection or app permissions without task-specific need.
- Prefer existing trusted broker integration over creating duplicate app credentials if available.

## Remaining work
Determine where the existing app's backend credentials/broker live. End users should select/install the app and approve Warden requests; app-private credentials belong only on the trusted host/service.

## Implementation and live checkpoint
- Located Monaddle Workspace app 4893871 and existing installation 160513175,
  selected only monaddle-too/panta. User completed GitHub access verification.
- Found existing encrypted app record on OVH; private helper reads/decrypts only
  app record and signs JWT there. No key copied to Mac or new app key generated.
- Added WARDEN_GITHUB_APP_BROKER host configuration, automatic per-approved-request
  repository/permission-scoped minting, strict owner/app/response checks, and UI
  hiding/disabling manual tokens in App mode. Source usable by separate Engines
  without sharing grants. Existing Google/Figma instances remain running.
- Live proxy acceptance passes: blocked before approval, panta metadata 200 after
  exact approval, replay/other-repository 428, unsupported issue POST 403. No GitHub
  writes performed. Remote helper installed without restarting Panta or editing DB.
- Remaining: final expanded tests, cleanup test grants/proxy, commit all work,
  export runtime and safely remove worktree. Document production boundaries.

## Final verification
- 152-test regression suite passed, then all 17 broker tests passed after adding
  write-permission and separate-Engine coverage. JavaScript syntax and whitespace
  checks passed. Test grants revoked and pending test requests cleared.
- Changes preserved on codex/github-app-integration; export committed runtime at
  original path and remove worktree registration so live control plane continues.
- No further inputs needed for the configured own-account installation broker.
