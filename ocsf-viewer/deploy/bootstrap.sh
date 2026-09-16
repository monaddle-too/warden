#!/usr/bin/env bash
# Run as root on Ubuntu, from this directory, with the CI PUBLIC key as argument.
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
public_key_file=${1:?path to the CI public key}
site_host=${2:?hostname resolving to this VPS}
[[ "$site_host" =~ ^[a-zA-Z0-9.-]+$ ]] || exit 64
grep -q '^ssh-ed25519 ' "$public_key_file"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl python3 ufw
if ! command -v docker >/dev/null; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  # shellcheck disable=SC1091
  . /etc/os-release
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' "$(dpkg --print-architecture)" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker
# Preserve SSH before enabling the firewall. Container ports must additionally
# be bound deliberately: Docker-published ports can bypass UFW filtering.
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw default deny incoming
ufw default allow outgoing
ufw --force enable
id ocsf-ci >/dev/null 2>&1 || useradd -m -s /bin/bash ocsf-ci
install -d -m 0700 -o ocsf-ci -g ocsf-ci /home/ocsf-ci/.ssh
# Expand SSH_ORIGINAL_COMMAND only on the server when sshd invokes the command.
# shellcheck disable=SC2016
{ printf 'restrict,command="sudo /usr/local/sbin/ocsf-receive \\"$SSH_ORIGINAL_COMMAND\\"" '; cat "$public_key_file"; } > /home/ocsf-ci/.ssh/authorized_keys
chown ocsf-ci:ocsf-ci /home/ocsf-ci/.ssh/authorized_keys
chmod 0600 /home/ocsf-ci/.ssh/authorized_keys
install -m 0755 receive.sh /usr/local/sbin/ocsf-receive
printf 'ocsf-ci ALL=(root) NOPASSWD: /usr/local/sbin/ocsf-receive *\n' > /etc/sudoers.d/ocsf-ci
chmod 0440 /etc/sudoers.d/ocsf-ci
visudo -cf /etc/sudoers.d/ocsf-ci
install -d -m 0700 /opt/ocsf
if [[ ! -f /opt/ocsf/config.env ]]; then
  printf 'SITE_HOST=%s\n' "$site_host" > /opt/ocsf/config.env
fi
chmod 0600 /opt/ocsf/config.env
python3 provision-storage.py
bash install-backups.sh
docker compose version
