# Rich document comment routes

Objective: let sandbox agents read saved comment context and post attributed replies through Warden, while browser draft synchronization stays owner-only.

Plan/progress:
- Added narrow GET comments/replies (paginated) and POST comments/{threadID}/replies routes to the existing signed document dispatch.
- Preserve exact body/path/context signatures, active leases, response credential filtering, and all existing enforcement.
- Explicitly test that collaboration/snapshot/editor routes remain unavailable to agents.
- Coordinate app implementation on codex/rich-documents-snapshots; leave standalone SaaS worktrees untouched.
- Validation: all 12 document gateway tests passed, including the real gateway/broker HTTP path, exact comment-reply signing, and denial of owner draft/snapshot routes.
- App browser acceptance passed with signed agent fixtures; it validates saved comment context/replies and rich proposal acceptance. This slice does not claim a new live SBX/provider run.
- Final app browser acceptance passed, including offline recovery and restart persistence. Delivery branch codex/rich-document-comments is fast-forwarded to local main; temporary worktree cleanup follows safe commit preservation. No push or deployment.
