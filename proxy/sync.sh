#!/bin/bash
set -euo pipefail
revision=/mnt/warden-assets/revision
test -f "$revision"
if ! cmp -s "$revision" /var/lib/warden/source-revision; then
  staging=$(mktemp -d /opt/warden-update.XXXXXX)
  trap 'rm -rf "$staging"' EXIT
  cp --reflink=never "$revision" "$staging/revision"
  mkdir "$staging/payload"
  # Do not truncate live policy/program files while copying from VirtioFS.
  cp -a --reflink=never /mnt/warden-assets/payload/. "$staging/payload/"
  sync -f "$staging"
  diff -r --no-dereference /mnt/warden-assets/payload "$staging/payload"
  cmp "$revision" "$staging/revision"
  nft --check --file "$staging/payload/proxy/firewall.nft"
  dnsmasq --test --conf-file="$staging/payload/proxy/dnsmasq.conf"
  python3 - "$staging/payload" <<'PY'
import os, sys
from pathlib import Path
source=Path(sys.argv[1])
destination=Path('/opt/warden')
for root, directories, files in os.walk(source):
    target=destination/Path(root).relative_to(source)
    target.mkdir(parents=True,exist_ok=True)
    for name in files:
        os.replace(Path(root)/name,target/name)
    fd=os.open(target,os.O_RDONLY|os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
PY
  cp /opt/warden/proxy/warden-*.service /etc/systemd/system/
  cp /opt/warden/proxy/dnsmasq.conf /etc/dnsmasq.d/warden.conf
  systemctl daemon-reload
  install -D -m 644 /opt/warden/proxy/dns_guard.py /usr/local/libexec/warden-dns-guard.py
  systemctl reset-failed warden-dns.service || true
  systemctl enable --now warden-dns.service
  systemctl restart warden-dns.service
  systemctl restart warden-firewall.service dnsmasq warden-proxy.service warden-assets.service
  systemctl enable --now warden-telemetry.service
  systemd-run --on-active=3 /bin/bash /opt/warden/proxy/activate-access.sh
  cp --reflink=never "$staging/revision" /var/lib/warden/source-revision.tmp
  sync -f /var/lib/warden/source-revision.tmp
  mv /var/lib/warden/source-revision.tmp /var/lib/warden/source-revision
fi
