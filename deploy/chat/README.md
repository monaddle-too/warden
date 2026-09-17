# Warden on OVH

Warden runs entirely on the OVH server. Browsers connect to
https://warden.monaddle.com. There is no laptop relay, Panta API, database,
Panta worker, or shared sandbox daemon in this deployment.

## Services and state

- `warden-sbx.service`: SBX 0.42.1, dedicated `warden` user with KVM access.
  OVH runs release `v0.1.0-alpha.8` (cut over 2026-09-17 with the procedure
  below; the image is built on the server from the release tarball).
  All five HOME/XDG directories are isolated under `/var/lib/warden/sbx`.
- Docker Compose project `warden`: policy broker (`warden policy`, Go, with
  in-process loopback gateways), runner, chat/API/static UI.
  They run as the Warden UID, read-only root filesystem, dropped capabilities,
  no Docker socket, and host networking for private loopback SBX gateways.
- `warden-edge.service`: Google ID-token verification and authenticated preview
  ingress; reads the chat endpoint file locally, including credential rotations.
- Existing Caddy terminates TLS. Separate files in persistent `/data/sites/`
  add Warden routes. Panta and SIEM routes/services remain unchanged.

State is private under `/var/lib/warden/{app,runner,policy,provider,sbx}`.
The model login is `/var/lib/warden/provider/auth.json`, mode 0600, Warden-owned;
only the policy container mounts it. The runner and guests never receive it.
The pinned x86_64 Codex 0.154.0 bundle was copied once from server assets into
Warden's own storage. The model login was created directly on OVH using device
authorization for the current Codex account. No Panta storage is mounted at runtime.

Claude chats use the pinned x86_64 Claude Code 2.1.272 executable at
`/opt/warden/claude/claude` (root-owned, world-readable; checksum recorded in
`/opt/warden/claude/VERSION` and verified against Anthropic's release manifest).
The runner mounts it read-only and copies it into a guest for each Claude run.
Its login is `/var/lib/warden/provider/claude.json`, also mode 0600 and
Warden-owned, holding only a long-lived `claude setup-token` value; the policy
broker injects it for `api.anthropic.com` message routes and nothing else. Create
or renew it (tokens last one year; Warden records 364 days) from a machine with
a browser: run `claude setup-token`, then on OVH run
`sudo -u warden /opt/warden/current/scripts/warden-claude-token /var/lib/warden/provider/claude.json`
and paste the value at the hidden prompt. Until that file exists, Claude chats
fail with "Refresh the Claude sign-in"; Codex chats are unaffected.
The two local prototype test chats remain preserved in `~/.warden-chats` on the
Mac; the OVH workspace starts with its own sandbox identities and state.

The OVH configuration limits resident Warden environments to two (1 CPU,
1536 MiB per environment) to fit alongside existing services. Demo pages use
plain HTML/CSS and Python's HTTP server, without dependency-heavy builds. Chats can share
an environment; idle environments stop after 15 minutes unless they have an
available published preview. The runner holds a credential-free SBX exec session
for each resident environment, preventing SBX's independent 30-second auto-stop
after agent disconnect. Warden releases this session on stop or shutdown.
Published previews keep their sandbox running until
unpublished or explicitly stopped. After an explicit stop or service restart,
resume the chat and its server and request the same port binding to restore the
preview. Persistent files and chat history survive normal service restarts.

## Build and launch

Releases are built by the `Warden release` workflow
(`.github/workflows/release.yml`) from a tag `v*`, or locally with
`scripts/release.sh` when Actions minutes are unavailable (same tarballs and
`SHA256SUMS`; `--publish` creates the GitHub release with `gh`; the server
image is then built on the server as before): the frontend, the one
`warden` binary (the services are its `policy`, `runner`, `serve` and
`edge` subcommands) for linux/amd64, linux/arm64 and darwin/arm64 with the tag
linked in as the revision, one tarball per target
(`warden-<tag>-<os>-<arch>.tar.gz` holding `bin/`, `web/`,
`config/policy.template.json`, `config/warden.server.example.json` and
`vendor/`), `SHA256SUMS`, a GitHub release, the server image
`ghcr.io/monaddle-too/warden:<tag>` for linux/amd64 and linux/arm64
(from `deploy/chat/Dockerfile`; the run summary lists the index digest to
pin and the per-platform manifest digests), and the Helm chart for the
Kubernetes shape at `oci://ghcr.io/monaddle-too/charts/warden` (chart
version `<tag without v>`, `appVersion` the tag, values pinned to that
image by digest; `scripts/package-chart.sh`, also attached to the release
as `warden-<version>.tgz`; `scripts/release.sh --chart` packages it
locally and `--publish` pushes it with your own `helm registry login
ghcr.io`; see `docs/warden-kubernetes.md`). A manual run builds everything
under a `v0.0.0-dev.<sha>` version, packages the chart without pushing it
and publishes no GitHub release. The
binary and each service subcommand print `<name> <revision> protocol=<n>`
with `--version`; the chat service refuses to start beside a runner or
policy service on another protocol number and logs a warning for a
different revision on the same protocol, so the containers may be updated
one at a time as before.

To build the same image by hand (for example on OVH before the first
workflow run), from the repository root:

```sh
pnpm --dir chat/web install --frozen-lockfile
pnpm --dir chat/web build
mkdir -p dist/linux-amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C chat build -trimpath \
  -ldflags "-X warden/chat/internal/release.Revision=$(git describe --always --dirty)" \
  -o ../dist/linux-amd64/warden ./cmd/warden
docker build -f deploy/chat/Dockerfile -t warden:<revision> .
```

Upload only `config/policy.template.json`, `vendor`, `deploy/chat`, the built
binaries under `dist/linux-amd64`, and `chat/web/dist`. Exclude AppleDouble
files and filesystem xattrs from tar archives. Never include private state or
logins. The image contains no Python or mitmproxy: `warden policy selfcheck`
loads the REST catalog, GitHub networks and policy template and initialises
an engine and gateway CA as the packaging smoke check. Keep previous
images/releases until the new release is verified.

## Policy broker

`warden-policy` replaces `python -m warden.sbx` and the per-sandbox `mitmdump`
gateways. It speaks the same version 1 control protocol on
`/state/sbx-control.sock`, keeps the same state layout under
`/var/lib/warden/policy` (`bindings.json`, `runtime-identities.json`,
`sandboxes/<digest>/`, `sharing.sqlite`, `google.sqlite`). Gateways run
inside the policy process on loopback ports recorded in `gateway-port.json`.
The `--mitmdump` flag is accepted and ignored.

The verifier runs off the chat path: host-wide checks are one cached fact
refreshed every two minutes, per-sandbox checks run when a runtime is first
proven and then every two minutes while the sandbox is warm (a lease, or a
`begin` in the last 20 minutes), and proofs last five minutes. `check`,
`begin` and `renew` only probe the gateway and read the cached proof. A
binding without a fresh proof is trusted provisionally once its gateway is
up and its gateway-only rule is established, and verified in the background
right after; a failed check revokes leases and re-applies the sandbox deny
rule as before, and that binding is then verified synchronously until a
full pass restores trust.

All gateways of one policy installation sign with a single host-wide CA kept
in `/var/lib/warden/policy/gateway-ca/` (`mitmproxy-ca.pem` holds the key,
`mitmproxy-ca-cert.pem` the public certificate); each gateway still mints its
own per-host leaf certificates from it. The runner installs the public CA
into a guest once and records its fingerprint per sandbox, so later runs skip
the install unless the CA changed or the guest lost the file. The service
rotates the CA at startup when it is older than `--gateway-ca-max-age`
(default 365 days; 0 disables). To rotate on demand, stop the policy service,
run the subcommand, then start it again; the previous CA directory is kept
as `gateway-ca.retired-<timestamp>` and guests pick up the new CA on their
next run:

```sh
sudo docker compose -f /opt/warden/current/deploy/chat/compose.yaml stop policy
sudo docker compose -f /opt/warden/current/deploy/chat/compose.yaml run --rm --no-deps policy warden policy rotate-gateway-ca --state /state
sudo docker compose -f /opt/warden/current/deploy/chat/compose.yaml start policy
```

The Warden guest image (`deploy/guest/`, published by the `Warden guest
image` workflow to `ghcr.io/monaddle-too/warden-guest`) ships the Codex
bundle, the Claude executable and this CA preinstalled. Install a published
image on the host with `sudo deploy/guest/install.sh IMAGE:TAG sha256:DIGEST`
(pull needs `docker login ghcr.io` with `read:packages`); it loads the image
into the sandbox runtime and sets `WARDEN_GUEST_TEMPLATE` and
`WARDEN_GUEST_DIGEST` in `.env`, which `compose.legacy.yaml` passed as the
runner's `--template` and the policy service's `--guest-image-digest`; with
`compose.yaml` copy them into `warden.json` as `sbx.guestImage` (the
reference without `@digest`) and `sbx.guestImageDigest` and run
`docker compose up -d`. New sandboxes then start with
nothing to copy; the runner still copies into guests made from the stock
template. After a CA rotation or a runtime bump, rebuild the image (update
`deploy/guest/warden-proxy.crt` or the pinned versions) and reinstall.

The GitHub App broker is the `warden policy github-broker --store DIR
--app-id ID --owner LOGIN` subcommand with the unchanged stdin/stdout JSON
protocol. Before switching, check `/var/lib/warden/github/broker.json`: its
`command` runs inside the policy container, so a command that invoked
`python -m warden.github_app` must be changed to
`/usr/local/bin/warden policy github-broker …` with the same `--store`,
`--app-id` and `--owner` values. A command that runs elsewhere (for example
over SSH) is unaffected. Git push reviews run `git` from the image under
`warden policy git-limited` resource limits in `/dev/shm`.

Switching back to the previous Python image needs no state migration: the
files, schemas and directory names are unchanged.

Install the service account, private directories, pinned SBX executable and an
independent x86_64 Linux Codex 0.154.0 runtime at `/opt/warden/runtime` and the
Claude Code executable at `/opt/warden/claude/claude` before starting. `warden-sbx` selects the isolated namespace for every host command.
Initialize it with deny-all, disable `ssh.agentForwardingEnabled`, set
`proxy.sandbox` to `direct`, and restart the daemon. Complete `warden-sbx login`
using its device flow. Check empty MCP inventory, no global network allows and
implicit default denial. The verifier checks all of these again for each run.

Create `.env` beside the installed Compose file with `WARDEN_IMAGE`, `WARDEN_UID`,
`WARDEN_GID` and `WARDEN_CONFIG_HOST` (the host path of `warden.json`, see
"Configuration file"). The current installation uses UID/GID 977. Start using:

```sh
sudo systemctl enable --now warden-sbx
sudo docker compose -f /opt/warden/current/deploy/chat/compose.yaml up -d
sudo systemctl enable --now warden-edge
```

`edge.example.json` contains public configuration only; install privately as
`/opt/warden-preview/config.json` and adjust origin/client/owner allowlist for
other deployments. `loginsFile` is the edge's private sign-in ledger behind the
owner admin console; its directory (`/var/lib/warden/edge`, Warden-owned, 0700)
must be listed in the unit's `ReadWritePaths` because the unit uses
`ProtectSystem=strict`. The public app requires a verified, allowlisted Google email,
not merely any Google account. Initial access is the owner's Google account.
Model login renewal is performed on the server with the pinned Codex CLI's
`login --device-auth`, writing only into Warden's provider directory; a browser
may approve the device code. The gateway refuses expired model credentials.

## Configuration file

All four binaries accept `--config PATH` (default `$WARDEN_CONFIG`) naming a
`warden.json` (schema: the appendix of
[docs/warden-local-deployments-plan.md](../../docs/warden-local-deployments-plan.md)).
`warden.example.json` holds the current OVH values in that schema
(`providers.github.appID` is a placeholder the service only validates; the
real ID is in `/var/lib/warden/github/broker.json`). `compose.yaml` passes
`--config /etc/warden/warden.json` to the three services, bind-mounted
read-only from the host file `WARDEN_CONFIG_HOST` in `.env` (default
`/opt/warden/current/deploy/chat/warden.json`). Every path in the file is a
container path, so the Compose file mounts each host directory at the same
path inside the container (`/var/lib/warden/{policy,runner,app,provider,
github,sbx}`, `/opt/warden/{runtime,claude}`, `/usr/bin/sbx`); the UI,
catalog and template are the image's `/app` paths. The previous
flag-configured file is kept as `compose.legacy.yaml` for rollback. Every
flag keeps working: without a file it overrides the computed defaults, with
a file it must agree with the loaded value or the service refuses to start
naming the flag and the field. `--manage-network` is accepted and always
on; `--mitmdump` is accepted and ignored. The edge unit keeps its own
`/opt/warden-preview/config.json` in the original `edge.example.json` shape;
`warden edge --config` also accepts `warden.json` and derives the same
settings from it (a test keeps the two example files in agreement).

Tests in each command (`TestOVHExampleFileMatchesTheComposeDeployment`,
`TestOVHExampleFilesAgree`) check that `warden.example.json` under those
mounts resolves to exactly the values the legacy command lines produced.
Two things moved from `.env` into the file: the guest image
(`WARDEN_GUEST_TEMPLATE`/`WARDEN_GUEST_DIGEST`, now `sbx.guestImage` and
`sbx.guestImageDigest`; after `deploy/guest/install.sh` copy the values it
prints into `warden.json` and `docker compose up -d`) and the warm spare
count (`WARDEN_SPARE_SANDBOXES`, now `sandboxes.warmSpares`).
`WARDEN_GITHUB_APP_BROKER` stays in the Compose file and must equal
`providers.github.brokerFile` (`/var/lib/warden/github/broker.json`); the
service refuses a disagreement. `/var/lib/warden/github` is mounted both at
that path and at the previous `/run/github`, so a `broker.json` whose
`command` names `--store /run/github/...` keeps working; new commands should
use `/var/lib/warden/github/...`.

| Flag | `warden.json` field |
|---|---|
| `warden policy --state` | `paths.state` + `/policy` (`/var/lib/warden/policy`; `compose.legacy.yaml` mounted it at `/state`) |
| `warden policy --sbx`, `warden runner --sbx` | `sbx.executable` |
| `warden policy --vendor-dir` | `paths.githubCatalog` |
| `warden policy --policy-template` | `paths.sandboxPolicyTemplate` |
| `warden policy --gateway-ca-max-age` | `sbx.inspectionCertMaxAgeDays` |
| `warden policy --guest-image-digest` | `sbx.guestImageDigest` (default: the stock template digest) |
| `warden policy --codex-auth-file` | `providers.codex.authFile` |
| `warden policy --claude-auth-file` | `providers.claude.authFile` |
| `warden policy --google-config` | `providers.google.docsClient` (a file path; `builtin` is the shared client) |
| `WARDEN_GITHUB_APP_BROKER` | `providers.github.brokerFile` (with `appID`, `appSlug`, `installationOwner`; `github-broker --app-slug` defaults to `monaddle-workspace`) |
| `warden policy --github-auth-file` | `providers.github.authFile` (local mode: the user token from `warden login github`; exclusive with the App broker) |
| `warden policy --chat-listen` | `chat.listen` (its port is the loopback redirect of the built-in Google Docs client) |
| `warden runner --root`, `--socket` | `paths.state` + `/runner`, `/runner/worker.sock` |
| `warden runner --warden-socket`, `warden serve --warden-socket` | `paths.state` + `/policy/sbx-control.sock` |
| `warden runner --template` | `sbx.guestImage` `@` `sbx.guestImageDigest` |
| `warden runner --runtime-dir`, `--claude-path` | `runtimes.codex`, `runtimes.claude` |
| `warden runner --sandbox-memory-mb`, `--max-resident`, `--spare-sandboxes`, `--idle-timeout`, `--retained` | `sandboxes.memoryMB`, `maxRunning`, `warmSpares`, `stopAfterIdleMinutes`, `keepStopped` |
| `warden serve --state`, `--runner-socket` | `paths.state` + `/app`, `/runner/worker.sock` |
| `warden serve --listen` | `chat.listen` |
| `warden serve --web-dir` | `paths.webAssets` |
| `warden serve --preview-suffix` | `previews.hostSuffix` (`previews.mode` `public`; `localhost` means loopback previews through the edge on `previews.edgeListen`) |
| edge `origin`, `previewSuffix`, `listen` | `auth.publicURL`, `previews.hostSuffix`, `previews.edgeListen` |
| edge `clientID`, `ownerEmails`, `demoDomains`, `loginsFile` | `auth.google.signInClientID`, `owners`, `demoDomains`, `signInLedger` (`auth.mode` `google`) |
| edge `upstream`, `upstreamHost`, `ownerTokenFile` | `chat.listen`, `paths.state` + `/app/endpoint.json` |

Left as flags: the runner's `--parallel` and `--tls-*` options and the
policy service's `--document-api-*` pair.

## Cutover to warden.json (OVH release procedure)

The switch from `compose.legacy.yaml` (flags) to `compose.yaml`
(`warden.json`) is one release with the usual rollback: the previous
release directory and its Compose file stay on disk. State is untouched;
only the command lines and mount paths change. Run as root on OVH; `<tag>`
is the release tag, `<index-digest>` the server image's index digest from
the workflow summary.

1. Obtain the release. Either let the `Warden release` workflow publish it
   (needs the Actions budget) and pull the image (`docker login ghcr.io`
   with `read:packages`; `docker pull ghcr.io/monaddle-too/warden:<tag>`
   and check `docker image inspect --format '{{index .RepoDigests 0}}'`
   shows `<index-digest>`), or build it on the server from the same commit
   with the hand-build commands above and tag it `warden:<tag>`.
2. Install the release directory: unpack `warden-<tag>-linux-amd64.tar.gz`
   (or the upload of `deploy/chat`, `config`, `vendor`) to
   `/opt/warden/releases/<tag>` and check `bin/warden --version` there
   prints `warden <tag> protocol=2`. Do not move
   `/opt/warden/current` yet.
3. Write `/opt/warden/releases/<tag>/deploy/chat/warden.json`: copy
   `warden.example.json`, then set `providers.github.appID` to the App ID in
   `/var/lib/warden/github/broker.json` and, if a guest image is installed,
   `sbx.guestImage` and `sbx.guestImageDigest` to the current
   `WARDEN_GUEST_TEMPLATE` (reference without `@digest`) and
   `WARDEN_GUEST_DIGEST` from `.env`. Keep it root-owned, mode 0644 (no
   secrets; the containers read it as the Warden UID). A malformed file, an
   unknown field or a value that disagrees with `WARDEN_GITHUB_APP_BROKER`
   makes each service exit at start naming the field; step 7 reads the logs.
4. `.env` in the new release's `deploy/chat`: copy the current `.env`, set
   `WARDEN_IMAGE=ghcr.io/monaddle-too/warden:<tag>@<index-digest>`
   (or the local `warden:<tag>`), keep `WARDEN_UID`/`WARDEN_GID` (977),
   add `WARDEN_CONFIG_HOST=/opt/warden/current/deploy/chat/warden.json`.
   `WARDEN_GUEST_*` and `WARDEN_SPARE_SANDBOXES` may stay; `compose.yaml`
   ignores them and `compose.legacy.yaml` still reads them on rollback.
5. Check `broker.json`: its `command` must name the single binary
   (`/usr/local/bin/warden policy github-broker --store ... --app-id ...
   --owner ...`) and `owner` must be the account's current login (it was
   rewritten to `monaddle-too` at the alpha.8 cutover after the account
   rename; the old name is redirected by GitHub but the broker compares
   logins exactly). A `--store /run/github/...` path keeps working (that
   mount is kept).
6. Switch: `ln -sfn /opt/warden/releases/<tag> /opt/warden/current.new && mv -T /opt/warden/current.new /opt/warden/current`
   (the previous target remains under `/opt/warden/releases/`), then
   `docker compose -f /opt/warden/current/deploy/chat/compose.yaml up -d`
   and `systemctl restart warden-edge` only if the edge binary changed
   (copy `bin/warden` to `/opt/warden-preview/warden.new` and `mv -f` it
   over `/opt/warden-preview/warden`: a plain `cp` onto the running binary
   fails with "Text file busy"; then install the updated
   `warden-edge.service`, whose ExecStart is `warden edge`; the config file
   is unchanged).
7. Verify, in order:
   - `docker logs --tail 20 warden-chat-1` shows
     `warden-chat <tag> protocol=2; warden-runner <tag> protocol=2; warden-policy <tag> protocol=2`
     and no `warning:` line about revisions;
   - `curl -sS -o /dev/null -w '%{http_code}\n' https://warden.monaddle.com/`
     is `200` (root);
   - `curl -sS -o /dev/null -w '%{http_code}\n' https://warden.monaddle.com/api/state`
     is `401` (signed out);
   - `systemctl is-active warden-edge warden-sbx` prints `active` twice and
     `docker compose -f /opt/warden/current/deploy/chat/compose.yaml ps`
     shows the three services running;
   - sign in, open a chat and complete one Codex turn (the agent answers
     and `sudo -u warden /opt/warden/warden-sbx ls` shows its sandbox).
8. Rollback (any step above fails): point `/opt/warden/current` back at the
   previous release directory the same way and run
   `docker compose -f /opt/warden/current/deploy/chat/compose.legacy.yaml up -d`
   (the previous release ships the flag-configured file as `compose.yaml`;
   `compose.legacy.yaml` in the new release is the same content, so either
   works with the previous `.env` and `WARDEN_IMAGE`). Restore the previous
   `warden-edge` binary if it was replaced. State needs no migration in
   either direction.

## Ingress

DNS A records `warden` and `*.preview` point to the OVH server. The host firewall
allows port 19081 only from the Caddy Docker bridge (`br-18bf3d714c9c`,
172.18.0.0/16 to 172.18.0.1); port 18780 is loopback-only. No port 19082 relay exists.

Validate the complete Caddy configuration before reloading. The global on-demand
TLS ask endpoint authorizes certificates only for current approved binding IDs;
the catch-all site routes only `*.preview.monaddle.com` to Warden. Existing exact
site routes remain in place. See Caddy's [on-demand TLS documentation](https://caddyserver.com/docs/caddyfile/options#on-demand-tls).

Agent `sandbox_bind_port` (and compatible `preview_attach`) requests require an
owner approval. URLs are `https://<binding-id>.preview.monaddle.com/`. Guest HTML
cannot be served from Warden's authenticated app origin. A one-use ticket binds
a host-only preview session to the main Google session and a browser challenge.
Every preview request checks the session and approved binding; logout/revocation
cancels streams. Cookies and Warden/provider authorization are stripped upstream.
The edge tells the chat who each request is from (`X-Warden-Principal`, the
Google subject; `X-Warden-Email`; `X-Warden-Name`, the account's display
name when Google supplies one), after discarding any such headers a client
sent. The chat attributes user messages to that identity (name, else email)
and keeps a per-chat "is typing" indicator for 8 s after each keystroke a
client reports, visible to everyone else in the chat. On a local install the
only identity is the owner.
Unpublishing persists revocation before trying worker cleanup. Stopped servers
return unavailable until the chat restarts its server/revalidates the binding.

## Operations

```sh
sudo systemctl status warden-sbx warden-edge
sudo docker compose -f /opt/warden/current/deploy/chat/compose.yaml ps
sudo docker logs --tail 50 warden-chat-1
sudo docker logs --tail 50 warden-runner-1
sudo docker logs --tail 50 warden-policy-1
sudo -u warden /opt/warden/warden-sbx ls
```

An edge restart signs viewers out; chat and agent work remain server-side.
A chat/worker restart marks uncertain in-flight work interrupted; it never replays
it silently. Back up private state with services quiesced and SBX stopped; retain
old state before rollback. Do not replace state with development snapshots.

Preview isolation audits run in the worker background once per minute, seeded
by the successful prepare/port-attach check. HTTP requests only inspect the
current result, sandbox generation, identity and binding approval. A result
older than 90 seconds denies new preview requests; a failed background audit
stops the sandbox. Full checks run outside the worker mutex so a routine audit
does not stall requests. Authentication and revocation remain per-request.
