# Size picker: every host core, and the host's live capacity

Status: started 2026-09-18 on branch `feat/size-picker-capacity` from
origin/main 7dcfa5a.

## Objective

Two things the owner hit on the local install. First, the workspace size
picker offered only one CPU on a 14-core Mac: the runner's ceiling was
1 CPU although nothing in `warden.json` set one. Second, when choosing a
size, the question "how much memory is actually free, how many cores are
there, what have the other workspaces already taken" had no answer in
the UI; the picker now shows the host's live capacity next to the
choices, on the New chat form and in the workspace panel's Change… row.

## What exists

- `services/runnersvc/config.go` `resourceLimits` derives the SBX offer:
  an unset ceiling is meant to fall back to 75 % of the host's memory and
  all its cores. The CPU ceiling goes through `wholeCPUs`, which is
  `CPUsFromSpec(CPUMilli(x))`; `CPUsFromSpec(0)` returns 1, so an unset
  `maxCPUs` becomes 1000 millicores and the host fallback (`== 0`) never
  runs. Memory does not use the helper, so its fallback works (the live
  runner reported `max: 1 CPU · 36 GiB` on a 14-core, 48 GiB host). No
  test covered `resourceLimits`.
- `hoststats` (`Collector.Snapshot`): CPU percent from `/proc/stat`
  deltas, cores, load, uptime, memory total/available, disk — Linux only;
  the other build returns an error. The runner's op `stats` returns it
  with `ActiveSessions`/`SessionLimit`; nothing calls `stats` any more.
- The runner's `health` op carries `Limits`; `Engine.Limits` caches it 30 s
  and `GET state` publishes it as `sandboxes`; `SizeSelect.tsx` builds its
  ladders from it (`ChatShell.tsx` form, `WorkspacePanel.tsx` Change…).
- Kubernetes: `ClusterInspector.Cluster` lists nodes with allocatable and
  live usage (metrics.k8s.io) — the cluster-shaped answer to the same
  question.
- Managed sandboxes (`managedSandbox.SandboxInfo.Resources`, `State`) say
  what each running workspace was given.

## Steps

1. `resourceLimits`: an unset `maxCPUs` stays 0 so the host's cores apply;
   tests for the derived ceiling (unset, set, fractional, above the host).
2. `hoststats` on macOS: cores from the runtime, load from
   `sysctl vm.loadavg`, memory from `hw.memsize` and `vm_stat` (free +
   inactive + speculative + purgeable pages), disk from `statfs`; CPU
   percent stays unknown (nil) there.
3. Runner op `capacity`: the host sample (SBX) or the cluster's allocatable
   and usage summed over ready nodes (Kubernetes), plus what running
   sandboxes reserve (sum of their sizes) and how many are running, and
   the limits. `stats` stays as it is.
4. Chat: `GET capacity` → `{kind, host, reserved, running, limits, at}`,
   cached 3 s like usage; forwarded by the edge for any admitted member.
5. Web: `SizeSelect` takes `capacity` and shows one line under the selects
   ("14 CPUs, load 5.1 · 21 GiB of 48 GiB free · 2 running workspaces hold
   3 CPUs · 6 GiB"), and a warning when the chosen memory exceeds what is
   free now. The form and the panel fetch it when the picker is shown and
   every 5 s while it is.
6. Feature map rows, this plan, a live check on the local install.

## Key decisions

1. A new op `capacity` rather than repurposing `stats`: `stats` is the
   old server-side metrics shape (`ActiveSessions`, `SessionLimit`); the
   picker needs the reservation sum and the cluster shape too.
2. "Available memory" on macOS is free + inactive + speculative + purgeable
   pages: what the kernel can hand out without swapping, the figure
   `MemAvailable` gives on Linux. Cached / compressed memory is not
   counted as free.
3. The capacity line is informational; the ceiling still comes from the
   limits. Exceeding what is free now is a warning, not a refusal — the
   host may free memory, and SBX itself decides at start.

## Progress log

- 2026-09-18: steps 1–6 done (5db320d, f7754cb; merged origin/main's
  background service in c496372). Live on `~/.warden/release` (build
  c496372, restarted as the launchd service): the runner's ceiling is
  14 CPUs · 36 GiB on this 14-core, 48 GiB Mac (was 1 CPU); `GET capacity`
  answers `host` with cores, load, memory free of total, disk, the warm
  spare's reservation; the New chat form and the panel's Change… show
  "Host: 14 CPUs, load 6.6 · 10.3 GiB of 48 GiB memory free · 1 warm
  spare holds 1 CPU · 2 GiB", the ladders run to the ceiling (14, 36 GiB),
  and choosing 16 GiB against ~10 GiB free shows the warning. Kubernetes
  path unit-tested (`fillFromCluster`), not live-tested.
- Also: the ladders include the limits' maxima (a 14-core host offered
  1–12 before), `cpuChoices`/`memoryChoices` moved to `sizes.ts`.
- Remaining: merge to main; a live look at the cluster line on GKE some
  day; the TUI's `warden chat new --cpus/--memory` has no capacity hint.
