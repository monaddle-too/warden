# A redelivered message ends its turn

Status: started 2026-09-18 on branch `fix/redelivered-message-turn` from origin/main 5cffbb6.

## Objective

A chat whose agent was handed a message it already holds no longer waits
for a first reply forever. Two chats on the owner's Mac ("docs test",
"Doc suggestions live test") had been looping since 11:45: every run of
either sat in `firstResponse` until it was stopped or the service
restarted, came back as `interrupted`/`failed`, and started again within
seconds. The web chat replayed the startup stages over and over, and the
menu bar's attention count flapped between 2 and 4 as approval and error
rows came and went with the chats' status.

## What exists

- `chats/sharing.go` (the `undelivered` poll, ~line 249): when the owner
  resolves a permission, pull-request or document-suggestion request, the
  policy service lists it as undelivered until the chat service acks it;
  the poller sends the agent a "Warden … resolved: {…}" notice with the
  **deterministic** message id `request_id[:32]`, acks only once the
  entry is `sent` and the chat `idle`, and re-queues the same entry when
  the chat ends `failed` or `interrupted` ("only this deterministic
  Warden notification may be retried after a crash").
- `agent/claude.go` (`turn/start`): the user frame handed to the CLI
  carries Warden's message id as its `uuid`, so a rewind can name it
  (`conversation/rewind`, item 11 of docs/claude-parity.md).
- Claude Code 2.1.272 reports a user message's life as
  `command_lifecycle` frames: `queued`, `started`, then, after the
  `result`, `completed`. A message whose `uuid` is already in the
  session's transcript is reported `completed` at once — no `queued`, no
  `started`, no API call, no `result` — and the process idles for the
  next input. The adapter did not read `command_lifecycle` at all, so the
  turn stayed open.

## What happened on the owner's Mac (2026-09-18)

1. 11:41 the owner resolved a Google Docs access request; the poller sent
   the notice to both chats at 11:45. The runs were slow (5–18 min to a
   first reply): the private `sbx` daemon was intermittently refusing
   every command ("docker hub refresh lock held by another process …
   401 … a prior ambiguous refresh attempt for this credential is still
   cooling down"; a second Warden stack, `~/.warden-p20`, runs its own
   `sbx daemon` against the same Docker Hub credential), a failed
   inspection clears every proof in `policy/verifier.go` (`failed`), and
   the gateway then answers the CLI's API calls with
   `503 provider route unavailable` until the next refresh passes; the
   CLI backs off for minutes.
2. The service was redeployed at 11:51 and 11:58 while those turns ran,
   so the chats ended `interrupted`; the poller re-queued the notices.
3. Every delivery since (11:51, 13:34, 13:59, 14:15, 14:26 …) reused the
   same message id; the CLI answered `command_lifecycle completed` and
   nothing else. Captured by wrapping `/tmp/warden-claude` in the guest
   with a tee shim: the adapter's frames in were the init handshake, the
   three MCP answers, `list_models` and the user message; the CLI's
   frames out ended with `{"type":"command_lifecycle","command_uuid":
   "4bdad319…","state":"completed"}`.
4. The same launch driven by hand (in the guest and from the host through
   `sbx exec -i`) with a fresh uuid ran the message in 2.5 s, which
   cleared the session, sandbox, gateway, relay, handshake and tool list.

## Steps

1. `agent/claude.go`: remember the turn's message id at `turn/start`; a
   `command_lifecycle` `completed` for that id while the turn is open
   (no `result` closed it, not a CLI-started turn) ends the turn as
   `completed` with nothing said. The lifecycle report that follows a
   normal result finds the turn closed and does nothing. Test
   `TestClaudeRedeliveredMessageEndsTurn`. ✔
2. Deploy to `~/.warden/release`, watch the two chats settle to `idle`
   and the poller ack the notices (`sbx.run` audit, `warden-chat.log`). ✔
3. Feature map: the Claude agent stream row names the lifecycle frame. ✔

## Key decisions

1. The fix is in the adapter, not the poller. A turn that can never end
   is the bug whatever sent the message; ending it lets the engine mark
   the entry `sent`, the chat `idle`, and the poller ack. The
   alternative — redeliver under a fresh uuid so the CLI runs it — would
   make the agent act on the same notice twice when the first run had
   already answered it (as here: "Access restored. Let me re-read the
   doc…"), and a redelivery only happens when the CLI had the message.
   The one thing lost is a notice whose first run was cut before the
   model answered; the owner can restate it.
2. The `sbx` daemon contention is reported, not fixed here: two Warden
   stacks on one Mac share the Docker Hub credential that `sbx` refreshes
   under a cross-process lock. Cloned test homes should be stopped when
   their session is done.

## Progress log

- 2026-09-18: diagnosed as above; step 1 done, adapter tests pass
  (`go -C chat test ./internal/agent/`), the new test fails on the
  unfixed adapter.
- 2026-09-18 14:31: deployed as v0.0.0-dev.fe4475e63d14 to
  `~/.warden/release` (the launchd service). The restart interrupted the
  two looping runs; the poller redelivered both notices at 14:31:50 and
  the turns ended in 6 s (`firstResponse 0.0s`); both chats idle since,
  no further runs in five minutes, the web chat back to its composer.
  Left as is: the pending review cards and the agent's open question in
  "docs test" are real and wait for the owner. Not done here: the
  `~/.warden-p20` stack (feat/web-attach-from-disk's clone) still runs a
  second `sbx daemon` against the same Docker Hub credential; and
  `~/.warden/release` no longer has the `menu` command (the menu bar was
  deployed from the unmerged feat/menu-bar build bb1cf87 and overwritten
  by later deploys; the feed process started at 11:51 keeps working on
  the old binary until the menu agent restarts).
- 2026-09-18: merged to main as f55de51 (main merged in first, 04ad636; the edge retention test `TestBugReportRetentionAndCap` re-based on the real clock, since its hardcoded 2026-09-18 days expired once UTC reached 2026-09-19).
