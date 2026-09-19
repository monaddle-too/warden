# SIEM integration contract

Start with `schemas/audit-event.schema.json`. The native feed is UTF-8 JSONL at
`.local/audit/events.jsonl`. Each complete line is one event. The host writes and
fsyncs events before authorizing GitHub access. When configured, the independent
host shipper forwards this feed to the SIEM after local persistence.

## Live delivery

Production ingestion is `https://siem.monaddle.com/api/v1/events`. Configure the
trusted host's `.local/siem.json` (or the directory selected by `WARDEN_STATE`):

```json
{"endpoint":"https://siem.monaddle.com/api/v1/events","source":"warden","token_file":"siem-token"}
```

Put the ingestion-only token in `.local/siem-token`, owned by the current user,
mode `600`. Keep both files in the ignored state directory; neither token nor
delivery state belongs in a VM, repository, or distributable package.

```sh
./warden siem       # start delivery without restarting the control plane or VMs
./warden status     # process state, counters, backlog bytes, receipt ID and errors
```

When configured, `./warden control`, `./warden run`, and `./warden proxy` also
start the shipper. It runs independently and catches/retries delivery errors with
backoff up to about a minute. After a host reboot, start Warden normally; this
does not install a login service. To stop just shipping, send SIGTERM to the PID
in `.local/siem.pid`. Restart with `./warden siem`; a file lock prevents concurrent
shippers. Configuration changes require a shipper restart. The token file is read
for every attempt, so replacing that file supports token rotation.

The first run backfills all complete native events, then follows new records
every two seconds. Batches contain at most 500 events and 1 MiB. OCSF records over
256 KiB or invalid native records pause delivery at that record, with a visible
error; they are never silently skipped. Fix/reconcile the record before restarting.
HTTP bodies remain omitted, including legacy records passed through the adapter.

`.local/siem.sqlite` durably stores the committed byte cursor, correlation cache,
and the exact pending bytes and idempotency key before sending. The cursor advances
only after a 202 receipt confirms every record accepted and none rejected. Lost
receipts retry identical bytes, including across restarts. Destination/source
changes are refused against an existing cursor. Preserve this database: deleting
it causes a full backfill and can create duplicate events.

A 202 means durable server queue acceptance. Local delivered counters do **not**
claim ClickHouse indexing; use the SIEM query API or admin batch receipt to check
indexing. Rejections and HTTP/network failures retain the pending batch and show
an error in `.local/siem-status.json` and `.local/siem.log`. Retries continue even
after prolonged outages. The SIEM retains batch receipts for seven days: if it
accepted a batch but the acknowledgement was lost, retrying after that window
can create duplicates. Delivery is at least once across that boundary; stable
native event IDs remain available for downstream deduplication.

The current native log is append-only. File replacement, truncation below the
cursor, or changes to the 256 bytes preceding the cursor stop shipping for
reconciliation. This checkpoint is not a full audit integrity scan and automated
log rotation is not supported. Keep the original file and cursor together.

Outbound HTTPS uses certificate verification and rejects redirects. SIEM outages
do not block proxy authorization; the durable local log holds the backlog.
New events continue accumulating in `audit/events.jsonl` during an outage, while
`siem.sqlite` retains the exact batch awaiting delivery. Retry delays grow from
about 2 seconds to at most 60 seconds and continue periodically. The failure count,
last error, and next retry time are also persisted, so restarting the shipper
preserves its retry schedule. Once the SIEM returns, it drains buffered events
in order and resumes following new events. There is no age-based retry cutoff.

`./warden status` exposes `unshipped_bytes` (the whole local backlog),
`pending_events`/`pending_bytes` (the next saved batch), `consecutive_failures`,
and `next_retry_at` (Unix seconds). Buffered data is not automatically discarded
or evicted; retain sufficient disk space for the outage duration.

## Manual export

```
./scripts/audit-export.py .local/audit/events.jsonl --follow
./scripts/audit-export.py .local/audit/events.jsonl --format ocsf --follow
./scripts/audit-export.py .local/audit/events.jsonl --format syslog --follow
./scripts/audit-export.py .local/audit/events.jsonl --format ecs --follow
```

The syslog formatter emits RFC 5424 messages (local0 facility) to stdout. The ECS
formatter maps the common fields; it does not claim complete ECS conformance.
An ingestor can consume JSONL directly without either conversion.

| Field | Meaning |
| --- | --- |
| `schema_version` | Semantic version of this event contract; currently `1.0.0` |
| `event_id` | UUID identifying this event; deduplicate on this field |
| `time` | UTC RFC 3339 timestamp with milliseconds |
| `event_type` | Stable dotted action name, listed below |
| `producer.instance_id` | New UUID on every control-plane start |
| `sequence` | Strictly increasing, starting at 1 within a producer instance |
| `request_id` | Correlates an inspected request with its decision and response |
| `decision_id` | Identifies an authorized attempt, including interruptions |
| `grant_id` | Identifies the human-granted permission used for that attempt |
| `previous_hash`, `event_hash` | Per-instance SHA-256 hash chain |
| `request`, `response` | Scrubbed HTTP metadata and body byte count |

`http.request` precedes a GitHub decision. `approval.requested` creates an item in
the human inbox. The client receives HTTP 428 and must retry after approval; the
server does not retain an executable request queue. `approval.granted` includes
the exact/scoped kind, expiration timestamp, allowed operation, URL, and body
constraints. An allowed retry emits `request.allowed`, followed by `http.response`
or `request.interrupted`/`proxy.error`. Non-GitHub requests use
`http.request.external`. A duplicate pending request can reference the existing
approval request ID; don't assume a one-to-one relationship between attempts and
approval inbox items.

Other events: `request.denied`, `approval.denied`, `approval.revoked`,
`policy.updated`, `credential.configured`, `credential.cleared`, `proxy.started`,
`system.started`, `network.denied`, and `dns.query`.

DNS and denied-packet events are collected from Linux dnsmasq/kernel logs. These
are metadata events; kernel/journald limits can suppress packets during a flood.
The synchronous request-metadata audit guarantee applies to inspected HTTP traffic,
not to a lossless capture of every Ethernet packet.

Use event ID for deduplication and `(producer.instance_id, sequence)` for ordering.
Keep unknown additional fields to support additive schema updates. Reject an
unsupported major version. A partial final line should be retried, not parsed.

## Capture and redaction

Authentication/cookie headers and all non-allowlisted header values are omitted.
HTTP request and response bodies are never persisted. They are inspected only in memory.
The configured GitHub token and common credential patterns are scrubbed from
text. The exact-request fingerprint is HMAC with a per-process random key. Approval
summaries contain metadata only; manually authored approval predicates are policy
configuration and remain persisted.

`body.capture = omitted_policy` and `body.bytes` record only the decoded byte
count. There is no `body.content`, body hash, prefix, or encoded body copy. The
host enforces this even for events from an older proxy. Bodies beyond the
inspection limit are rejected when they come from a model provider (scanned
for the brokered credential) or a Git remote (RPC bodies decoded and
reviewed); from any other granted host — a dependency download — they are
delivered uninspected, recorded as an `http.response.started` /
`http.response` pair with `response.passthrough = true`, the delivered byte
count and `response.complete`. Non-GitHub URL query values are omitted.

**Redaction is not a proof that arbitrary application data contains no secrets.**
An unknown credential in URL/header metadata, or a transformed encoding of a
secret, can escape pattern-based detection. Restrict access to these logs even
though normal authentication credentials are excluded. No raw mitmproxy flow
files, packet captures, or verbose credential-bearing traces are enabled.

## Hash verification

Remove `event_hash`, serialize the remaining event using sorted keys, compact
separators, ASCII JSON escaping, and no non-finite numbers; hash those UTF-8 bytes
with SHA-256. The first previous hash is 64 zeros. A new instance starts a new
chain. This detects alteration relative to a previously collected checkpoint;
it does not prevent a compromised host from rewriting an entire local chain.

Suggested alerts: repeated unsupported-channel attempts, denied repository or
operation, a burst of approval requests, missing event sequence numbers,
credential reconfiguration, policy changes, and proxy/control-plane errors.

## Body-retention migration

Previously captured body content was removed from local audit files and approval
summaries. Migrated events retain original IDs and original hash references in
`retention_migration`; their hash chains are recomputed over metadata-only events.
They are explicitly transformed evidence, not byte-identical historical records.
No body-bearing backup was retained. This does not erase copies previously
exported elsewhere or filesystem/backup snapshots.

## OCSF Explorer and ingester

Use `--format ocsf` for `ocsf-viewer/`. It emits UTF-8 NDJSON with OCSF 1.6.0
classification, integer Unix-millisecond `time`, and `metadata.product.name`
set to `Warden`. It does not change the native hash-chained audit format.

| OCSF field | Warden mapping |
| --- | --- |
| `metadata.uid` | Native `event_id`; stable across re-export |
| `metadata.correlation_uid` | `request_id`, when present |
| `metadata.event_code` | Native dotted `event_type` |
| `unmapped.warden` | Full metadata-only native event, including decision/grant IDs, producer instance, sequence, and hash references |
| `http_request`, `http_response` | Method, URL metadata, HTTP status and decoded body byte counts |
| `src_endpoint`, `dst_endpoint`, `query` | Observed network endpoints and DNS question |

HTTP and request-decision events use class 4002; DNS uses 4003; firewall denial
uses 4001; service start uses 6002; approval/configuration operations use 6003.
Unknown Warden operations remain inspectable as API Activity / Other. The
exporter keeps a bounded cache of 10,000 request metadata entries, scoped by
producer instance, to enrich later responses with their destination and method.
If context is unavailable, those fields are omitted and activity is Unknown;
correlation IDs remain available. No client identity is invented.

For a viewer import snapshot:

```sh
python3 scripts/audit-export.py .local/audit/events.jsonl --format ocsf > .local/audit/ocsf.jsonl
```

The ingester currently accepts `POST /api/v1/events` with these headers:

* `Authorization: Bearer <OCSF_INGEST_TOKEN>`
* `Content-Type: application/x-ndjson`
* `X-OCSF-Source: warden`
* `Idempotency-Key: <stable identifier for these exact batch bytes>`

Use batches of at most 2,000 events and 10 MiB, with each event at most 256 KiB.
Do not upload the whole snapshot if it exceeds the record limit. Request-body
compression is not supported. A 202 receipt means queued durably, not yet
written to ClickHouse; inspect `accepted`, `rejected`, and `problems`. Retry
uncertain deliveries with the identical bytes, source and idempotency key.
The current ingester deduplicates batches, not `metadata.uid` across differently
batched imports, so a live shipper must persist its batch/cursor state.

The manual exporter does not transmit events. Live delivery uses the same adapter
through `host/warden/siem.py`, with the persistent cursor and batch state described
above. The ingester and its deployment live in `ocsf-viewer/`.

OCSF references: [HTTP/2-independent HTTP activity schema](https://github.com/ocsf/ocsf-schema/tree/v1.6.0/events/network),
[metadata object](https://github.com/ocsf/ocsf-schema/blob/v1.6.0/objects/metadata.json).

### Repository workflow retention

Git requests use `git/read` and `git/push` operation metadata in the existing audit
and OCSF delivery path. Branch old/new object IDs are retained in native metadata.
The authenticated host review API holds source patches and PR fields only in
bounded, expiring memory. Linux Git review scratch lives on tmpfs and is removed;
canonical retry packs expire after two minutes. Previews and source contents are
never submitted as audit fields or sent to the SIEM. See repository-workflow.md.
