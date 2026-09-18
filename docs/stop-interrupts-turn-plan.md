# Stop interrupts the agent's turn

Status: started 2026-09-17 on branch `feat/stop-interrupts-turn` from origin/main b0ceb42.

## Objective

The chat's Stop button (and `/stop`, the TUI's `/stop`) interrupts what the
agent is doing — its thinking, the tool it is running — and hands the chat
back to the owner, the way Escape does in Claude Code or Codex. The
sandbox stays up and the agent's session stays resident, so the next
message is answered at once. Stopping the workspace (the panel's Stop,
Archive, resize) still stops the sandbox; that is the owner asking for the
sandbox, not for the turn.

## What exists

- `chats/engine.go` `Stop`: marks the chat `stopping`, tombstones the run
  on the runner (`cancel`), cancels the run's context, releases sibling
  sessions, then calls the runner's `stop` op until the sandbox is free.
  The runner's `finishManagedRun` stops the whole guest on an explicit
  cancel because killing the host `sbx exec` cannot prove the guest's
  descendants died. So every Stop cold-stopped the VM/pod and the next
  message paid a boot plus a `thread/resume`.
- `chats/engine.go` `turn`: a `turn/completed` with status `interrupted`
  is an error, which ends the run with a cancel.
- `agent/claude.go`: translates Codex's app-server protocol to Claude
  Code's stream-json control protocol; no `turn/interrupt`.
- Codex's app server has `turn/interrupt` (`threadId`, `turnId`); the turn
  then completes with status `interrupted`. Claude Code's control protocol
  has the `interrupt` control request; the query aborts and a `result`
  follows.

## Steps

1. `agent/claude.go`: `turn/interrupt` → `control_request` `interrupt`;
   the next `result` completes the turn as `interrupted` (no error event).
2. `chats/engine.go`: `Stop` asks the run's live agent to interrupt the
   turn and waits for it to end; the turn loop treats a requested
   interruption as a clean end; the chat becomes `interrupted` with the
   session resident. Idle sessions are released, not killed. The old
   cancel-and-stop path remains the fallback for a run without a live
   agent (still booting) or an agent that does not answer the interrupt
   in time.
3. `resume`: a chat marked `interrupted` may take its next message on the
   live session.
4. Tests in `chats/engine_test.go`, `chats/resident_test.go`,
   `agent/claude_test.go`; feature map row.

## Key decisions

1. Interrupt at the protocol level, not by killing processes: both agents
   implement it, and it is the only way to leave the guest running with
   nothing orphaned.
2. The fallback keeps the old semantics rather than leaving a run hung:
   an agent that ignores the interrupt for 10 s (`interruptGrace`) is
   cancelled and its sandbox stopped, as before. A run whose agent is not
   up yet (the sandbox still preparing) has nothing to interrupt and takes
   the same path; a live agent without a turn (session starting, or
   between turns) is ended the way an idle release ends it, with no
   cancel tombstone, so the sandbox stays.
3. Stop on a chat that is not running is a no-op: an idle resident
   session is worth keeping for the next message. A chat queued with no
   run is taken off the queue without touching the runner.
4. `Stop` decides how to stop from the run (turn in flight, no turn, idle,
   no run) and then marks the chat in one store update that checks the
   status agrees; the two are read apart, so a run ending or resuming in
   between is retried. The run's `interrupting` flag is set before the
   mark, so the Codex steering tick (which fails the run when `attempt`
   finds the chat not running) stands down instead.
5. `turn` treats any end of a turn whose interruption was requested as
   the stop done (interrupted, or completed when the turn was ending
   anyway) and marks the chat `interrupted`; `resume` no longer ends a
   session on `interrupted`, so the next message runs on it.
6. Workspace-level stops (`stopChats`: the panel's Stop, Archive, a
   restarting resize) still end the sessions, via `endSession` after the
   turn is interrupted, and `StopEnvironment` now waits out the runner's
   run cleanup (`stopSandbox`) the way the chat's Stop used to.

## Progress log

- 2026-09-17: implemented steps 1–4. `go vet`, `go test ./...` (chats
  also with `-race`; `TestAttributionAndTypingIndicators` has a
  pre-existing racy `now` closure the detector sometimes reports), web
  build and tests pass. Not yet exercised against a live agent: the
  Claude Code abort result's shape is taken as "whatever result follows
  the interrupt", so any result ends the turn as interrupted; if the CLI
  sends none, the 10 s fallback applies.
