#!/usr/bin/env bash
# Operator-only recovery drill. Intentionally attempts one unhealthy deployment.
# Run on the VPS as root after a successful release; briefly interrupts the app.
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run as root on the VPS' >&2; exit 1; }
root=/opt/ocsf
current=$(readlink -f "$root/current")
test -f "$current/release.env"
original=$(basename "$current")
trial=ffffffffffffffffffffffffffffffffffffffff
test ! -e "$root/releases/$trial"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
cat > "$work/Dockerfile" <<EOF
FROM ocsf-explorer:$original
LABEL org.opencontainers.image.revision=$trial
HEALTHCHECK --interval=1s --timeout=1s --start-period=0s --retries=1 CMD exit 1
EOF
docker build -t "ocsf-explorer:$trial" "$work"
docker save "ocsf-explorer:$trial" | gzip -n > "$work/image.tar.gz"
cp "$current/"{compose.yaml,Caddyfile} "$work/"
tar -C "$work" -cf "$work/release.tar" image.tar.gz compose.yaml Caddyfile
if /usr/local/sbin/ocsf-receive "deploy $trial" < "$work/release.tar"; then
  echo 'FAIL: unhealthy deployment unexpectedly succeeded' >&2
  exit 1
fi
[[ $(readlink -f "$root/current") == "$current" ]]
site_host=$(sed -n 's/^SITE_HOST=//p' "$root/config.env")
curl -fsS --max-time 15 "https://$site_host/healthz" > "$work/health.json"
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["revision"] == sys.argv[2]' "$work/health.json" "$original"
docker image rm "ocsf-explorer:$trial"
rm -rf -- "$root/releases/$trial"
echo "Rollback drill passed: unhealthy image rejected; $original restored over HTTPS"
