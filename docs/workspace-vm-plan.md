# Workspace VM

Status: planning, started 2026-09-19 on branch `plan/workspace-vm` from
origin/main 1725595. No code until the owner has read this plan.

## Objective

Warden can be developed and tested from inside Warden. A workspace can have
a **VM** attached: a Lima virtual machine on the Mac, created by the owner,
that the agent drives through Warden's tools. Inside it the agent clones the
repository, builds its branch and installs a full Warden — the Kubernetes
shape on k3s with the gVisor tier — whose provider access is brokered
through the outer Warden's gateway, so the VM holds no credential of any
kind. The owner opens the inner Warden as a preview. Nothing the agent
builds touches the host.

Out of scope for the first cut, each recorded in
[known-security-issues.md](known-security-issues.md) or below: the
host-side network confinement of the VM (`pf` anchor), SSH from the sandbox
into the VM, the SBX-runtime flavour of the inner Warden (needs a Docker
sign-in inside the VM — a dedicated account, entitlement to be checked), and
the dogfood-only host deploy mode.

## Decisions taken with the owner (2026-09-19)

1. The dev loop runs in VMs, never on the host: the agent's code is built
   and run inside the workspace VM; the host runs only published releases.
2. No git hosting on the host; code leaves the agent's VMs only through
   `request_pull_request` (owner-reviewed).
3. The inner Warden uses the Kubernetes runtime (k3s + gVisor) so that no
   Docker sign-in is needed; the SBX flavour is a later, opt-in template.
4. The inner Warden's provider credential is not a key in the VM: its
   gateway chains to the outer gateway, which recognises the VM's binding
   and injects the owner's token (the placeholder swap the sandboxes use
   today).
5. The agent reaches the VM through runner-mediated tools (every command a
   transcript card and an audit entry), not SSH; SSH may come later as an
   ergonomic addition, not a security change.
6. The `pf` anchor confining the VM's network is deferred (known issue 2).
7. A **host deploy** mode ("let the agent deploy to this Mac") will exist
   for dogfooding only, behind `dogfood.hostDeploy: true` in `warden.json`,
   absent from the UI otherwise, owner-approved per deploy, into a second
   Warden home with scoped keys. Designed in a later section, not in the
   first cut.

## UI flow

- New chat form, beside Size and Network: **VM** off / on with CPUs,
  memory, disk and a template (`k3s`: Ubuntu 24.04, Docker, k3s, nested
  virtualization on). Owner only; local Mac installs only. Also **Add VM…**
  from the workspace panel later. One VM per workspace.
- Startup stages in the chat's status line: creating the VM, downloading
  the image (first time), provisioning, VM ready.
- Workspace panel, **VM** section: name, state, size, IP, snapshot; Start /
  Stop / Snapshot / Reset to snapshot / Delete; a copyable
  `warden vm shell <name>`; the ports the agent exposed as previews.
- Transcript: `vm_run` as a Bash-style card (command, exit, output tail);
  `vm_put` / `vm_get` as file cards; `vm_expose` as a preview card.
  Permission rules apply as to any MCP tool (`mcp__warden__vm_run(…)`).
- Stop/Archive stop the VM with the workspace; Delete deletes it and says
  so. Idle stop follows the workspace's.
- CLI `warden vm list|shell|start|stop|snapshot|restore|rm`; TUI `/vm`.

## What happens

- Runner op `vm.create` runs `limactl` under a private
  `LIMA_HOME=<state>/runner/lima` from an embedded template: image pinned by
  digest, `vmType: vz`, `nestedVirtualization: true`, `mounts: []`,
  `networks: [{vzNAT: true}]`, a provision script for Docker, k3s and the
  Warden prerequisites. Sizes and count capped by `vms.limits`.
- State: `Environment.Machine {name, state, resources, ip, snapshot,
  createdAt}`; routes `environments/{id}/vm` and
  `…/vm/{start,stop,snapshot,restore}`; runner ops
  `vm.{create,start,stop,delete,snapshot,restore,status,exec,put,get}`.
- Agent tools in the `warden` MCP server: `vm_run {command, cwd, timeout}`,
  `vm_put {from, to}`, `vm_get {from, to}` (size-capped), `vm_expose {port,
  name}` (an edge preview binding to `vmIP:port`), `vm_status`. Executed by
  the runner over `limactl shell` / `limactl copy`; no network path from the
  sandbox to the VM.
- Provider chaining: the runner registers the VM (address + a per-VM
  placeholder) as a binding with the policy service at create; the gateway
  accepts that source; the policy gateway gains an **upstream gateway**
  mode (`gateway.upstream: {url, ca}`: CONNECT through a parent, trust its
  CA); the runner writes `/etc/warden/upstream.json` into the VM and
  `warden install` inside reads it and configures the provider as brokered
  upstream. Every provider call from the VM is in the outer audit log.
- Audit: every `vm.*` op with chat, principal, command, exit status and
  byte counts.
- Host prerequisites: Lima with `vz` nested virtualization (Apple Silicon
  M3+, macOS 15+), checked by `warden doctor`.

## Security stance

Holds: owner-allocated resource granted once at attach; no host exposure
(no mounts, no Docker socket, no keychain, no outer state); the owner's
credentials never enter it; the agent holds a tool, not a token, and every
use is logged and stoppable; capped size and count, one per workspace,
lifecycle tied to the workspace.

Accepted, recorded in [known-security-issues.md](known-security-issues.md):
the VM's network position until the `pf` anchor lands.

## Steps

To be ordered once the owner has read the plan. Candidate order:

1. Policy gateway upstream mode + VM bindings (the credential-free inner
   provider), unit-tested with the pipe fakes.
2. Runner `vm.*` ops on Lima, the embedded template, `warden doctor` check.
3. Chat store state, routes, New chat form, workspace panel section, CLI.
4. Agent tools and transcript cards; startup stages.
5. `warden install` reading `/etc/warden/upstream.json`; the k3s inner
   install proven end to end from a chat.
6. Feature-map rows and glossary entries.

## Progress log

- 2026-09-19: plan opened; decisions 1–7 taken in conversation with the
  owner; [known-security-issues.md](known-security-issues.md) created with
  the two entries the owner asked for.
