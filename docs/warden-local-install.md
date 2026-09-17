# Installing Warden on your own machine

This is the operator guide for a local, single-owner Warden: one person, one
machine, no domain, no TLS certificates, no Google sign-in client, no Docker
Compose. The four services (policy, runner, chat and edge, all subcommands
of the one `warden` binary) run as your user from one private state
directory, and agents
run in Docker Sandboxes (SBX) microVMs that Warden manages in an SBX namespace
of its own. The launcher is the `warden` binary (`chat/cmd/warden`). The
server-shaped installation on OVH is described in `deploy/chat/README.md`;
the design and the record of the first live run are in
`docs/warden-local-deployments-plan.md`.

What is verified: the whole flow below (install, doctor, Codex login, start,
open, a chat, an approved preview through the edge, unpublish) was run on an
Apple Silicon Mac on 2026-09-15 (plan, "Progress"). The Linux path is built
and unit-tested but has not been run on a fresh VM; it is marked as such.

## Supported hosts

| Host | Guest architecture | Status |
|---|---|---|
| Apple Silicon Mac | `linux/arm64` | Verified live (SBX 0.42.1 and 0.43.0, macOS, arm64 Codex in the stock template). |
| x86_64 Linux with KVM (`/dev/kvm` usable by you) | `linux/amd64` | Not yet verified on a fresh VM. The amd64 pins are the ones OVH runs. |

Not supported: Intel Macs, Windows, Linux without KVM, more than one user
(server mode is a successor plan). `warden install` refuses any other host
architecture with "unsupported host architecture".

## Prerequisites

- **sbx (Docker Sandboxes).** Any version that answers `sbx version`; Warden
  assumes the features it uses are present and fails at the call that needs
  a missing one. A setting an older sbx does not define (it answers
  `setting "…" is not defined`) is skipped: the feature it would disable is
  absent. The acceptance runs used 0.42.1 and 0.43.0. On a Mac
  the sbx Homebrew cask installs it at `/opt/homebrew/bin/sbx`, one of the
  places `warden install` looks (`$PATH`, then `/opt/homebrew/bin/sbx`,
  `/usr/local/bin/sbx`, `/usr/bin/sbx`); `--sbx PATH` names it elsewhere.
- **A Docker account** for the SBX device sign-in. SBX pulls the stock
  sandbox template from Docker Hub and answers its policy queries with 401
  until its namespace is signed in.
- **For chats, a Codex or Claude sign-in**: a ChatGPT account that Codex can
  log in to with `codex login --device-auth`, or a `claude setup-token`
  value from Claude Code. Warden runs without either, but every chat then
  fails with "Refresh the host Codex sign-in before running a chat" (or the
  Claude equivalent).
- Outbound HTTPS during install to `github.com` (the Codex bundle, about
  120 MB) and `storage.googleapis.com` (the Claude executable, about
  230 MB), plus whatever SBX itself needs for the template pull.
- Memory: the installer sizes sandboxes from the host (see "What install
  does", step 8); a sandbox is 1536 MiB, and 4 GiB is kept for the host.

## 1. Get the release

**From a release tarball.** A release is a tarball
`warden-<version>-<os>-<arch>.tar.gz` holding `bin/warden` (the one
binary; the services are its `policy`, `runner`, `serve` and `edge`
subcommands), `web/` (the built chat UI), `config/` (the sandbox policy template and the
server example config) and `vendor/` (the GitHub REST catalog). Unpack it
anywhere and run `bin/warden install` from there: the launcher and the
services find `web/`, `vendor/` and `config/` beside `bin/` without any
configuration (verified with the darwin-arm64 tarball on 2026-09-16).
Releases are produced either by the `Warden release` GitHub workflow from a
tag, or locally with `scripts/release.sh` (same layout, same checksums
file; `--publish` uploads them to a GitHub release with `gh`), which needs
no GitHub Actions minutes.

**From source** (developers). Go 1.26+, Node 22.12+ and pnpm 11.19+:

```sh
pnpm --dir chat/web install --frozen-lockfile && pnpm --dir chat/web build
mkdir -p dist/chat
go -C chat build -trimpath -o ../dist/chat/warden ./cmd/warden
```

`dist/chat/warden start` then finds `chat/web/dist`, `vendor/` and
`config/policy.template.json` in the checkout.

## 2. `warden install`

```sh
warden install            # or: dist/chat/warden install, bin/warden install
```

Flags: `--state DIR` (the state directory), `--config PATH` (where to write
`warden.json`; default `<state>/warden.json` or `$WARDEN_CONFIG`), `--sbx
PATH`, `--upgrade` (accept a state directory another Warden release
installed), `--guest-image-tar FILE` (a `docker save` of the pinned guest
image, only used once a release pins one), `--sbx-login=false` (do not run
the device sign-in).

The default state directory is `~/.warden` on macOS and
`$XDG_DATA_HOME/warden` (default `~/.local/share/warden`) on Linux. Keep it
short: sbx binds Unix sockets under `<state>/sbx/home/.sbx/run/…`, macOS
limits socket paths to 104 bytes, and the configuration refuses a state
directory whose sockets would not fit ("sbx.privateHome is too long" or
"paths.state is too long for the private Unix sockets"). This is why the
macOS default is not `~/Library/Application Support`.

Install prints one line per step (`name:            result`) and is safe to
re-run; every step reports "already" when its work exists. It never touches
your own sbx sandboxes, settings or policies, never deletes a sandbox and
never replaces a provider login. What it does, in order:

1. **State root.** Creates `<state>` and `policy`, `runner`, `app`, `edge`,
   `provider`, `sbx`, `runtimes`, `bin` under it, all mode 0700, and records
   the release in `<state>/install.json` (layout, Warden revision, SBX,
   Codex and Claude versions, guest architecture). A directory installed by
   a release with different pins is refused with both records printed
   unless you pass `--upgrade`.
2. **SBX namespace.** Finds `sbx`, creates the five private HOME/XDG
   directories under `<state>/sbx` (`home`, `cache`, `state`, `config`,
   `data`) and writes `<state>/bin/warden-sbx`, a shell wrapper that
   exports `HOME` and the four `XDG_*_HOME` variables to them and execs the
   pinned `sbx`. Every Warden command and service runs SBX only through
   this namespace, so your own `sbx ls` never shows Warden's sandboxes and
   Warden never reads your settings, policies or sandboxes. **On macOS** it
   also links `<state>/sbx/home/Library/Keychains` to your real
   `~/Library/Keychains`: sbx keeps its Docker session in the login
   keychain, and the Security framework looks for that keychain under
   `$HOME`. The namespace therefore shares your own sbx sign-in. If you
   have run `sbx login` before, the namespace is already signed in; if you
   have not, see step 4.
3. **Daemon and settings.** Runs `warden-sbx daemon start --policy deny-all
   --detach` unless `daemon status` reports `Status: running` (the command
   exits 0 either way; only that line says which), then sets
   `ssh.agentForwardingEnabled=false` and `proxy.sandbox=direct` and
   restarts the daemon if sbx says a setting needs it.
4. **SBX sign-in.** Once per state directory, recorded in
   `<state>/sbx/login.json` because sbx has no query for it. If the daemon
   already answers `mcp ls` (the namespace is signed in, for example
   through the keychain link), install records that and moves on. Otherwise,
   in a terminal, it runs `warden-sbx login` and prints "Signing Warden's
   SBX namespace in to Docker. Follow the device-code instructions below".
   Without a terminal, or with `--sbx-login=false`, install stops here with
   the exact command, because nothing later can succeed against an
   unsigned-in daemon (it answers every policy query with 401):

   ```
   Warden's SBX namespace is not signed in to Docker. Run this in your own terminal window (macOS may show a keychain prompt), then re-run warden install:
     <state>/bin/warden-sbx login
   ```

   Run that command yourself and re-run `warden install`; the re-run detects
   the sign-in. The keychain prompt on macOS only appears for a command run
   in your own terminal window in your GUI session.
5. **Host checks.** The same six checks the policy verifier runs, printed
   as `PASS`/`FAIL` lines: `sbx version`, the two settings, `sbx mcp
   inventory` (no servers, local gateway), `sbx global network policy` (no
   global rules) and `sbx implicit denial` (`example.com:443` denied
   implicitly with no governance). Any failure stops the install with the
   remediation under it.
6. **Runtimes.** Downloads the Codex 0.154.0 "package" bundle for the guest
   architecture into `<state>/runtimes/codex/` and the Claude Code 2.1.272
   Linux executable to `<state>/runtimes/claude/claude`, each by the URL
   and SHA-256 pinned in `chat/internal/release` (both architectures are
   pinned). A mismatch discards the download and fails the install; a
   re-run that finds the pinned files skips the download. The extracted
   bundle is made world-readable on purpose: `sbx cp` preserves modes, and
   the guest's agent user must be able to execute Codex (see
   Troubleshooting).
7. **Guest image.** No release pins a Warden guest image yet
   (`release.GuestImages` is empty for both architectures), so install
   prints `guest image: none pinned for <arch>; new sandboxes use
   docker/sandbox-templates:shell-docker and the runner copies the runtimes
   in` and records the stock template. Once a digest is pinned, install
   loads it from `--guest-image-tar` or pulls it with `docker` when that
   is installed.
8. **`warden.json`.** Writes the detected facts: the `sbx` executable, the
   private home, the runtime paths, the template and digest, the sandbox
   sizing (1536 MiB per sandbox, 2048 MiB on hosts with 32 GiB or more; at
   most one running sandbox per two cores and four in total, with 4 GiB
   kept for the host; one warm spare when two or more can run), the chat
   and edge loopback ports (18780 and 18781 when free, the next free ports
   otherwise, chosen on the first install only) and the provider login
   paths. The `host:` line shows the result. Edits you make to other fields
   survive a re-run. The file holds paths and numbers only, never a secret;
   its schema is the appendix of the plan. The config file is optional for
   the services (every field has a computed default), but `warden start`
   requires it to exist because it is where the detected facts live.

The last line is `Installed. Next: `warden login codex` (and `warden login
claude`, `warden login github` as needed), then `warden start` and `warden
open`.`

## 3. `warden doctor`

```sh
warden doctor
```

Runs every check without starting a service and prints one line per check,
`PASS name: detail` or `FAIL name: detail` followed by an indented `fix:`
line with the exact command, then `all checks passed`; exit status 1 when
anything failed. The checks: `config` (present and mode 0600), the state
directory and each subdirectory (present, owner-only), `host architecture`,
`sbx executable`, the five `sbx namespace` directories, `sbx keychain`
(macOS: the link is in place), `sbx wrapper` (present, selects the right
namespace and executable), the six host checks from install step 5 (with
`sign Warden's namespace in to Docker from your own terminal` as the fix
when the daemon answers 401), `guest image`, the runtime checks
(`codex-package.json` version, target and layout, the four executables,
the Claude SHA-256, and that the bundle is world-readable), and one line per
provider login (a missing login is a `PASS … not signed in`; a login
readable by others is a `FAIL`).

Run it whenever a chat answers "unsupported SBX version", "unsafe SBX
setting" or "agent worker disconnected".

## 4. Sign in to providers

Each login writes exactly one file, mode 0600, into `<state>/provider/` at
the path `warden.json` names, and refuses to replace an existing file unless
`--replace` is given. Only the policy service reads these files; they are
never copied into a sandbox, and the agent sees placeholders. All three take
`--config` and `--state` like the other commands.

**Codex** (`warden login codex`, default file `provider/auth.json`). The
sign-in is the `auth.json` that `codex login --device-auth` writes.

- On Linux the installed bundle's `bin/codex` is native and is used by
  default: the command runs `codex login --device-auth` with `CODEX_HOME`
  set to a private scratch directory under `provider/`, so only `auth.json`
  is produced, then moves it into place. Approve the device code in a
  browser signed in to the ChatGPT account Warden should use.
- On macOS the bundle's `codex` is a Linux executable, so either pass
  `--codex-cli PATH` to a host Codex CLI (same flow), or run the printed
  command yourself on any machine with a Codex CLI:

  ```sh
  mkdir -m 700 -p <state>/provider/.codex-login
  CODEX_HOME=<state>/provider/.codex-login codex login --device-auth
  ```

  and enter the path of the resulting `auth.json` at the `Path to
  auth.json:` prompt. The file is copied, mode 0600, and the source can be
  deleted. A file without `tokens` or `OPENAI_API_KEY` is refused.

**Claude** (`warden login claude`, default file `provider/claude.json`).
Run `claude setup-token` on a machine with a browser and paste the
`sk-ant-oat…` value at the hidden prompt (or pipe it in). Warden records a
364-day expiry (Anthropic's tokens last a year) and refuses API keys and
session tokens. This replaces `scripts/warden-claude-token`.

**GitHub** (`warden login github`, default file `provider/github.json`).
The command runs GitHub's device flow with Warden's own OAuth App (the
client ID ships in the release): it prints a one-time code, opens
github.com/login/device in your browser, and stores the resulting user
token once you approve. To store a token you already have instead:

```sh
gh auth token | warden login github --paste-stdin
```

(`--paste TOKEN` also exists; prefer stdin so the token stays out of your
shell history.) Either way the token is verified against GitHub and stored
with your login and scopes. Actions then appear as you; the repository
allowlist and per-request approvals stay the boundary, as for the other
providers.

**Google Docs** is connected from the browser, not the terminal: the Docs
connection uses the Desktop OAuth client shipped in the release. Open the
Admin console (sidebar) or a chat's Documents panel and choose "Sign in with
Google"; the sign-in completes in a popup and the token is stored by the
policy service.

**Signing out.** The Admin console lists every connected account. "Disconnect
Google" revokes the token with Google, forgets it and revokes every document
grant; "Disconnect GitHub" deletes the stored token and drops every
repository selection (the token itself stays valid at GitHub until you
revoke it under Settings, Applications, because revoking needs the OAuth
App's secret, which releases do not ship). Codex and Claude sign-ins are
files under `<state>/provider/`; delete the file, or run the login again
with `--replace`.

## 5. `warden start` and `warden open`

```sh
warden start
```

`start` requires `warden.json` (from install), takes `<state>/launcher.lock`
(a second `start` on the same state directory is refused with "Warden is
already running or shutting down in this state directory."), then runs
the four services as `warden policy`, `warden runner`, `warden serve` and
`warden edge` (the same executable, so one build always runs with itself),
each with `WARDEN_CONFIG` pointing at `warden.json` and output appended to
`<state>/<service>.log` (`warden-policy.log` and so on).
The policy service and the runner run `sbx` themselves, so `start` gives
them the namespace `HOME`/`XDG_*` environment; without it their sandboxes
would land in your own namespace. It waits (up to 10 s each) for the policy
socket, the runner socket and the chat endpoint file, then prints
`Warden started with state <state>. Run `warden open` to open it. Ctrl+C
stops this stack.` If any service exits, the others are stopped and `start`
exits 1 with `<service> stopped; inspect <state>/<service>.log`.

Other flags: `--web-dir`, `--vendor-dir`, `--policy-template` (asset
locations when `warden.json` has no `paths.*`), `--without-edge` (do not
start the edge; the app is then only reachable on the chat port and
previews are not served).

```sh
warden open           # opens the browser; --print prints the URL instead
```

`open` reads `<state>/app/endpoint.json` and opens
`<url>/?launch=<time>#session=<capability>`, rewritten onto
`auth.publicURL`, which is the edge: `http://127.0.0.1:18781` by default
(the edge port install chose). Going through the edge matters: the edge
identifies you as the owner by that capability and mints the cookie session
a preview navigation needs. `--without-edge` opens the chat origin
(`http://127.0.0.1:18780`) directly. The capability rotates every time the
chat service starts, so run `open` again after a restart; every edge
session ends at that rotation too.

## 6. The first chat

A new chat asks the runner for a sandbox. The runner keeps warm spares
(`sandboxes.warmSpares`, one on hosts that can run two sandboxes) booted
from the template, and a new chat adopts one when it exists; otherwise it
creates one (`sbx create … --template docker/sandbox-templates:shell-docker
--deny-network '**'`). Because no guest image is pinned yet, the runner then
copies the runtimes into the guest on its first run: the Codex bundle with
`sbx cp` into `/tmp/warden-runtime` (every sandbox, once), and for Claude
chats the Claude executable to `/tmp/warden-claude` (once per guest, again
only if the host file changes). It also installs the gateway CA and applies
the sandbox's deny-all policy. Expect the first turn of a new sandbox to
take noticeably longer than later ones; the live run did not time it.
Sandboxes stop after 15 minutes without user activity
(`sandboxes.stopAfterIdleMinutes`) unless they hold a published preview;
files and chat history survive a stop, and the next message resumes.

## 7. Previews

An agent that started a server on `0.0.0.0:<port>` inside its sandbox calls
`sandbox_bind_port` (Codex; `preview_attach` is the compatible name) with a
port, path and title. That is an approval, not an action: a card appears in
the chat reading "Make <title> reachable at a Warden URL. Only signed-in
Warden users can access it. The binding lasts until you revoke it; stopping
the sandbox makes it unavailable." with **Bind port** and **Decline**.

On approval the runner publishes the sandbox port on a Warden-chosen
loopback port (`sbx ports <sandbox> --publish 127.0.0.1:<host>:<port>/tcp4`)
and the chat records a binding whose URL is

```
http://<binding-id>.localhost:18781/<path>
```

Browsers resolve `*.localhost` to loopback without DNS, and each binding is
its own origin, so the per-binding session model of the public deployment
applies unchanged. The "Published ports" panel lists it with **Open
preview** and **Unpublish**. What the edge does with a request to that host:

- signed in (a browser that opened the app through `warden open`): a one-use
  ticket flow (303 to the app's `/auth/preview`, 303 back to
  `/_warden/login` on the preview host, cookie set, 303 to the path) and
  then the page, with cookies and Warden authorization stripped before the
  request reaches the guest;
- signed out: 303 to the app with `?next=` (the ticket flow starts with the
  app session; a browser without one cannot open a preview);
- an unknown binding: 403 (`unknown binding`);
- after **Unpublish** (or a revoked binding): 410 (`preview binding
  unavailable or revoked`); Unpublish persists the revocation before the
  runner removes the loopback publication.

The preview session cookie lasts eight hours; after that a preview tab
redirects to the app, which mints a new session on its next API call, and
the preview must be reopened from the chat page. Note that the runner's raw
publication, `127.0.0.1:<host-port>` (visible in `<state>/bin/warden-sbx
ls`), is reachable by any process on this machine, exactly as on OVH: the
edge is the only authenticated path, and the loopback publication is not a
second one. Do not expose that port.

## 8. Stopping, state and reset

**Stopping.** Ctrl+C in the `warden start` terminal (or SIGTERM to it) stops
the four services in reverse order (SIGTERM, then SIGKILL after 15 s) and
releases the lock. The runner releases the keep-alive session it holds on
each resident sandbox, after which SBX's own rule (a VM stops once its last
exec session disconnects) stops the guests; files are kept and the next
message after the next `start` resumes them. The SBX daemon in Warden's
namespace was started detached and keeps running.

**Where state lives** (`<state>`, `~/.warden` on macOS):

| Path | Content |
|---|---|
| `warden.json` | Detected facts and paths (mode 0600, no secrets). |
| `install.json` | Which release installed this directory. |
| `policy/` | Policy service state: `sbx-control.sock`, bindings, runtime identities, `sharing.sqlite`, `google.sqlite`, the gateway CA. |
| `runner/` | Runner state: `worker.sock`, `managed-v2.json`, sandbox metadata. |
| `app/` | Chat state: `chats.json`, `endpoint.json` (the owner capability). |
| `edge/` | Edge state. |
| `provider/` | The 0600 login files (`auth.json`, `claude.json`, `github.json`). |
| `sbx/` | The private SBX namespace (`home`, `cache`, `state`, `config`, `data`, `login.json`); sandboxes and templates live here. |
| `runtimes/` | `codex/` (the bundle) and `claude/claude`. |
| `bin/warden-sbx` | The namespace wrapper; use it for any manual `sbx` command against Warden's sandboxes. |
| `warden-policy.log`, `warden-runner.log`, `warden-chat.log`, `warden-edge.log` | Service output, appended across starts. |
| `launcher.lock` | Held while `warden start` runs. |

**Reset and uninstall.** `warden uninstall` stops a background Warden,
deletes every sandbox in the namespace, stops the namespace daemon and
removes `<state>`; `--keep-state` stops after the sbx cleanup, `--yes`
skips the confirmation. Nothing outside `<state>` was created by install,
so afterwards only the unpacked release directory (and any PATH entry for
it) remains; the command prints where to revoke Warden's GitHub and Google
sign-ins at the providers, which outlive the local files. Deleting only `app/` forgets the
chats but keeps sandboxes and logins; deleting only `provider/<file>` (or
`warden login … --replace`) renews one sign-in; deleting
`sbx/login.json` makes the next install run the SBX sign-in again. Your
own sbx namespace is unaffected by any of this.

## What an agent can ask for

Besides sharing documents and repositories yourself, an agent can ask, and
every request becomes an approval card in the chat, a popup and a
`warden chat approve` item. Nothing happens until you answer, and the
answer and who gave it land in the workspace's Access history.

- **Network access** (`request_network_access`): one public host over
  HTTP/HTTPS for a bounded time, this sandbox only, no credential attached.
  Useful in restricted mode when an install or download is refused.
- **Repository access** (`request_repository_access`): share a repository
  with the workspace, or add read categories (code, issues, pull requests)
  to one already shared.
- **Small GitHub writes** (`github_write`): a comment on an issue or pull
  request, a new issue, or labels. The card shows the exact text; Warden
  posts it with your credential. Anything larger is a pull request
  proposal.
- **Spreadsheets** are part of document sharing: the picker lists Google
  Sheets beside Docs, and a grant covers the Sheets API for the chosen
  IDs. Document grants come in three levels, each including the ones
  below: **read**; **write** (Docs text and styling, Sheets cell values);
  **structure** (tables, tabs, adding or deleting sheets, formats, charts).
  Reconnect Google once after upgrading so the spreadsheet scope is
  granted.
- **Images in Docs** (public deployments only): with structure access an
  agent can place an image it attached with `attach_image` into a shared
  document. Google only takes images by URL and copies them at insertion,
  so Warden publishes the attachment at an unguessable address under
  `auth.publicURL` for that one edit and withdraws it as soon as Google
  has answered. Any other image URL is refused. A loopback install has no
  address Google can reach, so the edit fails there with that reason.
- **Host directories** (local installs only; `request_host_directory` and
  `sync_host_directory`): copy a directory from this machine into the
  sandbox at `/home/agent/host/<name>` (a snapshot, up to 1 GiB, never
  Warden's own state), and later copy the sandbox's version back over it,
  merging file by file without deleting anything.

## Network access from a sandbox

Every sandbox is created with sbx's network fully denied and exactly one
exception: its own inspecting gateway on the host loopback. Agents are
started with `HTTPS_PROXY` pointing at it and Warden's CA installed, so
all HTTP and HTTPS traffic passes through the gateway, which decides per
request and injects credentials only for approved operations. The
sandbox never holds a real token: the Codex and Claude processes get
placeholders that the gateway swaps for the stored sign-in on the way to
the provider.

What the gateway lets through depends on `sandboxes.egress` in
`warden.json`:

- **`restricted`** (the default): the provider hosts, a few package
  registries and the destination list in the policy template. Anything
  else is refused with an audit entry, so `curl https://google.com` from
  the sandbox fails. GitHub, Google Docs and Figma hosts need an active
  grant (a shared repository or document); without one they are refused.
  A shared repository is read-only and limited to the categories ticked
  when sharing it: code (files, branches, commits, clone), issues (issues,
  comments, labels, milestones) and pull requests; repository metadata
  always. Anything outside the ticked categories is refused, and every
  write needs its own approval.
- **`open`**: any public HTTP or HTTPS host on port 80 or 443 with a
  canonical DNS name. GitHub, Google Docs and Figma requests that have a
  grant are brokered exactly as before; those without one are forwarded
  anonymously, with the guest's own credential headers stripped and
  nothing injected, so public repositories clone and public APIs answer
  while private ones still need the usual approval. Private and
  special-use addresses, IP literals, other ports and raw TCP (SSH,
  databases) remain unreachable in both modes.

Switch it in the Admin console (sidebar), "Network access": the choice
applies to every sandbox at once, running ones included, and is kept by
the policy service across restarts, overriding `warden.json`. Or set the
default in `warden.json` and restart Warden:

```json
"sandboxes": { "egress": "open" }
```

A console choice wins over the file until `<state>/policy/egress.json` is
deleted.

## 9. Limits

- One owner. The launcher capability is the only identity and it is the
  admin: the Admin console shows connected accounts, lets you disconnect
  them and manage documents tagged unsharable; there are no other users.
  Multi-user installs are server mode.
- Google Docs and GitHub use the OAuth clients shipped in
  `chat/internal/release` (a Desktop client and a device-flow OAuth App).
  While the Google project stays in Testing, its refresh tokens expire after
  seven days; reconnect from the Documents panel when that happens.
- No published guest image yet, so a fresh install starts every sandbox
  from the stock template and copies the runtimes in. Build a local one
  with `scripts/build-guest-image-in-sbx.sh` (see above) to skip the copy.
- Everything pinned stays pinned: Codex 0.154.0, Claude
  2.1.272, the template digests. A new Warden release moves them; the
  installer never floats versions and refuses another release's state
  directory without `--upgrade`.
- Idle sandboxes stop after 15 minutes; the sizing in `warden.json`
  (`sandboxes.*`, validated on load: memoryMB 512–65536,
  cpus 0.25–64, maxRunning ≥ 1; `maxMemoryMB`/`maxCPUs` cap what any one
  workspace may be given, 0 derives them from the host)
  can be edited by hand.
- The SBX sign-in cannot be verified non-interactively; install records it
  and `doctor` reports 401 answers as "sign in from your own terminal".
- Verified on the Mac: restart and resume of a published preview. After a
  `warden start` restart the binding stays approved, the preview answers
  "preview unavailable; resume its sandbox and server", and one chat turn
  that starts the server and calls `sandbox_bind_port` for the same port
  restores the same URL without a new approval.
- Not yet run: the Linux/KVM host, Claude chats (no token was installed on
  the acceptance Mac), GitHub push review and pull request creation through the user token.

## Troubleshooting

These are the failures met during the first live macOS install, in the
order they appeared, with what you see and what to do.

**"agent worker disconnected" on the first message, nothing else in the
chat.** The guest could not start Codex. On the live run the cause was the
extracted Codex bundle not being readable by the guest's agent user (`sbx
cp` preserves host modes). Run `warden doctor`: its runtime checks include
the bundle's modes and the four executables; re-running `warden install`
rewrites the bundle world-readable. Then look at `<state>/warden-runner.log`
for the provisioning step that failed.

**"no default account profile set: secret not found" from the namespace
daemon, or `sbx login` ending with "Keychain Error. (-60008)".** macOS
only. The namespace HOME cannot see your login keychain, where sbx keeps its
Docker session. `warden install` links `<state>/sbx/home/Library/Keychains`
to `~/Library/Keychains` (doctor: `sbx keychain`); re-run install if the
link is missing. If the namespace still is not signed in, run
`<state>/bin/warden-sbx login` in your own terminal window so the keychain
prompt can appear, then re-run `warden install`.

**Install stops with "Warden's SBX namespace is not signed in to Docker".**
Expected when install has no terminal, `--sbx-login=false` was given, or
the sign-in did not complete. Run the printed
`<state>/bin/warden-sbx login` yourself and re-run install; it detects the
sign-in and continues. Until then the daemon answers policy queries with
401, which is also what `doctor`'s `sbx mcp inventory` line reports.

**`sbx daemon status` says stopped, or doctor's `sbx mcp inventory`
says the daemon is not running.** This is the daemon in Warden's private
namespace, not yours; `sbx daemon status` from your own shell reports
your own daemon. Start Warden's with
`<state>/bin/warden-sbx daemon start --policy deny-all --detach` (the
`daemon start --policy deny-all` form is what install uses and what the
implicit-denial check expects), or re-run `warden install`, which does the
same when the status line is not `running`.

**"sbx.privateHome is too long" / "paths.state is too long for the
private Unix sockets", or sbx failing to bind a socket.** sbx's own sockets
live under the namespace HOME and macOS caps socket paths at 104 bytes.
Use a short state directory (`--state ~/.warden` is the macOS default for
exactly this reason; `~/Library/Application Support/...` does not fit).

**Warden's sandboxes show up in your own `sbx ls`.** The policy service or
the runner ran `sbx` without the namespace environment, so they created
sandboxes in your namespace. `warden start` sets `HOME` and the four
`XDG_*_HOME` variables for both services; make sure you started them with
`warden start`, not by hand with the raw binaries. Remove the stray
sandboxes with your own `sbx rm`; the ones under `<state>/sbx` are Warden's.

**404 at `/` after `warden open`.** The chat service could not find the
built UI. The chat service looks in `paths.webAssets`, then `web/` beside
the binary, then `chat/web/dist` two levels above it; `warden start` resolves
the same for a source checkout and passes `--web-dir`. Point it at the
`web` directory with `warden start --web-dir DIR` or set
`paths.webAssets` in `warden.json`, and check `<state>/warden-chat.log`.

**"unsupported SBX version" or "unsafe SBX setting" in a chat.** The
verifier refused the host. `warden doctor` prints which of the six host
checks fails and the `warden-sbx` command that fixes it.

**"Refresh the host Codex sign-in before running a chat" / "Refresh the
Claude sign-in …" / "Refresh the GitHub sign-in before using
repositories".** The provider file is missing, unreadable or its token was
rejected. `warden login <provider> --replace` writes a new one; no restart
is needed.

## Terminal client

`warden chat` is a terminal client of the same API and event stream the
browser uses, so a chat driven from the terminal is the same chat the app
shows, live, with the same approvals. The trust line is unchanged: you type
on the host, only the agent runs in the sandbox, and everything the agent
produced is printed as text (escape sequences are stripped).

```bash
warden start --detach      # run Warden in the background; logs in ~/.warden/warden.log
warden chat                # open the most recent chat interactively
warden chat 2              # or a number from `warden chat list`, an id prefix, or a title
warden stop                # stop the background Warden
```

Inside `warden chat`: type and press Enter to send (during a run the message
steers it); Alt+Enter or Ctrl+J inserts a line break and pasted text keeps
its newlines; `y` or `n` answers the first pending approval and typed text
answers an agent question. Scroll with the mouse wheel, Up/Down while the
composer is empty, PgUp/PgDn, Home/End; the status bar says how far up you
are, and shows a spinner with the elapsed time while a run is active.
Replies render lightweight markdown (headings, lists, code blocks, inline
code, bold); command output and file diffs show their last lines, with Tab
or `/expand` showing everything and diffs coloured. Commands: `/new
[title]`, `/chats`, `/switch N`, `/stop`, `/model M`, `/provider
codex|claude`, `/open` (the chat in the browser), `/previews`, `/preview N`
(a preview in the browser), `/unpublish N`, `/find TEXT` (scroll to the
previous match), `/copy` (the last reply to the clipboard), `/help`,
`/quit`. Ctrl+C stops the current run, or quits when idle. Text selection
in the terminal needs its modifier key (Option or Shift) because the client
takes mouse wheel events.

For scripts:

```bash
warden chat list
warden chat new --provider claude "Nightly triage"
warden chat send 1 "Summarise the failing tests" --wait   # streams until the agent is idle
warden chat approve 1            # allow the first pending approval (--decline, --answer TEXT)
warden status
```

`send --wait` prints each transcript entry once it is complete and announces
pending approvals; answer them with `warden chat approve` or in the app.

**Approval popups.** While Warden runs it watches for approvals and surfaces
each one once: a desktop notification (macOS Notification Center, or
`notify-send` on Linux) naming the chat and the request, and, when started
with `--detach`, the app opened in your browser on that chat so the card is
in front of you. `warden start --popups browser|notify|none` overrides the
default (`auto`: browser when detached, notification when in the
foreground). Popups never answer anything; the app, `warden chat` or
`warden chat approve` do. The terminal client also rings the bell when an
approval appears on the open chat.

## Optional: a guest image so new sandboxes start faster

Without a guest image every new sandbox is created from the stock template
and the runner copies the two runtimes in (a few hundred megabytes) before
the first turn. After `warden install` and one `warden start` (which creates
the gateway CA), build a guest image inside a throwaway sandbox, no Docker
needed:

```bash
scripts/build-guest-image-in-sbx.sh
```

It prints the template tag and lists the templates. Put the tag in
`~/.warden/warden.json` as `sbx.guestImage` and the full digest that
`sbx inspect` reports for a sandbox created from it as
`sbx.guestImageDigest` (the script's last lines explain how to read it),
then restart Warden. New spares and sandboxes then start from the image and
the runner copies nothing. Existing sandboxes keep the stock template, which
the verifier still accepts.
