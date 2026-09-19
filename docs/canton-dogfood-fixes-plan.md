# Canton dogfood: what broke cloning and building a large repository

## Objective

Clone `monaddle-too/canton` (Scala, sbt; a build that wants a lot of
memory) into a Warden workspace on the local install, change it, compile
part of it, and fix whatever Warden gets wrong on the way. Run on
2026-09-19 against `~/.warden` with an 8-CPU / 24 GB workspace
(`warden chat new --cpus 8 --memory 24g --network open`).

## What broke, and the fixes (branch `fix/runner-retained-limit`)

1. **The runner's `--retained` ceiling.** The first send failed with
   "worker has 32 retained sandboxes" (the deployed release predated
   78adb90), then, on 1658a41, with "…all bound or running": all 32 stopped
   workspaces were bound to old chats, so nothing was retirable. Raising
   `sandboxes.keepStopped` to 64 killed the runner at startup ("invalid
   worker limits"): `runnersvc` kept its own ceiling of 32 on `--retained`
   while the configuration accepts any value ≥ 1. Fixed: the ceiling is
   `maxRetained` (1024) and the refusal logs the values.

2. **The JVM could not reach the gateway.** SBX presets `JAVA_TOOL_OPTIONS`
   (and the proxy variables) to its own proxy, `gateway.docker.internal:3128`.
   Warden's `env` prefix replaces the proxy variables for the agent's
   process tree but left `JAVA_TOOL_OPTIONS`, and the JVM reads only that,
   so sbt got 403 on every artifact while `curl` beside it succeeded (the
   agent diagnosed this itself and wrote a wrapper). Fixed:
   `proxyEnvironment` in `sandbox/runtime.go` renders the JVM proxy
   properties for the gateway's host and port into `JAVA_TOOL_OPTIONS` for
   both agent launches (`TestProxyEnvironmentCoversTheJVM`). The gateway's
   CA is already in the guest's JVM trust store.

3. **Downloads above 16 MB were refused.** The gateway buffers every
   non-streamed response to inspect it and answered 413 ("response exceeds
   inspection limit") on a 50 MB jar from Maven Central (semanticdb-scalac;
   the agent disabled semanticdb to get past it). Fixed: a response too
   large to buffer from a host that is neither a model provider nor a Git
   remote is passed through uninspected (`passthroughResponse`), the lease
   checked and an `http.response.started` audited before the first byte,
   an `http.response` with the delivered size and `complete` at the end,
   revocation watched per chunk; provider hosts and Git remotes keep the
   refusal (`TestGatewayLargeDownloadPassesThroughUninspected`).
   `docs/siem.md` records the audit shape.

## Also seen, not fixed here

- The stored GitHub sign-in had lapsed (401); refreshed with
  `gh auth token | warden login github --paste-stdin --replace`, which the
  policy service picks up without a restart.
- `warden chat send --wait` returned at the first pending approval once and
  hung until its timeout on the second attempt (after a service restart
  rotated the capability); `warden chat approve` then a poll of
  `GET /api/state` worked.
- Another session installed release alpha.14 on the shared `~/.warden` mid
  run, which lacked fix 1 and looped "invalid worker limits" until this
  branch was redeployed.
- The guest image ships Java 25; Canton targets Java 21. The small modules
  compiled anyway.

## Result

On the deployed build the agent requested repository access, cloned
canton (195 MB), committed a README note on `warden/readme-note` (not
pushed), fetched the sbt launcher from Maven Central and compiled
`wartremover-annotations`, `logging-entries` and `daml-adjustable-clock`;
spend $1.50 for two turns.

## Progress

- 2026-09-19: da9ea3d deployed to `~/.warden/release`; the same chat
  printed the new `JAVA_TOOL_OPTIONS` (the gateway's host and port), fetched
  the 21 MB semanticdb jar with a 200 (the live audit log holds the
  `http.response.started` / `http.response` pair with `passthrough`), and
  compiled `wartremover-annotations` with semanticdb on, the wrapper's
  proxy override removed. Merged to main 773730a.

## Remaining

- Nothing for the fixes. GKE and OVH do not carry them yet.
