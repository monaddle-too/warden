# Document picker date filter

Objective: omit Google Docs created before September9,2026 from the sharing picker.

Implemented: Drive list query includes createdTime >= 2026-09-09T00:00:00Z, so filtering applies before pagination and uses creation rather than modification time. Picker states the date cutoff. Existing grants and direct document retrieval remain unchanged.

Remaining: build, verify query, deploy when chats idle, preserve changes and clean worktree.

Completed:23697ee deployed on OVH with rollback/state backup. Live sharing/files returns3 Google documents after the creation-date filter. Query/pagination assertion,4 sharing tests and frontend build/typecheck passed. Pushed origin/codex/warden-doc-date-filter; source preserved in workspace .local/warden-doc-date-source before worktree cleanup.
