# Chat state in SQLite

## Objective

The chat service kept every chat, transcript, approval, port binding,
instruction set and catalog in one `app/chats.json`, re-encoded, fsynced
and renamed whole on every accepted mutation (a streamed token included,
coalesced over 250 ms). The file scales with the install's whole history,
so every change cost the whole history. Chat state now lives in
`app/chats.sqlite`, and a mutation writes the rows it changed: one entry
row for a streamed token, one chat row for a title, one instruction row for
a person's text.

## Shape

**The working set stays in memory.** The clients' view is the whole state
(one frame per change on `GET events`), and sixty-odd mutations across the
engine close over `*State`; both keep working on the in-memory `State`.
What changes is persistence: the store keeps a *shadow* of what the
database holds (the JSON of each chat's record and of each entry, approval,
review, turn and permission event, and of each port, instruction,
environment and catalog row), diffs the mutated part of the state against
it after every mutation, and writes only the rows whose encoding changed,
in one transaction. A mutation that changes nothing writes nothing, as
before.

**Scoped mutations.** `Store.updateChat(id, fn)` and `Store.streamChat`
diff one chat; `Store.update(fn)` and `Store.stream(fn)` diff everything
(chat creation and deletion, ports, instructions, the catalog). The agent
frame path, where every streamed token arrives, is chat-scoped.

**Streamed tokens** still coalesce: `streamChat` marks the chat dirty and
the flush timer writes its changed rows within 250 ms; a durable update
writes them first. A restart loses at most the last 250 ms of streamed
text, as before.

**Reads.** `Store.Chat(id)` copies one chat from memory (unchanged);
`Snapshot` decodes the shared encoding of the whole state (unchanged).
Startup loads every table once. Lazy transcript loading would need the
client protocol to stop sending the whole state; not in scope.

## Schema (`app/chats.sqlite`, WAL, one connection)

Real columns carry what an operator would query with `sqlite3`; the JSON
column of a row is what the service loads. Both are written from the same
struct in the same statement, so they cannot drift.

| Table | Key | Columns |
|---|---|---|
| `meta` | `key` | `value` (`schema` = 1) |
| `chats` | `id` | `position` (order in the list), `provider`, `model`, `title`, `sandbox_id`, `status`, `archived`, `created_at` (the first entry's, or the row's insertion), `record` JSON (the chat without its lists) |
| `entries` | `chat_id, seq` | `id` (indexed with `chat_id`), `role`, `turn_id`, `parent_id`, `created_at`, `delivery`, `entry` JSON |
| `turns` | `chat_id, seq` | `id`, `started_at`, `ended_at`, `turn` JSON |
| `approvals` | `chat_id, seq` | `id`, `run_id`, `method`, `state`, `approval` JSON |
| `reviews` | `chat_id, seq` | `id`, `kind`, `status`, `review` JSON |
| `permission_events` | `chat_id, seq` | `id`, `at`, `tool`, `decision`, `how`, `event` JSON |
| `ports` | `id` | `seq`, `chat_id`, `sandbox_id`, `port`, `state`, `binding` JSON |
| `deleted_sandboxes` | `sandbox_id` | `seq` |
| `instructions` | `principal_id` | `text`, `updated_at`, `name` |
| `environments` | `sandbox_id` | `record` JSON (rules) |
| `catalog` | `provider` | `at`, `models` JSON |

`seq` is the position in the in-memory slice; a truncation (a rewind, a
withdrawn approval) deletes the rows past the new length.

## Migration

`Open` finds no `chats.sqlite` and a `chats.json`: it loads the JSON as
before, writes every row, and renames the file to `chats.json.migrated`
(kept as the backup; nothing reads it). A later start with both present
ignores the JSON. The restart fix-ups (running → interrupted, pending
approvals expired, streaming flags cleared) apply in memory after the load
and are written as the diffs they produce.

## Steps

1. `store.go`: schema, open/migrate/load, shadow diff, scoped mutations,
   transactional writes, failure handling (a failed transaction restores
   the mutated chats from the shadow and marks the store failed).
2. Engine frame path on `updateChat`/`streamChat`.
3. Tests: the store tests read rows instead of the file; migration test;
   the permissions test queries the chat row.
4. Docs: feature map, install layout, Kubernetes volume note, chat doc.

## Progress

- Steps 1–4 implemented (2026-09-19): `store.go` keeps the state and the
  mutation API (`updateChat` / `streamChat` added), `storedb.go` holds the
  schema, the loader and the shadow diff; the agent frame path is
  chat-scoped; tests `TestOpenImportsTheLegacyFileOnce` and
  `TestWritesAreTheChangedRowsOnly`; the store tests read rows over a
  second connection. The chats package's `sendAndDeliver` helper now waits
  on the message's own delivery: with row writes a fake agent's turn ends
  inside the helper's 10 ms poll, so "still running" was no longer
  observable.

## Decisions

- SQLite through `modernc.org/sqlite` (already the policy service's
  driver, pure Go); `synchronous=FULL` so an acknowledged message is on
  disk when its caller returns, as with the fsynced rename before.
- The whole state stays in memory and the mutation closures keep their
  shape: the change is where writes go and how much they cost, not the
  engine's API.
- Text is stored once, inside the entry JSON (`json_extract(entry,
  '$.text')` searches it); duplicating it in a column would double the
  transcript on disk.

## Rolling back

A release before this one reads `chats.json` only. To run one again on a
state directory the database has taken over, stop Warden and move
`app/chats.json.migrated` back to `app/chats.json`; what was written to
the database since the import is not in that file.

## Remaining

- Lazy transcript loading (send clients one chat's entries on demand)
  would let the memory working set shrink to chat records; it needs the
  event protocol changed and is a separate piece of work.
