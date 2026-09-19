# The dogfood loop: a chat that clones, changes, tests and proposes Warden

Status: started 2026-09-19 on branch `feat/dogfood-loop` from origin/main 1658a41.

## Objective

Run the whole developer loop from one Warden chat on a jailbroken workspace
(`docs/host-dogfood-plan.md`), as a real workflow rather than a demo: the
agent clones the Warden repository into its sandbox (repository sharing),
changes it, moves the checkout to the host and builds it into a second
instance (`host_put`, `warden release build --instance inner`), starts it,
exposes the inner Warden as a bound port of the chat (`host_expose`, the
same preview list a sandbox server lands in), verifies the change through
that port, and submits the change as a pull request proposal
(`request_pull_request`) that the owner reviews in the app. Then the owner
side: merge to main, tag a release (`release.yml` on the Mac runner),
`warden release install TAG` into the default instance.

Every failure on the way is a bug in the workflow and is fixed here, without
widening what the jailbreak already allows.

## What exists

- Instances, releases, `--instance`, `dogfood.jailbreak`, `host_run` /
  `host_put` / `host_get` / `host_expose` / `host_status`: `docs/host-dogfood-plan.md`.
- Repository sharing (`request_repository_access`, the proxied clone
  through the policy service's GitHub credential): `docs/github-repository-sharing-plan.md`.
- Pull request proposals (`request_pull_request`, `sharing/pr_resolve`):
  `docs/pull-request-approval-plan.md`.
- Releases from a tag: `.github/workflows/release.yml`, `docs/ci-mac-runner.md`;
  `warden release install TAG` downloads and verifies one.

## Steps

1. Outer instance `dogfood` rebuilt from main, GitHub sign-in refreshed,
   started; a jailbroken Claude chat in auto mode.
2. The chat runs the loop; each obstacle recorded below and fixed on this
   branch, redeployed with `warden release build . --instance dogfood --restart`.
3. The proposal reviewed and approved in the app; the PR merged to main
   (fast-forward), this branch merged after it.
4. Tag → GitHub release → `warden release install` into `~/.warden`.

## Key decisions

1. The dogfood instance's GitHub sign-in (expired, 401) was replaced with
   the `gh` CLI's token through `warden login github --paste-stdin
   --replace`, the path the flag's own help suggests; same account, same
   `repo` scope. The owner can re-run `warden login github` (device flow)
   to hold a Warden-specific token instead.

## Progress log

- 2026-09-19: branch opened; `~/.warden-dogfood/warden.json` carried
  `runtimes.opencode` and `providers.deepinfra` from unmerged branches
  (dropped, backup `warden.json.pre-main-*.bak`); main 1658a41 built into
  the instance.
- 2026-09-19: **the loop ran end to end.** A jailbroken Claude chat on the
  `dogfood` instance (main 1658a41, auto mode) was given the change "the
  signed-out page names the instance and its build (owner mode only)".
  Unprompted it: got `monaddle-too/warden` shared (one approval), cloned,
  made the change on a branch, `host_put` the checkout to
  `~/dogfood/warden-src`, created `inner` from `default`, built it there
  (`release build … --instance inner --restart`, ~18 s), started it, exposed
  its app port (`host_expose 18785` → a host-upstream binding in the chat's
  preview list), verified `/auth/session` reports `{name: inner, version}`
  on inner and nothing on the default instance, ran the Go and web tests on
  the host, and submitted the proposal; the owner approved it from the
  API (`sharing/pr_resolve` with the reviewed body) and Warden published
  [PR #1](https://github.com/monaddle-too/warden/pull/1) from the stored
  snapshot. Four obstacles it reported, fixed here (8484746) and verified
  live by the same chat on the rebuilt `dogfood`:
  1. `warden start --instance inner` through the default launcher ran the
     default's binary against inner's state: the launcher that re-executed
     into its release passed `WARDEN_RELEASE_REEXEC=1` to the runner and so
     to every `host_run`; the guard is now consumed where it is read and
     `hostEnv` drops every `WARDEN_*` variable. Verified: "running the
     instance's release warden-v0.0.0-dev.bad7b134be09".
  2. `host_put` onto an existing directory nested the copy (`src/src/`);
     `replace: true` removes the host tree first (audited). Verified.
  3. `release build` after an uncommitted edit reused the store entry of
     the same version, so the rebuilt tarball never reached the instance;
     a dirty checkout now replaces it ("has uncommitted changes: replacing
     …"). Verified with the fixed launcher (the default instance's older
     launcher of course lacks it until the release below is installed).
  4. `request_pull_request` refused ~170 changed lines because it carries
     whole files and `docs/feature-map.md` alone is 92 KiB: the cap is
     1 MiB of contents (rendered proposal 4 MiB). The agent had dropped the
     `docs/host-dogfood-plan.md` paragraph from the PR to fit; the
     description now says 1 MiB.
  Also noted, not changed: `~/.warden-inner/release/bin/warden` without
  `--instance` acts on the default instance (the store is shared, so the
  binary's path carries no instance; `--instance` is the contract); the
  inner edge answers 403 `unknown Warden host` unless the preview hop
  rewrites `Host`, which it does. Merged to main 57f46e1 (the PR merge and
  the fixes; `go test ./...`, web build and 298 web tests green), tagged
  `v0.1.0-alpha.14` for `release.yml` on the Mac runner.
- 2026-09-19: **release v0.1.0-alpha.14** built by `release.yml` on the Mac
  runner in 3 min (tarballs, `SHA256SUMS`, chart). `warden release install
  v0.1.0-alpha.14 --restart` into `~/.warden` downloaded and verified it,
  but the service crash-looped: the runner refused `--retained 64`
  ("invalid worker limits", `runnersvc/main.go` still caps retained at 32
  although 78adb90 made `sandboxes.keepStopped` configurable), and the
  owner's `warden.json` had meanwhile been raised to `keepStopped: 64` by
  another session that was running its fix for exactly that
  (`fix/runner-retained-limit` 3960909, `.local/warden-retained-limit`,
  unmerged) on the default instance. That build was restored
  (`release use v0.0.0-dev.3960909b652e --restart`); the release was
  installed into `dogfood` instead (its config keeps 32), from GitHub with
  the checksum check. Once 3960909 lands, the next tag carries it and
  `warden release install <tag>` into `~/.warden` is the remaining step.

## Round 2: a pull request that does not build

Two affordances the loop lacked (the owner, 2026-09-19): the agent could
open a pull request but not push a fix to it, and could not see whether it
built. Added (details in `docs/pull-request-approval-plan.md` § Updates and
CI results): `request_pull_request {pull_request: N}` updates a pull
request Warden published (reviewed against its head, committed onto its
`warden/pr-…` branch with `git/update-ref`), `view_ci_results` reads the
check runs and failed jobs' steps and log tails, and
`.github/workflows/ci.yml` builds and tests every pull request on the Mac
runner so those checks exist. The task: a chat makes a Warden PR that does
not build, reads the CI result, and pushes the fix to the same PR.
