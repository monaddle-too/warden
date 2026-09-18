# Bug reporting

Status: started 2026-09-18 on branch `plan/bug-reporting` from origin/main
b04792b. Two implementation tracks branch from it once this document is on
main: **server** (`feat/bug-reports-server`) and **client**
(`feat/bug-reports-client`). This document is the contract between them.

## Objective

Bugs in the installer and in a running Warden reach one place —
the cloud Warden at `https://cloud.warden.monaddle.com` — so the owner can
see what breaks on other people's machines. Reporting is **opt-in at
install**, toggled later with `warden bugs on|off`, and **never silent**:
every report is shown to the person in full, on a standalone page that is
not part of the chat app (like macOS's crash reporter), with *Send* and
*Don't send*. Two modes:

1. **Automatic for errors.** An installer failure, a service that exits
   unexpectedly, or an uncaught panic produces a draft report; the person
   sees it and decides.
2. **`/bug`.** A person writes a report in the chat composer, the terminal
   client or the CLI (`warden bugs send "…"`); the same page shows what
   will go with it.

`/test bugreporting` (composer, TUI, `warden bugs test`) raises a deliberate
exception in the chat service so the automatic path can be exercised end
to end.

## What exists

- Config `warden.json` (`chat/internal/config/config.go`, every field
  defaulted, validated by runtime kind; the Helm chart renders it in
  `deploy/helm/warden/templates/_helpers.tpl` `warden.config`).
- `warden install` (`chat/cmd/warden/install.go`): sequential steps with
  `in.step(name, result)`, errors returned up to `main`; no interactive
  questions today except the SBX login it delegates to the terminal;
  `install.json` records versions.
- Launcher `warden start` (`chat/cmd/warden/start.go`): supervises the four
  services (`chat/internal/services/*`), logs to `<state>/warden.log` and
  `<state>/warden-{chat,runner,policy,edge}.log`; `notify.go` `popups`
  watches the chat service for approvals and opens the browser
  (`openBrowser`, `desktopNotify`) — the pattern for surfacing something to
  the person from the launcher.
- The edge (`chat/internal/edge/edge.go`): the public entry (`/auth/*`,
  `/api/*` proxied to the chat with the principal headers, `/api/admin/*`
  answered by the edge itself, owner-only); on Kubernetes it has its own
  state PVC (`warden-edge-state`, `deploy/helm/warden/templates/pvcs.yaml`)
  holding `sessions.json`; the admin console is `AdminConsole.tsx` in
  `chat/web/src/components/`.
- Composer local commands (`chat/web/src/composer.ts`, Chat group), TUI
  commands (`chat/internal/tui/app.go` `command`), CLI subcommands
  (`chat/cmd/warden/main.go`).
- Kubernetes deployment to GKE: skill `.claude/skills/gke-deploy`, values in
  the git-ignored `deploy/k8s/gke/env` held by the worktrees the workspace
  table names.

## Contract

### Endpoint

`POST https://cloud.warden.monaddle.com/api/bug-reports` — served by the
**edge**, unauthenticated, enabled only where `edge.bugReports.enabled` is
true (the cloud chart values; a local Warden's edge answers 404).

- Body: one JSON report (schema below), `Content-Type: application/json`,
  at most **256 KiB**; larger → `413`.
- Response `202 {"id": "<report id>", "received": "<RFC 3339>"}`; the id is
  the client's `id` when it is well formed (32 hex), else server-assigned.
  Idempotent on `id`: a resend of the same id answers `202` again without
  storing twice.
- Limits: 30 reports per source IP per hour and 500 per day overall →
  `429` with `Retry-After`; malformed JSON or unknown `schema` → `400`;
  disabled → `404`.
- Storage: `<edge state>/bug-reports/<YYYY-MM-DD>/<id>.json`, the body as
  received plus `receivedAt` and a hashed source (`sha256(ip + daily
  salt)`, never the IP). Retention `edge.bugReports.retentionDays` (default
  90) and at most 10 000 files; the oldest go first.
- Owner view: `GET /api/admin/bug-reports?limit=100&before=<id>` → list of
  `{id, receivedAt, kind, component, version, os, arch, summary}`;
  `GET /api/admin/bug-reports/{id}` → the stored report;
  `DELETE /api/admin/bug-reports/{id}`. Owner-only like the rest of
  `/api/admin/*`. A "Bug reports" page in the admin console lists and
  shows them (the description, the error and stack, the logs, the
  environment), with delete.

### Report schema (`schema: 1`)

```json
{
  "schema": 1,
  "id": "32 hex, client generated",
  "kind": "error" | "user",
  "createdAt": "RFC 3339",
  "component": "install" | "launcher" | "chat" | "runner" | "policy" | "edge" | "tui" | "cli",
  "trigger": "install-step" | "service-exit" | "panic" | "test" | "user",
  "summary": "one line, <= 200 chars",
  "description": "the person's text (user reports; optional otherwise)",
  "error": { "message": "...", "stack": "...", "operation": "install step / route / op name" },
  "warden": { "version": "v0.0.0-dev.<sha>", "protocol": 2, "runtime": "sbx" | "kubernetes",
              "installed": { "codex": "0.154.0", "claude": "2.1.272", "guestArch": "arm64" } },
  "system": { "os": "darwin", "arch": "arm64", "osVersion": "24.6.0", "cpus": 10, "memoryMB": 32768 },
  "logs": [ { "name": "warden-chat.log", "lines": ["...", "..."] } ],
  "context": { "chatID": "...", "runID": "...", "provider": "claude", "model": "..." }
}
```

Everything in `logs`, `error`, `context` and `description` passes the
redaction below before it is written to disk on the client, so the page
shows exactly the bytes that would be sent. `logs` carries the tail (at
most 300 lines, 64 KiB) of the component's log and the launcher log.

### Redaction (client, `chat/internal/bugreport` `Redact`)

Applied to every string: bearer/capability tokens and anything after
`Authorization:`/`token=`/`capability=` → `<token>`; 32+ hex runs that are
not a known id kind → kept (report/chat/run ids are needed); e-mail
addresses → `<email>`; the home directory prefix → `~`; `sk-…`, `ghp_…`,
`gho_…`, `ya29.…` → `<secret>`; anything matching a value from
`warden.json` `providers.*.secret` or the owner capability → `<secret>`.
Chat *content* is never included: no transcript entries, no message text,
no attachment names; `context` carries ids only.

## Client design

- **Config** `reporting: { "enabled": false, "url": "https://cloud.warden.monaddle.com/api/bug-reports" }` in `warden.json`; validation: `url` https (http only for `127.0.0.1`).
- **Install**: when stdin is a terminal, `warden install` asks once —
  "Send bug reports to Monaddle? You review every report before it is
  sent. [y/N]" — and records the answer; `--bug-reports=yes|no` answers it
  non-interactively; a re-run keeps the earlier answer unless the flag is
  given. The install summary prints the setting.
- **CLI** `warden bugs`: `status`, `on`, `off`, `send "text"` (a user
  report from the terminal), `test` (raises the test exception in the
  running chat service; falls back to an in-process one when Warden is
  not running), `pending` (lists undecided drafts and re-offers them).
- **Package** `chat/internal/bugreport`: `Report` (the schema), `New*`
  constructors that gather `warden`/`system`/`installed`, `Redact`,
  `Capture(state, report)` writes `<state>/bug-reports/pending/<id>.json`
  when reporting is enabled (nothing at all when disabled), `Present`
  (below), `Send(url, report)`.
- **Present** (the standalone page): the presenting process (installer,
  launcher, or `warden bugs`) listens on `127.0.0.1:0`, serves
  `GET /<random token>` with an HTML page embedded in the binary (no
  dependency on the chat web bundle; plain HTML, minimal inline CSS, a
  few lines of inline JS): title, what triggered it, the environment
  table, the error and stack, the logs in a scrollable pre, an editable
  description, the full JSON in a details block, and two buttons —
  **Send report** and **Don't send**. `POST /<token>/send` posts to the
  configured URL and shows the result (report id, or the failure and a
  retry); `POST /<token>/discard` deletes the draft. The listener closes
  after a decision or 15 minutes (the draft stays pending). It opens the
  browser with `openBrowser`; in the installer the terminal also prints
  the URL. `desktopNotify` announces it when the launcher is detached.
- **Triggers**: (1) `warden install` — any step error → `kind: error`,
  `trigger: install-step`, presented before the installer exits; (2) the
  launcher — a service process exits unexpectedly (not during stop) →
  `service-exit` with that service's log tail; (3) every service — a
  recovered panic in an HTTP handler, a worker op or a run goroutine →
  `panic` with the stack, written as a pending draft by the service
  itself; (4) `/test bugreporting` → the chat service raises and recovers
  a deliberate panic on a goroutine ("test exception from /test
  bugreporting") → the same path as (3). The launcher watches
  `<state>/bug-reports/pending/` (poll every 3 s, like `popups`) and
  presents each new draft once; the foreground launcher does the same.
  Kubernetes services never present (no browser) — reporting there is a
  later concern.
- **`/bug`**: composer `/bug <text>` (Chat group, hint "Report a bug to
  Monaddle — you review it first"), TUI `/bug <text>`, both →
  `POST chats/{id}/bug {text}` → the chat service writes a `kind: user`
  draft with `context` ids and the chat log tail → the launcher presents
  it. Without a chat: `warden bugs send "text"` presents in-process.
  Web and TUI answer with a notice ("Bug report drafted — review it in
  the window that opened"); when reporting is off, the notice says how to
  turn it on (`warden bugs on`) and nothing is written.
- **`/test bugreporting`**: composer and TUI local command → `POST
  api/bug-test` → trigger (4). `warden bugs test` calls the same route.

## Server design

- Edge routes above, in a new `chat/internal/edge/bugreports.go`; config
  `edge.bugReports: { enabled, retentionDays, maxPerHour, maxPerDay }`
  (`config.go` `Edge` — add the struct if the edge has none), chart values
  `edge.bugReports.*` rendered into `warden.json` by `warden.config`, GKE
  values enable it. Storage under the edge state dir (a PVC on
  Kubernetes; `<state>/edge/bug-reports` locally when enabled).
- Rate limiting in memory per pod (one replica); the daily cap counts
  files. Reports are validated against the schema (unknown fields kept,
  strings bounded) before they are written.
- Admin console page (`AdminConsole.tsx`): a "Bug reports" section —
  list newest first with kind/component/version/summary, a detail view
  rendering the sections, delete.

## Steps

Server track (`.local/warden-bugs-server`):
1. Config + chart values + `warden.config` rendering; chart goldens.
2. Edge receiver (`POST /api/bug-reports`), storage, limits, retention; tests.
3. Admin routes + console page; tests.
4. Merge to main; deploy to GKE (`gke-deploy`); `curl` a sample report
   (`docs/bug-report-sample.json`) at the cloud URL and see it in the
   console.

Client track (`.local/warden-bugs-client`):
1. [x] `bugreport` package: schema, gather, redact, capture, send; tests
   (redaction is the one to be thorough about).
2. [x] Config + `warden install` question/flag + `warden bugs`; tests.
3. [x] Present: the page and the loopback server; a test drives it with an
   HTTP client.
4. [x] Triggers: installer, launcher (service exit + pending watcher),
   service panics; `/test bugreporting` route; tests with a fake state dir.
5. [x] `/bug` route + composer + TUI; `warden bugs send/test`; tests.
6. Merge to main; `deploy-local`; end to end against the cloud receiver
   once the server track is live (`/test bugreporting` → page → Send →
   the report in the cloud admin console).

## Key decisions

1. The receiver is a route on the edge, not a fifth service: it already
   holds the public hostname, TLS, the owner-only admin console and a
   state PVC. Off by default so a local install never receives.
2. Drafts are files under `<state>/bug-reports/pending/`: every process
   (installer, launcher, services) can write one, only processes with a
   person in front of them present, and nothing is lost if the browser
   never opens.
3. Redaction happens before the draft is written, so what the page shows
   is byte-for-byte what is sent.
4. No chat content ever; ids only. The person's `/bug` text is theirs to
   write.
5. `kind: error` reports are drafted only when reporting is enabled;
   disabled means no files, no page, nothing.
6. (client) A recovered panic is captured and then raised again, so a
   process fails exactly as before (net/http logs a handler's, a worker
   op's or run goroutine's takes the service down); only the deliberate
   `/test bugreporting` panic is kept in the process. The launcher skips
   its `service-exit` draft when that service drafted a `panic` within
   the last 30 s: the panic draft has the stack, the exit would only
   repeat it.
7. (client) "Was the question asked?" is the presence of a `reporting`
   section in `warden.json` (install always writes one, `warden bugs
   on|off` too), so a re-run keeps the answer without another record;
   without a terminal and without the flag the answer is no, printed with
   the way to change it.
8. (client) The chat's `/bug` notice is a line in the composer (not a
   transcript entry) and the TUI's status notice; the route answers
   `{drafted, id, notice}` so both surfaces show the same words.

## Progress log

- 2026-09-18: plan written; tracks start.
- 2026-09-18 (client, `.local/warden-bugs-client`, `feat/bug-reports-client`):
  steps 1–5 implemented and unit-tested. `chat/internal/bugreport` (schema,
  gatherers, `Redact` with the table test, `Capture` → pending only while
  `reporting.enabled` — re-read from `warden.json` at each capture so
  `warden bugs on` reaches running services —, `Recover`/`Trap`/`Handler`
  guards, `Send` with the contract's 202/404/413/429/400 mapping, `Present`
  with the embedded `page.html`); `config.Reporting`; `warden install`
  question/flag/re-run/summary and the install-step trigger; `warden bugs
  status|on|off|send|test|pending`; the launcher's 3 s watch over the
  pending directory and the service-exit trigger; guards in the chat
  (handler, engine loop, run goroutines, sharing delivery), runner
  (connection/op, preview server), policy (control loop, both gateways)
  and edge (handler, run loop); `POST chats/{id}/bug` and `POST bug-test`
  (owner-only at the edge); composer `/bug`, `/test bugreporting` with a
  notice line; TUI `/bug`, `/test bugreporting`, `/help`. Verified: `go
  test ./...` (kube alone), `pnpm build && pnpm test`.
- 2026-09-18 (client, live on a cloned home `~/.warden-bugs` with a
  loopback receiver answering `202 {"id"}`): `warden install
  --bug-reports=no` writes `reporting.enabled=false`; on a pty the question
  is asked once, `y` turns it on, a re-run keeps it without asking;
  `warden bugs status|on|off`, a non-loopback http URL refused. `/test
  bugreporting` from the web composer → notice in the composer → the
  detached launcher presented the page (URL in `warden.log`, browser
  opened, desktop notified): every section present; Send with an edited
  description → the receiver logged the JSON (2.5 KiB, `<email>`,
  `token=<token>`, home as `~`, both log tails) and the page showed the
  id; a second one with Don't send → draft deleted, nothing received.
  `/bug` and `/test bugreporting` from the TUI → drafts with ids only
  (no title, no message), presented one after the other. `kill -9` of
  the runner → `service-exit` draft (signal: killed, the runner's log
  tail + the launcher's) presented before the launcher exited; a seeded
  log line came out as `contact <email>, Authorization: <token>, bare
  <secret>` (the owner capability). `warden bugs test` with Warden
  stopped → in-process; SIGTERM → "interrupted; the draft stays";
  `warden bugs pending` listed and re-offered it → sent. Home torn down.
  Next: merge, deploy-local.
