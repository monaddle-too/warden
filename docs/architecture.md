# Warden architecture and security scope

**Target architecture:** [Warden platform plan](warden-platform-plan.md) defines the new standalone chat/SBX/MCP ownership boundary. The description below documents existing implementation and security scope, not completion of that plan.

The adversary controls root in the macOS guest. The physical Mac, its user,
Virtualization.framework, Warden launcher/control service, and Linux appliance
are trusted. Hypervisor or Linux proxy exploitation is outside the current
implementation's demonstrated protection. This is a prototype, not a certified
containment boundary; see the validation record before running hostile agents.

```
Host browser (127.0.0.1:18765, session authentication)
                 │ approvals, local policy, host-held GitHub token
           Host control plane ─── append-only redacted JSONL
                 │ private Unix socket
           Linux-only Virtio socket
                 │
macOS VM ── private SOCK_DGRAM link ── Linux proxy VM ── Apple NAT ── Internet
12 GiB        no host interface          4 GiB
```

The macOS VM has one network device and no NAT/bridge attachment, Virtio socket,
host directory sharing, shared clipboard device, microphone, or device passthrough.
It cannot obtain a host network route by changing its own configuration. Both ends
of the private Ethernet link exist in the launcher; no host interface is created.
If the launcher exits, both VMs stop. If Linux stops, the launcher stops the pair.

Host-initiated guest management uses the private host Unix/vsock connection to
Linux, which relays only to the guest's DHCP lease on TCP 2222. Guest SSH runs as
a LaunchDaemon with dedicated client-key authentication, pinned server identity,
no root/password login, and forwarding disabled. The firewall admits replies to
these Linux-initiated sessions; it still blocks new guest connections to host
management services. Commands run as the guest user, with two exact maintenance
sudo commands. Guest output remains untrusted. See [guest access](guest-access.md)
for provisioning, key handling, and availability limits.

The native launcher supports **Paste from Host** (⌘⇧V). Only invoking this
host menu/shortcut reads the host pasteboard. The launcher posts that single
UTF-8 snapshot to an authenticated same-origin host API; no guest message can
invoke the action or read the host pasteboard. Ordinary ⌘V remains guest-local.
The host holds at most 64 KiB in an in-memory slot, with a random transfer ID and
60-second expiry. Collection drops the text; acknowledgement retains only the
ID/status. Clipboard text is excluded from audit logs, SQLite, and state responses.
This is application retention, not a guarantee against OS swap or crash dumps.

A guest LaunchAgent polls `/clipboard` on the Linux bootstrap server. This route
issues only the fixed `clipboard.take` RPC over Linux-only vsock. The acknowledgement
route accepts only a bounded transfer ID and relays `clipboard.ack`; neither route
accepts arbitrary RPCs, credentials, configuration, or guest clipboard contents.
Custom headers and Origin rejection block ordinary cross-origin browser requests.
They do not authenticate against guest root: deliberately sent text is available
to every hostile process in the guest, which can also forge delivery acknowledgement
or modify its own clipboard. No acknowledgement can initiate a new host paste.

The guest receiver writes UTF-8 as plain text and acknowledges only after
NSPasteboard accepts it. The launcher waits up to roughly 10 seconds plus a bounded
HTTP request timeout. Only acknowledgement matching the locally initiated transfer
permits a fixed Cmd-V sequence directed to VZVirtualMachineView. It cancels on
focus loss or intervening keyboard/mouse input and waits for host modifier release.
No clipboard text is interpreted as a command or individual key events. The guest
application's normal paste behavior applies, including possible execution of
multiline text by a terminal. Cancellation cannot revoke text already collected.

Linux's private interface is `lan0`, 10.77.0.1/24; its NAT-facing interface is
`wan0`. Linux does **not** enable IP forwarding. nftables transparently redirects
guest HTTP(S) to mitmproxy, sends DNS to its controlled resolver, allows DHCP and
the read-only bootstrap endpoint, and drops all other guest packets. IPv6 and
QUIC are blocked. A root guest changing its source address, default route, DNS,
proxy environment, or trust store cannot create a forwarding path. Removing
the CA breaks inspected TLS connectivity; it does not disable interception.
The explicit Apple update exception below does not use the proxy CA.

The proxy resolves inspected DNS authorities independently, rejects private and
special-use destinations, pins the resulting upstream IP, and requires TLS SNI
and HTTP authority agreement. Upstream certificate verification stays enabled.
No general CONNECT, WebSocket upgrade, raw TCP fallback, or alternate HTTPS port
is configured. Host/LAN/metadata destinations are blocked at both application and
Linux output layers.

Apple's updater rejects TLS interception. Exact software-update hosts listed in
`proxy/addon.py` therefore receive end-to-end TCP/443 TLS passthrough. Before
forwarding, the proxy resolves the SNI hostname independently, rejects any DNS
answer that is private, special-use, or in the pinned GitHub networks, and chooses
an IPv4 destination. The guest's original destination is replaced, even if it was
a GitHub IP. The connection hook enforces that pinned address. Missing/other SNI,
other ports, audit failures, and invalid DNS answers never enable this exception.
The proxy must establish connections lazily; already-connected upstreams cannot
receive the exception. The firewall and disabled IP forwarding remain unchanged.

The guest validates Apple's original certificate. The proxy cannot inspect HTTP
hosts, actions, headers, status codes, or bodies inside these encrypted sessions.
Apple/CDN relaying remains within the already excluded indirect-access threat;
this exception is not a claim to eliminate domain fronting at third-party servers.
A `tls.passthrough` audit event records the permitted connection's hostname and
pinned IP as OCSF Network Activity (4001/Open), without plaintext bodies. An event
records permission to attempt the connection, not proof of a completed handshake.
Cleartext GET/HEAD URLs on exact package-download hosts receive a 307 redirect to
their supported HTTPS counterpart. Large packages then avoid the inspected 16 MiB
body cap without removing limits on inspected requests or responses.

## GitHub interpretation

The pinned GitHub OpenAPI-derived catalog has one entry per method/path operation.
Only `https://api.github.com:443` operations in that catalog can receive a token.
Unknown REST endpoints, GraphQL, website actions, SSH, LFS, package registries,
release-asset transfers, and unsupported GitHub domains remain blocked. HTTPS
Git smart HTTP on canonical `github.com/OWNER/REPO.git` remotes now supports
repository read sessions and verified, exact single-branch pushes. Read sessions
cover ref discovery and upload-pack for one repository; they never match writes.
The Linux appliance verifies/reviews objects in a temporary bare repository on
tmpfs and regenerates the outgoing pack, excluding unreachable guest objects.
See [repository workflow](repository-workflow.md) for the full boundary and limits.

Exact permissions bind method, host, port, scheme, complete path/query, inspected
headers, and raw JSON body. The atomic, host-side grant consumption prevents two
simultaneous retries from using the same single-use permission. Scoped grants
permit repeated requests to one operation and exact URL with optional JSON-pointer
body equality constraints. They do not generalize across repositories or routes.

Credentials supplied by the guest are discarded for GitHub. The host releases its
token only in the response to an authorized request over the Linux-only channel;
the trusted Linux proxy attaches it in memory. No token is included in a VM image
or guest bootstrap payload. Human control and proxy decision APIs are separate:
the proxy channel has no approve/token/policy-edit operation.

Denied supported operations create a human permission request and return HTTP
428. Unsupported operations return 403 and cannot be approved. A human can grant
a single action or a timed scope. Clients retry explicitly, avoiding unsafe
automatic replay of POSTs. Grants expire on wall/monotonic time, can be revoked,
and are all revoked on control-plane restart or policy changes. In-flight GitHub
flows are monitored, and responses are checked before release. Expiry/revocation
cannot retract a request already received or committed by GitHub; polling adds
up to 0.5 seconds plus control-channel latency to connection cancellation.

## Intentional first-version restrictions and residual gaps

* New environments restrict ordinary HTTP(S) and guest DNS to explicit destination profiles. Existing legacy policies can retain public mode until migrated. Allowed AI providers can receive code and perform hosted actions outside this proxy. See `protected-development.md`.
* GitHub hostnames and a pinned `/meta` address snapshot aid classification.
  GitHub's address list is not exhaustive and changes over time. Arbitrary aliases,
  third-party domains hosted on GitHub infrastructure, and future endpoints need
  further classification tests. The implementation is not a proof of recognizing
  every possible direct GitHub service. External relays are explicitly out of scope.
* Certificate-pinned clients may fail under MITM. Only the exact Apple software-
  update hosts described above have an exception; Apple account/iCloud services
  and arbitrary pinned applications are not covered.
* HTTP bodies are buffered in memory for inspection, but never persisted in audit
  logs or persistent approval summaries; only their byte counts are retained. Authenticated Git and PR review previews are separately held in bounded, expiring memory. Large bodies are rejected and
  streaming behavior may differ from a direct connection.
* Full arbitrary-secret detection is impossible with the current scrubber. See
  `siem.md` for the precise capture contract and binary omissions.
* The host stores the GitHub token in memory. Supply it again after restarting;
  Keychain persistence and multi-identity handling are future work.
* Temporary scoped predicates support equality, not arbitrary expressions.
  GraphQL parsing, binary/tag/submodule publication, and larger repository support are future work.

## Reproduction and distribution

`images.lock.json` pins the Canonical image URL/SHA-256 and Apple's supported
restore URL/build. `tools.lock.json` pins desktop installers, Node, GitHub CLI,
and the signed Homebrew package. Homebrew is installed by the guest-only setup
helper, after a guest user and Apple Command Line Tools exist; package scripts
are never executed against the host when staging a guest image.
`config/tooling/package-lock.json` pins CLI packages. `requirements.lock` pins and
hashes the Linux Python dependency graph. All local state is gitignored, with a
0700 state directory and 0600 control sockets/secrets.

Linux OS packages are installed from a pinned Ubuntu archive snapshot during
first boot. The snapshot timestamp is in `images.lock.json`; updating it is an
explicit build-input change. This is reproducible provisioning, not a bit-for-bit
reproducible OS build (VM identities, filesystem timestamps, and per-instance CA
keys intentionally vary). Signed release manifests, component inventories and verified installation/update rollback are implemented. Developer ID signing/notarization has a build path but requires an installed identity and notary profile; neither is configured on this Mac. Snapshot retention and vendor artifact
availability still limit how long an uncached historical build can be recreated.

macOS is installed with `VZMacOSRestoreImage` and `VZMacOSInstaller`, preserving
hardware model, unique machine identifier, auxiliary storage, and the persistent
disk. The existing host supports macOS 15.6.1. Setup Assistant and Apple terms
remain interactive on this host OS; no private provisioning API is used.

The package command ships the launcher, source, locks, and provisioning recipe.
It excludes host credentials, CA private keys, local policy, audit records, and
the user's VM disks. A redistributable preconfigured macOS image and bundled
third-party applications require a separate distribution-rights review. See
Apple's license instead of assuming a local VM license grants image resale or
redistribution rights.

## Primary references

* [Apple macOS VM installation](https://developer.apple.com/documentation/virtualization/installing-macos-on-a-virtual-machine)
* [Apple file-handle network attachment](https://developer.apple.com/documentation/virtualization/vzfilehandlenetworkdeviceattachment)
* [Apple NAT network attachment](https://developer.apple.com/documentation/virtualization/vznatnetworkdeviceattachment)
* [Apple macOS Sequoia license](https://www.apple.com/legal/sla/docs/macOSSequoia.pdf)
* [GitHub REST OpenAPI source](https://github.com/github/rest-api-description)
* [GitHub IP address guidance](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/about-githubs-ip-addresses)
* [mitmproxy transparent mode](https://docs.mitmproxy.org/stable/howto/transparent/)
* [Codex CLI installation](https://learn.chatgpt.com/docs/codex/cli)
* [Codex desktop application](https://learn.chatgpt.com/docs/app)
* [Claude Code installation](https://code.claude.com/docs/en/installation)


## Dashboard pagination

Warden's Python controller serves authenticated `/api/history` and `/api/traffic`
endpoints; the separate Go SIEM backend is not involved. History uses a SQLite
(created, id) cursor and LIMIT 26 (25 rows plus a lookahead), with indexes for time
and status. SQL bounds summary length before Python receives it. The periodic
state response now contains only the newest 25 pending approval summaries and a
pending count, rather than preloading historical records. Older pending requests
can be reviewed in the paged history view. Startup retention scrubbing iterates
SQLite rows instead of fetching the entire request table.

Traffic reads backward from a fixed byte-offset snapshot of events.jsonl. Each
call reads at most 512 KiB of log data plus a 128-byte snapshot anchor and a single
boundary byte, parses at most 64 KiB per complete record, and returns at most 25
projected metadata records. Two concurrent traffic reads are allowed; additional
readers fail promptly for retry. Oversized legacy records, partial writes, and
malformed lines never cause a whole-file read. Rotation/truncation invalidates the
cursor and asks the user to refresh. Appends do not shift older pages.

Traffic filters search within each bounded scan. A page may have zero matches and
an older cursor; the UI states this and allows continuing. The UI retains one
traffic/history page and at most 20 previous cursor tokens, cancels superseded
fetches, and releases a panel's data when switching away. It does not poll traffic
or append an unbounded list of DOM rows. Metadata strings/headers are abbreviated;
HTTP bodies and credential values are omitted. No Go code or SIEM full-log cache
was added.
