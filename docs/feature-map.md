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
| **suggestion** (a proposed edit to a Google Doc), **draft** (what the owner approves) | `policy.DocumentProposals`, `document_proposals` table, `sharing/doc_*` routes, `DocHunk` (one change), `DocParagraph` |
| **egress mode** restricted / open | `policy/egress.go`, `sharing/egress_set`, `<state>/policy/egress.json` overrides `warden.json` |
| **Kubernetes shape** (the third install, beside Mac and OVH) | config `runtime.kind: kubernetes`, `config.RuntimeKubernetes`; chart `deploy/helm/warden`; drivers `sandbox/kube`, `policy/kube` |
| **tier** (isolation boundary of a sandbox pod: `kata` or `gvisor`) | config `kubernetes.tier`, `config.TierKata`/`TierGVisor`; chart `runtime.tier`; the RuntimeClass `kubernetes.runtimeClass` |
| **canary** (two throwaway pods proving NetworkPolicy is enforced) | `policy/kube/canary.go` (`CanaryOptions`, `runCanaries`), pods labelled `warden.monaddle.com/canary` |
| **cluster facts** (what the policy service reads back about the cluster before trusting it) | `policy/kube/clusterfacts.go`, `policy/kube/inspector.go` |
| **trust bundle** (system CAs plus the gateway CA, mounted into every sandbox) | `policy/kube/trust.go` (`TrustPublisher`, `TrustBundleKey`), ConfigMap `warden-guest-trust` (`kubernetes.trustConfigMap`) |
| **shared gateway** (one credentialed egress listener for every sandbox) | `policy/sharedgateway.go` (`SharedGateway`), Service `warden-gateway`, config `kubernetes.gatewayService`/`gatewayPort`, `config.GatewayShared` |
| **startup stage** (what the chat's status line says while its sandbox starts) | `sandbox.Progress` / `Stage*` (`sandbox/progress.go`), `chats.Startup` (`chats/startup.go`), `Chat.startup`, labels in `web/src/stages.ts` |
| **Cluster** (the admin console section: nodes, sandbox pods, service pods, logs) | `sandbox.ClusterInspector` / `ClusterStatus` / `PodInfo` (`sandbox/cluster.go`), `sandbox/kube/cluster.go`, `chats/cluster.go`, `ClusterView.tsx` |
| **size** (a workspace's CPUs and memory) | `sandbox.Resources` (`cpuMilli`, `memoryMB`), `sandbox.ResourceLimits` (default, max, `cpuStepMilli`, `restart`), runner op `resize` |

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
[warden-local-deployments-plan.md](warden-local-deployments-plan.md) and
appendix A of [warden-kubernetes-plan.md](warden-kubernetes-plan.md)),
`chat/internal/release` (pinned runtime versions, guest image, OAuth client
IDs), `chat/internal/handshake` (version check between services),
`chat/internal/services` (what the four share), `chat/internal/transport`
(`unix://` and mutual-TLS `tls://` listeners and dials between them),
`chat/internal/kube` (the minimal Kubernetes API client: in-cluster or
kubeconfig, typed REST, watch, exec, logs).

State dir (`~/.warden` locally): `policy/ runner/ app/ edge/ provider/ sbx/
runtimes/ bin/` (`chat/cmd/warden/state.go`).

Kubernetes shape (`runtime.kind: kubernetes`): the same four services as
Deployments over `tls://`, state under `/var/lib/warden` on one PVC each,
sandboxes as pods in `kubernetes.namespace` with a workspace PVC; installed
by the chart `deploy/helm/warden`, never by `warden install`
([warden-kubernetes.md](warden-kubernetes.md)).

Legacy macOS-VM stack (pre-SBX, still in tree): `warden` (Python launcher),
`host/` (control plane + dashboard), `proxy/` (Linux inspection VM), `native/`
(Swift launcher), `tests/`, `scripts/*.py`. Not where new chat/SBX work goes.

## Features

| Feature (owner-facing) | Backend | API | Web / TUI | Tests | Plan |
|---|---|---|---|---|---|
| Chats: create, message (retry / edit-and-resend / copy as markdown from the transcript), typing indicator, rename/archive/export (markdown or JSON, built in the browser), search (⌘K palette across chats, ⌘F find in the transcript), jump-to-bottom and unread divider, turn timing and tokens/cost per agent turn, slash commands and `@path` mentions in the composer, stop | `chats/engine.go`, `chats/store.go`, `chats/paths.go`, `sandbox/paths.go` (worker op `paths`), `conversation/model.go` (`Turn`, `Usage`), `agent/claude.go` (usage from the `result`) | `POST chats`, `chats/{id}/message|typing|edit|stop`, `GET chats/{id}/file|image-file|paths`, `GET state`, `GET events` (SSE) | `ChatShell.tsx`, `Conversation.tsx`, `Suggest.tsx` (+ `composer.ts`, `composer.test.ts`), `EntryView.tsx`, `RichText.tsx` (+ `links.ts`, `links.test.ts`), `CodeBlock.tsx` (+ `code.ts`, `code.test.ts`), `Mermaid.tsx` (+ `mermaid.ts`, `mermaid.test.ts`), `DiffView.tsx` (+ `diff.ts`, `diff.test.ts`), `katex.ts` (+ `math.ts`, `math.test.ts`), `streaming.ts` (+ `streaming.test.ts`), `InlineImage.tsx`, `Lightbox.tsx` (+ `images.ts`, `images.test.ts`), `ExportDialog.tsx`, `useCopy.ts` (+ `export.ts`, `export.test.ts`), `SearchPalette.tsx`, `FindBar.tsx` (+ `search.ts`, `search.test.ts`), `transcript.ts` (+ `transcript.test.ts`), `TurnStats.tsx` (+ `turns.ts`, `turns.test.ts`) | `chats/*_test.go`, `sandbox/paths_test.go`, `conversation/turns_test.go` | [warden-chat-migration-plan](warden-chat-migration-plan.md), [chat-rich-rendering-plan](chat-rich-rendering-plan.md) |
| Composer attachments (paste, drop, picker; sent into the sandbox with the message) | `chats/attachments.go`, `sandbox/attachment.go` (worker op `attachment-write`), `agent/claude.go` (image blocks) | `POST chats/{id}/attachments` (multipart), `GET chats/{id}/attachments/{aid}`, `POST chats/{id}/attachments/{aid}/remove`, `attachments` on `chats/{id}/message` | `Attachments.tsx`, `Conversation.tsx` (+ `attachments.ts`, `attachments.test.ts`, `drafts.ts`, `drafts.test.ts`) | `chats/attachments_test.go`, `sandbox/attachment_test.go` | [chat-rich-rendering-plan](chat-rich-rendering-plan.md) |
| Provider + model choice (fixed per chat; model from composer) | `sandbox/agent_selection.go`, `chats/engine.go` `ConfigureAgentAndRelease` | `chats/{id}/agent` | `ModelSelect.tsx` | | [model-dropdown-plan](model-dropdown-plan.md), [warden-claude-provider-plan](warden-claude-provider-plan.md) |
| Codex agent stream (app-server over stdio) | `agent/rpc.go`, `sandbox/runtime.go` `Stream` | | | `agent/*_test.go` | [response-streaming-plan](response-streaming-plan.md) |
| Claude agent stream (SDK control protocol) | `agent/claude.go`, `sandbox/runtime.go` `Stream` | | | `agent/claude_test.go` | [warden-resident-claude-plan](warden-resident-claude-plan.md) |
| Thinking feedback: the model's reasoning as a `thinking` entry (Codex's summary streamed by delta and part; Claude's blocks as reasoning items, text withheld by its CLI), shown as each app shows it, the pending-reply row while nothing streams, status "Agent is thinking" | `conversation/conversation.go` (`reasoning` item, `Delta`, `Break`, `Entry.EndedAt`), `agent/claude.go` (`thinking` blocks, `thinking_tokens`), `chats/engine.go` (`item/reasoning/*`) | `GET state` / `events` → `entries[].role == "thinking"`, `endedAt` | `Thinking.tsx` (+ `thinking.ts`, `thinking.test.ts`), `EntryView.tsx`, `Conversation.tsx`, `stages.ts`; TUI `tui/render.go` | `conversation/conversation_test.go`, `agent/claude_test.go` | [thinking-feedback-plan](thinking-feedback-plan.md) |
| Approvals in the transcript (agent questions, tool permission) | `chats/engine.go` `ResolveAs` | `chats/{id}/approvals/{rid}` | `Approvals.tsx`; TUI `tui/watch.go` | | |
| Workspace panel: status, Stop / Start (a stopped sandbox back without a message; runner op `start`) / Archive (Stop and Archive end a running chat first) / Delete, sibling chats, documents, repositories, PRs, previews, access history | `chats/environments.go` (`StartEnvironment`, `stopChats`), `sandbox/managed.go` `startLocked` | `GET environments`, `environments/{id}/stop|start|archive|delete`, `sharing/history` | `WorkspacePanel.tsx` | `chats/engine_test.go`, `chats/sharing_test.go` | [warden-environments-plan](warden-environments-plan.md) § Workspace panel |
| Startup stages: the chat says which stage its start is at (queued with the blocker, binding, preparing, waiting, creating / resuming with the driver's detail, attesting, probing, installing, cloning, launching, initializing, connecting, sending, firstResponse — the last two on every turn); a start over 5 s logs its stages' durations; the model's reasoning is a "Thinking…" step and the status says "Agent is thinking" (`conversation.go` `reasoning`, `stages.ts`); runner reads never queue behind a creation | `sandbox/progress.go` (`progress` op, `Report`/`WithProgress`), `sandbox/snapshot.go` (`status`/`usage`/`pod` while `prepare` holds the lock), `sandbox/managed.go` `prepareLocked`, `sandbox/kube/driver.go` `StartupDetail`, `chats/startup.go` (`followProgress`), `Worker.PrepareTimeout` | `GET state` / `events` → `chat.startup`; `GET environments` → `chats[].stage` | `Conversation.tsx` status line, `ChatShell.tsx` sidebar dot, `WorkspacePanel.tsx` chat list, `stages.ts`; TUI `tui/render.go` `RenderStatus` | `sandbox/progress_test.go`, `sandbox/kube/cluster_test.go` (`TestStartupDetail`), `chats/startup_test.go`, `web/src/stages.test.ts` | [warden-startup-visibility-plan](warden-startup-visibility-plan.md) |
| Cluster visibility (Kubernetes): the workspace's pod in the panel; admin console Cluster section with nodes, sandbox pods, Warden service pods, live usage from metrics.k8s.io, and pod logs | `sandbox/cluster.go` (`ClusterInspector`, ops `pod`, `cluster.status`, `cluster.logs`), `sandbox/kube/cluster.go`, `kube/quantity.go`, `kube/exec.go` `LogsWith`, `chats/cluster.go`; chart `templates/rbac.yaml` (`warden-runner-view`, value `rbac.runnerClusterView`) | `GET environments` → `pod`; `GET cluster`; `GET cluster/logs?namespace=&pod=&container=&tail=&previous=` (owner-only at the edge) | `WorkspacePanel.tsx` `Pod`, `ClusterView.tsx`, `units.ts` | `sandbox/kube/cluster_test.go`, `kube/quantity_test.go`, `edge/edge_test.go`, `deploy/helm/warden/test.sh` | [warden-startup-visibility-plan](warden-startup-visibility-plan.md) |
| Sandbox lifecycle (create from template, clone, keep-alive, idle stop, remove) | `sandbox/managed.go`, `sandbox/runtime.go` (`RuntimeDriver` = the only sbx adapter), `sandbox/lock.go` | runner protocol `sandbox/client.go` | | `sandbox/managed_test.go`, `worker_test.go` | [sbx-integration-plan](sbx-integration-plan.md), [stop-status-plan](stop-status-plan.md) |
| Spare (warm) sandboxes | `sandbox/pool.go` | | | `sandbox/pool_test.go` | [warden-spare-sandbox-plan](warden-spare-sandbox-plan.md) |
| Workspace size: chosen at creation, changed by the owner, requested by the agent (`request_resources`) | `sandbox/resources.go`, `sandbox/runtime.go` `createArgs`/`Resize` (SBX regenerates), `sandbox/kube/driver.go` `Resize` (`pods/resize`, live), `sandbox/managed.go` `resizeLocked`/`resizeAdoptedLocked`, `chats/resources.go`, `runnersvc/config.go` `resourceLimits`/`kubernetesResourceLimits`; config `sandboxes.memoryMB|cpus|maxMemoryMB|maxCPUs`; chart `sandboxes.{cpus,maxCPUs,maxMemoryMB}` (quota from the ceiling), Role `pods/resize` | `POST chats` `resources`, `environments/{id}/resize` (validates, then applies in the background; `GET environments` → `resizing`), `GET state` `sandboxes`; runner `resize`, `health` `limits` | `SizeSelect.tsx` in `ChatShell.tsx` (form) and `WorkspacePanel.tsx` (Resources › Change…); `warden chat new --cpus --memory` | `sandbox/resources_test.go`, `sandbox/live_sbx_test.go`, `sandbox/kube/driver_test.go` (`TestResize*`), `sandbox/kube/live_test.go`, `chats/resources_test.go` | [warden-workspace-resources-plan](warden-workspace-resources-plan.md) |
| Workspace resources: provisioned and used CPU / memory / disk (guest-reported, 3 s cache) | `sandbox/usage.go` (`usage` op, `SandboxUsage`), `chats/environments.go` (`Environment.Usage`) | `GET environments` → `usage` | `WorkspacePanel.tsx` `UsageRows` | `sandbox/usage_test.go` | [warden-environments-plan](warden-environments-plan.md) § Workspace resources |
| Host resource stats (runner's own host) | `hoststats/`, surfaced in `sandbox.Response.Stats` / `pool.go` | `chats/{id}/runtime` (status) | | | |
| Previews: bind a port, loopback `*.localhost` or public hostnames, unpublish; on Kubernetes the runner's shared mTLS preview server (config `services.runner.previews.{listen,address}`, chart `services.runner.previewPort`) | `sandbox/preview.go`, `sandbox/ports.go`, `chats/ports.go`, `chats/preview.go`, `runnersvc/main.go`, `deploy/helm/warden/templates/{services,networkpolicies}.yaml` | `GET ports`, `ports/{id}/revoke`, `ports/{id}/proxy/*`; edge `/auth/preview`; runner `https://warden-runner:7446/{publicationID}/*` | `Previews.tsx`, `WorkspacePanel.tsx` | `sandbox/preview_shared_test.go`, `chats/ports_test.go` | [warden-public-previews-plan](warden-public-previews-plan.md), [warden-kubernetes-plan](warden-kubernetes-plan.md) § decisions 5, 10 |
| Agent tools: `preview_attach`, `sandbox_bind_port`, `attach_image` | `chats/preview.go`, `chats/images.go` | MCP server `warden` in `sandbox/runtime.go` / `agent/claude.go` | | | |
| Agent-requestable grants: `request_network_access`, `request_repository_access`, `github_write`, `request_host_directory`, `sync_host_directory` | `chats/grants.go`, `policy/sharing.go` (`network_allow`, `github_write`) | `sharing/request`, `sharing/resolve`, `sharing/revoke` | transcript cards in `Conversation.tsx` | `chats/grants_test.go` | alpha.10 notes in [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Google Docs / Sheets sharing: picker, grant levels read/write/structure (Docs are read-only at every level; write/structure are Sheets levels), expiry, create-document requests (read-level grant, filled through suggestions) | `policy/sharing.go`, `policy/documents.go`, `policy/adapters.go` (`AccessRank`), `policy/gateway.go` (guest document API) | `sharing/google/*`, `sharing/files`, `sharing/select`, `sharing/state`; tools `request_google_docs_access`, `request_google_document_creation` | `DocumentSharing.tsx` | `policy/*_test.go` | [google-docs-integration](google-docs-integration.md), [doc-write-workflow-plan](doc-write-workflow-plan.md), [document-api-plan](document-api-plan.md) |
| Google Docs suggestions: agent reads numbered paragraphs, proposes edits with reasons, owner accepts/rejects/edits the draft, Warden writes on approval, returned drafts revised by the agent | `policy/docmodel.go`, `policy/docdiff.go`, `policy/docchanges.go`, `policy/docview.go`, `policy/doccompile.go`, `policy/docproposals.go`, `policy/sharing.go` (`Document`, `BatchUpdate`) | `sharing/doc_state`, `sharing/doc_preview`, `sharing/doc_draft`, `sharing/doc_decide`, `sharing/doc_resolve`, `sharing/doc_return`, `sharing/doc_rebase`; tools `read_google_document`, `propose_google_document_edit` | `DocumentReview.tsx`, `web/src/documents/suggestions/` (vendored from Panta, see `web/src/documents/PROVENANCE.md`) | `policy/docmodel_test.go`, `policy/docchanges_test.go`, `policy/docview_test.go`, `policy/docproposals_test.go` | [doc-suggestions-plan](doc-suggestions-plan.md), [doc-suggestions-view-plan](doc-suggestions-view-plan.md) |
| Docs inline images from attachments (`warden-image:<id>`, one-edit publish) — no producer since direct Docs writes were retired (2026-09-17); kept for suggestions to carry images | `policy/images.go`, `chats/images.go`, `imageguard/` | `chats/{id}/images/*`, `/published/<token>.png`, gateway action `unpublish` | `ImageAttachment.tsx` | | [image-attachments-ovh-plan](image-attachments-ovh-plan.md) |
| GitHub repository sharing: checkout, per-repo read categories (contents / issues / pull_requests), GitHub App or user token | `policy/githubapp.go`, `policy/githubuser.go`, `policy/operations.go` (REST catalog), `sandbox/repository.go`, `sandbox/fetch.go` | `sharing/github/*`, `sharing/github_repositories`, `sharing/github_select` | `RepositorySharing.tsx` | `sandbox/repository_test.go`, `repository_acceptance_test.go` | [github-repository-sharing-plan](github-repository-sharing-plan.md), [github-app-integration](github-app-integration.md) |
| Pull request proposals: reviewed single-branch push, owner Review proposal… dialog | `policy/pullrequests.go`, `policy/gitreview.go`, `policy/gitprotocol.go`, `sandbox/publish_plan.go`, `sandbox/review.go`, `chats/proposal_file.go` | `sharing/pr_preview`, `sharing/pr_get`; tool `request_pull_request` | `PullRequestReview.tsx` | `sandbox/publish_plan_test.go`, `review_test.go` | [pull-request-approval-plan](pull-request-approval-plan.md) |
| Inspected egress: gateway CA, per-host leaf certs, provider SSE streaming (`ProviderEndpoints` in `policy/stream.go`: OpenAI, Codex and Anthropic `/v1/messages`; other routes are buffered), destination policy | `policy/ca.go`, `policy/gateway.go`, `policy/gatewaypool.go`, `policy/stream.go`, `policy/egress.go`, `policy/registry.go` | runner→policy `bindGateway`, `configureProvider`, `egress`, `authorize` | | `policy/*_test.go` | [warden-gateway-ca-plan](warden-gateway-ca-plan.md), [response-streaming](response-streaming.md) |
| Egress mode restricted / open (runtime switch) | `policy/egress.go`, `policy.Registry.SetEgressMode` | `sharing/egress`, `sharing/egress_set` | `AdminConsole.tsx` | | |
| Admin console: egress mode, disconnect Google / GitHub, sign-in ledger | `edge/owner.go`, `policy/sharing.go` `disconnect` | `/api/admin/*` (edge), `sharing/disconnect` | `AdminConsole.tsx` | | [admin-console-plan](admin-console-plan.md) |
| Sign-in: owner mode (local capability cookie) and Google (server mode; sessions kept across restarts in `<edge state>/sessions.json`) | `edge/edge.go`, `edge/owner.go`, `edge/ledger.go`, `browserauth/` (`Config.SessionsFile`) | `/auth/*`, `/oauth/*` | `AuthRoot.tsx`, `GoogleLogin.tsx` | `edge/*_test.go`, `browserauth/auth_test.go` | [google-login-publishing-plan](google-login-publishing-plan.md) |
| Provider logins (Codex / Claude / Google / GitHub device flow) | `login/github.go`, `cmd/warden/login.go`, `cmd/warden/github_login.go`, `sandbox/openai.go`, `policy/oauth.go`, `policy/credentials.go` | | `warden login` | | [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Local install: `warden install|doctor|start|open|stop|status|uninstall`, private sbx namespace, runtimes layout, popups | `cmd/warden/*.go` (`install.go`, `doctor.go`, `sbx.go`, `runtimes.go`, `notify.go`, `service.go`), `hostinfo/` | | | `cmd/warden/*_test.go` | [warden-local-install](warden-local-install.md), [warden-local-deployments-plan](warden-local-deployments-plan.md) |
| Terminal client `warden chat` (TUI), `chat list|new|send --wait|approve` | `tui/` | same HTTP API | `tui/app.go`, `tui/follow.go` | `tui/*_test.go` | |
| Releases: version handshake, pinned runtimes, guest image, OAuth clients; artefacts = tarballs, server image, Helm chart (`oci://ghcr.io/monaddle-too/charts/warden`, version = tag without `v`) | `release/release.go`, `handshake/`; `.github/workflows/release.yml`, `.github/workflows/guest-image.yml`, `scripts/release.sh` (`--chart`, `--publish`), `scripts/package-chart.sh` | | | | `scripts/build-guest-image-in-sbx.sh`, [warden-guest-image-plan](warden-guest-image-plan.md), [warden-kubernetes-plan](warden-kubernetes-plan.md) § decision 17 |
| CI: release, guest image and OCSF workflows on the self-hosted Mac runner | `.github/workflows/*.yml`, `deploy/ci/lima-docker.yaml` | | | | [ci-mac-runner](ci-mac-runner.md) |
| Deployment (OVH compose, Caddy, systemd; local `deploy-local.sh`) | `deploy/chat/*`, `deploy/guest/*` | | | | `deploy/chat/README.md`, [ovh-html-demo-plan](ovh-html-demo-plan.md) |
| Kubernetes shape: Helm chart (four Deployments over mTLS, hardened sandbox namespace, admission policy, NetworkPolicies, quota, TLS bootstrap Job or cert-manager, Ingress), Lima/k3s dev loop, GKE Autopilot test cluster with public previews (Cloud DNS zone, cert-manager DNS-01 by Workload Identity, amd64 images built under emulation), operator guide | `deploy/helm/warden/` (`values.yaml`, `templates/_helpers.tpl` renders `warden.json`), `deploy/k8s/dev/{lima.yaml,values.yaml}`, `scripts/k8s-dev.sh`, `deploy/k8s/gke/{env.example,values.yaml,cluster-issuer.yaml}`, `scripts/k8s-gke.sh` | | | `deploy/helm/warden/test.sh` (goldens in `testdata/`), `config/helm_test.go` | [warden-kubernetes](warden-kubernetes.md), [warden-kubernetes-plan](warden-kubernetes-plan.md) |
| Kubernetes config: `runtime.kind`, `kubernetes.*` (`namespace`, `tier`, `runtimeClass`, `guestImage`, `guestImageDigest`, `storageClass`, `workspaceSizeGi`, `gatewayService`, `gatewayPort`, `trustConfigMap`, `nodeSelector`, `tolerations`), `services.{policy,runner,chat}.{listen,address}` and `services.runner.previews`, `tls.{caFile,certFile,keyFile}`, `providers.<p>.secret`, `sandboxes.{cpus,maxCPUs,maxMemoryMB}` | `config/config.go` (`Kubernetes`, `Services`, `TLS`, `RuntimeKind`, `GatewayMode`, validation by kind) | | | `config/config_test.go`, `config/helm_test.go` | [warden-kubernetes-plan](warden-kubernetes-plan.md) appendix A |
| Service transport: `unix://` sockets or mutual TLS `tls://` (identity = certificate CN); `warden tls bootstrap` issues the deployment CA and the four certificates | `transport/transport.go`, `transport/ca.go`, `cmd/warden/tls.go` | | chart `templates/tls-bootstrap.yaml` | `transport/transport_test.go`, `cmd/warden/tls_test.go` | [warden-kubernetes-plan](warden-kubernetes-plan.md) § decision 5 |
| Kubernetes runtime driver: sandbox pods with a workspace PVC, stop/resume, fork by clone or copy, exec, reconcile by label; `warden runner --kubeconfig` for development | `sandbox/kube/driver.go`, `sandbox/kube/spec.go`, `sandbox/kube/exec.go`, `runnersvc/main.go` `runtimeDriver` | runner protocol unchanged | | `sandbox/kube/driver_test.go`, `sandbox/kube/live_test.go` (needs a cluster) | [warden-kubernetes-plan](warden-kubernetes-plan.md) § work item 4 |
| Kubernetes enforcement: inspector (cluster facts, per-pod facts, egress by label), canaries, trust-bundle publisher, Secret credential store; `warden policy --kubeconfig` / `--kube-namespace` for development | `policy/kube/inspector.go`, `policy/kube/clusterfacts.go`, `policy/kube/canary.go`, `policy/kube/refresh.go`, `policy/kube/trust.go`, `policy/kube/secretstore.go`, `policy/kube/labels.go`, `policysvc/main.go` | | chart `templates/sandbox-{namespace,networkpolicies,admission,quota}.yaml`, `templates/rbac.yaml` | `policy/kube/*_test.go`, `policy/kube/live_test.go` (needs a cluster) | [warden-kubernetes-plan](warden-kubernetes-plan.md) § work item 5, decisions 9, 11, 12 |
| Shared gateway: one egress listener for every binding, credential in the proxy URL, 407 challenge | `policy/sharedgateway.go`, `policy/gateway.go` (`BindingGateway`), `config.GatewayMode` | Service `warden-gateway`; `Proxy-Authorization: Basic bindingID:capability` | | `policy/sharedgateway_test.go` | [warden-kubernetes-plan](warden-kubernetes-plan.md) § decision 4 |
| Edge-minted owner capability (owner mode over a `tls://` upstream: the edge mints, persists and logs the launch URL) | `edge/capability.go`, `edgesvc/main.go` | `/auth/*` unchanged | | `edge/capability_test.go` | [warden-kubernetes-plan](warden-kubernetes-plan.md) § step 6 |
| Guest base image for Kubernetes sandboxes (no SBX template; pinned by platform manifest digest) | `deploy/guest/Dockerfile.base`, `deploy/guest/build-base.sh`, `.github/workflows/guest-image.yml` (`warden-guest-base`) | | chart `guestImage.{repository,digest}` | | [warden-kubernetes-plan](warden-kubernetes-plan.md) § work item 6 |
| Audit events / SIEM export | `policy/audit.go`, `policy/redact.go`, `policy/retention.go`, `schemas/audit-event.schema.json` | | | | [siem](siem.md) |
| Untrusted image normalisation | `imageguard/` | | | `imageguard/*_test.go` | |
| Kubernetes end-to-end suite: chat/preview/stop-resume flows and the adversarial networking rows against a deployed release | driven through the edge HTTP API and `kube.Client` exec | uses the edge API and `pods/exec` | `scripts/k8s-dev.sh test` | `chat/tests/k8s/*_test.go` (build tag `k8s`, needs `WARDEN_K8S_KUBECONFIG`) | [warden-kubernetes-plan](warden-kubernetes-plan.md) § work item 8, [warden-kubernetes.md](warden-kubernetes.md) § Development |

## API surface at a glance

Chat service (`chat/internal/chats/http.go`, all under `/api/`, bearer token
from the edge): `state`, `events`, `environments`, `environments/{id}/{stop,
archive,delete,start,resize}`, `chats`, `chats/{id}/{agent,message,typing,edit,stop,
activity,runtime,file,image-file,paths,attachments}`, `chats/{id}/attachments/{aid}[/remove]`,
`chats/{id}/approvals/{rid}`, `chats/{id}/images/*`,
`ports`, `ports/{id}/{revoke,proxy/*}`, `cluster`, `cluster/logs`, `sharing/*` (forwarded to the policy
service: `status, files, select, request, get, resolve, revoke, history,
blocked, block, connect, callback, disconnect, github_list,
github_repositories, github_select, github_write, network_allow, egress,
egress_set, pr_preview, pr_get`).

Runner protocol (`chat/internal/sandbox/client.go`): versioned request/response
over a private socket; responses may carry `Stats` (`hoststats.Sample`, the
runner's host), `Usage` (`SandboxUsage`, one guest), `Progress` (a startup
stage), `Pod`, `Cluster` or `Logs` (the cluster view).

Policy service internal endpoints (`chat/internal/policy/registry.go`):
`register, check, begin, end, gateway, configureProvider, bindGateway, proxy,
egress, egress.finish, authorize, active, event`.

## Docs index

Per-feature plans are `docs/*-plan.md`; the plan that is currently the
mainline's record of progress is
[warden-local-deployments-plan.md](warden-local-deployments-plan.md). The
Kubernetes shape: design and progress in
[warden-kubernetes-plan.md](warden-kubernetes-plan.md), operator guide
[warden-kubernetes.md](warden-kubernetes.md). Architecture and security scope:
[architecture.md](architecture.md). Local install: [warden-local-install.md](warden-local-install.md).
