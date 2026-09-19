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
