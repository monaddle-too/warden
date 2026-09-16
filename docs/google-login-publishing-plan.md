# Google login publishing

## Objective
Enable employee login for digitalasset.com and prevent this app from obtaining
employees' Google Docs/Drive permissions.

## Plan
- Identify target app/client and inspect live Google Cloud audience, branding and scopes.
- Confirm server-side employee-domain enforcement and separation from Docs authorization.
- Apply authorized login-only configuration; publish when prerequisites are met.
- Verify resulting access controls, preserve changes and clean up the worktree.

## Progress and decisions
- Dedicated branch codex/warden-google-login starts at 059e394.
- Existing project: Monaddle (boreal-doodad-508223-c8).
- Cloud audience is External / Testing. Publish app is disabled pending Branding configuration.
- User confirmed digitalasset.com. Target app clarification pending: Warden or SIEM.
- Warden has existing document read/write and Drive metadata integration; do not
  assume project-wide scope removal safely preserves its intended behavior.
- Google Workspace app policy can independently restrict allowed scopes to basic login.
- Branding currently has app name Monaddle, existing support/developer email, authorized
  domains sslip.io and monaddle.com, but empty homepage/privacy policy/terms URLs.
- Data Access has no declared scopes. This does not prevent Warden's code from requesting
  documents and drive.metadata.readonly, so it is not an enforcement mechanism.
- Warden edge currently accepts explicit owner emails; SIEM has digitalasset.com
  domain-role logic with hosted-domain validation. Target confirmation is necessary.

## Remaining work
User confirmed shared branding for both apps. Saved Google name as
Monaddle — Warden & SIEM; Google displayed Branding changes saved.
Clients confirmed: Warden sign-in, SIEM Viewer, Warden Google Docs, Workspace OVH,
and Monaddle local. Project-level branding/publishing affects all these clients.

Public app/privacy site source is in ../monaddle-branding, branch
codex/monaddle-branding (separate repository ../monaddle-branding-repo).
Sites project appgprj_6aa981906c1081919b8369df3709f819 created once;
version 1 source d3cfe0a9407a1a7aef62107a0ce8e3be0b364c0d.
Private deployment appgdep_6aa98226fe848191806cf3f4c744c911 pending.

User chose shared demo: all admitted users share the exact same chats/data.
DA users CAN select documents from the owner's existing Google connection and
grant/revoke chat access (including existing read/write/create workflow).
Only the owner may initiate/complete Google account connection/reconnection.
Employees cannot attach their own DA Google Docs account through the application.

Implemented DemoDomains with verified exact email + Google hosted-domain matching,
demo role/session revalidation, owner-only connection endpoint/callback and UI.
Security tests cover domain impersonation, unverified accounts, CSRF, revocation,
shared chat/grant operations and denied connection requests never reaching upstream.
Live inventory: five chats (two archived), one active document grant, no shared
repositories; no common credential patterns detected in serialized chats. This
is not a content-sensitivity certification. All admitted users see shared content.
SIEM already configured OCSF_GOOGLE_READ_DOMAINS=digitalasset.com.
Live Warden image 23697ee; preserve production max-resident=2 Compose adjustment.

Remaining: finish checks, deploy edge and frontend preserving other services,
complete public branding URLs and Google publication, preserve source/cleanup.

## Deployment and publication outcome (2026-09-15)
- ce4feba was deployed as a frontend overlay image with edge binary and
  demoDomains=digitalasset.com / ownerEmails=<owner email> in
  /opt/warden-preview/config.json; previous edge binary/config backed up under
  /var/backups/warden/demo-ce4feba/.
- The model dropdown branch (78c575f) was deployed afterwards from a sibling branch
  that lacked the demo-login frontend, so the chat image temporarily showed demo
  users a Connect button that edge rejected with 403. Merged both branches as
  b40b973 (main), full image build warden:b40b973, release
  /opt/warden/releases/b40b973, only the chat container recreated while no
  sandboxes were running. Runner/policy remain warden:23697ee. Edge was not
  restarted: the running binary is the ce4feba build and the merged Go source is
  identical. Post-deploy checks: served bundle matches local build, owner API
  healthy, signed-out /api/state 401, /oauth/callback and /api/sharing/connect 403.
- Google Auth Platform branding saved with homepage https://apps.monaddle.com/ and
  privacy https://apps.monaddle.com/privacy (observed "Branding changes saved!" and
  values persisted after reload). Terms of service left empty; no logo uploaded.
- Publishing status changed Testing -> In production (observed on Audience page).
  Google's dialog stated verification is needed only for >10 domains, a logo, or
  sensitive/restricted scopes; the separate Warden Docs connection still requests
  sensitive scopes and stays subject to the unverified-app screen and 100-user cap.
  Only the owner uses that consent flow. Verification Center was not inspected.
- Public branding site version 2 confirmed live: / and /privacy match the v2 source.

## Remaining
- Browser verification of real owner and digitalasset.com sign-in (shared chats,
  grant controls visible, Connect hidden for demo, connection denied) is still
  pending; user chose to run those sign-ins separately.
- Shared content is visible to every admitted user; the credential-pattern scan is
  not a full confidentiality audit.

