# Spare sandboxes and resident Codex sessions

Branch `codex/warden-spare-sandbox`, started 2026-09-15 from main c6b8fff.

## Objective

After the guest image, a brand-new environment still spent 6.7 s in
`sbx create` (guest boot, including the Docker daemon the shell-docker
flavour starts) and every Codex message spent 1.8 s starting the agent CLI.
Owner decisions: keep a booted spare guest ready and let it overprovision
memory beside the resident limit, since an idle guest uses a few hundred
MiB of its 1.5 GiB limit; and give Codex the same resident session that
Claude has.

## Spare pool (runner)

- `warden-runner --spare-sandboxes N` (compose: `WARDEN_SPARE_SANDBOXES`,
  default 1 on OVH). The lifecycle tick starts one creation at a time when
  fewer than N spares are ready; creation runs outside the worker lock, and
  a failure backs off 30 s and removes the half-made guest.
- A spare is `wc-spare-<random>`, created from the configured template with
  the usual deny-all rule and no workspace, and held resident by the same
  keep-alive session resident sandboxes use, so SBX does not auto-stop it.
- When a chat needs a sandbox that was never created and has no repository
  clone, prepare adopts a spare: the sandbox takes the spare's runtime name
  before its identity is registered with the policy service, so the broker
  only ever sees the final name. Clone-mode sandboxes are still created
  directly. The verifier's provisional trust applies to any binding without
  a proof, so adoption stays on the fast path.
- Spares live in the managed state, and a restarted worker removes them
  (its keep-alive died with it) and boots fresh ones. They do not count
  against `--max-resident`.

## Resident Codex

- `ResidentProviders` now defaults to Claude and Codex; steering is still
  Codex-only. Legacy engine tests pin residency off explicitly; the resident
  tests already ran against the Codex protocol fake.

## Steps

- [x] Runner spare pool, flag, compose wiring, tests (creation, adoption,
      refill, restart cleanup, clone-mode and disabled cases).
- [x] Codex resident default and test; go vet and race suite pass.
- [x] Release, deploy to OVH, confirm a spare boots.
- [ ] Confirm from the runner state that the next new chat adopted the
      spare (runtime name `wc-spare-…`) and record the timing.

## Deployment (2026-09-15, release 9924200)

- Switched with no chat running; policy, runner and chat recreated. Runner
  args carry `--spare-sandboxes 1` beside the guest template. Root 200,
  signed-out `/api/state` 401.
- The first spare (`wc-spare-920a20cedf7d9833`) was running within the
  first lifecycle ticks from the guest image, holding 100 MiB of guest
  memory against its 1.5 GiB limit; host memory available 3.4 GiB with two
  resident-capable slots plus the spare.
- Rollback: `ln -sfn /opt/warden/releases/5073beb /opt/warden/current` and
  `docker compose up -d` there (no spares; the stale spare is harmless and
  removed by the next runner start that has spares enabled, or by
  `sbx rm --force`).

## Expected effect

- New chat on a new environment: no 6.7 s boot; the chat takes the booted
  spare and the runner boots the next one in the background.
- Codex chats: the first message after a quiet spell still starts the CLI
  (1.8 s); later messages within 10 minutes are turns on the live process.
