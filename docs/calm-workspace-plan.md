# Option A: a calm, unified workspace

## Objective
Implement the approved Figma Option A with one Warden sidebar, a clean document library, and contextual editor review. Keep organization isolation, collaboration, agent tools, drawings, and all account/admin controls.

## Decisions
- Cloud UI only for the navigation redesign; local Warden retains existing navigation.
- Organization switcher at the top; account/admin actions in a menu at the bottom.
- Panta remains a private embedded service, with no duplicate navigation.
- Reuse existing React controls, Lucide icons and Warden tokens. Figma's generic Simple Design System Button is not installed in either app; existing semantic buttons are the appropriate native equivalent.
- Use actual folder/type metadata. No invented author/dates or browser-persisted recent history.
- No API or database migrations; existing and new releases remain compatible.

## Progress
- [x] Inspect approved Figma library/editor and current implementation.
- [x] Implement and build both frontends.
- [x] Verify navigation, library, editing and review in browser.
- [x] Commit, deploy both services and verify live.

## Verification and release — 2026-09-27

- Warden: `pnpm --dir chat/web build` and `pnpm --dir chat/web test` passed (39 files, 302 tests). Linux/amd64 Go binary and production image built successfully.
- Panta: `npm --prefix apps/docs run build` passed. `NODE_ENV=test node --test apps/docs/tests/service.test.mjs apps/docs/tests/collaboration-store.test.mjs` passed 20, skipped the PostgreSQL case without a DSN; the subsequent PostgreSQL run passed all 10 with no skips: `PANTA_TEST_POSTGRES_URL=… node --test apps/docs/tests/organizations.test.mjs apps/docs/tests/warden-tools.test.mjs apps/docs/tests/collaboration-store.test.mjs`.
- CUA browser verification: integrated shell/library at desktop and 390px; document creation, typing and persistence on reload; comment creation; mutually exclusive comments/suggestions/history panels; folder creation and breadcrumbs; drawing creation and adding a frame.
- Live browser verified library, editor Saved state, existing comments and accepted suggestions, drawing filter, personal settings (profile/passkeys/connected agents), platform organization management, and Warden → another organization → Warden from an open document. Organization changes return to the destination library instead of retaining a foreign document URL.
- The live drawing navigation check was interrupted by the Chrome connection. The same built drawing UI was then verified successfully in the isolated local fixture. The updated standalone Playwright spec was not executed; browser workflows were exercised through CUA.
- Figma target uses a light warm palette and Inter. The cloud shell and embedded content share these tokens; Inter is served locally with the SIL Open Font License included. Local Warden navigation markup remains unchanged.
- No runtime persistence changes or database migrations. No production documents were changed during acceptance checks. Test documents/drawings were confined to the disposable local fixture.
- Deployed Helm revision 46: Warden `cloud-org-ce28e64`, private Panta `c214cbf`. Revision 45 contained the visual rollout; revision 46 preserves local-install footer markup without changing the cloud layout. All five deployments ready, no restarts observed after revision 45; final readiness checked after revision 46.
- Deployment branches remain local and unmerged; no public push (cloud integration history includes private deployment identifiers). Roll back to Helm revision 44 to restore the previous UI; the database schema is unchanged.
