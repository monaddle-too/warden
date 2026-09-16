# Figma integration plan

## Objective
Connect the owner's Figma account to Warden using the existing GitHub credential-proxy and human approval pattern. Keep provider credentials outside agents; validate real read access through Warden when account authorization and a test file are available.

## Baseline and decisions
- Worktree: `.local/figma-integration`, branch `codex/figma-integration`, starting at origin/main (8053bdd).
- Preserve unrelated uncommitted video work in the main ai-vm checkout.
- Existing GitHub integration is an HTTP credential proxy with host approvals, not the proposed general MCP authorization server.
- Initial Figma scope: explicit read operations (file content/metadata/comments and account identity). No canvas edits or comment writes.
- Prefer user-authorized Figma OAuth, with credentials isolated from agent requests. Existing Codex Figma credentials are not Warden credentials.

## Steps and progress
1. [done] Inspect host authorization, proxy routing, SBX integration and account OAuth setup.
2. [done] Implement a narrow Figma operation adapter and credential connection flow using existing approval enforcement.
3. [done] Verify authorization isolation, denied routes, credential handling, and GitHub regressions.
4. [done] Connect the owner's account and perform a read-only live test on an owner-selected file.
5. [done] Document results/limitations and commit all work. Preserve a committed runtime snapshot at the existing service path during worktree cleanup.

## Remaining inputs
- None for the own-account REST integration; historical checkpoints below record resolved blockers.

## September 14 checkpoint
- Implemented `host/warden/figma.py`: narrow REST read adapter, PKCE OAuth, fixed token endpoints, refresh, and in-memory connection.
- Added authenticated Figma setup/connect/disconnect UI and callback route. Credentials are isolated by provider; Figma grants are invalidated when replacing/disconnecting the account.
- Added Figma protected-host handling to the shared proxy and SBX destination admission. No MCP server or shared SBX credential onboarding is claimed.
- 111 relevant tests passed with the existing proxy-test-venv; JS syntax and git whitespace checks passed. Browser inspection confirmed the new connection controls render.
- Owner asked us to choose the file. Selected existing Connection Test Mockup (file key `rswcmZ4HYeLQxG2vugVPwq`) from Figma Recents, without editing it.
- Chrome is signed into Figma. No OAuth apps existed. Prepared Create app with name Warden, owner Daniel's team, but did not submit. Submission accepts Figma Developer Terms and creates persistent app credentials, requiring an action-time confirmation under the browser tool's policy.
- Isolated control process is running on port 18769 with state at `/Users/danielporter/Documents/warden-workspace/.local/figma-state`; original Warden services were not restarted.
- Pending: owner confirmation for app creation/terms; configure the app's scopes/callback, connect account, live read test, then final cleanup. Retain worktree while these steps remain unfinished.

## Account connection and live acceptance
- Owner explicitly approved app creation, Developer Terms, and read-only account connection.
- Created Warden under Daniel's team, saved loopback callback and the four read scopes, and connected the owner's account through OAuth. Credentials remain in the running control plane only.
- App is currently draft. Private publication requires the logo; Chrome denied upload because the ChatGPT extension lacks file-URL access. Owner then explicitly requested enabling that setting through computer use, but native computer use reports the Mac is locked. Await manual unlock; do not change unrelated extension permissions.
- Live Figma read through a loopback mitmproxy gateway initially exposed an AWS load-balancer rejection of GET Content-Length: 0. Fixed the Figma-only forwarding path to omit that header; 18 proxy regression tests passed.
- Live acceptance now passes: unapproved metadata read 428; exact-approved read 200 with Connection Test Mockup; replay 428; other file 428; comment write 403. Bogus caller Authorization and X-Figma-Token were stripped. Test grants revoked.
- Evidence: `.local/figma-state/live-acceptance.json`; helper source `.local/figma-live-check.py` and `.local/figma-live-proxy.py`. This validates the actual HTTP credential proxy and Figma API, not a hostile SBX network run.
- Remaining: unlock Mac, enable file-URL access on the ChatGPT browser extension as requested, upload the existing generated Warden icon, publish privately, preserve running connection, and clean up worktree after final documentation/commit.

## Private publication follow-up
- After the owner unlocked the Mac, enabled only ChatGPT extension Allow access to file URLs through native Chrome UI, as explicitly requested. Verified the setting was on.
- Uploaded the existing Warden 512px icon and completed the four scope explanations.
- Figma's Review scopes screen now blocks publication until the owner's account has two-factor authentication. Publish to Daniel's team is disabled. Do not bypass or change the app audience; the owner must complete authentication-credential setup directly.
- The OAuth account connection and previous live acceptance remain valid. Worktree retained while publication awaits owner 2FA setup.

## Google sign-in correction
- Owner clarified that Google sign-in prevents configuring Figma-native 2FA. Official Figma documentation confirms this; the earlier instruction to simply enable Figma 2FA was incomplete.
- The publication UI exemption applies to organizations requiring SSO, not ordinary personal Google sign-in. No account credentials or organization authentication settings were changed.
- Owner testing can continue with the draft app, as documented at https://developers.figma.com/docs/rest-api/oauth-apps/ and already demonstrated by live acceptance. Publication is not required for the original own-account test.
- If publication is pursued, Figma documents switching from Google SSO to email/password through its password-reset flow, then Figma-native 2FA can be configured by the owner: https://help.figma.com/hc/en-us/articles/360039820114-Manage-email-address-or-password . Leave this account-authentication choice to the owner.

## Publication completed
- Owner reported enabling 2FA. Refreshed app settings; the publishing restriction disappeared. Restored the required metadata explanation, which had not persisted, and published to Daniel's team.
- Verified Figma app list status: Private; Daniel's team members can access. No scope expansion or public audience change.
- Implementation and documentation are preserved on codex/figma-integration. Cleanup retains an exported committed runtime at the original .local/figma-integration path so the live memory-only connection can continue without a restart; the Git worktree registration is removed separately.
- Original scope is complete: owner OAuth connection, scoped REST approval proxy, live read acceptance, and private app publication. Shared SBX onboarding, MCP transport, and durable credential storage remain documented future work.
