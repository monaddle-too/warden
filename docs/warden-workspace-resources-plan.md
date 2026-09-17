# Workspace resources: chosen at creation, requested by the agent

Status: implemented for SBX on branch `feature/workspace-resources` (from
`origin/main` 442618b), September 17, 2026; the Kubernetes driver's `Resize`
waits for that branch to be synced with main (see Steps). Applies to both
runtime drivers: SBX (`chat/internal/sandbox/runtime.go`) and Kubernetes
(`chat/internal/sandbox/kube`, branch `plan/warden-kubernetes`).

## Objective

A workspace has a CPU and memory size of its own. The person creating a
chat on a fresh workspace picks the size (web form, `warden chat new`, the
API); an agent that runs out asks for more through the same owner-approved
grant flow as network hosts and repositories; the workspace panel shows
what a workspace has. The owner's configuration sets the default and the
ceiling.

## What exists

- One global size. `sandboxes.memoryMB` (512–16384, default 1536) in
  `chat/internal/config`; CPUs are hard-coded to one in both drivers
  (`runtime.go` `--cpus 1`, `kube/spec.go` `cpu: "1"`). Nothing is stored
  per sandbox.
- The sandbox is created lazily by the runner on the chat's first
  `prepare`, from the worker's size; the durable record is `managedSandbox`
  in the runner registry, keyed by sandbox ID; the chat store knows a
  workspace only as the `SandboxID` its chats share, and workspace
  attributes (today: `Repository`) live on the chat and are copied when a
  chat joins the workspace.
- Warm spares are guests booted at the global size and adopted by the next
  fresh sandbox.
- Agent-requestable grants (`chat/internal/chats/grants.go`): a tool call
  becomes a pending approval, the owner answers, the effect becomes the
  tool result. Five methods exist; the shape is reused unchanged here.
- Platform facts, checked 2026-09-17:
  - SBX 0.43.0 sets `--cpus`/`--memory` only at `create`/`run`; there is
    no resize verb and no disk size at all. `sbx template save` snapshots
    a local sandbox's filesystem into a template a new sandbox can be
    created from (`scripts/build-guest-image-in-sbx.sh` relies on it).
  - Kubernetes 1.33+ has the `pods/resize` subresource on by default. On
    the dev cluster (k3s 1.36.4) a memory resize 512Mi → 1Gi took effect
    live under both gVisor and Kata (QEMU): a 900 MiB allocation was
    killed before the resize and succeeded after it, with no container
    restart. What the guest reports does not follow: gVisor's
    `/proc/meminfo` and the Kata VM's `MemTotal` kept their old values.
    CPU was not verified (`nproc` read 2 before and after under both).

## Decisions

1. **A size is CPU in millicores and memory in MiB** (`sandbox.Resources`
   `{cpuMilli, memoryMB}`). Millicores because Kubernetes takes fractions;
   each runner declares the CPU step it accepts in its limits
   (`cpuStepMilli`: 1000 on SBX, whose `--cpus` is an integer, 250 on
   Kubernetes), so the owner's "can we do fractional CPUs" is yes where the
   platform can and whole CPUs elsewhere, decided by the runner, not the
   client. Memory moves in 512 MiB steps. Disk is out: SBX has no disk
   size, and on Kubernetes it is PVC expansion, which the dev cluster's
   local-path provisioner cannot do; it is a real-cluster feature for the
   successor plan.
2. **The runner's record is the truth; the chats carry the size to create
   with.** `Resources` is set on the chat at creation, sent with the
   `bind-chat` that registers the sandbox, adopted by the runner into
   `managedSandbox` and reported in `SandboxInfo` from then on. Every
   change (owner or grant) updates the runner and then the chats on the
   workspace, so a workspace whose sandbox does not exist yet is created
   at the new size and the panel's fallback stays right; once the sandbox
   exists the panel reads the runner. Kept on the chat rather than a new
   workspace record because the chat store has no workspace record and
   `Repository` already takes this shape; introducing a workspace record
   is a separate cleanup, not this plan's.
3. **Configuration is a default and a ceiling.** `sandboxes.memoryMB` and
   a new `sandboxes.cpus` stay the defaults; `sandboxes.maxMemoryMB` and
   `sandboxes.maxCPUs` cap any one workspace, whoever asks. Ceiling
   defaults follow what the platform would refuse anyway: on SBX 75 % of
   host memory and the host's CPUs (SBX's own maximum), on Kubernetes the
   chart's values, with the namespace `ResourceQuota` as the real limit.
   The runner resolves and validates every size (`ResourceLimits.Resolve`);
   the chat validates too, for a prompt answer, with the limits it reads
   from the runner's `health` answer (cached 30 s) and hands to clients in
   `GET state` as `sandboxes`. `maxRunning` stays a count; the worst case
   is `maxRunning × ceiling` and the owner set both. One knob fewer, and
   oversubscribing a local machine is the owner's choice.
4. **Resize is a driver operation with two implementations.**
   - Kubernetes: one PATCH to `pods/resize`, live, no restart. The
     runner's Role gets `patch` on the `pods/resize` subresource only; the
     "runner never patches" rule (kube plan) stays true for the pod
     itself, and the labels remain the policy service's.
   - SBX: regenerate the instance on the same files: stop → `sbx template
     save wc-X <tag>` → `sbx rm wc-X` → `sbx create --name wc-X --cpus
     --memory --template <tag> --deny-network '**' --no-share-skills
     shell` → `sbx template rm <tag>`. Same name, so the registry entry
     and the chat bindings do not change. The reason is the platform: SBX
     cannot change a sandbox's limits, and the alternative (copy
     `/home/agent` out, create from the guest image, copy in, reinstall
     runtimes) is slower and loses the runtime install fingerprints.
5. **Spares are adopted only when they fit.** A spare is booted at the
   default size. Kubernetes adopts and resizes it in place. SBX creates a
   non-default workspace directly (slower first message, said in the
   form), because resizing an SBX spare is the regeneration above and
   would cost more than it saves.
6. **A grant's effect depends on the platform, and the agent is told.** On
   Kubernetes the tool result is "granted, N MiB and M CPUs, in effect
   now; the guest may still report the old total". On SBX the tool result
   is "granted, the sandbox restarts with N MiB and M CPUs", the run ends
   at that turn, the runner regenerates, and the chat resumes the run the
   way stop/resume already does (session state is under `/home/agent`).
   The agent asked from inside the instance being replaced; there is no
   way to keep its process.
7. **Only growth is agent-requestable; the owner can shrink.** An agent
   asks for more, never less (a smaller request is refused with the
   current size and a pointer to the panel). The owner sets any size
   within the ceiling from the workspace panel's Size section (same RPC),
   larger or smaller; on SBX that needs the workspace's chats stopped
   first, so nothing is interrupted behind their back, and the form's
   button says so. A Kubernetes memory decrease the kubelet refuses in
   place falls back to a new generation (stop, create at the new size),
   which the driver already does for a stopped workspace.
8. **A shared workspace has its size already.** `POST chats` with both
   `sandboxID` and `resources` is refused; the form hides the size fields
   when an existing workspace is chosen.

## Work

### 1. Model and configuration

- `sandbox.Resources{CPUs, MemoryMB int}` with `Validate(ceiling)`;
  zero means the default.
- Config: `sandboxes.cpus` (default 1), `maxMemoryMB`, `maxCPUs`;
  detection fills the SBX ceilings from host facts (`hostinfo`); the
  appendix of the local-deployments plan gains the fields.
- `Chat.Resources`, `Request.Resources` (client.go), `SandboxInfo.Resources`
  and `managedSandbox.Resources` (registry; older entries read back as the
  default, no migration).
- The view's `State` carries `sandboxes: {defaults, ceiling, platform}` so
  the clients can offer choices without a second endpoint.

### 2. Creation

- `Engine.Create` takes `Resources`; validated against the ceiling;
  refused with `sandboxID`.
- Web form: two selects under "Create a fresh workspace", CPUs
  1…`maxCPUs` and memory in steps 512 MiB … `maxMemoryMB`, defaults
  preselected, with the "will not use a warm spare" note on SBX when the
  size is not the default.
- `warden chat new --cpus N --memory 4g`; `tui.Client.Create` grows the
  same two fields.
- Runner: `prepareLocked` adopts a spare only when it fits (decision 5),
  otherwise creates at the requested size; `RuntimeSpec` carries the size
  and both drivers' `Create` read it instead of the worker's.

### 3. Resize

- `RuntimeDriver.Resize(ctx, name, Resources) (restarted bool, error)`.
- Kubernetes: PATCH `pods/resize`; wait for the allocated resources in
  `status.containerStatuses[0].resources` to match; chart Role gains the
  subresource verb.
- SBX: the sequence in decision 4 with a temporary tag
  `warden-resize-<sandboxID>`; failure before `sbx rm` leaves the old
  sandbox untouched; failure after it recreates from the tag on the next
  `prepare` (the tag is kept until the new sandbox is confirmed).
- Runner RPC `resize` (chat → runner): validates against the ceiling,
  calls the driver, updates the registry, and when `restarted` marks the
  chat's active run for resumption exactly as an idle stop does.
- Workspace panel: a Resources row (`2 CPUs · 4 GiB`) with an owner edit
  that calls the RPC.

### 4. The grant

- Tool `request_resources {cpus?, memory_mb?, reason}` → method
  `warden/sandbox/resources`; params say current and requested size and
  the reason; decrease and over-ceiling requests are answered at once
  without asking (like an already-satisfied repository request).
- Approval performs the `resize` RPC; the tool result is decision 6's
  text with the real numbers; the Approvals component, the terminal
  client and the popups render the new method (text only, same as the
  others).
- Access history records it (`sharing/history` already lists grants).

### 5. Verification

- Kubernetes, both RuntimeClasses: the memory probe above as a live test
  next to `TestLiveDriver`; a CPU probe (a spinner's throughput before and
  after 1 → 2 CPUs); a decrease that the kubelet refuses, falling back to
  a generation.
- SBX: template-save round trip preserves `/home/agent`, the runtime
  install (`Installed`, `ClaudeInstalled`, `ProxyCA` fingerprints still
  match, no reinstall on the next prepare) and the network deny; a chat
  resumes after a regeneration mid-run with the agent's next turn seeing
  the new limit.
- Both: the form's defaults and ceilings match the config; a shared
  workspace refuses a size; `warden chat new --memory 32g` is refused
  with the ceiling in the message.

## Owner decisions (2026-09-17)

- CPUs: fractional where the platform takes them (Kubernetes, in quarter
  CPUs), otherwise any whole number up to the ceiling; no presets. The
  selects offer the sizes people reach for plus the current one.
- The owner can shrink from the panel; agents only grow.
- A stopped SBX workspace is regenerated at the new size right away (it is
  just a stop-less version of the same sequence).

## Steps

- [x] 0 Model (`sandbox/resources.go`), config fields, runner limits
      (`runnersvc/config.go` `resourceLimits`), `health` carries them,
      `GET state` carries them as `sandboxes`; the SBX `Create` reads the
      spec's size. No behaviour change at the defaults.
- [x] 1 Creation: `POST chats` `resources` (refused with `sandboxID`),
      the form's Size fieldset (`SizeSelect.tsx`), `warden chat new --cpus
      --memory`, `bind-chat` registers the size, spares adopted only at
      the default size.
- [ ] 2 Kubernetes `Resize` (PATCH `pods/resize`, wait for the allocated
      resources), the chart Role's `pods/resize` patch verb, the kube
      `Create` reading `spec.Resources`, `Restart: false` and
      `CPUStepMilli: 250` in that runner's limits, a live test beside
      `TestLiveDriver`. Blocked on syncing `plan/warden-kubernetes` with
      main: the branches conflict in `sandbox/managed.go` and
      `sandbox/runtime.go`, which this feature touches, so the kube half
      is written once against the merged tree rather than twice.
- [x] 3 SBX `Resize` by regeneration (`sbxRuntime.Resize`: stop, `template
      save`, `rm --force`, `create` from the tag, `template rm`), the
      runner's `resize` op (`resizeLocked`), resume-after-restart on the
      chat side (`restartResized`: Stop → resize → Warden's note queues
      the next run). Verified against the real daemon by
      `WARDEN_SBX_LIVE=1 go test ./internal/sandbox -run TestLiveSBXResize`
      (marker kept, 1→2 CPUs, 512 MiB→1 GiB, nothing left behind, 20 s).
- [x] 4 The grant (`request_resources` → `warden/sandbox/resources`,
      `chats/resources.go`), its card in `Approvals.tsx`, the panel's Size
      section with the owner's edit (`environments/{id}/resize`).
- [ ] 5 Verification on a running install (a chat whose agent asks, the
      restart, the resumed turn seeing the new limit), the Kubernetes CPU
      probe, docs: the local-install guide gains a "Workspace size"
      section once the Kubernetes half lands and the wording covers both.

## Progress

- 2026-09-17: feasibility checked on both platforms; live memory resize
  proven on the dev cluster under gVisor and Kata (512 Mi → 1 Gi, a
  900 MiB allocation killed before and surviving after, no restart; the
  guest-reported totals did not follow); plan written.
- 2026-09-17: steps 0, 1, 3, 4 implemented and tested on
  `feature/workspace-resources` (Go race suite, vet, web build and tests
  green; the SBX driver's live test passed on the owner's Mac). Kubernetes
  half (step 2) deferred to the branch sync, see the step.
