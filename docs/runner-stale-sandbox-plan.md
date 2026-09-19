# Runner: a stale "creating" sandbox record must not fail the start

## Objective

`warden stop` / `warden restart` reported a failed stop on this Mac, and the
launcher drafted a "warden-runner exited unexpectedly (exit status 1)" bug
report at every start. The runner, not the stop, was at fault: a sandbox
registered as *creating* whose creation never happened (the sbx daemon had
lost its Docker session at the time) stayed in `managed-v2.json`, and every
restart tried to `sbx stop` a name sbx does not know, failed, and exited 1.
The launcher saw a service die, drafted the report and returned nonzero.

## Steps

1. **Done on main (6e16716).** A refused startup stop marks the sandbox
   failed instead of taking the runner down.
2. **This branch.** A record that was only *creating* (nothing created) is
   reset to never-created when its stop is refused: the runtime name is
   removed best effort, `Creating` cleared, state `stopped`; the next run
   creates the sandbox afresh. A *created* sandbox keeps its workspace and
   stays marked failed, as in step 1. The sbx driver's `Stop` error carries
   sbx's stderr, so the log says "sandbox 'x' not found" rather than
   "exit status 1".
3. **Done** (2026-09-19, deployed to `~/.warden/release` as 429d1ae). Live
   on `~/.warden`: the first start logged one warning ending in sbx's own
   "sandbox 'wc-6f5f…' not found", the record went to `stopped` with
   `Creating` cleared, the next start logged nothing; `warden stop` exited
   0 twice and launchd's last exit code is 0.

## Progress

- 429d1ae: the reset, the sbx stderr, the test
  (`TestRestartResetsAnInterruptedCreationWhoseStopIsRefused`); full Go
  and web verification green; merged to main.

## Remaining

- Separate and still open: the first launch of a freshly built binary
  under launchd was killed once (`OS_REASON_CODESIGNING`), so `warden
  start` right after `deploy-local.sh` printed "exited during startup" and
  exited 1 while launchd respawned the service 10 s later. Swapping the
  `~/.warden/release` symlink between two already-run builds and
  kickstarting did not reproduce it, so it is tied to a fresh build's
  first spawn. Options: sign releases with a stable identity, or let
  `awaitReady` ride out one launchd throttle interval before reporting a
  start as failed.

## Decisions

- The Kubernetes driver is untouched: its guests outlive the worker and
  its `Reconciler` settles them; only drivers whose guests die with the
  worker (SBX) take the stop path.
- The four pending bug-report drafts about the runner exit are left for the
  owner (`warden bugs pending`); they describe the pre-fix behaviour.
