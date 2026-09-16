#!/bin/bash
set -euo pipefail
# Run in a separate transient job, so stopping the recovery listener cannot
# terminate the command that starts its persistent replacement.
# This script is loaded after copying new source, including when an older sync
# process is performing the upgrade. Install new service prerequisites here.
install -D -m 644 /opt/warden/proxy/dns_guard.py /usr/local/libexec/warden-dns-guard.py
systemctl daemon-reload
systemctl reset-failed warden-dns.service || true
systemctl enable --now warden-dns.service
systemctl restart warden-dns.service dnsmasq
systemctl stop warden-management.service 2>/dev/null || true
systemctl enable warden-access.service
systemctl restart warden-access.service
