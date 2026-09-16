# SBX Warden implementation progress

Objective: provide a separate, fail-closed SBX enforcement controller and
fixed-identity inspected gateway while preserving the existing VM path.
Branch: `codex/warden-sbx-integration`; baseline `5598776`.
The coordinator owns live SBX configuration and acceptance testing. Synthetic
tests use isolated fixtures; the authorized live demonstration uses the host
Codex login through the broker. Existing VM topology and state remain untouched.

## Plan and progress

- [x] Agree private newline JSON v1 register/check/begin/renew/end contract with worker.
- [x] Implement durable bindings, verified readiness, bounded leases and revocation.
- [x] Reuse the inspected Guard through local Unix RPC with fixed gateway identity.
- [x] Implement the pinned SBX 0.42.1 managed verifier and scoped network transition.
- [x] Keep provider credentials on the host, including the existing Codex login.
- [x] Run synthetic, process and existing regression tests: 193 tests pass.
- [x] Demonstrate live chat, interactive preview, fresh/shared chats, idle/resume
  and controller-loss shutdown through the integrated path.
- [x] Validate recovery after Warden restart and explicit user-cancel shutdown.
- [ ] Complete the remaining adversarial network acceptance matrix; preserve
  changes and clean up worktrees.

## Current behavior and decisions

Without the explicit SBX executable/gateway configuration, `UnsupportedVerifier`
denies both create and runtime readiness. With `--sbx`, `--mitmdump` and
`--manage-network`, the implemented verifier supports the specific inspected SBX
0.42.1 profile described below. Registration, worker assertions and a running
listener alone cannot enable execution or provider traffic.

Create and runtime checks are distinct. Trusted host registration and immutable
gateway configuration establish sandbox identity; guest headers cannot select
it. Each sandbox has one owner principal. Chats and retained root processes in a
shared sandbox share that sandbox's authority during an active authorized lease;
this is not per-chat or per-process isolation.

The control endpoint is a private Unix socket, with no TCP listener or CORS.
Existing VM controller ports, firewall, credentials and state are unchanged.
The new contexts have an empty repository allowlist; GitHub operations remain
unapproved pending a scoped human-review UI/API.

## Implemented control contract

`host/warden/sbx.py` accepts one newline-delimited JSON message per connection.
The host-only state directory is 0700 and socket 0600. This is a trusted host
account boundary, not protection against hostile processes under that account.

Worker requests use `{version:1,operation,context}`. Context contains exactly
`projectID,sandboxID,runtimeName,generation,chatID,runID,principalID`, all nonempty
bounded identifier strings.

- `register`: idempotent durable binding; returns `ok:true,ready:false`.
- `check`: additionally requires `phase:create|runtime`; only trusted verifier
  code can return readiness.
- `begin` / `renew`: require runtime proof and an available host credential;
  grant one run at a time for 120 seconds, renewed by the worker every 30 seconds.
- `end`: revokes the matching lease and broker decisions before returning.

Ready begin/renew replies return `leaseSeconds`,
`apiKeyPlaceholder:warden-proxy-managed`,
`providerBaseURL:http://host.docker.internal:PORT/openai/v1`, and `proxyURL` for the
same gateway. They never return a real provider credential. Missing/false
readiness denies execution; worker-supplied ready/evidence fields are rejected.

Project/runtime/principal are immutable. A generation can rotate after the prior
run ends or expires. Retired generations are durably rejected; rotation clears
leases/decisions, rotates gateway capabilities, preserves policy and the same
owner's configured memory credential, and requires new gateway binding/proof.
Gateway ports and runtime UUID pins survive controller restart. Restart discards
leases, decisions, gateway capabilities and memory-only API keys, and requires
fresh proof. An explicitly configured host Codex credential file remains an
available credential source after restart, but cannot restore a lease by itself.
Failed manifest persistence poisons readiness until restart/reconciliation.

Host launcher operations `bindGateway`, `gateway`, `configureProvider` and `proxy`
are separate from worker operations. A fixed gateway capability authorizes only
that binding's proxy messages. It cannot approve requests, configure credentials,
assert readiness, change policy or access existing VM management/clipboard APIs.

## Managed startup

Use a separate short private state path, never the existing VM `.local`.
Install the pinned requirements in a worktree-local Python 3.12+ environment:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install --require-hashes -r proxy/requirements.lock
PYTHONPATH=host .venv/bin/python -m warden.sbx \
  --state /tmp/warden-sbx-private \
  --sbx /opt/homebrew/bin/sbx \
  --mitmdump /absolute/worktree/.venv/bin/mitmdump \
  --manage-network \
  --codex-auth-file /absolute/private/auth.json
```

The auth argument is a file path, never a token. It is optional when a trusted
host caller configures a binding's API key privately through `configureProvider`.
Omitting the SBX/gateway configuration starts a deliberately unready controller.
The standalone `warden.sbx_gateway` launcher is also available for adapter tests;
starting that listener alone does not establish runtime readiness.

The requirements lock scopes the Linux helper to Linux and adds the matching
pinned macOS helper/hash. Existing Linux pins are unchanged. Tests used Nix Python
3.13.15 in this worktree's ignored `.venv`.

## Managed verification and gateway boundary

`SbxCliVerifier` checks exact CLI/daemon version 0.42.1, disabled SSH forwarding,
`proxy.sandbox=direct`, an empty local MCP server inventory, no global network
grants and implicit default denial. It requires the shell template digest
`sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919`.
Runtime checks inspect actual inventory, durably pin its daemon UUID, reject kits
and reported host mounts, and accept only that sandbox's exact gateway rule.

The reviewed worker creates a mountless shell with `--deny-network '**'`,
`--no-share-skills` and the pinned template, without host hooks or MCP/env/volume
additions. Managed checking starts a dedicated loopback gateway and verifies its
HMAC challenge. It adds the exact scoped `localhost:GATEWAYPORT` allow while the
bootstrap deny remains active, then removes only the scoped bootstrap deny and
checks the resulting rules plus denied destinations. It does not change global
policy, daemon settings or another sandbox's rules. On failure it attempts to
restore scoped denial for an already pinned owned runtime and returns unready;
the worker stops a runtime whose readiness cannot be verified. Unexpected
versions, schemas and permissions fail closed.

Host policy evidence caches for at most two seconds; gateway health is checked
on every proof. Stopped runtimes can pass policy checks for safe resume. Preview
availability separately requires the worker's current runtime/port inventory.
Gateway children exit when their controller parent disappears and on normal
controller close. Loss of proof, lease expiry or `end` denies new broker use.
Previously authorized decision IDs cannot reactivate in a later lease.

The gateway uses regular mitmproxy mode, lazy verified upstream TLS, no raw TCP
or WebSockets, no Apple update exception and no Linux vsock/firewall heartbeat.
CONNECT enters inspected TLS rather than opaque forwarding. Policy and active
lease checks precede gateway DNS resolution. Reverse provider routing fixes the
upstream authority and strips guest auth/cookies. Browser Origins are rejected,
no CORS headers or control credentials are exposed, and missing configuration
closes the gateway. Its bounded local HMAC health response bypasses broker audit
recursion; ordinary traffic still requires proof and a lease.

SBX inspection does not expose every resource capability, so this design also
trusts the reviewed worker creation contract. SBX 0.42.1 reports an internal MCP
gateway even with no registered servers. The verifier requires an empty server
inventory; a live guest probe could not resolve its internal hostname under the
scoped policy. That observation does not prove every hidden host channel absent.
The checks are bounded configuration/runtime evidence, not universal containment
against a compromised host, daemon, hypervisor or implementation vulnerability.

## Host credential handling

A binding-specific API key supports exact HTTPS POST `/v1/responses` and
`/v1/chat/completions` at `api.openai.com`. The optional Codex login source maps
only `/v1/responses` to HTTPS `chatgpt.com/backend-api/codex/responses`. The latter
route and account header follow the pinned official Codex 0.154.0
[base URL](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/model-provider-info/src/lib.rs),
[authentication headers](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/model-provider/src/auth.rs)
and [Responses path](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/model-provider/src/provider.rs).
A binding API key takes precedence over the optional host login source.

`CodexCredentials` accepts a private, owner-held regular file without following
symlinks, bounds its size and validates access-token structure/expiry. It reads
fresh access/account fields for host-side injection only after the verified
lease and exact route/decision checks. It never returns refresh/ID tokens or
performs token refresh. Missing/expired login denies begin/renew and credential
use. The guest app-server uses a custom Responses provider with the placeholder
and verified gateway URL; no real external-auth token is passed into it.

Known exact credential echoes in provider response headers/bodies are rejected
before guest delivery. Retained flows have injected auth/account headers cleared.
Provider responses remain buffered with the existing 16 MiB limit; gateway
chunk streaming is not implemented. These protections are not general secret
exfiltration detection or support for arbitrary providers.

## Validation record — September 11

The coordinator's latest complete regression run passed **193 tests in 34.471
seconds**:

```sh
PYTHONPATH=host:tests .venv/bin/python -m unittest discover -s tests -p 'test_*.py'
```

Coverage includes binding/generation identity, lease expiry/revocation, malformed
control requests, verifier failure states, credential source validation, real
Unix RPC flows and real mitmdump denial of HTTP/CONNECT/raw protocols. A module
entrypoint regression covers the stable shared `NetworkProof` type. Desktop
fixtures now mock sufficient disk space and separately test low-disk refusal;
production disk checks are unchanged.

The coordinator verified these live integrated behaviors:

- Codex inside `wc-adf5a50ead0a6168413b39c1` built a counter, explicitly called
  `preview_attach`, and returned `http://127.0.0.1:62820`. Browser interaction
  changed the counter from 0 to 1 using the host Codex login through the gateway.
- Fresh and shared chats completed successfully, with separate provider thread
  IDs. Shared chats retained their intended shared sandbox/workspace behavior.
- After 90 seconds idle, the counter sandbox stopped and the browser showed the
  stopped state. The original chat resumed the same provider thread and restarted
  its preview.
- Killing Warden during an active 90-second sleep caused the actual sandbox to
  stop after **12.48 seconds**. The chat showed an error and retained its history.
  This is a measured failure case, not a guaranteed universal shutdown bound.
- After Warden restarted, the original chat recovered its provider thread and
  existing files, and the same stable preview URL `http://127.0.0.1:62820` became
  available again.
- Clicking UI Stop during an active 60-second sleep with the updated worker
  stopped the actual sandbox and all preview states. The chat reported context
  cancellation and retained its history. All three owned test sandboxes were
  stopped at the end of acceptance testing.

Controlled mountless guest probes with proxy environment variables removed
reached only the explicitly allowed host listener. An adjacent host port and a
direct external IPv4 destination were denied; tested external DNS names were
denied. An IPv6 connection reached a proxy-local socket then EOF, not a successful
external connection. These samples and the internal MCP DNS denial are bounded
observations; the full adversarial acceptance matrix remains separate work.

After the first counter demonstration, a read-only credential persistence check
scanned every regular nonsymlink file in `/tmp/warden-sbx-live`, including logs
and binary SQLite files. The current host access, refresh and ID token values
were held only in scanner memory, never printed, copied to an artifact or sent
to the guest. Across **41 files / 244,281 bytes**, each token had **zero exact byte
occurrences**, with zero matching files.

The point-in-time audit contained four `sbx.provider.authorized` events for
`chatgpt.com` and four upstream responses, all HTTP 200; four egress allowances,
four external-request metadata records, seven DNS-policy events, one run start
and two renewals. All lines parsed. Three earlier proxy errors remained recorded.
This scan does not cover encoded values, memory/swap or every exfiltration path.
Guest auth/env inspection was not performed during idle testing because SBX exec
can auto-start a stopped runtime. No runtime/process/policy mutation was part of
that scan.

## Remaining work and limits

Normal completion intentionally preserves background processes such as previews.
A retained root process can make new requests during a later active lease in the
same sandbox; revoking one run is not per-process isolation. Explicit user
cancellation revokes the lease and stops the entire sandbox, and the coordinator
validated that path through UI Stop. Normal completion continues to preserve the
sandbox for previews and shared workspace use. Neither revocation nor shutdown
undoes a request already delivered upstream.

Complete the remaining adversarial network/host-channel matrix and document
exact tested cases rather than claiming universal containment. Browser preview
JavaScript runs outside the SBX egress boundary. Multi-provider configuration and
scoped GitHub approval remain deferred. The coordinator owns final source
preservation, live acceptance consolidation and worktree cleanup. No commit or
push has been performed by this track.


## Main integration — September 11, 2026

The delivered source is now committed in the main integration branch. See [merge record](warden-main-merge.md) for scope, validation, and preservation details; earlier uncommitted-status notes are historical.
