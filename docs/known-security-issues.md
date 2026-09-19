# Known security issues

Gaps the owner has looked at and chosen to carry, with what bounds each one
and what would close it. An entry stays until the fix lands, then moves to
the closed list with the commit. This is not the threat model — that is
[architecture.md](architecture.md) — and not a place for open questions;
an entry records a decision to ship with a known weakness.

The invariant every entry is measured against: **the agent never holds a
credential of the owner's and never has a path to the host; only the owner
moves code or credentials across a VM boundary.**

## Open

### 1. The agent can imitate Warden's own UI in the transcript

**What.** The transcript renders two kinds of thing: typed items Warden
produced from the agent protocol (tool cards, diffs, approvals, status
lines, the compaction divider, review cards) and the model's own text,
rendered as Markdown (with code, tables, images, Mermaid, math). Only the
first kind is authoritative, but nothing in the rendering stops the second
kind from *looking* like the first. A model can write prose that reads as a
Bash card with a passing test run it never made, an "approved" line, a
system notice, or a summary of a tool result that contradicts the real card
above it. A person skimming a long transcript has no reliable visual cue
that separates a card Warden drew from a card the model drew in Markdown.

**Why it matters.** Warden's controls (permission modes, approvals, the
audit log) are only as good as the owner's ability to tell what actually
happened. A model that misreports its own work — by mistake or under a
prompt injection carried in a repository, a document or a web page — can
get an approval or a merge it would not get on the true record.

**What bounds it today.** Raw HTML is not rendered (the Markdown renderer
has no raw-HTML pass; Mermaid runs at `securityLevel: "strict"`), so an
imitation is an approximation, not a pixel copy. Every real tool call is in
the policy audit log and the runner's records independently of the
transcript. Rewind and the session diff show the workspace's real state.

**What would close it.**
- A visual channel the model cannot reproduce: a mark on every
  Warden-authored item (border, gutter icon, "verified" affordance) with
  model text rendered in a visibly different frame, and no Markdown feature
  that can approximate the mark.
- Cross-checks in the UI: a reply that claims a command ran or tests passed
  with no matching tool card in the same turn gets a "no tool call recorded"
  note; a tool result summary that disagrees with the card is at least
  shown beside it.
- Stronger: quote-style rendering of model text inside a turn that contains
  tool cards, so cards and prose cannot be confused in flow.

Recorded 2026-09-19.

### 2. A workspace VM can reach the host's network, sibling VMs and the LAN

**What.** The workspace VM feature
([workspace-vm-plan.md](workspace-vm-plan.md)) gives a workspace a Lima VM
on the Mac that the agent drives through Warden's tools. A sandbox is
deny-all except the gateway; the VM is not: with Lima's networking it has
open internet *and* an interface onto the host, from which it can reach any
host service listening on a non-loopback address, the other Lima VMs on the
same bridge (the CI runner's VM, the Kubernetes dev cluster) and the local
network. None of that is the agent's.

**Why it matters.** "The agent has a VM" becomes "the agent has a foothold
on the owner's network" for the life of the VM.

**What bounds it today.** The VM holds nothing of the owner's (no mounts,
no credentials — the inner Warden's provider access is brokered through the
outer gateway; images are public). Host services that matter bind to
loopback. The VM is owner-created, owner-stopped, capped in size and count,
and every command the agent runs in it is in the transcript and the audit
log.

**What would close it.** A host-side `pf` anchor per VM address installed
by the runner at create and removed at delete: allow the internet and the
outer gateway, deny the host's other addresses, the other VMs and RFC 1918.
That rule is the VM's `--deny-network` and confines the inner Warden's
sandboxes too, since they NAT through the VM. Deferred by the owner on
2026-09-19; the feature ships without it.

Recorded 2026-09-19.

### 3. A jailbroken workspace's agent is the owner on the Mac

**What.** The jailbreak ([host-dogfood-plan.md](host-dogfood-plan.md),
Part B) lets a local owner give one workspace host access. Its agent then
holds `host_run` (a shell command on this machine, as the owner, through
their login shell), `host_put` / `host_get` (files each way under the
home directory) and `host_expose` (a host port as a preview). Inside that
fence the agent is the owner: anything in the home directory, `~/.ssh`,
what the keychain gives a launchd agent, the outer Warden's own state and
provider sign-ins (the copies refuse that directory; a command does not),
every other Warden instance, the LAN and the internet with the owner's
network position. A prompt injection carried into a jailbroken workspace
(a repository, a document, a page the agent reads) can do what the owner
can do, subject only to the permission mode and rules. The register's
invariant — the agent never holds a credential of the owner's and never
has a path to the host — is suspended for that workspace, by the owner,
visibly.

**Why it matters.** The rest of Warden's design assumes a sandbox is the
security boundary. One jailbroken workspace makes that boundary the
owner's judgement about what they ask the agent to do and what the agent
reads while doing it.

**What bounds it today.** Off by default, absent from every UI and API
unless the owner writes `dogfood.jailbreak` into `warden.json` (or runs
`warden start --jailbreak`), refused at config validation outside a local
owner install, never on a server or Kubernetes, and refused by the runner
unless it was started with the flag, so a chat-service bug cannot reach
it. Per workspace, the owner's opt-in at creation or from the panel,
turned off at once. Marked: a red JAILBROKEN badge Warden draws in the
sidebar, the chat header, the workspace panel and the TUI status line
(known issue 1 applies to everything else in the transcript, not to the
badge). Runner-mediated, never a network path: the sandbox stays
`--deny-network **`, the gateway still refuses loopback; the agent holds
tools, not a shell, and each call is a HOST card in the transcript, a
decision under the chat's permission mode and rules (`deny
mcp__warden__host_run(rm *)` and the like), and an audit entry in the
install-wide chain and the sandbox's own, with the command, cwd, exit,
bytes and duration. Stop kills the command's process group. Copies are
size-capped and stay under the home directory, outside the outer
instance's state.

**What would close it.**
- A dedicated macOS user for `host_run`: the runner would run the command
  as that user (a `sudo` rule, or a second runner process under it), so a
  jailbroken agent owns a scratch account rather than the owner's.
- The `pf` anchor of known issue 2 applied to host commands' network.
- An allowlist of command prefixes in `dogfood.jailbreak` itself, enforced
  by the runner, instead of only in permission rules the model can see.

Recorded 2026-09-19.

## Closed

None yet.
