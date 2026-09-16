# Warden — an AI sandbox for Apple Silicon

**Current direction:** [Warden platform plan](docs/warden-platform-plan.md). Warden will own agent chats, SBX lifecycle, granular permissions, and the agent-facing MCP server. Current work focuses only on Warden; Panta is a future application integration. The implementation description below records the existing prototype.
To run Warden's chat stack on your own machine (Apple Silicon Mac, or x86_64 Linux with KVM), see [the local install guide](docs/warden-local-install.md).

A persistent macOS development VM connected only to a trusted Linux inspection
VM, with host-side GitHub permissions and redacted audit events. The guest may
have root. The host and Linux appliance are trusted.

**Prototype under validation. Do not yet treat this as a proven hostile-agent
security boundary.** New environments restrict HTTP(S) and DNS to approved destinations, with individually approved
GitHub REST calls, with destination-pinned TLS passthrough for Apple software
updates. GitHub REST requires approval. HTTPS Git supports repository read sessions and exact, reviewed single-branch pushes. GitHub web/GraphQL/SSH and unsupported protocols remain blocked.
Read [the architecture and limitations](docs/architecture.md) and [the protected development guide](docs/protected-development.md).

A Linux kernel fault observed during validation is tracked as an unreproduced
anomaly. Development continues with the host heartbeat watchdog in place.
See [the validation record](docs/validation.md).

## Repository development

Warden supports checkout → locked dependencies → checks → review → approved push
→ draft PR. In the guest, use `warden-repo clone OWNER/REPO`, then `branch`,
`setup`, `check`, `review`, `publish`, and `pr`. The host dashboard has a Repository
workflow guide and a verified push-review panel. Credentials stay outside the guest.

See [the complete workflow and existing-guest install command](docs/repository-workflow.md)
for setup, project configuration, retries, and limits. Direct GitHub requests are
gated; hosted AI tools and other remote relays remain a separate coverage gap.

## Protected development controls

`./warden setup` checks host and live guest readiness. Use `./warden project` for
separate project states, `./warden recovery` for verified stopped-disk checkpoints,
and `./warden admin` for destination policy, revoke-all and network cutoff. The
menu includes network controls and grouped approval notifications with exact
request links. `./warden release` signs manifests; `./warden update` verifies,
installs, rolls back or uninstalls managed releases. `./warden validate` runs the
security checks. See [commands, guarantees and remaining limits](docs/protected-development.md).

## Guest access without desktop interaction

Once paired, use `./warden guest shell`, `./warden guest exec -- COMMAND`, and
`./warden guest put|get SOURCE DESTINATION` from the host. The private SSH service
runs independently of the guest screen lock, uses a dedicated key and pinned
guest identity, and leaves GitHub approvals in place. New image provisioning
includes its installer; see [installation, pairing, and limits](docs/guest-access.md).

## Start on this Mac

Requires Apple Silicon, macOS 14+, 16 GiB combined VM memory (12 macOS + 4 Linux),
Python 3.9+, Apple command-line build tools, Node/npm, and `qemu-img` for image
conversion (`brew install qemu`). Reserve at least 70 GB free plus workspace growth;
the macOS virtual disk is sparse with a 128 GiB logical capacity.

```sh
./warden build
./warden control
./warden open
./warden prepare
./warden install-mac
./warden run
```

`prepare` downloads checksum-pinned Linux/tool artifacts and the pinned Apple
restore image. `install-mac` refuses to overwrite an existing macOS disk. `run`
waits for Linux firewall/proxy readiness before booting macOS. After initial
provisioning, use only `./warden control`, `./warden open`, and `./warden run`.

The **Warden menu bar** starts with `./warden control` or `./warden run`. Use
`./warden menu` to launch it separately. Its shield icon shows the pending approval
count; the menu shows proxy protection and SIEM delivery/buffering separately.
Open the VM, review requests in the authenticated host dashboard, open the SIEM,
or find logs. It can also start an already provisioned VM. Quitting the menu bar
leaves the VMs, control plane, and SIEM shipper running.

With the updated VM launcher, closing the VM window hides it; **Show VM** restores
it. Shut down macOS from the guest's Apple menu when you want to stop the VM pair.
An already running older launcher keeps its previous close-window behavior until
the next guest restart. The menu bar can run alongside it immediately.

The guest display uses the host screen's pixel density and automatically adjusts
its resolution as the VM window resizes. Use the green window button to enter
full screen for a display sized to the host screen. Display changes to the
launcher take effect after shutting down the guest and running `./warden run` again.

The installer stages Node, GitHub CLI (`gh`), Codex CLI, Claude Code, both desktop
applications, and the signed Homebrew installer in the image, unsigned in.
Complete macOS Setup Assistant in the guest. Then open
the **guest's** Terminal and trust its proxy certificate:

```sh
sudo /usr/local/bin/warden-trust-proxy
```

The helper trusts this appliance's CA in the guest only and refuses to run on a
physical Mac. `./warden stage-tools` can stage the tools into an existing stopped
guest image. A full guest bootstrap installer is also available at
`http://10.77.0.1:8081/bootstrap.sh` for repair. Install Apple
Command Line Tools in the guest for Git/compilers using `xcode-select --install`.
The trust helper also starts Homebrew setup for the logged-in guest user at
`/opt/homebrew`. If Command Line Tools are missing, Homebrew installation retries
automatically once they are installed. Open a new Terminal afterward for PATH.

To add `gh` and Homebrew to an already running guest after `./warden prepare`
has refreshed the appliance assets, run in the **guest's** Terminal:

```sh
curl -fsS http://10.77.0.1:8081/install-guest-dev-tools.sh -o /tmp/warden-dev-tools.sh
bash /tmp/warden-dev-tools.sh
```

The helper requests the guest's sudo password and checks both artifact hashes.
Installation progress is in `/var/log/warden-devtools.log` in the guest. `gh`
stays unsigned in; the proxy's GitHub REST approval rules still apply. Homebrew
downloads through blocked GitHub/GHCR endpoints still require future policy support.

The dashboard's **Request history** and **Traffic** views use server-side cursor
paging, 25 records at a time. History filters/searches SQLite on disk; traffic
reads at most 512 KiB of the audit log per page and can include HTTP, DNS, blocked
connections, and Apple update TLS metadata. Search applies to each bounded traffic
scan; continue to older events if a scanned section has no matches. The browser
replaces each page instead of accumulating rows. History/traffic never return bodies. Explicit authenticated push and PR reviews return short-lived previews separately.

Supply a GitHub PAT or OAuth access token (such as the host gh login token) through
**Policy & credentials** in the host webapp. Tokens
are memory-only and excluded from image payloads. The guest needs no GitHub PAT.
For example, in the guest:

```sh
curl --cacert /usr/local/share-warden-ca.pem https://api.github.com/repos/openai/codex
```

The first call returns 428 and appears in the host inbox. Approve it for a limited
duration, then retry. Exact approval permits one identical request. Scoped grants
permit repeated requests to one operation and exact URL, optionally constrained
by JSON-pointer body equality. Unsupported channels return 403 and cannot be
approved. Approval cannot undo a request GitHub has already received.

## Development and artifacts

Approved provider SSE responses support [response-only streaming](docs/response-streaming.md).
Requests remain buffered for inspection and authorization.

Copy text on the host, focus the Warden VM window, and press **⌘⇧V**
(**Warden → Paste from Host**). The launcher transfers that one clipboard snapshot,
waits for the guest receiver to acknowledge it, and sends the guest **⌘V**.
Ordinary **⌘V** continues to paste the guest's own clipboard.

Unicode and multiple lines are supported (64 KiB limit). No dashboard step or
continuous clipboard sharing is involved. Transfers stay in memory and are not
logged. Switching away or typing/clicking while delivery is pending cancels the
paste. If delivery fails, the launcher reports the failure and sends no paste
keystroke. The guest application's normal paste behavior applies; a terminal may
execute pasted newlines. Treat everything deliberately pasted as visible to the AI.

Newly staged images include the receiver at login. For an existing guest, install
it once as your normal guest user, without sudo:

```sh
curl -fsS http://10.77.0.1:8081/install-guest-clipboard.sh -o /tmp/warden-clipboard-setup.sh
bash /tmp/warden-clipboard-setup.sh
```

```sh
./warden test                 # adversarial policy and HTTP security tests
./warden proxy                # Linux appliance console without macOS
./warden sync                 # update the trusted appliance from repo source
./warden siem                 # start configured SIEM delivery; see docs/siem.md
./warden status               # includes SIEM delivery counters and backlog
./scripts/proxy-exec.py systemctl status warden-proxy --no-pager
./warden package              # launcher + reproducible recipe zip
```

All state is in `.local/` (gitignored). `WARDEN_STATE` selects another absolute
directory. Policy starts from `config/policy.template.json`; UI edits are saved
to `.local/policy.json`. Mac disk and identity files are in `.local/mac/`; Linux
disk and console log are in `.local/proxy/`. Shut down the guest from its Apple
menu to stop the pair cleanly. Terminating the launcher is an emergency power-off.

* [SIEM contract](docs/siem.md) and [event JSON Schema](schemas/audit-event.schema.json)
* [Architecture, threat model, and release gaps](docs/architecture.md)
* [Validation record](docs/validation.md)

The distributable zip contains code, locks, and the launcher. It intentionally
does not contain configured VM disks, tokens, policy, logs, or CA private keys.
Prepared macOS image distribution is not implemented in this first release.

### Local app and DMG

A standalone local-preview DMG can be built entirely from cached inputs. It bundles
Python, Node, QEMU, the native apps and an offline setup window; no Homebrew or
Command Line Tools are needed on the recipient Mac. See
[local DMG packaging](docs/dmg-packaging.md) for the build command, input cache,
validation and remaining signing/first-boot limits. Persistent state is separate
from the app under `~/Library/Application Support/Warden`.

## Standalone chats

Warden now includes the chat UI and managed SBX backend extracted from Panta.
Install and run it on your own machine with the `warden` launcher, following
[the local install guide](docs/warden-local-install.md) (`warden install`,
`doctor`, `login`, `start`, `open`); [the standalone chat guide](docs/warden-chat.md)
describes what the stack does. The `scripts/warden-chat` launcher is deprecated.
[Migration progress](docs/warden-chat-migration-plan.md) records validation and the remaining platform work.
