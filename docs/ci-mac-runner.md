# CI on the self-hosted Mac runner

All three workflows (`release.yml`, `guest-image.yml`, `ocsf-deploy.yml`) run
on one self-hosted GitHub Actions runner on the owner's Apple-silicon Mac,
registered to the `monaddle-too/warden` repository with the default labels
`self-hosted`, `macOS`, `ARM64`. GitHub-hosted `ubuntu-24.04` is no longer
used. This document is the runbook and the durable plan for that setup.

## Objective

Run Warden CI on owner-controlled hardware: no Actions minutes, the same
machine that builds releases by hand (`scripts/release.sh`), and a Docker
engine the workflows can reach for the multi-architecture images.

## Layout on the Mac

| Piece | Where | Notes |
|---|---|---|
| Runner | `~/actions-runner` (actions/runner 2.337.0, osx-arm64) | LaunchAgent `actions.runner.monaddle-too-warden.<name>` via `svc.sh`; job workspaces under `_work/`, tool cache under `_work/_tool/` |
| Docker engine | Lima VM `ci-docker` from [`deploy/ci/lima-docker.yaml`](../deploy/ci/lima-docker.yaml) | Rootful dockerd (Ubuntu LTS, 4 CPU, 8 GiB, 60 GiB); socket forwarded to `~/.lima/ci-docker/sock/docker.sock`; `~/actions-runner` mounted writable so bind-mounts under `$RUNNER_TEMP` work |
| Docker CLI | Homebrew `docker`, `docker-buildx`, `docker-compose` | `~/.docker/config.json` lists `/opt/homebrew/lib/docker/cli-plugins` in `cliPluginsExtraDirs` |
| Runner environment | `~/actions-runner/.env`, `~/actions-runner/.path` | `.env` sets `DOCKER_HOST=unix://…/ci-docker/sock/docker.sock`; `.path` is the job `PATH` (Homebrew, Nix profile) |
| Toolchains | `actions/setup-node`, `actions/setup-go` | Downloaded once into the runner tool cache; Actions cache disabled (`cache: false`) because module and pnpm stores persist in the runner user's home |

Both the runner and the VM run as the owner's user; nothing is installed
system-wide except the Homebrew formulae.

## Setup steps (done once, repeat on a new Mac)

1. `brew install docker docker-buildx docker-compose lima` and add the
   `cliPluginsExtraDirs` entry to `~/.docker/config.json`.
2. `limactl start --name=ci-docker deploy/ci/lima-docker.yaml`. Check with
   `DOCKER_HOST=unix://$HOME/.lima/ci-docker/sock/docker.sock docker version`.
   The VM starts with the user session; `limactl start ci-docker` after a
   reboot if it is not `Running` in `limactl list`.
3. Download the runner from github.com/actions/runner releases, verify the
   published SHA-256, extract into `~/actions-runner`.
4. Register with a short-lived token minted by `gh` (needs `gh auth login`
   with `repo` scope; the token never lands in a shell history):
   ```sh
   cd ~/actions-runner
   gh api -X POST repos/monaddle-too/warden/actions/runners/registration-token --jq .token \
     | xargs -I{} ./config.sh --unattended --url https://github.com/monaddle-too/warden \
         --token {} --name "$(scutil --get LocalHostName)" --work _work --replace
   ```
5. Append `DOCKER_HOST=unix:///Users/<user>/.lima/ci-docker/sock/docker.sock`
   to `~/actions-runner/.env` and make sure `.path` contains
   `/opt/homebrew/bin`, `/nix/var/nix/profiles/default/bin` and
   `~/.nix-profile/bin` (the runner captured `PATH` at `config.sh` time).
6. `./svc.sh install && ./svc.sh start`; `./svc.sh status` shows the
   LaunchAgent. The runner appears under Settings → Actions → Runners.

## Workflow changes for the Mac

- `runs-on: [self-hosted, macOS, ARM64]` everywhere.
- `release.yml` runs `scripts/release.sh --version …` (BSD tar, `shasum`
  fallback, host binary self-check) instead of the inline Linux steps; the
  script honours a caller-set `GOPROXY` (CI sets the public proxy, by hand it
  stays `off`). Image build, digests and the GitHub release are unchanged.
- QEMU is registered for `linux/amd64` (the daemon is arm64), the reverse
  of the GitHub-hosted setup.
- `ocsf-deploy.yml`: the deploy job keeps the SSH identity and known_hosts
  under `$RUNNER_TEMP/ssh` (`UserKnownHostsFile`, `IdentitiesOnly`) instead
  of the runner user's `~/.ssh`.

## Security

- The repository is public. A self-hosted runner executes whatever a
  workflow on a matching label asks. Keep **Settings → Actions → General →
  Fork pull request workflows** on "Require approval for all outside
  collaborators" (or stricter) so a fork PR cannot schedule onto this Mac,
  and treat any new `pull_request`-triggered workflow as reviewable code.
- Secrets used here (`OVH_DEPLOY_KEY`, `OVH_KNOWN_HOSTS`, `GITHUB_TOKEN`)
  pass through the job's temp directory only.
- The Lima VM has a rootful daemon: a `--privileged` container owns the VM,
  not the Mac. The only host directory it can write is `~/actions-runner`.

## Progress

- 2026-09-17: runner extracted (2.337.0, SHA verified); `ci-docker` VM
  running, amd64 emulation and writable bind-mounts verified; workflows,
  `release.sh` and this document updated; `actionlint` clean.

## Remaining work / known gaps

- Register the runner and install the LaunchAgent (step 4–6) — needs a
  valid `gh` login.
- `ocsf-deploy.yml` build job: `scripts/integration.mjs` and the workflow
  call `sudo install -o 65532 …` on host paths. On the Mac this needs
  passwordless sudo for `/usr/bin/install` (a sudoers rule the owner adds),
  and the virtiofs bind-mount must honour the 65532 ownership for the
  container's mode-600 `queue.db`. Unverified until the first run; if it
  fails, move the data directory to a Docker named volume for CI.
- First runs of each workflow on the Mac (release via `workflow_dispatch`
  pushes a dev image to ghcr; guest image likewise).
