#!/usr/bin/env bash
# Consistent queue snapshot FIRST, then event export. Stable event IDs make
# replay safe when a queued row is also present in the subsequent event export.
set -euo pipefail
umask 077
root=${OCSF_ROOT:-/opt/ocsf}
exec 8>"$root/backup.lock"
flock -n 8 || exit 0
# Do not race deployment or rollback.
exec 9>"$root/deploy.lock"
flock -w 600 9
release=$(readlink -f "$root/current")
compose() { docker compose -p ocsf --env-file "$root/config.env" --env-file "$release/release.env" -f "$release/compose.yaml" "$@"; }
mkdir -p "$root/backups"
# Retention also runs before export so expired copies cannot block new backups.
find "$root/backups" -maxdepth 1 -type f -name 'ocsf-*.tar.gz' -mtime +6 -delete
available=$(df -Pk "$root" | awk 'NR==2 {print $4}')
[[ "$available" -ge 2097152 ]] || { echo 'Backup refused: less than 2 GiB available' >&2; exit 1; }
# Bound each output file to leave room for the snapshot, export, and archive.
ulimit -f "$(( (available - 1048576) / 4 ))"
work=$(mktemp -d "$root/backups/.snapshot.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
python3 - "$root/config.env" "$work/curl.conf" <<'PY'
import pathlib, sys
config = dict(line.split('=', 1) for line in pathlib.Path(sys.argv[1]).read_text().splitlines() if '=' in line)
pathlib.Path(sys.argv[2]).write_text('header = "Authorization: Bearer '+config['OCSF_ADMIN_TOKEN']+'"\n')
PY
curl --config "$work/curl.conf" --fail --silent --show-error --max-time 120 http://127.0.0.1:8080/api/v1/backup > "$work/queue.db"
rm "$work/curl.conf"
# Expand the database credential inside its container, not on the host.
# shellcheck disable=SC2016
compose exec -T clickhouse sh -c 'exec clickhouse-client --user ocsf --password "$CLICKHOUSE_PASSWORD" --query "SELECT id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw FROM ocsf.events FINAL FORMAT JSONEachRow"' | gzip -n > "$work/events.ndjson.gz"
printf 'revision=%s\nqueue_snapshot_before_event_export=true\n' "$(basename "$release")" > "$work/manifest.txt"
(cd "$work" && sha256sum queue.db events.ndjson.gz > SHA256SUMS)
name="ocsf-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
tar -C "$work" -czf "$root/backups/$name.partial" queue.db events.ndjson.gz manifest.txt SHA256SUMS
mv "$root/backups/$name.partial" "$root/backups/$name"
echo "Backup completed: $root/backups/$name"
