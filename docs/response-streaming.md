# Provider response streaming

Warden streams successful `text/event-stream` responses for authorized HTTPS
POST requests to these exact endpoints:

- `api.openai.com/v1/responses`
- `api.openai.com/v1/chat/completions`
- `chatgpt.com/backend-api/codex/responses`
- `api.anthropic.com/v1/messages` (reached through the gateway's `/anthropic`
  reverse route; `/v1/messages/count_tokens` is JSON only and stays buffered)

Requests remain buffered for inspection and authorization. Other response types,
error responses, and GitHub operations retain the existing buffered behavior.
Streaming is selected in the shared Guard, so the SBX gateway inherits it after
its provider route and credential checks.

Before delivering headers, Warden verifies permission, checks known credential
echoes, clears Alt-Svc, and records `http.response.started`. The response callback
checks every chunk and holds only a suffix that could become a known credential
when the next chunk arrives. Ordinary complete SSE events pass immediately.
This protects exact registered credential strings (including their registered
base64 representations); it is not general semantic secret detection.

The existing 16 MiB limit applies independently to received and decoded bytes,
including responses without Content-Length. The proxy asks providers for identity
encoding and also decodes gzip and zlib-wrapped deflate incrementally. Unsupported
encodings, malformed/truncated compressed streams, additional compressed members,
and declared response trailers are rejected. HTTP/2 trailers that arrive without
a declaration are discarded before delivery. The pinned mitmproxy does not
support HTTP/1 trailers.

Audit records retain only headers after redaction, byte counts and stream phase;
response body content is not retained. `http.response` records completion and
`request.interrupted` records interruption. Update/restart the host broker along
with the proxy when deploying: the host must accept `http.response.started`.
An older broker rejects the new event and streaming fails closed.

The permission watcher stays active throughout delivery. A revoked or expired
permission, credential echo, size violation, or inspection failure stops further
delivery. Pinned mitmproxy 12.2.3 does not interrupt an in-transit flow immediately
with `flow.kill()` alone, so Warden also closes the client transport. Other HTTP/2
streams sharing that connection are closed too. The connection-handler call is
version-sensitive and covered by real proxy tests; rerun them before upgrading
mitmproxy.

Rejected SSE headers close the flow immediately, rather than waiting for the
upstream body to end before returning a JSON error. Once streaming has begun,
already delivered bytes cannot be withdrawn. An audit failure at completion
closes the stream without a successful HTTP end-of-message marker. A recorded
start without a terminal event can indicate a later audit outage, proxy exit, or
an SBX lease revocation after which the broker no longer accepts flow events.

## Validation

Use the pinned proxy Python environment, with `PYTHONPATH=host`, to run:

```sh
python -m unittest discover -s tests -p 'test_response_stream*.py' -v
```

The process tests run a real mitmdump against a loopback upstream using synthetic
credentials and fixture authorization. They verify first-event delivery before
upstream EOF, HTTP/2 over intercepted TLS, compression, idle revocation, client
cancellation, split-secret rejection, byte limits, and audit failures.
