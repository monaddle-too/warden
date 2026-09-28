# Document deletion

## Objective
Delete documents from the organization library while preserving stable references that explicitly report deletion. Keep deletion reversible and prevent stale editors/agents from recreating deleted content.

## Design
An additive, organization-local tombstone table retains node metadata and the materialized collaborative snapshot. An atomic delete removes the live node and replaces live state with a blank snapshot under a new epoch. The state ID remains reserved to prevent older releases recycling the UUID; older releases see a missing document and cannot overwrite it; newer releases report HTTP 410. Restore retains the ID but rotates the collaboration epoch. Comments, versions and suggestions remain in PostgreSQL.

Deletion requires a signed-in organization member, explicit UI confirmation and an idempotency key. Deleted documents are absent from library/search; batch reference resolution returns a deleted status only within the owning organization. Existing ordinary links retain their labels and get an explicit deleted-document destination, with restore available.

## Steps
- [x] Read current deployment sources; create dedicated worktrees.
- [x] Tombstones, deletion/restore endpoints, stale-client guards and integration tests.
- [x] Delete confirmation, deleted page, reference status handling and cache invalidation.
- [x] Builds, browser acceptance, deployment and live checks.

## Validation (2026-09-27)
- Warden web build and all 307 tests pass; cloudauth and edge Go suites pass against PostgreSQL.
- Panta web build and all 24 service/organization/tool/collaboration/deletion tests pass against PostgreSQL.
- Deletion test also ran against previous release Documents implementation: old writes to unrelated documents continue, deleted IDs cannot be recycled, stale epoch writes fail, restored content is readable by the previous release.
- Browser: confirmation layout, deletion, existing chat reference to deleted page, restore retaining ID and editor verified with real PostgreSQL/Panta fixture.
- Browser also verified a saved Panta @ mention after deletion, library removal, and transition of another already-open editor to the deleted page.

## Deployment
2026-09-27: Helm revision 49, all five services ready. Warden `f42167a` (`cloud-org-f42167a`, digest `sha256:7057bde1591bc07891439b5c0582b0f7b0af4df92253fa2c8bb255b0edc76fe0`); private Panta `b37c93c` (digest `sha256:2eaeb5adbd1ef3d0c8a619eec7105f2458bff230898463c04f3f09bb6d7bf45d`).
Live browser: created disposable document, saved content, deleted, reloaded existing URL and saw explicit deletion message, restored same ID and original content, then deleted test document again. One save-barrier timeout after restoration displayed a retryable error; retry succeeded without data loss. Original selected organization restored.
No public push: inherited cloud branch history stays local. Database migration is additive and rollback compatible. Registry session removed; build VM and test PostgreSQL shut down after acceptance.
