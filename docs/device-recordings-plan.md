# Device recordings

## Objective
Organization members register devices in the web UI and use a one-time secret with the supplied Python client to upload WAV recordings or stream PCM audio. Recordings appear immediately, live transcripts update, and final audio can be played back.

## Contract
- Device credentials: random bearer secrets stored as hashes, scoped to organization and creator membership. Creator/admin can rotate/revoke. Rechecked on each chunk and status read.
- PCM s16le, 16 kHz mono; binary WebSocket messages: 8-byte big-endian sequence + PCM. 100 ms suggested, 1 second maximum. Durable acknowledgements, duplicate digest checking, next sequence/byte offset for resume. Completed WAV upload uses the same ingestion path.
- PostgreSQL audio chunks are a durable bounded processing queue. Final 10-second audio segments go to private GCS; transcript/segment metadata stays in PostgreSQL. No server disk persistence. Worker leases recover across restarts. Audio archiving continues even if transcription fails.
- Transcriber interface initially Google Speech-to-Text, language en-US default. 5-second provisional text, 10-second final segments; last segment finalized at finish. Basic quality, no diarization or summaries.
- Browser recording list/detail/device management, SSE snapshots with reconnect, authenticated playback. OpenAPI HTTP contract + AsyncAPI WebSocket contract and Python example.
- Bounds: 2 hours per recording, 2 unfinished recordings/device, 20 devices/member, max 1 second/chunk, bounded pending audio/device. No provider secrets on devices.

## Progress
- [x] Read feature map, branch from origin/main and integrate current cloud release; register checkout.
- [x] Database, device authentication, ingest/upload/WebSocket and worker.
- [x] UI, specs, Python client and tests.
- [ ] Cloud infrastructure/deploy, UI-generated key to Python upload/stream/reconnect acceptance.

Cloud history stays local; do not push to public origin. Additive schemas support previous release rollback.

## Validation so far
- PostgreSQL integration: UI credential registration, hash-only listing, HTTP upload, WebSocket resume, duplicate/conflicting chunks, finish sequence, provisional/final transcript, overlapping workers, expired leases, provider failure with preserved audio, retry, key rotation and removed member denial pass.
- 309 web tests pass. HTTP spec validated with openapi-spec-validator; streaming spec with @asyncapi/parser.
- Dedicated private GCS bucket and workload identity grants for edge; Google Speech API enabled. Cloud resources use existing project.

## Cloud release
- Deployed 2026-09-28: Helm revision 50, Warden `1191d9d`, image `cloud-org-1191d9d`; private Panta unchanged. All five services ready.
- Live Recordings → Register a device form verified in the existing organization session; UI layout checked. Live OpenAPI validates and the published Python client starts successfully.
- Pending final acceptance: browser-created persistent credential requires action-time confirmation under browser tooling rules. Form is prepared for “Python acceptance test” in the test organization. After confirmation, upload synthetic speech, stream with deliberate reconnect, verify live/final transcripts and playback, then revoke test key. Do not claim this acceptance passed yet.

## Copyable device script
New/replacement key confirmation offers Copy Python script and an expandable read-only preview. It reuses the published client source with the issued key and current origin filled in; no credential persistence is added. Existing hashes cannot recover older keys.

Copyable-script release deployed 2026-09-28: Helm revision 51, image `cloud-org-b4be791`. Web build and 309 tests pass; generated Python syntax/configuration verified. Public client confirms release. A test organization was created in the platform UI. Live device acceptance still pending earlier credential confirmation.

## Automatic invitations
Adding a member sends an invitation through the configured SMTP service after membership is saved. The response/UI distinguishes successful mail submission from failure; resubmitting the same member retries delivery without duplicating membership. Existing members can be resubmitted to send an invitation.

Automatic invitations deployed 2026-09-28: Helm revision 52, image `cloud-org-731aa46`; all five services ready. Go cloudauth/edge tests and web build pass. Live existing-member submission for the test organization returned “Invitation email sent” via configured SMTP; membership remains organization admin. This verifies provider acceptance, not recipient inbox placement.
