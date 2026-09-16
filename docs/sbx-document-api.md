# Documents through the SBX gateway

Warden can forward a small project document API to the local workspace app. The
app owns documents, revisions and proposal approval. This route adds no cloud
login and exports no user session or signing key into a sandbox.

## Host configuration

Add both arguments to the existing `python -m warden.sbx` command:

```sh
--document-api-origin http://localhost:8080 \
--document-api-key-file /absolute/private/workspace-document-api.key
```

Use the same signing key file configured for the app. It must be an owned,
regular, private file (0600), contain at least 32 printable ASCII characters,
and not be a symlink. A trailing newline is accepted. The key is read at broker
startup; changing it requires restarting both services. Do not put the key in
a command argument, sandbox environment, runtime instructions, gateway config,
or browser storage.

The origin must be HTTP with an explicit port and exactly `localhost`,
`127.0.0.1`, or `[::1]`, with no credentials, path, query or fragment. `localhost`
is dialed as `127.0.0.1`; the configured Host header is retained for the app's
canonical-host check. The HTTP client ignores environment proxy settings and
never follows redirects. Omitting configuration leaves document access disabled.
The existing provider routes and incremental provider streaming are unchanged.

A ready `begin`/`renew` reply adds only this public discovery value:

```text
documentBaseURL=http://host.docker.internal:GATEWAYPORT/workspace/v1/documents
```

Agents make ordinary HTTP requests to that address, with no authentication
header. Supported routes are GET collection, GET document detail, GET document
revisions, GET document proposals, and POST `/{documentID}/proposals` with a JSON
object. Collection GET can include an exact matching `projectID`. Collection,
revisions and proposals GET accept a numeric `offset` for pagination. Unknown
queries, duplicate keys, encoded path aliases, mismatched project IDs, other
methods and acceptance/authentication endpoints are rejected.

## Authentication and permission boundary

The gateway is bound to one sandbox by a host-held capability. For each document
request the broker independently checks the binding, runtime enforcement proof
and active lease. Identity comes from that binding and lease, never guest
headers or request arguments. Shared-sandbox processes share permissions; chat
and run IDs attribute the active authorized run.

The gateway discards all guest headers and forwards method, path and body over
its private broker connection. The broker then sends HTTP to the fixed app
origin with an entirely new header set. Neither its signing key nor generated
signature is sent to the gateway process. Requests use:

- `X-Warden-Context`: unpadded base64url compact JSON containing the seven string
  fields projectID, sandboxID, runtimeName, generation, chatID, runID, principalID.
- `X-Warden-Timestamp`: Unix seconds.
- `X-Warden-Nonce`: 32 random bytes encoded as 64 lowercase hex characters.
- `X-Warden-Signature`: lowercase hex HMAC-SHA256 over the newline-joined method,
  exact upstream path/query, SHA256 body hex, timestamp, nonce and encoded context.

The upstream prefix is `/agent/v1/documents`. There is no final newline in the
signature input. The app verifies signature, freshness, replay and current
project/run assignment before authorizing document access.

Request bodies are limited to 2 MiB and must be JSON objects for proposals;
GET bodies are rejected. Responses must be uncompressed JSON objects/arrays and
fit 4 MiB. App pagination keeps revision/proposal responses under this transport
limit. Responses return only JSON Content-Type and no-store headers; cookies,
redirects and upstream identity headers cannot reach the guest. A response
reflecting the key, request signature or encoded context is rejected. Every
HTTP exchange has a five-second total deadline as well as socket timeouts.

The broker releases its registry lock while awaiting the app, so cancellation
can revoke a lease independently. It checks the proof and same active run again
before delivering the result. Audit records contain route, IDs and result status,
not document content or authentication headers. Missing configuration, failed
proof, expired/revoked lease, lost broker access, unsafe upstream responses and
app failure all deny access.

## Verification

`tests/test_sbx_documents.py` exercises exact signatures and context against a
real controlled HTTP server; denial of route/query/context forgery, cross-sandbox
capability reuse, expired authorization, response reflection, redirects,
non-JSON/oversize responses; and revocation during an upstream request. Its
process test sends HTTP through a real mitmdump gateway, private broker socket
and controlled document server, then verifies revocation blocks the next request.
The verifier and keys in these tests are synthetic and cannot assert production
SBX enforcement.

`tests/test_sbx_proxy.py` adds real mitmproxy-flow tests for guest header removal,
broker loss and protocol rejection while retaining provider streaming regressions.
The coordinator separately verifies signed requests against the actual Go app,
including assignment checks, conflicts and persistent nonce handling.


## Saved rich snapshots and comment replies

The app now synchronizes rich editor drafts using separate owner-only endpoints.
Those collaboration and snapshot-write routes are deliberately absent from this
gateway. Agent GETs return saved revisions and comment context, never Yjs drafts.

The allowlist additionally supports:
- `GET /{documentID}/comments/replies?offset=0`
- `POST /{documentID}/comments/{threadID}/replies` with a JSON object containing
  `id`, `baseRevision`, and `text`.

The app verifies the saved thread, active actor/project assignment and idempotent
reply ID. Replies do not change the document revision or trigger another run.
All existing signing, lease and credential-isolation checks apply unchanged.
