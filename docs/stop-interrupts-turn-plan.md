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
   an agent that ignores the interrupt for 10 s is cancelled and its
   sandbox stopped, as before.

## Progress log

- 2026-09-17: plan.
