# Resident Claude sessions and background policy verification

Branch `codex/warden-resident-claude`, started 2026-09-15 from main e43cdf0.

## Objective

Claude turns on OVH still took 14 to 18 seconds from message to first response
after the Claude executable stopped being copied per turn. Measured on the
server for a turn starting more than 20 seconds after the previous one:

| Step before the model is asked | Time |
|---|---|
| Policy verifier: 14 sequential SBX CLI commands, cached 20 s | 5.3 s |
| Gateway CA copy plus `update-ca-certificates` in the guest | 2.2 s |
| mkdir, Claude binary check, spawning the CLI (3 guest round-trips) | 1.1 s |
| Claude CLI startup and session resume | 1.0 s |

Model responses then take 2 to 11 seconds each. Owner decisions: the verifier
must run separately from the chat path, and Claude chats should stay running
between turns instead of being started for every message.

## Decisions

- **Background verification (policy broker).** `SbxCliVerifier` gains a
  refresher thread that re-verifies every warm binding (active lease, or a
  `begin` in the last 10 minutes) every 20 s, holding only a verifier lock
  while the CLI runs, never the registry lock. Proofs last 30 s (the registry
  already accepts up to 30 s). The chat path (`check`/`begin`/`renew`) uses a
  fresh cached proof after confirming the gateway is still healthy; a stale or
  missing proof still verifies synchronously, so cold starts keep today's
  behaviour and nothing is ever admitted on an expired proof. A failed
  background verification clears the cache exactly as a failed synchronous
  one does.
- **Resident sessions (chat engine).** For Claude chats the run does not end
  when a turn completes. The CLI process, its lease and its worker run stay
  open; the chat reports `idle`, and the next message becomes another turn on
  the same process. The session ends after 10 minutes without a turn, when
  the chat is stopped or archived, when its model is changed, when the worker
  closes the stream (environment stopped), or when another chat needs the
  sandbox or one of the two engine run slots. Ending the session is the same
  clean path as a completed run today, so the sandbox idle rules apply after
  that. Codex chats keep starting a run per message.
- The run ID is assigned when a run actually starts, so a message that arrives
  just as a resident session expires never reuses a finished run ID.
- The runner and the UI are unchanged: a resident session is an ordinary
  active run with lease renewals, and the chat status values are the same.
- Not done here: installing the gateway CA once per guest (2.2 s per cold
  start, also paid by every Codex turn).

## Steps

- [x] Verifier refresher with tests; broker starts and stops it.
- [x] Engine: resident turn loop, message routing into a live session,
      release on stop/archive/model change/contention, idle timeout; tests.
- [x] Go vet and race suite, Python tests.
- [x] Release, deploy to OVH.
- [ ] Verify turn timing from the audit log after the owner's next Claude chat.

## Implementation notes

- `host/warden/sbx_verifier.py`: `verify` first looks for a fresh cached proof
  and confirms gateway health (an HTTP probe, no CLI); only then does it take
  the verifier lock and inspect the host. `refresh_once` re-verifies warm
  bindings with `force=True` so the old proof stays valid until replaced; a
  failure clears the cache and applies the deny rule exactly as before.
  `start_refresher` runs it every 12 s (a full inspection takes about 5 s;
  proofs still last 20 s). The registry records `last_begin` and stops the
  thread on close. Tests: `RefresherTests` in `tests/test_sbx_verifier.py`.
- `chat/internal/chats/engine.go`: the run assigns its run ID when it starts,
  so a message that arrives as a resident session expires never reuses a
  finished run ID. After a completed turn on a resident provider the run calls
  `settleTurn` (approvals expire, streaming flags clear, status idle or queued)
  and `awaitMessage` (250 ms poll of `resume`, frames still drained, ends on
  release, idle timeout, run cancel, chat stop/archive, or worker closing the
  stream). `Serve` no longer counts a chat's own live session as busy, and
  asks an idle session to yield when a queued chat needs its sandbox or one of
  the two run slots; the taking-over chat retries a busy `prepare` for up to
  15 s while the worker finishes the released run. `Stop`, `StopEnvironment`,
  `DeleteEnvironment`, archiving and `ConfigureAgentAndRelease` (used by the
  `/agent` route) release sessions first. Steering is now a provider list
  (`SteeringProviders`, default Codex) beside `ResidentProviders`
  (default Claude). Tests: `chat/internal/chats/resident_test.go`.
- Local Python run: the SBX modules pass; 39 suite errors are the local
  sandbox refusing socket binds and are identical on the untouched worktree.

## Deployment (2026-09-15, release 0b1d270)

- Staged `/opt/warden/releases/0b1d270` with Linux binaries built from this
  branch and `chat/web/dist` copied from 38d3a9c (no frontend change). Image
  `warden:0b1d270` built on OVH; the SBX Python modules were run inside that
  image (57 tests) with the one pre-existing failure
  `test_module_entrypoint_managed_gateway_accepts_shared_proof_type`, which
  fails identically in the 38d3a9c image.
- Switched with no chat running; policy, runner and chat recreated on the new
  image. Root 200, signed-out `/api/state` 401, edge and SBX active.
- Rollback: `ln -sfn /opt/warden/releases/38d3a9c /opt/warden/current` and
  `docker compose up -d` there.

## What to expect

- First message on a Claude chat after a quiet spell: unchanged cold start
  (verifier, CA install, agent start) of roughly 9 s plus the model.
- Every following message within 10 minutes: no run start at all; the message
  becomes a turn on the live process, so latency is the model's response time
  plus tool time. Between turns the runner keeps renewing the lease and the
  broker's refresher keeps the proof fresh, so neither the turn start nor the
  20-second renewals run the SBX CLI on the chat path.
- The chat shows idle between turns as before; the environment stays running.
  Stop, archive, model change, environment stop/delete, another chat needing
  the sandbox, or a third chat needing a run slot end the session cleanly.
