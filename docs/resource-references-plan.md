# Resource references

## Objective
Link organization documents, chats and shared conversations by typing @ followed by a title, in chats and documents. Preserve ordinary links and stable IDs; no new collaborative schema or transcript expansion.

## Design
- Authenticated, organization-scoped metadata search and batch resolution; bounded results and request deadlines.
- Shared Warden reference client with small memory cache; iframe requests pass through an origin/source checked bridge.
- Existing chat picker and a Tiptap adapter support multiword queries, keyboard selection, cancellation and stale response suppression.
- Preserve explicit path/resource mention syntax. Ordinary canonical links navigate through the Warden shell.
- No persistent browser cache, content preload, search engine or document rewrite on rename.

## Progress
- [x] Inspect deployed sources and create isolated Warden/Panta worktrees.
- [x] Metadata providers and API; organization isolation tests.
- [x] Shared client, chat picker, document picker and navigation.
- [x] Build, targeted automated tests and browser acceptance.
- [x] Deploy both services, verify live, record versions.

## Validation in progress
- Warden frontend: 306 tests passing; production build passed.
- PostgreSQL: cloudauth/edge tests pass, including search metadata, scope, resolution after rename and membership revocation.
- Panta PostgreSQL organization/tools tests pass, including private reference bridge, wildcard escaping and cross-org resolution.
- Integrated browser fixture: chat multiword search, keyboard insertion, document-to-shared-conversation navigation, retained shell and saved link verified.
- Ordinary links preserve insertion-time labels; resolving a clicked link uses current metadata. No automatic label rewrites, backlinks or previews in this release.

## Deployment and final verification — 2026-09-27
- Helm revision 48: Warden `cloud-org-779080d` (digest `sha256:88a060514a99896570014a6857769bde8c918738559ff22cf8817637f4bab6d8`); private Panta `e568c54` (digest `sha256:3fdbf4e8552ae5de0715277b70ddd0c7f4bf4e230fcbac2b0c355d6346695c2c`). All five deployments ready. Revision 46 is the pre-feature rollback; ordinary-link writes remain compatible.
- Live Chrome: chat search returned the actual document and two chats; Enter inserted its canonical URL. Panta search found organization chats through the shell bridge and inserted a chat link. Temporary draft/document edits were undone, with Saved confirmed.
- Live organization switch: other organization's library remained empty and opening the Warden document was refused. Final unavailable screen shows Back to documents / Try again and stops initial automatic retries. Original organization restored.
- Local integrated browser: all three target types, multiword query, keyboard insertion, consecutive references, Escape, persistence, document-to-chat/shared navigation, retained Warden shell and 390px viewport.
- Final builds passed. Web suite: 306 tests / 40 files. Panta service/organization/tools/collaboration suite: 23 tests, no skips. Go cloudauth and edge passed with PostgreSQL. Broad chats run had one existing four-second timing failure in TestDenyAndAskRulesInAuto; that test passed on targeted rerun together with cloud/organization/Panta tests.
- No database or document-schema migration. Local-only commits; private cloud history was not pushed to the public origin. Test fixture and PostgreSQL stopped, registry login removed, build VM stopped.
