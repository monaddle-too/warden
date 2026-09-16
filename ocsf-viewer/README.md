# OCSF Explorer

A Go ingestion service and investigation workspace for Open Cybersecurity Schema Framework events. Authenticated batches are durably queued in bbolt, indexed in ClickHouse, and searchable in the browser. Everything for this app lives in `ocsf-viewer/`; it does not depend on or change the other projects in this repository.

## Stored events and ingestion

Production: https://siem.monaddle.com. Sign in with an approved Google account to investigate events. Admin accounts can upload files and review batches. Browser sessions expire after eight hours; use Sign out to revoke a session. API bearer tokens remain available for producers and operations. See [the ingestion contract](docs/ingestion.md) for API usage, validation, limits, and recovery guarantees. The ingest-only credential is intended for producers.

## Run locally

Use the pinned Nix + direnv environment (Node and Go). See
[deployment and recovery](deploy/README.md) for the OVH pipeline and setup.

```sh
cd ocsf-viewer
direnv allow
npm ci
npm run dev -- --port 4317
```

Open http://localhost:4317 and choose **Local files**. The local dataset contains **1,248 fictional events**, clearly marked as sample data. Use **Import events** to replace it with your own data.

For a production build served locally:

```sh
npm run build
LISTEN_ADDR=:4317 npm start
```

The production server is written in Go and serves the static React export.
Docker Compose runs it behind Caddy HTTPS on OVH. Run either the development
or production server on a port, not both.

## Local-file investigation workflow

- Import a single JSON event, an array, `{ "events": [...] }`, or JSONL/NDJSON. Drag a file into the import dialog or paste JSON.
- Review readable records, skipped records with line/record numbers, and basic field issue counts before committing the import. Append or replace; the sample is always replaced.
- Search across original nested JSON and resolved OCSF labels. Combine class, source, severity, UTC time, bookmark, and data-quality filters.
- Click a histogram bar to focus on its exact time interval. Clear the time filter to zoom back out.
- Sort by time, page through 50 rows at a time, and inspect original timestamps, entities, source context, classification, individual fields, and highlighted JSON.
- Use the search icon on a field or the actor link to pivot into matching events.
- Bookmark records during an investigation; save named searches and filters for future sessions.
- Export **all matching events across all pages**, in the current time order, as JSON, NDJSON, or CSV summary columns. Export or copy individual event JSON from the inspector.
- Press `/` to focus search and Escape to close the inspector. Dialogs and filters have keyboard controls.

### Local-file search syntax

Terms are ANDed. Matching is case-insensitive.

| Query                            | Meaning                                  |
| -------------------------------- | ---------------------------------------- |
| `credential`                     | Contains this text anywhere in the event |
| `"authentication failure"`       | Contains this phrase                     |
| `severity:high`                  | Resolved severity contains `high`        |
| `actor="svc-deploy"`             | Actor equals this value                  |
| `src_endpoint.ip:10.24.`         | A nested field contains this value       |
| `severity_id>=4 -severity_id=99` | Numeric comparisons and exclusion        |
| `class_uid=3002 status_id=2`     | Authentication failures                  |
| `-status:success`                | Exclude successful events                |

Aliases: `class`, `category`, `severity`, `source`, `actor`, `target`, `activity`, and `status`. Other names refer to original JSON fields using dot notation (including array indices). Use numeric fields such as `severity_id` for numeric comparisons. OR, regular expressions, SQL, wildcards, and a full SIEM query language are intentionally not implemented. Invalid or unfinished queries show an error and do not broaden results.

## Local-file data behavior and limits

- Event files are parsed in a **Web Worker** and are never uploaded to a server. This offline workflow does not call the ingestion API. The separate server workspace uploads only when you select Ingest file.
- Imported events and bookmarks live in memory for the current tab session. **Export before reloading or closing the tab.** The app does not persist datasets to disk or browser storage.
- Explicitly saved views (names, search strings and filters) live in this browser's localStorage. They may contain values you searched for. Delete a saved view with its × control.
- Per import: **25 MiB / 50,000 input records**. The active dataset is limited to 50,000 events, including appends. Per-record limit: 1,000,000 JavaScript string units. Skipped records are counted, with the first 100 diagnostics shown. Nothing beyond a dataset limit is silently truncated.
- JSON / NDJSON export retains parsed original fields and values, not normalized display labels. This is a JSON semantic round trip, not a byte-for-byte copy: whitespace, key formatting, duplicate keys, and numeric lexical forms are not retained. Numbers use JavaScript number precision.
- CSV exports summary columns, quotes cell content, and prefixes formula-like cell values for spreadsheet safety.
- Missing timestamps sort last. Timestamps use OCSF Unix milliseconds, with `time_dt` as a display fallback. All displayed times and range filters are UTC.
- Unknown classes and custom fields remain inspectable and exportable. Field inspection shows up to 1,000 search matches at a time; nested field flattening stops after 12 levels. The JSON tab keeps the parsed original object.

## OCSF coverage

`lib/catalog.json` is an offline display dictionary generated from **OCSF 1.6.0**, including 82 core and extension classes. It provides class/category names, activity enums, top-level field descriptions, and classification checks. Optional producer captions are retained for display.

**Validation covers the OCSF base contract, not the complete schema.** Checks cover base field types, metadata presence, severity enums, `type_uid = class_uid * 100 + activity_id`, and known class/category/activity consistency. They do not validate every nested object, class-specific requirement, extension, profile, or version. Diagnostics for records from other versions should be read in that context.

Regenerate the dictionary explicitly (requires network access):

```sh
npm run schema:update
```

The updater is pinned to 1.6.0 in `scripts/update-schema.mjs`. It sends no event data. Source: https://schema.ocsf.io and https://github.com/ocsf/ocsf-schema/tree/v1.6.0. See `THIRD_PARTY_NOTICES.md` and `licenses/OCSF-LICENSE`.

## Validation

```sh
npm test              # Parser, queries, enums, boundaries, time, CSV, sorting
npm run typecheck
npm run lint
npm run build
npm run test:worker   # Actual built worker: good/bad input + 50,000 records
npm audit
```

The production worker test uses Node worker_threads with a browser message-port shim. It exercises the actual compiled import worker, including its bundled dictionary. It is not browser end-to-end testing.

The supplied UI primitives are left intact and excluded from application linting because the scaffold's current lint rules flag its own vendored components. The app's source is linted; documented exceptions cover a compiler-lint internal error, external request loading state, and keyboard focus for a scrollable table. The `sharp` patch override addresses a known vulnerability in the local preview dependency chain.

Optional WebMCP tools (`search_ocsf_events`, `inspect_ocsf_event`) share the app's state and feature-detect browser support. They are unavailable in browsers without `document.modelContext`. A supported live WebMCP validation context was not exercised; ordinary UI controls work independently.

## Source map

- `app/page.tsx` — investigation state and workspace UI
- `components/import-dialog.tsx` — cancellable import/review flow
- `components/search-help.tsx`, `components/json-view.tsx` — search reference and JSON display
- `lib/events.ts` — pure parsing, display resolution, diagnostics, query/filter/export functions
- `lib/import.worker.ts` — background import entrypoint
- `lib/demo.ts` — deterministic fictional sample dataset
- `lib/catalog.json` — pinned OCSF display dictionary
- `tests/events.test.ts` — data-contract regression tests

The server implements authenticated HTTP batch ingestion, durable retries, quarantine, retention, and stored-event search. Detection rules, alerts, source-specific normalization, multi-tenancy, and incident case management remain future work.
