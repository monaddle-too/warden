# Response streaming implementation

Objective: stream approved provider SSE responses while retaining buffered request inspection, incremental response enforcement, and revocation. Test and merge to main as requested.

## Plan and progress
- [x] Inspect current checkout, MITM hooks, and existing SBX integration.
- [x] Implement bounded response stream inspection and lifecycle handling.
- [x] Test real incremental delivery, credential splits, encodings, limits, audit failure, cancellation, and regressions.
- [x] Review final diff and merge to main; clean up the worktree once preserved.

## Decisions
- Dedicated worktree/branch: codex/proxy-response-streaming.
- At task start main contained uncommitted SBX integration. Its Guard.control refactor was preserved as a prerequisite. During this task the integration was committed separately to main (91364d8); this streaming branch was rebased onto it. Temporary validation copies were hash-checked against their preserved originals before removal.
- Stream only successful text/event-stream responses for the three existing exact provider endpoints. Requests and other response types remain buffered.
- Validate/audit headers before streaming. Inspect body chunks with bounded state and count bytes. Midstream violations terminate delivery; previously delivered bytes cannot be withheld retroactively.
- Keep the existing permission watcher alive until completion/error. Stream audit records contain metadata/counts rather than buffered content.

## Validation and remaining work
- Initial 24 streaming unit/process tests passed, plus all 15 existing proxy flow tests. Process tests cover first-event delivery before EOF with identity/gzip/deflate, idle revocation, client cancellation, split credential rejection, unknown-length limits, and start/final audit rejection.
- Real transport tests exposed that returning an empty bytes DATA event can emit a premature zero-length HTTP/1 chunk. The callback now returns an empty iterable to suppress output.
- Pinned mitmproxy cannot immediately cancel in-transit flows with flow.kill alone. Streaming termination also closes the client transport through its pinned connection handler. This closes sibling HTTP/2 streams on that connection as well.
- Rejected SSE headers terminate at the header hook: replacing the response alone would wait for upstream EOF. Declared trailers and unsupported encodings are rejected; gzip/deflate are decoded with limits, identity is requested upstream.
- Added http.response.started to the host event allowlist and audit schema. Completion uses http.response; interruption uses request.interrupted. Body retention remains metadata only.
- HTTP/2 over intercepted TLS also passed a real-process first-event and idle-revocation test. SBX broker integration passed direct inherited streaming and split-credential checks; permanent SBX tests cover those paths and run-end revocation.
- The first full run against the old base found five desktop tests dependent on host free disk space, plus a missing Host in a new test fixture (fixed). Updated main already includes isolated desktop disk-space fixtures. Rerunning the full suite on the rebased branch.
- Final rebased full suite: 224 tests passed in 46.686 seconds with mitmproxy 12.2.3. Command: `PYTHONPATH=host /Users/danielporter/Documents/ai-vm/.local/proxy-test-venv/bin/python -m unittest discover -s tests -v`.
- Final diff reviewed; `git diff --check` passed. Documentation describes supported routes/encodings, metadata-only audit, fail-closed behavior, and connection-level cancellation.
- Merged to local main by fast-forward at 4e5f09f. Verified the worktree was clean and its HEAD was contained in main, then removed it. Validation logs are preserved in `.local/response-streaming-validation-20260911/` in the main checkout. Existing untracked `docs/cli-mvp-assessment-plan.md` was left intact.
- Complete. No remaining implementation work. No deployment or remote push requested.
