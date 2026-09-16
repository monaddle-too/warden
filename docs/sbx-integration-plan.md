# Warden enforcement plan for local SBX chats

**Superseded ownership direction:** [Warden platform plan](warden-platform-plan.md) is the current plan. Warden now owns chats and SBX orchestration; Panta work is deferred. Retain this document as implementation history and technical evidence.

Implementation checkpoint (September 11, 2026): the first local chat milestone is implemented and live-tested. See [current SBX implementation and validation](sbx-implementation-progress.md). The plan below is retained as the original planning record.


Prepared September 11, 2026 against Warden `5598776` on
`codex/warden-handoff-plan`. Objective: identify the smallest viable enforcement
path for the project → arbitrary chat → local SBX → explicitly attached web
preview flow in the workspace `HANDOFF.md`.

This is a source-grounded implementation plan, not a completed integration.
The coordinator found no installed SBX executable on this Apple Silicon/macOS
15.6 machine. No live SBX networking claim below has been validated. The existing
Warden VM pair, credentials, and networking were not inspected or modified by
this track. Editable documents and distribution work remain deferred.

## Durable progress and decisions

- [x] Inspect ingress, identity, policy, approvals, revocation and failure paths.
- [x] Review architecture, protected-development and validation records.
- [x] Check current official Docker policy/credential documentation.
- [x] Run isolated existing host policy/server fixtures: 45 passed.
- [x] Specify a bounded feasibility gate, implementation order and attack matrix.
- [ ] Prove an enforced gateway-only route on an installed, pinned SBX version.
- [ ] Implement the selected ingress and trusted sandbox identity contract.
- [ ] Run live adversarial tests and the integrated product demonstration.

Use SBX as the execution environment and preserve Warden's GitHub approvals,
restricted destination policy, revocation and metadata-only audit. Do not start
an AI runtime if enforcement readiness is absent. Keep the current macOS/Linux
VM deployment intact. Do not repurpose its control socket, port or state directory
for concurrent SBX contexts. A proxy environment variable is a connectivity
convention; trusted code outside the guest must make alternatives unreachable.

## What existing Warden code actually provides

| Area | Evidence and consequence |
| --- | --- |
| Interception | `proxy/warden-proxy.service` starts mitmproxy in **transparent** mode. `proxy/firewall.nft` redirects `lan0` TCP 80/443 to 8080 and permits it only with DNAT connection state. This is not an existing HTTP upstream-proxy endpoint that SBX can simply use. |
| Network boundary | The native launcher supplies the macOS VM a private NIC; Linux drops forwarding, IPv6 and other traffic. The guest cannot change the trusted appliance firewall. None of these NIC/firewall guarantees automatically apply to SBX. |
| Policy RPC | `proxy/addon.py:rpc` connects through Linux-only vsock 7000 to the protected Unix socket. `host/warden/server.py:internal` trusts that transport and can return an authorized GitHub credential. Never expose that RPC to SBX or a browser, even behind a guest-held bearer token. |
| Identity | `Engine` in `host/warden/core.py` owns one policy, token, audit and SQLite database. Requests/grants/decisions have no sandbox/project columns; request fingerprints identify content, not originating sandbox. A new ingress must select a trusted context before calling Engine. |
| Existing projects | `environments.py:project_create` requires a repository and provisions a fresh macOS-oriented state. CLI startup assumes one desktop controller on 18765. It is not the app's new repository-optional Project or independently reusable Sandbox model. |
| Guest transport | `guest.py` pins a macOS VM identity and SSH key and uses a fixed Linux relay. Leave this path for the existing VM. The app worker should execute through SBX, not adopt it as a fallback. |
| HTTP policy | `Guard.request` validates hostname, authority, SNI and public DNS answers, pins IPv4 upstream, and rejects unsupported transports/headers. It denies policy/audit failures. GitHub credentials supplied by the guest are removed and replaced only after approval. |
| Credentials | `Engine.set_token` supports GitHub PAT/OAuth tokens held in memory. Ordinary external requests retain incoming credentials. Warden currently has no AI-provider credential broker; forwarding a guest OAuth token is not host-only credential handling. |
| Existing exception | `Guard.tls_clienthello` has opaque TLS exceptions for exact Apple update hosts. They exist for the macOS guest. Disable these in the new Linux SBX profile; this milestone does not need them. |
| Audit | Persistent bodies are omitted; Git/PR review content is bounded ephemeral memory. Maintain that contract when adding identities, provider flows and readiness events. |

`docs/validation.md` records successful historical Linux-namespace and real-VM
tests, and an unresolved kernel/storage anomaly. Those results establish useful
regression coverage for the existing path, not an SBX validation result.

## First gate: select a topology using evidence

The smallest candidate worth testing is **one Warden-only network destination
per SBX**, with a dedicated trusted gateway listener for that runtime. Applications
inside SBX speak explicit HTTP proxy protocol to this endpoint. The SBX host
enforcer permits only that endpoint and denies all direct internet, other host
services and other sandboxes. Warden resolves destination names and applies the
existing inspected HTTP(S) policies before forwarding. Removing guest proxy
settings then causes requests to fail.

This is a proposed composition, not a documented turnkey feature. Docker documents
host-service access through `host.docker.internal`, translated to a specifically
allowed `localhost:PORT`, and explicit loopback preview publication. Verify that
raw TCP to the gateway and nested proxy CONNECT reach it without the SBX proxy
rewriting their intended authority. Publication is an independent inbound route;
it must not add a guest-to-host control API permission.
[Docker development workflow](https://docs.docker.com/ai/sandboxes/workflows/development/)

Start a disposable sandbox without user code, credentials or a shared workspace.
First establish the effective deny policy, then permit only its assigned gateway
endpoint, then test connectivity and rejection. The gate must answer:

1. Can SBX apply effective **gateway-only** policy before any hostile startup
   process runs, including when a sandbox or daemon restarts?
2. Can a sandbox reach only its own gateway listener? Reject other listener
   ports even when the requester guesses their numbers or forges IDs/headers.
3. Does the SBX resolver refuse arbitrary outside names without emitting their
   query labels upstream? Gateway-based HTTP must not leave a DNS escape path.
4. Can the gateway terminate CONNECT for inspected TLS, require authority/SNI
   agreement and reject non-HTTP bytes without forwarding them upstream?
5. Can the controller verify policy/readiness and preserve a deny state through
   crashes, identity replacement, publication-triggered startup and daemon restart?

Docker's current local-policy docs describe global, sandbox and kit rules;
built-in kits can add rules. A deny-all preset alone is insufficient evidence
after creation. Inspect effective rule origins, including organization governance,
and run policy checks plus actual packet tests. Use a reviewed minimal kit with
no automatic internet grants. Do not reset a user's global policy: reset stops
running sandboxes. A dedicated SBX configuration/daemon instance would be useful
only if the installed version actually supports that separation.
[Docker local policy](https://docs.docker.com/ai/sandboxes/governance/access-controls/local/)

An alternative is an external host/appliance restriction over all SBX outbound
traffic, with Warden the only possible egress path. This has a larger host-network
integration cost and is not selected by this plan. If the gateway-only gate fails,
record the exact failure and investigate that alternative before claiming secure
execution. Do not silently loosen the policy to make a demonstration work.

Configuring Warden merely as SBX's upstream proxy is not the gate: SBX also allows
non-HTTP TCP by destination rules. Current upstream settings are experimental,
have direct/no-proxy exceptions and separate daemon/sandbox scopes. Proxy choices
can change on sandbox restart or with system/PAC changes. Freeze and verify
effective configuration rather than inherit the host shell or OS defaults.
[Docker upstream proxy](https://docs.docker.com/ai/sandboxes/configuration/upstream-proxy/)
[Docker isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)

## Minimal implementation after the gate passes

1. **Add an SBX ingress adapter without replacing the existing transparent mode.**
   Reuse Guard's inspected request handling and Engine authorization. Add a regular
   forward-proxy listener, with CONNECT permitted only as an entry into inspected
   TLS, never as an opaque upstream tunnel. Disable raw TCP, WebSockets, upgrades
   and Apple passthrough for this profile. Explicit-mode mitmproxy CONNECT hooks,
   address selection, certificate trust and DNS behavior need new integration
   tests; changing `--mode` alone is not sufficient. If the adapter runs on macOS,
   give it a private Unix-socket policy transport; do not emulate Linux vsock or
   assume nftables applies. If it stays in a separate Linux appliance, provide
   a narrow host-side data relay and its own private control transport/state.

2. **Bind identity to a trusted ingress, not agent fields.** The worker registers
   immutable `project_id`, `sandbox_id`, actual SBX runtime identity, generation,
   policy revision and assigned gateway endpoint through authenticated host IPC.
   The gateway accepts traffic only for that binding, and the SBX host policy
   prevents another sandbox reaching it. Header IDs and source addresses after
   SBX NAT are not authentication. An explicit-proxy bearer inside the guest can
   be stolen by root; at most it is a narrow sandbox capability, never a Warden
   admin credential or authority for another context. Use separate internal
   listener connections or authenticated host-held transport credentials if
   traffic crosses processes/VMs; never forward a guest-selected context value.

3. **Isolate authorization state per sandbox first.** A small registry can route
   each binding to a separate Engine/state directory and a shared trusted approval
   server. This reuses existing grant matching without a broad multi-tenant SQL
   migration. Project membership selects policy/credential configuration; duplicate
   credentials need not enter guests. Register repository-less contexts with an
   explicit empty repository allowlist so adding chats does not implicitly grant
   account-wide GitHub access. Do not call the current repository-required project
   provisioner. An alternative shared Engine requires sandbox identity in every
   lookup, fingerprint, review-cache key, grant, decision and audit path; do not
   mix requests first and retrofit scope later.

4. **Expose context-aware human review.** Display trusted project/sandbox labels
   beside existing exact requests, use context-specific approval/revocation APIs,
   and preserve global emergency disconnect. A request or decision identifier
   from one sandbox must be unusable in another. All chats sharing one sandbox
   share that sandbox's security authority; chat attribution is diagnostic since
   hostile root controls all processes there. Serializing turns does not make
   surviving background processes trustworthy representatives of a chat.

5. **Add host-held AI credential injection.** For the first selected provider,
   keep its real secret in trusted memory/store and inject only after Warden
   allows an exact provider authority/method/path class. Supply the guest only
   the placeholder required by its runtime. Strip untrusted provider auth fields,
   keep redirects from carrying credentials to another host, and clear credentials
   from retained flows. Reuse Warden's redactor/audit omissions. The candidate
   gateway route may bypass SBX's own forward-proxy credential injector, so it
   cannot assume that injector will authenticate provider requests. A composition
   using SBX host injection is acceptable only after injection order, credential
   isolation and Warden enforcement are proven together. Do not copy a real
   `.codex/auth.json`, provider token environment or OAuth response into SBX.

6. **Gate worker execution and resume.** Before starting the AI/runtime, require
   a current readiness result matching the actual runtime generation and policy
   digest, then create its bounded active-turn lease. Recheck on every create,
   resume, daemon recovery and port publication that can start a sandbox. Fail
   pending turns with an enforcement-unavailable state; never execute on the host
   or reuse a stale readiness file. Resolve/serialize lifecycle transitions for
   all chats sharing an environment. Scope broker permission to the sandbox,
   runtime generation, active run lease and one owner credential principal for
   the first local slice; deny provider credential use when no authorized lease
   is active. Reject a principal change in an existing sandbox until a separately
   validated transition exists. The worker's current stop-at-turn-end behavior
   also clears personal credentials; removing that stop requires proving broker
   revocation first. A hostile surviving process can use an active sandbox lease,
   so this does not create per-process or per-chat credential isolation.

Docker documents sentinel-based credential injection, with optional OAuth
passthrough that exposes real responses, and SSH-agent forwarding enabled by
default. Configure neither passthrough nor host SSH-agent forwarding for these
hostile-agent sandboxes. Credential storage may default to global scope; use
explicit runtime scope if relying on SBX's credential store.
[Docker credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/)

Use mountless workspaces for arbitrary chats. Disable shared writable host skills,
host MCP execution bridges and host lifecycle/credential commands derived from
agent-controlled files. SBX's Docker isolation does not constrain host-run MCP
servers. Confirm the created sandbox's actual resources, not only a template's
intended contents. Trusted, reviewed worker operations for attach/remove remain
narrow explicit capabilities.
[Docker isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)

## Failure, shutdown and recovery contract

Current Engine startup revokes all grants and stales pending requests; the token
starts empty. Disconnect persists a flag and revokes decisions. Guard checks
in-flight decisions roughly every 0.5 seconds; RPC has an eight-second timeout.
Its heartbeat attempts every three seconds, applying the Linux cutoff on control
failure. The native launcher pauses its macOS guest after more than 20 seconds
without a changing heartbeat, checked every three seconds. These are separate
mechanisms, not an instantaneous cancellation guarantee, and do not supervise SBX.

For the new path, gateway unavailability must leave the SBX host deny rules in
place, eliminating fallback regardless of worker liveness. Keep a persistent
registration/generation state outside the guest; restart all new gateway contexts
closed until policy and binding reconciliation finish. Revoke grants/decisions
on restart and identity replacement. A worker crash must neither remove the
egress boundary nor auto-replay an interrupted turn. A gateway's failure to query
Warden, validate policy or write required audit denies new requests and closes
in-flight flows on bounded detection. Measure the actual cancellation delay.

The worker may stop a sandbox as defense in depth after enforcement failure,
but delayed stop is not the primary network boundary. Stop preserves disk and
ends processes. On resume, validate identity/configuration again and reconcile
preview mappings; do not replay external writes to restart a preview. Report
unavailable/stopped previews without retaining an iframe aimed at an endpoint
whose port may now belong to another service. Revocation cannot undo an upstream
request already committed; evidence must separate attempted dispatch from an
upstream side effect.

## Trusted browser and preview separation

Existing Warden uses `127.0.0.1:18765`, an explicit sessionStorage bearer token,
exact Host checking, exact same-origin checking on POST, no credential cookies,
no permissive CORS, and a CSP preventing framing. Keep the approval page separate
and link to it from the app; do not embed it beside sandbox content or transmit
its token through a preview URL, referrer or `postMessage` bridge.

For the app's localhost bearer origin and controller-published
`127.0.0.1:PORT` previews, pin exact schemes/hostnames/ports independently; a
hostname distinction is useful only if control APIs reject aliases/rebinding and
credentials stay out of ambient cookies. Do not whitelist preview origins on
control APIs. If cookies are later introduced, port separation does not isolate
them. Serve trusted assets only from trusted code. Browser preview traffic is
outside SBX egress enforcement; it must not be described as Warden-inspected.

## Adversarial validation matrix

Run every networking row against a disposable root-capable SBX and controlled
destination servers with unique request markers. Record the pinned SBX version,
template digest, effective rules/configuration, runtime IDs, actual destination
receipts, Warden metadata events and elapsed failure times. For denied traffic,
absence of a Warden event is not proof of blocking: check the destination receipt
or host-side packet evidence. Use synthetic credentials only. Preserve logs
without real tokens or bodies.

| Attack or transition | Required result |
| --- | --- |
| Approved dependency GET and AI HTTPS | Succeed through the registered gateway, matching Warden audit/context; provider streaming/cancel works within documented buffering limits. |
| Unset upper/lowercase proxy env; set no_proxy=*; custom socket client | Direct external traffic still fails; explicitly configured inspected gateway path still works. |
| IPv4 literal; alternative destination on port 80/443/8443 | No direct forwarding or alternate-port bypass. Public DNS authority is independently resolved and pinned by Warden. |
| IPv6 global/literal/mapped addresses and guest route changes | No IPv6 escape or special-address path; document every supported/blocked family. |
| DNS A/AAAA, TXT, arbitrary labels; external UDP/TCP 53; DoH/DoT | No arbitrary resolver/name exfiltration path. Gateway names resolve only as intended; approved provider behavior is distinguished from general DNS relay. |
| SSH, opaque TCP and TLS with non-HTTP bytes on an otherwise allowed port | No upstream data forwarding; CONNECT establishes inspected TLS only. Test Apple SNI specifically to prove exception disabled. |
| Missing/mismatched SNI/Host, encoded authority, duplicate headers, redirect to LAN/metadata | Deny invalid routing; no token attachment or private upstream connection. |
| Drop/change proxy CA and certificate-pinned clients | TLS fails with no direct fallback. |
| Guest root modifies routes/firewall/DNS; nested Docker bridge/host network/privileged container | Still subject to external SBX enforcement; no host Docker socket or independent internet route. |
| Forge project/sandbox headers; use another gateway port; reuse old generation or decision | No cross-context credentials/grants/audit attribution. Same sandbox legitimately shares authority across its chats. |
| Host gateway/control/app/other preview ports through host.docker.internal, localhost/IP aliases and LAN IP | Only registered gateway destination reachable outbound; no approval API, worker API, SSH agent, MCP host bridge or another sandbox's listener. |
| Credential echo/redirect, guest file/env inventory, real-token substitution attempt | Only synthetic sentinel visible in guest; injection restricted to authorized upstream. No credential in response, audit, history or preview URL. |
| End/cancel run while a hostile background process retains connections; switch owner principal | Expired lease cannot obtain new broker credentials; existing flows close within measured bounds. Principal switch is rejected. A later valid lease remains shared sandbox authority. |
| Kill gateway; stop Warden; lose control transport; stall heartbeat | New traffic fails and in-flight requests close within measured bounds. Direct external retry stays blocked. Worker cannot begin/resume work on stale readiness. |
| Audit failure/ENOSPC, malformed policy, context registration unavailable | No new authorization/forwarding; user sees enforcement failure. |
| Restart worker, gateway, SBX or daemon with existing sessions/mappings | Deny until registration/policy validation; old grants revoked; no automatic action replay or broadened rules. |
| Idle stop racing attach/new turn and two chats sharing sandbox | Lifecycle lock and all-chat leases prevent premature stop; publication cannot bypass start readiness. |
| Preview POST/fetch/iframe/postMessage toward control and approval origins; DNS rebinding | No authorized mutation/read or token disclosure; controls reject invalid Host/Origin and expose no permissive CORS. |

## Tests run in this planning slice

From this worktree, on September 11:

```sh
PYTHONPATH=host:tests python3 -m unittest test_core test_server test_milestone -v
```

Result: **45 tests passed in 6.548 seconds**. These use temporary Engine state,
synthetic tokens, ephemeral loopback fixture servers, mocked audit ENOSPC and
small synthetic recovery/signature files. They cover authorization/replay,
revocation/restart, host authentication/Origin, policy/DNS and independent project
state. They did not read real credentials, contact GitHub, touch live VM disks,
run the native launcher, exercise production mitmproxy or validate SBX networking.

## Remaining work and ownership handoff

The coordinator owns installing/establishing a disposable SBX prerequisite and
combining this plan with the app data-model/worker/preview tracks. No executable
startup recipe for an enforced integration exists yet; produce exact commands
only after pinning the installed version and selecting the topology through the
gate above. Product source edits, final integration tests, real local demo and
commits remain future actions. The coordinator preserved this plan uncommitted
in the original repository, verified the copy and removed the planning worktree.
No product code was changed in this track.

The largest blockers are effective gateway-only policy (including DNS and startup
rules), ingress compatibility, unforgeable sandbox binding, and host-only AI
credentials. App schema/UI work can proceed independently against a worker that
reports enforcement unavailable; enabling actual agent execution depends on all
four. The worker must provision the Linux arm64 agent/runtime artifacts required
by its existing pinned versions on Apple Silicon; the host macOS CLI is not a
substitute for an executable inside SBX. The existing VM and its known validation
limits remain unchanged.
