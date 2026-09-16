# Private guest access

Run development commands and transfer files without interacting with the guest
desktop. The guest SSH daemon runs as a system LaunchDaemon, independent of the
screen lock. Commands run as the paired ordinary guest account.

```sh
./warden guest status
./warden guest exec -- warden-repo doctor
./warden guest exec -- /bin/sh -c 'cd ~/src/project && npm test'
./warden guest shell
./warden guest put ./patch.diff /tmp/patch.diff
./warden guest get /tmp/test-results.json ./test-results.json
```

`exec` preserves argument boundaries, streams output, and returns the remote exit
code. Use an explicit shell for pipelines, redirection, or changing directories.
`shell` allocates a terminal; exit it with `exit` or Ctrl-D. Transfers use SFTP,
accept literal filenames (including spaces and wildcard characters), and overwrite
the requested destination if it exists. They do not recursively copy directories.

## Installation and pairing

`./warden prepare` generates a dedicated client key in `.local/guest/` and stages
only its public key into the appliance assets. `./warden install-mac` and
`./warden stage-tools` include the guest installer and public key in the stopped
image. The normal first-login `sudo /usr/local/bin/warden-trust-proxy` step
installs the SSH service. The developer-tool retry service also installs it after
a guest account becomes available. Setup Assistant and initial pairing are still
one-time interactive steps.

For an existing running guest, run `./warden sync` on the host, wait for asset
sync, then run this once in the **guest's** Terminal:

```sh
curl -fsS http://10.77.0.1:8081/install-guest-access.sh -o /tmp/warden-access.sh
bash /tmp/warden-access.sh
```

The installer asks for the guest sudo password. Read its username and SHA256
host-key fingerprint directly from the guest, then pin them on the host:

```sh
./warden guest pair --user YOUR_GUEST_USER --fingerprint SHA256:THE_PRINTED_FINGERPRINT
./warden guest status
```

Pairing compares the supplied fingerprint with the key obtained through the
trusted Linux appliance. A mismatch fails. Replacing the key, user, or VM identity
requires inspecting the new guest and explicitly pairing with `--replace`.
Do not copy the host's `.local/guest` directory into an image or another machine.
Deleting its private client key requires installing the new public key in the
guest and pairing again; key rotation is not automatic.

The current running VM has the service installed on its persistent disk. The
distribution includes the provisioning recipe, not a copy of that personalized
disk or either machine's private SSH keys.

## Boundary and privileges

The host OpenSSH client uses a private Unix socket and the existing host-only
Linux vsock management service. Linux resolves the guest's fixed MAC from its
current DHCP lease and connects only to that guest's TCP port 2222. It accepts
no caller-selected forwarding destination. There is no host TCP listener, guest
vsock device, shared host directory, or new guest Internet route. nftables admits
only replies to Linux-initiated SSH connections on this port.

The guest accepts a dedicated public key, restricts the account to connections
from 10.77.0.1, and disables password/root login and all SSH forwarding. The host
client ignores normal SSH configuration, disables its agent and forwarding, and
requires the pinned Ed25519 server key. Existing GitHub approval gates continue
to apply to requests initiated by remote commands.

Two exact passwordless maintenance commands are installed:

```sh
sudo -n /usr/local/libexec/warden-maintain status
sudo -n /usr/local/libexec/warden-maintain install-workflow
```

The second fetches only the fixed `warden-repo` asset from the trusted appliance
and installs it at a fixed root-owned location. It accepts no path, URL, or shell
argument. General administrator work still requires the guest sudo password.
This means routine development is unattended; arbitrary root changes are not.

Guest root can alter its own daemon, keys, files, and command output. SSH does not
make guest output trustworthy. Treat returned text and downloaded files as
untrusted; an OpenSSH client vulnerability remains part of the host attack surface.
Host processes with access to Warden's private state are already trusted and can
use this management channel.

## Availability and troubleshooting

The physical Mac must be awake and the VM pair must be running. This setup does
not disable screen locking, configure automatic login, or bypass FileVault's
preboot unlock after a cold start. GUI automation and applications that require
an unlocked login keychain may still need interaction. SSH shell commands and
ordinary files do not depend on the guest desktop being unlocked.
Updating appliance source with `./warden sync` restarts the management relay and
can disconnect active SSH sessions. Finish interactive work before applying an
update; this first version does not provide persistent job supervision.

`./warden guest status` performs a real authenticated `id` command. If it fails:

* Check the VM pair is running and the guest has finished booting.
* In the guest, inspect `/var/log/warden-ssh.log` and run
  `sudo launchctl print system/dev.warden.ssh`.
* On the host, use `./scripts/proxy-exec.py systemctl status warden-access.service`
  to inspect the trusted relay. An older appliance may have a failed legacy
  `warden-management` recovery unit; `warden-access` is its persistent replacement.
* Resolve changed-key errors by inspecting and pairing the guest again; do not
  disable host-key checking.

`.local/guest/access.jsonl` records session start/end, operation, timestamp, and
exit code. It excludes command arguments, transferred paths, and output. These
local management records are separate from the proxy audit stream and are not
currently forwarded to the SIEM. Network activity from remote development
commands still follows normal proxy auditing and SIEM delivery.
