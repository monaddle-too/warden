# Feature map

Where each owner-facing feature lives. A map, not documentation: one line per
feature, enough to open the right file without grepping. Read the linked plan
for the why. Keep it current — see the rule in [AGENTS.md](../AGENTS.md).

## Glossary — what the owner sees vs what the code says

Product naming drifted from code naming on purpose (API paths and Go
identifiers were kept stable). Grep for the right-hand column.

| Owner-facing term | In code / API |
|---|---|
| **workspace** (panel, sidebar entries) | `Environment`, `sandboxID`, `environments/*` routes, `chats/environments.go`, `sandbox.SandboxInfo` |
| **sandbox** (the VM itself) | sbx sandbox with `RuntimeName` `wc-…`/`ws-…`, `sandbox.RuntimeDriver` |
| **chat** | `chats.Chat`, `chats/*` routes; a chat's transcript is `Entry`/`conversation` |
| **preview** (a published port) | `Port`/`ports/*` routes, edge "binding", `sandbox/preview.go`, `chats/ports.go` |
| **spare** (pre-warmed sandbox) | `sandbox/pool.go`, config `sandboxes.warmSpares` |
| **grant** / **access** (documents, repos, network…) | `sharing/*` routes, `policy/sharing.go`, `chats/grants.go`; levels `read < write < structure` |
| **share documents** / **share repositories** | `sharing/select`, `sharing/github_select`, `DocumentSharing.tsx`, `RepositorySharing.tsx` |
| **pull request proposal** | `policy/pullrequests.go`, `sandbox/publish_plan.go`, `request_pull_request` tool |
| **approval** (agent asks, owner answers in the transcript) | `chats/{id}/approvals/{rid}` → `Engine.ResolveAs`, `Approvals.tsx` |
| **provider** (Codex / Claude) | `broker.Provider`, `agent/rpc.go` (Codex app-server), `agent/claude.go` |
| **owner** | the single capability holder; `edge/owner.go` role `admin`; `auth.mode: owner` |
| **principal** | who a sharing connection belongs to (`PrincipalID`), keyed for future multi-user |
| **policy** / **broker** / **gateway** | `warden policy` (`policysvc`), `chat/internal/policy` — the trusted control plane and inspected egress proxy |
| **runner** / **worker** | `warden runner` (`runnersvc`), `chat/internal/sandbox` — drives sbx |
| **edge** | `warden edge` (`edgesvc`), `chat/internal/edge` — sign-in, preview hosts, ingress |
| **guest image** | `deploy/guest/`, `release.GuestImage*`, config `sbx.guestImage` |
| **egress mode** restricted / open | `policy/egress.go`, `sharing/egress_set`, `<state>/policy/egress.json` overrides `warden.json` |

## Processes and layout

One binary, `chat/cmd/warden` (module `warden/chat`). `warden start` runs the
four services as subcommands of itself; `warden install|doctor|login|open|chat
|stop|status|uninstall` are the owner CLI.

| Service | Command | Package | Owns |
|---|---|---|---|
| policy | `warden policy` | `chat/internal/services/policysvc`, `chat/internal/policy` | per-sandbox policy engines, gateway CA + inspected proxy, credentials, sharing/grants, PR proposals, audit, image store |
| runner | `warden runner` | `chat/internal/services/runnersvc`, `chat/internal/sandbox` | sbx lifecycle, spares, agent streams, previews, repository fetch/review, host stats |
| chat | `warden serve` | `chat/internal/services/chatsvc`, `chat/internal/chats`, `chat/web` | chat state + HTTP API + web UI |
| edge | `warden edge` | `chat/internal/services/edgesvc`, `chat/internal/edge` | authentication (owner cookie / Google), preview hostnames, public ingress |

Shared: `chat/internal/config` (the one `warden.json` schema; appendix of
[warden-local-deployments-plan.md](warden-local-deployments-plan.md)),
`chat/internal/release` (pinned runtime versions, guest image, OAuth client
IDs), `chat/internal/handshake` (version check between services),
`chat/internal/services` (what the four share).

State dir (`~/.warden` locally): `policy/ runner/ app/ edge/ provider/ sbx/
runtimes/ bin/` (`chat/cmd/warden/state.go`).

Legacy macOS-VM stack (pre-SBX, still in tree): `warden` (Python launcher),
`host/` (control plane + dashboard), `proxy/` (Linux inspection VM), `native/`
(Swift launcher), `tests/`, `scripts/*.py`. Not where new chat/SBX work goes.

## Features

| Feature (owner-facing) | Backend | API | Web / TUI | Tests | Plan |
|---|---|---|---|---|---|
| Chats: create, message, typing indicator, rename/archive, stop | `chats/engine.go`, `chats/store.go` | `POST chats`, `chats/{id}/message|typing|edit|stop`, `GET chats/{id}/file|image-file`, `GET state`, `GET events` (SSE) | `ChatShell.tsx`, `Conversation.tsx`, `EntryView.tsx`, `RichText.tsx`, `CodeBlock.tsx` (+ `code.ts`, `code.test.ts`), `Mermaid.tsx` (+ `mermaid.ts`, `mermaid.test.ts`), `DiffView.tsx` (+ `diff.ts`, `diff.test.ts`), `InlineImage.tsx`, `Lightbox.tsx` (+ `images.ts`, `images.test.ts`) | `chats/*_test.go` | [warden-chat-migration-plan](warden-chat-migration-plan.md), [chat-rich-rendering-plan](chat-rich-rendering-plan.md) |
| Provider + model choice (fixed per chat; model from composer) | `sandbox/agent_selection.go`, `chats/engine.go` `ConfigureAgentAndRelease` | `chats/{id}/agent` | `ModelSelect.tsx` | | [model-dropdown-plan](model-dropdown-plan.md), [warden-claude-provider-plan](warden-claude-provider-plan.md) |
| Codex agent stream (app-server over stdio) | `agent/rpc.go`, `sandbox/runtime.go` `Stream` | | | `agent/*_test.go` | [response-streaming-plan](response-streaming-plan.md) |
| Claude agent stream (SDK control protocol) | `agent/claude.go`, `sandbox/runtime.go` `Stream` | | | `agent/claude_test.go` | [warden-resident-claude-plan](warden-resident-claude-plan.md) |
| Approvals in the transcript (agent questions, tool permission) | `chats/engine.go` `ResolveAs` | `chats/{id}/approvals/{rid}` | `Approvals.tsx`; TUI `tui/watch.go` | | |
| Workspace panel: status, Stop / Archive / Delete, sibling chats, documents, repositories, PRs, previews, access history | `chats/environments.go` | `GET environments`, `environments/{id}/stop|archive|delete`, `sharing/history` | `WorkspacePanel.tsx` | `chats/engine_test.go`, `chats/sharing_test.go` | [warden-environments-plan](warden-environments-plan.md) § Workspace panel |
| Sandbox lifecycle (create from template, clone, keep-alive, idle stop, remove) | `sandbox/managed.go`, `sandbox/runtime.go` (`RuntimeDriver` = the only sbx adapter), `sandbox/lock.go` | runner protocol `sandbox/client.go` | | `sandbox/managed_test.go`, `worker_test.go` | [sbx-integration-plan](sbx-integration-plan.md), [stop-status-plan](stop-status-plan.md) |
| Spare (warm) sandboxes | `sandbox/pool.go` | | | `sandbox/pool_test.go` | [warden-spare-sandbox-plan](warden-spare-sandbox-plan.md) |
| Sandbox memory / CPU sizing | `sandbox/runtime.go` `Create` (`--cpus 1 --memory`), config `sandboxes.memoryMB` | | | | |
| Host resource stats (runner's own host) | `hoststats/`, surfaced in `sandbox.Response.Stats` / `pool.go` | `chats/{id}/runtime` (status) | | | |
| Previews: bind a port, loopback `*.localhost` or public hostnames, unpublish | `sandbox/preview.go`, `sandbox/ports.go`, `chats/ports.go`, `chats/preview.go` | `GET ports`, `ports/{id}/revoke`, `ports/{id}/proxy/*`; edge `/auth/preview` | `Previews.tsx`, `WorkspacePanel.tsx` | | [warden-public-previews-plan](warden-public-previews-plan.md) |
| Agent tools: `preview_attach`, `sandbox_bind_port`, `attach_image` | `chats/preview.go`, `chats/images.go` | MCP server `warden` in `sandbox/runtime.go` / `agent/claude.go` | | | |
| Agent-requestable grants: `request_network_access`, `request_repository_access`, `github_write`, `request_host_directory`, `sync_host_directory` | `chats/grants.go`, `policy/sharing.go` (`network_allow`, `github_write`) | `sharing/request`, `sharing/resolve`, `sharing/revoke` | transcript cards in `Conversation.tsx` | `chats/grants_test.go` | alpha.10 notes in [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Google Docs / Sheets sharing: picker, grant levels read/write/structure, expiry, create-document requests | `policy/sharing.go`, `policy/documents.go`, `policy/adapters.go` (`AccessRank`), `policy/gateway.go` (guest document API) | `sharing/google/*`, `sharing/files`, `sharing/select`, `sharing/state`; tools `request_google_docs_access`, `request_google_document_creation` | `DocumentSharing.tsx` | `policy/*_test.go` | [google-docs-integration](google-docs-integration.md), [doc-write-workflow-plan](doc-write-workflow-plan.md), [document-api-plan](document-api-plan.md) |
| Docs inline images from attachments (`warden-image:<id>`, one-edit publish) | `policy/images.go`, `chats/images.go`, `imageguard/` | `chats/{id}/images/*`, `/published/<token>.png`, gateway action `unpublish` | `ImageAttachment.tsx` | | [image-attachments-ovh-plan](image-attachments-ovh-plan.md) |
| GitHub repository sharing: checkout, per-repo read categories (contents / issues / pull_requests), GitHub App or user token | `policy/githubapp.go`, `policy/githubuser.go`, `policy/operations.go` (REST catalog), `sandbox/repository.go`, `sandbox/fetch.go` | `sharing/github/*`, `sharing/github_repositories`, `sharing/github_select` | `RepositorySharing.tsx` | `sandbox/repository_test.go`, `repository_acceptance_test.go` | [github-repository-sharing-plan](github-repository-sharing-plan.md), [github-app-integration](github-app-integration.md) |
| Pull request proposals: reviewed single-branch push, owner Review proposal… dialog | `policy/pullrequests.go`, `policy/gitreview.go`, `policy/gitprotocol.go`, `sandbox/publish_plan.go`, `sandbox/review.go`, `chats/proposal_file.go` | `sharing/pr_preview`, `sharing/pr_get`; tool `request_pull_request` | `PullRequestReview.tsx` | `sandbox/publish_plan_test.go`, `review_test.go` | [pull-request-approval-plan](pull-request-approval-plan.md) |
| Inspected egress: gateway CA, per-host leaf certs, provider SSE streaming, destination policy | `policy/ca.go`, `policy/gateway.go`, `policy/gatewaypool.go`, `policy/stream.go`, `policy/egress.go`, `policy/registry.go` | runner→policy `bindGateway`, `configureProvider`, `egress`, `authorize` | | `policy/*_test.go` | [warden-gateway-ca-plan](warden-gateway-ca-plan.md), [response-streaming](response-streaming.md) |
| Egress mode restricted / open (runtime switch) | `policy/egress.go`, `policy.Registry.SetEgressMode` | `sharing/egress`, `sharing/egress_set` | `AdminConsole.tsx` | | |
| Admin console: egress mode, disconnect Google / GitHub, sign-in ledger | `edge/owner.go`, `policy/sharing.go` `disconnect` | `/api/admin/*` (edge), `sharing/disconnect` | `AdminConsole.tsx` | | [admin-console-plan](admin-console-plan.md) |
| Sign-in: owner mode (local capability cookie) and Google (server mode) | `edge/edge.go`, `edge/owner.go`, `edge/ledger.go`, `browserauth/` | `/auth/*`, `/oauth/*` | `AuthRoot.tsx`, `GoogleLogin.tsx` | `edge/*_test.go` | [google-login-publishing-plan](google-login-publishing-plan.md) |
| Provider logins (Codex / Claude / Google / GitHub device flow) | `login/github.go`, `cmd/warden/login.go`, `cmd/warden/github_login.go`, `sandbox/openai.go`, `policy/oauth.go`, `policy/credentials.go` | | `warden login` | | [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Local install: `warden install|doctor|start|open|stop|status|uninstall`, private sbx namespace, runtimes layout, popups | `cmd/warden/*.go` (`install.go`, `doctor.go`, `sbx.go`, `runtimes.go`, `notify.go`, `service.go`), `hostinfo/` | | | `cmd/warden/*_test.go` | [warden-local-install](warden-local-install.md), [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Terminal client `warden chat` (TUI), `chat list|new|send --wait|approve` | `tui/` | same HTTP API | `tui/app.go`, `tui/follow.go` | `tui/*_test.go` | |
| Releases: version handshake, pinned runtimes, guest image, OAuth clients | `release/release.go`, `handshake/` | | | | `scripts/release.sh`, `scripts/build-guest-image-in-sbx.sh`, [warden-guest-image-plan](warden-guest-image-plan.md) |
| CI: release, guest image and OCSF workflows on the self-hosted Mac runner | `.github/workflows/*.yml`, `deploy/ci/lima-docker.yaml` | | | | [ci-mac-runner](ci-mac-runner.md) |
| Deployment (OVH compose, Caddy, systemd; local `deploy-local.sh`) | `deploy/chat/*`, `deploy/guest/*` | | | | `deploy/chat/README.md`, [ovh-html-demo-plan](ovh-html-demo-plan.md) |
| Audit events / SIEM export | `policy/audit.go`, `policy/redact.go`, `policy/retention.go`, `schemas/audit-event.schema.json` | | | | [siem](siem.md) |
| Untrusted image normalisation | `imageguard/` | | | `imageguard/*_test.go` | |

## API surface at a glance

Chat service (`chat/internal/chats/http.go`, all under `/api/`, bearer token
from the edge): `state`, `events`, `environments`, `environments/{id}/{stop,
archive,delete}`, `chats`, `chats/{id}/{agent,message,typing,edit,stop,
activity,runtime,file,image-file}`, `chats/{id}/approvals/{rid}`, `chats/{id}/images/*`,
`ports`, `ports/{id}/{revoke,proxy/*}`, `sharing/*` (forwarded to the policy
service: `status, files, select, request, get, resolve, revoke, history,
blocked, block, connect, callback, disconnect, github_list,
github_repositories, github_select, github_write, network_allow, egress,
egress_set, pr_preview, pr_get`).

Runner protocol (`chat/internal/sandbox/client.go`): versioned request/response
over a private socket; responses may carry `Stats` (`hoststats.Sample`).

Policy service internal endpoints (`chat/internal/policy/registry.go`):
`register, check, begin, end, gateway, configureProvider, bindGateway, proxy,
egress, egress.finish, authorize, active, event`.

## Docs index

Per-feature plans are `docs/*-plan.md`; the plan that is currently the
mainline's record of progress is
[warden-local-deployments-plan.md](warden-local-deployments-plan.md). The
Kubernetes runtime direction is `docs/warden-kubernetes-plan.md` on branch
`plan/warden-kubernetes` (not on this branch yet). Architecture and security scope:
[architecture.md](architecture.md). Local install: [warden-local-install.md](warden-local-install.md).
