# Warden platform plan

Status: current product direction, approved September 14, 2026.
Current focus: Warden only.
Deployment: all Warden services and sandbox execution run on OVH. The Mac is a browser/development client, with no production relay or runtime dependency.

## Objective

Make Warden the standalone home for sandboxed agent chats, SBX lifecycle
management, granular permissions, and an MCP server through which agents access
connected applications. Warden must operate without Panta.

This plan supersedes earlier ownership assumptions in which Panta owns agent
chats, orchestrates SBX, or supplies Warden's integration credentials. Existing
implementation and validation records remain historical evidence; this document
describes the intended direction, not features already delivered.

## Ownership

Warden owns:

- SBX creation, start/stop, execution readiness, lifecycle, and cleanup.
- User-facing agent chats: messages, session history, streaming, cancellation,
  steering, and resume, with durable conversation/session state.
- The agent-facing MCP server and its tool contracts.
- Provider connections, credential storage and renewal, and credential isolation.
- Granular tool/action/resource permissions, approval requests, bounded grants,
  expiry, revocation, and audit records.
- The API and UI needed to use these capabilities independently.

Panta remains an application with its own projects, documents, and collaboration
features. In the target architecture it is another Warden integration, alongside
Google Docs, Figma, and GitHub. Panta may eventually consume Warden APIs or display
Warden chats, but it is not required to run or authorize Warden agents.

Do not implement or redesign Panta in the current work. Its future adapter and
any embedded chat experience are deferred. Do not move all Panta application
features into Warden merely because chat execution moves there.

## Target flow

User → Warden chat → Warden-managed SBX agent → Warden MCP server →
permission decision / user approval → authenticated application operation.

Provider consent establishes a Warden-owned account connection. That connection
is distinct from an agent's permission to use it. Agents receive only a scoped
Warden connection, never provider credentials or access to host management APIs.
Bind MCP requests to a trusted sandbox/session identity; do not accept a caller's
claimed user, sandbox, or chat ID as proof of authority.

Warden exposes concrete integration operations as MCP tools and checks the
requested action, resource, and applicable constraints before executing them.
Approval must describe the actual operation and target. Exact or repeated grants
must have clear scope, expiry, and revocation behavior. Dangerous writes retain
appropriate review of their proposed effect. Credentials are attached only on
the trusted side after authorization.

Keep network enforcement beneath the tool layer so a sandbox cannot bypass
Warden by calling a provider directly, using alternate endpoints, or supplying
its own credentials. MCP is the agent interface; it does not itself establish
network containment. Preserve explicit limits on supported providers and routes.

## Current starting point

Existing work includes an HTTP permission proxy, human approvals/audit, SBX
integration work, and GitHub/Figma/Google Docs credential adapters. These are
building blocks, not a completed standalone chat platform or MCP server.

The latest provider prototypes run in separate local control processes and have
not been consolidated into a single Warden product. Google/Figma credentials are
memory-only. GitHub's installation broker currently reads the existing app record
from Panta's encrypted store on OVH. This is a transitional dependency to remove,
not the target ownership model. Existing SBX/chat work on other branches and
uncommitted changes must be inventoried and preserved before integration.

## Implementation sequence

1. **Inventory and consolidate Warden.** Review existing branches, uncommitted
   work, runtime processes, SBX code, chat/streaming work, and provider adapters.
   Identify reusable pieces and actual validation gaps. Establish one Warden
   control-plane contract and durable implementation plan before merging changes.
2. **Standalone SBX and chat foundation.** Warden owns chat/session persistence,
   sandbox bindings, lifecycle, streaming, steering, cancellation, and resume.
   Define restart/recovery behavior and enforce readiness before starting agents.
   Demonstrate a chat without any Panta service or session.
3. **Permission-backed MCP server.** Define a small initial tool surface using
   existing provider operations. Bind clients to sandbox/chat identity, route
   calls through the approval engine, return clear pending/denied outcomes, and
   support safe retry after approval. Separate agent tools from owner controls.
4. **Warden-owned connections.** Consolidate account connection management and
   durable protected credential storage/refresh. Move GitHub app credential
   ownership from Panta's store to Warden without breaking the existing app or
   exposing secrets. Provide user connection flows without pasted provider tokens;
   keep operator app registration separate from user onboarding.
5. **Integrated Warden experience.** Present chats, SBX status, connections,
   pending permissions, revocation, and audit in Warden's UI. Correlate each tool
   call, approval, and result with its actual session and sandbox.
6. **End-to-end acceptance.** Exercise the real chat → SBX → MCP → approval →
   provider flow. Verify denial, resource boundaries, replay, expiration,
   revocation, reconnect/restart, concurrent sessions, and attempted network
   bypass. Document what is demonstrated separately from remaining assumptions.

Resolve implementation details against the existing code during step 1. Avoid
speculative platform expansion before the core standalone loop works.

## First milestone acceptance

- A user opens Warden, starts a chat, and runs an agent in a Warden-managed SBX.
- The agent calls a supported integration tool through Warden MCP.
- An unapproved action cannot execute; the user can inspect and grant a bounded
  permission in Warden, and the agent can then complete the approved operation.
- Another resource or session does not inherit that permission implicitly.
- Provider secrets stay outside the sandbox, and direct provider traffic cannot
  bypass the demonstrated enforcement boundary.
- Warden shows the result and audit trail, supports cancellation/revocation, and
  handles restart/resume explicitly.
- The complete demonstration runs without Panta.

## Scope deferred

Panta changes and its integration adapter; embedding Warden chats in other apps;
additional provider breadth beyond the initial useful tools; broad multi-tenant
hosting/distribution work. These are not prerequisites for the first Warden-only
milestone. Do not start Panta changes as an implicit part of this plan.
Single-owner installs on arbitrary Macs and Linux hosts are planned separately
in [the local deployments plan](warden-local-deployments-plan.md).

## Progress and remaining work

- [x] Record the revised Warden/Panta boundary and Warden-only focus.
- [x] Link this plan from repository entry points and mark older ownership plans
  as historical where their assumptions conflict.
- [ ] Inventory and reconcile Warden implementations and active work.
- [ ] Deliver the standalone chat/SBX/MCP/approval milestone above.

The user subsequently authorized moving Panta's chat components and backend into Warden.
The [chat migration](warden-chat-migration-plan.md) delivers the standalone chat/SBX
foundation; the MCP/provider approval milestone remains outstanding. Provider
credential ownership and Panta itself are unchanged by that extraction.

The chat/SBX foundation, Google sign-in, and approved authenticated port previews now run entirely on OVH. See [the deployment plan](warden-public-previews-plan.md). The Mac relay prototype is retired. The broader unified MCP/provider connection console remains separate work.
