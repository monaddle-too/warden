# Chat service responsiveness

Status: started 2026-09-18 on branch `fix/chat-service-responsiveness` from
origin/main b2a11c8.

## Objective

The local Warden's web UI froze for seconds at a time and `warden serve`
burned ~30 % of a core with nobody using it. Diagnosed on the owner's Mac
with a symbolised build of the live release (`sample` on the process):

1. **The freeze.** `Engine.View` (every event-stream tick of every tab,
   `GET state`) called `Engine.Limits`, which under `limitsMu` asked the
   runner for `health` with a 5 s timeout, and a failed call did not
   refresh `limitsAt`, so the next `View` asked again. `health` took the
   runner's `w.mu`, which `SweepIdle` (once a second) held across
   `Runtime.Stop` — an `sbx stop` subprocess, seconds each, up to 30 s —
   and `maintainSpares` across `Runtime.Remove`. While a workspace was
   being stopped, every tab's stream and every state read queued on
   `limitsMu`, 5 s each, serially.
2. **The idle CPU.** `HTTP.events` re-built the whole view every 200 ms
   per tab: `Store.Snapshot` marshalled the entire state (660 KB for 32
   chats) and unmarshalled it back under `Store.mu`, `state` recomputed
   every chat's spend, then the handler marshalled it again to compare
   with the previous frame; ~7 % of a core per tab.
3. **Per-token writes.** Each streamed delta ran `Store.update`, which
   marshalled the state twice (before / after) and `save` a third time,
   then wrote and fsynced 660 KB, all under `Store.mu` — 12 ms per
   token, tens of tokens a second.
4. **Every `Store.Snapshot`** (70 call sites) was that same 660 KB JSON
   round-trip under the lock.
5. **The edge** built a new `httputil.ReverseProxy` *and* a new
   `http.Transport` per request, so every upstream keep-alive connection
   sat in an orphaned pool until the chat service's idle timeout reaped
   it: 280 edge→chat connections for 6 browser ones.

## What exists

- `chat/internal/chats/store.go`: `Store` (`mu`, `state`, `path`,
  `failed`), `update` (marshal before, apply, marshal after, compare,
  `save` marshals again + temp file + fsync + rename + dir fsync),
  `Snapshot` (marshal + unmarshal under the lock).
- `chat/internal/chats/engine.go`: `View` → `state` (`Snapshot`, spend,
  startup, typing) + `Limits` (runner `health`, 30 s TTL, 5 s timeout
  under `limitsMu`) + catalog; `notification` applies every agent frame
  through `Store.update`; `Serve` snapshots the store on every wake.
- `chat/internal/chats/http.go`: `events` (200 ms ticker, full view
  marshalled per tick, sent when the bytes differ or 5 s passed), `GET
  state`.
- `chat/internal/sandbox/managed.go`: `handle` (`health` under `w.mu`),
  `SweepIdle` / `stopLocked` (`Runtime.Stop` under `w.mu`),
  `maintainSpares` (`Runtime.Remove` under `w.mu` on failure),
  `lifecycleLoop` (1 s ticker). `w.Limits` is set once by
  `defaultsLocked` from `initializeManaged`, before `Serve` accepts.
- `chat/internal/edge/edge.go`: `proxy` (per-request `ReverseProxy` +
  `Transport`).

## Steps

1. `Engine.Limits` never blocks a view: a background refresher
   (started by `Serve`) keeps `e.limits` current; `Limits` returns the
   cached offer, fetching synchronously only when nothing is cached yet
   (the New chat form before the first refresh); a failed fetch still
   sets `limitsAt`, so it is retried at most once per TTL; the RPC is
   made outside `limitsMu`.
2. Runner: `health` answers from an atomic copy of the offer
   (`Worker.offer`, settled by `defaultsLocked`) without `w.mu`.
3. Store: a version counter and a cached encoding per version. `update`
   marshals once (the previous encoding is the "before"), `save` writes
   the bytes it is given; `Snapshot` decodes the cached encoding outside
   the lock; `stream` (streaming deltas and other per-token frames)
   applies in memory and coalesces the write (250 ms), flushed before
   any durable `update` and at `Close`; `Wait(ctx, since)` wakes on the
   next mutation.
4. Engine + events: one encoded view per generation (store version +
   typing + startup + limits changes), shared by every client and `GET
   state`; the stream sends when the generation changed (paced to 200 ms
   at most) or 5 s passed, and does nothing while idle.
5. Edge: one `http.Transport` (`Server.upstream`) for the server's
   lifetime, carrying every proxied request and the edge's own calls;
   the `ReverseProxy` value with its per-request `Director` (identity
   headers, binding path) is still built per request, which costs
   nothing.

Each step verified by the package's tests, then the whole by a local
deploy: `sample` on `warden serve` idle (no JSON work), `GET state`
latency while a workspace stops, edge→chat connection count.

## Key decisions

1. Streaming deltas are not durable per token: a restart loses at most
   the last 250 ms of streamed text, and a restart already marks the chat
   interrupted. User messages, approvals and every other mutation stay
   write-before-acknowledge.
2. `Snapshot` keeps returning an independent copy (its callers mutate
   what they get, `state` in particular), decoded from the cached bytes
   outside the lock; the lock is held only to fetch the slice.
3. The runner's mutex discipline is left as it is. Reading further, it
   holds `w.mu` across every long operation, `prepare` for the whole VM
   boot included, not only the idle stop; the chat's views therefore
   stalled on every workspace start too. Moving one stop out from under
   the lock would have been partial and unsafe (a stop running unlocked
   beside a prepare of the same sandbox running locked); `health` not
   needing the lock removes the coupling for all of them.

## Progress log

- 2026-09-18: diagnosed on the owner's Mac (see Objective); worktree and
  plan opened.
- 2026-09-18: steps 1, 3, 4 (a004c7e) and 2, 5 (99d4168) done, with
  tests: `store_test.go` (coalesced streaming writes, one encoding per
  version, `Wait`, a view that does not wait on a stalled runner and is
  encoded once per generation, a stream that sends on change only),
  `worker_test.go` (`health` under the held mutex), `edge_test.go`
  (upstream connections reused). `TestBugReportRetentionAndCap` in the
  edge package fails on origin/main too (unrelated; main fixed it in
  f55de51, merged in at cffa842).
- 2026-09-18: a sixth cost found on the first live deploy: the workspace
  panel's `GET environments` poll (5 s per tab) made three runner calls
  per workspace, and `Engine.Runtime` decoded the whole store for each
  (~60 decodes per poll per tab, ~17 % of a core). `Store.Chat` copies
  one chat; every inline `Snapshot().chat(id)` reads through it
  (14a0ec6).
- 2026-09-18: live-verified on the owner's Mac (deploy-local
  v0.0.0-dev.14a0ec682cf6, three browser tabs open, 673 KB state):
  `warden serve` idle at ~1.2 % of a core (was ~28 %); a running VM
  stopped from the panel held the runner 5.6 s while `GET state` stayed
  at ~1 ms throughout (was 5 s per read); a 400-word streamed reply:
  `GET state` p90 12 ms, max 38 ms, peak 30 % of a core (was 48 %); the
  event stream idle sends keepalives only; edge→chat connections 7 for 3
  tabs (was 280 for 6). Not merged.
- 2026-09-18: merged to main as cde9bca (fast-forward; Go 25 packages and web 292 tests green on the merged tree). GKE deploy next.
- 2026-09-18: deployed to GKE from main abc9701 (image `v0.1.0-alpha.13-72-gabc9701`, helm rev 31): all four rollouts complete, canaries passed, the live workspace pod kept across the restart, the public host answers. Nothing remains; the worktree can go.
