#!/bin/bash
# Executes only inside the macOS guest. Never installs a host-facing listener.
set -euo pipefail
[[ $(uname -s) == Darwin && $(sysctl -n hw.model) == VirtualMac* ]] || { echo 'Run this inside the Warden guest.' >&2; exit 1; }
if [[ $EUID != 0 ]]; then exec sudo /bin/bash "$0" "$@"; fi
guest_user=$(stat -f '%Su' /dev/console)
if [[ ! $guest_user =~ ^[a-z_][a-z0-9_-]{0,31}$ || $guest_user == root || $guest_user == loginwindow || $guest_user == _* ]]; then
  echo 'Guest access setup is waiting for the first user login.'; exit 0
fi
id "$guest_user" >/dev/null
work=$(mktemp -d /private/tmp/warden-access.XXXXXX)
trap 'rm -rf "$work"' EXIT
base=http://10.77.0.1:8081
mkdir -p /opt/warden/installers /usr/local/libexec /usr/local/bin /etc/ssh /Library/LaunchDaemons /etc/sudoers.d
# Protect every writable ancestor of the privileged helper and its destination.
for directory in /usr/local /usr/local/libexec /usr/local/bin /etc/sudoers.d; do
  # SIP protects /usr/local itself, even from an otherwise redundant chown.
  [[ $(stat -f '%u:%g' "$directory") == 0:0 ]] || chown root:wheel "$directory"
  [[ $(stat -f '%Lp' "$directory") == 755 ]] || chmod 755 "$directory"
done
if [[ ${1:-} == --offline ]]; then
  cp /opt/warden/installers/guest-access.pub "$work/client.pub"
else
  curl -fsS --max-time 15 "$base/guest-access.pub" -o "$work/client.pub"
fi
[[ $(wc -l < "$work/client.pub" | tr -d ' ') == 1 ]]
grep -Eq '^ssh-ed25519 [A-Za-z0-9+/=]+$' "$work/client.pub"
ssh-keygen -lf "$work/client.pub" >/dev/null
install -o root -g wheel -m 644 "$work/client.pub" /etc/ssh/warden_authorized_keys
server_key=/etc/ssh/warden_host_ed25519_key
if [[ ! -f $server_key ]]; then ssh-keygen -q -t ed25519 -N '' -C warden-guest -f "$server_key"; fi
chown root:wheel "$server_key" "$server_key.pub";chmod 600 "$server_key";chmod 644 "$server_key.pub"
cat > "$work/sshd_config" <<EOF
Port 2222
AddressFamily inet
ListenAddress 0.0.0.0
HostKey $server_key
PidFile /var/run/warden-sshd.pid
AuthorizedKeysFile /etc/ssh/warden_authorized_keys
AllowUsers $guest_user@10.77.0.1
AuthenticationMethods publickey
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PermitRootLogin no
UsePAM yes
StrictModes yes
DisableForwarding yes
PermitUserEnvironment no
PermitUserRC no
PermitTTY yes
MaxAuthTries 3
MaxStartups 3:30:6
LoginGraceTime 20
ClientAliveInterval 30
ClientAliveCountMax 3
SetEnv PATH=/usr/local/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/bin:/bin:/usr/sbin:/sbin
SetEnv NODE_EXTRA_CA_CERTS=/usr/local/share-warden-ca.pem
Subsystem sftp internal-sftp
LogLevel VERBOSE
EOF
/usr/sbin/sshd -t -f "$work/sshd_config"
install -o root -g wheel -m 600 "$work/sshd_config" /etc/ssh/warden_sshd_config
cat > "$work/maintain" <<'EOF'
#!/bin/bash
set -euo pipefail
[[ $EUID == 0 && $# == 1 ]] || exit 64
case "$1" in
  install-workflow)
    work=$(/usr/bin/mktemp -d /private/tmp/warden-maintain.XXXXXX)
    trap '/bin/rm -rf "$work"' EXIT
    /usr/bin/curl -fsS --max-time 30 http://10.77.0.1:8081/warden-repo -o "$work/warden-repo"
    /usr/bin/install -o root -g wheel -m 755 "$work/warden-repo" /usr/local/bin/warden-repo
    ;;
  status) /bin/launchctl print system/dev.warden.ssh ;;
  *) echo 'Supported actions: install-workflow, status' >&2; exit 64 ;;
esac
EOF
install -o root -g wheel -m 755 "$work/maintain" /usr/local/libexec/warden-maintain
printf '%s ALL=(root) NOPASSWD: /usr/local/libexec/warden-maintain install-workflow, /usr/local/libexec/warden-maintain status\n' "$guest_user" > "$work/sudoers"
/usr/sbin/visudo -cf "$work/sudoers"
install -o root -g wheel -m 440 "$work/sudoers" /etc/sudoers.d/warden-maintenance
cat > "$work/daemon.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.warden.ssh</string>
<key>ProgramArguments</key><array><string>/usr/sbin/sshd</string><string>-D</string><string>-e</string><string>-f</string><string>/etc/ssh/warden_sshd_config</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>10</integer>
<key>StandardErrorPath</key><string>/var/log/warden-ssh.log</string>
</dict></plist>
EOF
install -o root -g wheel -m 644 "$work/daemon.plist" /Library/LaunchDaemons/dev.warden.ssh.plist
if ! launchctl print system/dev.warden.ssh >/dev/null 2>&1; then
  launchctl bootstrap system /Library/LaunchDaemons/dev.warden.ssh.plist
else
  launchctl kill SIGHUP system/dev.warden.ssh
fi
echo "Guest SSH installed for user: $guest_user"
echo 'Verify and pin this fingerprint on the host:'
ssh-keygen -lf "$server_key.pub" -E sha256
echo 'Host command: ./warden guest pair --user USER --fingerprint SHA256:...'
