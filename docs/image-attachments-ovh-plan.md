# Image attachments and OVH release

Objective: agents attach images to chats and PRs, with safe rendering and durable reviewed snapshots; deploy and verify on OVH.

Design: attach_image reads a bounded, regular file beneath the active conversation workspace using no-follow descriptor traversal. A separate image-normalizer process accepts only PNG/JPEG, bounds dimensions/pixels/bytes/time, decodes and re-encodes PNG without metadata. Immutable private files are bound to chat/sandbox and served via authenticated owner API. Frontend fetches authenticated blobs; never renders remote URLs or SVG from agents. PR proposals reference attachment IDs; host validates ownership and includes sanitized PNGs in the reviewed Git tree, adding GitHub-supported relative image references to the body before owner review. Exact approved body integrity remains mandatory. GitHub publication exposes images only under the destination repo's permissions.

Deployment: preserve production state and previous release, build linux/amd64, update Compose sharing socket and isolated GitHub broker access, install on OVH, smoke-test auth and native sandbox flows. No laptop relay. Existing production Google owner sign-in remains. Real PR publication requires approval of a concrete reviewed proposal; test publication with simulated GitHub transport.

- [x] Attachment ingestion, sanitizer, storage, authenticated retrieval and chat display.
- [x] PR attachments, preview and immutable publication tests.
- [x] Build, tests, OVH credential/runtime configuration and rollout.
- [x] Authenticated live acceptance, rollback documentation, preserve branch and clean worktree.


Progress: implemented attach_image, descriptor-safe guest reads, PNG/JPEG normalizer with Linux amd64 seccomp syscall allowlist plus hard CPU and wall-time limits, a soft Go memory target, and pixel/byte bounds, immutable SQLite storage and per-chat/global quotas, authenticated image endpoint and blob rendering. PRs can include up to four sanitized attachments (including image-only proposals); their GitHub relative image links enter the editable body before approval. Binary blobs are published with the reviewed tree, never through public Warden URLs. Body digest enforcement remains intact.

All Go race tests passed; targeted image/PR tests passed. Linux sanitizer verified directly on OVH, including seccomp startup. Provisioned an independent Warden-owned encrypted GitHub App record under /var/lib/warden/github (only the app record; no user tokens). Runtime does not mount Panta state. Compose now mounts the private policy socket for sharing/image RPC and enables the local installation broker. Updated edge CSP for authenticated blob images.


Completed 2026-09-14. Production release `/opt/warden/releases/13cd008` is active through `/opt/warden/current`, using `warden:13cd008` for policy, runner and chat. Updated edge binary and restarted edge service; existing Google owner sign-in verified again in system Chrome. Native SBX remains on OVH; no local service is required. Warden's independent GitHub App broker successfully discovered the installed owned repository from inside its restricted container.

Validation: full Python suite (335 tests), targeted image/PR suite after final refinements (15 tests), full Go race suite, Go vet and frontend production build passed. Linux normalizer passed both direct and restricted-container tests on OVH. Added an owner-authentication/nonexistent-conversation endpoint regression test. GitHub publication transport tests verify binary image content and exact reviewed body; real GitHub publication was deliberately not performed during acceptance.

Live acceptance: Codex in native OVH SBX created and attached a PNG. Authenticated public Chrome rendered it in chat, PR Files changed and the edited body preview. Correct conversation retrieval returned 200 image/png; anonymous public retrieval returned 401; another conversation returned 404. Rejected the image-only PR through the UI; edited body and feedback persisted, and the agent acknowledged rejection and returned idle. Removed test repository access and archived the acceptance chat; closed the test browser tab. No pending owner action remains. The existing preview sandbox was already stopped before deployment; acceptance resumed it. No existing preview route was changed.

Rollback: previous release `/opt/warden/releases/0769e79` and Docker image `warden:0769e79` remain. Private pre-rollout state backup and previous edge binary are at `/var/backups/warden/images-13cd008/` (`state.tar.gz`, `warden-edge`). If rollback is needed, restore previous binaries/release and restart services first; do not overwrite newer production state without assessing intervening activity. Backup includes credentials and must remain private.

Preservation: implementation commit `13cd008` and follow-up acceptance documentation/test are on `codex/warden-images-ovh`. Export final committed source to `/Users/danielporter/Documents/warden-workspace/.local/warden-images-ovh-source` before removing the clean temporary worktree. Production contains the implementation; follow-up commit changes documentation/tests only. No remaining implementation or deployment work.
