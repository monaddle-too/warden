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
