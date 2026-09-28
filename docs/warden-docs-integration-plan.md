# Warden documents integration

## Objective
One Warden navigation shell for chats, organization administration, documents and drawings. Agents can list, read, create, edit and comment on every document in their chat's organization, and submit suggestions for human review without per-document grants.

## Decisions
- Keep private Panta source in its own repository. Warden embeds the trusted same-origin document application inside its persistent shell.
- Organization identity and agent attribution come from the host, never tool arguments. Existing database schemas, revisions, idempotency receipts and suggestion resolution remain authoritative.
- No schema changes. Old/new releases remain compatible with the same document data.

## Progress
- Inspected current shell, private proxy, native document tools and proposal review.
- Implemented persistent shell routes and embedded private editor, coherent design tokens, and eight organization-bound agent document tools.
- Warden Go edge/chats/chatsvc suites pass; 302 web tests pass; both frontends build. Panta 23 tests pass with PostgreSQL (organization tools, legacy service/MCP, concurrent instances, collaboration persistence and suggestions).
- Helm lint passes. Deployed Warden `1758e04` and private Panta `aaade56` as Helm revision 43 on 2026-09-26. Five services ready, zero restarts.
- Live Chrome verified the persistent Warden sidebar, document library, drawing editor, dark-theme controls, direct document links and return-to-library navigation. Fixed embedded route synchronization, comment highlight contrast, organization sharing label and the new-chat modal's viewport overflow.
- Live Claude created a disposable document, read it, edited it directly, added a comment, proposed a pending suggestion and read back all three states. Codex independently read the document and suggestion, confirming consistent revision numbers. The older cloud Codex credential was replaced with the owner's current authorized local sign-in.
- Organization isolation, concurrent edits, idempotent retries, stale suggestion rejection, human acceptance/rejection, old document-method compatibility and persistence covered by 23 passing Panta tests against PostgreSQL. No database schema changes.
- Live browser acceptance verified: the suggestion changed the test document from “Hello organization.” to “Welcome organization.”, marked Accepted, with its comment preserved. Build VM and test database stopped; registry credentials logged out. Work complete.
