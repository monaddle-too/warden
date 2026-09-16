# Bounded traffic and history pages (2026-09-09)

Added authenticated server-side pagination for approval history and HTTP/DNS/
network traffic metadata. 92 Python tests and native checks passed, including
cursor boundaries, tied database timestamps, filter continuation, partial writes,
oversized legacy records, snapshot stability under append, log replacement,
redaction, and endpoint authentication. Startup request retention no longer uses
fetchall. Historical rows no longer ride along with periodic dashboard state.

A synthetic browser instance verified 25 rows per page, replacement on Next,
server-side status filtering, and removal of traffic DOM rows when switching to
history. On the real 10,480,963-byte log, one traffic page scanned 524,288 bytes,
returned 25 events and a 10,265-byte JSON response, with 567,393 bytes of peak
tracemalloc-tracked Python allocation during the page read (not total process RSS).

Reloaded the host controller and restored the previously authorized host gh token
without printing it. Live authenticated endpoints returned seven history rows
(2,742 bytes) and 25 traffic events (11,924 bytes). The guest/proxy were not
restarted. Controller reload revokes prior permissions by design.

# Request history dashboard (2026-09-09)

Added a Request history panel using the existing authenticated state response.
Shows the latest 100 stored approval requests, with status descriptions, creation
timestamps, IDs, expandable redacted metadata, search, status filters, and 20-row
pages. Approval is explicitly distinguished from dispatch/completion. Requests
blocked before the approval queue remain in the audit log. Disconnect clears the
history display so stale data cannot look current. Existing backend/storage and
credentials were unchanged; no controller restart was needed.

Verified the authenticated Chrome dashboard shows seven historical requests,
including both GET /user approvals and five stale repository lookups, while the
pending queue is empty. JavaScript syntax check passed. Further native UI actions
were stopped when the user switched Chrome to an unrelated task.

# Host gh OAuth credential import (2026-09-09)

Extended host credential validation to accept GitHub CLI OAuth (`gho_`) tokens
alongside PATs. Verified OAuth credentials remain subject to request approval,
are redacted, and are absent from snapshots and stored state. Full suite passed
(83 Python tests plus native paste checks). Reloaded the controller, retrieved the
host gh token through a captured subprocess pipe, and posted it directly to the
fixed loopback token endpoint with redirects and environment proxies disabled.
The token was neither displayed nor placed in a transfer file or guest. Verified
`token_configured` and opened a fresh authenticated dashboard. Controller reload
revoked prior grants; the guest must retry and obtain a fresh approval.

# Dashboard expired-session display (2026-09-09)

A gh GET /user request was queued correctly, but an older dashboard tab retained
its empty-queue display after controller restart invalidated its browser session.
Changed refresh failure handling to discard displayed queue/grant state, close any
approval dialog, show an unknown request count and proxy state, and give explicit
host `./warden open` instructions. Later successful refresh clears the connection
error. Refresh generations prevent older responses from overwriting newer state.

Verified the existing unauthenticated in-app tab now shows “Sign in to view
permission requests” instead of “Nothing waiting”. Reopened the dashboard with
`./warden open` and verified the authenticated Chrome page displays queued requests.
The actual GET /user record subsequently showed approved; no approvals or GitHub
credentials were changed by this UI repair. No controller restart was needed.

# Apple developer tools network fix (2026-09-09)

Observed guest-side TLS handshake rejection at swscan.apple.com, mesu.apple.com,
and other update endpoints during Command Line Tools installation. Added exact
Apple update TLS exceptions with fresh DNS validation, private/GitHub address
rejection, destination pinning, and mandatory metadata auditing. Cleartext package
URLs are upgraded to HTTPS with 307 responses. General forwarding and inspected
body limits remain unchanged. Apple documents the interception incompatibility:
https://support.apple.com/en-us/101555

80 Python tests plus native checks passed before deployment. New tests cover
spoofed original destinations, exact SNI matching, private/mixed/GitHub DNS,
resolver/audit failure, altered destinations, and OCSF Network Activity mapping.
Deployed through the trusted sync channel; the macOS VM stayed running. Restarted
the host controller for the new audit event after checking no GitHub token or
active grants were configured.

Live Linux loopback tests used the deployed addon and real host audit RPC with a
temporary regular proxy listener (subsequently stopped): Apple's certificate
validated without the Warden CA; Apple SNI with a GitHub CONNECT destination
received Apple's 404 with valid TLS; GitHub REST still returned 428. Downloaded
20,971,520 bytes of a real CLTools_Executables.pkg range, discarding the data, via
an HTTP-to-HTTPS redirect: HTTP 206, certificate verification success, no 16 MiB
cutoff. The update catalog was also fetched and parsed through the exception.
Initial probes had intermittent TLS failures, while subsequent certificate and
package checks succeeded; destination/audit rejections now have explicit logging.
The real guest has emitted permitted mesu.apple.com and updates.cdn-apple.com
connections. Full installer completion is pending the user's Apple license
acceptance; the dialog is open in the guest.

# Physical shortcut follow-up (2026-09-09)

The user subsequently reported that physical Command-Shift-V remained pending,
then a manual Command-V pasted twice. The earlier CUA-driven successes below did
not establish reliable physical-keyboard behavior.

Moved cancellation to NSApplication before Virtualization's local monitors, so
manual Command-V cancels the automatic paste before the guest can receive it.
Consume both halves of the host shortcut and suppress repeats. Track keyboard
modifier release from application events instead of polling the global combined
physical/synthesized modifier snapshot. The acknowledgement wait now distinguishes
waiting for shortcut release in the window subtitle.

Native regression checks cover both release orders, modifier holds, unrelated
application events, repeat suppression, manual-paste cancellation decisions, and
the fixed Command/V sequence. All 72 Python tests and native checks passed, as did
the release build. After the user shut down the guest normally, installed the
staged launcher with the VM state lock held and verified its code signature.
Both VMs restarted successfully; the guest reached its login screen.
Physical-keyboard verification is still pending guest login.

# Native paste dispatch correction (2026-09-09)

Live testing after guest login showed that the menu action delivered text and
received its acknowledgement, but the virtual keyboard typed a plain `v`.
The host shortcut did not reach the NSWindow handler at all. Fixed dispatch by
subclassing NSApplication and handling Command-Shift-V before Virtualization's
local event monitors. Replaced modifier-only flagsChanged notifications with
physical Command key-down/key-up events around V, all delivered directly to the
virtual-machine view. No host event posting or guest automation permissions were
added. The native regression check verifies the application subclass and the
physical Command/V sequence, and now runs as part of `./warden test` on macOS.
The corrected build was installed following a normal guest shutdown. After guest
login, Command-Shift-V transferred and pasted `warden-paste-check` into Terminal
without submitting it. A second shortcut invocation pasted four lines into guest
TextEdit, preserving Chinese, an emoji with skin tone and ZWJ, Arabic, an em dash,
and accented Latin text. Both transfers reported the matching delivered status.
The actual guest documents were inspected on-screen; this closes the previous
live-verification gap.

# Native Paste from Host (2026-09-09)

Removed the dashboard transfer form and added a native Warden menu action with
Command-Shift-V. The host reads its clipboard only on this action, sends one
64 KiB-bounded snapshot with a random transfer ID, and waits for an acknowledgement
from the guest receiver. The acknowledgement must match the current collected,
unexpired transfer. Fixed Cmd-V events go directly to VZVirtualMachineView;
clipboard bytes never become synthetic character events or a host command.
Window focus loss and intervening keyboard/mouse input cancel pending paste.
Ordinary Command-V remains guest-local. System-key capture is disabled so host
shortcuts stay in AppKit and to avoid the observed lost guest modifier events.

Swift release build passed, as did 72 Python tests (including SIEM tests added in
parallel). Added acknowledgement ordering/replay/cancellation tests and fixed-route
HTTP checks. Updated the guest receiver and restarted the guest normally into the
new launcher. Confirmed the native menu is present and the live dashboard has no
transfer form. The initial live test exposed native event-dispatch bugs; their correction and
successful end-to-end verification are recorded above.

# Validation record — 2026-09-09

Host: Apple Silicon, macOS 15.6 (24G84), 48 GiB RAM.

## Passed

* Swift release build, ad-hoc signing with the Virtualization entitlement, and
  `VZVirtualMachine.isSupported` on the actual host.
* Download and install macOS 15.6.1 (24G90) through Apple's restore/install APIs.
* Boot both VMs together within the 16 GiB memory allocation; macOS reaches
  Setup Assistant only after the proxy readiness gate succeeds.
* Persistent Ubuntu 24.04 ARM64 appliance boots through Apple's EFI VM APIs;
  its firewall, MITM proxy, host-only maintenance channel, and host policy
  channel run without host administrator privileges.
* **48 automated tests**, passing on both macOS/Python 3.12 and inside the Linux
  appliance. These cover operation identification, exact-request binding,
  concurrent single-use consumption, scoped body predicates, expiry/clock
  rollback, revocation/restart, audit failure, credential scrubbing, ambiguous
  HTTP/JSON rejection, host/origin checks, and real mitmproxy flow objects.
* **10 live network checks** using an attacker Linux network namespace through
  the actual production nftables/mitmproxy path: public HTTPS works; GitHub REST
  yields 428; GitHub web and GraphQL yield 403; SNI/Host mismatch is rejected;
  public bootstrap CA is available; SSH, raw Git, and host control-port access
  are blocked; stopping the proxy leaves no forwarding bypass.
* DNS queries and rejected-packet metadata reach the host JSONL audit stream.
* A live watchdog check stopped the proxy and confirmed the native host paused
  macOS after heartbeats ceased.
* The pinned Ubuntu archive snapshot resolves successfully, with installed
  Python venv, nftables, and dnsmasq versions matching snapshot candidates.
* Host control UI renders in the browser, authenticates, shows live proxy
  readiness, and displays a real intercepted `repos/get` approval request.
* Codex.app and Claude.app signatures verify before offline installation into
  the guest Data volume. Node and pinned Codex/Claude CLI files are installed
  into the image without copying any host account data.

## Not yet demonstrated

* An independent hostile-agent or hypervisor security review.
* Exhaustive classification of all present/future direct GitHub endpoints.
* Git transfer, GraphQL, GitHub website actions, arbitrary non-HTTP protocols.
* Real GitHub PAT authentication (the user has not supplied a token).
* Agent sign-in and desktop/CLI execution inside a completed macOS user session.
* Full-disk failure injection, malicious TLS parser fuzzing, sustained resource
  exhaustion, and long-duration VM suspend/resume testing.
* Signed/notarized public distribution or preconfigured macOS-image redistribution.

## Observed kernel fault — unreproduced anomaly

During the combined-VM session, Linux 6.8.0-138-generic suffered an undefined
instruction fault in `nfnetlink_rcv` while the proxy heartbeat queried nftables.
The kernel became tainted (128); subsequent nft commands faulted. Existing
kernel packet-drop logging continued, but this does not establish that all
enforcement remained correct after the kernel fault. The cause is unresolved.
Diagnostics are retained in `.local/proxy-kernel-fault.log`.

After reboot, the kernel reported no taint and the firewall/proxy started again.
The launcher now pauses macOS if the proxy heartbeat stops changing for over
20 seconds (checked every three seconds), and the proxy refuses readiness after
a kernel oops. Pausing does not undo already-forwarded traffic. These are
containment measures, not a fix for the kernel fault. Investigation was paused
at the user's request after the diagnostic runs below did not reproduce it.
Development continues with the incident tracked; no cause has been established.

Follow-up diagnostics used a separate copy of the Linux disk. Package integrity
checks passed. Repeated firewall queries, approximately 2.8 GiB of added guest
memory load, cgroup and global OOM kills, and concurrent disk copies did not
reproduce the original fault. The sampled `nfnetlink_rcv` code stayed unchanged.
The global OOM test showed ordinary OOM-killer records that were absent from the
original incident. These tests do not rule out host memory pressure or an
intermittent fault. The planned combined-VM and alternate-kernel comparisons
were not performed before the investigation was paused.

Detailed diagnostic outputs and experiment caveats are in `.local/diagnosis/`.
The bounded query/code-integrity probe is `scripts/diagnose-netfilter.py`.

The namespace checks validate the Linux enforcement path, not all aspects of a
second VM's virtual hardware. Source inspection confirms the Mac configuration
has one private NIC and no NAT, bridge, Virtio socket, or host directory share.
The build intentionally stops short of claiming this prototype is a proven
security boundary against a hostile AI.

The macOS guest still needs Setup Assistant. Complete it, then
run `sudo /usr/local/bin/warden-trust-proxy` in its Terminal. Enter the GitHub
PAT separately in the host dashboard. No guest password or real GitHub token
has been supplied by this build.

Local detailed outputs: `.local/test-results-full.log`,
`.local/test-results-linux.log`, `.local/network-test-results.log`,
`.local/proxy-packages.tsv`, `.local/macos-install.log`, and
`.local/mac/tools-staged.json`.

## Browser diagnosis and recovery — 2026-09-09

Safari in the completed macOS guest showed certificate trust warnings on ordinary
sites. Facebook attempted repeated UDP/443 (HTTP/3) traffic, then disconnected
after the intercepted TCP TLS handshake. Proxy TLS diagnostics now report client
certificate rejection in the existing SIEM `hostname` and `reason` fields.
The guest must run `sudo /usr/local/bin/warden-trust-proxy` once for this VM pair.
Browser verification after that step remains pending user input.

QUIC is now redirected locally and immediately rejected with ICMP port-unreachable,
including when Linux forwarding is disabled. HTTP responses clear Alt-Svc so
clients do not retain advertisements for unsupported alternate transports.
These changes preserve inspection and GitHub approval requirements.

During this diagnosis, Linux cached reads of `/usr/bin/bash` failed the package
checksum and execution raised SIGILL. After syncing and dropping Linux page
caches, the same file matched its package checksum and executed successfully.
An earlier failed update left zero-filled proxy source files, subsequently
restored from intact host assets. These observations do not establish a cause
or prove a connection to the older netfilter fault. The native launcher now
uses explicit `.cached` disk attachments with `.full` synchronization as a
mitigation under validation. After restarting, all installed package checksums
passed (`dpkg -V` emitted no differences), and all enforcement services started.

Updates now copy into staging, flush, compare against the shared source, validate
firewall/DNS configuration, and replace individual files atomically before
restarting services. The revision advances only after successful activation.
This guards against a bad copy but is not a transactional multi-file deployment
or a general fix for memory/storage corruption.

Local evidence is retained in `.local/browser-diagnosis/`.

Post-recovery probes returned HTTP 200 for example.com, Google, and Facebook
over both HTTP/1.1 and HTTP/2, with GitHub REST returning 428 in both protocols.
The QUIC rejection chain returned port-unreachable in 0.11 ms in an isolated
test namespace. These probes used the actual appliance CA explicitly; they do
not demonstrate that the macOS guest trust store has been configured.

## Google browser header regression — 2026-09-09

After CA trust, Safari's Google homepage request was rejected by the blanket
duplicate-header check. HTTP/2 permits split Cookie fields (RFC 9113 §8.2.3);
the proxy now joins these with `; ` before inspection, preserving ordinary
website cookies while continuing to strip them from GitHub requests. Other
duplicate headers retain the previous rejection behavior.

Early validation denials previously returned a local response without an audit
event. They now emit `proxy.error` with request ID, status, reason, hostname,
protocol and header names, excluding header values and query contents. Only
428 responses direct the user to request approval. A live duplicate-header
probe returned 403 and produced the matching host audit event.

Verified the Google homepage renders in Safari in the actual macOS guest, with
a matching HTTP 200 audit response. All 38 automated tests pass, including
HTTP/2 cookie normalization/redaction, GitHub cookie stripping, continued
duplicate Authorization rejection, and early-denial logging.

## Metadata-only retention and OCSF — 2026-09-09

Body retention was disabled at the user's request. `Redactor.body` returns only
byte count and `omitted_policy`; the host independently removes body payloads
before durable audit writes. Approval summaries are metadata-only. Tests cover
legacy proxy payload rejection, SQLite summary cleanup, and explicit migration
of historical audit chains. Existing body-bearing audit records were rewritten
with original-hash references and migration markers; no body backup was kept.

OCSF 1.6.0 export preserves native event IDs, correlation IDs and metadata in
`unmapped.warden`. The real viewer parser accepted a 4,963-event export with zero
field issues. Its largest record was 9,054 bytes, below both viewer and current
ingester record limits. The sibling application's source was not modified.
All 48 automated Warden tests passed.

The current Go ingester parser also accepted the same 4,963 records with zero
rejections in 1,000-record batches. Live post-change probes returned HTTP 200
for ordinary HTTPS and 428 for unapproved GitHub REST. Audit responses retained
only body byte counts and `omitted_policy`.
# Explicit host-to-guest clipboard transfer (2026-09-09)

Added an authenticated host text form, a 64 KiB/60-second in-memory single-use
inbox, and a guest-only Swift LaunchAgent writing plain UTF-8 text to NSPasteboard.
No shared clipboard device, host clipboard reader, guest-to-host clipboard upload,
or automatic keyboard execution was added. The existing Linux bootstrap service
exposes only a fixed inbox read, with no arbitrary RPC forwarding.

Built with the local Swift toolchain. Installed the receiver in the live macOS
guest without restarting the VM. Sent text containing multiple lines, Chinese,
Arabic, a composed emoji, combining accents, and literal shell punctuation.
The guest's `pbpaste | shasum -a 256` matched the host byte hash:
`3154a52fa1e153281963955eb163e61f88d1ba2ed4304b395d347f4e02ca5d26`.
The Python suite passed 56 tests, including new UTF-8/size checks, expiry,
cancellation/replacement, concurrent single consumption, host authentication and
origin checks, absence of text in logs/SQLite/state, fixed guest route/method
checks, cache headers, and safe bridge failure. This does not establish resistance
to every hostile-guest denial of service or exploitation of platform APIs.


## Repository workflow — 2026-09-09

Implemented HTTPS Git read sessions, verified single-branch publication, transient
host source/PR review, and the provisioned `warden-repo` guest CLI. The full local
suite passes **107 Python tests**, native paste checks, and native menu checks.
Nine repository protocol/integration tests also pass inside the running Linux
appliance with its production Git 2.43.0 and subprocess resource limits.

The integration harness uses an actual Git client, Guard, Engine, and local Git
HTTP backend. It validates clone → read approval → checkout → commit → host diff
→ exact approval → push → fetch, initial publication to an empty repository,
changed-content rejection, force-push rejection, repository scope, expired
previews, binary changes hidden in history, and removal of unreachable objects.
It substitutes DNS/upstream transport and uses temporary fixtures; no production
GitHub repository was pushed to. CLI tests exercise real npm lifecycle-script
suppression, check receipts, failed checks, stale commits, credential-bearing
remote rejection, and preservation of existing clone destinations.

The host browser was exercised against a separate fixture controller: review
showed verified object IDs and source patches, prohibited repeated push grants,
and successfully granted a local test permission. The new workflow guide was
checked. Source/PR previews are authenticated, expire, and are absent from audit,
SQLite and state/history endpoints. No fixture traffic was sent to the live SIEM.

The running host controller and Linux payload were updated without restarting
the macOS VM. The current guest's CLI installation still requires its local
install command: the physical Mac is locked, so guest UI installation was not
possible. Provisioning, offline staging and local asset serving include the CLI.

This supersedes the earlier blanket “Git transfer unsupported” restriction for
the supported subset. Binary/tag/submodule publication, GraphQL, opaque protocols,
large repository support, host-attested guest checks, and remote AI relay/egress
controls remain unimplemented. See repository-workflow.md for exact limits.
# Private guest access validation — 2026-09-09

Installed and paired the running macOS 15.6.1 guest. Verified ordinary-user
command execution, developer-tool PATH, repository doctor, an interactive PTY,
literal shell arguments, and propagation of exit code 37. A 256 KiB binary file
round-tripped exactly through SFTP using spaces, quotes, brackets, `*`, and `?`
in its filename. This exposed and fixed double escaping in the SFTP batch parser.

The live SSH daemon rejected an unknown client key, a mismatched server key, and
remote port forwarding. Its narrow workflow-install sudo command succeeded;
general `sudo -n true` failed. The installer preserves SIP-protected `/usr/local`
ownership and installs the authorized public-key file readable by the login user.
The trusted Linux relay is enabled as a persistent system service. Proxy and
firewall services remained active, and SIEM had zero pending bytes/events.

All 118 Python tests and both native check suites passed, including 11 new guest
identity/transport tests. Full tool staging into a temporary Data-volume directory
confirmed the exact installer and matching public key were included, with no
client or server private keys. The rebuilt distribution contains provisioning
code and excludes personal VM identity and credential files. No running VM disk
was mounted or replaced during validation.

Screen locking and a full cold reboot were not exercised in this validation.
Independence from the GUI comes from the system LaunchDaemon and authenticated
network channel; preboot FileVault unlock and host sleep remain availability
limits. Guest management session metadata remains local, separate from SIEM.

## Protected development milestone — 2026-09-09

The expanded suite passes **135 Python tests** plus the native paste/menu checks.
New tests cover destination/method restrictions, DNS policy, project repository
boundaries, persisted disconnect/revocation, audit ENOSPC, corrupt checkpoints,
interrupted restore rollback, independent project keys, signed manifest tampering,
archive traversal, host-only emergency APIs and exact approval links.

A freshly provisioned, separate Linux appliance passed 10 attacker-namespace
network checks through production nftables/mitmproxy, including raw Git/SSH and
host-port denial, SNI mismatch, GitHub REST approval, and fail-closed proxy stop.
Stopping its host controller also activated the appliance network cutoff. A real
1 MiB tmpfs filled to ENOSPC prevented a new authorization when the audit needed
another block. Five 64 MiB durable write/read/cache-drop rounds passed. A 30-second
probe with 1 GiB additional guest memory and four nft workers kept sampled kernel
module code stable, reported zero taint, and verified its memory contents. Package
integrity checks reported no differences. The original VM pair stayed running.

Provisioning tests exposed and fixed a DNS-service script permission issue and an
older-sync migration gap. A real guest DNS probe exposed refusal replies without
the question section; corrected replies now return EREFUSED immediately. The
cutoff now persists through an atomic firewall reload, without a temporary gap.

Live development-VM checks: restricted egress denied example.com with HTTP 403
even using an IP override; registry.npmjs.org returned 200; an unknown DNS name
returned EREFUSED. Network cutoff was acknowledged by Linux, private guest SSH
continued working, and the cutoff remained present across a firewall restart.
The network was re-enabled afterward, with restricted destination policy intact.
A Codex CLI prompt returned WARDEN_AI_READY through inspected HTTPS. Its blocked
WebSocket attempts fell back to HTTPS as designed; opaque upgrades remain denied.

The updated menu is running. macOS notification permission is enabled and the
menu's own notification API reported one delivered approval notice. Its real
validation request for GET /repos/openai/codex was closed without granting access.
An isolated browser fixture verified exact-request links, policy controls and
setup guidance without writing fixture events to the live SIEM. Native notices
contain no request bodies or credentials. Existing approval state was revoked
by deployment; the host GitHub credential was restored in memory.

A locally signed release was installed into a separate managed prefix, a second
verified release applied, and rollback preserved a marker in independent VM
state. Reversible uninstall removed that prefix from its active path and retained
its recovery directory. The production source checkout was not overwritten.

Evidence: `.local/milestone-tests.log`, `.local/validation/20260909-220459/`,
`.local/milestone-full-disk.log`, `.local/milestone-ai-smoke.log`, and
`.local/milestone-readiness.json`. No Developer ID identity is installed: Apple
notarization is uncompleted. The historical kernel/storage anomaly remains open;
these bounded tests did not reproduce it and do not establish its cause or fix.
No off-device backup destination or independent hostile-guest audit is configured.

## VM resource monitoring — 2026-09-10

Added live, separate CPU/RAM measurements to the dashboard and native menu.
The 139-test regression run passed; the subsequently added authenticated resource
API test also passed with all 11 server tests, and native checks cover resource
formatting and expired/future samples. Parser checks cover the macOS interval
sample/page size, Linux CPU normalization/reboot, and bounded transport output and
timeout cleanup. Browser verification showed both live cards, usable narrow-panel
layout and no JavaScript errors. A nine-second, one-core load plus 128 MiB
allocation in each guest increased the independent readings as expected; evidence
is `.local/resource-load-check.json`. Both workloads exited normally. Controller
and menu deployment preserved the running VM pair and restored the host GitHub
credential in memory; no grants were active at deployment. Metrics are diagnostic
guest reports and are not used as security evidence.

## Local DMG packaging — 2026-09-10

Built a roughly 115 MiB read-only DMG entirely from local tools and cached inputs.
It contains one Warden.app with a native setup window/CLI, VM/menu helpers, Python
standard library, pinned Node and a relocated QEMU dependency closure. The app
requires Apple Silicon/macOS 15.0 for this locally available QEMU build. No personal
VM disks, repositories or credentials are included. App/component signatures are
ad-hoc, not Apple-notarized. The DMG has a SHA256/size/platform manifest.

The build ran the bundled CLI and Python/Node/QEMU with an empty environment.
Mounted the DMG, copied its app into a separate preview location, ejected the DMG,
and verified the copied app's deep signatures. Its native setup window launched
and correctly detected the existing development controller. Imported pinned
inputs into an isolated state and prepared a fresh Linux disk and cloud-init seed
using only local files. The successful prepare ran with no Homebrew on PATH and
performed no downloads or npm installation. This exposed and fixed a missing
provisioning test-directory dependency. Disk and seed preparation now publishes
its directory only after the whole operation succeeds; interruption is tested.

All **148 Python tests**, native paste/menu checks and JavaScript syntax checks
pass. New cases cover isolated state, exact input import, corrupt input rejection,
offline download refusal and transactional disk preparation. UI inspection shows
verified inputs, prepared proxy, macOS installation as the next step, and start
controls disabled while another environment occupies the controller port.

The new VM was not booted, and the original VM process remained running. Fresh
macOS restore and first proxy boot were not exercised in this packaging run. First
proxy boot still needs apt/pip network access. Apple notarization, clean-machine
Gatekeeper behavior, public redistribution review and an app update channel remain
uncompleted. Evidence: `.local/dmg-build.log`, `.local/dmg-import.log`,
`.local/dmg-prepare.log`, `.local/dmg-tests.log` and the output manifest in `dist/`.

## Simplified desktop setup — 2026-09-10

Replaced the initial multi-step control panel with one primary setup action and a
collapsed customization sheet. The default action orchestrates local input
verification, disk preparation and macOS installation. It skips completed stages
on retry and resumes tool staging without re-restoring an installed Mac. It does
not automatically boot either VM. Storage/cache choices and prepare-only mode are
optional; technical logs live in a separate Details window. Guest pairing appears
only when a running guest needs it. Progress uses backend stage events and actual
installer percentages, not timer-based estimates.

All **152 Python tests** and native check suites pass. New orchestration tests
cover ordered setup, repeat/resume, prepare-only stopping and failure boundaries.
Copied the rebuilt app from its DMG and tested its welcome screen, customization
sheet, folder picker, progress and completion screens through native UI control.
Using isolated preferences and a fresh state, one primary action plus installer
folder selection completed import, verification and Linux image preparation with
no additional prompts. Prepare-only was selected for this live test, so macOS
restore and first proxy boot were not executed. Screenshots verified the default
view has no log panel, pairing form or separate low-level setup buttons. The
original development VM remained running.
