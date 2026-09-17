# Working in this repository

Read [docs/feature-map.md](docs/feature-map.md) before searching the tree.
It maps owner-facing features to the files, routes, tests and plan that own
them, and translates product names to code names (the owner says *workspace*,
the code says `Environment`/`sandbox`). Start there, not with grep.

## Keep the feature map current

The map is part of the definition of done. In the same change that:

- adds, removes or renames an owner-facing feature, route, agent tool, CLI
  subcommand or config field → add, remove or rename its row;
- moves the code that owns a feature → update the row's paths;
- introduces a new owner-facing term or a new code name for an existing one →
  add it to the glossary;
- lands a new plan document → link it from the row it belongs to.

A row is one line: enough to open the right file, no explanation. Check that
every path and link in a row you touch exists. Do not let the map describe
intent; it describes the tree as committed.

## Conventions

- Keep the durable plan document for the work (`docs/*-plan.md`) up to date
  throughout: objective, steps, progress, decisions, remaining work, so
  another session can resume from it.
- Start fresh work in a new git worktree on a dedicated branch from
  `origin/main`. Clean the worktree up after the work is merged; never discard
  uncommitted work during cleanup.
- Owner identifiers (emails, server addresses) never appear in anything
  pushed to `origin`.
- API paths and Go identifiers stay stable when owner-facing wording changes;
  record the new wording in the glossary instead of renaming code.

## Build and test

- Go: `cd chat && GOPROXY=off GOFLAGS=-mod=mod go test ./...` (module
  `warden/chat`; dependencies resolve from the local module cache, `go mod
  tidy` is not expected to work offline).
- Web: `cd chat/web && pnpm build && pnpm test` (Vite + vitest; `tsc --noEmit`
  runs inside `build`).
- Release: `scripts/release.sh` (tarballs + GitHub release), guest image via
  `scripts/build-guest-image-in-sbx.sh`; local deploy `scripts/deploy-local.sh`.
