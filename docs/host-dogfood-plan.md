# Dogfooding on the host: instances and the jailbreak

Status: landed on main 14216d4 (2026-09-19); started 2026-09-19 on branch `plan/host-dogfood` from
`plan/workspace-vm` ad03d67; Part A on `feat/warden-instances`, Part B on
`feat/jailbreak` (done, live-tested, awaiting merge), both integrated here.
Companion to [workspace-vm-plan.md](workspace-vm-plan.md), whose decisions
1, 2 and 7 this plan revises (see "Relation to the workspace VM plan").

## Objective

A Warden chat can develop Warden and try its build **on this Mac**: deploy
the build as a second Warden beside the one it runs in, start it, drive it
(create a chat there, send a message, read its logs), open it in the
browser, and tear it down. Two things are missing today:

1. **Several Warden versions on one machine are a hand-rolled affair.**
   Every command takes `--state DIR`, the service labels already derive
   from the directory, and `install` already picks free ports — but the
   rest lives in shell scripts and session memory: cloning a home is
   `.local/clone-warden-home.sh` (a `sed` over `warden.json` and the sbx
   wrapper), the `releases/<version>` + `release` symlink convention is
   only in `scripts/deploy-local.sh`, a re-install into a dev home is
   refused without `--upgrade`, nothing lists what is installed, and the
   web UI does not say which Warden the person is looking at. An agent
   cannot be asked to follow that recipe reliably. Part A makes instances
   and releases first-class.
2. **A sandbox has no path to the host, by design.** The sandbox is
   `--deny-network **`, the gateway refuses every non-global address
   (`policy/gateway.go`, `isGlobalIP`), the runner's `exec` op only runs
   inside the guest, and a preview binding may only point at a port the
   runner published from a sandbox (`chats/preview.go`,
   `validateAttachment`). Part B adds the **jailbreak**: an owner-enabled,
   per-workspace, local-only mode in which the agent has runner-mediated
   tools to run commands on the host, move files, and expose a host port —
   every use approved under the permission rules, in the transcript and
   in the audit log.

## Relation to the workspace VM plan

[workspace-vm-plan.md](workspace-vm-plan.md) decided on 2026-09-19 that the
dev loop runs in VMs, never on the host (decision 1), and that a host
deploy mode would exist "for dogfooding only, behind `dogfood.hostDeploy`"
(decision 7). The owner's direction on the same day: a Warden chat is going
to need to deploy and interact with Warden on the host, so that mode is
designed now, as a general jailbreak rather than a deploy-only tool. The
revision, to be recorded in that plan once the owner has read this one:

- Decision 1 stands as the **default** for every workspace: no host path.
  The jailbreak is the explicit, marked exception a local owner opts a
  workspace into.
- Decision 2 (no git hosting on the host, code leaves only through pull
  requests) is **not** a property of a jailbroken workspace: the agent can
  write anywhere the owner can. That is what jailbreak means, and it is
  recorded as known security issue 3.
- Decision 7 is replaced by Part B below: the config key is
  `dogfood.jailbreak`, the tools are `host_*`, and "into a second Warden
  home with scoped keys" becomes "into an instance" (Part A); the keys are
  not scoped in the first cut (known issue 3 says why that is acceptable).
- The VM's `vm_run` / `vm_put` / `vm_get` / `vm_expose` tools and the
  jailbreak's `host_run` / `host_put` / `host_get` / `host_expose` are the
  same shape on purpose: one runner op family (`exec.target` = sandbox, vm,
  host), one transcript card, one permission-rule syntax, one audit entry
  type. Whichever lands first defines the shape; the other reuses it.

## Part A — instances and releases

### What the owner sees

- **An instance is a state directory with a name.** The default instance
  is `~/.warden` (name `default`); a named one is `~/.warden-<name>`
  (matching `serviceNames` and the clone script, and keeping the sbx
  socket paths short). Every command accepts `--instance NAME` /
  `WARDEN_INSTANCE=NAME` as the way to say `--state ~/.warden-NAME`;
  `--state` stays for arbitrary directories.
- `warden instance list` — name, state directory, release version running
  from, chat and edge ports, service status (running / stopped / not
  registered), sandboxes running, whether it is a dev instance.
- `warden instance create NAME [--dev] [--from default]` — `warden install`
  into `~/.warden-NAME` with ports picked free (as today), plus, with
  `--from`, the provider sign-ins, GitHub token, runtimes and guest image
  pin **copied** from the source instance so no device flow is needed.
  `--dev` marks the record so later installs of other builds into it are
  accepted without `--upgrade` (its pins are expected to move).
- `warden instance rm NAME [--yes]` — stop the service and the menu item,
  stop its sandboxes, unregister the launchd agents, delete the directory.
  Refuses the default instance.
- `warden release install TARBALL|DIR [--instance NAME] [--restart]` —
  the Go version of `scripts/deploy-local.sh`: unpack under
  `<state>/releases/<version>/`, repoint `<state>/release`, run `install`
  into the instance (`--upgrade` implied for a dev instance), restart the
  service if it runs. `warden release list` and `warden release use
  VERSION` switch the link back. `scripts/deploy-local.sh` becomes a
  wrapper that builds and calls this.
- **The UI says which Warden this is.** A non-default instance shows its
  name and version in the sidebar header and the browser title ("Warden ·
  dev v0.1.0-alpha.13-…"); the menu bar item's title carries the name (it
  already carries the state). The default instance shows nothing new.
- `warden doctor` checks each instance it knows about, not only the one
  it was pointed at, and lists port and label collisions.

### What happens

- `cmd/warden/state.go`: `resolveState` learns `--instance` and
  `WARDEN_INSTANCE`; `instanceName(state)` is the inverse (`default`, or
  the basename with `.warden-` stripped). `installRecord` gains `Name` and
  `Dev bool`; `checkRecord` accepts pin drift when `Dev`.
- `cmd/warden/instance.go` (new): `list` reads every `~/.warden*/install.json`
  plus the ones recorded in `~/.warden/instances.json` (created elsewhere
  with `--state`), `create` = the install path with the copy step
  (`cp -Rc` on APFS, plain copy elsewhere; sockets and locks skipped, the
  list of what the clone script deletes), `rm` = `uninstall` for a named
  instance. The `warden.json` of a created instance is written by
  `install`, never rewritten by `sed`.
- `cmd/warden/release.go` (new): the `releases/` layout in Go
  (`Release` = version, path, installed at; `current` = the `release`
  symlink). The launcher already finds `web/`, `vendor/`, `config/` beside
  `bin/`, so nothing changes in `start.go`.
- **SBX namespace.** First cut: each instance keeps its own namespace and
  daemon, as the clone script does today. The known cost is two daemons
  sharing one Docker Hub credential through the keychain link
  (`docs/redelivered-message-turn-plan.md` records the refresh-lock
  failures on `~/.warden-p20`); `warden instance list` shows which
  daemons run and `doctor` names the contention. Second cut, if the
  contention bites: `sbx.privateHome` of a created instance points at the
  default instance's namespace and the runtime names carry the instance
  (`wc-<name>-…`), so one daemon, one sign-in, one image cache serve every
  instance; the runner's inventory filter already keys on the prefix.
- **Provider sign-ins** are copied, not shared: two processes refreshing
  one `auth.json` race. `warden instance create --from` copies; `warden
  login --instance NAME` re-signs one instance.
- The web client reads `agentOptions.instance {name, version}` from `GET
  state` (the chat service knows its state path and `revision`).

### Tests

`cmd/warden`: instance name round trips; `create --from` copies the
provider files and skips sockets/locks; `rm` refuses `default`; `release
install` unpacks, links, implies `--upgrade` on a dev record; `list` output
with two fake instances. Live: `warden instance create dev --from default
--dev`, `warden release install dist/release/…tar.gz --instance dev
--restart`, a chat on the dev instance through `warden chat send --instance
dev --wait`, then `rm`.

## Part B — the jailbreak

### What the owner sees

- Nothing, unless `warden.json` has `dogfood: {"jailbreak": true}` **and**
  the install is local (`Engine.LocalMode`: owner auth, not Kubernetes).
  `warden start --jailbreak` sets it for that run. On a server or GKE the
  key is refused at config validation ("dogfood.jailbreak needs auth.mode
  owner and a local runtime").
- With it on: the New chat form gains **Host access** (off by default,
  owner only) beside Size and Network, with the text "The agent can run
  commands on this Mac as you. Every command is shown here and asks for
  approval unless a rule allows it." Also `POST chats {jailbreak: true}`
  and `warden chat new --jailbreak`. The workspace panel shows a red
  **JAILBROKEN** mark on such a workspace, in the sidebar entry and in the
  chat header; the mark is a Warden-drawn item (known issue 1 applies to
  everything else). The panel's toggle can turn it off (immediately: the
  tools disappear from the next `tools/list`) or on (owner only, logged).
- Tools the agent gets in a jailbroken workspace, on the `warden` MCP
  server:
  - `host_run {command, cwd?, timeout?}` — a shell command on the host, as
    the owner's user, through their login shell (`$SHELL -lc`, so
    Homebrew's `go`, `pnpm`, `sbx` resolve as they do in a terminal; the
    runner runs under launchd with a bare environment). Output capped and
    tailed as the sandbox `exec` op does; timeout default 10 minutes, max
    an hour; Stop kills the process group.
  - `host_put {from, to, replace?}` / `host_get {from, to}` — a file or directory
    between the sandbox and the host (`sbx cp` under the runner),
    size-capped, `to` must be absolute. (`sync_host_directory` remains for
    the bidirectional directory sync.)
  - `host_expose {port, name}` — an edge preview binding whose upstream
    is `127.0.0.1:<port>` on the host, so the owner opens the dev
    instance from the outer app's preview list and the agent can read it
    through `WebFetch` as it does any preview.
  - `host_status {}` — the host's Warden instances (`warden instance list`
    as JSON), so the agent does not have to parse text.
- Every call is a transcript card: `host_run` as a Bash-style card marked
  HOST (command, exit status, output tail), `host_put`/`host_get` as file
  cards, `host_expose` as a preview card. Permission rules apply exactly as
  to any MCP tool: `mcp__warden__host_run(*)` under **ask** prompts each
  time (the default for a fresh jailbroken workspace, whatever the chat's
  mode); an allow rule such as `mcp__warden__host_run(warden *)` or
  `mcp__warden__host_run(~/.warden-dev/release/bin/warden *)` lets the
  dogfood loop run unattended. **auto** mode does not cover `host_*`
  unless a rule names it.
- The dogfood loop this enables, from one chat:
  1. `request_repository_access` for the Warden repo, work on a branch in
     the sandbox as today.
  2. `host_put` the checkout (or `sync_host_directory` it) to
     `~/.warden-dev/src`, `host_run scripts/release.sh --skip-tests` there
     (the Mac has the toolchains; the guest image does not need them).
  3. `host_run warden release install dist/release/…tar.gz --instance dev
     --restart` (creating the instance first with `warden instance create
     dev --from default --dev` if `host_status` does not list it).
  4. `host_expose 18790 dev-warden` so the owner opens the dev Warden;
     `host_run warden chat new --instance dev …` / `chat send --wait` to
     drive it; `host_run tail -n 200 ~/.warden-dev/warden.log` to read.
  5. `request_pull_request` with the change, as today.

### What happens

- `config`: `Dogfood struct { Jailbreak bool }` on `Config`, validated
  against `Auth.Mode == owner && RuntimeKind() != kubernetes`.
  `Engine.Jailbreak` is set beside `Engine.LocalMode` in
  `services/chatsvc/main.go`; `AgentOptions.Jailbreak` advertises it to
  the clients.
- `chats`: `Environment.Jailbroken bool`, set from `POST chats` /
  `PUT environments/{id}/jailbreak` (owner only, refused unless
  `Engine.Jailbreak`). `grantTools` gains `hostTools()` appended only when
  the workspace is jailbroken; a `host_*` call on a workspace that is not
  is refused at the validation point as `request_host_directory` is
  outside LocalMode. Each `host_*` call is a `Tool` entry with
  `Target: "host"` so the cards render the mark; `host_expose` reuses
  `requestPort`/`bindPort` with a host upstream.
- `sandbox` (the runner): a `host` op family beside `exec`:
  `host.exec {command, cwd, timeout, limit}` (`exec.Command($SHELL, "-lc",
  cmd)` in its own process group, cancelled by the request's context,
  output capped), `host.put` / `host.get` (`sbx cp` each way with a size
  cap, refusing paths under the runner's own state except `releases/`),
  `host.expose {port}` (a publication whose upstream is the host port,
  minted only for jailbroken workspaces; `validateAttachment` admits it
  because the runner reports it), `host.status`. The runner refuses the
  whole family unless `--jailbreak` was passed at start, so a chat service
  bug cannot reach it.
- `chats/preview.go` `validateAttachment` and `sandbox/preview.go`:
  a publication carries `Upstream` (`sandbox` or `host`); the reverse
  proxy does not change.
- Audit: `host.exec` (chat, principal, cwd, command, exit, bytes,
  duration), `host.file` (direction, paths, bytes), `host.expose` (port),
  `workspace.jailbreak` (on/off, actor); all through the policy service so
  they are in the hash chain, emitted by the chat service through a new
  `sharing/host_event` call.
- Stop: `turn/interrupt` also cancels in-flight `host.exec` requests for
  the chat (the runner tracks them per chat as it does `exec`).
- Nothing is scoped: `host_run` is the owner's shell. The plan does not
  pretend otherwise; see known security issue 3.

### Tests

Unit: config validation (refused off-local), tools advertised only when
`Environment.Jailbroken`, refusal of a `host_*` call otherwise, permission
rules matching `mcp__warden__host_run(warden *)`, runner `host.exec` with
a fake shell (exit codes, timeout, output cap, process-group kill on
cancel), `host.put`/`host.get` path and size refusals, `validateAttachment`
admitting a host upstream from the runner, audit entries. Live, on a
cloned home with `dogfood.jailbreak` on: a chat that runs the dogfood loop
above end to end, with the ask prompts and then an allow rule.

## Part C — versions

### What the owner asked

`warden start --version <version_string>` starts that version; `warden
status` lists the running things; `warden versions` lists the versions
available for install and run.

### What happens

- **One release store.** Releases are unpacked once, into
  `~/.warden/releases/<name>/` (the default state directory's `releases`,
  so the default instance's Part A layout is already the store), and
  every instance's `<state>/release` link points into it (`store.go`:
  `storeDir`, `availableReleases`, `resolveRelease`). `release install`
  and `release build` reuse a release already there (`--force` unpacks it
  again); `release list` shows, per release, which instances are pinned
  to it and which run it. An instance's pre-store `<state>/releases/`
  stays readable ("older copy": listed, usable by `use` and `start
  --version`); nothing new is written there. A version is a tag, a dev
  version, a bare sha prefix matching one dev release, `latest`, or a
  release-directory name.
- **`running.json`.** Every `warden start` mode (foreground, detached
  child, service) writes `<state>/running.json` — version
  (`release.Revision`), release directory, binary, pid, start time, chat
  and edge listen addresses — once the four services are up, and removes
  it on a clean stop (`running.go`). A record whose pid is gone is stale
  and reads as not running. Nothing else consults the pid file for the
  version: this is the one source of "what runs".
- **`warden start --version V [--use] [--as NAME]`** (`start.go`,
  `startVersion`). V is resolved in the store and the instance's older
  copies; the release's own `bin/warden start` is executed for the
  instance with the other flags passed through and `WARDEN_RELEASE_REEXEC`
  set so the child does not re-execute into the pinned release. Without
  `--use` the link is untouched, a trial run; `--use` repoints it first
  (and runs that release's `install --upgrade`, as `release use` does).
  An instance that runs is refused: "instance X is running <version>;
  stop it, or add --as NAME to run V beside it". `--as NAME` creates the
  instance from the current one (`instance create NAME --from <current>
  --dev`, the shared SBX namespace) when missing, links V into it and
  starts it detached unless `--foreground`. A bare `--version` is a usage
  error pointing at `warden versions`.
- **`warden status`.** With no `--instance`/`--state`: a table of every
  instance (NAME, RUNNING, PINNED, PID, CHAT, EDGE, SERVICE, UP), then the
  default instance's detail lines exactly as before, so scripts reading
  them keep working; with an instance: the detail plus a `running:` line
  that names a trial run's pinned release; `--json` for the rows. `instance
  list` gained RUNNING from the same rows (`describeInstance`).
- **`warden versions [--json]`** (`versions.go`): the GitHub releases of
  monaddle-too/warden first (public API, 10 s, one line when unreachable)
  with the tarball for this host, whether `SHA256SUMS` is attached and
  whether the store has it, then the store (VERSION, INSTALLED, PINNED
  BY, RUNNING ON, WHERE, `*` for this launcher's version). `warden release install TAG` downloads that tarball
  into the store and verifies it against `SHA256SUMS`; a release without
  the checksums is refused. Everything else is offline.

### Decisions

1. The store is the default instance's `releases/` directory rather than
   a new path: the 79 releases the owner's `~/.warden` already held became
   the store with no move, and the installer's `~/.warden/releases/` +
   `~/.warden/release` convention stays true.
2. `versions` always asks GitHub (the owner: "warden versions should
   default to listing the github releases"; the `--remote` flag of the
   first cut is gone). Offline it prints one line for that half and the
   store as usual.
3. A trial run (`--version` without `--use`) under a registered service
   is refused rather than started detached beside the unit: the service
   manager would otherwise report a stopped unit while a stranger ran on
   its ports. `--use` (the unit's program is the link) or `--foreground`.
4. `running.json` is removed only by the process that wrote it, so a
   replacement instance racing a shutting-down one never deletes the
   newer record.
5. The launcher test package runs under a temporary `HOME` (`TestMain`):
   the first run of the store tests unpacked a stub into the owner's live
   store, which is exactly the shared-state hazard the store introduces.
6. A launcher from before `running.json` (the owner's live `~/.warden`,
   every release before this branch) is still shown with its version:
   `status` reads the live pid's real executable from the process table
   (`lsof -d txt` on macOS, `/proc/<pid>/exe` on Linux; `ps` would
   report the `release` symlink, which may have moved since) and takes
   the version from the release directory it lies in (`processBinary`).
   `running.json` stays the authority whenever it exists.
7. `installWith` passes a release's `install` only the flags its `-h`
   lists (`installFlagsOf`): `v0.1.0-alpha.13` has no `--service` /
   `--menu` and refused them. What it cannot bridge: a release older than
   the instance's configuration refuses that configuration (alpha.13
   does not know `sandboxes.namePrefix`, a Part A field), so a GitHub
   release from before Part A downloads and lists fine but installs only
   into an instance whose `warden.json` it can read.

## Security stance

Part A changes nothing in the threat model: instances are what `--state`
already allowed, made legible.

Part B is a deliberate hole with a fence around it:

- **Off by default, absent from every UI and API unless the owner edits
  `warden.json`**, refused outside a local owner install, and never on a
  server or GKE.
- **Per workspace, marked** — the JAILBROKEN mark is drawn by Warden, not
  the model, and the workspace list and `warden instance list` show it.
- **Runner-mediated, never a network path.** The sandbox stays
  `--deny-network **`; the gateway still refuses loopback. The agent holds
  tools, not a shell: each call is a transcript card, an approval under
  the permission rules, and an audit entry; Stop cancels it.
- **What it does not bound**, recorded as
  [known security issue 3](known-security-issues.md): inside the fence the
  agent is the owner on the Mac. Anything in the owner's home, keychain
  access from a launchd agent, `~/.ssh`, the outer Warden's own state
  directory and provider logins, other instances, the LAN. A prompt
  injection carried into a jailbroken workspace can do what the owner can,
  subject only to the ask prompts. The invariant in the register ("the
  agent never holds a credential of the owner's and never has a path to
  the host") is suspended for that workspace, by the owner, visibly.

What would narrow it later, in order of value: a dedicated macOS user for
`host_run` (the runner would need to run the command as that user — a
`sudo` rule, or a second runner process under that account), the `pf`
anchor from known issue 2 applied to host commands' network, an allowlist
of command prefixes in `dogfood.jailbreak` itself instead of only in
permission rules.

## Steps

Candidate order, to be settled once the owner has read the plan:

1. Part A: `--instance` / `WARDEN_INSTANCE`, `instanceName`, `installRecord.Dev`
   and `Name`; `warden instance list|create|rm`; `warden release
   install|list|use`; `deploy-local.sh` as a wrapper. Unit tests, then a
   live create + deploy + chat + rm on this Mac.
2. Part A: the instance name and version in the web header, browser title
   and menu bar title; `doctor` across instances.
3. Part B: config, `Engine.Jailbreak`, `Environment.Jailbroken`, the New
   chat form option, panel toggle and marks, `warden chat new --jailbreak`.
4. Part B: runner `host.*` ops behind `--jailbreak`; `host_run`,
   `host_put`/`host_get`, `host_status` tools; cards; audit; Stop.
5. Part B: `host_expose` (publication upstream kinds, `validateAttachment`).
6. Live dogfood loop from a chat on a cloned home; known security issue 3
   entry; feature-map rows; the workspace-vm plan's decisions revised.

## Decisions taken with the owner (2026-09-19)

1. The key is `dogfood.jailbreak`; the workspace option is "Host access"
   and the mark JAILBROKEN.
2. `host_run` follows the chat's permission mode: under **auto** it runs
   unprompted ("yolo mode"), under **ask** it prompts, rules apply as to
   any MCP tool. The ask-by-default proposal above is withdrawn.
3. Instances share one SBX namespace from the first cut: a created
   instance's `sbx.privateHome` is the default instance's, one daemon and
   one Docker sign-in serve every instance, and sandbox runtime names
   carry the instance so inventories do not collide.
4. `host_put` / `host_get` reach anywhere under the owner's home except the
   outer instance's own state directory (the proposal; the owner did not
   object).
5. The CLI must run several release versions at once **and a non-release
   build**: `warden release build [CHECKOUT] --instance NAME` builds the
   checkout (`scripts/release.sh --skip-tests`) and installs the result as
   a release `v0.0.0-dev.<sha>` of that instance; `scripts/deploy-local.sh`
   becomes a wrapper over it.
6. Implement everything now and merge to main.

## Progress log

- 2026-09-19: plan opened on `plan/host-dogfood`; the code seams surveyed
  (`grantTools`/`LocalMode`, runner `exec`, the gateway's loopback refusal,
  `validateAttachment`, `serviceNames`, `install`'s free-port choice,
  `deploy-local.sh`'s `releases/` layout); no code written.
- 2026-09-19: **Part A landed on `feat/warden-instances`** (steps 1 and 2).
  `--instance NAME` / `$WARDEN_INSTANCE` on every command that takes
  `--state` (`cmd/warden/state.go`: `instanceDir`, `instanceName`,
  `stateFlags`); `installRecord.Name` / `Dev`, a dev record accepts pin
  drift without `--upgrade`, `warden install --dev`; `warden instance
  list|create|rm` (`instance.go`); `warden release list|install|use|build`
  (`release.go`) and `scripts/deploy-local.sh` as a wrapper over `release
  build`; sandbox runtime names carry the instance
  (`sandboxes.namePrefix`, `sandbox/names.go`: `wc-<name>-<hex>`,
  `Owned` filters an inventory; the default instance keeps `wc-<hex>`);
  `agentOptions.instance {name, version}` shown by the web header and
  browser title, the TUI status line and the menu bar item; `doctor`'s
  `instances:` table with port / label collisions. Two things the plan
  did not foresee: (1) an install of a release that pins no guest image
  now keeps the instance's (or the source's) loaded pin instead of the
  stock template (`localGuestImage`), without which a created instance
  would have lost the default instance's `warden-guest:db0102d-arm64`
  (the same fix `feat/install-keep-guest-image` carries, unmerged);
  (2) `instance create` reads the source's `warden.json` raw when this
  launcher's config package refuses it (`loadSourceConfig`): the owner's
  `~/.warden` was meanwhile running a build with a `vms` section this
  branch does not know. A fresh install also avoids every other
  instance's ports, running or not (`otherInstancePorts`).
  **Live test on this Mac** (the default instance never touched; its
  namespace held 30 `wc-spare-*` sandboxes before and after):
  `dist/chat/warden instance create dogfood-a --from default --dev` →
  `~/.warden-dogfood-a` with `provider/` (3 files) and `runtimes/` (8)
  cloned, `sbx.privateHome = ~/.warden/sbx`, the wrapper pointing there,
  "sbx daemon: running" (none started), "sbx login: already signed in",
  the guest image kept, chat :18782 / edge :18783, `namePrefix:
  dogfood-a`, `install.json` `name: dogfood-a, dev: true`; `warden
  release build . --instance dogfood-a` → `scripts/release.sh
  --skip-tests`, `releases/warden-v0.0.0-dev.1519a42d04e8-darwin-arm64`
  unpacked, the link repointed, the release's own `warden install
  --upgrade --service=false --menu=false --sbx /opt/homebrew/bin/sbx`
  run; `warden start --instance dogfood-a --detach` (pid 63270);
  `warden instance list` showed default (running, pid 58780), dogfood-a
  (running detached, dev yes), p20, vm; `GET state` answered
  `agentOptions.instance = {dogfood-a, v0.0.0-dev.1519a42d04e8}`;
  `warden chat new --instance dogfood-a --provider claude "instance
  smoke"` + `chat send … "reply with the single word ok" --wait` →
  `claude: ok` in 14 s; `warden-sbx ls` then listed exactly two new
  names, `wc-dogfood-a-spare-06e9…` (adopted by the chat) and
  `wc-dogfood-a-spare-f2c0…`, beside the untouched `wc-spare-*`;
  `warden stop --instance dogfood-a`, `warden instance rm dogfood-a
  --yes` → "sandboxes: 2 removed", "sbx daemon: left running (the
  namespace … is shared)", the directory gone; afterwards the default
  instance's sandbox list was identical to before, its daemon running,
  `com.monaddle.warden` and `.menu` still running. `warden doctor
  --state ~/.warden-p20` printed the `instances:` table with no
  collisions. Unit tests: `cmd/warden/instance_test.go`,
  `release_test.go`, `menu_test.go`, `sandbox/names_test.go`,
  `config/config_test.go`, `tui/tui_test.go`, `web/src/instance.test.ts`;
  `go test ./...` and the web suite green. Not done: `instance list`
  does not count sandboxes per instance (it would need the daemon
  answering on every listing); the Kubernetes driver's `validName` still
  wants lower-case names (the prefix is lower-cased for it, kube
  otherwise out of scope); `~/.warden/instances.json` for `--state`
  directories elsewhere was not added (list scans `~/.warden*`).
- 2026-09-19: **Part B landed on `feat/jailbreak`** (79ae544 … fbea345,
  unit-tested, `go test ./...` and the web build/tests green):
  - `config.Dogfood{Jailbreak}` (`dogfood.jailbreak`, refused unless owner
    auth on a local runtime), `warden start --jailbreak` passing
    `--jailbreak` (and `--host-home`) to the runner and the chat service.
  - Runner `host.*` family (`sandbox/host.go`) behind `--jailbreak`:
    `host.exec` through `$SHELL -lc` in its own process group (default
    10 min, max 1 h, output tailed, killed on timeout, on the connection
    closing and on the run's cancel), `host.put`/`host.get` (≤ 512 MiB,
    under the owner's home, never the state directory), `host.expose`
    (a publication with `Upstream: host` under the runner's loopback
    listener, no guest publication, isolation audit not applied),
    `host.status` (instances under `~/.warden*`).
  - Chat service: `Chat.Jailbroken`/`Environment.Jailbroken`, `POST chats
    {jailbreak}`, `POST|PUT environments/{id}/jailbreak`, `warden chat new
    --jailbreak`, `agentOptions.jailbreak`; the `host_*` tools appended to
    `dynamicTools` for a jailbroken workspace with a prompt line; each
    call answered off the frame loop (an hour-long command does not stall
    the session), ended by Stop, refused at once when the switch is off;
    `Tool.Target "host"`; permission rules take
    `mcp__warden__host_run(cmd *)` like Bash rules and "Allow always"
    records them (`warden` is a subcommand program).
  - Audit through `sharing/host_event` → `Registry.EmitHostEvent`: the
    install-wide chain `<state>/policy/audit/events.jsonl` (a new file;
    the per-sandbox chains stay per sandbox) plus the bound sandbox's own
    chain; `host.exec` (cwd, command, exit, bytes, duration), `host.file`
    (direction, paths, bytes), `host.expose` (port, URL),
    `workspace.jailbreak` (on, actor). Like every audit file, each
    process run starts a new segment (sequence 1, zero previous hash).
  - Web: New chat "Host access" (owner, only with `agentOptions.jailbreak`),
    red JAILBROKEN badges (sidebar, header, panel; panel Turn on/off),
    HOST cards (`HostBody`), host ports marked in the preview list. TUI:
    JAILBROKEN in the status line, HOST cards, `--jailbreak`.
  - Docs: feature-map rows, known security issue 3, a paragraph in
    `warden-local-install.md`.
  - Decision 4 held as written: the outer instance's **own state
    directory** is refused in both directions (the plan's earlier
    "except `releases/`" was not implemented; a release goes through
    `warden release install` on the host instead).
  - Tool-list refresh: Claude Code takes the MCP tool list at session
    start (`thread/start`/`thread/resume` → `tools/list`), so turning host
    access off removes the tools at the next session start; every call
    is refused immediately meanwhile (verified live, after a fix: the
    check now reads the store, not the run's snapshot).
- 2026-09-19: **live test** on a cloned home `~/.warden-jb` (chat :18830,
  `dogfood.jailbreak` on; the clone's copied `runner/managed-v2.json` had
  to go — its sandboxes belong to the other daemon — and the live
  `warden.json`'s `vms` key from the sibling VM branch had to be dropped
  for this branch's parser). A Claude chat created with `warden chat new
  --jailbreak` in auto mode ran, unprompted: `host_run` `uname -a && echo
  hello-from-host` → `Darwin … arm64 / hello-from-host`, exit 0, 12 ms;
  `host_status` → the four instances (`default` running, `jb` this,
  `p20`, `vm`) once the runner learned the owner's home (first run: the
  runner's `$HOME` is the sbx namespace under the state, so every home
  path was refused and no instance listed — fixed by `--host-home`);
  `host_put` of `/home/agent/workspace/dogfood-test.txt` to
  `~/dogfood-test-jb.txt` → 15 bytes, the file on the host reads
  `dogfood put ok`; the same put to `~/.warden-jb/dogfood-test.txt` and a
  `host_get` of `~/.warden-jb/warden.json` refused ("Warden's own state
  directory … is not reachable", decision 4); `host_get` of the host file
  back to `/home/agent/workspace/back.txt` → 15 bytes, `cat` matched;
  `host_expose 18830 "jb chat port"` → `http://<id>.localhost:18831/`,
  listed under `GET ports` with `upstream: host`, and
  `ports/{id}/proxy/` answered with the jb instance's own sign-in page
  (the upstream reached; the proxy strips the bearer as it must). Under
  **ask** mode a `host_run` became an `item/tool/requestPermission` card
  ("Allow always" offering `` `echo` host commands ``), answered with
  `warden chat approve`, then ran. A plain chat on the same instance
  listed its warden tools: no `host_*`. The audit chain at
  `~/.warden-jb/policy/audit/events.jsonl` held `workspace.jailbreak`,
  `host.exec`, `host.file` (successes and refusals with the message),
  `host.expose`, hashes chained; the sandbox's own chain held its host
  events too. In the browser: JAILBROKEN on the sidebar entry, the chat
  header and the panel's Host access section, HOST on the cards and on
  the published port ("Port 18830 on this computer"); the panel's Turn
  off removed the marks. The switch, live: a session started before the
  switch answered `host_run` after it (bug: the check read the run's
  snapshot; fixed in fbea345, unit-tested), and on the fixed build a
  fresh jailbroken chat ran `echo on-again`, was switched off with
  `PUT environments/{id}/jailbreak {"jailbreak": false}`, and its next
  turn had no `host_run` at all ("No such tool available") — the
  transcript card the tool would have made never appeared. Torn down
  afterwards (`~/.warden-jb`, the daemon, the host file); the live
  `~/.warden` was never touched.
- 2026-09-19: **merged to main** at 14216d4 (`feat/warden-instances` and
  `feat/jailbreak` integrated on `plan/host-dogfood`, main merged in twice
  without conflicts beyond both-sides additions; `go test ./...` and the
  web suite green on the merged tree). Not deployed to `~/.warden`.
  Remaining ideas, none blocking: `instance list` counting sandboxes,
  the Kubernetes driver and `namePrefix`, the tracked `chat/warden` binary
  that `go build ./cmd/warden` rewrites in place.
- 2026-09-19: **the full dogfood loop, end to end, from a chat.** An outer
  instance `dogfood` (`warden instance create dogfood --from default
  --dev`, `warden release build . --instance dogfood`, `dogfood.jailbreak`
  on, started detached) hosted a chat created with `--jailbreak` in auto
  mode. Unprompted, through `host_run`/`host_status`/`host_expose`, the
  agent created `inner` from `default`, built the mainline checkout into
  it (`warden release build … --instance inner`, 16 s with a warm build
  cache), started it, opened a chat there and got `pong` back
  (`warden chat send --instance inner … --wait`, 11 s), exposed inner's
  chat port through the outer edge (`http://<binding>.localhost:18783/`,
  a signed-in preview) and read its log. Four bugs surfaced on the way,
  each fixed with a unit test and re-deployed with `warden release build
  --instance dogfood --restart` between attempts:
  1. `host_run`'s `HOME` was the runner's SBX namespace (the wrapper's
     `HOME`/`XDG_*_HOME`), so `--from default` resolved the default
     instance under `~/.warden/sbx/home` (`hostEnv`, 860ffcc; a
     `description` argument Claude Code adds is now ignored too).
  2. `host_run` inherited the launcher's `WARDEN_CONFIG`, so `warden
     instance create` read the outer instance's config and failed
     (dropped from the host environment, da69515).
  3. That failure offered a bug-report review and waited five minutes for
     it: `isTerminal` took `/dev/null` for a terminal (now
     `term.IsTerminal`), and the chat service's worker-socket deadline was
     a flat five minutes regardless of the host command's timeout (now the
     command's timeout plus a minute; da69515).
  4. `warden start --instance inner` from the outer launcher ran the
     outer binary against inner's state; the agent noticed the log header
     disagreed with `instance list`. `start` now re-executes the
     instance's own `<state>/release/bin/warden` (0368f85), verified: the
     inner process is `…/releases/warden-v0.0.0-dev.0c4bc1ad4979…/bin/warden`.
  The default instance was never touched. `inner` removed afterwards;
  `dogfood` kept, stopped, for the next loop.
- 2026-09-19: **Part C landed on `feat/start-version`** (the release store,
  `running.json`, `start --version`, the `status` table, `warden
  versions`; unit tests in `cmd/warden/versions_test.go`,
  `start_version_test.go`, `release_test.go`; `go test ./...` green).
  **Live test on this Mac** with the launcher built from the branch
  (`dist/chat/warden`), the owner's `~/.warden` (launchd service, pid
  87311, link `a62e0c835e88`) never touched — its pid, link and
  service state identical before and after:
  - `warden status` (bare) before anything: the table showed `default`
    running `v0.0.0-dev.a62e0c835e88` (pinned the same, registered
    running, from the process table since that launcher predates
    `running.json`), `dogfood` and `p20` not running with their pins;
    then the default's detail lines — which end in `warden.json: json:
    unknown field "vms"`, this branch's config package against the
    owner's live build (the same gap Part A hit; the table is printed
    first, the exit status is 1).
  - `dist/chat/warden release build . --instance dogfood` (17.8 s): the
    tarball `warden-v0.0.0-dev.f61f7cc78ffb-darwin-arm64` unpacked to
    `~/.warden/releases/`, `~/.warden-dogfood/release` relinked there
    (its four Part A copies under `~/.warden-dogfood/releases/` left as
    "older copy" rows), the release's `install --upgrade --service=false
    --menu=false --sbx …` run into dogfood. `warden versions` listed it
    first, `PINNED BY dogfood`.
  - `start --instance dogfood --detach` → re-executed into the store's
    binary, pid 38029, `~/.warden-dogfood/running.json` written with
    version, release, binary, pid, `startedAt`, chat `127.0.0.1:18782`,
    edge `:18783`; `status` showed `dogfood v0.0.0-dev.f61f7cc78ffb …
    detached 0s`.
  - `start --instance dogfood --version 0368f857 --as dogfood-b --detach`
    (8.8 s): resolved the sha prefix to dogfood's older copy
    `v0.0.0-dev.0368f857c63c`, created `dogfood-b` from dogfood (shared
    namespace, ports 18784/18785, dev), linked the older copy into it,
    ran that release's install, and exec'd its launcher: pid 38193,
    `lsof` confirmed the process's binary under
    `~/.warden-dogfood/releases/warden-v0.0.0-dev.0368f857c63c…`.
    `status` then showed three different RUNNING values (default
    `a62e0c835e88`, dogfood `f61f7cc78ffb`, dogfood-b `0368f857c63c`);
    `versions` showed `RUNNING ON` for each; `release list --instance
    dogfood-b` said `pinned by dogfood-b; running on dogfood-b`.
  - The refusal: `start --instance dogfood-b --version f61f7cc` while it
    ran → "instance dogfood-b is running …; stop it, or add --as NAME to
    run v0.0.0-dev.f61f7cc78ffb beside it", exit 1. After `stop
    --instance dogfood-b`, the same command (no `--use`) started the new
    build as a trial: `status` showed `dogfood-b RUNNING f61f7cc78ffb
    PINNED 0368f857c63c`, the `running:` detail line named the pinned
    release and the `--use` that makes it stick, the link untouched.
  - Teardown: `stop --instance dogfood-b`, `instance rm dogfood-b --yes`
    (directory gone), `stop --instance dogfood` → `running.json` removed
    on the clean stop, `status` back to dogfood not running, pinned
    `f61f7cc78ffb`.
  - `warden versions` (then `--remote`; sandbox off, 0.37 s): the 13 GitHub
    pre-releases `v0.1.0-alpha.1` … `alpha.13`, each with a tarball for
    darwin/arm64 and `SHA256SUMS`, `installed` where the store already
    held the tag (alpha.1–7, 11, 12).
  - `release install v0.1.0-alpha.13 --instance dogfood`: downloaded the
    9 MiB tarball, `SHA256 verified`, unpacked into the store, dogfood
    relinked. Its own `install --upgrade` step failed twice, each a
    finding: first `flag provided but not defined: -service` (fixed:
    `installFlagsOf`, decision 7), then `unknown field "namePrefix"` in
    dogfood's `warden.json` (alpha.13 predates Part A; not bridgeable,
    recorded in decision 7). `release use f61f7cc --instance dogfood`
    put dogfood back (its install run again).
