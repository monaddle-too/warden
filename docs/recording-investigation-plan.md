# Recording workflow investigation

Started 2026-10-01 from origin/main 03027db.

## Objective
Diagnose and fix the reported cloud device recording failure.

## Steps
1. Inspect production readiness, logs and recording metadata.
2. Reproduce the failing step and implement a focused fix.
3. Verify and deploy; record evidence and remaining limitations.

## Progress
Awaiting specific error from user while inspecting live service. Prior implementation never completed live device-key acceptance.

## Findings and fix
Storage accepts the direct GKE federated token; Speech v1 returns 401 for the same token. Google documents that only Speech v2 accepts identity federation directly. Three non-empty recordings have all audio archived but failed transcript segments.

Configure keyless IAM service-account impersonation on the edge KSA using recordings.serviceAccountEmail; bind the existing speech/storage permissions to the dedicated IAM account. Restart edge to refresh cached tokens, verify Speech and Storage, then retry only the affected failed segments.

## Recovery
- Helm revision 53 applied the dedicated IAM identity; edge restarted to clear its token cache. Live Speech v1 probe changed from 401 to 200 after IAM propagation.
- Retried seven failed segments across the three non-empty recordings. Browser shows all complete; inspected transcript and played audio (30-second duration, playback progressed without error). Audio had been preserved throughout.
- Separately, the invited user’s chat failed because all four retained workspace slots were occupied. Increase the cloud retention limit to eight, preserving maxRunning=2 and existing workspaces.
- Add a pod-template identity checksum so future service account changes automatically restart edge and refresh cached tokens.
