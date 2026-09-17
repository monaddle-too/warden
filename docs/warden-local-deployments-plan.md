# Warden on arbitrary local deployments

Status: plan, approved direction September 15, 2026. Branch
`codex/warden-local-deployments`, started from main c47a718. Step 0 is done; the
four tracks are in progress. This document records the inventory, decisions and work.

## Objective

Anyone with a supported machine installs Warden and gets the milestone flow
(chat → Warden-managed SBX agent → approved preview → Google Docs and GitHub
through the policy broker) without the OVH runbook, without a domain, TLS
certificates or a Google sign-in client, and without hand-copying runtimes or
filling in a configuration file.

"Local deployment" here means a single-owner installation on the operator's
own machine: Apple Silicon Mac or an x86_64 Linux host with KVM. The OVH
installation keeps working unchanged throughout. Server mode, meaning a shared
multi-user environment, is a successor plan (see Decisions 8 and 9); this plan
only avoids building anything that plan would have to undo.

## What is coupled to the OVH host today

Inventory taken from main c47a718.

- **Layout.** `deploy/chat/compose.yaml`, the systemd units, the Caddy
  fragment, `deploy/guest/install.sh` and `edge.example.json` fix `/opt/warden`,
  `/var/lib/warden`, UID/GID 977, host networking, the Caddy bridge address,
  `warden.monaddle.com`, `*.preview.monaddle.com`, the Monaddle Google client
  and the owner email. Images are built on the server from an uploaded
  tarball, linux/amd64 only. There is no published release artefact.
- **Hardcoded identifiers in code.** The GitHub App slug `monaddle-workspace`
  in `chat/internal/policy/githubapp.go` and the installation link in
  `chat/web/src/components/RepositorySharing.tsx`.
- **No previews without a public suffix.** `scripts/warden-chat` starts
  `warden-chat` with an empty `--preview-suffix`; the port tool then answers
  "external previews are not configured" (`chat/internal/chats/ports.go`) and
  approved bindings are always `https://<id>.<suffix>`. The runner's loopback
  publish (`sbx ports --publish 127.0.0.1:…`) exists but nothing calls it.
  `docs/warden-chat.md` still describes iframe previews in local mode.
- **SBX host must be a private, exactly pinned namespace.** The verifier
  requires SBX 0.42.1, `proxy.sandbox=direct`, agent forwarding off, no MCP
  servers, no global network allows and implicit denial. A developer's shared
  SBX fails these as soon as it has one allow rule. OVH isolates Warden with
  the `warden-sbx` wrapper (five HOME/XDG directories); nothing creates that
  namespace elsewhere. The stock template digest is pinned in code; the
  guest image digest is a flag.
- **Runtimes are pinned per architecture with no fetcher.** Codex must be
  0.154.0 for the guest architecture, inferred from the host architecture in
  `chat/internal/sandbox/runtime.go`; Claude must be 2.1.272. The guest image
  (`deploy/guest/Dockerfile`) fetches only x86_64 Codex and linux-x64 Claude,
  so an Apple Silicon host copies a host bundle the operator obtained by hand.
  The published guest image build is blocked by the GitHub Actions budget.
- **Identity.** The edge needs a Google OAuth client and an owner allowlist.
  Local mode has one bearer capability from `endpoint.json`, no identity, no
  admin console. All admitted users share one `chats.json`, one provider login
  and one sandbox pool.
- **Provider logins are manual and owner-shared.** Codex is an existing host
  `auth.json`, never refreshed; Claude is a `setup-token` value installed with
  `scripts/warden-claude-token`; Google Docs and GitHub use Monaddle-owned
  registrations, and GitHub specifically needs the App's private key on the
  host. The Google consent screen is in Testing, so refresh tokens expire
  after seven days and only listed test users can consent.
- **Ingress.** Wildcard DNS, Caddy on-demand TLS with the `/_tls/allow` ask
  endpoint, the edge listening on the Docker bridge, KVM on Linux. None of
  this is documented as a support matrix.

## Decisions

1. **Two shapes, one binary set.** Local single-owner mode (this plan) and
   server mode share the same `warden-policy`, `warden-runner`, `warden-chat`
   and `warden-edge` binaries; only configuration differs.
2. **Local mode is autoconfigured; the config file is optional.** Every
   setting has a computed default: paths derive from one state root, release
   constants (SBX, Codex and Claude versions, guest image digests, the policy
   template, the GitHub catalog, the built-in OAuth clients) live in the
   binary, host facts (SBX executable, architecture, memory, free ports) are
   detected, and mode consequences (loopback previews, owner auth) follow
   from the mode. `warden install` writes a `warden.json` holding only what
   it detected and where the logins are, for `doctor` and support to read.
   Explicit configuration is the server-mode artefact; its schema is in the
   appendix. A flag and a config value that disagree is an error.
3. **Local previews use loopback and `*.localhost`.** Approved bindings are
   published by the runner on a loopback port and served by the edge over
   plain HTTP as `http://<binding-id>.localhost:<edge-port>/`. Browsers resolve
   `*.localhost` to loopback without DNS, and it keeps each preview on its own
   origin, so the existing per-binding session, ticket and revocation model
   applies unchanged. No TLS in local mode.
4. **Local identity is the owner capability, not Google.** The edge runs in
   local mode without Google sign-in: one owner, authenticated by the
   launcher's capability, with the same preview ticket flow. The admin console
   stays server-only.
5. **Google Docs uses one Warden-wide client (owner decision: this option
   only, bring-your-own deferred).** Register a Desktop-type OAuth client in
   the Monaddle project, complete Google's verification for the Docs and
   Drive metadata scopes once, and ship its client ID with Warden. Desktop
   clients accept loopback redirect URIs with any port, so a local install
   needs no per-deployment registration. Google treats Desktop client secrets
   as non-confidential; Warden still keeps it out of guests and redacts it.
   The existing `--google-config` file remains as an override.
6. **GitHub in local mode is a user OAuth token obtained the way the GitHub
   CLI obtains one.** Warden registers its own GitHub OAuth App (not a GitHub
   App); its client ID is public and ships in the binary; `warden login
   github` runs GitHub's device flow and stores the resulting classic user
   token, 0600, in the provider directory, exactly as Codex's login is
   handled. A pasted `gh auth token` value is accepted as a fallback. The
   token is never copied into a guest and is injected only into requests
   the gateway has approved; the repository allowlist and per-request
   approvals are the boundary, as they already are for Codex, Claude and
   Google. Actions appear as the person. No GitHub App key, no hosted
   broker, no GitHub verification process.
7. **Runtimes stay pinned; sbx is not.** Codex 0.154.0, Claude 2.1.272 and
   the template digests are pinned and the installer fetches exactly them.
   The sbx version is not required anywhere (owner decision 2026-09-16):
   Warden needs an executable that answers `sbx version` and assumes the
   features it uses are present, failing at the call that needs a missing
   one rather than up front. `release.SBXTestedVersion` records what the
   acceptance runs used, for messages only.
8. **Provider connections and grants are keyed by principal from the
   start.** Local mode has exactly one principal, the owner, but the broker's
   provider sources, the sharing store and the grant rows record who
   connected and who approved, so server mode can hold many without a
   schema change. The sharing store already records which chat asked; it
   gains which principal.
9. **Server mode is a successor plan, not a configurable OVH.** The target
   server is a shared, non-demo, multi-user environment: per-user Google and
   GitHub connections, per-user or per-team chat visibility, Google sign-in
   with the existing web client and an https callback, and GitHub through
   the GitHub App in user-to-server mode (the organisation installs the App
   on admitted repositories; each user authorises it; tokens are scoped to
   the intersection). The current owner-shares-everything OVH deployment is
   a stepping stone. This plan keeps the App broker working for OVH and
   invests nothing further in it.
10. **Warden owns its SBX namespace on every host.** The installer creates
    the private HOME/XDG directories and a wrapper equivalent to
    `warden-sbx`, runs the deny-all init and settings, and the services only
    ever run SBX through it. The operator's own SBX namespace is never
    inspected or changed.
11. **OVH is untouched until the end.** Defaults for every new option
    reproduce the current OVH behaviour; the OVH release moves to the new
    configuration surface only in the last step, with the same rollback rule
    as every previous release.

## Work

### 1. Defaults, detection and the optional config file

- Settle the `warden.json` schema (appendix) first, in a short task, so the
  other tracks code against it.
- Every flag of the four binaries gets a computed default; all four read
  the optional file from `--config` or `$WARDEN_CONFIG`; the existing flags
  remain as overrides for one release; disagreement is an error.
- Path derivation from `paths.state`; release constants moved into one Go
  package; host detection (SBX executable search and version check, guest
  architecture, memory and core based sandbox sizing, free loopback ports).
- Move the GitHub App slug, installation link and App ID into config; hide
  the repository, Google and GitHub sections of the UI when their provider
  is absent (`sharing/status` gains `configured` beside `connected`).
- Principal on provider sources, sharing grants and repository selections
  (decision 8); local mode fills it with the owner.
- Compose file and units read the same file; `deploy/chat/README.md` shows
  how the current OVH values map onto it.

Progress (Track A, 2026-09-15, branch `codex/wld-track-a`):

- Done: all four binaries take `--config` (default `$WARDEN_CONFIG`) and
  load `chat/internal/config`; every legacy flag maps to one field
  (`chat/cmd/*/config.go`, table in `deploy/chat/README.md`). Unset flags
  take the config value; with no file a set flag overrides the computed
  defaults; with a file a set flag must equal the loaded value or startup
  fails naming the flag, the field and the file (`config.Override`). A
  service given only its state flag runs with `config.Defaults` for the
  parent directory. `--manage-network` is always on; `--mitmdump` stays
  ignored. The runner falls back to `hostinfo.FindSBX` when nothing names
  the executable. A provider set to JSON `null` is removed (the step-0
  follow-up), which is how a file hides an integration.
- Done: release pins read from `chat/internal/release` in the verifier and
  runner; `chat/internal/hostinfo` (SBX search and version check, guest
  architecture, memory/core sizing, free loopback ports) with tests, for
  the installer to call.
- Done: GitHub App slug on `providers.github.appSlug` (`github-broker
  --app-slug`, default the OVH App); `sharing/status` gains
  `google.configured` and `github.configured` plus `github.appSlug`; the UI
  hides the repository and document sections for absent providers and
  builds the installation link from the slug.
- Done: `deploy/chat/warden.example.json` (OVH values; parse-checked by a
  test) and the README mapping. `compose.yaml` and the units are unchanged:
  they keep passing flags and move to the file in step 7 (decision 11);
  `chat/cmd/*/config_test.go` checks the OVH command lines reproduce
  today's values without a file.
- Not in this track: principal keying (Track D); the built-in Google client
  (`providers.google.docsClient: builtin` currently means "no operator
  file", so the Google section shows as unconfigured until Track D wires
  `release.GoogleDocsClientID`); the user-token GitHub provider (Track D;
  `github.configured` is already true for `providers.github.authFile`).
- Verified: `gofmt`, `go vet ./...`, `go test -race ./...` (all packages),
  frontend `tsc`/`vite build` and `vitest`.

### 2. Loopback previews

- Runner: on approval in loopback mode, publish the sandbox port to a
  Warden-chosen loopback port (existing `Publish`/`Unpublish`), record the
  host port beside the binding, and keep the current revocation-before-
  cleanup order and the stale-mapping reconciliation from the public
  preview work.
- Chat engine: binding URL scheme and host come from the preview mode;
  `ValidatePreviewSuffix` accepts `localhost`.
- Edge: `http` mode with a loopback listener, `*.localhost` host matching,
  the same one-use ticket and per-request binding checks, and the same
  cookie and authorization stripping upstream. Content-Security-Policy frame
  source derived from the configured scheme and suffix.
- Verify: signed-out denial, cross-binding isolation, revocation, streaming,
  assets, restart reconciliation, and the idle and residency rules already
  covered by the resident-session tests.

Progress (Track A, 2026-09-15):

- Runner: nothing new was needed. `preview.attach`, which the chat engine
  calls on approval (`bindPort`), already publishes the sandbox port to a
  Warden-chosen free loopback port through `Runtime.Publish`, records the
  host port beside the attachment in `managed-v2.json`, unpublishes after
  the chat has durably revoked the binding, and reconciles restored
  mappings on resume. The existing tests cover it
  (`TestRevokedMappingIdentitySurvivesRuntimeRestoration`,
  `TestUnpublishedPreviewAllowsIdleStop`,
  `TestManagedIdleLeaseAndStoppedPreview`,
  `TestExplicitCancelStopsCurrentGuestAndInvalidatesPreview`).
- Chat engine: binding URLs are `<scheme>://<binding-id>.<suffix>[:port]/`
  from `previews.mode` and the `previews.edgeListen` port
  (`Engine.PreviewScheme`, `Engine.PreviewPort`, `previewURL`);
  `ValidatePreviewSuffix` accepts `localhost`; the "external previews are
  not configured" refusal only remains for an explicitly empty suffix
  (`scripts/warden-chat`). Owner approval is unchanged.
- Edge: `auth.mode: owner` with an `http://127.0.0.1:<port>` origin listens
  on that loopback port, matches `<binding>.localhost:<port>`, uses
  host-scoped non-`Secure` cookies (`__Host-` needs TLS), keeps the one-use
  ticket, per-request binding checks, revocation and upstream stripping,
  and derives `frame-src` from scheme, suffix and port. The owner is
  identified by the `endpoint.json` capability; a capability-authenticated
  API call mints a read-only cookie session (needed because the ticket
  flow starts with a navigation), and every session ends when the chat
  rotates the capability. The original edge JSON still loads for OVH.
- Verified with unit and integration tests only (fake worker and fake
  upstream): `chat/internal/edge` loopback tests (signed-out and
  wrong-host denial, owner ticket flow, cookie-only writes refused,
  cross-binding isolation, revocation, capability rotation, stream
  cancellation on revocation, asset proxying with header stripping and
  CSP), `chat/internal/chats` `TestLoopbackBindingURLsCarryTheEdgePort`,
  the runner tests above. No live SBX acceptance was run on this Mac (the
  local sandbox daemon was not to be touched); the end-to-end run on a
  laptop waits for the Track B launcher that starts the edge.
- Known limit: the owner cookie session lasts eight hours; after it lapses
  a preview tab redirects to the app, which mints a new session on its next
  API call, and the preview must be reopened from the chat page.

### 3. Installer, doctor, logins and launchers

- `warden install` (a subcommand of `warden-chat` or a small `warden`
  launcher binary; decide during implementation) that: creates the state
  root with owner-only modes; creates the private SBX namespace and wrapper;
  runs `daemon start --policy deny-all`, sets the two settings, checks empty
  MCP inventory and no global allows; runs the SBX device login; fetches the
  pinned Codex bundle and Claude executable for the host's guest
  architecture by version and SHA-256 (x86_64 SHAs are in
  `deploy/guest/Dockerfile`; add the aarch64 Codex bundle and the linux-arm64
  Claude executable, verified against Anthropic's release manifest); loads
  the guest image; writes `warden.json`.
- `warden doctor` that runs every verifier host check and every runner
  bundle check and prints exact remediation, so a failing install never
  reaches the verifier's opaque "unsupported SBX version" or "unsafe SBX
  setting".
- Logins, each writing one private file in the provider directory:
  `warden login codex` (device flow, as on OVH), `warden login claude` (the
  existing token script), `warden login google` (Desktop-client consent with
  a loopback redirect, decision 5), `warden login github` (device flow with
  Warden's OAuth App client ID, or a pasted token, decision 6).
- Mac and Linux launchers replace `scripts/warden-chat`: run the three
  services plus the edge as owner processes with the private state, without
  Docker, from the installed binaries. Keep the single-writer locks.

Progress (Track B), 2026-09-15, branch `codex/wld-track-b`:

- Done: a small `warden` launcher binary (`chat/cmd/warden`, standard
  library plus `config` and `release`), the "small launcher binary" option
  of the first bullet. `install` creates the owner-only state root and the
  private SBX namespace with a `bin/warden-sbx` wrapper, starts the daemon
  deny-all, sets the two settings, mirrors `verifier.go` `hostChecks`
  command for command, runs the SBX device login once (recorded in
  `sbx/login.json`; SBX has no query for it), fetches the pinned Codex
  bundle and Claude executable by URL and SHA-256 for the guest
  architecture (an unpinned architecture is refused with a message),
  loads a pinned guest image or records the stock template, and writes
  `warden.json` through `config.Write` with the detected facts; every step
  is idempotent, logins and sandboxes are never touched, a state directory
  from another release is refused without `--upgrade`. `doctor` prints
  PASS/FAIL per host and bundle check with the exact remediation. `login
  codex|claude|github` each write one 0600 file. `start` and `open` replace
  the Python `start`/`open` (launcher lock, readiness waits, reverse-order
  shutdown); `scripts/warden-chat` carries a deprecation note. Guide:
  `docs/warden-local-install.md`.
- Verified: race tests with a fake `sbx` script and an in-process download
  server (install, idempotency, SHA-256 mismatch, unpinned architecture,
  host-check failure, foreign state, doctor output, logins, legacy flag
  sets, archive escapes); `go vet`, `gofmt`; the doctor host-check lines
  against this Mac's real `sbx` with read-only commands only (all six
  pass in the owner's namespace, format confirmed).
- Not exercised: a real `warden install` (daemon start, device login,
  downloads) and `warden start` on this Mac, because the owner's SBX
  daemon must not be touched from this branch and the arm64 runtime SHAs
  are still empty; the Linux/KVM VM run. `sbx daemon status` is assumed
  to exit non-zero when no daemon runs in the namespace.
- Merge notes: `start` passes `--config` only when `warden-policy -h`
  lists that flag and the legacy flags otherwise, in which case the edge
  is not started (no local mode before step 2); remove the legacy sets
  after Track A merges. `warden login github` calls a local
  `gitHubLogin` interface whose stub returns "GitHub login is not
  available in this build"; the merge replaces it with
  `login.GitHub(ctx, os.Stdout, authFile)` and `login.GitHubPaste(token,
  authFile)` from Track D. `hostinfo.go` holds minimal detection helpers
  to reconcile with Track A's `chat/internal/hostinfo`. `warden login
  google` is Track D's. `deploy/chat/README.md`'s config mapping and the
  Linux VM run remain.

### 4. GitHub user-token provider

- A user-token provider source beside the App broker in
  `chat/internal/policy`, selected when the provider file holds a user token.
  Same injection point in the gateway; same redaction; expired or revoked
  tokens fail closed with "Refresh the GitHub sign-in", matching Codex.
- Repository selection lists the user's repositories (`GET /user/repos`)
  instead of an installation's; push review and pull request creation
  already use bearer HTTPS and need no change beyond the token source.
- The App slug check and install link are skipped when no App is
  configured (from step 1). The App broker path stays as it is for OVH.
- Register the Warden GitHub OAuth App under the Monaddle organisation with
  device flow enabled; record its client ID in the release constants.

Progress (Track D), 2026-09-15, branch `codex/wld-track-d`:

- Done. `GitHubCredentials` (`chat/internal/policy/githubuser.go`) is the
  interface the engine, sharing store, gateway and pull requests see; the
  App broker and the new `GitHubUserCredentials` implement it. The user
  source reads `{"token","login","scopes","obtained"}` from a private 0600
  file through `openPrivate`, registers the token with the redactor,
  injects `Authorization: Bearer <token>` at the existing injection points
  only after a grant matched, rechecks `GET /repos/{repo}` (and the pinned
  repository ID) on each approval, and fails closed with "Refresh the
  GitHub sign-in before using repositories" when the file is missing,
  malformed, public, a symlink, or the token is rejected. Listing uses
  `GET /user/repos?per_page=100&affiliation=owner,collaborator,organization_member`
  with pagination; user mode has no owner boundary and never touches the
  App slug check (that lives in the broker's `readApp`). `warden-policy`
  gains `--github-auth-file` (exclusive with `WARDEN_GITHUB_APP_BROKER`;
  `chat/cmd/warden-policy/providers.go`). `chat/internal/login` provides
  `GitHub(ctx, w, authFile)` (device flow, scopes `repo read:org`,
  slow_down handling, atomic 0600 write) and `GitHubPaste(token, authFile)`.
  Git over HTTPS keeps the gateway's `x-access-token:<token>` basic-auth
  form: GitHub's docs state the username is not used when a token is the
  password, and the GitHub CLI's credential helper sends `x-access-token`.
- Verified: `go test -race ./...` in `chat` (policy: file shape and
  permissions, injection only after approval, pagination and selection,
  fail-closed on a missing file, revoked token, registry mutual exclusion,
  gateway injection; login: pending, slow_down, expired_token,
  access_denied, cancellation, pasted-token validation).
- Remaining: register the OAuth App (device flow enabled) and paste its
  client ID into `release.GitHubOAuthClientID`; Track A maps
  `providers.github.authFile` to `--github-auth-file`; Track B wires
  `warden login github` to the library; live verification of listing and
  push through a real token once the App exists.

### 5. Guest image for both architectures

- Build `deploy/guest/Dockerfile` for linux/amd64 and linux/arm64 with the
  per-architecture Codex and Claude downloads; publish both once the
  Actions budget is raised, or document the server-built path for each.
- Confirm which digest `sbx inspect` reports for a multi-architecture image
  on Apple Silicon and pin accordingly; the verifier's allowed-image set
  must include the stock template for that architecture as well.

Progress (Track C, 2026-09-15, branch `codex/wld-track-c`): the arm64 Codex
bundle and Claude executable checksums are verified against the publishers'
own checksum files and pinned in `chat/internal/release`; the Dockerfile
builds by `TARGETARCH`, the workflow builds linux/amd64 and linux/arm64 with
QEMU and prints each platform's manifest digest, and `install.sh` installs
one platform by digest on either host type. The stock template is a
multi-architecture index (amd64 `53b08fa7…`, arm64 `d353bf15…`); `sbx
inspect` reports the index digest on both host types (evidence in
`docs/warden-guest-image-plan.md`, "Two architectures"), so the stock pin is
unchanged and the verifier's allowed set comes from
`release.StockTemplateDigests(GOARCH)` (index only on amd64; index plus the
arm64 manifest on arm64). Still open: build and publish once the Actions
budget is raised, fill `release.GuestImages`, and the one-command
confirmation probe on a Mac with a responsive daemon.

### 6. Google Docs shared client

- Create the Desktop-type client in the Monaddle project; submit the Docs
  and Drive metadata scopes for verification with the published privacy
  page at `https://apps.monaddle.com/privacy`. Until verification completes,
  local installs see Google's unverified-app interstitial and seven-day
  refresh tokens; the plan records the date verification is granted. Start
  this on day one; Google review takes weeks.
- Policy broker: a built-in Google client entry (ID and non-confidential
  secret) used when no `--google-config` is given; the loopback redirect
  port is the chat listen port. The existing `Configure` loopback rule
  already admits it.
- Connection state stays in `google.sqlite`, now keyed by principal;
  disconnect and revoke behave as today.

Progress (Track D), 2026-09-15, branch `codex/wld-track-d`:

- Done. `NewGoogleConnectionWithClient` (`chat/internal/policy/sharing.go`)
  takes `GoogleClientOptions{ConfigFile, ChatListen, BuiltinClientID,
  BuiltinClientSecret}`: the operator file always wins; otherwise, when the
  release constant is non-empty, the connection is configured with the
  built-in client and the redirect
  `http://127.0.0.1:<chat port>/oauth/google_docs/callback` (port taken
  from `--chat-listen`, default `127.0.0.1:18780`, new flag on
  `warden-policy`); with neither, `Configured()` is false. `Configure`'s
  loopback rule admits the redirect unchanged. The callback reaches the
  policy service because `warden-chat` serves `/oauth/google_docs/callback`
  itself (`chat/internal/chats/http.go`) and forwards it as the sharing
  `callback` operation over the control socket; Google redirects the
  browser straight to the chat port, so the edge is not involved. The chat
  handler requires `Host` to equal its listen address, so Track A must keep
  `chat.listen` on `127.0.0.1` (not `localhost` or `::1`). Principal
  keying: `requests`, `repositories`, `pull_requests` (sharing.sqlite) and
  `credentials` (google.sqlite) gain `principal TEXT NOT NULL DEFAULT
  'owner'` through `ensureTextColumns`; existing rows back-fill to `owner`,
  operations record `data["principal"]` or `owner`, lookups do not filter.
- Verified: built-in client redirect and redaction, unconfigured branch,
  operator-file override, invalid listen addresses, migration back-fill from
  the old schemas (`googleclient_test.go`, `principal_test.go`).
- Remaining: register the Desktop client and paste its ID and secret into
  `release.GoogleDocsClientID` / `GoogleDocsClientSecret`; submit the scopes
  for verification; Track A maps `chat.listen` to `--chat-listen` and hides
  the Google section on `configured:false`; live consent and document read
  once the client exists.

### 7. Releases

- GitHub release workflow producing darwin/arm64 and linux/amd64 binaries
  and a multi-architecture container image, each with digests, plus the
  guest images. Version and control-protocol handshake between chat, runner
  and policy so mismatched components fail with a clear message.
- OVH moves to the released image and `warden.json` in its own release,
  verified with the usual root 200, signed-out 401, edge and SBX active,
  with rollback to the previous release directory.

Progress (release track), 2026-09-16, branch `codex/wld-release` from
54fae85:

- Built. One build revision (`release.Revision`, linked in with
  `-ldflags "-X warden/chat/internal/release.Revision=<tag>"`, default
  `development`) and one protocol number (`release.Protocol` = 2; the sandbox
  worker protocol aliases it) shared by all five binaries, each printing
  `<name> <revision> protocol=<n>` with `--version` (`warden-chat` and
  `warden-edge` gained the flag). `chat/internal/handshake`: `warden-chat`
  asks the runner (worker `health`) and the policy service (new control
  operation `version`) at startup, refuses to start when a protocol differs
  naming both revisions, warns for a different revision on the same
  protocol, and warns (does not refuse) for a peer that is down after 30 s
  or predates the handshake, so one-container-at-a-time updates keep
  working. `warden start` prints every binary's version line and refuses a
  set with mismatched protocol numbers; older binaries without the number
  are reported and tolerated. The launcher, `warden-chat` and
  `warden-policy` find `web/`, `vendor/` and `config/` beside `bin/` in an
  unpacked release tarball. `.github/workflows/release.yml` (tag `v*` or
  manual; actions pinned by commit SHA): frontend via corepack pnpm
  11.27.0, gofmt/vet/race tests, the five binaries for linux/amd64,
  linux/arm64 and darwin/arm64 (`CGO_ENABLED=0`), one tarball per target
  (`warden-<tag>-<os>-<arch>/{bin,web,config,vendor}`), `SHA256SUMS`, a
  GitHub release (tags only), and the multi-architecture server image
  `ghcr.io/monaddle-too/warden:<tag>` from `deploy/chat/Dockerfile`
  (now `TARGETARCH`-aware, base pinned by digest, build fails on
  differing binary revisions) with the index and per-platform digests in
  the run summary. `deploy/chat/compose.yaml` passes
  `--config /etc/warden/warden.json` (host path `WARDEN_CONFIG_HOST`,
  default `/opt/warden/current/deploy/chat/warden.json`) and mounts every
  host directory at its own path inside the containers;
  `compose.legacy.yaml` is the previous flag-configured file for rollback;
  the cutover checklist and rollback are in `deploy/chat/README.md`.
- Verified. `gofmt`, `go vet ./...`, `go test -race ./...` green;
  cross-compilation of all five binaries for the three targets from this
  Mac with the revision ldflag, each printing its version line;
  `warden-chat` against a fake runner on protocol 3 exits with the refusal
  naming both revisions; `warden-policy selfcheck` from a release layout
  finds `vendor/` and `config/` beside `bin/`; new tests in each command
  load `deploy/chat/warden.example.json` and assert field by field the
  values the legacy OVH command lines produced (no value in the example
  needed changing; the mapping differences were the container paths, which
  the new mounts make identical to the host, `WARDEN_GITHUB_APP_BROKER`
  having to equal `providers.github.brokerFile`, and the `.env` guest image
  and spare overrides moving into the file). Docker is not installed here,
  so the Dockerfile and workflow were checked by inspection and YAML parse
  only.
- Pending the owner: raise the Actions budget and push a tag `v*` (or
  dispatch the workflow) to publish the first release and image; then the
  OVH cutover per the README checklist (image, release directory,
  `warden.json`, `.env`, `up -d`, the four checks, rollback by switching
  `/opt/warden/current` back and `compose.legacy.yaml`). Nothing on OVH
  was touched.

### 8. Documentation

- `docs/warden-local-install.md`: supported hosts (Apple Silicon Mac,
  x86_64 Linux with KVM), install, logins, previews, limits.
- Correct `docs/warden-chat.md` (preview description, launcher) and point
  `README.md`'s standalone-chats section at the install guide.
- `deploy/chat/README.md` keeps the OVH specifics and gains the config
  mapping.

Progress (docs track), 2026-09-16, branch `codex/wld-docs`:

- Rewritten: `docs/warden-local-install.md` as the operator guide, from the
  launcher code at 54fae85 and the live macOS run above: supported hosts
  (Mac verified; Linux/KVM marked unverified), prerequisites, getting a
  release (tarball or source build of the five binaries), what `warden
  install` does and prints step by step (state root, namespace and macOS
  keychain link, daemon and settings, the SBX sign-in stop with the exact
  command, host checks, runtimes, stock template, `warden.json`),
  `doctor`'s PASS/FAIL lines, the three logins with what each accepts,
  `start` and `open` through the edge, the first chat, previews (approval
  card, `*.localhost` URL, 303/403/410, the raw loopback publication),
  stopping, the state layout, reset, limits, and a troubleshooting section
  built from the seven live failures.
- Corrected: `docs/warden-chat.md` "Local startup" (retired Python
  launcher, `preview_attach` iframe, hardcoded worker caps) replaced by a
  "Local mode" summary pointing at the guide; the Behavior bullets now say
  the caps come from `warden.json` and previews are served by the edge on a
  separate `*.localhost` (or public) origin in a new tab after an owner
  approval. `README.md` points at the guide (top of file and "Standalone
  chats"). `scripts/warden-chat`'s deprecation note records the Mac
  verification. `deploy/chat/README.md`: the config mapping gains the
  `--github-auth-file` and `--chat-listen` rows the merge added; the rest
  of its statements matched the code and were left alone (the cutover
  content is another track's).
- Not confirmable from the code and marked so in the guide: the release
  tarball's top-level `web/`, `config/`, `vendor/` layout is not one
  `warden start` or `warden-chat` searches at this commit (they look
  beside the binaries or in a source checkout), so the guide names the
  `paths.*` fields and flags; the Linux/KVM path; how long a first run's
  runtime copy takes; sbx's daemon stop command (Warden only runs `daemon
  start`, `status` and `restart`).

## Steps and parallel tracks

- [x] 0 Settle the `warden.json` schema (appendix) and the principal field.
      Done 2026-09-15: `chat/internal/config` (schema, defaults, derived
      paths, mode validation, tests) and `chat/internal/release` (pinned
      versions, checksums, guest images, public OAuth client IDs).
- [x] 1 Defaults, detection, optional file, principal keying; OVH-identical
      defaults; Go race tests, vet, frontend build. Done (Tracks A and D,
      verified live on the Mac 2026-09-15/16; OVH parity is asserted by the
      cmd config tests and the compose/example equality tests).
- [x] 2 Loopback previews end to end on the Mac, with the verification list
      above (2026-09-15/16, including restart and resume). Code and tests done (Track A); the live Mac run waits for the
      Track B launcher.
- [x] 3 Installer, doctor, logins and launchers on the Mac (2026-09-15).
      The fresh x86_64 Linux VM with KVM remains unverified.
- [x] 4 GitHub OAuth App registered (client ID Ov23lijlKrGFnq0DclLn, owner
      account, device flow); user-token provider verified locally on
      2026-09-16: device-flow login, 15 repositories listed, a granted
      private repository cloned through the gateway, an ungranted one
      refused with 403. Push review and PR creation through the user token
      remain unexercised live.
- [ ] 5 Two-architecture guest image; digest behaviour on Apple Silicon
      confirmed.
- [x] 6 Google Desktop client registered and shipped as the built-in client;
      local consent (loopback redirect on the chat port) and a granted
      document read through the gateway verified 2026-09-16, an ungranted
      document refused with 403. Scope verification with Google is NOT yet
      submitted: tokens expire after seven days while the app is in Testing.
- [x] 7 Release workflow; OVH switched to the released image and config (v0.1.0-alpha.8, 2026-09-17).
      First release published 2026-09-16: `v0.1.0-alpha.1` (pre-release) at
      https://github.com/monaddle-too/warden/releases/tag/v0.1.0-alpha.1,
      built locally with `scripts/release.sh --publish` from 30ab65d after
      merging main; main fast-forwarded to it. The OVH cutover to this
      release is still pending.
      Workflow, handshake, tarball layout and the config-driven Compose
      file done (see the section's progress note); the first workflow run
      and the OVH switch wait for the owner.
- [x] 8 Documentation. Done 2026-09-16 (docs track); see the section's
      progress note.

Four tracks after step 0. Track A: steps 1 then 2, the critical path, one
sequence because they touch the same binaries. Track B: step 3, new code
with no overlap. Track C: step 5, no code dependency; the Apple Silicon
digest probe and the aarch64 SHAs should start immediately. Track D: the
external registrations in steps 4 and 6 (GitHub OAuth App, Google client and
verification) start on day one; their code wiring waits for step 0. Steps 7
and 8 last. Step 2 is the first user-visible milestone: the original
chat → SBX → preview flow working on a laptop again.

## Out of scope

Multi-user local installs; server mode as described in decision 9
(successor plan); bring-your-own Google clients; a hosted GitHub token
broker (considered and rejected in favour of decision 6); Figma and other
provider breadth; Panta; Windows hosts; Linux hosts without KVM.

## Appendix: `warden.json`

Optional in local mode (written by the installer), explicit in server mode.
No secrets: every credential is a path to a 0600 file, and the built-in
OAuth clients live in the binary. Only `paths.state` is required; everything
else has a computed default. Mode fields gate validation: `previews.mode:
public` requires a dotted suffix and `auth.mode: google`; `loopback` requires
suffix `localhost`; `auth.mode: google` requires the `auth.google` block; a
missing `providers.github` hides the repository UI instead of failing.

```jsonc
{
  "version": 1,
  "paths": {
    "state": "…",                        // required; everything else derives
    "webAssets": "…",
    "githubCatalog": "…",
    "sandboxPolicyTemplate": "…"
  },
  "sbx": {
    "executable": "…",
    "privateHome": "<state>/sbx",
    "guestImage": "…",
    "guestImageDigest": "sha256:…",
    "inspectionCertMaxAgeDays": 365
  },
  "runtimes": { "codex": "…", "claude": "…" },
  "sandboxes": {
    "memoryMB": 1536, "maxRunning": 2, "warmSpares": 1,
    "stopAfterIdleMinutes": 15, "keepStopped": 32
  },
  "chat": { "listen": "127.0.0.1:18780" },
  "previews": { "mode": "loopback", "hostSuffix": "localhost", "edgeListen": "127.0.0.1:18781" },
  "auth": {
    "mode": "owner",
    "publicURL": "http://127.0.0.1:18781",
    "google": { "signInClientID": "…", "owners": ["…"], "demoDomains": ["…"], "signInLedger": "<state>/edge/logins.json" }
  },
  "providers": {
    "codex":  { "authFile": "<state>/provider/auth.json" },
    "claude": { "authFile": "<state>/provider/claude.json" },
    "google": { "docsClient": "builtin" },                 // or a path to an operator client file
    "github": { "authFile": "<state>/provider/github.json" } // local: user token
    // server (App): { "appID": 0, "appSlug": "…", "installationOwner": "…", "brokerFile": "…" }
  }
}
```

| Section / field | Meaning | Read by |
|---|---|---|
| `paths.state` | The one private directory holding all Warden data. Every other path, socket and the SBX private home default under it. | all |
| `paths.webAssets` | The built chat UI served to the browser. | chat |
| `paths.githubCatalog` | The GitHub REST catalog files the broker uses to recognise and approve GitHub calls (today `vendor/`). | policy |
| `paths.sandboxPolicyTemplate` | The starting network and operation policy copied into every new sandbox. | policy |
| `sbx.executable` | The `sbx` command-line tool. Detected. | policy, runner |
| `sbx.privateHome` | The private HOME/XDG directory set that makes SBX see only Warden's sandboxes. Created by the installer. | policy, runner |
| `sbx.guestImage` | Which guest image new sandboxes are created from. | runner |
| `sbx.guestImageDigest` | The exact checksum that image must have; the verifier refuses anything else. | policy |
| `sbx.inspectionCertMaxAgeDays` | How old the certificate Warden uses to inspect sandbox HTTPS traffic may get before it is regenerated at startup (today the gateway CA max age). | policy |
| `runtimes.codex` | The pinned Codex CLI bundle for the guest architecture. | runner |
| `runtimes.claude` | The pinned Claude Code executable for the guest. | runner |
| `sandboxes.memoryMB` | RAM per new sandbox. | runner |
| `sandboxes.maxRunning` | How many sandboxes may run at once. | runner |
| `sandboxes.warmSpares` | Booted empty sandboxes kept ready so a new chat starts fast. | runner |
| `sandboxes.stopAfterIdleMinutes` | Minutes without user activity before a running sandbox stops; files are kept. | runner |
| `sandboxes.keepStopped` | Stopped sandboxes kept on disk before the oldest are deleted. | runner |
| `sandboxes.egress` | What a sandbox may reach through its gateway besides the brokered providers: `restricted` (the template's destination list; default) or `open` (any public HTTP/HTTPS host). Credentials are injected only for approved requests in both modes; in `open`, a brokered host without a grant is reached anonymously instead of refused. The Admin console can switch it at runtime; that choice persists in the policy state and overrides this value. | policy |
| `chat.listen` | Loopback address of the chat API and UI; the edge sits in front. | chat, edge |
| `previews.mode` | `loopback`: served on this machine over plain HTTP. `public`: through a real domain with TLS. | chat, runner, edge |
| `previews.hostSuffix` | Hostname tail each preview gets: `localhost` locally, `preview.monaddle.com` on OVH. | chat, edge |
| `previews.edgeListen` | Where the edge listens: a loopback port locally, the Docker bridge address Caddy forwards to on OVH. | edge |
| `auth.mode` | `owner`: one person, authenticated by the launcher's capability. `google`: Google sign-in with an allowlist. | edge, chat |
| `auth.publicURL` | The exact URL browsers use to reach Warden; other origins are rejected. | edge, chat |
| `auth.google.signInClientID` | The Google OAuth client used only for sign-in; not the Docs client. | edge |
| `auth.google.owners` | Emails with owner rights: connect accounts, admin console. | edge |
| `auth.google.demoDomains` | Email domains admitted with the demo role. | edge |
| `auth.google.signInLedger` | Where the edge records who signed in. | edge |
| `providers.codex.authFile` | The ChatGPT login Codex uses; read, never copied into a sandbox. | policy |
| `providers.claude.authFile` | The `claude setup-token` value. | policy |
| `providers.google.docsClient` | `builtin` for the shared verified Warden client, or a path to an operator client file. | policy |
| `providers.github.authFile` | Local mode: the user OAuth token from `warden login github`. | policy |
| `providers.github.appID` / `appSlug` / `installationOwner` / `brokerFile` | Server mode only: the GitHub App and how to run its token broker. | policy, chat UI |

Left as flags on purpose: the runner's remote-worker TLS options (unused in
both shapes) and the old Panta document-route origin and key (until that
route is revived or removed). `--manage-network` is dropped; it is always on.

## Progress

- 2026-09-15: inventory and plan written. Owner decisions: shared verified
  Google Desktop client (decision 5, this option only); GitHub in local mode
  by a user OAuth token obtained like the GitHub CLI does (decision 6), a
  hosted broker rejected; local mode autoconfigured with the file optional
  (decision 2); connections keyed by principal now because server mode will
  be a shared non-demo environment (decisions 8 and 9).
- 2026-09-15: step 0 done (config and release packages). Tracks A–D started
  in parallel on branches `codex/wld-track-a` … `codex/wld-track-d` from this
  branch; each records its own progress below its section when merged.
- 2026-09-15: all four tracks merged into `codex/warden-local-deployments`
  (C `a97e653..8ae4874`, D `1b51acc..d3e60d6`, A `98268ef..0f89e96`,
  B `0f13a76..2a71154`). Merge reconciliation: the policy service selects
  the GitHub source (user token or App broker) and the Google client
  (operator file or built-in) from the resolved settings, so
  `providers.github.authFile` and `chat.listen` map to `--github-auth-file`
  and `--chat-listen`; sharing status reports `configured` beside
  `connected` for both GitHub kinds; the launcher's GitHub login stub is
  replaced by `chat/internal/login`. Verified on the merged tree: `gofmt`,
  `go vet ./...`, `go test -race ./...` (all packages), frontend install,
  build and tests. Not yet done: a live `warden install` and loopback
  preview run on this Mac (local sandboxd unresponsive during this work),
  the Linux/KVM VM run, the guest image publication (Actions budget), and
  the two owner registrations (GitHub OAuth App with device flow; Google
  Desktop client) whose IDs go in `chat/internal/release/release.go`.
- 2026-09-15 (evening): first real macOS install and live loopback preview
  run on the owner's Mac, fixed on `codex/warden-local-deployments`
  (33a722d, 5b5043f, 23c9c30, 44ed139, c0b1dc2, 3d3df77 and the `open`
  fix). Found and fixed in order: the Application Support default state
  path pushes sbx's own Unix sockets past 104 bytes (macOS default is now
  `~/.warden`, config validates the private home); `sbx daemon status`
  exits 0 when stopped; the SBX device login must precede the host checks
  (401 otherwise) and install stops with the exact command when the
  namespace is not signed in; sbx keeps its Docker session in the login
  keychain, which the Security framework finds under `$HOME/Library/
  Keychains`, so the namespace HOME now links to it and shares the owner's
  own sbx sign-in; the launcher must pass resolved asset paths in config
  mode and warden-chat finds the UI beside the binary; the launcher must
  give the policy service and runner the namespace HOME/XDG environment
  (the first live run created sandboxes in the owner's namespace); the
  extracted Codex bundle must be world-readable because `sbx cp` preserves
  modes and the guest agent user could not start Codex ("agent worker
  disconnected" was the only symptom).
  Live acceptance on the Mac (Apple Silicon, SBX 0.42.1, private namespace,
  arm64 Codex 0.154.0 copied into the stock template): install → doctor all
  PASS → Codex login imported → `warden start` (policy, runner, chat, edge)
  → new chat through the edge at 127.0.0.1:18781 → agent wrote
  counter.html and style.css, started a detached server on 0.0.0.0:8000,
  requested `sandbox_bind_port` → owner approved in the UI → preview served
  at `http://<binding>.localhost:18781/counter.html` with the CSS applied
  and the button incrementing 0→2 → signed-out request to the preview host
  answers 303 to the app's ticket flow, an unknown binding 403 → Unpublish
  removed the loopback publication and the URL answers 410. Not yet run:
  restart/resume of a published preview, the Linux/KVM host, Claude chats
  (token not installed here), GitHub and Google logins (registrations
  pending). Note for operators: the runner's loopback publication
  (`127.0.0.1:<port>` in `sbx ls`) is reachable by any local process, as
  on OVH; the edge is the only authenticated path.
- 2026-09-16: restart and resume verified live on the Mac. The chat's
  environment had been idle-stopped overnight; one turn resumed it, the
  agent restarted its server and, because the earlier binding was revoked,
  a new approval produced binding `f1da6140…` which rendered. Then the
  whole stack was restarted with `warden start`: the binding stayed
  `approved` in the store, the runner had stopped the sandbox, and the
  preview host answered "preview unavailable; resume its sandbox and
  server" to the signed-in browser (303 to the ticket flow signed out).
  One further turn ("start the server again and call sandbox_bind_port
  for port 8000") revalidated the existing binding with no approval card,
  the same URL rendered and the button incremented again. Steps 2 and 3
  are complete for macOS; steps 7 and 8 are in progress on
  `codex/wld-release` and `codex/wld-docs`.
- 2026-09-16: `scripts/release.sh` builds the same release locally (frontend,
  five binaries for three targets with the revision linked in, tarballs,
  SHA256SUMS, unpacked-tarball selfcheck, optional `gh release create`), so
  publishing no longer depends on the GitHub Actions budget; only the server
  image still needs a Docker host (OVH builds it). Verified: a dev release
  from 17f14a6 produced the three tarballs and the darwin-arm64 one passed
  `warden-policy selfcheck` and reported `protocol=2` from `bin/`.
- 2026-09-16: arm64 guest image built on the Mac without Docker with
  `scripts/build-guest-image-in-sbx.sh` (throwaway deny-all sandbox in
  Warden's namespace, runtimes copied from the host, CA and manifest
  written, `sbx template save`); tag `warden-guest:9a25004-arm64`, digest
  `sha256:396434368ab7…c75fc`, files verified present in a probe sandbox,
  pinned in the local `warden.json`. This removes the guest image's
  dependency on the Actions budget for local installs; the GHCR-published
  images remain for shared distribution. Step 5's remaining item is only
  the publication.
  Wiring fixes found while pinning it: a template in the daemon's own store
  must be passed to `sbx create --template` by tag alone (a `name@sha256:`
  reference is treated as a registry pull and fails), so `GuestTemplate()`
  appends `@digest` only to registry references; `warden doctor` and the
  installer recognise a loaded template by the 12-character id that
  `template ls --json` prints; the runner now includes sbx's stderr in
  "SBX creation failed". Live: the warm spare is created from
  `warden-guest:9a25004-arm64` with the pinned digest, and a fresh chat's
  first turn (adopting it, no runtime copy) answered in about 7 seconds
  against roughly 30 seconds with the copy.
- 2026-09-16 (afternoon): `warden start` now starts the private sandbox
  daemon when a reboot removed it (9c58842); the guest image build script
  pins its result into `warden.json`; the release tarball is installed
  under `~/.warden/releases/<version>` with `/opt/homebrew/bin/warden`
  linked to it, and a flagless `warden start` from `$HOME` found its assets
  beside `bin/`, printed five matching versions and the daemon check, and
  served the app. The owner installed a Claude setup-token with
  `warden login claude`; a Claude chat in a fresh guest-image sandbox
  answered `CLAUDE_LOCAL_OK aarch64 agent` in about 40 s. Local mode is
  now verified for both providers. Still pending the owner: the GitHub
  OAuth App client ID and the Google Desktop client (decisions 5 and 6).
- 2026-09-16 (late): both OAuth clients shipped (1e1a15c, 8a7e4d8). Live on
  the Mac: `warden login github` device flow signed in as the owner; GitHub
  and Google both report connected; a repository and a document were granted
  to a chat's environment through the app API (the document grant needs a
  `duration`; without one the request stays pending and the chat wrapper
  reports only "Sharing unavailable", a message worth improving); the agent
  cloned the private repository and read the document's title through the
  gateway with both credentials injected there, and an ungranted document
  and an ungranted repository were both refused with 403. Remaining owner
  action: submit the Docs scope for Google verification.
- 2026-09-16: the sbx version pin was removed everywhere (owner decision):
  the verifier's host check only requires `sbx version` to answer like sbx,
  the per-sandbox check only requires a daemon version to be present, the
  host detection and doctor accept any version, and the install record no
  longer compares it. Proof evidence IDs lost the version suffix.
- 2026-09-16: terminal client. `warden chat` (interactive, x/term raw mode,
  full-repaint viewport, single-line composer with history, inline
  approvals, slash commands) and `warden chat list|new|send [--wait]|approve`
  for scripts, all as a thin client of the chat API and event stream in
  `chat/internal/tui`; `warden start --detach`, `warden stop` and
  `warden status` for a headless stack (pid and log under the state
  directory). Agent output is sanitised before it reaches the terminal.
  Verified on the Mac: `send --wait` to the owner's Claude chat answered
  `TUI_OK`; the interactive client under a pseudo-terminal rendered the
  transcript, `/chats` and `/help`, and left the alternate screen cleanly;
  detach, status, stop and restart cycle. Tests: key decoding, editor,
  rendering (sanitisation, wrapping, approvals), frame composition and
  scrolling, follow-until-idle and submit semantics against a fake chat
  service, and the launcher subcommands against a fake endpoint.
  Native Codex remote-TUI relay and subagent nesting remain future work.
- 2026-09-16: approval popups. The launcher watches the event stream and
  surfaces each newly pending approval once (desktop notification; browser
  opened on the chat in `--popups browser`, the default when detached); the
  web client keeps `?chat=ID` through the capability hand-off so the popup
  selects the right chat; the terminal client rings the bell. Verified on
  the Mac against the detached stack: a `sandbox_bind_port` request from a
  terminal-sent message produced the log line, the notification and the
  browser popup, `warden chat approve` allowed it and the preview was
  published; a popup URL opened the named chat. Also verified: the full
  detach, status, stop, restart cycle once the owner's foreground launcher
  had been stopped, and `send --wait` with the flag after the text (bug
  fixed in 9f18c28).
- 2026-09-16 (late): terminal client polish, all verified under a
  pseudo-terminal and deployed to `$WARDEN_HOME`: scrolling by wheel,
  arrows, pages and Home/End with a position indicator; a multi-line
  composer with Alt+Enter and bracketed paste; markdown-lite replies;
  coloured diffs and collapsible tool output (Tab); a run spinner with
  elapsed time and instant redraw on resize; `/find`, `/copy` and
  `/preview N`. `warden chat` opens the most recently active chat.
  `scripts/deploy-local.sh` builds and switches `$WARDEN_HOME` (a symlink
  under `~/.warden/releases`) and restarts a background stack; the owner's
  shell exports `WARDEN_HOME` and puts its `bin` on PATH.
- 2026-09-16: released `v0.1.0-alpha.1` (GitHub pre-release, three tarballs
  and SHA256SUMS, no Actions minutes used). main is at the tagged commit
  30ab65d, which includes main's Claude message-splitting and workspace
  archive work merged in. The owner's Mac runs the tagged build from
  `$WARDEN_HOME`. Alpha caveats recorded in the plan's discussion: Apple
  Silicon only verified, no second user has followed the guide, the daemon
  is not a launchd service, Google tokens expire weekly until verification.
- 2026-09-16: the repository moved to the public
  https://github.com/monaddle-too/warden as a single-commit history (the
  private `ai-vm` keeps the full history); owner identifiers were scrubbed
  from docs and example configs and the release re-published from the
  public commit. Install, doctor and the verifier now skip an sbx setting
  the running sbx does not define (an older tester build failed on
  `ssh.agentForwardingEnabled`). SBX 0.43.0 verified live on the Mac: doctor,
  Codex chat, a published preview, and its new idle auto-stop of created
  sandboxes is transparent (`sbx exec` restarts a stopped sandbox, the
  staged runtime under `/tmp` and published ports survive; only processes
  the agent started, such as a preview server, are gone after the restart).
- 2026-09-16: `v0.1.0-alpha.2` released for the second tester machine:
  install/start restart Warden's daemon when it predates the sbx CLI (the
  0.43.0 upgrade made every sbx call fail with "cannot prompt for restart:
  stdin is not a terminal"), doctor reports the mismatch, and settings an
  sbx build does not define are skipped.
- 2026-09-16: the Admin console is available in local mode (the owner is
  the admin; the sign-in ledger section appears only with an edge that
  authenticates people). Its Connected accounts section shows the GitHub
  sign-in (login, scopes, when stored) and the Google Docs connection, and
  can disconnect either: `sharing/disconnect` on the policy service revokes
  the Google token with Google and forgets it (revoking document grants) or
  deletes the GitHub token file (dropping repository selections); the edge
  gates it owner-only. Verified live on the Mac (GitHub disconnect and
  re-login). Released as `v0.1.0-alpha.3`.
- 2026-09-16: one binary. The four services moved from `chat/cmd/warden-*`
  to `chat/internal/services/{policysvc,runnersvc,chatsvc,edgesvc}` and run
  as `warden policy|runner|serve|edge`; the launcher spawns its own
  executable, so the cross-binary version check, the `--bin-dir` flag and
  the legacy pre-`--config` flag path are gone (the socket handshake
  between services stays). Releases ship `bin/warden` alone (13 MB instead
  of 41 MB); the server image, compose files, the edge systemd unit,
  release.sh and release.yml follow. `warden uninstall` stops a background
  Warden, deletes the namespace's sandboxes, stops its daemon and removes
  `<state>` (`--keep-state`, `--yes`).
- 2026-09-16: `sandboxes.egress` (`restricted` default, `open`). Open
  mode sets every sandbox engine's egress policy to `public` at load
  (`RegistryOptions.EgressMode`, authoritative over the stored policy
  file), and the gateway reaches brokered hosts without a grant
  anonymously (guest credentials stripped, nothing injected, audited with
  `anonymous: true`) instead of refusing them. sbx's own deny-all with the
  single gateway exception is unchanged, so everything still passes the
  inspecting gateway.
- 2026-09-16: the egress mode is switchable from the Admin console
  ("Network access"): `sharing/egress` and owner-only
  `sharing/egress_set` reach `Registry.SetEgressMode`, which changes
  every live engine's policy in place (grants kept, external leases
  dropped), applies to later engines and persists `<policy>/egress.json`,
  which overrides `sandboxes.egress` at the next start.
- 2026-09-16: per-repository read categories. Sharing a repository now
  records which read categories it covers (`contents`, `issues`,
  `pull_requests`; metadata always), chosen per repository in the sharing
  dialog and defaulting to all three for new selections (rows from before
  keep code + pull requests). Issue reads (`issues/*` and `reactions/*`
  GET operations scoped to a repository) joined the operation catalog's
  known read set; before this, reading issues of a shared repository was
  refused as unsupported while pull requests worked.
- 2026-09-16: multiplayer basics for the web deployment. Sessions carry the
  Google display name; the edge forwards principal, email and name to the
  chat in headers it owns; user entries record the sender (name, then email)
  and the UI and terminal client show it; `POST /api/chats/{id}/typing`
  keeps a per-person indicator for 8 s after the last reported keystroke
  (clients report at most every 3 s), merged into the live state by
  `Engine.View` and shown as "Name is typing…" to everyone else.
- 2026-09-17: OVH cut over to `v0.1.0-alpha.8` (plan step 7 done): release
  tarball unpacked to `/opt/warden/releases/v0.1.0-alpha.8`, deploy files
  from the tagged clone, image `warden:v0.1.0-alpha.8` built on the server
  from the tarball's binary and UI, `warden.json` written from the example
  with the live guest image, owners, demo domain and App ID, `.env` updated,
  `broker.json` rewritten for the single binary and the renamed account,
  edge binary and unit replaced. All three containers and the edge report
  alpha.8; root 200, `/api/state` 401 signed out.

