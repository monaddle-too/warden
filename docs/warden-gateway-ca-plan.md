# Host-wide gateway CA with once-per-guest installation

Branch `codex/warden-gateway-ca`, started 2026-09-15 from main 3099468.

## Objective

Every stream start copied the gateway's CA into the guest and ran
`update-ca-certificates`, 2.2 s on the one-CPU OVH guest, paid on each Claude
cold start and on every Codex turn. The CA was also generated per sandbox
binding, so a guest that moved between bindings had to trust a new one.

## Decisions (owner)

- One gateway CA per policy installation, under `<state>/gateway-ca/`, used by
  every in-process gateway. Per-host leaf certificates are still minted per
  gateway: each gateway derives its own leaf cache from the shared key.
- Rotation yearly at service startup (`--gateway-ca-max-age`, default 365
  days) or on demand with `warden-policy rotate-gateway-ca --state DIR`, which
  requires the service lock so the service must be stopped first. The old
  directory is retired beside the new one, never deleted. A running service
  does not rotate in place: that would break resident sessions whose guests
  trust the old CA mid-run, so an aged CA waits for the next restart.
- The runner installs the CA once per guest and records its SHA-256 in the
  sandbox's managed state (`ProxyCA`), like the Codex bundle and Claude
  executable flags. The prepare step's existing `mkdir` round-trip now also
  reports whether the guest still holds the file, so a lost copy is
  reinstalled without an extra guest command. A changed fingerprint
  (rotation) reinstalls.
- Per-binding `gateway/ca` directories are no longer created or read; existing
  ones are left in place for rollback to fcf98b6.
- Future: a pinned SBX image with the runtimes preinstalled can carry this CA
  and remove the guest install entirely.

## Steps

- [x] Policy: `HostCADir`, `PrepareGatewayCA`, `RotateGatewayCA`, `Derive`,
      registry-owned CA, pool uses it, `begin` returns it.
- [x] Command: `--gateway-ca-max-age` flag, `rotate-gateway-ca` subcommand.
- [x] Runner: `RuntimeDriver.InstallCA`, `ensureProxyCALocked`, prepare
      reports the guest file, state reset with the runtime.
- [x] Tests: shared CA across bindings and gateways, rotation by age and on
      demand with retired directories, once-per-guest install with rotation
      and loss; go vet and race suite.
- [x] Release, deploy to OVH, confirm the CA directory.
- [ ] Confirm the one-time reinstall from the runner state after the owner's next runs.

## Deployment (2026-09-15, release ea1d822)

- Main had gained e1c35da (verifier off the chat path, two-minute refresh)
  after this branch started and it was already deployed, so main was merged
  here first (README paragraphs from both sides kept). Vet and the race suite
  pass on the merge.
- Release `/opt/warden/releases/ea1d822`: four Linux binaries built from the
  merge, `chat/web/dist` copied from e1c35da, image `warden:ea1d822` built on
  OVH (selfcheck now exercises `PrepareGatewayCA`). The never-switched
  03f91d7 staging directory and image were removed.
- Switched with no chat running; policy, runner and chat recreated. Root 200,
  signed-out `/api/state` 401, edge and SBX active. The policy service
  created `/var/lib/warden/policy/gateway-ca/` (0700, two 0600 files),
  subject `O=Warden, CN=Warden Gateway CA`, valid ten years, rotated by age
  at startup after a year.
- `rotate-gateway-ca` smoke-tested in a throwaway container: rotates twice,
  retires both old directories, and refuses while the service lock is held.
- Rollback: `ln -sfn /opt/warden/releases/e1c35da /opt/warden/current` and
  `docker compose up -d` there; the per-binding CA directories it reads are
  untouched, and guests reinstall that CA on their next run.

## Expected effect

- Every guest reinstalls the CA exactly once on its next stream start (the
  fingerprint changed from its per-binding CA to the host CA), then never
  again until rotation. Codex turns and Claude cold starts lose the 2.2 s.
