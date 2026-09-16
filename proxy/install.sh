#!/bin/bash
set -euo pipefail
# Executes exclusively inside the trusted Linux appliance, never on the host.
test "$(uname -s)" = Linux
export DEBIAN_FRONTEND=noninteractive
apt_snapshot=$(python3 -c 'import json; print(json.load(open("/mnt/warden-assets/payload/config/images.lock.json"))["linux"]["apt_snapshot"])')
apt-get -o Acquire::Languages=none -o Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false -o Acquire::IndexTargets::deb::CNF::DefaultEnabled=false update --snapshot "$apt_snapshot"
apt-get -o APT::Get::Always-Include-Phased-Updates=true install --snapshot "$apt_snapshot" -y --no-install-recommends python3 python3-venv python3-pip nftables dnsmasq ca-certificates curl git openssh-client
mkdir -p /opt/warden /var/lib/warden/mitmproxy
cp -a /mnt/warden-assets/payload/. /opt/warden/
python3 -m venv /opt/warden/venv
/opt/warden/venv/bin/pip install --require-hashes -r /opt/warden/proxy/requirements.lock
cp /opt/warden/proxy/dnsmasq.conf /etc/dnsmasq.d/warden.conf
mkdir -p /etc/systemd/resolved.conf.d
cat > /etc/systemd/resolved.conf.d/warden.conf <<'EOF'
[Resolve]
DNS=1.1.1.1 9.9.9.9
FallbackDNS=
EOF
systemctl restart systemd-resolved
cat > /etc/sysctl.d/90-warden.conf <<'EOF'
net.ipv4.ip_forward=0
net.ipv6.conf.all.disable_ipv6=1
net.ipv6.conf.default.disable_ipv6=1
net.ipv4.conf.all.send_redirects=0
net.ipv4.conf.all.accept_redirects=0
EOF
sysctl --system
cp /opt/warden/proxy/warden-*.service /etc/systemd/system/
cp /opt/warden/proxy/warden-*.timer /etc/systemd/system/
systemctl disable --now ssh.service ssh.socket || true
systemctl daemon-reload
systemctl enable --now warden-firewall.service
install -D -m 644 /opt/warden/proxy/dns_guard.py /usr/local/libexec/warden-dns-guard.py
systemctl enable --now warden-dns.service
systemctl restart dnsmasq
systemctl enable --now warden-proxy.service warden-assets.service
systemctl enable --now warden-telemetry.service
cp /mnt/warden-assets/revision /var/lib/warden/source-revision
systemctl enable --now warden-sync.timer
# Replace the boot-time recovery listener after this installation exits.
systemd-run --on-active=3 /bin/bash /opt/warden/proxy/activate-access.sh
touch /var/lib/warden/provisioned
