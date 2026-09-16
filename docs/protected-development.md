# Protected development environments

The original eight-point milestone now has working controls for repository work,
restricted egress, separate project states, disk recovery, menu actions,
notifications, installation/update/rollback and repeatable validation. The current
VM uses restricted egress. Apple notarization still needs Developer ID credentials,
and the historical kernel/storage fault remains unresolved.

## Daily operation

The menu shows VM state, enforcement, pending requests and SIEM backlog. It also
provides **Revoke All Permissions**, **Disconnect Network**, and approval
notifications. Disconnect is acknowledged only after Linux applies the cutoff;
private guest SSH remains available. The cutoff survives firewall reloads and
controller failure. Reconnecting never restores revoked GitHub grants.

```sh
./warden setup                      # host and live guest readiness
./warden guest shell
./warden admin revoke-all
./warden admin network off
./warden admin network on
```

Notifications group requests by repository and operation, suppress repeats for
one minute, remember notified request IDs, and open the exact request's review.
They show suggested scope/expiration; the human chooses the actual grant. They
never approve an action. Enable/disable them from the menu; macOS notification
preferences also apply. No request bodies or credentials enter notifications.

## Where source code may leave

New environments default to `config/egress.restricted.json`. Exact hostnames and
HTTP methods are grouped into AI providers, dependency downloads, development
services and Apple updates. Other HTTP(S) destinations are denied. Dependency
registries default to GET/HEAD; AI endpoints allow the methods needed to submit
prompts. GitHub remains separately gated by repository and operation permissions.

Guest DNS also consults the host policy before resolving names. Restricted mode
permits only the exact configured hosts plus github.com and api.github.com.
Queries are canonicalized, restricted to A/AAAA and stripped of caller-controlled
extra records before forwarding to the configured resolver. DNS policy/audit
failure returns refusal. IP overrides cannot evade HTTP authority/SNI validation.
Apple's fixed TLS exceptions also require destination-policy permission.

Edit policy in the authenticated dashboard, or load a reviewed profile:

```sh
./warden admin policy restricted --file /absolute/path/egress.json
```

Saving policy revokes grants and inspected in-flight decisions. An already
established Apple TLS download can continue until its connection closes; use
Disconnect Network to stop all guest network traffic immediately at the appliance.
The older public-web mode remains an explicit opt-in through
`./warden admin policy public`. Do not use it for confidential work without
accepting arbitrary public HTTP(S)/DNS egress.

An approved AI provider can receive source code. These controls do not constrain
that provider's hosted search/connectors, prove that a GET contains no secrets,
or eliminate covert channels through an approved destination. They are
*destination controls*, not general DLP or a provider-side GitHub gate. Adding a
host expresses trust in that destination. Wildcards are deliberately unsupported.

## Separate projects and disposable environments

```sh
./warden project create feature-a --repo OWNER/REPO
./warden project run feature-a -- setup --apply
./warden project run feature-a -- run
./warden project run feature-a -- guest shell
```

A project has its own disks, policy, guest SSH key, audit, approvals and credential
configuration under `.local/projects/NAME`. It begins with fresh provisioning,
not a copy of the personal development disk. No parent GitHub credential, AI
login or SIEM token is inherited. The repository allowlist rejects requests for
other repositories, including account-wide API operations without a repository.
Supply credentials separately for each project.

This desktop currently runs one project control plane on port 18765 at a time.
Shut down the old guest and run `./warden services stop` before switching. Commands
refuse to attach a new project to a different project's active controller. Use
short names; Unix socket path-length limits are checked before creation.

Disposable tasks can use fresh projects, then `./warden project archive NAME`.
Archive requires the guest and controller stopped and retains the private state
under `archived-projects` for recovery. It does not claim secure erasure. Fresh
macOS setup still includes Apple's Setup Assistant and first guest login/trust.

## Recovery and backups

```sh
# Shut down the guest from its Apple menu first.
./warden recovery capture before-change
./warden recovery verify before-change
./warden recovery list
./warden services stop
./warden recovery restore before-change
./warden control
./warden run
./warden admin network on
```

Checkpoints capture both stopped VM disks and their hardware/EFI identity files,
with full SHA-256 checksums. APFS clone copies avoid allocating the entire logical
disk up front; checksumming still reads all logical bytes. They are confidential
local backups containing guest work and guest logins. Copy a completed checkpoint
directory to a trusted encrypted backup device for protection from host disk loss.
Local checkpoints alone are not off-device disaster recovery.

Restore verifies every file and the VM identity before replacement, stages all
files first, and journals replacement so interruption can roll back. An unfinished
journal blocks startup; use `./warden recovery recover`. Current host policy,
audit, credentials, and SSH client keys are never restored from a checkpoint.
Controller restart revokes all old grants. Restores start disconnected. Restoring
an older guest SSH server key may require explicit fingerprint pairing again.
Capture refuses a running VM; this is stopped-disk recovery, not live VM snapshots.

## Installation, verified updates and uninstall

`./warden setup --apply` checks prerequisites and performs the missing build,
prepare, image-install and controller steps. The dashboard's **Setup & recovery**
page shows readiness and exact next commands, including a live guest-tool check.
Apple terms/Setup Assistant and initial guest trust remain interactive.

```sh
./warden release build
# For a Developer ID release, once the identity/profile exist:
./warden release build --apple-identity 'Developer ID Application: NAME (TEAM)' \
  --notary-profile PROFILE
```

The release produces a zip, a component/checksum manifest and an Ed25519 signature.
The local publisher key is private in `.local/release-signing/`; only its `.pub`
file is shared. This signature is separate from Apple code signing. The optional
Apple path enables hardened runtime, submits with notarytool, staples the tickets,
and repackages. No Developer ID identity is installed on this development Mac,
so the built release is locally signed, **not Apple-notarized**.

Verify the publisher public key through a trusted channel before first install:

```sh
./warden update install --prefix "$HOME/Applications/WardenLocal" \
  --archive warden-0.1.0-arm64.zip \
  --manifest warden-0.1.0-arm64.manifest.json \
  --signature warden-0.1.0-arm64.manifest.json.sig --key publisher.pub
```

The installed command is `PREFIX/bin/warden`. Managed installations separate
persistent `state` from immutable release directories. Updates verify the pinned
publisher signature and whole archive before extraction, reject archive traversal
and unexpected symlinks, verify app code signatures, and atomically switch the
release pointer. Supply downloaded artifacts with `update apply` and the same
arguments; existing installs use their pinned key. An older signed timestamp is
rejected; an intentional downgrade uses `update rollback --prefix PREFIX`.

Shut down the guest before applying or rolling back. Host services are stopped
before activation; start the new version with `PREFIX/bin/warden control` and
`run`. Failed staging leaves the previous release active. VM disks and policy are
preserved. Source checkouts are never overwritten by the managed updater.

`./warden update uninstall --prefix PREFIX` stops owned host services and moves
the entire installation to a recovery directory; it refuses an active VM. Rename
it back to its original path to recover it. This deliberately retains confidential
VM data rather than silently destroying it. No system daemon or login item is
installed on the host.

## Security validation

```sh
./warden validate
# Disruptive tests require a separately provisioned, running Linux-only appliance:
./warden validate --appliance /absolute/path/to/disposable-state
```

The harness records reports and logs under `.local/validation/`. It includes
policy/HTTP/Git regressions and native checks. The optional appliance run uses a
root attacker namespace through the production firewall/proxy, stops the proxy,
loads memory while querying nftables, verifies package integrity, fills a small
isolated filesystem to test audit failure, and performs durable disk/cache-drop
checks. It refuses the primary state and an appliance containing a macOS disk.
The namespace script additionally refuses an appliance with a macOS guest lease.
Do not run source sync concurrently with disruptive tests: management restarts
can interrupt remote commands. Fixture/browser tests do not ship to the live SIEM.

See `docs/validation.md` for results and unresolved findings. Passing these tests
is not proof against hypervisor exploits or a resolution of an intermittent fault.

## Live VM resources

The dashboard and Warden menu show separate CPU and RAM readings for the macOS
and proxy VMs. Sampling runs every five seconds through the existing pinned SSH
and private proxy management channels, independently of permission processing.
No additional guest package or network listener is installed. Telemetry stays in
controller memory and is exposed only through the authenticated state API.

CPU is normalized to 0–100% of each VM's total vCPU capacity, with the vCPU count
shown on the dashboard. macOS uses the second one-second `top` sample; Linux uses
changes in `/proc/stat` over the sampling interval, treating I/O wait as idle.
RAM is an estimate of working memory versus guest-usable physical RAM: macOS
subtracts free, inactive and speculative pages; Linux subtracts `MemAvailable`.
These figures exclude reclaimable cache and are not host RSS or an exact match
for Activity Monitor's memory accounting. Values are guest-reported diagnostics,
never authorization or health evidence. Stopped/unreachable guests show
Unavailable; samples older than 20 seconds are not displayed as current values.
