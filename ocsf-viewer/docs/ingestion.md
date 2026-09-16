# HTTP OCSF ingestion contract

Base URL: `https://siem.monaddle.com`. Machine clients require HTTPS and an explicit
`Authorization: Bearer` header. No token query parameters or cross-origin browser
access. Admin can ingest/query/manage; read can query/status; ingest can only POST
events. This is one trusted tenant with three rotatable shared credentials.

## Send a batch

```sh
# Set OCSF_INGEST_TOKEN securely in the environment. Keep the same key AND body
# for retries; generate a new key for a new batch. Do not put tokens in source.
curl --fail-with-body https://siem.monaddle.com/api/v1/events \
  -H "Authorization: Bearer $OCSF_INGEST_TOKEN" \
  -H 'Content-Type: application/x-ndjson' \
  -H 'X-OCSF-Source: my-collector' \
  -H 'Idempotency-Key: collector-batch-00001' \
  --data-binary @events.ndjson
```

Supported: a JSON event, array, `{ "events": [...] }`, or NDJSON. Limits: 10 MiB
request, 2,000 nonblank records, 10,000 physical NDJSON lines, 256 KiB and 64 nesting levels per record.
Compressed HTTP bodies are not supported. Up to four concurrent API requests.
Use batches instead of posting one event at a time; delivery processes a batch
per second. This deployment has not been load-certified for a particular EPS.

A `202` receipt contains `id`, `state`, `accepted`, `rejected`, `duplicate`, and the
first 100 validation problems. It means the receipt and original records committed
to a synced bbolt transaction on the VPS disk. It does **not** mean searchable yet.
Partial batches are accepted: valid rows queue, invalid rows remain in quarantine.
If every row is invalid, state is `quarantined`. The caller must inspect counts.

The idempotency key is scoped to credential role + source stream. Identical
content returns the original receipt; different content with that key returns
409. Preserve exact request bytes and content type on retry. Completed receipts
and keys expire after seven days; retries after expiration can create new events.
Transport failures, 429 and 503 should retry with backoff and the same key; honor
Retry-After. A full 512 MiB live queue/receipt store or <1 GiB free disk returns429.
Other 4xx responses need caller correction. Never treat HTTP success alone as
proof that all records passed validation.

## Validation and preservation

Enforces integer Unix-ms time, class/category/activity/type/severity identifiers,
type formula, nonempty metadata.version/product.name/product.vendor_name, and
known OCSF 1.6.0 class category/activity enums. Unknown custom classes are accepted.
It does not validate nested class-specific objects, profiles, or every OCSF
version. Invalid UTF-8 and oversized records are quarantined. A malformed JSON
array/envelope rejects the request; malformed NDJSON lines quarantine individually.

Original JSON text is stored separately from indexed columns and returned as the
`raw` string. Outer whitespace/BOM and NDJSON line whitespace are removed. Numeric
lexical values and custom fields survive ingestion/export. Browser inspection
uses JavaScript numeric precision; the Original JSON download preserves raw text.
Invalid bytes are available as `raw_base64` in the admin receipt.

## Search and operations

- `GET /api/v1/events`: read/admin. Filters `q` (literal case-insensitive raw text),
  `class_uid`, `severity_id`, `source` (exact product), `stream`, `start`/`end`
  (inclusive Unix ms), `field` + `value` (exact scalar dot path), `limit` (1–200).
  Returns events, total, and next_cursor. Reuse the same filters with that cursor;
  order is time descending then stable ID. Refresh to include new arrivals.
  Queries use typed SQL parameters, memory/time limits, and no arbitrary SQL.
- `GET /api/v1/status`: read/admin; storage readiness, queue occupancy/capacity,
  receipt-window counts, last database error and retention. Counts aren't lifetime
  telemetry. Completed receipt counts age out after seven days.
- `GET /api/v1/batches`: admin, latest 100 receipts. `GET /batches/{id}` includes
  quarantine. `POST /batches/{id}/replay` retries a dead-letter delivery unchanged.
  To correct invalid records, download, fix, and submit with a new idempotency key.
- `GET /api/v1/backup`: admin, consistent bbolt snapshot; see deployment recovery.
- `/healthz`: public process/revision check. `/readyz`: public ClickHouse readiness.
  Ingestion can still queue while readiness reports503.

ClickHouse writes are synchronous with fsync settings. Transient delivery errors
retry indefinitely with capped backoff. Permanent insert errors become dead
letters for operator replay; raw accepted records remain on disk. A crash between
insert and receipt commit retries stable IDs. ReplacingMergeTree plus FINAL queries
prevents duplicate search results (physical duplicates can exist until merge).
This is at-least-once delivery with deduplicated reads, not distributed exactly-once.

Events expire after 30 days by **receipt time**; query visibility enforces the
window immediately while ClickHouse TTL reclaims disk asynchronously. Queued and
dead-letter batches never expire automatically. Very old recovered events outside
the window won't appear. Completed receipts/quarantine expire after seven days.
Retention/capacity values are currently deployment defaults, not per-source knobs.

Search runs against live data with a receipt cutoff, not a transactional snapshot:
late indexing or retention expiry can change totals between pages. Pages also stop at 4 MiB of raw event text and return a cursor, so a page can
contain fewer rows than the requested limit. The UI
exports one server page at a time; the API cursor supports exporting all pages.

This single VPS can queue through a database outage, but disk/host loss requires
backup restoration. No replication, detections, alerts, syslog listener, source
normalization, SSO, per-user audit, or multi-tenant isolation is claimed.
