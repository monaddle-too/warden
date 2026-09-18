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
1. ✓ Config + chart values + `warden.config` rendering; chart goldens.
2. ✓ Edge receiver (`POST /api/bug-reports`), storage, limits, retention; tests.
3. ✓ Admin routes + console page; tests.
4. ✓ Merge to main; deploy to GKE (`gke-deploy`); `curl` a sample report
   (`docs/bug-report-sample.json`) at the cloud URL and see it in the
   console.

Client track (`.local/warden-bugs-client`):
1. `bugreport` package: schema, gather, redact, capture, send; tests
   (redaction is the one to be thorough about).
2. Config + `warden install` question/flag + `warden bugs`; tests.
3. Present: the page and the loopback server; a test drives it with an
   HTTP client.
4. Triggers: installer, launcher (service exit + pending watcher),
   service panics; `/test bugreporting` route; tests with a fake state dir.
5. `/bug` route + composer + TUI; `warden bugs send/test`; tests.
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

6. Server: a report whose `id` is not 32 hex is stored under the
   server-assigned id *and* its `id` field is rewritten to it, so the file
   name and the document always agree. Unknown `before` cursors on the
   list route are `400`, not an empty page. The list route answers a bare
   JSON array (the contract's "list of"); `DELETE` answers `204`. The
   source address behind the ingress is `X-Forwarded-For`'s **last** entry,
   taken only when the connection's peer is a private or loopback address
   (ingress-nginx on GKE runs without `use-forwarded-headers`, so it
   replaces the header with what it saw; a public peer's header is
   ignored). The daily cap is a sliding 24 h over the stored files (the
   index is rebuilt from disk at start, so a restart does not reset it).
   The admin list/get/delete routes answer `404` where the receiver is
   disabled, and the console hides the section on that.

## Progress log

- 2026-09-18: plan written; tracks start.
- 2026-09-18 (server track, `feat/bug-reports-server`): steps 1–3 done.
  Config `edge.bugReports` + chart values + `warden.config` rendering
  (goldens updated, `helm_test.go` checks both states; GKE values enable
  it); the receiver `chat/internal/edge/bugreports.go` (validation,
  storage, per-IP hour / global day limits, retention + 10 000 cap, the
  owner's list/get/delete) with `bugreports_test.go` against
  `docs/bug-report-sample.json`; the console section `BugReports.tsx` +
  `bugreports.ts` (vitest). Live-checked on a cloned home
  (`.local/clone-warden-home.sh bugs 18820`, receiver enabled by hand):
  `curl` of the sample → 202, resend → same id/time, 300 KB → 413,
  unknown kind → 400; the section listed both reports, opened the error
  one (stack, two log files, environment, raw JSON); `DELETE` through the
  edge → 204 then 404. Left: merge, GKE deploy, the cloud `curl` check.
- 2026-09-18 (server track landed): merged to main as **c86ae18**
  (fast-forward; conflicts only in the feature map's Admin console row,
  unioned with round 2 A's spend section). Deployed to GKE as image
  `v0.1.0-alpha.12-326-gc86ae18` (helm revision 23); the rendered
  `warden.json` on the cluster has `edge.bugReports.enabled: true`. At
  `https://cloud.warden.monaddle.com/api/bug-reports`: the sample → `202
  {"id":"3f2a…5e6f","received":"2026-09-18T13:40:23Z"}`, the same body
  again → `202` with the same id and time (one file on the edge PVC,
  `bug-reports/2026-09-18/<id>.json`, mode 0600, `source` a 64-hex hash),
  300 KB → `413`, `{"schema":1}` → `400 kind is required`, `GET` → `405`,
  `/api/admin/bug-reports` signed out → `403`. The cloud admin console
  (owner session) lists the report and opens it. Server step 4 done.
  Finding: ingress-nginx runs with the default `externalTrafficPolicy:
  Cluster`, so the address it forwards is often a node-internal
  `10.128.x.x` (kube-proxy SNAT) rather than the client's; the per-IP
  hourly limit and the source hash are therefore coarse on GKE. The fix
  is `--set controller.service.externalTrafficPolicy=Local` on the
  ingress-nginx release in `scripts/k8s-gke.sh` (`up`); not applied, an
  operator decision.
