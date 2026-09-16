# Codex stall diagnosis

Objective: determine why the local Canton workflow stopped progressing without completing the sandboxed agent's work for it.

Plan: correlate saved chat events, guest Codex logs, and proxy audit; inspect the implicated transport/tool lifecycle; record confirmed causes separately from hypotheses. Do not edit guest work, submit proposals, or restart the running stack for diagnosis.

Progress: created isolated branch from the preserved workflow implementation. Investigating the late continuation stalls separately from earlier dependency-install disconnections.

## Findings (2026-09-15)

Confirmed defect: `proxy/addon.py` responseheaders initializes buffering and enables inspected streaming only when an exact provider route returns 2xx with `Content-Type: text/event-stream`. The completed Codex responses around the stalls have no Content-Type header. All used buffered `http.response` events, without `http.response.started` or stream counters. This prevents delivery of response events until upstream EOF, including progress that could otherwise demonstrate a live request.

Verified with the real pinned mitmdump and existing `tests/test_response_stream_process.py` loopback fixture, changing only whether the fixture emits Content-Type. The upstream emits `data: first`, waits for a release signal, then emits the second event and EOF. With Content-Type, the client receives the first event before release; without it, no event or response headers reach the client during a one-second observation interval. Releasing EOF delivers both events correctly in either case. No provider credentials or live sandbox mutations were needed.

Late review stall timeline (UTC):
- 13:26:30: first model response delivered, HTTP200, 126,673 bytes, no Content-Type.
- 13:26:31: Codex completed its code-mode call in 664ms. The shell returned `python: command not found`; the tool itself completed. Codex logged `needs_follow_up=true`, token-limit false, ~12.7k tokens against a ~244.8k compaction threshold, and began another sampling request.
- 13:26:34.830: Warden recorded the next POST to `/backend-api/codex/responses` (request `a0d83e9f-7b90-44cd-a666-45746e2cdc3e`). Provider authorization succeeded two milliseconds later.
- No response-completion or streaming-start event followed. Codex did not log returned HTTP headers. Owner steering at 13:29:33 was accepted but did not finish the in-flight model request.
- 13:36:13: proxy recorded client disconnection when the prior controller intervention ended the run, approximately 9m38s after dispatch.

The implementation conversation shows the same boundary: preview_attach completed at 13:14:44, another model POST was authorized at 13:14:50, and no response completion was recorded before later intervention. A resumed request at 13:23:33 also has no completion. Successful short final turns still used buffering; their upstream requests simply completed within a few seconds.

Evidence sources: local app/chats.json; guest read-only `/home/agent/.codex/logs_2.sqlite`; audit/events.jsonl in sandbox binding directories d16ce953…, ce551639…, 2fea50ce…, fcd84bd9…. Header values containing cookies or provider state are not copied into this report. The remote-control authentication polling and startup plugin denials concern separate operations, not a demonstrated failure of model authorization.

Limits: saved audit does not record upstream header arrival for buffered responses or partial byte counts. Therefore it cannot distinguish an upstream response that never started from a response receiving bytes that Warden withheld. The buffering bug is reproduced and explains silent waiting, but is not proven to be the sole cause of the prolonged upstream request. Earlier npm/control-lease disconnections are a separate failure and were not diagnosed by this probe.

Recommended follow-up: support inspected streaming for the exact Codex route when streaming was requested and the response lacks Content-Type; preserve credential inspection, byte limits, authorization and revocation. Add tests with omitted Content-Type and diagnostics for upstream-header/first-byte/last-byte timing without recording payloads or secrets. Then reproduce using a separate diagnostic agent conversation. Do not complete guest edits or submit a PR in place of the agent.

No production code, running service, sandbox project files, proposals, or permissions changed during this investigation.
