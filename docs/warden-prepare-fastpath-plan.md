# Prepare fast path

Branch `codex/warden-prepare-fastpath`, started 2026-09-15 from main c47a718.

## Objective

With a spare guest already booted, a new environment's prepare still made
three serial guest round-trips at about 0.4 s each: the guest report, the
port-publication check, and the keep-alive session. Owner asked for the
prepare optimizations.

## Changes (runner)

- `probeGuestLocked` runs the three round-trips concurrently and skips each
  one whose answer is already known:
  - a spare's guest report is taken once at boot (`spareSandbox.report`) and
    handed to the adopting sandbox (`pendingReport`), so adoption reads it
    without an exec;
  - a guest this worker just created or adopted is marked `fresh` and cannot
    hold a port publication, so the mappings check is skipped for that run
    only; later runs verify as before;
  - an adopted spare already has its keep-alive session, so none is opened.
- The keep-alive now starts alongside the report instead of after the
  repository step; it is attached to the sandbox as soon as it exists so a
  later failure in prepare still releases it.
- `reconcileRemovedLocked` is split: the publication verification runs in
  the probe, and `unpublishRemovedLocked` only touches the guest when a
  removed publication with a host port exists.

Net: an adopted spare's prepare makes no guest round-trip at all; an
existing guest's prepare makes its three in the time of one.

## Steps

- [x] Implementation and tests (fresh guest skips mappings, resumed guest
      verifies again without a second keep-alive, adopted spare needs no
      round-trip); go vet and race suite pass.
- [x] Release b78844b deployed to OVH (switched idle; root 200, signed-out
      API 401; runner, policy and chat recreated).
- [ ] Time a new chat (spare adoption) and a resumed chat from the audit log.
