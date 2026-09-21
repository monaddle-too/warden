# Agent stream loss: name the cause, log the stop, keep the sandbox

Status: started 2026-09-21 on branch `fix/agent-stream-loss` from origin/main 50b809b.

## Objective

When the chat service loses its stream to the agent mid-turn, the owner
sees why ("agent stream ended: read tcp …: connection reset by peer"),
the runner's log says what it did to the sandbox and why, and the
workspace is kept running so the next message resumes the thread in
seconds instead of paying a cold start.

## What exists

On 2026-09-21 at 16:57 UTC a GKE chat showed "agent worker disconnected"
while its reply was streaming. The diagnosis (memory
`warden-gke-agent-disconnect-2026-09-21`) needed the GKE audit log, the
node's containerd log, the policy audit and the agent's own transcript,
because:

- `agent/claude.go` `ClaudeStream` reads the runner's connection with a
  scanner and, on any read error, a line it cannot parse or a line over
  8 MiB, silently returns and closes the bridge pipe; `agent/rpc.go`
  then sees a clean EOF and reports "agent worker disconnected" whatever
  happened.
- `chats/engine.go` `run`: every run that ends in error is tombstoned
  with the runner's `cancel` op, and `sandbox/managed.go`
  `finishManagedRun` then stops the sandbox (the pod is deleted). A
  stream loss with a healthy agent and workspace cost a two-minute
  resume on GKE.
- The runner logs nothing for a `cancel`, a `stop`, an idle sweep or an
  enforcement stop; only "running" lines.

## Steps

1. `agent/rpc.go`: `ErrStreamEnded` wraps the reason the stream ended; a
   stream that knows its cause (`Cause() error`) is asked.
2. `agent/claude.go`: the adapter records why its read loop ended (the
   connection's error, an unparseable line with its first bytes, a line
   over 8 MiB, the worker closing it) and reports it through `Cause`.
3. `chats/engine.go` `run`: a run that ended because its stream ended is
   not tombstoned: the runner sees a plain disconnect and keeps the
   sandbox resident; the chat is `failed` with the cause named, and the
   run's end is logged either way.
4. `sandbox/managed.go`, `sandbox/control.go`: a log line for every
   sandbox stop with its reason (cancelled by the chat, permission end
   failed, renewal failed, stop op, idle sweep, enforcement) and for the
   `cancel` op.
5. Tests: the adapter names the cause (worker closed, unparseable line);
   an engine test that a stream cut mid-turn leaves the sandbox alone
   and names the cause.
6. `docs/feature-map.md` rows.

## Key decisions

1. A stream loss keeps the sandbox. The old rule ("a failed run is
   tombstoned so the runner stops its sandbox") guarded against a guest
   left with a live agent nobody controls; the runner already closes the
   agent's exec session when the stream ends, the same way an idle
   release ends it, and the run's permission is ended by
   `finishManagedRun`. Failures the agent reports (a failed turn, a
   protocol error) and the chat's own still stop the sandbox.

## Progress log

- 2026-09-21: plan written from the diagnosis.
