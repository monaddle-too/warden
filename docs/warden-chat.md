# Standalone Warden chat

Warden now owns its chat UI, persistent conversations, agent runs, and SBX worker.
This stack runs without a Panta process, login, database, or source checkout.
Panta remains a future app integration, as described in [the platform plan](warden-platform-plan.md).

## What moved

The implementation adapts Panta commit `bf61d5b072b024e3b5ad0b7a315c3610c2a4c828`:

- `backend/internal/agent` → `chat/internal/agent`: bidirectional app-server RPC.
- `backend/internal/sandbox` and `hoststats` → Warden-owned packages under `chat/internal`: managed SBX lifecycle, enforcement verification, runtime installation, checkout, preview publication, resource limits and the existing tests.
- `backend/internal/workspace/conversation.go` → `chat/internal/conversation`: transcript items, streaming deltas, history hydration and human attribution. Panta document-plan fields were removed.
- `web/src/components/Conversation.tsx`, `ChatShell.tsx`, `chat.css`, and conversation styles → `chat/web`: the transcript presentation, composer layout, sidebar and chat controls, adapted to Warden's API.

The Warden-specific `chat/internal/chats` package owns storage, HTTP authentication,
queueing and orchestration. It follows Panta's run identity and conservative
steering delivery flow. The Panta document editor, task/objective model, GitHub
publication workflow, memberships and shared document composer were not copied.

## Production deployment

The active installation runs entirely on OVH at https://warden.monaddle.com,
including Google sign-in, chats, policy enforcement, SBX execution, and private
port previews. There is no Mac relay. See [server deployment and operations](../deploy/chat/README.md).
The next section is the single-owner installation on your own machine.

## Local mode

Running the standalone stack on your own machine is the local-deployments
work: the `warden` launcher (`chat/cmd/warden`: `install`, `doctor`,
`login`, `start`, `open`) replaces the retired Python `scripts/warden-chat`
`start`/`open`. The operator guide is
[docs/warden-local-install.md](warden-local-install.md); the design and the
live macOS acceptance are in
[docs/warden-local-deployments-plan.md](warden-local-deployments-plan.md).

In short: `warden install` creates one owner-only state directory
(`~/.warden` on macOS, `$XDG_DATA_HOME/warden` on Linux), a private SBX
namespace (`<state>/bin/warden-sbx`, five HOME/XDG directories under
`<state>/sbx`, daemon started deny-all, the two verifier settings, the SBX
device sign-in), the pinned Codex and Claude runtimes, and a `warden.json`
holding only detected facts. The configuration file is optional for the
four services: every field has a computed default, `--config PATH` (default
`$WARDEN_CONFIG`) names a file, and a flag that disagrees with a loaded file
is a startup error (the flag-to-field mapping is in
[deploy/chat/README.md](../deploy/chat/README.md)). `warden start` runs
`warden-policy`, `warden-runner`, `warden-chat` and `warden-edge` as your
user with that namespace's environment, and `warden open` opens the app
through the edge at `auth.publicURL` (`http://127.0.0.1:18781` by default)
with the capability from `<state>/app/endpoint.json`. The edge in owner mode
authenticates the one owner by that capability and serves approved
previews on loopback as `http://<binding-id>.localhost:18781/…`, each on its
own origin with the same ticket, per-request binding check and revocation
model as the public deployment; the runner publishes the sandbox port on a
Warden-chosen loopback port behind it.

For development, `scripts/warden-chat build` still builds the three
original binaries and the UI; use the source build in the install guide
for all five.

## Behavior

- Each chat gets a new sandbox by default. Explicitly sharing an existing
  environment preserves separate chat/provider histories while sharing files.
- The local workspace runs one chat at a time. Other chats wait in the durable
  queue. The worker caps come from `warden.json` (`sandboxes.maxRunning`,
  `keepStopped`, `stopAfterIdleMinutes`; local defaults 2, 32 and 30 minutes,
  with `maxRunning` sized by `warden install` from the host's memory and
  cores) or the equivalent runner flags.
- Messages, streaming transcript, provider thread IDs and approval decisions
  persist in a private SQLite database, `chats.sqlite`, a row per chat, entry,
  approval and setting (docs/chat-sqlite-store-plan.md). The state directory has
  a single-writer lock. Restart marks unfinished work interrupted; it never
  automatically resends uncertain messages.
- Send during a run steers its current turn. A delivery attempt is persisted
  before the RPC. An unconfirmed attempt stays visibly failed and is not replayed.
  Browser retries reuse the message ID, including after reload when local storage
  is available. A follow-up queued before completion but not yet attempted runs
  afterward in the same chat.
- Stop interrupts the agent and stops the whole environment, including its
  previews. Later sends resume the provider thread and retained sandbox files.
  Other chats explicitly sharing that environment are affected by environment stop.
- Command/file/permission requests and agent questions appear in the chat. Answers
  apply only to the current run and exact pending RPC; stale/repeated answers are
  rejected. These agent approvals do not grant third-party provider access.
- Agents request a preview with `sandbox_bind_port(port, path, title)`
  (`preview_attach` is the compatible name) after starting a server on
  `0.0.0.0`. The request is an owner approval in the chat; on approval the
  worker publishes the port on a Warden-chosen loopback port and the edge
  serves it on a separate origin, `http://<binding-id>.localhost:<edge
  port>/` locally or `https://<binding-id>.<suffix>/` publicly, to signed-in
  browsers only. The UI lists it under "Published ports" with an "Open
  preview" link (a new tab, never an iframe on the app origin) and
  "Unpublish". With an explicitly empty `--preview-suffix` the tool answers
  that external previews are not configured.
- Local file links in agent messages download through the authenticated chat API. The worker reads only the bound sandbox; there is no host filesystem fallback.
- Chats can be renamed, archived and restored. Drafts remain on the current
  browser; there is no cross-browser Yjs composer synchronization in this version.
- Optional repository input accepts the worker's existing public GitHub checkout
  flow. Private repository onboarding is separate provider/MCP work.

## Current boundary

This delivers the standalone chat/SBX foundation. The unified connections and
permission console and the general agent MCP server remain platform-plan work.
Existing Figma, Google Docs and GitHub integrations are preserved, but this change
does not claim an end-to-end chat → MCP → provider approval demonstration.
Panta history is not migrated. Rich Panta documents, collaborative drafting,
publication/review UI are not part of this extraction.
The JSON store and full SSE snapshots target a local installation; large-history
pagination and multi-user deployment need a separate storage/API pass.

## Validation

```sh
go -C chat test -race ./...
go -C chat vet ./...
pnpm --dir chat/web test
pnpm --dir chat/web build
```

The new engine tests exercise streaming, steering, stop/resume, failed enforcement,
uncertain delivery, restart recovery, single-writer state, message retry
idempotency, run-bound approvals, Host/Origin/authentication checks, forged
principal rejection, wrong-sandbox responses, shared environments and archiving.
The imported sandbox/agent tests continue to run in Warden.

Live acceptance evidence and remaining checks are recorded in
[the migration plan](warden-chat-migration-plan.md).
