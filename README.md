# Warden

Sandboxed AI coding agents behind a policy gateway.

Warden runs Codex or Claude Code in a microVM of its own, with no network
and no credentials, and gives you a chat to work with it from the browser
or the terminal. Everything the agent wants from the outside world — a
host on the network, a GitHub repository, a Google document, a published
preview of a port, more CPU or memory — is a request you approve in the
chat, and the policy service performs it on the agent's behalf; the
sandbox never holds a token. Files and chat history survive a stop;
sandboxes stop by themselves when idle.

The same binary installs three ways: on your own machine (this page), as
a Docker Compose stack on a server ([deploy/chat/README.md](deploy/chat/README.md)),
or as a Helm chart on Kubernetes, where each sandbox is a pod under a Kata
or gVisor RuntimeClass ([docs/warden-kubernetes.md](docs/warden-kubernetes.md)).

## Install on your own machine

Warden desktop is a single-owner install: one person, one machine, no
domain, no certificates. It runs on an **Apple Silicon Mac** or an
**x86_64 Linux host with KVM** (Intel Macs, Windows and Linux without KVM
are not supported).

### 1. Prerequisites

- **Docker Sandboxes (`sbx`)** and a Docker account. Sandboxes are `sbx`
  microVMs; Warden drives them through a private namespace of its own, so
  your own sandboxes and settings are untouched.

  ```bash
  brew install --cask sbx
  ```

  On Linux, install `sbx` from [docker/sbx-releases](https://github.com/docker/sbx-releases).
- **An agent sign-in**: a ChatGPT account for Codex, or a Claude Code
  `claude setup-token` for Claude. Warden installs and runs without one,
  but every chat needs one.
- Outbound HTTPS during install (the Codex bundle from `github.com`, about
  120 MB; the Claude executable from `storage.googleapis.com`, about
  230 MB; the sandbox template from Docker Hub).

### 2. Install

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/monaddle-too/warden/main/install.sh)"
```

[install.sh](install.sh) downloads the newest release for your host,
checks it against the release's `SHA256SUMS`, unpacks it under
`~/.warden/releases/`, points `~/.warden/release` at it and runs
`warden install`. Run it with `sh -c` as above, not `curl | sh`: the
install signs Warden's sandbox namespace in to Docker with a device code
and needs the terminal for that. Set `WARDEN_VERSION=v0.1.0-alpha.13` to
pin a release, or pass `--no-install` to only download and unpack.

`warden install` creates the state directory (`~/.warden` on macOS,
`~/.local/share/warden` on Linux, owner-only), the private `sbx`
namespace, the Docker sign-in, the agent runtimes, and writes
`warden.json`. It prints one line per step, is safe to re-run, and asks
one question: whether to send bug reports (you review every report before
it goes). The details of every step are in
[the install guide](docs/warden-local-install.md).

Then put the launcher on your `PATH`:

```bash
echo 'export PATH="$HOME/.warden/release/bin:$PATH"' >> ~/.zshrc
```

Prefer to do it by hand? Download the tarball for your host from
[the releases page](https://github.com/monaddle-too/warden/releases),
unpack it anywhere and run `bin/warden install` from there; the launcher
finds `web/`, `config/` and `vendor/` beside `bin/`.

### 3. Sign in to an agent

```bash
warden login claude        # paste the value of `claude setup-token`
warden login codex         # Codex device sign-in (on a Mac: --codex-cli /path/to/codex, or see the guide)
warden login github        # optional: lets agents propose pull requests as you
```

Each sign-in is one owner-only file under `~/.warden/provider/`; only the
policy service reads it, and the agent sees a placeholder. Google Docs is
connected from the browser (Admin console → Sign in with Google).

### 4. Run it

```bash
warden start --detach      # the four services in the background
warden open                # the chat in your browser
```

`warden doctor` checks every host and runtime invariant and prints the
fix for anything that fails; run it when a chat says "unsupported SBX
version" or "agent worker disconnected". `warden status` says what is
running, `warden stop` stops it, `warden uninstall` removes everything
install created. Without `--detach`, `warden start` runs in the
foreground and Ctrl+C stops it.

The first message of a new chat takes longer than later ones: the runner
boots a sandbox and copies the agent runtime into it.

### From the terminal instead of the browser

`warden chat` is a client of the same chats and approvals the browser
shows, live:

```bash
warden chat                # the most recent chat, interactively
warden chat new "Fix the flaky test"
warden chat send --wait 1 "Run the suite and tell me what fails"   # 1 = a number from `warden chat list`
```

### Upgrading

Run the install line again: it unpacks the newer release beside the old
one, moves the `~/.warden/release` link, and `warden install` reports
"already" for what exists. Rolling back is `ln -sfn` to the previous
directory under `~/.warden/releases/`. Restart Warden afterwards
(`warden stop && warden start --detach`).

## What is in the box

| Area | Where |
|---|---|
| Install guide: every step, state layout, limits, troubleshooting | [docs/warden-local-install.md](docs/warden-local-install.md) |
| What agents can ask for (network, repositories, documents, previews, resources) and how approvals work | [docs/warden-local-install.md § What an agent can ask for](docs/warden-local-install.md#what-an-agent-can-ask-for) |
| Permission modes, rules, rewind, `/compact`, forks, `/btw` — the Claude Code feature set in the chat | [docs/claude-parity.md](docs/claude-parity.md) |
| Bug reporting (opt-in; you review every report) | [docs/bug-reporting-plan.md](docs/bug-reporting-plan.md) |
| Server install with Docker Compose | [deploy/chat/README.md](deploy/chat/README.md) |
| Kubernetes: chart, tiers, NetworkPolicy, GKE recipe | [docs/warden-kubernetes.md](docs/warden-kubernetes.md) |
| Where each feature lives in the code | [docs/feature-map.md](docs/feature-map.md) |

## Developing

Go 1.26+, Node 22.12+ and pnpm 11.19+. The launcher and the four services
are one Go binary, `chat/cmd/warden`; the chat UI is `chat/web`.

```bash
pnpm --dir chat/web install --frozen-lockfile && pnpm --dir chat/web build
go -C chat build -trimpath -o ../dist/chat/warden ./cmd/warden
dist/chat/warden install   # finds chat/web/dist, vendor/ and config/ in the checkout
```

Tests: `cd chat && GOPROXY=off GOFLAGS=-mod=mod go test -race ./...` and
`cd chat/web && pnpm test`. A release is `scripts/release.sh` (what the
`Warden release` workflow runs from a `v*` tag), and
`scripts/deploy-local.sh` installs the current checkout into
`~/.warden/release`. See [AGENTS.md](AGENTS.md) for the conventions.

## History

Warden began as a macOS development VM paired with a Linux inspection VM
(the Python `./warden` launcher at the repository root, `host/`, `proxy/`,
`native/`). That prototype is documented in
[docs/architecture.md](docs/architecture.md),
[docs/protected-development.md](docs/protected-development.md) and
[docs/validation.md](docs/validation.md); the chat product above replaced
it and is what releases contain.
