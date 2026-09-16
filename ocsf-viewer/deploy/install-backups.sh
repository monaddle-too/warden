#!/usr/bin/env bash
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run as root' >&2; exit 1; }
cd "$(dirname "$0")"
install -m 0755 backup.sh /usr/local/sbin/ocsf-backup
cat > /etc/systemd/system/ocsf-backup.service <<'UNIT'
[Unit]
Description=OCSF queue and ClickHouse backup
After=docker.service
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/ocsf-backup
Nice=10
TimeoutStartSec=1h
UNIT
cat > /etc/systemd/system/ocsf-backup.timer <<'UNIT'
[Unit]
Description=Daily OCSF backup (seven local copies)
[Timer]
OnCalendar=*-*-* 04:30:00 UTC
RandomizedDelaySec=15m
Persistent=true
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now ocsf-backup.timer
