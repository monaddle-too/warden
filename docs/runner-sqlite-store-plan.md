# Runner inventory in SQLite

## Objective

The runner kept its sandbox inventory (`managedState`: sandboxes, chat
bindings, cancellations, preview attachments, publications, attachment
calls, spares) in `runner/managed-v2.json`, rewritten whole and atomically
on every change, and one marker file per cancelled run under
`runner/cancelled-runs/` (220 of them on the owner's Mac, never pruned).
Both now live in `runner/runner.sqlite`, and a save writes only the rows
whose encoding changed.

## Why not the chat service's database

The owner asked for the inventory to go into the chat SQLite. The runner
is its own process, and on Kubernetes its own pod with its own
ReadWriteOnce volume (`warden-runner-state`), so the chat service's file
is not reachable from it, and SQLite is not safe over a shared network
mount anyway. "One database" therefore means one per service: the chat
service has `app/chats.sqlite`, the runner `runner/runner.sqlite`, and the
policy service is the remaining candidate (273 per-sandbox
`control.sqlite` files plus `sharing.sqlite` and `google.sqlite`; see
Remaining).

## Shape

`internal/rowstore` is what the services share: `Open` (WAL, every commit
fsynced, one connection, the file made private, a `meta` schema version),
`Encode` (a map's values as JSON by key), `Sync` (write the rows of a
map-shaped table whose encoding differs from the shadow, delete the keys
the map lost, report the shadow updates to apply after commit) and `Load`.

`sandbox/store.go`: one table per map of `managedState`, keyed as the map
is, with the columns an operator would query (`sandboxes.runtime_name`,
`sandboxes.state`, `chats.sandbox_id`, `attachments.chat_id|sandbox_id|state`,
`publications.sandbox_id|state`) and the value's JSON in `record`;
`cancelled_runs(hash)` holds the tombstones. `saveManagedLocked` (49 call
sites, unchanged) encodes the seven maps, syncs each against its shadow
and commits once; nothing is written when nothing changed. The tombstone
reads and writes in `control.go` are queries on `cancelled_runs`.

## Migration

The first start finds no sandbox rows and a `managed-v2.json`: it loads
the file into memory, writes every row, and renames the file
`managed-v2.json.migrated`. The `cancelled-runs/` directory's file names
(the run-key hashes) are inserted into `cancelled_runs` and the directory
renamed `cancelled-runs.migrated`. Neither backup is read again.

## Progress

- Implemented and tested (2026-09-19): `TestRunnerStoreImportsTheLegacyFilesOnce`
  (import, backups, reload, tombstones across a restart, changed rows
  only, deletions); the two inventory tests that read the JSON file now
  read the database (`savedManaged`, `json_remove` on a row for the
  legacy-record case). The runner worker root is created by the store when
  missing (the JSON writer used to create it on the first save).
- Live on `~/.warden` (build 02c6c3a): `managed-v2.json` (32 sandboxes,
  34 bindings, 3 attachments, 3 publications, 4 calls, 1 spare) and the
  220 marker files imported with exact counts; deleting two workspaces
  removed their sandbox and binding rows; a fresh chat's turn wrote its
  binding and the adopted spare's row. The first send after the deploy
  failed with "worker has 32 retained sandboxes": a pre-existing hard cap
  in `bindLocked` (the `--retained` flag / `sandboxes.keepStopped` is read
  into `Worker.Retained` but never consulted), not the store. Fixed on
  `fix/runner-retained-cap` (2026-09-19): `bindLocked` honours
  `Worker.Retained` (default 32) and, when the inventory is full, retires
  the oldest stopped sandboxes that no chat is bound to and no run holds
  (runtime removed, rows dropped) before refusing; it refuses only when
  every retained sandbox is still bound or running
  (`TestFullInventoryRetiresTheOldestUnboundStoppedSandbox`). Merged to
  main 78adb90 (2026-09-19). Deployed to `~/.warden` 2026-09-19 (1658a41):
  the same send then failed with "…all bound or running" because all 32
  stopped sandboxes were bound to old chats, and raising `keepStopped` to
  64 killed the runner at startup ("invalid worker limits"): `runnersvc`
  kept its own ceiling of 32 on `--retained` while the configuration
  accepts any value ≥ 1. Fixed on `fix/runner-retained-limit`: the
  ceiling is `maxRetained` (1024) and the refusal logs the values.

## Decisions

- The in-memory `managedState` and the 49 `saveManagedLocked` callers are
  untouched; the change is in what a save costs and where it goes.
- The whole state is diffed on every save (seven small maps, ~50 KB
  encoded on the owner's Mac): cheaper than tracking scopes through 49
  call sites, and the writes are the changed rows either way.
- The `rowstore` helper is new; the chat store's per-chat lists need the
  positional (`seq`) diff it does not offer, so `chats/storedb.go` keeps
  its own code for now. Folding its map-shaped tables (ports, instructions,
  environments, catalog) onto `rowstore` is a small follow-up.

## Rolling back

A release before this one reads `managed-v2.json` and the marker
directory only: stop Warden, move `managed-v2.json.migrated` and
`cancelled-runs.migrated` back to their old names; what the database
recorded since the import is not in them.

## Remaining

- Policy service: one `policy/policy.sqlite` in place of one
  `control.sqlite` per sandbox (every table gains a `sandbox` column and
  every query a scope; ~45 query sites in `engine.go`, `images.go`,
  `pullrequests.go`, `docproposals.go`) plus `sharing.sqlite` and
  `google.sqlite` folded in. The per-sandbox `audit/events.jsonl` logs and
  `policy.json` / `gateway-port.json` would stay files or become tables in
  the same pass. Not started.
