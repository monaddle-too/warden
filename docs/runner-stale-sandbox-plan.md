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
3. Live check on `~/.warden`: one warning at the first start, none at the
   next; `warden stop` exits 0; launchd's last exit code 0.

## Decisions

- The Kubernetes driver is untouched: its guests outlive the worker and
  its `Reconciler` settles them; only drivers whose guests die with the
  worker (SBX) take the stop path.
- The four pending bug-report drafts about the runner exit are left for the
  owner (`warden bugs pending`); they describe the pre-fix behaviour.
