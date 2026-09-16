# Authenticated external Warden previews

## Objective and user decisions
Agents request a container/sandbox port binding. Warden authorizes that binding
and proxies it to an externally reachable URL. Only users signed in to Warden
may open it; possession of a URL is not authorization. This replaces the prior
loopback-only preview design. Warden runs entirely on OVH: chat, auth, policy,
sandbox daemon, execution and previews. The Mac is only a browser/development client.

## Implementation plan
- [x] Preserve chat extraction at 8c71824 and start a dedicated worktree/branch.
- [x] Inspect existing ingress, DNS and Warden authentication; choose a deployment path.
- [x] Define a binding bound to sandbox, chat, port, identity, lifecycle and revocation.
- [x] Implement authenticated public ingress with no direct unauthenticated sandbox URL.
- [x] Adapt the agent tool and chat preview UI to the public URL.
- [x] Verify signed-out denial, signed-in access, cross-binding isolation, stop/revocation, assets and streaming.
- [x] Document, preserve changes, deploy within the authorized scope, and clean up the worktree.

## Constraints
Keep agent-controlled destinations limited to a port inside its registered sandbox.
Warden derives the upstream and external URL. No arbitrary host/URL proxying.
Preview traffic must not receive Warden session or provider credentials.
Preserve the existing Panta and SIEM deployments. Reuse ingress only after checking
its routes and validating the complete updated config. The initial chat stack is
local single-owner; its browser capability is not a public multi-user login system.

## Progress / remaining work
- User selected Google sign-in in existing Monaddle project (`boreal-doodad-508223-c8`). Created separate **Warden sign-in** web client with origin `https://warden.monaddle.com`; no client secret is needed for ID-token verification.
- Added authoritative Squarespace A records `warden` and `*.preview` -> the OVH server, TTL 30 minutes. Existing DNS records preserved.
- OVH Caddy has a persistent `/data/sites/*.caddy` include; use this for independent Warden routes. Preserve Panta and SIEM routes and service deployments.
- Edge uses verified Google identity with initial owner allowlist (the owner's Google account). Preview sessions are host-only and tied to the main session through one-use challenge-bound tickets; logout/revocation cancels streams.
- Local Warden port tool requires an owner approval. Binding IDs map to exact chat/sandbox/port and never accept an arbitrary upstream URL. Revocation is persisted before worker cleanup.
- Full Go race tests passed; added focused port approval/revocation/path/credential tests. Frontend build passes; live public acceptance remains.
- Superseded prototype: edge listener `172.18.0.1:19081`; private reverse SSH tunnel on `127.0.0.1:19082` to Mac Warden `127.0.0.1:18780`. Only edge sees local owner capability; browser/guest never receives it.
- Public edge, Google sign-in and signed-out API denial verified live. Panta and SIEM health checks pass after independent Caddy include deployment.
- User clarified OVH-only deployment. Remove the temporary SSH relay; create dedicated `warden` service account (UID/GID 977), isolated SBX namespace and supervised server services. Initial provider login and pinned runtime are copied once from existing server assets into Warden-owned storage; no Panta runtime mounts/dependency.
- OVH-only cutover complete: edge directly uses 127.0.0.1:18780. Local chat/runner/policy and relay stopped; public app remained available. Server port 19082 is absent.
- Dedicated Warden SBX authenticated using its own Docker device login; KVM sandbox launched as x86_64 with verified default-deny/gateway policy. Linux CLI exposes an immutable implicit-deny sentinel; verifier now recognizes its exact schema and still independently checks implicit denial. Added regression test.
- Fresh model login established directly on OVH for the current Codex account, replacing an exhausted older account. Enabled Codex device authorization to complete this server login. No Mac credential sync.
- Real agent created marker WARDEN_OVH_OK, HTML counter and separate CSS asset, requested sandbox_bind_port, waited for approval, and returned an HTTPS preview. Browser counter increment and styling verified; anonymous viewers redirect to Warden login; Unpublish returns 410.
- All 307 Python tests pass; Go race suite, vet, frontend tests/build pass. Deployment image checks policy and gateway imports at build time.
- Restart acceptance exposed a revoked SBX publication restored from runtime state after its host port had been forgotten. Keep the exact removed host-port identity durably, reconcile only that owned mapping on resume, and confirm unpublish through the CLI inventory. Regression test simulates restored mappings; public access remained denied throughout.
- Deployed application revision d9a952c as image `warden:d9a952c` (`sha256:74469b5659f1c6392264761d5818f0ad6067d6f1fdc28b4afd14c2f5e77489c9`), with `/opt/warden/current` pointing to its release. Complete source is preserved in the release alongside built artifacts. Edge code is from 58ab5f6 (unchanged by the runner reconciliation fix).
- Restart/persistence acceptance passed: marker downloaded through the authenticated file endpoint (200); agent resumed the same provider thread, confirmed WARDEN_OVH_OK, restarted its detached server and received a fresh approval/URL. One intermediate worker disconnect was surfaced as a failed run; an explicit retry completed, with no silent message replay.
- Fresh preview rendered after restart. Old binding stays revoked (410). Signing out of Warden made the previously authenticated preview redirect back to Google sign-in. Mac chat endpoint and reverse relay remain stopped; public access works entirely on OVH.
- Code and plans are preserved on `codex/warden-public-previews`; local prototype history remains untouched. Final workspace documentation is exported before cleaning the dedicated worktree. No implementation work remains for this deployment; the broader MCP/connection console is tracked separately in the platform plan.

## Preview idle shutdown fix (2026-09-14)
- Reproduced in system Chrome: approved OVH counter URL returns 503 after the
  worker's 15-minute idle sweep stops its sandbox.
- Decision: an available published preview keeps its sandbox resident. Explicit
  Stop environment still stops it; unpublishing restores normal idle shutdown.
  Stale, removed or unavailable publications must not pin a sandbox.
- Work uses dedicated branch `codex/warden-preview-lifecycle`.
- [x] Restore the existing counter through its Warden chat, preserving its approved URL.
- [x] Add lifecycle regression coverage, deploy runner, and verify in system Chrome.
- [x] Preserve code and updated operational documentation; clean up the worktree.

- Full Go race suite and vet pass. Regression tests advance the idle clock past
  expiry: a published preview stays running, explicit Stop still returns 503,
  and an unpublished preview allows normal idle shutdown.
- Deployed `warden:efe13a3` to all three OVH Warden services; current release is
  `/opt/warden/releases/efe13a3`. Edge and other applications were unchanged.
- Restored the counter after deployment through its existing Warden chat and
  revalidated the same approved binding. System Chrome loaded the HTTPS page
  and the Increase counter button changed 0 to 1 after deployment.
- Operational limit: explicit stops and service restarts still require resuming
  the chat/server and revalidating its port. The idle fix does not auto-run
  stored guest commands or revive revoked bindings.

## SBX session lifetime correction
- User immediately reproduced the failure after the previous deployment.
- Daemon log proves native SBX auto-stops 30 seconds after the last session
  disconnects, independently of Warden's idle sweep. Previous browser checks
  ended too early; the earlier idle explanation was incomplete.
- Plan: maintain a credential-free SBX control session for each resident VM,
  release it on stop/failure/shutdown, retain the existing Warden idle policy,
  and test the public preview well beyond native auto-stop after agent completion.
- Work is isolated on `codex/warden-sbx-preview-session`.
- [x] Implement and test control-session lifecycle.
- [x] Deploy and verify delayed Chrome access, preserve changes and clean up.

- Full Go race suite and vet pass. The runner now creates a credential-free
  interactive exec session with a readiness handshake before marking a sandbox
  running. Agent completion retains it; explicit stop, enforcement failure,
  prepare failure and runner shutdown release it.
- Deployed application `0cd1e90` to OVH. Delayed live acceptance is in progress;
  do not treat an immediate HTTP 200 as sufficient for this failure mode.

- Delayed acceptance passed: agent completed at 00:41:28 UTC; HTTP preview
  returned 200 at 41 and 85 seconds after completion and native SBX inventory
  still reported running. Chrome then reloaded the same public URL and the
  counter incremented from 0 to 1, beyond the 30-second native grace period.
- One independent residency session was observed on the server. No new model
  requests or guest exec commands were used to keep the delayed test alive.
- Application source is preserved at `0cd1e90`, deployed to all three Warden
  containers; this documentation update records acceptance separately.

## Background preview audits
- User requests sandbox isolation audits once per minute and off the HTTP request path.
- Plan: seed a short-lived audit result during authorized prepare/attach, refresh in
  the background every minute without holding the worker mutex, reject stale results,
  and stop the sandbox on audit failure. Keep per-request identity/state/revocation checks.
- [x] Implement, test, deploy and measure warm and expired-cache page requests.

- Deployed `warden:0769e79` on OVH. Preview requests no longer call the isolation
  verifier; a worker task refreshes the proof every minute outside the request
  mutex. Proofs expire after 90 seconds; failures stop the registered sandbox.
  Initial prepare/attach still verifies isolation before granting access.
- Full Go race suite and vet pass. Added tests for no inline audits, refresh
  cadence, stale-proof denial, failed-audit shutdown, and successful HTTP
  requests while a background audit is deliberately blocked.
- Live preview restored at the same approved URL. Initial full chat-proxy
  requests measured 29/8/8 ms; 21 further requests over 40 seconds measured
  mean 10 ms and maximum 34 ms while passing the first minute after completion.
  Native daemon logs confirmed background policy audits. System Chrome loaded
  the counter correctly. Previously a fresh request took 3.16 seconds.
- Updated source and operational docs are preserved before worktree cleanup.
