# Per-workspace network access

Status: started 2026-09-18 on branch `feat/workspace-egress` from
origin/main 863acb8.

## Objective

Today the egress mode (restricted: the template's destination list; open:
any public HTTP/HTTPS host, credentials still only for approved requests)
is one switch for the whole install: `sandboxes.egress` in `warden.json`,
overridden at runtime by the admin console's Network access section
(`sharing/egress_set`, persisted as `<policy state>/egress.json`).

After this work a workspace can have its own mode. The owner chooses it
when the workspace is created (the New chat form, `POST chats`, `warden
chat new --network`) and changes it later from the workspace panel; the
default is "the install's setting", so nothing changes for workspaces that
do not choose. Agents cannot request it: `request_network_access` stays a
per-host, bounded grant, and there is no tool for the mode.

The admin console's switch keeps its meaning for every workspace without a
choice of its own, and says how many workspaces override it.

## What exists

- **Policy engine, one per sandbox.** `policy.Engine` (`chat/internal/policy/engine.go`)
  owns a sandbox's `policy.json` under `<state>/sandboxes/<binding digest>/`,
  created from the template at `Registry.load` with `EngineOptions.EgressMode`
  forcing `egress.mode`. `Engine.SetEgressMode(mode)` rewrites one engine's
  mode in place, keeping grants and dropping in-flight leases. `AuthorizeEgress`
  asks `EgressPermits(e.egressPolicy(), …)`, so a per-engine mode is enforced
  by the gateway with no gateway change. The state directory is keyed by the
  binding digest, which changes when the sandbox is regenerated (resize,
  keepalive re-adoption), so nothing per sandbox survives there.
- **Registry** (`chat/internal/policy/registry.go`): `Bindings` by sandbox
  ID; `SetEgressMode` loops over every binding's engine, sets
  `options.EgressMode` for later ones and writes `egress.json`; `EgressMode()`
  reports mode and source (`config` / `console`); `AllowHost(sandbox, host,
  until)` is the existing per-sandbox network operation.
- **Sharing operations** (`chat/internal/policy/sharing.go`): `egress` /
  `egress_set` (the `EgressSwitch` interface, `sharing.Egress = registry`),
  `network_allow` (the `NetworkGrants` interface, `sharing.Network =
  registry`), reached from the chat service by `Engine.sharingCall`
  (`chat/internal/chats/sharing.go`). The edge admits `api/sharing/egress_set`
  to the owner only (`edge.go` ~471).
- **Chat service.** `Chat` (`chats/store.go`) carries `SandboxID` and the
  creation-time `Resources`; a workspace is every chat with the same
  `SandboxID` (`st.environmentChats`). `Engine.CreateFrom` takes the creation
  options; `Environment` (`chats/environments.go`) is what the panel reads;
  `ResizeEnvironment` + `recordResources` are the pattern for an owner action
  on a workspace recorded on all its chats. `Fork` shares the workspace (same
  `SandboxID`) or copies it (`copyWorkspace`, a new sandbox ID).
- **Clients.** New chat form in `ChatShell.tsx` (`create()`, `SizeSelect`);
  workspace panel `WorkspacePanel.tsx` (the Resources section with "Change…"
  is the model); CLI `warden chat new` in `chat/cmd/warden/chat.go`
  (`--cpus`, `--memory`); the TUI has no workspace panel (out of scope
  beyond showing the mode in `/status`-like output if cheap).
- **Admin console** `AdminConsole.tsx` Network access section; this branch's
  first commit (4ed41a5) made its failure path legible.
- **Unmerged neighbour:** `feat/create-with-repositories`
  (`.local/warden-create-with-repositories`) adds `repositories` to the New
  chat form and `POST chats`; expect a small merge in `ChatShell.tsx`,
  `http.go` and `chat.go`.

## Design

- Values: `""` (follow the install; the default), `"restricted"`, `"open"`.
  Wire names match the console's (`restricted` / `open`); the engine's
  internal `public` stays internal.
- The policy service is the source of truth for enforcement. It keeps the
  per-sandbox choices in `<state>/egress-overrides.json` (`{sandboxID:
  mode}`), keyed by sandbox ID so they survive regeneration and restarts,
  applied in `Registry.load` (the override wins over `options.EgressMode`)
  and at once to a live binding's engine on change. Clearing an override
  returns the sandbox to the install's mode immediately.
- `sharing/egress` and `egress_set` gain an optional `sandboxID`: with it
  they read / set that sandbox's override (`mode: ""` clears); without it
  they are the global switch as today. `egress` without a sandbox also
  reports `overrides` (the count) for the console.
- The chat service records the choice as `Chat.Network` on every chat of the
  workspace (like `Resources`) so the panel and the New chat flow can show
  it without a policy round trip, and forwards it to the policy service at
  creation (before the sandbox exists — the override is by ID), on change,
  and for a fork copy (the copy inherits the original's choice).
- Owner only, end to end: the edge admits the new route to the owner as it
  does `egress_set`; the agent gets no tool and no prompt text about it.

## Steps

1. Policy service: `Registry.SetSandboxEgress(sandbox, mode)`,
   `SandboxEgress(sandbox)`, the overrides file, application in `load`;
   `egress` / `egress_set` with `sandboxID`; unit tests (override applied
   on load, changed live, cleared, survives a registry restart, global
   switch leaves overridden sandboxes alone).
2. Chat service: `Chat.Network`; `CreateFrom` takes it and calls
   `egress_set`; `environments/{id}/network` (owner) sets it for the
   workspace; `Environment.Network`; fork copy carries it; tests with the
   scripted policy fake.
3. Edge: admit `api/environments/*/network` to the owner only; `POST chats`
   with `network` likewise owner-only when set.
4. Web: New chat form control (default "Install setting (restricted)" /
   "Open" / "Restricted", showing the install's current mode); workspace
   panel Network section with the current mode, its source, and "Change…";
   admin console shows the override count.
5. CLI: `warden chat new --network restricted|open`; `warden chat`'s
   listing unchanged.
6. Docs: `docs/feature-map.md` (egress mode row), `docs/warden-local-install.md`
   (the network section), this plan's log.
7. Live test on a cloned home (`.local/clone-warden-home.sh`) and on GKE:
   a workspace opened "open" reaches a public host while a sibling
   workspace is refused; the global switch does not touch it; clearing it
   does.

## Key decisions

1. Owner-set only (creation and the workspace panel); agents may not
   request the mode (owner, 2026-09-18). `request_network_access` remains
   the agent's per-host path.
2. Per workspace, not per chat: chats sharing a workspace share its
   sandbox and gateway, so the mode is a property of the sandbox.
3. Overrides live in the policy service keyed by sandbox ID, not in the
   engine's per-binding directory, so a regenerated sandbox keeps its mode.
4. Only the owner chooses, end to end. The edge forwards `X-Warden-Role:
   admin` on the owner's requests (stripped from clients like the other
   identity headers) because `POST chats` cannot be gated by path; the
   chat service refuses `network` on `POST chats` and the
   `environments/{id}/network` route from anyone else (an edge-less
   request is the owner's), and the edge also reserves the route.
5. Wire values are the console's (`restricted` / `open`, `""` to follow);
   the engine's `public` stays internal to the policy package.

## Progress log

- 2026-09-18: worktree opened; first commit 4ed41a5 (console failure
  messages, from the debugging of a click lost to a redeploy).
- 2026-09-18: steps 1–6 done. Policy 638e7be (`SetSandboxEgress`,
  `egress-overrides.json`, scoped `egress`/`egress_set`; registry and
  sharing tests). Chat service + edge dae64db (`Chat.Network`,
  `network.go`, creation / route / fork copy / shared-workspace
  inheritance; owner gate; `network_test.go`, edge tests). Web 2d094ac
  (`network.ts`, `NetworkSelect`, New chat fieldset, panel section,
  console override count). CLI 18a4070 (`--network`). Docs in this
  commit. Full Go, web and CLI suites pass. Step 7 (live test on a cloned
  home and on GKE) remains.
